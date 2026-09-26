package sqlite_test

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Sawmonabo/codectx/internal/model"
	"github.com/Sawmonabo/codectx/internal/testenv"
)

// publishOneUnit builds a single unit holding n node facts and n relation
// facts (each with one evidence row) and seals it, returning how long the
// publication took and how many bytes the store file holds afterwards.
func publishOneUnit(t *testing.T, n int) (time.Duration, int64) {
	t.Helper()
	dir := t.TempDir()
	path := dir + "/seal.db"
	f := newFixture(t, path)
	ff := f.file("pkg/scale.go", strings.Repeat("x", 4096))
	snap := f.snapshot("scale", ff)
	gen, err := f.s.BeginGeneration(f.ctx, f.repo, snap.ID, model.H("semantic"), "main")
	if err != nil {
		t.Fatalf("BeginGeneration: %v", err)
	}
	run := f.run(gen)
	w := f.begin(gen, run, ff)

	rng := &model.SourceRange{Start: model.Position{Byte: 0, Line: 1}, End: model.Position{Byte: 16, Line: 1, Column: 16}}
	nodeAt := func(i int) (model.Node, string) {
		name := "F" + strconv.Itoa(i)
		key := model.CanonicalNodeKey(ff.path, name)
		id := model.NewNodeID(f.repo, model.NodeFunction, key)
		return model.Node{ID: id, Kind: model.NodeFunction, Language: "go", Name: name,
			QualifiedName: ff.path + "." + name, FileID: ff.id, ContentHash: ff.hash, Range: rng}, key
	}

	const batch = 500
	start := time.Now()
	for lo := 0; lo < n; lo += batch {
		hi := min(lo+batch, n)
		facts := make([]model.NodeFact, 0, hi-lo)
		for i := lo; i < hi; i++ {
			node, key := nodeAt(i)
			ev := model.Evidence{UnitID: w.UnitID(), ProviderID: providerID, ProviderVersion: providerVersion,
				OriginRunID: run, NodeID: node.ID, Precision: model.PrecisionSyntax, FileID: ff.id,
				ContentHash: ff.hash, Range: rng, NativeKey: node.Name}
			ev.ID = model.NewEvidenceID(ev)
			facts = append(facts, model.NodeFact{Node: node, CanonicalKey: key, Evidence: []model.Evidence{ev}})
		}
		if err := w.PutNodes(f.ctx, facts); err != nil {
			t.Fatalf("PutNodes: %v", err)
		}
		aliases := make([]model.NativeAlias, 0, hi-lo)
		docs := make([]model.SearchUnit, 0, hi-lo)
		for i := lo; i < hi; i++ {
			node, _ := nodeAt(i)
			aliases = append(aliases, model.NativeAlias{ScopeKey: ff.path, NativeKey: node.Name, NodeID: node.ID})
			docs = append(docs, model.SearchUnit{ID: model.H("search-scale", node.Name), NodeID: node.ID,
				FileID: ff.id, Path: ff.path, Kind: model.NodeFunction, Name: node.Name,
				QualifiedName: node.QualifiedName, Bytes: model.ByteRange{Start: 0, End: 16},
				Body: "func " + node.Name + "() zqxsignature", TokenCount: providerTokenCount})
		}
		if err := w.PutAliases(f.ctx, aliases); err != nil {
			t.Fatalf("PutAliases: %v", err)
		}
		if err := w.PutSearchUnits(f.ctx, docs); err != nil {
			t.Fatalf("PutSearchUnits: %v", err)
		}
	}
	for lo := 0; lo < n; lo += batch {
		hi := min(lo+batch, n)
		facts := make([]model.RelationFact, 0, hi-lo)
		for i := lo; i < hi; i++ {
			from, _ := nodeAt(i)
			to, _ := nodeAt((i + 1) % n)
			rel := model.Relation{From: from.ID, Kind: model.RelCalls, To: to.ID}
			rel.ID = model.NewRelationID(f.repo, rel.From, rel.Kind, rel.To)
			ev := model.Evidence{UnitID: w.UnitID(), ProviderID: providerID, ProviderVersion: providerVersion,
				OriginRunID: run, RelationID: rel.ID, Precision: model.PrecisionSyntax, FileID: ff.id,
				ContentHash: ff.hash, Range: rng, NativeKey: from.Name}
			ev.ID = model.NewEvidenceID(ev)
			facts = append(facts, model.RelationFact{Relation: rel, Evidence: []model.Evidence{ev}})
		}
		if err := w.PutRelations(f.ctx, facts); err != nil {
			t.Fatalf("PutRelations: %v", err)
		}
	}
	if err := f.s.SealUnit(f.ctx, w); err != nil {
		t.Fatalf("SealUnit: %v", err)
	}
	elapsed := time.Since(start)
	var bytes int64
	for _, suffix := range []string{"", "-wal", "-shm"} {
		if st, err := os.Stat(path + suffix); err == nil {
			bytes += st.Size()
		}
	}
	return elapsed, bytes
}

// TestSealingOneUnitCostsFlatTimePerFact publishes one unit at a curve of
// sizes and asserts the cost PER FACT does not grow with the size of the unit.
// A superlinear seal path -- an unindexed per-row probe, a per-row prepare, an
// insert order random with respect to a hot index -- shows up here as a
// per-fact cost that climbs with N, which is what the assertion forbids.
func TestSealingOneUnitCostsFlatTimePerFact(t *testing.T) {
	testenv.SkipIfLoaded(t)
	sizes := []int{2000, 4000, 8000, 16000}
	if raw := os.Getenv("CODECTX_SEAL_SIZES"); raw != "" {
		sizes = nil
		for _, part := range strings.Split(raw, ",") {
			n, err := strconv.Atoi(strings.TrimSpace(part))
			if err != nil {
				t.Fatalf("CODECTX_SEAL_SIZES: %v", err)
			}
			sizes = append(sizes, n)
		}
	}
	var base float64
	var report []string
	for i, n := range sizes {
		elapsed, bytes := publishOneUnit(t, n)
		perFact := float64(elapsed.Nanoseconds()) / float64(2*n)
		if i == 0 {
			base = perFact
		}
		report = append(report, fmt.Sprintf("N=%-6d facts=%-6d wall=%-10s us/fact=%.2f ratio=%.2f bytes=%d bytes/fact=%.1f",
			n, 2*n, elapsed.Round(time.Millisecond), perFact/1000, perFact/base, bytes, float64(bytes)/float64(2*n)))
		// Flat means flat within noise, not exact: 2x the smallest size's
		// per-fact cost still admits ordinary variance and cache effects
		// while refusing any curve that tracks N.
		if perFact > 2*base {
			t.Errorf("per-fact cost at N=%d is %.2f us, %.2fx the cost at N=%d: the seal path is superlinear in the size of one unit",
				n, perFact/1000, perFact/base, sizes[0])
		}
	}
	t.Log("\n" + strings.Join(report, "\n"))
}
