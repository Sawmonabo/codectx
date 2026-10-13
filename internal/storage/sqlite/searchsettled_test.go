package sqlite_test

import (
	"context"
	"database/sql"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Sawmonabo/codectx/internal/model"
	"github.com/Sawmonabo/codectx/internal/search/textindex"
	store "github.com/Sawmonabo/codectx/internal/storage/sqlite"
)

// TestRecoverOfASettledStoreOpensNoSearchWriter recovers a store whose last
// run activated cleanly: the library's last commit is the active generation,
// no unit is pending and no delete is queued, so there is nothing to
// reconcile. Recover must not open the search writer -- its memory admission,
// its writer lock and a commit's sync are paid on every recovery otherwise,
// and a second process holding the writer would make the recovery wait on it.
//
// Mutation that fails it: drop the searchSettled skip in reconcileSearch (the
// reconcile's delete of unallocated doc ids opens the writer and Reserve is
// called).
func TestRecoverOfASettledStoreOpensNoSearchWriter(t *testing.T) {
	dbPath := t.TempDir() + "/codectx.db"
	f := newFixture(t, dbPath)
	a := f.file("pkg/a.go", "package pkg\nfunc F() {}\n")
	gen, err := f.s.BeginGeneration(f.ctx, f.repo, f.snapshot("one", a).ID, model.H("semantic"), "main")
	if err != nil {
		t.Fatal(err)
	}
	f.unit(gen, f.run(gen), a)
	f.activate(gen, 0)
	if err := f.s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	var reserved atomic.Int64
	reopened := newFixtureWithOptions(t, dbPath, store.Options{TextIndex: textindex.Options{
		Reserve: func(context.Context, int64) (func(), error) {
			reserved.Add(1)
			return func() {}, nil
		},
	}})
	if err := reopened.s.Recover(reopened.ctx, time.Now()); err != nil {
		t.Fatalf("Recover: %v", err)
	}
	if n := reserved.Load(); n != 0 {
		t.Fatalf("Recover of a settled store opened the search writer %d times, want none", n)
	}
}

// TestRetentionCountsTheUnitCapabilityRows sweeps one superseded generation
// from two otherwise identical stores, one whose swept unit stored a
// capability row with a long remediation, and compares the bytes each reports
// reclaimed. The row is the unit's and goes with it, so the store holding it
// must report at least its text more.
//
// Mutation that fails it: leave unit_capabilities out of generationBytes (both
// stores report the same bytes).
func TestRetentionCountsTheUnitCapabilityRows(t *testing.T) {
	remediation := strings.Repeat("r", 1000)
	reclaimed := func(withCapability bool) int64 {
		f := newFixture(t, t.TempDir()+"/codectx.db")
		a := f.file("pkg/a.go", "package pkg\nfunc F() {}\n")
		b := f.file("pkg/b.go", "package pkg\nfunc G() {}\n")
		old, err := f.s.BeginGeneration(f.ctx, f.repo, f.snapshot("old", a).ID, model.H("semantic"), "old")
		if err != nil {
			t.Fatal(err)
		}
		run := f.run(old)
		w := f.begin(old, run, a)
		f.fill(w, run, a)
		if withCapability {
			if err := w.SetCapabilities([]model.CapabilityState{{ProviderID: providerID, Capability: "structure",
				Scope: a.path, State: model.CapabilityPartial, DiagnosticCode: "CTX_PARTIAL", Remediation: remediation}}); err != nil {
				t.Fatalf("SetCapabilities: %v", err)
			}
		}
		if err := f.s.SealUnit(f.ctx, w); err != nil {
			t.Fatalf("SealUnit: %v", err)
		}
		f.activate(old, 0)
		next, err := f.s.BeginGeneration(f.ctx, f.repo, f.snapshot("new", b).ID, model.H("semantic"), "new")
		if err != nil {
			t.Fatal(err)
		}
		f.unit(next, f.run(next), b)
		f.activate(next, old)
		report, err := f.s.RetainByRef(f.ctx, f.repo, store.RetentionPolicy{RetainRefs: 1}, time.Now())
		if err != nil {
			t.Fatalf("RetainByRef: %v", err)
		}
		if report.GenerationsSwept != 1 {
			t.Fatalf("RetainByRef swept %d generations, want the superseded one", report.GenerationsSwept)
		}
		return report.BytesReclaimed
	}
	without, with := reclaimed(false), reclaimed(true)
	if with-without < int64(len(remediation)) {
		t.Fatalf("a swept unit's capability row added %d reclaimed bytes, want at least its %d-byte remediation",
			with-without, len(remediation))
	}
}

// TestACollectionNeverCommitsOverAnUnfinishedActivation stops an activation
// after the search index committed generation 2, as a failed or interrupted
// activation leaves it, then collects that generation's unit in the same
// process before any recovery. The collection queues the unit's document
// deletes and must leave them queued: committing them with the active
// generation as payload would lower the library's payload from 2 back to 1,
// and the payload ahead of SQLite is the one durable mark that the library
// may hold documents of an ingestion group SQLite lost. With it erased, the
// next Recover would find the index settled and keep them.
//
// Mutation that fails it: drop the searchAhead guard from commitSweepDeletes
// (the payload reads 1 after the collection).
func TestACollectionNeverCommitsOverAnUnfinishedActivation(t *testing.T) {
	dbPath := t.TempDir() + "/codectx.db"
	f := newFixture(t, dbPath)
	a1 := f.file("pkg/a.go", "package pkg\nfunc F() { zqxone() }\n")
	gen1, err := f.s.BeginGeneration(f.ctx, f.repo, f.snapshot("one", a1).ID, model.H("semantic"), "main")
	if err != nil {
		t.Fatal(err)
	}
	f.unit(gen1, f.run(gen1), a1)
	f.activate(gen1, 0)

	a2 := f.file("pkg/a.go", "package pkg\nfunc F() { zqxtwo() }\n")
	gen2, err := f.s.BeginGeneration(f.ctx, f.repo, f.snapshot("two", a2).ID, model.H("semantic"), "main")
	if err != nil {
		t.Fatal(err)
	}
	f.unit(gen2, f.run(gen2), a2)
	restore := store.StopAfterSearchCommit()
	_, err = f.s.Activate(f.ctx, gen2, gen1, model.HealthFresh, fixtureCaps, "norm-v1")
	restore()
	if err == nil {
		t.Fatal("the stopped activation succeeded")
	}
	if err := f.s.Abort(f.ctx, gen2); err != nil {
		t.Fatalf("Abort(%d): %v", gen2, err)
	}
	if err := f.s.DeleteGeneration(f.ctx, gen2); err != nil {
		t.Fatalf("DeleteGeneration(%d): %v", gen2, err)
	}
	// The collection did queue deletes, so a commit was due and declined.
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var queued int64
	if err := db.QueryRowContext(f.ctx, `SELECT count(*) FROM search_doc_deletes`).Scan(&queued); err != nil || queued == 0 {
		t.Fatalf("the collection queued %d search document deletes (%v), want the collected unit's", queued, err)
	}
	if payload, err := f.s.SearchCommitted(); err != nil || payload != strconv.FormatInt(int64(gen2), 10) {
		t.Fatalf("after the collection the search index's last commit is %q (%v), want generation %d kept for recovery",
			payload, err, gen2)
	}
}
