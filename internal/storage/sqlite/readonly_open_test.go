package sqlite_test

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"os"
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
	// The pin wrote no lease row: that is the write it used to perform, and a
	// row here would mean the reader had taken the writer connection after all.
	// Counted in the database rather than asked of the reader, so the assertion
	// survives the reader having no opinion about leases at all.
	wantNoLeaseRow(t, ctx, f)
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
	wantNoLeaseRow(t, ctx, f)
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

// wantNoLeaseRow fails unless the database holds no retention lease. It reads
// through the fixture's writing store because the count is a fact about the
// database, not about the reader under test.
func wantNoLeaseRow(t *testing.T, ctx context.Context, f *fixture) {
	t.Helper()
	st, err := f.s.Stats(ctx)
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}
	if st.Leases != 0 {
		t.Fatalf("a writerless pin left %d retention leases", st.Leases)
	}
}

// digestTriple is the length and content digest of a store's three files: the
// database, its write-ahead log and the shared-memory index beside it. A file
// that is not there is reported as absent, which is itself an answer: the
// engine's close unlinks the log and the index, and an absence where there was
// a file is the write this test exists to catch.
func digestTriple(t *testing.T, dbPath string) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, suffix := range []string{"", "-wal", "-shm"} {
		path := dbPath + suffix
		b, err := os.ReadFile(path)
		if err != nil {
			if !errors.Is(err, fs.ErrNotExist) {
				t.Fatalf("reading %s: %v", filepath.Base(path), err)
			}
			out[suffix] = "absent"
			continue
		}
		sum := sha256.Sum256(b)
		out[suffix] = fmt.Sprintf("%d bytes %x", len(b), sum[:8])
	}
	return out
}

// A command that answers questions must leave the workspace byte for byte as it
// found it. `Options.ReadOnly` opened no writer connection and set query_only,
// which refuses writes through SQL -- and left the connection read-WRITE at the
// file level, so the last one to close ran the engine's log close: the log
// checkpointed into the database, and the log and the shared-memory index were
// unlinked. A `status` on a workspace whose finished run had left an 82 MB log
// grew the database from 12,865,536 to 94,973,952 bytes and removed both.
//
// That is a write of the whole published index by a command whose help, and
// docs/operations.md, both promise it writes nothing. It is also destructive
// under a concurrent run: the checkpoint and the unlink are performed holding
// the database exclusively, which is minutes of held lock on a large log.
//
// The store is copied aside with its log and index while the writer that made
// them is open and between transactions, which is how a test gets the state a
// crashed or killed run leaves: a database with a live log and no writer.
//
// Mutation that fails this test: drop `mode=ro` from the read-only pools'
// connection string. The digests then differ and the log and index are absent.
func TestAReadOnlyOpenLeavesTheDatabaseItsLogAndItsIndexUntouched(t *testing.T) {
	ctx := context.Background()
	src := filepath.Join(t.TempDir(), "codectx.db")
	f := newFixture(t, src)
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

	// A HOT log: one whose frames the database does not hold. Every commit
	// this store makes ends in a passive checkpoint, which folds the group's
	// frames into the database -- so a log copied from an ordinary fixture
	// holds nothing the database has not got, and a close that folded it would
	// copy zero bytes and prove nothing about the real case. A reader holding
	// a snapshot is what stops a passive checkpoint advancing, so one is held
	// here across a second generation's commit.
	snapshotHolder, err := sql.Open("sqlite", "file:"+src+"?_txlock=deferred")
	if err != nil {
		t.Fatal(err)
	}
	held, err := snapshotHolder.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	var seen int
	if err := held.QueryRowContext(ctx, `SELECT count(*) FROM generations`).Scan(&seen); err != nil {
		t.Fatal(err)
	}
	b2 := f.file("pkg/b.go", "package pkg\nfunc B() {}\n")
	snap2 := f.snapshot("two", a, b2)
	gen2, err := f.s.BeginGeneration(ctx, f.repo, snap2.ID, model.H("semantic"), "main")
	if err != nil {
		t.Fatalf("BeginGeneration(2): %v", err)
	}
	run2 := f.run(gen2)
	f.unit(gen2, run2, b2)
	if err := f.s.CompleteProviderRun(ctx, model.ProviderResult{RunID: run2, State: model.RunSucceeded, RecordsEmitted: 1, BytesProcessed: 24}, ""); err != nil {
		t.Fatalf("CompleteProviderRun(2): %v", err)
	}
	f.activate(gen2, gen)
	if err := f.s.Flush(ctx); err != nil {
		t.Fatalf("Flush: %v", err)
	}

	// The copy, taken with the writer open, no transaction in flight, and the
	// second generation's frames still only in the log.
	dst := filepath.Join(t.TempDir(), "codectx.db")
	for _, suffix := range []string{"", "-wal", "-shm"} {
		b, err := os.ReadFile(src + suffix)
		if err != nil {
			t.Fatalf("a live store has no %s beside it: %v", suffix, err)
		}
		if err := os.WriteFile(dst+suffix, b, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	_ = held.Rollback()
	snapshotHolder.Close()
	before := digestTriple(t, dst)
	for _, suffix := range []string{"-wal", "-shm"} {
		if before[suffix] == "absent" {
			t.Fatalf("the copied store has no %s, so this test would not exercise the log close at all", suffix)
		}
	}
	// The log is hot, proved rather than assumed: the database ALONE answers
	// with the first generation's single file, and only the log carries the
	// second. A close that folded the log would therefore change the database
	// by more than the rewind, which is the damage this test bounds.
	onlyDB := filepath.Join(t.TempDir(), "codectx.db")
	dbBytes, err := os.ReadFile(dst)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(onlyDB, dbBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	coldGen := activeGeneration(t, ctx, onlyDB, f.repo)
	if coldGen != gen {
		t.Fatalf("the database alone already holds generation %d; the log's frames were folded and this "+
			"test would not bound the close that copies them", coldGen)
	}

	reader, err := store.Open(ctx, dst, store.Options{ReadOnly: true})
	if err != nil {
		t.Fatalf("a read-only open of a store with a live log was refused: %v", err)
	}
	pinned, err := reader.PinGeneration(ctx, f.repo, 0, time.Minute)
	if err != nil {
		t.Fatalf("PinGeneration through a read-only open of a store with a live log: %v", err)
	}
	if pinned.Binding().GenerationID != gen2 {
		t.Fatalf("the read-only open pinned generation %d, want the %d only the log holds: it is reading "+
			"the database without the log", pinned.Binding().GenerationID, gen2)
	}
	nodes, err := pinned.NodesInFile(ctx, b2.id, 0, "", 10)
	if err != nil {
		t.Fatalf("NodesInFile: %v", err)
	}
	if len(nodes) != 1 {
		t.Fatalf("the read-only open answered with %d nodes for a file only the log carries, want 1", len(nodes))
	}
	pinned.Close()
	// After the close, which is where the engine's log close runs: the last
	// connection to let go of a read-write handle is the one that checkpoints
	// and unlinks.
	if err := reader.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	after := digestTriple(t, dst)
	for _, suffix := range []string{"", "-wal"} {
		if before[suffix] != after[suffix] {
			t.Fatalf("a read-only open changed %q: %s before, %s after",
				"codectx.db"+suffix, before[suffix], after[suffix])
		}
	}
	// The shared-memory index is the one file a reader does touch, and it is
	// not workspace content: it holds no database bytes at all, it is the
	// index OF the log and is rebuilt from the log, and the region a reader
	// writes is the read mark by which it takes its snapshot without blocking
	// the writer. Measured here: fewer than a dozen bytes differ, every one of
	// them inside the first 136 -- the two index header copies and the
	// read-mark block -- and the count varies with how the snapshot lands.
	// What must not happen is what the log close does: the file unlinked, or
	// resized. Asserting its bytes identical would be asserting that a reader
	// takes no snapshot.
	if strings.Fields(after["-shm"])[0] != strings.Fields(before["-shm"])[0] {
		t.Fatalf("a read-only open removed or resized the shared-memory index: %s before, %s after",
			before["-shm"], after["-shm"])
	}
}

// activeGeneration opens a store read-only and reports the generation it finds
// active, which is how a copy of the database alone is asked what it holds
// without its log.
func activeGeneration(t *testing.T, ctx context.Context, path string, repo model.RepositoryID) model.GenerationID {
	t.Helper()
	s, err := store.Open(ctx, path, store.Options{ReadOnly: true})
	if err != nil {
		t.Fatalf("a read-only open of %s: %v", filepath.Base(path), err)
	}
	defer s.Close()
	pinned, err := s.PinGeneration(ctx, repo, 0, time.Minute)
	if err != nil {
		t.Fatalf("PinGeneration on %s: %v", filepath.Base(path), err)
	}
	defer pinned.Close()
	return pinned.Binding().GenerationID
}

// A read-only open of a workspace nothing has ever published must say so, and
// must not bring the workspace into being on the way. The open used to create
// the database file -- the engine's own create flag -- and learn from the empty
// schema that nothing was published; the file it left behind was a write by a
// command that promises none, and the next writing open then adopted it.
//
// Mutation that fails this test: return the raw open failure instead of the
// typed answer, or restore the creating open, and the assertion on the absent
// file or on the code fails.
func TestAReadOnlyOpenOfAWorkspaceNeverIndexedCreatesNothing(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "codectx.db")
	_, err := store.Open(ctx, dbPath, store.Options{ReadOnly: true})
	if err == nil {
		t.Fatal("a read-only open of a workspace that has published nothing succeeded")
	}
	wantCode(t, err, model.CodeNoActiveGeneration)
	if _, statErr := os.Stat(dbPath); !errors.Is(statErr, fs.ErrNotExist) {
		t.Fatalf("the read-only open left a database behind: %v", statErr)
	}
}
