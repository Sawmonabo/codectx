package context

import (
	"fmt"
	"testing"

	"github.com/Sawmonabo/codectx/internal/model"
	"github.com/Sawmonabo/codectx/internal/pagination"
)

// persistManifest replays the exclusion run TWICE -- once to fold the canonical
// manifest hash, once to write the rows inside the manifest transaction. The
// package's own fixtures exclude a handful of candidates, so their run never
// spills and both replays take SortedRun's in-heap branch. The branch that
// matters on a real repository is the file-backed one, which reopens the file
// per call, and a divergence there persists a CanonicalHash folded over rows
// the store does not hold.
//
// The run buffer is forced to two records so the run spills, and the two
// replays are compared element for element under the compiler's own codec and
// comparator -- the ones the exclusion sort is built with in Compile.
func TestExcludedRunReplaysIdenticallyWhenItSpills(t *testing.T) {
	sorter, err := pagination.NewExternalSort(t.TempDir(), "excluded-", 2,
		encodeRecord[model.ExcludedContextEntry], decodeRecord[model.ExcludedContextEntry], lessExcludedOrdinal)
	if err != nil {
		t.Fatalf("NewExternalSort: %v", err)
	}
	defer sorter.Close()
	const n = 9
	for i := 0; i < n; i++ {
		if err := sorter.Add(model.ExcludedContextEntry{Ordinal: i,
			Reference: model.ContextReference{NodeID: model.NodeID(fmt.Sprintf("node-%d", i)),
				FileID: model.FileID(fmt.Sprintf("file-%d", i)), Path: fmt.Sprintf("src/f%d.go", i)},
			Reason: dropFileLimit}); err != nil {
			t.Fatalf("Add(%d): %v", i, err)
		}
	}
	run, err := sorter.Sorted()
	if err != nil {
		t.Fatalf("Sorted: %v", err)
	}
	defer run.Close()
	drain := func() []model.ExcludedContextEntry {
		var out []model.ExcludedContextEntry
		if err := run.Each(func(x model.ExcludedContextEntry) error {
			out = append(out, x)
			return nil
		}); err != nil {
			t.Fatalf("Each: %v", err)
		}
		return out
	}
	hashPass, writePass := drain(), drain()
	if len(hashPass) != n || len(writePass) != n {
		t.Fatalf("replays yielded %d and %d rows, want %d each", len(hashPass), len(writePass), n)
	}
	for i := range hashPass {
		if hashPass[i] != writePass[i] {
			t.Fatalf("replay %d diverged: hash pass saw %+v, write pass saw %+v", i, hashPass[i], writePass[i])
		}
		if hashPass[i].Ordinal != i {
			t.Fatalf("row %d carries ordinal %d; the run is not in C1 ordinal order", i, hashPass[i].Ordinal)
		}
	}
}
