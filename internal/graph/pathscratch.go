package graph

import (
	"context"
	"database/sql"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/Sawmonabo/codectx/internal/model"
	"github.com/Sawmonabo/codectx/internal/paced"
	"modernc.org/sqlite"
)

// The scratch is driven on a context DERIVED FROM the request but stripped of
// its cancellation (context.WithoutCancel): these are writes to a local file
// that must complete to leave the state consistent, and a deadline landing
// between two of them would surface as CTX_INTERNAL instead of the truncated
// answer the contract promises. The request deadline is honoured by
// pathWalk.checkDeadline alone, which is the ONE place the search stops.
//
// pathScratch is the external-memory state of one shortest-path search: the
// settled set, the parent edges of the shortest-path DAG and the tentative
// cost buckets, all on disk in a private SQLite file with a FIXED page cache.
//
// Why a database and not a sorted spool. An exact Dijkstra needs random-access
// membership ("is this node settled, and at what distance?") on every
// relaxation, and random access again at enumeration to walk parent pointers.
// A spool is forward-only, so the spool shape costs a merge-join sweep of the
// whole settled set per cost phase -- O(settled x phases), superlinear in the
// reachable set. A B-tree gives the same answers in O(log n) per probe with a
// working set bounded by the page cache, which is the trade the external-memory
// shortest-path literature makes explicit (Munagala and Ranade, SODA 1999;
// Meyer and Zeh): the bucket phases keep the I/O sequential, the index keeps
// the probes cheap, and neither structure is ever held in heap.
//
// The pragma set is the one internal/provider/scip/scratch.go already uses for
// the same purpose -- journalling off because a scratch file has nothing to
// recover, temp_store FILE so SQLite's own transient sorts and the recursive
// enumeration spill to disk instead of heap, mmap off so pages are not mapped
// outside the cache bound, and a negative cache_size, which SQLite reads as
// KiB rather than pages and is therefore a byte ceiling rather than a
// page-size-dependent one.
type pathScratch struct {
	dir string
	db  *sql.DB
	tx  *sql.Tx
	// retained marks state reopened from a previous page's retention. The
	// engine reads it to decide whether the search is being seeded or resumed.
	retained bool
}

// pathScratchJournalFresh and pathScratchJournalRetained are the journal modes
// the two entry points open under, and the difference is load-bearing.
//
// A FRESH search's directory is this request's alone: whatever happens to it,
// close() removes it, so there is nothing to roll back and OFF is the cheapest
// correct mode.
//
// A REOPENED search's directory is state a continuation NAMES. When a resumed
// page exits on a retryable failure the directory is kept so the caller's retry
// can present the same cursor, and the page's half-written transaction has to
// be undone exactly -- committing it would advance `pending` and
// `progress.expand_after` past edges the cursor's counters say were never
// folded into a bucket, which loses routes silently. Under journal_mode=OFF
// SQLite documents ROLLBACK as behaving in an undefined way and the file as
// likely corrupt, so the retained search is opened under a real rollback
// journal instead. DELETE, not MEMORY -- a memory journal would grow with the
// page's writes, and the per-page budgets are unlimited by default -- and not
// WAL, which is persistent in the file and would add sidecar files the spool
// store's directory adoption knows nothing about.
const (
	pathScratchJournalFresh    = "OFF"
	pathScratchJournalRetained = "DELETE"
)

// pathScratchCacheKiB is the SQLite page-cache ceiling for one path search, in
// KiB, passed as a negative cache_size. It is a fixed working-set floor for the
// B-tree probes, not a bound on the search: exceeding it costs reads, never a
// truncated answer, so it is not a scale limit and takes no configuration key.
const pathScratchCacheKiB = 4096

const pathScratchSchema = `
CREATE TABLE settled(node TEXT PRIMARY KEY, dist INTEGER NOT NULL, depth INTEGER NOT NULL) WITHOUT ROWID;
CREATE TABLE parent(node TEXT NOT NULL, rel TEXT NOT NULL, frm TEXT NOT NULL, kind TEXT NOT NULL,
	PRIMARY KEY(node, rel)) WITHOUT ROWID;
CREATE INDEX parent_out ON parent(frm, rel);
CREATE TABLE bucket(cost INTEGER NOT NULL, node TEXT NOT NULL, rel TEXT NOT NULL, frm TEXT NOT NULL,
	kind TEXT NOT NULL, depth INTEGER NOT NULL, PRIMARY KEY(cost, node, rel)) WITHOUT ROWID;
CREATE TABLE reach(node TEXT PRIMARY KEY) WITHOUT ROWID;
CREATE TABLE pending(node TEXT PRIMARY KEY) WITHOUT ROWID;
CREATE TABLE progress(k INTEGER PRIMARY KEY, expand_after TEXT NOT NULL, depth_pruned INTEGER NOT NULL);
INSERT INTO progress(k, expand_after, depth_pruned) VALUES(0, '', 0);
`

// pending and progress are what make the search RESUMABLE, and they are here
// rather than in the continuation token because neither is a position a token
// can name.
//
// pending holds every node that has been settled but whose outgoing edges have
// not all been read yet. A page that stops between settling a node and
// expanding it -- the deadline, the per-page visited budget, the per-page edge
// budget all land there -- would otherwise lose that node's edges forever: the
// next page re-reads the same cost bucket and settle's staleness check skips
// it, because it IS settled. The routes through it would simply be missing.
// A node's row is deleted only once its whole keyset walk is done.
//
// progress carries the expansion's keyset position inside the batch in flight,
// so a resumed page does not re-read edges it has already charged against the
// edge budget, and the depth-pruned flag, which is an answer-level disclosure
// that must survive the page that discovered it.

// openPathScratch creates the search's database in a fresh private directory
// under parent.
func openPathScratch(ctx context.Context, parent string) (*pathScratch, error) {
	// An engine with no spool store has no directory of its own; the system
	// temporary directory then holds the ONE directory this search owns and
	// close() removes. Creating a second, outer directory here would leave one
	// behind per query, because close() removes only what s.dir names.
	var (
		dir string
		err error
	)
	if parent == "" {
		dir, err = os.MkdirTemp("", "codectx-path-")
	} else {
		if err = os.MkdirAll(parent, 0o700); err != nil {
			return nil, internalErr("path scratch directory: " + err.Error())
		}
		dir, err = os.MkdirTemp(parent, "path-")
	}
	if err != nil {
		return nil, internalErr("path scratch directory: " + err.Error())
	}
	s := &pathScratch{dir: dir}
	if err := s.open(ctx, pathScratchJournalFresh); err != nil {
		s.close()
		return nil, err
	}
	if _, err := s.db.ExecContext(ctx, pathScratchSchema); err != nil {
		s.close()
		return nil, internalErr("path scratch schema: " + err.Error())
	}
	if err := s.begin(ctx); err != nil {
		return nil, err
	}
	return s, nil
}

// reopenPathScratch reattaches to the state a previous page retained. The
// schema is NOT re-applied: the tables, and everything the earlier pages put
// in them, are the point.
func reopenPathScratch(ctx context.Context, dir string) (*pathScratch, error) {
	s := &pathScratch{dir: dir, retained: true}
	if err := s.open(ctx, pathScratchJournalRetained); err != nil {
		s.close()
		return nil, err
	}
	if err := s.begin(ctx); err != nil {
		return nil, err
	}
	return s, nil
}

// open attaches the database handle to s.dir's file, creating it if it is not
// there. It is shared by the fresh and the resumed path so both searches run
// under exactly the same page-cache ceiling and pragma set; journal names the
// one pragma that differs between them (see the constants above).
func (s *pathScratch) open(ctx context.Context, journal string) error {
	q := url.Values{}
	for _, p := range []string{"journal_mode(" + journal + ")", "synchronous(OFF)", "temp_store(FILE)", "mmap_size(0)",
		"cache_size(-" + strconv.Itoa(pathScratchCacheKiB) + ")"} {
		q.Add("_pragma", p)
	}
	dsn := (&url.URL{Scheme: "file", Path: filepath.Join(s.dir, "path.db"), RawQuery: q.Encode()}).String()
	connector, err := sqlite.NewConnector(dsn)
	if err != nil {
		return internalErr("path scratch connector: " + err.Error())
	}
	s.db = sql.OpenDB(connector)
	// One connection, one transaction: every read below streams rows on that
	// connection, so a second concurrent statement would block on it forever.
	// Callers materialize a bounded chunk and close the rows before writing.
	s.db.SetMaxOpenConns(1)
	s.db.SetMaxIdleConns(1)
	if err := s.db.PingContext(ctx); err != nil {
		return internalErr("path scratch open: " + err.Error())
	}
	// Read the journal mode BACK. A retained search's ability to undo an
	// abandoned page is the whole reason its state survives a retryable
	// failure, and a DSN pragma that was quietly ignored would leave that
	// promise resting on an undefined rollback. Refusing here is honest; a
	// silently unrollbackable search is not.
	var got string
	if err := s.db.QueryRowContext(ctx, "PRAGMA journal_mode").Scan(&got); err != nil {
		return internalErr("path scratch journal mode: " + err.Error())
	}
	if !strings.EqualFold(got, journal) {
		return internalErr("path scratch journal mode: asked for " + journal + ", got " + got)
	}
	return nil
}

// begin opens the search's single transaction.
func (s *pathScratch) begin(ctx context.Context) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		s.close()
		return internalErr("path scratch transaction: " + err.Error())
	}
	s.tx = tx
	return nil
}

// detach COMMITS the page's writes, closes the database and hands the caller
// the directory, which is now the caller's to retain. close() afterwards is a
// no-op on the directory, so the ordinary defer cannot remove state a
// continuation was just minted for.
func (s *pathScratch) detach() (string, error) {
	if s.tx != nil {
		err := s.tx.Commit()
		s.tx = nil
		if err != nil {
			return "", internalErr("path scratch commit: " + err.Error())
		}
	}
	if s.db != nil {
		err := s.db.Close()
		s.db = nil
		if err != nil {
			return "", internalErr("path scratch close: " + err.Error())
		}
	}
	dir := s.dir
	s.dir = ""
	return dir, nil
}

// retain rolls the page's writes back and closes the database WITHOUT removing
// the directory, leaving the retained state exactly as the page found it. The
// caller's close() afterwards is then a no-op on the directory.
//
// This is the non-terminal exit: a retryable failure tells the caller to
// present the SAME cursor again, and that cursor names this directory. The
// rollback is what makes the retry see the pre-page state rather than a page
// torn off halfway through.
func (s *pathScratch) retain() error {
	if s == nil {
		return nil
	}
	var rollback error
	if s.tx != nil {
		rollback = s.tx.Rollback()
		s.tx = nil
	}
	if s.db != nil {
		if err := s.db.Close(); err != nil && rollback == nil {
			rollback = err
		}
		s.db = nil
	}
	if rollback != nil {
		// The undo did not complete, so what is on disk is not the state the
		// cursor names. s.dir is deliberately LEFT set: the caller's close()
		// then removes it, and the retry is refused with CTX_CURSOR_INVALID
		// rather than resumed from a half-rolled-back search.
		return internalErr("path scratch retain: " + rollback.Error())
	}
	s.dir = ""
	return nil
}

// close discards the transaction and removes the directory. It is safe to call
// more than once and is what makes the state a scratch file rather than a leak.
func (s *pathScratch) close() error {
	if s == nil {
		return nil
	}
	if s.tx != nil {
		s.tx.Rollback()
		s.tx = nil
	}
	if s.db != nil {
		s.db.Close()
		s.db = nil
	}
	if s.dir != "" {
		err := paced.RemoveAll(s.dir)
		s.dir = ""
		if err != nil {
			return internalErr("path scratch cleanup: " + err.Error())
		}
	}
	return nil
}

func (s *pathScratch) exec(ctx context.Context, query string, args ...any) error {
	if _, err := s.tx.ExecContext(ctx, query, args...); err != nil {
		return internalErr("path scratch write: " + err.Error())
	}
	return nil
}

// row runs a single-row query; found is false when there is no row.
func (s *pathScratch) row(ctx context.Context, query string, args []any, dest ...any) (bool, error) {
	err := s.tx.QueryRowContext(ctx, query, args...).Scan(dest...)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, internalErr("path scratch read: " + err.Error())
	}
	return true, nil
}

// each streams a query's rows through fn in the order the query states. fn must
// not write to the scratch: the rows hold the single connection until it ends.
//
// The ctx here is the walk's UNCANCELLABLE one (see openPathScratch's caller),
// so the per-row check below is the structural guard rather than the deadline:
// a scratch read is never the thing that ends a path search.
func (s *pathScratch) each(ctx context.Context, query string, args []any,
	fn func(scan func(dest ...any) error) error) error {
	rows, err := s.tx.QueryContext(ctx, query, args...)
	if err != nil {
		return internalErr("path scratch read: " + err.Error())
	}
	defer rows.Close()
	for rows.Next() {
		if err := ctx.Err(); err != nil {
			return model.Canceled(err)
		}
		if err := fn(rows.Scan); err != nil {
			return err
		}
	}
	if err := rows.Err(); err != nil {
		return internalErr("path scratch read: " + err.Error())
	}
	return nil
}

// internalErr is the package's shape for a failure of the engine's own
// machinery, distinct from a caller's argument error or a resource limit.
func internalErr(msg string) *model.Error {
	return &model.Error{Code: model.CodeInternal, Message: msg}
}

// kindCodesFor maps a query's relation kinds onto the pinned generation's kind
// dictionary. The second result says the query named kinds and the generation
// seals NONE of them, which is the case a caller must not confuse with the nil
// slice the port reads as "every kind": a query for a vocabulary this
// generation never produced answers nothing, not everything.
//
// A query that named no kind at all walks every kind, and both results are
// then empty and false.
func kindCodesFor(reader GraphReader, kinds []model.RelationKind) ([]KindCode, bool) {
	if len(kinds) == 0 {
		return nil, false
	}
	codes := make([]KindCode, 0, len(kinds))
	for _, k := range kinds {
		if code, ok := reader.Kinds().Code(k); ok {
			codes = append(codes, code)
		}
	}
	return codes, len(codes) == 0
}

// encodeEdgePos and decodeEdgePos are the scratch's spelling of a scan
// position. It lives in the state file rather than in the continuation token
// because it is a position inside ONE pending batch's scan, which the token
// never names: the token names the cost bucket, and the batch is rebuilt from
// `pending` on the next page.
//
// The zero position is the empty string, so a state file written before the
// first edge page reads back as "the beginning of the scan" and needs no
// separate flag.
func encodeEdgePos(p EdgePos) string {
	if p.IsZero() {
		return ""
	}
	return strconv.FormatUint(uint64(p.Node), 10) + ":" + strconv.FormatUint(uint64(p.Index), 10)
}

func decodeEdgePos(s string) (EdgePos, error) {
	if s == "" {
		return EdgePos{}, nil
	}
	node, index, ok := strings.Cut(s, ":")
	if !ok {
		return EdgePos{}, internalErr("path scratch holds a malformed scan position")
	}
	n, err := strconv.ParseUint(node, 10, 64)
	if err != nil {
		return EdgePos{}, internalErr("path scratch holds a malformed scan position")
	}
	i, err := strconv.ParseUint(index, 10, 32)
	if err != nil {
		return EdgePos{}, internalErr("path scratch holds a malformed scan position")
	}
	return EdgePos{Node: NodeRef(n), Index: uint32(i)}, nil
}
