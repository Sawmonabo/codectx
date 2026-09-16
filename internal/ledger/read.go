package ledger

import (
	"context"
	"database/sql"
	"encoding/hex"
	"errors"
	"time"

	"github.com/Sawmonabo/codectx/internal/model"
)

// A RunRow is one recorded run as a reader sees it. WallMS is the run's
// elapsed time: the time between its start and its finish, or between its
// start and now while it is still going, so a live run reads as how far it has
// got rather than as a zero.
type RunRow struct {
	RunID               string
	Kind                Kind
	RepositoryID        string
	GenerationID        *int64
	StartedAt           time.Time
	FinishedAt          *time.Time
	WallMS              int64
	Outcome             Outcome
	FileCount           int64
	SourceBytes         int64
	UnitsPlanned        int64
	UnitsSucceeded      int64
	UnitsFailed         int64
	UnitsSubdivided     int64
	EventsDropped       int64
	ProcessPeakRSSBytes *uint64
}

// A SpanRow is one span as a reader sees it, and also what the collector hands
// a subscriber when a span finishes. Every measured field is a pointer: a
// figure the platform could not give is absent, never zero.
type SpanRow struct {
	RunID string
	Seq   int64
	// ParentSeq is the ordinal of the span this one nests under, absent for a
	// span the run opened at its top level. It is the ordinal rather than a
	// row id because the ordinal is what both sides of the bus know.
	ParentSeq *int64
	Stage     string
	ScopeKey  string
	Provider  string
	StartedAt time.Time
	// FinishedAt is absent while the span is running and on a span that was
	// still open when its run ended.
	FinishedAt *time.Time
	// WallMS is the span's measured wall once it has finished, and its elapsed
	// time so far while it is still running, so a live reader sees a stage
	// that has been going for a minute as a minute rather than as nothing.
	WallMS int64
	// Running is whether WallMS is elapsed-so-far rather than a final
	// measurement, so a caller never presents the two as the same thing.
	Running         bool
	CPUUserMS       *int64
	CPUSysMS        *int64
	CPUUnattributed string
	PeakRSSBytes    *uint64
	ReadBytes       *uint64
	WriteBytes      *uint64
	ItemsIn         int64
	ItemsOut        int64
	Outcome         Outcome
	DiagnosticCode  string
	Failure         string
	// ShareOfWall is this span's wall as a fraction of its run's, computed
	// when the row is read. It is zero on a row published to a subscriber,
	// where the run's own wall is not yet known.
	ShareOfWall float64
}

// A RunView is one run and its spans. Truncated says the run holds more spans
// than one page carries, so a caller reports a partial view as partial.
type RunView struct {
	Run       RunRow
	Spans     []SpanRow
	Truncated bool
}

// A Reader is a read-only view of a ledger file. It is a separate connection
// with query_only set, so it reads while a run writes and can disturb nothing:
// this is what makes the live view cost the run nothing.
type Reader struct{ db *sql.DB }

// OpenReader opens the ledger beside the index store in dir for reading. It
// verifies the schema fingerprint, so a reader never half-reads a file written
// by a different schema.
func OpenReader(ctx context.Context, dir string) (*Reader, error) {
	path := Path(dir)
	db, err := openPool(path, readerPragmas(), "deferred", 2)
	if err != nil {
		return nil, err
	}
	var fingerprint string
	if err := db.QueryRowContext(ctx, `SELECT fingerprint FROM ledger_meta WHERE singleton = 1`).Scan(&fingerprint); err != nil {
		db.Close()
		return nil, wrap("read the ledger schema", err)
	}
	if fingerprint != Fingerprint {
		db.Close()
		return nil, &model.Error{Code: model.CodeSchemaMismatch,
			Message:     "run ledger schema fingerprint " + fingerprint + " does not match this binary's " + Fingerprint,
			Details:     map[string]string{"path": path},
			Remediation: "delete the run ledger beside the index cache, or rebuild the cache with `codectx index --rebuild`"}
	}
	return &Reader{db: db}, nil
}

// Close releases the reader's connections.
func (r *Reader) Close() error { return wrap("close", r.db.Close()) }

// LatestRun is the run a status surface shows for a repository: the one that
// is still running if one is, and otherwise the one that produced
// generationID. A generationID of zero or less means the repository has no
// active generation, in which case the most recently started finished run
// answers -- which is what an operator asking about the run that just failed
// is asking for.
//
// It returns false, and no error, when the repository has no recorded run.
// Spans are bounded to one page; a run with more says so.
func (r *Reader) LatestRun(ctx context.Context, repositoryID string, generationID int64) (RunView, bool, error) {
	repo, err := model.DecodeID(repositoryID)
	if err != nil {
		return RunView{}, false, err
	}
	// One statement, so the choice between "the live run" and "the run that
	// produced the generation" cannot see two different snapshots of the file.
	// A running run sorts ahead of everything; among the rest the generation's
	// own run sorts ahead of the others, and then the most recent start wins.
	const query = `SELECT run_id, kind, repository_id, generation_id, started_at, finished_at, outcome,
		file_count, source_bytes, units_planned, units_succeeded, units_failed, units_subdivided,
		events_dropped, process_peak_rss_bytes
		FROM runs WHERE repository_id = ?
		ORDER BY (outcome = 'running') DESC, (generation_id IS NOT NULL AND generation_id = ?) DESC, started_at DESC
		LIMIT 1`
	var view RunView
	var raw, rawRepo []byte
	var generation sql.NullInt64
	var started string
	var finished sql.NullString
	var kind, outcome string
	var peak sql.NullInt64
	err = r.db.QueryRowContext(ctx, query, repo, generationID).Scan(&raw, &kind, &rawRepo, &generation, &started, &finished, &outcome,
		&view.Run.FileCount, &view.Run.SourceBytes, &view.Run.UnitsPlanned, &view.Run.UnitsSucceeded,
		&view.Run.UnitsFailed, &view.Run.UnitsSubdivided, &view.Run.EventsDropped, &peak)
	if errors.Is(err, sql.ErrNoRows) {
		return RunView{}, false, nil
	}
	if err != nil {
		return RunView{}, false, wrap("latest run", err)
	}
	view.Run.RunID = hex.EncodeToString(raw)
	view.Run.Kind = Kind(kind)
	view.Run.RepositoryID = hex.EncodeToString(rawRepo)
	view.Run.Outcome = Outcome(outcome)
	if generation.Valid {
		id := generation.Int64
		view.Run.GenerationID = &id
	}
	if peak.Valid {
		bytes := uint64(peak.Int64)
		view.Run.ProcessPeakRSSBytes = &bytes
	}
	if view.Run.StartedAt, err = parseTime(started); err != nil {
		return RunView{}, false, err
	}
	now := time.Now()
	end := now
	if finished.Valid {
		at, err := parseTime(finished.String)
		if err != nil {
			return RunView{}, false, err
		}
		view.Run.FinishedAt = &at
		end = at
	}
	view.Run.WallMS = end.Sub(view.Run.StartedAt).Milliseconds()
	if view.Spans, view.Truncated, err = r.spans(ctx, raw, view.Run.WallMS, now); err != nil {
		return RunView{}, false, err
	}
	return view, true, nil
}

// spans reads one page of a run's spans in the order they were opened, filling
// a running span's elapsed time and every span's share of the run's wall. The
// page is model.MaxRecordsPerResult wide, the same bound every other list in
// the product carries; one extra row is read to tell a full page from a
// truncated one.
func (r *Reader) spans(ctx context.Context, runID []byte, runWallMS int64, now time.Time) ([]SpanRow, bool, error) {
	const query = `SELECT s.seq, p.seq, s.stage, s.scope_key, s.provider, s.started_at, s.finished_at,
		s.wall_ms, s.cpu_user_ms, s.cpu_sys_ms, s.cpu_unattributed, s.peak_rss_bytes, s.read_bytes, s.write_bytes,
		s.items_in, s.items_out, s.outcome, s.diagnostic_code, s.failure_json
		FROM spans s LEFT JOIN spans p ON p.id = s.parent_id
		WHERE s.run_id = ? ORDER BY s.seq LIMIT ?`
	rows, err := r.db.QueryContext(ctx, query, runID, model.MaxRecordsPerResult+1)
	if err != nil {
		return nil, false, wrap("run spans", err)
	}
	defer rows.Close()
	hexID := hex.EncodeToString(runID)
	out := make([]SpanRow, 0, model.MaxRecordsPerResult)
	truncated := false
	for rows.Next() {
		if len(out) == model.MaxRecordsPerResult {
			truncated = true
			break
		}
		var row SpanRow
		var parent, wall, cpuUser, cpuSys, peak, read, write sql.NullInt64
		var started string
		var finished sql.NullString
		var outcome string
		if err := rows.Scan(&row.Seq, &parent, &row.Stage, &row.ScopeKey, &row.Provider, &started, &finished,
			&wall, &cpuUser, &cpuSys, &row.CPUUnattributed, &peak, &read, &write,
			&row.ItemsIn, &row.ItemsOut, &outcome, &row.DiagnosticCode, &row.Failure); err != nil {
			return nil, false, wrap("run spans", err)
		}
		row.RunID = hexID
		row.Outcome = Outcome(outcome)
		if parent.Valid {
			seq := parent.Int64
			row.ParentSeq = &seq
		}
		row.CPUUserMS = optionalInt(cpuUser)
		row.CPUSysMS = optionalInt(cpuSys)
		row.PeakRSSBytes = optionalBytes(peak)
		row.ReadBytes = optionalBytes(read)
		row.WriteBytes = optionalBytes(write)
		if row.StartedAt, err = parseTime(started); err != nil {
			return nil, false, err
		}
		if finished.Valid {
			at, err := parseTime(finished.String)
			if err != nil {
				return nil, false, err
			}
			row.FinishedAt = &at
		}
		switch {
		case wall.Valid:
			row.WallMS = wall.Int64
		case row.Outcome == OutcomeRunning:
			// A running span has no measured wall yet; what it has is the time
			// it has been going, which is the number a live view needs.
			row.Running = true
			row.WallMS = now.Sub(row.StartedAt).Milliseconds()
		}
		if runWallMS > 0 {
			row.ShareOfWall = float64(row.WallMS) / float64(runWallMS)
		}
		out = append(out, row)
	}
	return out, truncated, wrap("run spans", rows.Err())
}

func optionalInt(v sql.NullInt64) *int64 {
	if !v.Valid {
		return nil
	}
	n := v.Int64
	return &n
}

func optionalBytes(v sql.NullInt64) *uint64 {
	if !v.Valid {
		return nil
	}
	n := uint64(v.Int64)
	return &n
}
