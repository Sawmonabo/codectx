package scip

import (
	"context"
	"database/sql"
	"errors"
	"net/url"
	"os"
	"path/filepath"

	"github.com/Sawmonabo/codectx/internal/model"
	arena "github.com/Sawmonabo/codectx/internal/scratch"
	"modernc.org/sqlite"
)

// scratch is the disk-backed native-symbol map and occurrence spool of one
// import (Section 11.4): a private SQLite file under the run's work directory
// with a fixed page-cache bound, so forward and external references are
// resolved without an in-memory symbol table and a document of any size is
// grouped into relation facts without holding its occurrences. Everything in
// it is derived from the index bytes and none of it outlives the import.
//
// The file is a surface of the provider's scratch pool, taken for the import
// and given back at its length. It used to be created per import and removed
// at the end of it, so indexing a repository unit by unit handed the
// filesystem one import's spool after another in the middle of its work. On a
// host that discards freed blocks under a sparse virtual disk that stalls
// every writer on the machine for about a minute, a minute later.
//
// SQLite's own transient sorts still spill under temp_store=FILE and are
// created and removed by the library through the paced shim, which is where
// that boundary is drawn for every database this product opens.
//
// The whole import runs in one transaction on one connection: with the
// journal off there is nothing to recover, and the surface is emptied on the
// way in whatever happened to the import before it.
type scratch struct {
	lease *arena.Lease
	db    *sql.DB
	tx    *sql.Tx
	bytes int64
	// broken says the surface's own contents are what failed, so it leaves
	// the pool instead of going back to be handed to the next import.
	broken bool
}

// scratchEmpty is run before the schema. The surface is never truncated --
// that is the point of pooling it -- so it arrives holding the import before
// it, and a CREATE TABLE IF NOT EXISTS would leave that import's documents,
// symbols and occurrences in place for this one to publish as its own facts.
// Dropping the tables returns their pages to the database's own free list,
// which this import writes over, and gives the filesystem nothing back.
const scratchEmpty = `
DROP TABLE IF EXISTS docs;
DROP TABLE IF EXISTS occ;
DROP TABLE IF EXISTS sym;
DROP TABLE IF EXISTS rel;
DROP TABLE IF EXISTS defs;
DROP TABLE IF EXISTS edges;
DROP TABLE IF EXISTS manifest;
`

const scratchSchema = `
CREATE TABLE docs(idx INTEGER PRIMARY KEY, path TEXT NOT NULL, lang TEXT NOT NULL, enc INTEGER NOT NULL,
	file_id TEXT NOT NULL, content_hash TEXT NOT NULL, size INTEGER NOT NULL, file_node TEXT NOT NULL DEFAULT '');
CREATE TABLE occ(doc INTEGER NOT NULL, seq INTEGER NOT NULL, symbol TEXT NOT NULL, roles INTEGER NOT NULL,
	r0 INTEGER NOT NULL, r1 INTEGER NOT NULL, r2 INTEGER NOT NULL, r3 INTEGER NOT NULL,
	e0 INTEGER, e1 INTEGER, e2 INTEGER, e3 INTEGER, PRIMARY KEY(doc, seq)) WITHOUT ROWID;
CREATE TABLE sym(key TEXT PRIMARY KEY, symbol TEXT NOT NULL, kind INTEGER NOT NULL, name TEXT NOT NULL, sig TEXT NOT NULL,
	sigcut INTEGER NOT NULL DEFAULT 0,
	node TEXT NOT NULL DEFAULT '', def_doc INTEGER NOT NULL DEFAULT -1, def_start INTEGER NOT NULL DEFAULT 0, def_end INTEGER NOT NULL DEFAULT 0) WITHOUT ROWID;
CREATE TABLE rel(key TEXT NOT NULL, target TEXT NOT NULL, flags INTEGER NOT NULL, PRIMARY KEY(key, target)) WITHOUT ROWID;
CREATE TABLE defs(doc INTEGER NOT NULL, start INTEGER NOT NULL, end INTEGER NOT NULL, node TEXT NOT NULL, PRIMARY KEY(doc, start, end, node)) WITHOUT ROWID;
CREATE TABLE edges(frm TEXT NOT NULL, kind TEXT NOT NULL, dst TEXT NOT NULL, file_id TEXT NOT NULL, content_hash TEXT NOT NULL,
	start INTEGER NOT NULL, end INTEGER NOT NULL, sl INTEGER NOT NULL, sc INTEGER NOT NULL, el INTEGER NOT NULL, ec INTEGER NOT NULL,
	symbol TEXT NOT NULL, detail TEXT NOT NULL, PRIMARY KEY(frm, kind, dst, file_id, start, end, detail)) WITHOUT ROWID;
CREATE TABLE manifest(path TEXT PRIMARY KEY, hash TEXT NOT NULL) WITHOUT ROWID;
`

// openScratch opens the scratch database on a surface of the pool under
// parent, which is the provider's own work directory and never a run's: a run
// directory is removed when the run ends, and a pool inside one would be
// created and freed exactly as often as the file it was meant to keep. What
// it spools is counted, not capped: see scratch.charge.
func openScratch(ctx context.Context, parent string) (*scratch, error) {
	if !filepath.IsAbs(parent) {
		return nil, invalid("the scip work directory must be an absolute private path")
	}
	if err := os.MkdirAll(parent, 0o700); err != nil {
		return nil, internal("scip work directory: " + err.Error())
	}
	lease, err := arena.For(parent).Take(arena.ImportSpool)
	if err != nil {
		return nil, internal("scip scratch surface: " + err.Error())
	}
	s := &scratch{lease: lease}
	q := url.Values{}
	for _, p := range []string{"journal_mode(OFF)", "synchronous(OFF)", "temp_store(FILE)", "mmap_size(0)", "cache_size(-4096)"} {
		q.Add("_pragma", p)
	}
	dsn := (&url.URL{Scheme: "file", Path: lease.Path(), RawQuery: q.Encode()}).String()
	connector, err := sqlite.NewConnector(dsn)
	if err != nil {
		s.close()
		return nil, internal("scip scratch connector: " + err.Error())
	}
	s.db = sql.OpenDB(connector)
	s.db.SetMaxOpenConns(1)
	s.db.SetMaxIdleConns(1)
	if _, err := s.db.ExecContext(ctx, scratchEmpty+scratchSchema); err != nil {
		// Emptying the spool is what failed, so the surface itself is the
		// problem -- an image a crash left mid-write -- and it leaves the
		// pool rather than failing every import after this one.
		s.broken = true
		s.close()
		return nil, internal("scip scratch schema: " + err.Error())
	}
	if s.tx, err = s.db.BeginTx(ctx, nil); err != nil {
		s.close()
		return nil, internal("scip scratch transaction: " + err.Error())
	}
	return s, nil
}

// close discards the transaction and gives the surface back to the pool at
// its length, freeing nothing. It is safe to call more than once.
func (s *scratch) close() error {
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
	if s.lease != nil {
		if s.broken {
			s.lease.Unusable()
		} else {
			s.lease.Release()
		}
		s.lease = nil
	}
	return nil
}

// charge accounts n spooled bytes. The spool is an on-disk database under the
// shared temporary-bytes budget, so the running total is a figure the import
// reports against providers.scip.max_spool_bytes when the operator set one --
// never a reason to stop spooling and fail a unit halfway through an index.
// The error result is kept so callers read like every other scratch write.
func (s *scratch) charge(n int64) error {
	s.bytes += n
	return nil
}

func (s *scratch) exec(ctx context.Context, query string, args ...any) error {
	if _, err := s.tx.ExecContext(ctx, query, args...); err != nil {
		return internal("scip scratch write: " + err.Error())
	}
	return nil
}

// row runs a single-row query; found is false when there is no row.
func (s *scratch) row(ctx context.Context, query string, args []any, dest ...any) (found bool, err error) {
	err = s.tx.QueryRowContext(ctx, query, args...).Scan(dest...)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, internal("scip scratch read: " + err.Error())
	}
	return true, nil
}

// each streams a query's rows through fn in the order the query states.
func (s *scratch) each(ctx context.Context, query string, args []any, fn func(scan func(dest ...any) error) error) error {
	rows, err := s.tx.QueryContext(ctx, query, args...)
	if err != nil {
		return internal("scip scratch read: " + err.Error())
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
		return internal("scip scratch read: " + err.Error())
	}
	return nil
}

func invalid(msg string) *model.Error {
	return &model.Error{Code: model.CodeArgumentInvalid, Message: msg}
}

func internal(msg string) *model.Error {
	return &model.Error{Code: model.CodeInternal, Message: msg}
}
