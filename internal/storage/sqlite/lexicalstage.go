package sqlite

import (
	"context"
	"database/sql"
	"net/url"
	"os"
	"path/filepath"
	"strconv"

	"modernc.org/sqlite"

	"github.com/Sawmonabo/codectx/internal/writeback"
)

// A unit's lexical staging. Every token instance the unit publishes is written
// once, here, in the order the tokenizer produced it, and read back exactly
// once at seal in (term, document, column) order to be folded into the unit's
// segment. It is a database of the unit's own under <data_dir>/tmp: the main
// database's write-ahead log would otherwise carry every staged row a second
// time, and a staging table inside it would be a random-keyed insert into a
// b-tree far larger than the writer's page cache (ADR-0009).
//
// Nothing here is durable. The file is private to one unit, single-writer and
// deleted at seal, at Abandon and at Fail; losing it to a crash loses only a
// unit that is not sealed and therefore does not exist.

// lexicalStageCacheKiB is the staging database's page cache. It is the buffer
// the engine sorts the seal-time read in, so a unit whose vocabulary fits it is
// ordered without a spill file, and it is an INTERNAL layout constant, not a
// user limit: no count of terms or documents is refused because of it, a larger
// unit simply spills under <data_dir>/tmp.
const lexicalStageCacheKiB = 16 << 10

// stageSchema is appended to and never updated. No secondary index exists
// while the rows load; the one ordered read at seal is the engine's external
// merge sort over them, which is written once, front to back.
const stageSchema = `
CREATE TABLE unit_terms(seq INTEGER PRIMARY KEY, batch INTEGER NOT NULL, doc INTEGER NOT NULL,
	term TEXT NOT NULL, col TEXT NOT NULL, n INTEGER NOT NULL);
CREATE TABLE batch_docs(batch INTEGER NOT NULL, doc INTEGER NOT NULL, doc_id INTEGER NOT NULL,
	PRIMARY KEY(batch, doc)) WITHOUT ROWID;
`

// stageOrderedRead is the one ordered read of the whole design, and the only
// ORDER BY any plan of this store issues. It resolves each staged instance
// row's batch-local document number to the document rowid the ingestion
// transaction assigned it, and delivers the result in exactly the order
// lexicalWriter folds. A batch whose ingestion failed left no batch_docs rows
// and is dropped by the join, which is what makes a retried batch harmless.
const stageOrderedRead = `SELECT t.term, d.doc_id, t.col, t.n
	FROM unit_terms t JOIN batch_docs d ON d.batch = t.batch AND d.doc = t.doc
	ORDER BY t.term, d.doc_id, t.col`

// lexicalStage is one building unit's staging database.
type lexicalStage struct {
	db    *sql.DB
	path  string
	pacer *writeback.Pacer
	stmt  map[string]*sql.Stmt
	inTx  bool
	rows  int64
	// batch is the PutSearchUnits call the rows being staged belong to. A
	// document number is only unique inside one tokenizer pass, so the batch
	// is half of its key.
	batch int64
}

// stageCommitEvery bounds the dirty pages the staging holds before it commits,
// not the rows it can hold.
const stageCommitEvery = 200_000

// stageDirName is the directory under the data directory that holds the
// engine's temporary files; a unit's staging database joins them there, so the
// staging shares the disk the operator gave the data rather than a system
// temporary directory that may be a memory filesystem.
const stageDirName = "tmp"

// stageDir is the directory a unit's staging database lives in.
func (s *Store) stageDir() string { return filepath.Join(filepath.Dir(s.path), stageDirName) }

// stagePath is the staging database of the unit with row id unitRow. It is
// named from the row id alone, so the collection of a dead unit can remove that
// unit's file without touching a staging another process is still writing.
func (s *Store) stagePath(unitRow int64) string {
	return filepath.Join(s.stageDir(), "lexical-"+strconv.FormatInt(unitRow, 10)+".db")
}

// openLexicalStage creates the staging database of the unit with row id
// unitRow, replacing any file a previous, abandoned attempt left behind.
func (s *Store) openLexicalStage(ctx context.Context, unitRow int64) (*lexicalStage, error) {
	dir := s.stageDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, internal("lexical staging directory: " + err.Error())
	}
	path := s.stagePath(unitRow)
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return nil, internal("lexical staging: " + err.Error())
	}
	q := url.Values{}
	for _, p := range []string{"journal_mode(OFF)", "synchronous(OFF)", "temp_store(FILE)",
		"locking_mode(EXCLUSIVE)", "cache_size(-" + strconv.Itoa(lexicalStageCacheKiB) + ")"} {
		q.Add("_pragma", p)
	}
	dsn := (&url.URL{Scheme: "file", Path: path, RawQuery: q.Encode()}).String()
	connector, err := sqlite.NewConnector(dsn)
	if err != nil {
		return nil, internal("lexical staging connector: " + err.Error())
	}
	db := sql.OpenDB(connector)
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	if _, err := db.ExecContext(ctx, stageSchema); err != nil {
		db.Close()
		os.Remove(path)
		return nil, wrap("lexical staging", err)
	}
	return &lexicalStage{db: db, path: path, pacer: writeback.Start(path), stmt: map[string]*sql.Stmt{}}, nil
}

// exec runs one reused statement inside the staging transaction, opening the
// transaction and compiling the statement on first use. Without both, each row
// is its own transaction and its own compile.
func (g *lexicalStage) exec(ctx context.Context, query string, args ...any) error {
	if !g.inTx {
		if _, err := g.db.ExecContext(ctx, "BEGIN"); err != nil {
			return wrap("lexical staging", err)
		}
		g.inTx = true
	}
	st, ok := g.stmt[query]
	if !ok {
		var err error
		if st, err = g.db.PrepareContext(ctx, query); err != nil {
			return wrap("lexical staging", err)
		}
		g.stmt[query] = st
	}
	if _, err := st.ExecContext(ctx, args...); err != nil {
		return wrap("lexical staging", err)
	}
	g.rows++
	if g.rows%stageCommitEvery == 0 {
		return g.commit(ctx)
	}
	return nil
}

// commit ends the staging transaction. The staging has one connection, so its
// reads see uncommitted rows anyway; this bounds dirty pages rather than
// publishing anything.
func (g *lexicalStage) commit(ctx context.Context) error {
	if !g.inTx {
		return nil
	}
	g.inTx = false
	if _, err := g.db.ExecContext(ctx, "COMMIT"); err != nil {
		return wrap("lexical staging", err)
	}
	return nil
}

// nextBatch claims the batch number the next tokenizer pass stages under.
func (g *lexicalStage) nextBatch() int64 {
	g.batch++
	return g.batch
}

// putTerm stages one grouped token instance: how many times a term occurs in
// one column of one document of the current batch.
func (g *lexicalStage) putTerm(ctx context.Context, batch, doc int64, term, col string, n int64) error {
	return g.exec(ctx, `INSERT INTO unit_terms(batch, doc, term, col, n) VALUES(?, ?, ?, ?, ?)`,
		batch, doc, term, col, n)
}

// putDoc records the document rowid the ingestion transaction assigned a
// batch-local document. It runs after that transaction, so a batch that never
// committed stages no mapping and its terms are dropped by the seal's join.
func (g *lexicalStage) putDoc(ctx context.Context, batch, doc, docID int64) error {
	return g.exec(ctx, `INSERT INTO batch_docs(batch, doc, doc_id) VALUES(?, ?, ?)`, batch, doc, docID)
}

// docCount is how many documents the staging resolved, which is the segment's
// document count.
func (g *lexicalStage) docCount(ctx context.Context) (int64, error) {
	var n int64
	err := g.db.QueryRowContext(ctx, `SELECT count(*) FROM batch_docs`).Scan(&n)
	return n, wrap("lexical staging", err)
}

// close releases the staging and deletes its file. It is called at seal, at
// Abandon and at Fail, and is safe to call twice.
func (g *lexicalStage) close() error {
	if g.db == nil {
		return nil
	}
	for _, st := range g.stmt {
		st.Close()
	}
	g.pacer.Stop()
	err := g.db.Close()
	g.db = nil
	if rmErr := os.Remove(g.path); rmErr != nil && !os.IsNotExist(rmErr) && err == nil {
		err = rmErr
	}
	if err != nil {
		return internal("lexical staging: " + err.Error())
	}
	return nil
}
