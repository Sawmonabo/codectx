package sqlite_test

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Sawmonabo/codectx/internal/model"
	store "github.com/Sawmonabo/codectx/internal/storage/sqlite"
)

// A second process must be able to read a published workspace while a run holds
// the ingestion group's write transaction. Every read-only command used to
// begin a write of its own before it read anything -- the schema check at open,
// and the retention lease every pinned generation inserted -- and both are
// `_txlock=immediate` on the single writer connection, so a second process was
// refused after the busy timeout for the whole length of an index: `status`,
// every query and every tool an agent called answered nothing exactly while the
// workspace was being refreshed.
//
// Two stores over one file are two independent connection sets, which is what
// two processes are to the engine, so the refusal reproduces here exactly as it
// did between processes.
//
// Mutation that fails this test: open the second store with ReadOnly false (the
// composition a mutating command uses). Open is then refused
// "database is busy: begin" while the group is open.
func TestReadOnlyStoreReadsWhileAnotherHoldsTheWriteTransaction(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "codectx.db")
	f := newFixture(t, dbPath)

	a := f.file("pkg/a.go", "package pkg\nfunc A() {}\n")
	snap := f.snapshot("one", a)
	gen, err := f.s.BeginGeneration(ctx, f.repo, snap.ID, model.H("semantic"), "main")
	if err != nil {
		t.Fatalf("BeginGeneration: %v", err)
	}
	run := f.run(gen)
	f.unit(gen, run, a)
	if err := f.s.CompleteProviderRun(ctx, model.ProviderResult{RunID: run, State: model.RunSucceeded, RecordsEmitted: 1, BytesProcessed: 24}, ""); err != nil {
		t.Fatalf("CompleteProviderRun: %v", err)
	}
	f.activate(gen, 0)

	// The state a run spends most of its wall clock in: a second generation
	// staging, with the ingestion group's write transaction open and unflushed.
	a2 := f.file("pkg/a.go", "package pkg\nfunc A() { changed() }\n")
	snap2 := f.snapshot("two", a2)
	gen2, err := f.s.BeginGeneration(ctx, f.repo, snap2.ID, model.H("semantic"), "main")
	if err != nil {
		t.Fatalf("BeginGeneration(2): %v", err)
	}
	run2 := f.run(gen2)
	w := f.begin(gen2, run2, a2)
	f.fill(w, run2, a2)

	// A short busy timeout so the mutation fails fast rather than waiting out
	// the five-second default; it is also what makes "answers at once" a claim
	// this test can make.
	opts := store.Options{ReadOnly: true, BusyTimeout: 500 * time.Millisecond}
	reader, err := store.Open(ctx, dbPath, opts)
	if err != nil {
		t.Fatalf("a read-only open was refused while the writer held its group: %v", err)
	}
	defer reader.Close()

	pinned, err := reader.PinGeneration(ctx, f.repo, 0, time.Minute)
	if err != nil {
		t.Fatalf("a read-only pin of the active generation was refused while the writer held its group: %v", err)
	}
	defer pinned.Close()
	if pinned.Binding().GenerationID != gen {
		t.Fatalf("pinned generation = %d, want the active %d", pinned.Binding().GenerationID, gen)
	}
	// The pin took no lease: that is the write it used to perform, and a lease
	// row here would mean the reader had taken the writer connection after all.
	if pinned.LeaseID() != "" {
		t.Fatalf("a read-only pin took retention lease %q", pinned.LeaseID())
	}
	// And it reads: the unsealed second generation is invisible, the published
	// one answers.
	nodes, err := pinned.NodesInFile(ctx, a.id, 0, "", 10)
	if err != nil {
		t.Fatalf("NodesInFile through a read-only pin: %v", err)
	}
	if len(nodes) != 1 {
		t.Fatalf("read-only pin returned %d nodes, want 1", len(nodes))
	}

	// Nothing the read-only store offers can change the workspace, and it says
	// so with a typed refusal rather than reaching for a writer it never opened.
	err = reader.EnsureRepository(ctx, f.repo, "/repo")
	if err == nil {
		t.Fatal("a read-only store accepted a write")
	}
	wantCode(t, err, model.CodeInternal)
	if !strings.Contains(err.Error(), "read-only") {
		t.Fatalf("read-only refusal does not name the cause: %v", err)
	}
}
