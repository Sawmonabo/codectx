package ledger

import (
	"context"
	"database/sql"
	"encoding/hex"
	"errors"
	"io/fs"
	"os"
	"time"

	"github.com/Sawmonabo/codectx/internal/model"
)

// A RunRow is one recorded run as a reader sees it. WallMS is the run's
// elapsed time: the time between its start and its finish, or between its
// start and now while it is still going, so a live run reads as how far it has
// got rather than as a zero. It is zero on a run whose writer stopped
// refreshing its liveness, because that run's end was never measured; such a
// run reads as interrupted and carries no FinishedAt.
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

// A RunView is one run and its spans. SpansOmitted is how many of the run's
// spans this page does not carry, so a caller reports a partial view as
// partial and says by how much: "some stages are missing" does not tell an
// operator whether the page dropped three of them or three hundred.
type RunView struct {
	Run          RunRow
	Spans        []SpanRow
	SpansOmitted int64
}

// A Reader is a read-only view of a ledger file. It is a separate connection
// with query_only set, so it reads while a run writes and can disturb nothing:
// this is what makes the live view cost the run nothing.
type Reader struct{ db *sql.DB }

// OpenReader opens the ledger beside the index store in dir for reading, and
// verifies its schema fingerprint so a reader never half-reads a file written
// by another schema.
//
// It reports false, and no error, for a workspace that has never recorded a
// run: there is nothing to read and nothing to report as broken. The
// connection is read-only, so asking never creates the file.
func OpenReader(ctx context.Context, dir string) (*Reader, bool, error) {
	path := Path(dir)
	if _, err := os.Stat(path); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, false, nil
		}
		return nil, false, internal("stat " + path + ": " + err.Error())
	}
	db, err := openPool(path, readerPragmas(), "deferred", 2, true)
	if err != nil {
		return nil, false, err
	}
	var fingerprint string
	if err := db.QueryRowContext(ctx, `SELECT fingerprint FROM ledger_meta WHERE singleton = 1`).Scan(&fingerprint); err != nil {
		db.Close()
		return nil, false, wrap("read the ledger schema", err)
	}
	if fingerprint != Fingerprint {
		db.Close()
		return nil, false, &model.Error{Code: model.CodeSchemaMismatch,
			Message:     "run ledger schema fingerprint " + fingerprint + " does not match this binary's " + Fingerprint,
			Details:     map[string]string{"path": path},
			Remediation: "delete the run ledger beside the index cache, or rebuild the cache with `codectx index --rebuild`"}
	}
	return &Reader{db: db}, true, nil
}

// Close releases the reader's connections.
func (r *Reader) Close() error { return wrap("close", r.db.Close()) }

// liveRun is the one rule this product answers "is the process writing that
// run still there" by: the run says it is running AND the deadline its own
// collector published on the row has not elapsed against the reader's clock.
// It takes the clock as its single parameter.
//
// It is one constant rather than the same expression written out beside each
// query, so the run a status surface reports as interrupted, the run a live
// view measures and the run a process waiting for the workspace lock judges
// the holder by cannot come to different conclusions about one row.
const liveRun = `outcome = 'running' AND expires_at > ?`

// LatestRun is the run a status surface shows for a repository: the one that
// is still live if one is, and otherwise the one that produced generationID. A
// generationID of zero or less means the repository has no active generation,
// in which case the most recently started run answers -- which is what an
// operator asking about the run that just failed is asking for.
//
// Live means the run's writer has renewed the deadline it published on the run
// row, not merely that the row says 'running'. A process that died without
// stopping its ledger leaves 'running' behind for ever, and preferring it would
// show that dead run instead of the run that produced the active generation --
// exactly when an operator is investigating the crash. Such a run reads as
// interrupted, with its still-open spans interrupted too, which is what the
// collector itself writes for a ledger that stops; the reader states the same
// fact without touching the file, because a status surface must never write.
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
	// A live run sorts ahead of everything; among the rest the generation's
	// own run sorts ahead of the others, and then the most recent start wins.
	// Liveness is the writer's own unelapsed deadline, evaluated once and
	// returned beside the row, so the ordering and what the caller is told
	// about the run it chose cannot disagree. The stamps are fixed-width UTC,
	// which is why the clock compares as text.
	const query = `SELECT run_id, kind, repository_id, generation_id, started_at, finished_at, outcome,
		file_count, source_bytes, units_planned, units_succeeded, units_failed, units_subdivided,
		events_dropped, process_peak_rss_bytes, (` + liveRun + `) AS live
		FROM runs WHERE repository_id = ?
		ORDER BY live DESC, (generation_id IS NOT NULL AND generation_id = ?) DESC, started_at DESC
		LIMIT 1`
	now := time.Now()
	return r.read(ctx, now, r.db.QueryRowContext(ctx, query, formatTime(now), repo, generationID))
}

// Run is the run with this identifier, and false when the file holds none: the
// answer for a caller that knows which run it is asking about. A run reports
// on itself through this and not through LatestRun, which answers with the
// live run of the repository -- and a process can have a second run of its own
// live, a deferred publication beside an index, which would then answer for
// the run that asked.
//
// It judges liveness, bounds its spans and reports what it omitted exactly as
// LatestRun does; the two differ in which row they select and in nothing else.
func (r *Reader) Run(ctx context.Context, runID string) (RunView, bool, error) {
	id, err := model.DecodeID(runID)
	if err != nil {
		return RunView{}, false, err
	}
	const query = `SELECT run_id, kind, repository_id, generation_id, started_at, finished_at, outcome,
		file_count, source_bytes, units_planned, units_succeeded, units_failed, units_subdivided,
		events_dropped, process_peak_rss_bytes, (` + liveRun + `) AS live
		FROM runs WHERE run_id = ?`
	now := time.Now()
	return r.read(ctx, now, r.db.QueryRowContext(ctx, query, formatTime(now), id))
}

// read turns one selected run row and its page of spans into a view. It is the
// one place the two queries' rows are interpreted, so what a run reads as --
// interrupted rather than running, no wall where nobody measured one -- cannot
// depend on which of them asked.
func (r *Reader) read(ctx context.Context, now time.Time, row *sql.Row) (RunView, bool, error) {
	var live bool
	var view RunView
	var raw, rawRepo []byte
	var generation sql.NullInt64
	var started string
	var finished sql.NullString
	var kind, outcome string
	var peak sql.NullInt64
	err := row.Scan(&raw, &kind, &rawRepo, &generation, &started, &finished, &outcome,
		&view.Run.FileCount, &view.Run.SourceBytes, &view.Run.UnitsPlanned, &view.Run.UnitsSucceeded,
		&view.Run.UnitsFailed, &view.Run.UnitsSubdivided, &view.Run.EventsDropped, &peak, &live)
	if errors.Is(err, sql.ErrNoRows) {
		return RunView{}, false, nil
	}
	if err != nil {
		return RunView{}, false, wrap("read the run", err)
	}
	view.Run.RunID = hex.EncodeToString(raw)
	view.Run.Kind = Kind(kind)
	view.Run.RepositoryID = hex.EncodeToString(rawRepo)
	view.Run.Outcome = Outcome(outcome)
	// A run that says 'running' but whose writer stopped refreshing is a run
	// that was cut off, which is the same fact the collector writes for itself
	// when a ledger stops.
	lapsed := Outcome(outcome) == OutcomeRunning && !live
	if lapsed {
		view.Run.Outcome = OutcomeInterrupted
	}
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
	end := now
	if finished.Valid {
		at, err := parseTime(finished.String)
		if err != nil {
			return RunView{}, false, err
		}
		view.Run.FinishedAt = &at
		end = at
	}
	if lapsed {
		// Nobody measured how long this run took: it has no finish time, and
		// the deadline it last published is the writer's promise, not an end.
		// It reads as no wall at all, exactly as a span that was open when its
		// ledger stopped reads as no wall, and so every span in it reports no
		// share of one either. A span inside it may still carry the seconds it
		// was measured for; the run around it was never measured.
		view.Run.WallMS = 0
	} else {
		view.Run.WallMS = end.Sub(view.Run.StartedAt).Milliseconds()
	}
	if view.Spans, view.SpansOmitted, err = r.spans(ctx, raw, view.Run.WallMS, now, lapsed); err != nil {
		return RunView{}, false, err
	}
	return view, true, nil
}

// spans reads one page of a run's spans in the order they were opened, filling
// a running span's elapsed time and every span's share of the run's wall.
// lapsed says the run's writer stopped refreshing its liveness, in which case a
// span the run left open never finished and reads as interrupted rather than as
// one that has been going ever since. The
// page is model.MaxRecordsPerResult wide, the same bound every other list in
// the product carries; one extra row is read to tell a full page from a
// truncated one, and only a page that is actually truncated pays for the one
// COUNT that says by how much. The count is a second statement rather than a
// longer scan because the run's spans are indexed by (run_id, seq): counting
// them reads the index, while scanning past the page would read every row of a
// run this page deliberately did not read.
func (r *Reader) spans(ctx context.Context, runID []byte, runWallMS int64, now time.Time, lapsed bool) ([]SpanRow, int64, error) {
	const query = `SELECT s.seq, p.seq, s.stage, s.scope_key, s.provider, s.started_at, s.finished_at,
		s.wall_ms, s.cpu_user_ms, s.cpu_sys_ms, s.cpu_unattributed, s.peak_rss_bytes, s.read_bytes, s.write_bytes,
		s.items_in, s.items_out, s.outcome, s.diagnostic_code, s.failure_json
		FROM spans s LEFT JOIN spans p ON p.id = s.parent_id
		WHERE s.run_id = ? ORDER BY s.seq LIMIT ?`
	rows, err := r.db.QueryContext(ctx, query, runID, model.MaxRecordsPerResult+1)
	if err != nil {
		return nil, 0, wrap("run spans", err)
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
			return nil, 0, wrap("run spans", err)
		}
		row.RunID = hexID
		row.Outcome = Outcome(outcome)
		// Judged before the wall below, so an open span of a run whose writer
		// died reports no measurement rather than an elapsed time that grows
		// for as long as the row survives.
		if lapsed && row.Outcome == OutcomeRunning {
			row.Outcome = OutcomeInterrupted
		}
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
			return nil, 0, err
		}
		if finished.Valid {
			at, err := parseTime(finished.String)
			if err != nil {
				return nil, 0, err
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
	if err := rows.Err(); err != nil {
		return nil, 0, wrap("run spans", err)
	}
	if !truncated {
		return out, 0, nil
	}
	// The page is closed before the count so the reader's two connections are
	// not both held for one answer.
	rows.Close()
	var total int64
	if err := r.db.QueryRowContext(ctx, `SELECT count(*) FROM spans WHERE run_id = ?`, runID).Scan(&total); err != nil {
		return nil, 0, wrap("count run spans", err)
	}
	omitted := total - int64(len(out))
	if omitted < 0 {
		// A span deleted between the page and the count: the page is still
		// every span this read saw, and nothing was omitted from it.
		omitted = 0
	}
	return out, omitted, nil
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

// LiveStage is what a process waiting for the workspace lock needs of this
// file and nothing more: whether SOME run in it is still live by the liveRun
// rule above, and the stage that run is inside right now.
//
// It is not scoped to a repository, and deliberately so. The lock and this
// ledger are both per workspace directory, the waiter is asking about the one
// process holding that directory, and the repository identity is derived far
// below the point where a waiter must decide whether to keep waiting.
//
// The stage is the innermost span the live run still has open, which is the
// most specific thing that can be said about where the holder is. It is empty
// for a run that has opened no span yet, which is a live holder that has not
// reached a stage rather than a holder that is doing nothing.
//
// One statement, so the liveness and the stage cannot be read from two
// different moments of a file another process is writing.
func (r *Reader) LiveStage(ctx context.Context) (stage string, live bool, err error) {
	const query = `SELECT COALESCE((SELECT s.stage FROM spans s
			WHERE s.run_id = runs.run_id AND s.outcome = 'running'
			ORDER BY s.seq DESC LIMIT 1), '')
		FROM runs WHERE ` + liveRun + ` ORDER BY started_at DESC LIMIT 1`
	err = r.db.QueryRowContext(ctx, query, formatTime(time.Now())).Scan(&stage)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, wrap("read the live run", err)
	}
	return stage, true, nil
}
