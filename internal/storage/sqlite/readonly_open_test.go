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

// A writerless pin is a SNAPSHOT, not a lease, and the two things that follow
// from that are what this protects.
//
// A query that cannot take a lease used to be refused any generation but the
// active one, which made a continuation unanswerable the moment the run it was
// minted against published the next generation -- the exact instant a second
// process is most likely to be asking. It is allowed now because the lease was
// never what a read needed: Store.read runs one deferred read transaction per
// call, so within a call the log snapshot is fixed and a collection in another
// process cannot take rows out from under the read.
//
// Across calls the generation really can go, and that is the second half: the
// pin that finds no row must be recognisable as a COLLECTED generation, because
// that is what a continuation naming it turns into CTX_CURSOR_INVALID. Reported
// as an ordinary invalid argument it would tell the caller it typed something
// wrong, when what it presented was a token this process handed it.
//
// Mutation that fails this test: restore the read-only refusal of a
// non-active generation in PinGeneration, or drop the generation_state detail
// from the missing-generation refusal.
func TestReadOnlyPinHoldsASupersededGenerationAndNamesItsCollection(t *testing.T) {
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

	// A second generation is published, which is what supersedes the first.
	a2 := f.file("pkg/a.go", "package pkg\nfunc A() { changed() }\n")
	snap2 := f.snapshot("two", a2)
	gen2, err := f.s.BeginGeneration(ctx, f.repo, snap2.ID, model.H("semantic"), "main")
	if err != nil {
		t.Fatalf("BeginGeneration(2): %v", err)
	}
	run2 := f.run(gen2)
	f.unit(gen2, run2, a2)
	if err := f.s.CompleteProviderRun(ctx, model.ProviderResult{RunID: run2, State: model.RunSucceeded, RecordsEmitted: 1, BytesProcessed: 24}, ""); err != nil {
		t.Fatalf("CompleteProviderRun(2): %v", err)
	}
	f.activate(gen2, gen)

	reader, err := store.Open(ctx, dbPath, store.Options{ReadOnly: true, BusyTimeout: 500 * time.Millisecond})
	if err != nil {
		t.Fatalf("read-only Open: %v", err)
	}
	defer reader.Close()

	pinned, err := reader.PinGeneration(ctx, f.repo, gen, time.Minute)
	if err != nil {
		t.Fatalf("a writerless pin of the superseded generation a continuation names was refused: %v", err)
	}
	if pinned.LeaseID() != "" {
		t.Fatalf("a read-only pin took retention lease %q", pinned.LeaseID())
	}
	nodes, err := pinned.NodesInFile(ctx, a.id, 0, "", 10)
	if err != nil {
		t.Fatalf("NodesInFile through a writerless pin of a superseded generation: %v", err)
	}
	if len(nodes) != 1 {
		t.Fatalf("the superseded generation answered %d nodes, want 1", len(nodes))
	}
	pinned.Close()

	// The other process collects it. Nothing retains it -- that is the point of
	// a pin that takes no lease -- so the collection succeeds.
	if err := f.s.DeleteGeneration(ctx, gen); err != nil {
		t.Fatalf("DeleteGeneration: %v", err)
	}
	_, err = reader.PinGeneration(ctx, f.repo, gen, time.Minute)
	if err == nil {
		t.Fatal("a pin of a collected generation was accepted")
	}
	if !store.IsGenerationCollected(err) {
		t.Fatalf("a pin of a collected generation is not recognisable as one, so a continuation cannot answer CTX_CURSOR_INVALID: %v", err)
	}
}
