package scip

import (
	"context"
	"os"
	"testing"

	"github.com/Sawmonabo/codectx/internal/paced"
)

// TestAnImportSpoolIsReusedAndNeverCarriesTheLastImportsFacts is the whole of
// the spool surface's contract, and both halves are failures no error would
// report.
//
// Reuse: the spool database was created per import and removed at the end of
// it, so indexing a repository unit by unit handed the filesystem one
// import's worth of deallocation after another in the middle of its work. On
// a host that discards freed blocks under a sparse image that stalls every
// writer on the machine for about a minute. The surface is pooled instead: it
// keeps its high-water length and the next import writes over it, which is
// what the paced step count proves -- a step is one window of disk handed
// back.
//
// Emptiness: because the surface is reused, an import that took one whose
// tables still held the previous import's documents and occurrences would
// publish THAT unit's symbols and relations as its own facts, with every
// invariant the import checks satisfied.
//
// Mutation: empty the surface with CREATE TABLE IF NOT EXISTS instead of
// dropping the tables first and the second import answers 3 documents it
// never read.
func TestAnImportSpoolIsReusedAndNeverCarriesTheLastImportsFacts(t *testing.T) {
	ctx := context.Background()
	work := t.TempDir()

	first, err := openScratch(ctx, work)
	if err != nil {
		t.Fatalf("openScratch: %v", err)
	}
	for i := range 3 {
		if _, err := first.tx.ExecContext(ctx,
			`INSERT INTO docs(idx, path, lang, enc, file_id, content_hash, size) VALUES(?,?,?,?,?,?,?)`,
			i, "a.go", "go", 0, "f", "h", 1); err != nil {
			t.Fatalf("insert: %v", err)
		}
	}
	if err := first.tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
	first.tx = nil
	path := first.lease.Path()
	if err := first.close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	st, err := os.Stat(path)
	if err != nil {
		t.Fatalf("the spool was removed when the import ended: %v", err)
	}
	high := st.Size()
	if high == 0 {
		t.Fatal("the first import's spool is empty; it spooled nothing and proves nothing")
	}

	freedBefore := paced.FreedBytes()
	second, err := openScratch(ctx, work)
	if err != nil {
		t.Fatalf("second openScratch: %v", err)
	}
	defer second.close()
	if freed := paced.FreedBytes() - freedBefore; freed != 0 {
		t.Fatalf("taking the spool again freed %d bytes of disk; it must free none", freed)
	}
	if got := second.lease.Path(); got != path {
		t.Fatalf("the second import spooled into %s, want the pooled surface %s: a spool is created per import again", got, path)
	}
	var docs int64
	if err := second.tx.QueryRowContext(ctx, `SELECT count(*) FROM docs`).Scan(&docs); err != nil {
		t.Fatalf("counting spooled documents: %v", err)
	}
	if docs != 0 {
		t.Fatalf("the reused surface still holds %d documents of the import before it; this import would publish their symbols as its own", docs)
	}
	if st, err := os.Stat(path); err != nil || st.Size() < high {
		t.Fatalf("the surface is %v bytes after being emptied, want at least the high-water %d: emptying it gave space back to the filesystem", st.Size(), high)
	}
}
