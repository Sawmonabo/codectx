package sqlite

import (
	"context"
	"database/sql"
	"net/url"
	"path/filepath"
	"strconv"

	"modernc.org/sqlite"

	"github.com/Sawmonabo/codectx/internal/scratch"
)

// A unit's lexical staging. Every token instance the unit publishes is written
// once, here, in the order the tokenizer produced it, and read back exactly
// once at seal in (term, document, column) order to be folded into the unit's
// segment. It is a database of the unit's own under <data_dir>/tmp: the main
// database's write-ahead log would otherwise carry every staged row a second
// time, and a staging table inside it would be a random-keyed insert into a
// b-tree far larger than the writer's page cache (ADR-0009).
//
// Nothing here is durable. The database is private to one building unit and
// single-writer while that unit holds it; losing it to a crash loses only a
// unit that is not sealed and therefore does not exist. Its writes reach the
// disk through the process's paced file system like every other connection's
// (ADR-0008).
//
// The FILE outlives the unit. It is a slot of the store's scratch pool: a unit
// takes a slot at its first batch, empties its tables so it stages into a
// database with no rows, and gives the slot back at seal, at Abandon and at
// Fail. The engine recycles the emptied pages from its own free list and
// auto_vacuum stays off, so the file keeps its high-water length and the next
// unit writes over it. Nothing is freed for the life of the store: a unit that
// staged gigabytes and removed them handed the filesystem every one of those
// extents at once, and on a host that discards freed blocks that stalls every
// writer on the machine for about a minute.

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

// lexicalStage is one building unit's staging database: the slot it holds and
// the connection it stages through.
type lexicalStage struct {
	db    *sql.DB
	lease *scratch.Lease
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

// stageEmpty drops whatever the slot's previous tenant staged. Dropping the
// tables rather than deleting their rows is one statement whatever the slot
// holds, and it returns every page to the database's free list for this unit
// to fill again; auto_vacuum is off, so the file does not shrink and nothing
// is handed back to the filesystem.
const stageEmpty = `DROP TABLE IF EXISTS unit_terms;
DROP TABLE IF EXISTS batch_docs;
`

// openLexicalStage takes a staging slot from the store's scratch pool and
// empties it, so the unit stages into a database with no rows in a file that
// already has its pages.
func (s *Store) openLexicalStage(ctx context.Context) (*lexicalStage, error) {
	lease, err := scratch.For(filepath.Dir(s.path)).Take(scratch.LexicalStage)
	if err != nil {
		return nil, internal("lexical staging: " + err.Error())
	}
	stage, err := s.openStageSlot(ctx, lease.Path())
	if err != nil {
		// Emptying the slot is the first thing openStageSlot does, so a
		// failure here is the slot's own contents: an image a crash left
		// mid-write whose DROP TABLE cannot run. Released, it would fail the
		// next unit's seal the same way, and every seal after that.
		lease.Unusable()
		return nil, err
	}
	stage.lease = lease
	return stage, nil
}

// openStageSlot opens one slot's database and empties it.
func (s *Store) openStageSlot(ctx context.Context, path string) (*lexicalStage, error) {
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
	if _, err := db.ExecContext(ctx, stageEmpty+stageSchema); err != nil {
		db.Close()
		return nil, wrap("lexical staging", err)
	}
	return &lexicalStage{db: db, stmt: map[string]*sql.Stmt{}}, nil
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

// close gives the staging slot back to the pool, with its file at the length
// this unit left it. It is called at seal, at Abandon and at Fail, and is safe
// to call twice. Nothing is removed: the next unit to stage takes this slot
// and empties its tables.
func (g *lexicalStage) close() error {
	if g.db == nil {
		return nil
	}
	for _, st := range g.stmt {
		st.Close()
	}
	err := g.db.Close()
	g.db = nil
	if g.lease != nil {
		g.lease.Release()
		g.lease = nil
	}
	if err != nil {
		return internal("lexical staging: " + err.Error())
	}
	return nil
}
