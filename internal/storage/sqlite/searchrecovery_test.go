package sqlite_test

import (
	"errors"
	"strconv"
	"testing"
	"time"

	"github.com/Sawmonabo/codectx/internal/model"
	"github.com/Sawmonabo/codectx/internal/search/textindex"
	store "github.com/Sawmonabo/codectx/internal/storage/sqlite"
)

// searchMatches is how many visible documents of gen match term.
func searchMatches(t *testing.T, f *fixture, gen model.GenerationID, term string) int64 {
	t.Helper()
	r, err := f.s.PinGeneration(f.ctx, f.repo, gen, time.Minute)
	if err != nil {
		t.Fatalf("PinGeneration(%d): %v", gen, err)
	}
	defer r.Close()
	view, err := r.SearchView(f.ctx)
	if err != nil {
		t.Fatalf("SearchView(%d): %v", gen, err)
	}
	defer view.Close()
	page, err := view.Search(f.ctx, textindex.Request{Query: term, Depth: 100})
	if err != nil {
		t.Fatalf("Search(%q): %v", term, err)
	}
	if !page.MatchesExact {
		t.Fatalf("Search(%q) counted %d matches as a lower bound on a fixture every block of which is visited", term, page.Matches)
	}
	return page.Matches
}

// reopen closes f's store as a process that ends would and opens the same
// database in a new store, keeping the fixture's files.
func reopen(t *testing.T, f *fixture, dbPath string) *fixture {
	t.Helper()
	if err := f.s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	g := newFixture(t, dbPath)
	g.files = f.files
	return g
}

// TestACrashBetweenTheSearchCommitAndTheActivationRecoversTheLastGeneration
// stops an activation after the search index committed generation 2 and
// before SQLite activated it, as if the process died there, then recovers.
//
// Requirement: the library commits first and SQLite activates after it, so
// the crash leaves a library commit ahead of SQLite and never the reverse.
// Recover must leave generation 1 active and searched as generation 1 --
// generation 2's documents are in the library but in no visible set -- with
// nothing pending, and a later activation of the same units must publish
// their documents.
//
// Store.Close commits the open ingestion group, so the sealed units of the
// stopped generation survive the "crash" exactly as a committed group would;
// Recover then aborts the staging generation and keeps them reusable.
//
// Mutation that fails it: activate in SQLite before the library commit (the
// stopped activation then leaves generation 2 active over an index that never
// committed it, and its search answers CTX_STORAGE_CORRUPT), or skip the
// reconcile in Recover (generation 2's units stay marked pending).
func TestACrashBetweenTheSearchCommitAndTheActivationRecoversTheLastGeneration(t *testing.T) {
	dbPath := t.TempDir() + "/codectx.db"
	f := newFixture(t, dbPath)
	a1 := f.file("pkg/a.go", "package pkg\nfunc F() { zqxfirst() }\n")
	snap1 := f.snapshot("one", a1)
	gen1, err := f.s.BeginGeneration(f.ctx, f.repo, snap1.ID, model.H("semantic"), "main")
	if err != nil {
		t.Fatal(err)
	}
	f.unit(gen1, f.run(gen1), a1)
	f.activate(gen1, 0)

	a2 := f.file("pkg/a.go", "package pkg\nfunc F() { zqxsecond() }\n")
	snap2 := f.snapshot("two", a2)
	gen2, err := f.s.BeginGeneration(f.ctx, f.repo, snap2.ID, model.H("semantic"), "main")
	if err != nil {
		t.Fatal(err)
	}
	unit2 := f.unit(gen2, f.run(gen2), a2)
	restore := store.StopAfterSearchCommit()
	_, err = f.s.Activate(f.ctx, gen2, gen1, model.HealthFresh, fixtureCaps, "norm-v1")
	restore()
	if !errors.Is(err, store.ErrStoppedAfterSearchCommit) {
		t.Fatalf("Activate(%d) = %v, want it stopped after the search index commit", gen2, err)
	}
	if payload, err := f.s.SearchCommitted(); err != nil || payload != strconv.FormatInt(int64(gen2), 10) {
		t.Fatalf("the search index's last commit is %q (%v), want generation %d committed before SQLite", payload, err, gen2)
	}

	f = reopen(t, f, dbPath)
	if err := f.s.Recover(f.ctx, time.Now()); err != nil {
		t.Fatalf("Recover: %v", err)
	}
	if active, err := f.s.ActiveGeneration(f.ctx, f.repo); err != nil || active != gen1 {
		t.Fatalf("after Recover the active generation is %d (%v), want %d", active, err, gen1)
	}
	if pending, err := f.s.PendingUnits(f.ctx); err != nil || pending != 0 {
		t.Fatalf("after Recover %d units are still pending (%v), want none", pending, err)
	}
	if n := searchMatches(t, f, gen1, "zqxfirst"); n != 1 {
		t.Fatalf("generation %d matches zqxfirst %d times, want its own one document", gen1, n)
	}
	if n := searchMatches(t, f, gen1, "zqxsecond"); n != 0 {
		t.Fatalf("generation %d matches zqxsecond %d times: the stopped generation's document is visible in it", gen1, n)
	}

	snap3 := f.snapshot("three", a2)
	gen3, err := f.s.BeginGeneration(f.ctx, f.repo, snap3.ID, model.H("semantic"), "main")
	if err != nil {
		t.Fatal(err)
	}
	if err := f.s.AttachUnit(f.ctx, gen3, unit2); err != nil {
		t.Fatalf("AttachUnit(%s): %v", unit2, err)
	}
	f.activate(gen3, gen1)
	if n := searchMatches(t, f, gen3, "zqxsecond"); n != 1 {
		t.Fatalf("generation %d matches zqxsecond %d times, want the reused unit's one document", gen3, n)
	}
	if n := searchMatches(t, f, gen3, "zqxfirst"); n != 0 {
		t.Fatalf("generation %d matches zqxfirst %d times: a unit it does not select is visible", gen3, n)
	}
}

// TestAUnitWhoseDocumentsNeverReachedACommitIsRebuilt seals a unit whose rows
// are durable in SQLite while the search index's writer closes without a
// commit, as a process that died before its activation leaves it: the
// documents the unit added are gone from the library and its rows still name
// them.
//
// Requirement: Recover compares every pending unit's mark with what the
// library's last commit holds and fails a unit that differs, so the planner
// rebuilds it like any unit that is not sealed; a sealed unit with missing
// documents would otherwise be reused and published with a search tier that
// silently lacks it. Nothing the lost unit added may be visible afterwards:
// the rebuilt unit's generation holds exactly one matching document.
//
// Mutation that fails it: never check the pending marks in Recover -- the
// unit stays sealed and is reused, and its generation matches nothing.
func TestAUnitWhoseDocumentsNeverReachedACommitIsRebuilt(t *testing.T) {
	dbPath := t.TempDir() + "/codectx.db"
	f := newFixture(t, dbPath)
	a := f.file("pkg/a.go", "package pkg\nfunc F() { zqxlost() }\n")
	snap := f.snapshot("one", a)
	gen1, err := f.s.BeginGeneration(f.ctx, f.repo, snap.ID, model.H("semantic"), "main")
	if err != nil {
		t.Fatal(err)
	}
	unit := f.unit(gen1, f.run(gen1), a)
	flushed(t, f.s)
	if pending, err := f.s.SearchPending(f.ctx, unit); err != nil || pending != 1 {
		t.Fatalf("the sealed unit's pending mark is %d (%v), want its one document", pending, err)
	}
	if err := f.s.DiscardSearchAdds(); err != nil {
		t.Fatalf("DiscardSearchAdds: %v", err)
	}

	f = reopen(t, f, dbPath)
	if err := f.s.Recover(f.ctx, time.Now()); err != nil {
		t.Fatalf("Recover: %v", err)
	}
	if state, exists, err := f.s.UnitState(f.ctx, unit); err != nil || (exists && state != model.UnitFailed) {
		t.Fatalf("after Recover the unit whose documents were lost is %q (exists %v, %v), want failed or collected", state, exists, err)
	}
	if pending, err := f.s.PendingUnits(f.ctx); err != nil || pending != 0 {
		t.Fatalf("after Recover %d units are still pending (%v), want none", pending, err)
	}

	// Re-planned: the same unit identity is built again, not refused as an
	// existing unit, and its generation serves exactly its one document.
	gen2, err := f.s.BeginGeneration(f.ctx, f.repo, snap.ID, model.H("semantic"), "main")
	if err != nil {
		t.Fatal(err)
	}
	if rebuilt := f.unit(gen2, f.run(gen2), a); rebuilt != unit {
		t.Fatalf("the rebuilt unit is %s, want the same identity %s", rebuilt, unit)
	}
	f.activate(gen2, 0)
	if n := searchMatches(t, f, gen2, "zqxlost"); n != 1 {
		t.Fatalf("the rebuilt generation matches zqxlost %d times, want exactly the rebuilt unit's one document", n)
	}
}
