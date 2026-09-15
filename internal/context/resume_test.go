package context

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/Sawmonabo/codectx/internal/config"
	"github.com/Sawmonabo/codectx/internal/model"
	"github.com/Sawmonabo/codectx/internal/pagination"
)

// resumeArea opens a compile sort area and a state directory beside it, as a
// compile that checkpoints does: both under one root, so the rename a
// checkpointed sort performs is the same-filesystem move production makes.
func resumeArea(t *testing.T) (*compileSorts, string) {
	t.Helper()
	root := t.TempDir()
	sortDir := filepath.Join(root, "sorts")
	stateDir := filepath.Join(root, "state")
	for _, d := range []string{sortDir, stateDir} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatalf("mkdir %s: %v", d, err)
		}
	}
	sorts, err := newCompileSorts(config.Config{}, sortDir)
	if err != nil {
		t.Fatalf("newCompileSorts: %v", err)
	}
	// A tiny run budget forces the SPILLED branch of every sort below. The
	// round trip is trivially exact when everything fits one buffer; the branch
	// that matters is the one where order comes out of a merge.
	sorts.runBytes = 1
	t.Cleanup(func() { _ = sorts.Close() })
	return sorts, stateDir
}

func drainCounts(t *testing.T, run *pagination.SortedRun[pkgCountRec]) []pkgCountRec {
	t.Helper()
	var out []pkgCountRec
	if err := run.Each(func(r pkgCountRec) error { out = append(out, r); return nil }); err != nil {
		t.Fatalf("drain: %v", err)
	}
	return out
}

// TestCheckpointedRunRestoresIdenticallyAfterSpilling is ruling C7's core
// invariant: a stream a completed pass produced must survive the interruption
// unchanged, or the resumed compile does not answer the plan an uninterrupted
// one would. It is asserted on the spilled branch, where the restored order is
// produced by a merge over adopted runs rather than by one in-heap sort.
func TestCheckpointedRunRestoresIdenticallyAfterSpilling(t *testing.T) {
	sorts, stateDir := resumeArea(t)

	// Built already folded, exactly as P-E hands its counts on: equal
	// neighbours are collapsed before the checkpoint, so the restore must NOT
	// re-apply the fold.
	sorter, err := newSort(sorts, "pkg-count", lessPkg, sizeOfPkgCount)
	if err != nil {
		t.Fatalf("newSort: %v", err)
	}
	sorter = sorter.WithFold(foldPkgCount)
	const packages = 4000
	for i := packages - 1; i >= 0; i-- {
		// Each package is added twice so the fold has something to collapse:
		// a restore that re-folded would halve nothing, but a restore that
		// re-folded a SUM would double these edge counts.
		for range 2 {
			if err := sorter.Add(pkgCountRec{Pkg: fmt.Sprintf("pkg/%04d", i), Edges: 3}); err != nil {
				t.Fatalf("add: %v", err)
			}
		}
	}
	run, err := sortedRun(sorts, sorter)
	if err != nil {
		t.Fatalf("sorted: %v", err)
	}
	want := drainCounts(t, run)
	if len(want) != packages {
		t.Fatalf("fixture folded to %d packages, want %d", len(want), packages)
	}

	files, err := checkpointRun(stateDir, "pkg-count", sorts.runBytes, run, lessPkg, sizeOfPkgCount)
	if err != nil {
		t.Fatalf("checkpointRun: %v", err)
	}
	if len(files) < 2 {
		t.Fatalf("the fixture did not spill: %d run files, want at least 2", len(files))
	}

	restored, err := restoreRun(sorts, stateDir, "pkg-count", files, lessPkg, sizeOfPkgCount)
	if err != nil {
		t.Fatalf("restoreRun: %v", err)
	}
	got := drainCounts(t, restored)
	if len(got) != len(want) {
		t.Fatalf("restored %d records, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("record %d restored as %+v, want %+v", i, got[i], want[i])
		}
	}
}

// TestCheckpointedSortKeepsItsFoldAcrossTheInterruption covers the other class:
// a sort the interrupted pass had not ended yet is PRE-fold, so the resuming
// call must re-attach exactly the fold it was built with. Without it the
// duplicate edges below survive the merge and P-E over-counts every package's
// centrality -- the failure pagination.AdoptRuns warns is undetectable.
func TestCheckpointedSortKeepsItsFoldAcrossTheInterruption(t *testing.T) {
	sorts, stateDir := resumeArea(t)

	const packages = 2000
	add := func(s *pagination.ExternalSort[pkgEdgeRec]) {
		for i := packages - 1; i >= 0; i-- {
			e := pkgEdgeRec{Pkg: fmt.Sprintf("pkg/%04d", i), RelationID: model.RelationID(fmt.Sprintf("r%04d", i))}
			for range 3 {
				if err := s.Add(e); err != nil {
					t.Fatalf("add: %v", err)
				}
			}
		}
	}

	// The uninterrupted reference.
	ref, err := newSort(sorts, "pkg-edge-ref", lessPkgEdge, sizeOfPkgEdge)
	if err != nil {
		t.Fatalf("newSort: %v", err)
	}
	add(ref.WithFold(foldPkgEdgeDistinct))
	refRun, err := sortedRun(sorts, ref)
	if err != nil {
		t.Fatalf("sorted: %v", err)
	}
	if refRun.Len() != packages {
		t.Fatalf("reference folded to %d edges, want %d", refRun.Len(), packages)
	}

	// The interrupted one: filled, detached mid-pass, adopted, then driven to
	// completion by the resuming call with the fold re-attached.
	interrupted, err := newSort(sorts, "pkg-edge", lessPkgEdge, sizeOfPkgEdge)
	if err != nil {
		t.Fatalf("newSort: %v", err)
	}
	add(interrupted.WithFold(foldPkgEdgeDistinct))
	files, err := checkpointSort(stateDir, "pkg-edge", interrupted)
	if err != nil {
		t.Fatalf("checkpointSort: %v", err)
	}
	if len(files) < 2 {
		t.Fatalf("the fixture did not spill: %d run files, want at least 2", len(files))
	}
	for _, f := range files {
		if _, err := os.Stat(filepath.Join(stateDir, f)); err != nil {
			t.Fatalf("checkpointed run is not in the state directory: %v", err)
		}
	}

	adopted, err := restoreSort(sorts, stateDir, "pkg-edge", files, lessPkgEdge, sizeOfPkgEdge)
	if err != nil {
		t.Fatalf("restoreSort: %v", err)
	}
	resumed, err := sortedRun(sorts, adopted.WithFold(foldPkgEdgeDistinct))
	if err != nil {
		t.Fatalf("sorted: %v", err)
	}
	if resumed.Len() != refRun.Len() {
		t.Fatalf("the resumed sort holds %d edges, the uninterrupted one %d", resumed.Len(), refRun.Len())
	}
	var want []pkgEdgeRec
	if err := refRun.Each(func(e pkgEdgeRec) error { want = append(want, e); return nil }); err != nil {
		t.Fatalf("drain reference: %v", err)
	}
	i := 0
	if err := resumed.Each(func(e pkgEdgeRec) error {
		if e != want[i] {
			return fmt.Errorf("edge %d resumed as %+v, want %+v", i, e, want[i])
		}
		i++
		return nil
	}); err != nil {
		t.Fatalf("%v", err)
	}
}

// TestCheckpointStateRefusesAnEscapingRunName keeps a crafted state file from
// pointing the merge at a file outside the directory the cursor named.
func TestCheckpointStateRefusesAnEscapingRunName(t *testing.T) {
	sorts, stateDir := resumeArea(t)
	for _, name := range []string{"../escape", "sub/run", "/abs/run", ""} {
		if _, err := restoreSort(sorts, stateDir, "x", []string{name}, lessPkg, sizeOfPkgCount); err == nil {
			t.Fatalf("run name %q was accepted", name)
		}
	}
}

// TestCheckpointStateRoundTrip asserts the state file a resume re-validates
// carries the pass index and the request identity it is checked against.
func TestCheckpointStateRoundTrip(t *testing.T) {
	dir := t.TempDir()
	st := checkpointState{Pass: 4, RequestHash: "sha256:abc",
		Streams: map[string][]string{"ranked": {"ctx-resume-ranked-0"}},
		Scalars: checkpointScalars{ScopeComplete: true, ReasonsDropped: 7}}
	if err := writeCheckpointState(dir, st); err != nil {
		t.Fatalf("write: %v", err)
	}
	got, err := readCheckpointState(dir)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if got.Pass != 4 || got.RequestHash != "sha256:abc" ||
		got.Scalars.ReasonsDropped != 7 || !got.Scalars.ScopeComplete ||
		len(got.Streams["ranked"]) != 1 {
		t.Fatalf("state round-tripped as %+v", got)
	}
	if _, err := readCheckpointState(t.TempDir()); err == nil {
		t.Fatal("a missing state file was accepted")
	}
}

// TestACheckpointedRunKeepsItsArrivalOrderUnderANonTotalComparator pins what
// finding B3 found unpinned: the runs a checkpoint detaches are stored in
// DETACH order, because the merge that adopts them breaks ties by run index.
// Storing them under their os.CreateTemp suffixes instead orders the tied
// records by a random number, so the resumed compile ranks a package's
// candidates differently from the uninterrupted one.
//
// lessScoredPkg is the compile's one checkpointed non-total comparator (it
// orders on Pkg alone), so it is the comparator the invariant is asserted on:
// under a total comparator every order is the same order and the test would
// guard nothing.
func TestACheckpointedRunKeepsItsArrivalOrderUnderANonTotalComparator(t *testing.T) {
	sorts, stateDir := resumeArea(t)

	sorter, err := newSort(sorts, "scored", lessScoredPkg, sizeOfScored)
	if err != nil {
		t.Fatalf("newSort: %v", err)
	}
	// Many candidates per package: the ties are what the run index orders.
	const packages, perPkg = 300, 8
	for i := packages - 1; i >= 0; i-- {
		for j := range perPkg {
			r := scoredRec{Pkg: fmt.Sprintf("pkg/%04d", i)}
			r.Cand.Seq = int64(i*perPkg + j)
			if err := sorter.Add(r); err != nil {
				t.Fatalf("add: %v", err)
			}
		}
	}
	run, err := sortedRun(sorts, sorter)
	if err != nil {
		t.Fatalf("sorted: %v", err)
	}
	var want []int64
	if err := run.Each(func(r scoredRec) error { want = append(want, r.Cand.Seq); return nil }); err != nil {
		t.Fatalf("drain: %v", err)
	}

	files, err := checkpointRun(stateDir, "scored", sorts.runBytes, run, lessScoredPkg, sizeOfScored)
	if err != nil {
		t.Fatalf("checkpointRun: %v", err)
	}
	if len(files) < 2 {
		t.Fatalf("the fixture did not spill: %d run files, want at least 2", len(files))
	}
	restored, err := restoreRun(sorts, stateDir, "scored", files, lessScoredPkg, sizeOfScored)
	if err != nil {
		t.Fatalf("restoreRun: %v", err)
	}
	i := 0
	if err := restored.Each(func(r scoredRec) error {
		if i >= len(want) {
			return fmt.Errorf("the restored run is longer than the %d records checkpointed", len(want))
		}
		if r.Cand.Seq != want[i] {
			return fmt.Errorf("record %d restored as seq %d, want %d", i, r.Cand.Seq, want[i])
		}
		i++
		return nil
	}); err != nil {
		t.Fatalf("%v", err)
	}
	if i != len(want) {
		t.Fatalf("the restored run holds %d records, want %d", i, len(want))
	}
}
