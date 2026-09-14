package graph

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"testing"
	"time"

	"github.com/Sawmonabo/codectx/internal/model"
	"github.com/Sawmonabo/codectx/internal/pagination"
)

// This is the whole test budget for the graph engine: one shared in-memory
// Adjacency over a hand-built graph, plus one scenario table. Each fill-in lane
// adds its cases under its own marker below, so the lanes never edit the same
// lines. A case exists only to protect a critical invariant -- a silently
// dropped edge, a nondeterministic order, a budget that resets across pages, a
// collapsed occurrence count. Cases for getters, wiring or enum spelling are
// defects, not coverage.
//
// The graph deliberately contains every shape the engine must survive:
//
//	cycle           n-a -calls-> n-b -calls-> n-c -calls-> n-a
//	high fan-out    n-hub with 40 outgoing calls edges
//	equal cost      n-a -> n-p -> n-z and n-a -> n-q -> n-z, both cost 2.
//	                These are the ONLY two routes under outgoing + calls; a case
//	                pinning equal-cost order must scope itself that way, because
//	                DirectionBoth also admits a third route through the package
//	                containment edges (n-a <-contains pkg-app -imports-> pkg-lib
//	                -contains-> n-z, cost 14).
//	dependence-only reads / data_flows_to edges alongside ordinary kinds
//	two packages    pkg-app and pkg-lib, so a rollup has a distinct pair
//
// newGraphFixture has no caller until the fill-in lanes land their rows; that
// is expected for the skeleton commit and is not dead code.

// fixtureID maps a readable fixture name to the 64-lowercase-hex id every
// model validator requires. The graph below is written in readable names so it
// stays legible, and every id that crosses a validated boundary is derived
// here: a fixture that used the readable name directly would be rejected by
// Validate before any engine logic ran.
func fixtureID(name string) string {
	sum := sha256.Sum256([]byte(name))
	return hex.EncodeToString(sum[:])
}

// fixtureNodeID is fixtureID for node names; a test names its seeds with it.
func fixtureNodeID(name string) model.NodeID { return model.NodeID(fixtureID(name)) }

// fixtureRelationID is the RelationID the fixture assigns to the i-th declared
// edge. Edge ids are the zero-padded hex counter of the declaration order, so
// the keyset order is still readable from the source; a case naming an edge
// derives its id here rather than writing the padded literal out.
func fixtureRelationID(i int) model.RelationID { return model.RelationID(fmt.Sprintf("%064x", i)) }

// fixtureLeafCount is the hub's fan-out. It is larger than any per-page bound a
// test sets, so a truncation case has something real to truncate.
const fixtureLeafCount = 40

// fixtureWideCount is the fan-out of n-wide, the hub that exists purely to
// exceed fixtureEdgePageLimit's clamp: reading its neighbourhood whole needs a
// SECOND keyset page, which is the only way a loop that ends on a short page
// can be told apart from one that ends on an empty page. n-wide hangs off no
// package and nothing else reaches it, so it changes no other scenario's answer.
const fixtureWideCount = model.MaxPageItems + 60

// graphFixture is a deterministic in-memory Adjacency. Relations are stored
// once, sorted by RelationID, so every read is keyset-ordered without the test
// having to sort anything.
type graphFixture struct {
	nodes     map[model.NodeID]model.Node
	relations []model.Relation
	evidence  map[model.RelationID][]model.Evidence
	caps      []model.CapabilityState
	binding   model.Binding

	// EdgeCalls counts Edges round trips, so a lane can prove a walk batches
	// its frontier instead of issuing one query per node.
	EdgeCalls int
}

// newGraphFixture builds the shared graph described above.
func newGraphFixture(t *testing.T) *graphFixture {
	t.Helper()
	f := &graphFixture{
		nodes:    map[model.NodeID]model.Node{},
		evidence: map[model.RelationID][]model.Evidence{},
		binding: model.Binding{
			RepositoryID: model.RepositoryID(fixtureID("repo-1")),
			SnapshotID:   model.SnapshotID(fixtureID("snap-1")),
			GenerationID: 1,
			AnalysisKey:  model.AnalysisKey(fixtureID("akey-1")),
		},
	}

	addNode := func(name string, kind model.NodeKind, label string) {
		id := fixtureNodeID(name)
		f.nodes[id] = model.Node{
			ID: id, Kind: kind, Name: label, QualifiedName: label,
			Language: "go", SemanticSource: model.SemanticCanonical,
		}
	}
	addNode("pkg-app", model.NodePackage, "app")
	addNode("pkg-lib", model.NodePackage, "lib")
	for _, name := range []string{"n-a", "n-b", "n-c", "n-hub", "n-p", "n-q", "n-z"} {
		addNode(name, model.NodeFunction, name)
	}
	addNode("n-var", model.NodeVariable, "n-var")
	addNode("n-sink", model.NodeFunction, "n-sink")
	for i := 0; i < fixtureLeafCount; i++ {
		addNode(fmt.Sprintf("n-leaf-%02d", i), model.NodeFunction, fmt.Sprintf("leaf%02d", i))
	}
	addNode("n-wide", model.NodeFunction, "n-wide")
	for i := 0; i < fixtureWideCount; i++ {
		addNode(fmt.Sprintf("n-wide-leaf-%03d", i), model.NodeFunction, fmt.Sprintf("wide%03d", i))
	}

	// edges are declared in a stable order; RelationIDs are assigned from it so
	// the keyset order is reproducible from the source alone.
	type edge struct {
		from string
		kind model.RelationKind
		to   string
	}
	edges := []edge{
		// containment: pkg-app owns the entry points, pkg-lib the targets, so a
		// rollup sees exactly one distinct (from-package, to-package) pair.
		{"pkg-app", model.RelContains, "n-a"},
		{"pkg-app", model.RelContains, "n-b"},
		{"pkg-app", model.RelContains, "n-hub"},
		{"pkg-app", model.RelContains, "n-p"},
		{"pkg-lib", model.RelContains, "n-c"},
		{"pkg-lib", model.RelContains, "n-q"},
		{"pkg-lib", model.RelContains, "n-z"},
		{"pkg-lib", model.RelContains, "n-var"},
		{"pkg-lib", model.RelContains, "n-sink"},
		{"pkg-app", model.RelImports, "pkg-lib"},

		// the cycle
		{"n-a", model.RelCalls, "n-b"},
		{"n-b", model.RelCalls, "n-c"},
		{"n-c", model.RelCalls, "n-a"},

		// two equal-cost routes from n-a to n-z (calls costs 1, so both are 2)
		{"n-a", model.RelCalls, "n-p"},
		{"n-p", model.RelCalls, "n-z"},
		{"n-a", model.RelCalls, "n-q"},
		{"n-q", model.RelCalls, "n-z"},

		// dependence-only kinds mixed in with ordinary ones
		{"n-a", model.RelReads, "n-var"},
		{"n-b", model.RelWrites, "n-var"},
		{"n-b", model.RelDataFlowsTo, "n-sink"},
		{"n-c", model.RelControlDependsOn, "n-sink"},
	}
	for i := 0; i < fixtureLeafCount; i++ {
		edges = append(edges, edge{"n-hub", model.RelCalls, fmt.Sprintf("n-leaf-%02d", i)})
	}
	// n-a calls the hub, so the fan-out is reachable from the same seed as the cycle.
	edges = append(edges, edge{"n-a", model.RelCalls, "n-hub"})
	// n-wide's fan-out is declared last so every relation id above keeps the
	// position it had before this hub existed.
	for i := 0; i < fixtureWideCount; i++ {
		edges = append(edges, edge{"n-wide", model.RelCalls, fmt.Sprintf("n-wide-leaf-%03d", i)})
	}

	// evidenceRow is a whole evidence row, not just its id: an occurrence's
	// precision class, file and byte range live here and nowhere else, so a
	// fixture that carried ids alone could not tell a hydrated occurrence from
	// an unhydrated one.
	evidenceRow := func(rel model.RelationID, i, n int) model.Evidence {
		return model.Evidence{
			ID:              model.EvidenceID(fixtureID(fmt.Sprintf("ev-%04d-%d", i, n))),
			UnitID:          model.UnitID(fixtureID("unit-1")),
			ProviderID:      "treesitter",
			ProviderVersion: "0.0.1",
			OriginRunID:     model.ProviderRunID(fixtureID("run-1")),
			RelationID:      rel,
			Precision:       model.PrecisionSyntax,
			FileID:          model.FileID(fixtureID("file-1")),
			Range: &model.SourceRange{
				Start: model.Position{Byte: uint64(i) * 16, Line: uint32(i) + 1, Column: 0},
				End:   model.Position{Byte: uint64(i)*16 + 8, Line: uint32(i) + 1, Column: 8},
			},
		}
	}
	for i, e := range edges {
		id := fixtureRelationID(i)
		f.relations = append(f.relations, model.Relation{
			ID: id, From: fixtureNodeID(e.from), Kind: e.kind, To: fixtureNodeID(e.to)})
		// One occurrence per edge, except n-a -> n-b, which carries two: the
		// §9.2 relation-count vs occurrence-count distinction needs a relation
		// that is one relation and two occurrences.
		f.evidence[id] = []model.Evidence{evidenceRow(id, i, 0)}
		if e.from == "n-a" && e.to == "n-b" && e.kind == model.RelCalls {
			f.evidence[id] = append(f.evidence[id], evidenceRow(id, i, 1))
		}
	}
	sort.Slice(f.relations, func(i, j int) bool { return f.relations[i].ID < f.relations[j].ID })

	// A deferred dependence row, as PinnedReader.Capabilities returns it. An
	// answer that traverses a dependence-only kind must disclose this.
	f.caps = []model.CapabilityState{{
		ProviderID:     "dependence",
		Capability:     "data_flows_to",
		Scope:          "workspace",
		State:          model.CapabilityUnavailable,
		DiagnosticCode: model.CodeProviderUnavailable,
		Details:        map[string]string{"reason": "units_deferred"},
	}}
	return f
}

// fixtureEdgePageLimit is the clamp sqlite.pageLimit applies to every
// Adjacency.Edges call: any limit above model.MaxPageItems, and any
// non-positive one, comes back as model.MaxPageItems. The fixture applies it
// verbatim, because a fake that honours whatever limit it is given lets a
// keyset loop that ends on "short page" pass while the shipped reader silently
// truncates every walk.
func fixtureEdgePageLimit(limit int) int {
	if limit <= 0 || limit > model.MaxPageItems {
		return model.MaxPageItems
	}
	return limit
}

// Edges returns the visible edges touching any of nodes, keyset-ordered by
// RelationID after `after`, at most limit rows. It follows the Adjacency
// contract: an empty kinds slice is "no kind filter", and the requested limit
// is clamped exactly as the shipped reader clamps it.
func (f *graphFixture) Edges(ctx context.Context, nodes []model.NodeID, direction model.Direction,
	kinds []model.RelationKind, after model.RelationID, limit int) ([]model.Relation, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	f.EdgeCalls++
	limit = fixtureEdgePageLimit(limit)
	want := make(map[model.NodeID]bool, len(nodes))
	for _, n := range nodes {
		want[n] = true
	}
	allowed := make(map[model.RelationKind]bool, len(kinds))
	for _, k := range kinds {
		allowed[k] = true
	}
	var out []model.Relation
	for _, r := range f.relations {
		if r.ID <= after {
			continue
		}
		if len(allowed) > 0 && !allowed[r.Kind] {
			continue
		}
		switch direction {
		case model.DirectionOutgoing:
			if !want[r.From] {
				continue
			}
		case model.DirectionIncoming:
			if !want[r.To] {
				continue
			}
		default: // DirectionBoth
			if !want[r.From] && !want[r.To] {
				continue
			}
		}
		out = append(out, r)
		if len(out) == limit {
			break
		}
	}
	return out, nil
}

// NodesByID hydrates known nodes; unknown ids are omitted rather than faked.
func (f *graphFixture) NodesByID(ctx context.Context, ids []model.NodeID) ([]model.Node, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	var out []model.Node
	for _, id := range ids {
		if n, ok := f.nodes[id]; ok {
			out = append(out, n)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

// EvidenceFor hydrates the evidence IDENTITIES backing a page of relations in
// one call -- the frozen Adjacency port, which is all a traversal needs.
func (f *graphFixture) EvidenceFor(ctx context.Context, relations []model.RelationID, limit int) (map[model.RelationID][]model.EvidenceID, error) {
	rows, err := f.EvidenceRows(ctx, relations, limit)
	if err != nil {
		return nil, err
	}
	out := make(map[model.RelationID][]model.EvidenceID, len(rows))
	for rel, list := range rows {
		ids := make([]model.EvidenceID, 0, len(list))
		for _, row := range list {
			ids = append(ids, row.ID)
		}
		out[rel] = ids
	}
	return out, nil
}

// EvidenceRows is the optional hydration seam: whole rows, so a reference
// occurrence carries the precision class, file and range that make it
// checkable. The fixture implements it because the app adapter does, and a fake
// that omitted it would let a hydration regression pass unnoticed.
func (f *graphFixture) EvidenceRows(ctx context.Context, relations []model.RelationID, limit int) (map[model.RelationID][]model.Evidence, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	out := make(map[model.RelationID][]model.Evidence, len(relations))
	for _, id := range relations {
		ev := f.evidence[id]
		if len(ev) == 0 {
			continue
		}
		if limit > 0 && len(ev) > limit {
			ev = ev[:limit]
		}
		out[id] = append([]model.Evidence(nil), ev...)
	}
	return out, nil
}

func (f *graphFixture) Capabilities(ctx context.Context) ([]model.CapabilityState, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return append([]model.CapabilityState(nil), f.caps...), nil
}

func (f *graphFixture) Binding() model.Binding { return f.binding }

// LeaseID makes the fixture a LeaseHolder: a continuation token names the
// retention lease holding its generation, and an Adjacency without one offers
// no continuation at all, so a paging case could not run against a fake that
// held no lease.
func (f *graphFixture) LeaseID() string { return fixtureID("lease-1") }

// fixtureLeases is the LeaseStore pagination.Spools consults before it writes
// or replays a spool. Storage owns the real retention_leases rows; the graph
// engine only needs a live lease to exist, so the fake reports every lease as
// live for the fixture window.
type fixtureLeases struct{}

func (fixtureLeases) AcquireLease(context.Context, model.Lease) error     { return nil }
func (fixtureLeases) RenewLease(context.Context, string, time.Time) error { return nil }
func (fixtureLeases) ReleaseLease(context.Context, string) error          { return nil }
func (fixtureLeases) LeaseExpiry(context.Context, string) (time.Time, error) {
	return time.Now().Add(time.Hour), nil
}

// fixtureLimits is the resolved budget every scenario starts from. A case that
// needs a tighter bound copies it and overrides the one field it is testing,
// so no case depends on another case's mutation.
func fixtureLimits() Limits {
	return Limits{
		MaxDepth: 3, MaxVisited: 50000, MaxEdges: 100000,
		MaxPageItems: 200, MaxReasonPaths: 3,
		QueryTimeout: 10 * time.Second, CursorTTL: 15 * time.Minute,
		FrontierBytes: 32 << 20,
	}
}

// graphScenario is one invariant under test. The body is a closure rather than
// typed request/want fields because the operations have incompatible shapes,
// and a shared struct would make every lane edit the same declaration.
type graphScenario struct {
	name string
	run  func(t *testing.T, f *graphFixture)
}

// TestGraphScenarios runs every lane's cases against a fresh fixture.
func TestGraphScenarios(t *testing.T) {
	scenarios := []graphScenario{
		// L1 TRAVERSE rows
		{
			// Protects the silent-drop failure mode: a hub whose fan-out exceeds
			// the edge budget must stop AT the budget, say so, and return every
			// edge it counted. A walk that kept going and dropped the overflow --
			// or that stopped without setting Truncated -- would present a
			// partial neighbourhood as the whole one. It also pins the batching:
			// 40 out-edges cost far fewer Edges round trips than one per node.
			name: "traverse/hub at max edges truncates without dropping",
			run: func(t *testing.T, f *graphFixture) {
				const maxEdges = 10
				e, err := New(Options{Adjacency: f, Limits: fixtureLimits()})
				if err != nil {
					t.Fatalf("New: %v", err)
				}
				got, err := e.Neighbors(context.Background(), model.GraphRequest{
					Start:     []model.NodeID{fixtureNodeID("n-hub")},
					Relations: []model.RelationKind{model.RelCalls},
					Direction: model.DirectionOutgoing,
					MaxEdges:  maxEdges,
				})
				if err != nil {
					t.Fatalf("Neighbors: %v", err)
				}
				if err := got.Validate(); err != nil {
					t.Fatalf("result does not satisfy its own contract: %v", err)
				}
				if !got.Meta.Truncated || got.Meta.TruncationReason != reasonEdgeBudget {
					t.Fatalf("truncated=%v reason=%q, want true and %q",
						got.Meta.Truncated, got.Meta.TruncationReason, reasonEdgeBudget)
				}
				if got.EdgeCount != maxEdges {
					t.Fatalf("edge_count = %d, want %d", got.EdgeCount, maxEdges)
				}
				if int64(len(got.Relations)) != got.EdgeCount {
					t.Fatalf("returned %d relations but counted %d: edges were dropped silently",
						len(got.Relations), got.EdgeCount)
				}
				if got.Direction != model.DirectionOutgoing {
					t.Fatalf("direction = %q, want %q", got.Direction, model.DirectionOutgoing)
				}
				if f.EdgeCalls >= fixtureLeafCount {
					t.Fatalf("%d Edges round trips for a %d-edge hub: the frontier was not batched",
						f.EdgeCalls, fixtureLeafCount)
				}
			},
		},

		{
			// The keyset loops ask Adjacency.Edges for adjacencyBatch (256)
			// rows, but the shipped reader clamps any limit above
			// model.MaxPageItems (200) down to it. A loop that ends on "the
			// page came back shorter than I asked for" therefore ends after its
			// FIRST page always, reads 200 of n-wide's 260 edges, and reports
			// the result as complete -- a capped neighbourhood presented as
			// exhaustive, and a directly connected target reported the way a
			// genuinely unreachable one is. Both walks that page edges are
			// pinned here: expand's levelEdges and pathWalk.expandBatch.
			name: "traverse/fan-out past the storage page clamp is read whole",
			run: func(t *testing.T, f *graphFixture) {
				limits := fixtureLimits()
				// The page bound must sit above the fan-out, so the only thing
				// that can cut the answer short is the keyset loop itself.
				limits.MaxPageItems = fixtureWideCount + 1
				e, err := New(Options{Adjacency: f, Limits: limits})
				if err != nil {
					t.Fatalf("New: %v", err)
				}
				wide := fixtureNodeID("n-wide")
				got, err := e.Callees(context.Background(), model.GraphRequest{
					Start:     []model.NodeID{wide},
					Direction: model.DirectionOutgoing,
				})
				if err != nil {
					t.Fatalf("Callees: %v", err)
				}
				if len(got.Relations) != fixtureWideCount || got.EdgeCount != int64(fixtureWideCount) {
					t.Fatalf("returned %d relations (edge_count %d) for a %d-edge hub: the keyset walk stopped on its first page",
						len(got.Relations), got.EdgeCount, fixtureWideCount)
				}
				if got.Meta.Truncated {
					t.Fatalf("truncated=%v reason=%q: a complete neighbourhood must not be reported as cut short",
						got.Meta.Truncated, got.Meta.TruncationReason)
				}
				// The last leaf by relation id is the one a first-page-only walk
				// never sees, so a one-hop route to it is the honest test of
				// "unreachable" against "never read".
				last := fixtureNodeID(fmt.Sprintf("n-wide-leaf-%03d", fixtureWideCount-1))
				route, err := e.ShortestPath(context.Background(), model.PathRequest{From: wide, To: last})
				if err != nil {
					t.Fatalf("ShortestPath: %v", err)
				}
				if len(route.Paths) != 1 {
					t.Fatalf("paths = %d (truncated %v, reason %q), want the one 1-hop route: a reachable target was reported unreachable",
						len(route.Paths), route.Meta.Truncated, route.Meta.TruncationReason)
				}
			},
		},

		// L2 PATH rows
		{
			// The two n-a -> n-z routes cost exactly the same (calls is 1 per
			// hop), so nothing about the facts orders them: the ONLY thing that
			// fixes the answer is the frozen tie-break -- cost ascending, then
			// the RelationID sequence compared lexicographically. If map
			// iteration order or heap-insertion order leaked into the walk,
			// two identical queries over identical facts would answer
			// differently, which is precisely the determinism Section 15.1
			// canonical context identity rests on. The query is run 100 times
			// because a map-order defect surfaces across runs, not within one.
			name: "path/equal_cost_routes_keep_the_frozen_order",
			run: func(t *testing.T, f *graphFixture) {
				engine, err := New(Options{Adjacency: f, Limits: fixtureLimits()})
				if err != nil {
					t.Fatalf("New: %v", err)
				}
				want := [][]model.RelationID{
					{fixtureRelationID(13), fixtureRelationID(14)}, // n-a -> n-p -> n-z
					{fixtureRelationID(15), fixtureRelationID(16)}, // n-a -> n-q -> n-z
				}
				for run := 0; run < 100; run++ {
					got, err := engine.ShortestPath(context.Background(), model.PathRequest{
						From:      fixtureNodeID("n-a"),
						To:        fixtureNodeID("n-z"),
						Relations: []model.RelationKind{model.RelCalls},
					})
					if err != nil {
						t.Fatalf("run %d: ShortestPath: %v", run, err)
					}
					if len(got.Paths) != len(want) {
						t.Fatalf("run %d: got %d paths, want %d: %+v", run, len(got.Paths), len(want), got.Paths)
					}
					for i, w := range want {
						if fmt.Sprint(got.Paths[i].Relations) != fmt.Sprint(w) {
							t.Fatalf("run %d: path %d relations = %v, want %v", run, i, got.Paths[i].Relations, w)
						}
						if got.Paths[i].CostUnits != 2 {
							t.Fatalf("run %d: path %d cost_units = %d, want 2", run, i, got.Paths[i].CostUnits)
						}
					}
				}
			},
		},

		// L3 IMPACT rows
		{
			// Protects the Section 14.3 entry contract: an impact answer whose
			// entries carry no reason, or no incoming/outgoing discriminator, is
			// an unexplained ranking a caller cannot act on -- and a cyclic graph
			// (n-a -> n-b -> n-c -> n-a) that admits a node twice would double the
			// same symbol and inflate its apparent impact. It also holds the
			// deferred-dependence disclosure, which no completed real index can
			// produce (the run drains the deferred queue), so this fixture is its
			// only proof.
			name: "impact entries are explained, directed and deduplicated across a cycle",
			run: func(t *testing.T, f *graphFixture) {
				engine, err := New(Options{Adjacency: f, Limits: fixtureLimits()})
				if err != nil {
					t.Fatalf("New: %v", err)
				}
				res, err := engine.Impact(context.Background(), model.ImpactRequest{
					Start:     []model.NodeID{fixtureNodeID("n-a")},
					Direction: model.DirectionOutgoing,
				})
				if err != nil {
					t.Fatalf("Impact: %v", err)
				}
				if len(res.Entries) == 0 {
					t.Fatal("Impact returned no entries; the fixture reaches n-b, n-c and the hub from n-a")
				}
				seen := map[model.NodeID]bool{}
				for _, entry := range res.Entries {
					if seen[entry.NodeID] {
						t.Errorf("node %q appears twice; the cycle admitted it more than once", entry.NodeID)
					}
					seen[entry.NodeID] = true
					if entry.NodeID == fixtureNodeID("n-a") {
						t.Error("the seed n-a is reported as affected by its own change")
					}
					if len(entry.Reasons) == 0 {
						t.Errorf("entry %q carries no reason", entry.NodeID)
					}
					if entry.Direction != model.DirectionOutgoing {
						t.Errorf("entry %q direction = %q, want the outgoing discriminator",
							entry.NodeID, entry.Direction)
					}
					// Every reason must end in the direction and the depth of the
					// edge that contributed it; a reason naming neither is an
					// unexplained ranking.
					for _, reason := range entry.Reasons {
						named := false
						for depth := 0; depth <= fixtureLimits().MaxDepth; depth++ {
							want := fmt.Sprintf("(outgoing, depth %d)", depth)
							if len(reason) >= len(want) && reason[len(reason)-len(want):] == want {
								named = true
							}
						}
						if !named {
							t.Errorf("entry %q reason %q names no direction and depth", entry.NodeID, reason)
						}
					}
				}
				if !seen[fixtureNodeID("n-c")] {
					t.Error("n-c is missing; it is reachable only through the cycle")
				}
				// The default impact allowlist includes reads and writes, so the
				// deferred dependence row must be disclosed unchanged and the
				// answer must not claim to be exhaustive.
				if !res.Meta.Truncated || res.Meta.TruncationReason != reasonDependence {
					t.Errorf("meta = (%v, %q), want truncated with %q",
						res.Meta.Truncated, res.Meta.TruncationReason, reasonDependence)
				}
				if len(res.Meta.Completeness) != 1 {
					t.Fatalf("completeness rows = %d, want the one deferred dependence row", len(res.Meta.Completeness))
				}
				row := res.Meta.Completeness[0]
				if row.State != model.CapabilityUnavailable || row.DiagnosticCode != model.CodeProviderUnavailable ||
					row.Details["reason"] != "units_deferred" {
					t.Errorf("deferred row = %+v, want it copied unchanged from the capability report", row)
				}
			},
		},

		// L5 CURSOR rows
		{name: "resumed page keeps the cumulative budget, neither reset nor doubled", run: func(t *testing.T, f *graphFixture) {
			signer, err := pagination.OpenSigner(t.TempDir())
			if err != nil {
				t.Fatalf("open signer: %v", err)
			}
			e, err := New(Options{Adjacency: f, Signer: signer, Limits: fixtureLimits()})
			if err != nil {
				t.Fatalf("new engine: %v", err)
			}
			lease, err := model.NewRandomID()
			if err != nil {
				t.Fatalf("lease id: %v", err)
			}
			const endpoint = "graph.neighbors"
			queryHash := traversalQueryHash(model.DirectionOutgoing,
				[]model.RelationKind{model.RelCalls}, []model.NodeID{"n-a"}, 3, 200)

			// Page 1 spent this much of the cumulative budget.
			const spentVisited, spentEdges = 7, 11
			token, err := e.nextTraversalCursor(&budget{visited: spentVisited, edges: spentEdges},
				continuation{Endpoint: endpoint, QueryHash: queryHash, LeaseID: lease,
					Depth: 1, LastOwner: fixtureNodeID("n-a"), LastKey: "rel-0003"})
			if err != nil || token == "" {
				t.Fatalf("page 1 cursor: token %q, err %v", token, err)
			}
			// Replaying page 1's cursor restores the SAME allowance every time: a
			// resume that reset it would hand the walk a fresh budget, and one that
			// accumulated would double it on the second replay.
			for attempt := 1; attempt <= 2; attempt++ {
				got, err := e.resumeTraversal(context.Background(), token, endpoint, queryHash, time.Now().Add(time.Minute))
				if err != nil {
					t.Fatalf("resume %d: %v", attempt, err)
				}
				if got.Budget.visited != spentVisited || got.Budget.edges != spentEdges {
					t.Fatalf("resume %d: budget = visited %d, edges %d; want %d and %d",
						attempt, got.Budget.visited, got.Budget.edges, spentVisited, spentEdges)
				}
				if got.Cursor.LastKey != "rel-0003" || got.Cursor.Depth != 1 {
					t.Fatalf("resume %d: keyset position = %q at depth %d; want rel-0003 at depth 1",
						attempt, got.Cursor.LastKey, got.Cursor.Depth)
				}
			}
			// A tampered token is never honoured with a budget of its own
			// choosing. The flipped character must NOT be the last one: the
			// token is unpadded base64 and Go's non-strict decoder ignores the
			// final character's unused low bits, so up to 16 distinct
			// characters there decode to identical bytes -- a "tamper" that is
			// a no-op on roughly one run in four, which is a flaky test rather
			// than a weak signature. Every interior character is significant.
			at := len(token) / 2
			flipped := byte('A')
			if token[at] == flipped {
				flipped = 'B'
			}
			tampered := token[:at] + string(flipped) + token[at+1:]
			_, err = e.resumeTraversal(context.Background(), tampered, endpoint, queryHash, time.Now().Add(time.Minute))
			var typed *model.Error
			if !errors.As(err, &typed) || typed.Code != model.CodeCursorInvalid {
				t.Fatalf("tampered cursor: err %v; want %s", err, model.CodeCursorInvalid)
			}
		}},
		{
			// A traversal level is emitted in (owner.Node asc, rel.ID asc) order
			// across independently keyset-paged node chunks, so a page that stops
			// mid-level cannot be resumed from a relation id alone. These two
			// seeds make that concrete: n-wide sorts BELOW n-a as a node id while
			// its 260 relation ids all sort ABOVE n-a's four, so the level emits
			// the high ids first. A resume that skipped on the relation id alone
			// would drop every n-a row as "already seen" -- a silent gap in a
			// paged neighbourhood, which is the C1 failure wearing a cursor.
			name: "a resumed traversal page repeats no edge and drops none",
			run: func(t *testing.T, f *graphFixture) {
				signer, err := pagination.OpenSigner(t.TempDir())
				if err != nil {
					t.Fatalf("open signer: %v", err)
				}
				spools, err := pagination.NewSpools(t.TempDir(), 1<<20, fixtureLeases{})
				if err != nil {
					t.Fatalf("new spools: %v", err)
				}
				limits := fixtureLimits()
				limits.MaxDepth = 2
				limits.MaxPageItems = 400
				e, err := New(Options{Adjacency: f, Signer: signer, Spools: spools, Limits: limits})
				if err != nil {
					t.Fatalf("new engine: %v", err)
				}
				seeds := []model.NodeID{fixtureNodeID("n-a"), fixtureNodeID("n-wide")}
				req := model.GraphRequest{GenerationID: 1, Start: seeds,
					Direction: model.DirectionOutgoing, Relations: []model.RelationKind{model.RelCalls}}

				// Ground truth: the same walk in one page.
				whole, err := e.Neighbors(context.Background(), req)
				if err != nil {
					t.Fatalf("unpaged walk: %v", err)
				}
				if whole.Meta.Truncated {
					t.Fatalf("ground-truth walk must be complete, got %q", whole.Meta.TruncationReason)
				}

				// The same walk in pages of 120, which lands the first boundary
				// inside n-wide's fan-out and the last one past it.
				req.Page = model.PageRequest{Limit: 120}
				seen := map[model.RelationID]int{}
				var order []model.RelationID
				for page := 1; ; page++ {
					if page > 8 {
						t.Fatalf("paged walk did not terminate after %d pages", page-1)
					}
					res, err := e.Neighbors(context.Background(), req)
					if err != nil {
						t.Fatalf("page %d: %v", page, err)
					}
					for _, rel := range res.Relations {
						seen[rel.ID]++
						order = append(order, rel.ID)
					}
					if res.Meta.NextCursor == "" {
						break
					}
					// A cumulative counter that reset per page would let a walk
					// spend its whole budget again on every continuation.
					if res.VisitedCount < int64(len(seen)) {
						t.Fatalf("page %d: visited %d is below the %d nodes already admitted",
							page, res.VisitedCount, len(seen))
					}
					// A cursor pins its own generation, and the page limit is part
					// of the normalized query the cursor is bound to, so a
					// continuation repeats the limit and drops the generation.
					req.GenerationID = 0
					req.Page = model.PageRequest{Limit: 120, Cursor: res.Meta.NextCursor}
				}
				if len(order) != len(whole.Relations) || len(seen) != len(order) {
					t.Fatalf("paged walk returned %d relations (%d distinct); the one-page walk returned %d",
						len(order), len(seen), len(whole.Relations))
				}
				for _, rel := range whole.Relations {
					if seen[rel.ID] != 1 {
						t.Fatalf("relation %s appeared %d times across the paged walk, want exactly once",
							rel.ID, seen[rel.ID])
					}
				}
			},
		},

		// L7 REFS rows
		{
			// Section 9.2: a relation and an occurrence are different counts. The
			// fixture's n-a -calls-> n-b edge is ONE sealed relation backed by TWO
			// evidence rows. Collapsing the two occurrences into one item would
			// under-report how often the symbol is used; emitting the relation
			// twice as two relations would over-report how many edges reach it.
			// Both are silent wrong answers a caller cannot detect.
			name: "references keeps relation count and occurrence count distinct",
			run: func(t *testing.T, f *graphFixture) {
				e, err := New(Options{Adjacency: f, Limits: fixtureLimits()})
				if err != nil {
					t.Fatalf("New: %v", err)
				}
				page, err := e.References(context.Background(), model.ReferenceRequest{
					NodeID:         fixtureNodeID("n-b"),
					Operation:      model.ReferenceReferences,
					SemanticSource: model.SemanticCanonical,
				})
				if err != nil {
					t.Fatalf("References: %v", err)
				}
				relations := map[model.RelationID]int{}
				occurrences := map[model.EvidenceID]bool{}
				for _, o := range page.Items {
					relations[o.RelationID]++
					occurrences[o.EvidenceID] = true
				}
				if len(relations) != 1 || len(occurrences) != 2 || len(page.Items) != 2 {
					t.Fatalf("want 1 relation and 2 distinct occurrences, got %d relations, %d occurrences in %d items: %+v",
						len(relations), len(occurrences), len(page.Items), page.Items)
				}
				if page.Meta.Truncated {
					t.Fatalf("a complete two-occurrence answer must not be truncated: %q", page.Meta.TruncationReason)
				}
			},
		},
	}
	for _, sc := range scenarios {
		t.Run(sc.name, func(t *testing.T) {
			sc.run(t, newGraphFixture(t))
		})
	}
}
