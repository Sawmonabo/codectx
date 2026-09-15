package graph

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"testing"

	"github.com/Sawmonabo/codectx/internal/config"
	"github.com/Sawmonabo/codectx/internal/model"
)

// This file is lane GP-L3's own test budget: the identity proofs that the
// consumers moved onto graph.GraphReader (ADR-0005) answer exactly what the
// containment-walking implementations answered, and the direction proof for the
// shortest-path search.
//
// The identity proofs are GOLDEN: the answers were captured from the
// pre-reader implementation, committed as testdata, and are re-checked against
// the reader path. A test that only compared the new implementation with
// itself would pass over any wholesale change of what the answer MEANS, which
// is the shortcut the scale posture forbids.

// memGraphFor builds the reference GraphReader over the shared fixture's own
// nodes and relations, so the two read paths see one graph. The walk still
// reads through Adjacency (it is another lane's), so an engine under test
// carries both ports over the same facts.
func memGraphFor(f *graphFixture) *MemoryGraph {
	nodes := make([]model.Node, 0, len(f.nodes))
	for _, n := range f.nodes {
		nodes = append(nodes, n)
	}
	sort.Slice(nodes, func(i, j int) bool { return nodes[i].ID < nodes[j].ID })
	return NewMemoryGraph(f.binding, nodes, append([]model.Relation(nil), f.relations...))
}

// golden compares got against the committed capture, and writes it when
// CODECTX_UPDATE_GOLDEN is set. The capture is the pre-reader answer; the
// update path exists to MAKE that capture once, never to bless a drift.
func golden(t *testing.T, name string, got any) {
	t.Helper()
	path := filepath.Join("testdata", name)
	blob, err := json.MarshalIndent(got, "", "  ")
	if err != nil {
		t.Fatalf("marshal %s: %v", name, err)
	}
	blob = append(blob, '\n')
	if os.Getenv("CODECTX_UPDATE_GOLDEN") != "" {
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatalf("mkdir testdata: %v", err)
		}
		if err := os.WriteFile(path, blob, 0o644); err != nil {
			t.Fatalf("write %s: %v", path, err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v (capture it with CODECTX_UPDATE_GOLDEN=1)", path, err)
	}
	if string(want) != string(blob) {
		t.Fatalf("%s differs from the captured pre-reader answer\n got: %s\nwant: %s", name, blob, want)
	}
}

// TestRollupAnswerIsUnchangedByTheReader is the rollup identity proof: the same
// pairs, in the same order, with the same edge and evidence counts the
// containment-walking rollup produced.
func TestRollupAnswerIsUnchangedByTheReader(t *testing.T) {
	f := newGraphFixture(t)
	limits := fixtureLimits()
	limits.MaxDepth, limits.MaxVisited, limits.MaxEdges = 0, 0, 0
	limits.MaxPageItems = 2 * fixtureWideCount
	e, err := New(Options{Adjacency: f, Reader: memGraphFor(f), Limits: limits})
	if err != nil {
		t.Fatalf("new engine: %v", err)
	}
	page, err := e.PackageDependencies(context.Background(), model.GraphRequest{
		GenerationID: f.binding.GenerationID,
		Start:        []model.NodeID{fixtureNodeID("n-a"), fixtureNodeID("n-wide")},
		Direction:    model.DirectionOutgoing,
		Relations:    []model.RelationKind{model.RelCalls},
	})
	if err != nil {
		t.Fatalf("PackageDependencies: %v", err)
	}
	if page.Meta.Truncated || page.Meta.NextCursor != "" {
		t.Fatalf("the identity answer must be one complete page: truncated=%v reason=%q",
			page.Meta.Truncated, page.Meta.TruncationReason)
	}
	if len(page.Items) == 0 {
		t.Fatal("the rollup produced no pairs; the fixture proves nothing")
	}
	golden(t, "rollup_pairs.json", page.Items)
}

// TestOverviewCountsAreUnchangedByTheReader is the repository-map identity
// proof: every container's direct file, symbol and byte counts, its parent and
// its depth, exactly as the containment-walking map reported them.
func TestOverviewCountsAreUnchangedByTheReader(t *testing.T) {
	f := newGraphFixture(t)
	limits := fixtureLimits()
	limits.MaxPageItems = 200
	e, err := New(Options{Adjacency: f, Reader: memGraphFor(f), Limits: limits})
	if err != nil {
		t.Fatalf("new engine: %v", err)
	}
	page, err := e.Overview(context.Background(), model.OverviewRequest{
		GenerationID: f.binding.GenerationID})
	if err != nil {
		t.Fatalf("Overview: %v", err)
	}
	if page.Meta.Truncated {
		t.Fatalf("a map inside every bound reports truncated: %q", page.Meta.TruncationReason)
	}
	if len(page.Items) == 0 {
		t.Fatal("the map listed no container; the fixture proves nothing")
	}
	golden(t, "overview_items.json", page.Items)
}

// TestContainmentScanEndsOnTheLastEntry is F7, carried onto the packed reader:
// the containment scan used to report an incomplete read the moment its edge
// allowance was reached, so a containment set whose entries fill the allowance
// EXACTLY reported complete=false -- and both callers refuse an incomplete
// containment read, turning a whole answer into an omitted container. Only an
// allowance with an entry still standing in front of it is incomplete.
//
// Mutation: make the allowance test `maxEdges.Exceeded(read)` in
// scanNeighbours and the exact-fill leg below fails.
func TestContainmentScanEndsOnTheLastEntry(t *testing.T) {
	f := newGraphFixture(t)
	reader := memGraphFor(f)
	contains, ok := reader.Kinds().Code(model.RelContains)
	if !ok {
		t.Fatal("the fixture seals no containment; the scan cannot be exercised")
	}
	// pkg-app's outgoing containment: four `contains` children, a fixed count
	// the allowances below are set against.
	owner := sortedRefs(context.Background(), reader, []model.NodeID{fixtureNodeID("pkg-app")})
	if len(owner) != 1 {
		t.Fatalf("pkg-app resolved to %d surrogate(s), want 1", len(owner))
	}
	const rows = 4
	for _, tc := range []struct {
		name     string
		budget   int64
		wantRead int
		wantDone bool
	}{
		{name: "the allowance is exactly filled", budget: rows, wantRead: rows, wantDone: true},
		{name: "an entry is left past the allowance", budget: rows - 1, wantRead: rows - 1, wantDone: false},
		{name: "the allowance is unlimited", budget: 0, wantRead: rows, wantDone: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			read := 0
			b := &budget{}
			done, err := scanNeighbours(context.Background(), reader, owner,
				model.DirectionOutgoing, []KindCode{contains}, config.Limit(tc.budget), b,
				func(owners, _ []NodeRef) error { read += len(owners); return nil })
			if err != nil {
				t.Fatalf("scanNeighbours: %v", err)
			}
			if read != tc.wantRead || done != tc.wantDone {
				t.Fatalf("read %d entr(ies), complete=%v; want %d, complete=%v",
					read, done, tc.wantRead, tc.wantDone)
			}
			if b.edges != int64(tc.wantRead) {
				t.Fatalf("the scan charged %d edge(s) to the page budget, want %d", b.edges, tc.wantRead)
			}
		})
	}
}
