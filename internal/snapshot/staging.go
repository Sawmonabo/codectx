package snapshot

import (
	"context"
	"database/sql"
	"errors"
	"net/url"
	"os"
	"path/filepath"

	"github.com/Sawmonabo/codectx/internal/model"
	"github.com/Sawmonabo/codectx/internal/paced"
	_ "modernc.org/sqlite"
)

// staging is the indexed on-disk staging of Section 10.1: a private SQLite
// file that lives for exactly one capture. It joins the three membership
// streams (Git index, Git status, filesystem walk), answers the walk's
// per-path hooks, and yields the manifest in canonical path order, so no
// repository-sized list ever lives in the Go heap. It is not a catalog: the
// durable manifest is what storage.PutSnapshot imports from it, and the file
// is removed when the capture ends (or by Sweep after a crash).
//
// Durability is deliberately off. A staging database that survives a crash is
// garbage, and the syncs it would cost are paid for nothing.
type staging struct {
	db   *sql.DB
	path string
}

// stagingPageRows bounds one page read from staging so a cursor is never held
// open across the writes that follow it on the single connection.
const stagingPageRows = 500

const stagingSchema = `CREATE TABLE entries (
	path TEXT PRIMARY KEY,
	tracked INTEGER NOT NULL DEFAULT 0,
	mode TEXT NOT NULL DEFAULT '',
	oid TEXT NOT NULL DEFAULT '',
	change TEXT NOT NULL DEFAULT '',
	status TEXT NOT NULL DEFAULT '',
	hash TEXT NOT NULL DEFAULT '',
	size INTEGER NOT NULL DEFAULT 0,
	executable INTEGER NOT NULL DEFAULT 0,
	mtime INTEGER NOT NULL DEFAULT 0,
	lfs INTEGER NOT NULL DEFAULT 0,
	seen INTEGER NOT NULL DEFAULT 0
) WITHOUT ROWID`

// openStaging creates a fresh staging database under dir (created 0700).
func openStaging(ctx context.Context, dir string) (*staging, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, ioError("staging directory", err)
	}
	id, err := model.NewRandomID()
	if err != nil {
		return nil, err
	}
	path := filepath.Join(dir, stagingPrefix+id[:16]+".db")
	// The DSN shape (file URL plus _pragma=name(value) pairs) is the same one
	// internal/storage/sqlite/open.go builds for the durable store. The two
	// differ in every value (this one trades durability for speed), and the
	// driver DSN grammar is the shared contract; if a third caller appears the
	// builder belongs in one place.
	q := url.Values{}
	for _, p := range []string{"journal_mode(OFF)", "synchronous(OFF)", "temp_store(FILE)", "cache_size(-4096)", "locking_mode(EXCLUSIVE)"} {
		q.Add("_pragma", p)
	}
	db, err := sql.Open("sqlite", (&url.URL{Scheme: "file", Path: path, RawQuery: q.Encode()}).String())
	if err != nil {
		return nil, internal("staging open: %v", err)
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	s := &staging{db: db, path: path}
	if _, err := db.ExecContext(ctx, stagingSchema); err != nil {
		s.Close()
		return nil, s.wrap("staging schema", err)
	}
	return s, nil
}

// Close removes the database. It is safe to call more than once.
func (s *staging) Close() error {
	if s.db == nil {
		return nil
	}
	err := s.db.Close()
	s.db = nil
	rerr := paced.Remove(s.path)
	if err != nil {
		return internal("staging close: %v", err)
	}
	if rerr != nil && !errors.Is(rerr, os.ErrNotExist) {
		return internal("staging remove: %v", rerr)
	}
	return nil
}

func (s *staging) wrap(op string, err error) error {
	if err == nil {
		return nil
	}
	return ioError(op, err)
}

func (s *staging) exec(ctx context.Context, op, query string, args ...any) error {
	_, err := s.db.ExecContext(ctx, query, args...)
	return s.wrap(op, err)
}

// putIndex records one Git index entry.
func (s *staging) putIndex(ctx context.Context, path, mode, oid string) error {
	return s.exec(ctx, "staging index", `INSERT INTO entries(path, tracked, mode, oid) VALUES(?, 1, ?, ?)
		ON CONFLICT(path) DO UPDATE SET tracked = 1, mode = excluded.mode, oid = excluded.oid`, path, mode, oid)
}

// putChange records one `git status` row. A later row for the same path wins,
// which is what makes a path both deleted from the index and present as
// untracked read as untracked: Git lists the untracked entry last. headOID is
// kept only for a path the index no longer names, where it is the only
// provenance a staged deletion has.
func (s *staging) putChange(ctx context.Context, path, change, headOID string) error {
	return s.exec(ctx, "staging status", `INSERT INTO entries(path, change, oid) VALUES(?, ?, ?)
		ON CONFLICT(path) DO UPDATE SET change = excluded.change,
			oid = CASE WHEN entries.tracked = 1 THEN entries.oid ELSE excluded.oid END`, path, change, headOID)
}

// resetChanges clears the status view before a reconciliation re-run, so a
// path Git no longer reports reads as clean or unknown rather than stale.
func (s *staging) resetChanges(ctx context.Context) error {
	return s.exec(ctx, "staging reset", `UPDATE entries SET change = '' WHERE change <> ''`)
}

// row is one staging entry as the builder reasons about it.
type row struct {
	path       string
	tracked    bool
	mode       string
	oid        string
	change     string
	status     model.FileStatus
	hash       string
	size       int64
	executable bool
	mtime      int64
	// lfs marks captured bytes that are a Git LFS pointer file.
	lfs  bool
	seen int
}

const rowColumns = `path, tracked, mode, oid, change, status, hash, size, executable, mtime, lfs, seen`

func scanRow(sc interface{ Scan(...any) error }) (row, error) {
	var r row
	var tracked, executable, lfs int64
	err := sc.Scan(&r.path, &tracked, &r.mode, &r.oid, &r.change, &r.status, &r.hash, &r.size, &executable, &r.mtime, &lfs, &r.seen)
	r.tracked, r.executable, r.lfs = tracked == 1, executable == 1, lfs == 1
	return r, err
}

// get returns the entry at path; found is false when there is none.
func (s *staging) get(ctx context.Context, path string) (row, bool, error) {
	r, err := scanRow(s.db.QueryRowContext(ctx, `SELECT `+rowColumns+` FROM entries WHERE path = ?`, path))
	if errors.Is(err, sql.ErrNoRows) {
		return row{}, false, nil
	}
	if err != nil {
		return row{}, false, s.wrap("staging get", err)
	}
	return r, true, nil
}

// forcedPredicate selects paths Git tracks: in the index, or reported by
// status as added, modified or deleted. Section 10.2 forces these past ignore
// rules and policy exclusions.
const forcedPredicate = `(tracked = 1 OR change IN ('added', 'modified', 'deleted'))`

// knownPredicate selects every path Git reported at all. A path in neither
// the index nor the status output is ignored, inside a submodule, or in a
// nested repository: not this repository's source.
const knownPredicate = `(tracked = 1 OR change <> '')`

// exists runs an existence query. err is a typed error or nil.
func (s *staging) exists(ctx context.Context, query string, args ...any) (bool, error) {
	var one int
	err := s.db.QueryRowContext(ctx, query, args...).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, s.wrap("staging lookup", err)
	}
	return true, nil
}

func (s *staging) forced(ctx context.Context, path string) (bool, error) {
	return s.exists(ctx, `SELECT 1 FROM entries WHERE path = ? AND `+forcedPredicate, path)
}

func (s *staging) known(ctx context.Context, path string) (bool, error) {
	return s.exists(ctx, `SELECT 1 FROM entries WHERE path = ? AND `+knownPredicate, path)
}

// forcedUnder and knownUnder ask the same questions of a whole directory: is
// any such path beneath dir? The range [dir+"/", dir+"0") is exactly the set
// of paths with that prefix under bytewise ordering, because '0' follows '/'.
func (s *staging) forcedUnder(ctx context.Context, dir string) (bool, error) {
	return s.exists(ctx, `SELECT 1 FROM entries WHERE path >= ? AND path < ? AND `+forcedPredicate+` LIMIT 1`, dir+"/", dir+"0")
}

func (s *staging) knownUnder(ctx context.Context, dir string) (bool, error) {
	return s.exists(ctx, `SELECT 1 FROM entries WHERE path >= ? AND path < ? AND `+knownPredicate+` LIMIT 1`, dir+"/", dir+"0")
}

// captured records the outcome of reading one file in pass.
func (s *staging) captured(ctx context.Context, r row, pass int) error {
	return s.exec(ctx, "staging capture", `INSERT INTO entries(path, status, hash, size, executable, mtime, lfs, seen)
		VALUES(?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(path) DO UPDATE SET status = excluded.status, hash = excluded.hash, size = excluded.size,
			executable = excluded.executable, mtime = excluded.mtime, lfs = excluded.lfs, seen = excluded.seen`,
		r.path, string(r.status), r.hash, r.size, boolInt(r.executable), r.mtime, boolInt(r.lfs), pass)
}

// relabel changes only the manifest status of an entry whose bytes are
// unchanged, and marks it seen in pass.
func (s *staging) relabel(ctx context.Context, path string, status model.FileStatus, pass int) error {
	return s.exec(ctx, "staging relabel", `UPDATE entries SET status = ?, seen = ? WHERE path = ?`, string(status), pass, path)
}

// seen marks an entry as present in pass without changing its capture.
func (s *staging) markSeen(ctx context.Context, path string, pass int) error {
	return s.exec(ctx, "staging seen", `UPDATE entries SET seen = ? WHERE path = ?`, pass, path)
}

// tombstone turns an entry into a deleted manifest row.
func (s *staging) tombstone(ctx context.Context, path string, pass int) error {
	return s.exec(ctx, "staging tombstone", `UPDATE entries SET status = ?, hash = '', size = 0, executable = 0, mtime = 0, lfs = 0, seen = ? WHERE path = ?`,
		string(model.FileDeleted), pass, path)
}

// drop removes an entry from the manifest entirely.
func (s *staging) drop(ctx context.Context, path string) error {
	return s.exec(ctx, "staging drop", `DELETE FROM entries WHERE path = ?`, path)
}

// eachPage streams rows matching where in path order, one bounded page at a
// time, so fn may write to staging between pages. fn must not hold rows.
func (s *staging) eachPage(ctx context.Context, where string, args []any, fn func(row) error) error {
	after := ""
	for {
		query := `SELECT ` + rowColumns + ` FROM entries WHERE path > ? AND ` + where + ` ORDER BY path LIMIT ?`
		rows, err := s.db.QueryContext(ctx, query, append(append([]any{after}, args...), stagingPageRows)...)
		if err != nil {
			return s.wrap("staging page", err)
		}
		page := make([]row, 0, stagingPageRows)
		for rows.Next() {
			r, err := scanRow(rows)
			if err != nil {
				rows.Close()
				return s.wrap("staging page", err)
			}
			page = append(page, r)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return s.wrap("staging page", err)
		}
		for _, r := range page {
			if err := fn(r); err != nil {
				return err
			}
		}
		if len(page) < stagingPageRows {
			return nil
		}
		after = page[len(page)-1].path
	}
}

// eachUnseen streams every entry the walk of pass did not emit.
func (s *staging) eachUnseen(ctx context.Context, pass int, fn func(row) error) error {
	return s.eachPage(ctx, `seen <> ?`, []any{pass}, fn)
}

// eachManifest streams the manifest rows -- every entry with a status -- in
// canonical path order.
func (s *staging) eachManifest(ctx context.Context, fn func(row) error) error {
	return s.eachPage(ctx, `status <> ''`, nil, fn)
}

func boolInt(b bool) int64 {
	if b {
		return 1
	}
	return 0
}
