package sqlite_test

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/Sawmonabo/codectx/internal/model"
	store "github.com/Sawmonabo/codectx/internal/storage/sqlite"
)

// twoOffsetNode is the node two units disagree about: one publishes it at
// byte 50, the other at byte 100. node_facts is keyed (unit_id, node_id), so
// this is the ONLY way one file can carry a node at two offsets.
const twoOffsetNode = "Twice"

// declare publishes one node fact for name over ff at [start,end) inside a
// building unit. The fixture's own putNode always spans the whole file, and a
// node's offset is exactly what this test varies.
func declare(t *testing.T, f *fixture, w *store.UnitWriter, run model.ProviderRunID,
	ff fileFixture, name string, start, end uint64) model.NodeID {
	t.Helper()
	key := model.CanonicalNodeKey("pkg", name)
	id := model.NewNodeID(f.repo, model.NodeFunction, key)
	rng := &model.SourceRange{Start: model.Position{Byte: start, Line: 1},
		End: model.Position{Byte: end, Line: 1, Column: uint32(end)}}
	node := model.Node{ID: id, Kind: model.NodeFunction, Language: "go", Name: name,
		QualifiedName: "pkg." + name, FileID: ff.id, ContentHash: ff.hash, Range: rng}
	ev := model.Evidence{UnitID: w.UnitID(), ProviderID: providerID, ProviderVersion: providerVersion,
		OriginRunID: run, NodeID: id, Precision: model.PrecisionSyntax, NativeKey: name,
		FileID: ff.id, ContentHash: ff.hash, Range: rng}
	ev.ID = model.NewEvidenceID(ev)
	if err := w.PutNodes(f.ctx, []model.NodeFact{{Node: node, CanonicalKey: key, Evidence: []model.Evidence{ev}}}); err != nil {
		t.Fatalf("PutNodes(%s@%d): %v", name, start, err)
	}
	return id
}

// unitOver opens a unit over ff under its own scope key, which is what lets two
// units of one provider sit in one generation (generation_units is unique on
// generation, provider and scope).
func unitOver(t *testing.T, f *fixture, gen model.GenerationID, run model.ProviderRunID,
	ff fileFixture, scope string, binding model.SourceBinding) *store.UnitWriter {
	t.Helper()
	input := model.UnitInput{FileID: ff.id, ContentHash: ff.hash}
	h := model.NewUnitInputHasher()
	if err := h.Add(input); err != nil {
		t.Fatal(err)
	}
	spec := model.UnitSpec{ProviderID: providerID, ProviderVersion: providerVersion, ScopeKey: scope,
		InputHash: h.Sum(), DependencyHash: model.DependencyHash(nil)}
	spec.ID = model.NewUnitID(spec, scope)
	build := model.UnitBuild{Spec: spec, AnalysisConfigHash: scope, OriginRunID: run, SourceBinding: binding}
	w, err := f.s.BeginUnit(f.ctx, gen, build, func(yield func(model.UnitInput) error) error { return yield(input) })
	if err != nil {
		t.Fatalf("BeginUnit(%s): %v", scope, err)
	}
	return w
}

// A retrieval tier reads a file's declarations by node IDENTITY, so a node two
// units place at two offsets must reach it once, at the offset of the unit the
// Section 9.4 precedence order picks -- the same row Nodes serves for that
// identity. document-symbols reads the same file as a list of DECLARED
// OFFSETS, so NodesInFile must keep returning both rows. The two readings are
// two methods, and this test pins both against one store.
func TestDistinctNodesInFileKeepsOneRowPerNode(t *testing.T) {
	f := newFixture(t, filepath.Join(t.TempDir(), "codectx.db"))
	ctx := f.ctx

	a := f.file("pkg/a.go", "package pkg\n"+string(make([]byte, 300)))
	snap := f.snapshot("one", a)
	gen, err := f.s.BeginGeneration(ctx, f.repo, snap.ID, model.H("semantic"), "main")
	if err != nil {
		t.Fatalf("BeginGeneration: %v", err)
	}
	run := f.run(gen)

	// The unverified unit declares the shared node EARLIER in the file, so a
	// rule that took the first offset instead of the precedence winner would
	// serve this row and disagree with Nodes about the node's range.
	loose := unitOver(t, f, gen, run, a, "scope-loose", model.SourceBindingUnverified)
	declare(t, f, loose, run, a, twoOffsetNode, 50, 60)
	if err := f.s.SealUnit(ctx, loose); err != nil {
		t.Fatalf("SealUnit(loose): %v", err)
	}
	strict := unitOver(t, f, gen, run, a, "scope-strict", model.SourceBindingVerified)
	declare(t, f, strict, run, a, "Before", 10, 20)
	twice := declare(t, f, strict, run, a, twoOffsetNode, 100, 110)
	declare(t, f, strict, run, a, "After", 200, 210)
	if err := f.s.SealUnit(ctx, strict); err != nil {
		t.Fatalf("SealUnit(strict): %v", err)
	}
	f.activate(gen, 0)
	r, err := f.s.PinGeneration(ctx, f.repo, 0, time.Minute)
	if err != nil {
		t.Fatalf("PinGeneration: %v", err)
	}
	defer r.Close()

	starts := func(nodes []store.StoredNode) []uint64 {
		out := make([]uint64, 0, len(nodes))
		for _, n := range nodes {
			var s uint64
			if n.Bytes != nil {
				s = n.Bytes.Start
			}
			out = append(out, s)
		}
		return out
	}

	// The every-offset reading is unchanged: document-symbols still sees both
	// declarations, in document order.
	all, err := r.NodesInFile(ctx, a.id, 0, "", 100)
	if err != nil {
		t.Fatalf("NodesInFile: %v", err)
	}
	if got := starts(all); len(got) != 4 || got[0] != 10 || got[1] != 50 || got[2] != 100 || got[3] != 200 {
		t.Fatalf("NodesInFile served offsets %v, want both declarations of the shared node kept: [10 50 100 200]", got)
	}

	// The identity reading emits the shared node ONCE, at the precedence
	// winner's offset, with the neighbours still in document order.
	distinct, err := r.DistinctNodesInFile(ctx, a.id, 0, "", 100)
	if err != nil {
		t.Fatalf("DistinctNodesInFile: %v", err)
	}
	if got := starts(distinct); len(got) != 3 || got[0] != 10 || got[1] != 100 || got[2] != 200 {
		t.Fatalf("DistinctNodesInFile served offsets %v, want one row per node in document order: [10 100 200]", got)
	}
	seen := map[model.NodeID]int{}
	for _, n := range distinct {
		seen[n.Node.ID]++
	}
	if seen[twice] != 1 {
		t.Fatalf("the node declared at two offsets was served %d times, want once", seen[twice])
	}

	// The row it serves is the row Nodes serves for the same identity: an
	// identity read of a node reports one byte range whichever method answers.
	byName, err := r.Nodes(ctx, store.NodeFilter{Name: twoOffsetNode}, "", 10)
	if err != nil {
		t.Fatalf("Nodes: %v", err)
	}
	if len(byName) != 1 {
		t.Fatalf("Nodes served %d rows for one node identity", len(byName))
	}
	if starts(byName)[0] != starts(distinct)[1] {
		t.Fatalf("Nodes puts the shared node at byte %d and DistinctNodesInFile at %d; the two reads "+
			"of one identity must agree on its range", starts(byName)[0], starts(distinct)[1])
	}

	// The (start_byte, node_id) keyset still walks the file to its end, one
	// node per page, without skipping or repeating across the boundaries.
	var walked []uint64
	var afterStart int64
	var after model.NodeID
	for {
		page, err := r.DistinctNodesInFile(ctx, a.id, afterStart, after, 1)
		if err != nil {
			t.Fatalf("DistinctNodesInFile(page): %v", err)
		}
		if len(page) == 0 {
			break
		}
		walked = append(walked, starts(page)...)
		n := page[len(page)-1]
		afterStart, after = 0, n.Node.ID
		if n.Bytes != nil {
			afterStart = int64(n.Bytes.Start)
		}
	}
	if len(walked) != 3 || walked[0] != 10 || walked[1] != 100 || walked[2] != 200 {
		t.Fatalf("the paged walk served offsets %v, want the whole file once: [10 100 200]", walked)
	}
}
