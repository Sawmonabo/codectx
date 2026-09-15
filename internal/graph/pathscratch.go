package graph

import (
	"context"
	"database/sql"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"strconv"

	"github.com/Sawmonabo/codectx/internal/model"
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
}

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
`

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
	q := url.Values{}
	for _, p := range []string{"journal_mode(OFF)", "synchronous(OFF)", "temp_store(FILE)", "mmap_size(0)",
		"cache_size(-" + strconv.Itoa(pathScratchCacheKiB) + ")"} {
		q.Add("_pragma", p)
	}
	dsn := (&url.URL{Scheme: "file", Path: filepath.Join(dir, "path.db"), RawQuery: q.Encode()}).String()
	connector, err := sqlite.NewConnector(dsn)
	if err != nil {
		s.close()
		return nil, internalErr("path scratch connector: " + err.Error())
	}
	s.db = sql.OpenDB(connector)
	// One connection, one transaction: every read below streams rows on that
	// connection, so a second concurrent statement would block on it forever.
	// Callers materialize a bounded chunk and close the rows before writing.
	s.db.SetMaxOpenConns(1)
	s.db.SetMaxIdleConns(1)
	if _, err := s.db.ExecContext(ctx, pathScratchSchema); err != nil {
		s.close()
		return nil, internalErr("path scratch schema: " + err.Error())
	}
	if s.tx, err = s.db.BeginTx(ctx, nil); err != nil {
		s.close()
		return nil, internalErr("path scratch transaction: " + err.Error())
	}
	return s, nil
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
		err := os.RemoveAll(s.dir)
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
