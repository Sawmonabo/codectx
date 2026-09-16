package sqlite

import (
	"crypto/rand"
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/Sawmonabo/codectx/internal/storage/pacedvfs"
)

// The requirement: a statement that dirties more than the statement-journal
// spill threshold inside a savepoint completes. The engine's memory journal
// spills to a file in chunks the size of the threshold, and the wrapped file
// system keeps only seventeen bits of a write's length, so a 64 MiB chunk
// would reach it as a write of nothing and come back as "database or disk is
// full" -- which is what the third uncapped index of a 6 270-file repository
// died of at activation. Mutation: make the shim's xWrite hand the wrapped
// write the whole length (unixWritePiece = 1 << 30) and the update fails
// with the engine's full-disk code.
func TestAStatementJournalPastTheSpillThresholdIsWrittenWhole(t *testing.T) {
	if engineConfigErr != nil {
		t.Fatal(engineConfigErr)
	}
	if err := pacedvfs.Register(); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "spill.db")
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=journal_mode(WAL)&_pragma=synchronous(OFF)&_pragma=cache_size(-262144)")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TABLE t(b BLOB)`); err != nil {
		t.Fatal(err)
	}
	// Three halves of the threshold: the journal holds every page's
	// pre-image, so the update below journals more than the threshold and
	// the journal spills with at least one whole chunk to write.
	const rows = 3 * statementJournalSpillBytes / 2 >> 20
	blob := make([]byte, 1<<20)
	if _, err := rand.Read(blob); err != nil {
		t.Fatal(err)
	}
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < rows; i++ {
		if _, err := tx.Exec(`INSERT INTO t VALUES(?)`, blob); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	tx, err = db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`SAVEPOINT probe`); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(`UPDATE t SET b = b`); err != nil {
		t.Fatalf("an update journaling %d MiB inside a savepoint failed: %v", rows, err)
	}
	if _, err := tx.Exec(`RELEASE probe`); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := db.QueryRow(`SELECT count(*) FROM t`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != rows {
		t.Fatalf("%d rows after the update; want %d", n, rows)
	}
}
