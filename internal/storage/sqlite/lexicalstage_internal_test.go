package sqlite

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// TestAStagingSlotIsReusedAndNeverCarriesTheLastUnitsRows is the whole of the
// staging slot's contract, and both halves are failures that no error would
// report.
//
// Reuse: a staging database used to be created per unit and removed at seal,
// so a run of a large repository handed the filesystem every one of those
// files' extents, unit after unit, in the middle of its work. On a host that
// discards freed blocks under a sparse image that stalls every writer on the
// machine for about a minute. The slot is pooled instead: the file keeps its
// high-water length and the next unit writes over it.
//
// Emptiness: because the file is reused, a unit that took a slot whose tables
// still held the previous unit's rows would fold THAT unit's token instances
// into its own sealed segment — one unit's terms served as another's, with the
// segment's own invariants all satisfied.
func TestAStagingSlotIsReusedAndNeverCarriesTheLastUnitsRows(t *testing.T) {
	ctx := context.Background()
	s := &Store{path: filepath.Join(t.TempDir(), "store.db")}

	first, err := s.openLexicalStage(ctx)
	if err != nil {
		t.Fatalf("openLexicalStage: %v", err)
	}
	batch := first.nextBatch()
	// Enough rows that the file has real pages to inherit.
	for i := range 2000 {
		if err := first.putTerm(ctx, batch, int64(i), "term", "name", 1); err != nil {
			t.Fatalf("putTerm: %v", err)
		}
		if err := first.putDoc(ctx, batch, int64(i), int64(i+1)); err != nil {
			t.Fatalf("putDoc: %v", err)
		}
	}
	if err := first.commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}
	n, err := first.docCount(ctx)
	if err != nil {
		t.Fatalf("docCount: %v", err)
	}
	if n != 2000 {
		t.Fatalf("the first unit staged %d documents, want 2000", n)
	}
	path := first.lease.Path()
	if err := first.close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	st, err := os.Stat(path)
	if err != nil {
		t.Fatalf("the staging file was removed when the unit ended: %v", err)
	}
	high := st.Size()
	if high == 0 {
		t.Fatal("the first unit's staging is empty; it staged nothing and proves nothing")
	}

	second, err := s.openLexicalStage(ctx)
	if err != nil {
		t.Fatalf("openLexicalStage: %v", err)
	}
	defer second.close()
	if got := second.lease.Path(); got != path {
		t.Fatalf("the second unit staged into %s, want the pooled slot %s: a staging file is created per unit again", got, path)
	}
	n, err = second.docCount(ctx)
	if err != nil {
		t.Fatalf("docCount: %v", err)
	}
	if n != 0 {
		t.Fatalf("the reused slot still holds %d documents of the unit before it; this unit would seal them as its own", n)
	}
	var terms int64
	if err := second.db.QueryRowContext(ctx, `SELECT count(*) FROM unit_terms`).Scan(&terms); err != nil {
		t.Fatalf("counting staged terms: %v", err)
	}
	if terms != 0 {
		t.Fatalf("the reused slot still holds %d token instances of the unit before it", terms)
	}
	if st, err := os.Stat(path); err != nil || st.Size() < high {
		t.Fatalf("the slot is %v bytes after being emptied, want at least the high-water %d: emptying it gave space back to the filesystem", st.Size(), high)
	}
}
