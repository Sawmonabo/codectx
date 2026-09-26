package sqlite_test

import (
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/Sawmonabo/codectx/internal/model"
	store "github.com/Sawmonabo/codectx/internal/storage/sqlite"
)

// TestCarryOverDrainsAStreamThatReadsTheStore hands CarryOver a replaced-file
// stream that walks the predecessor's inputs through the store while the
// carry-over's own ingestion call holds the group: the shape of the
// dependence applier's merge join.
//
// Requirement: a producer stream may read the store from inside the ingestion
// call that drains it. Its reads run on the reader pool and see the last
// commit, which holds the predecessor, so they never wait on the group the
// call holds.
//
// Mutation that fails it: read the unit's inputs on the group's own
// connection (readOwn in UnitInputs): the call waits for itself, the test
// reports it after a minute, and the package then ends on its own timeout,
// since a store whose group is held by a call that never returns cannot
// close.
func TestCarryOverDrainsAStreamThatReadsTheStore(t *testing.T) {
	f := newFixture(t, filepath.Join(t.TempDir(), "codectx.db"))
	a1 := f.file("pkg/a.go", "package pkg\nfunc F() {}\n")
	b := f.file("pkg/b.go", "package pkg\nfunc F() { F() }\n")
	snap1 := f.snapshot("one", a1, b)
	gen1, err := f.s.BeginGeneration(f.ctx, f.repo, snap1.ID, model.H("semantic"), "main")
	if err != nil {
		t.Fatal(err)
	}
	run1 := f.run(gen1)
	w1 := f.beginScope(gen1, run1, configHash, a1, b)
	f.fillScope(w1, run1, a1, b)
	if err := f.s.SealUnit(f.ctx, w1); err != nil {
		t.Fatalf("SealUnit: %v", err)
	}
	// The predecessor is committed, as it is by the activation of the
	// generation it belongs to, before the run that carries from it begins.
	flushed(t, f.s)

	a2 := f.file("pkg/a.go", "package pkg\nfunc F() { /* edited */ }\n")
	snap2 := f.snapshot("two", a2, b)
	gen2, err := f.s.BeginGeneration(f.ctx, f.repo, snap2.ID, model.H("semantic"), "main")
	if err != nil {
		t.Fatal(err)
	}
	run2 := f.run(gen2)
	w2 := f.beginScope(gen2, run2, "cfg-delta", a2, b)
	f.fillFile(w2, run2, a2)
	f.fillIndexLevel(w2, run2, a2)

	var streamErr error
	replaced := store.Replaced{
		Files: func(yield func(model.FileID) bool) {
			// The predecessor declared a.go with other bytes: replaced.
			for in, err := range f.s.UnitInputs(f.ctx, w1.UnitID()) {
				if err != nil {
					streamErr = err
					return
				}
				if in.FileID == a2.id && !yield(in.FileID) {
					return
				}
			}
		},
		Scopes: slices.Values([]string{"file:" + a2.path}),
		Keys:   slices.Values(keyList("key:node:" + a2.path)),
	}
	type outcome struct {
		stats store.CarryOverStats
		err   error
	}
	done := make(chan outcome, 1)
	go func() {
		stats, err := w2.CarryOver(f.ctx, w1.UnitID(), replaced)
		done <- outcome{stats, err}
	}()
	select {
	case out := <-done:
		if out.err != nil {
			t.Fatalf("CarryOver: %v", out.err)
		}
		if out.stats.Nodes != 1 || out.stats.SearchUnits != 1 || out.stats.Aliases != 1 {
			t.Errorf("carried %+v, want b.go's node, alias and document", out.stats)
		}
	case <-time.After(time.Minute):
		t.Fatal("CarryOver did not return within a minute: the stream's store read is waiting on the group its own call holds")
	}
	if streamErr != nil {
		t.Fatalf("replaced-file stream: %v", streamErr)
	}
	if err := f.s.SealUnit(f.ctx, w2); err != nil {
		t.Fatalf("SealUnit: %v", err)
	}
}
