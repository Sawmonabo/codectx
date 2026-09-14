package graph

import (
	"context"
	"fmt"
	"sort"
	"testing"
	"time"

	"github.com/Sawmonabo/codectx/internal/model"
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

// fixtureLeafCount is the hub's fan-out. It is larger than any per-page bound a
// test sets, so a truncation case has something real to truncate.
const fixtureLeafCount = 40

// graphFixture is a deterministic in-memory Adjacency. Relations are stored
// once, sorted by RelationID, so every read is keyset-ordered without the test
// having to sort anything.
type graphFixture struct {
	nodes     map[model.NodeID]model.Node
	relations []model.Relation
	evidence  map[model.RelationID][]model.EvidenceID
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
		evidence: map[model.RelationID][]model.EvidenceID{},
		binding: model.Binding{
			RepositoryID: "repo-1",
			SnapshotID:   "snap-1",
			GenerationID: 1,
			AnalysisKey:  "akey-1",
		},
	}

	addNode := func(id model.NodeID, kind model.NodeKind, name string) {
		f.nodes[id] = model.Node{
			ID: id, Kind: kind, Name: name, QualifiedName: name,
			Language: "go", SemanticSource: model.SemanticCanonical,
		}
	}
	addNode("pkg-app", model.NodePackage, "app")
	addNode("pkg-lib", model.NodePackage, "lib")
	for _, id := range []model.NodeID{"n-a", "n-b", "n-c", "n-hub", "n-p", "n-q", "n-z"} {
		addNode(id, model.NodeFunction, string(id))
	}
	addNode("n-var", model.NodeVariable, "n-var")
	addNode("n-sink", model.NodeFunction, "n-sink")
	for i := 0; i < fixtureLeafCount; i++ {
		addNode(model.NodeID(fmt.Sprintf("n-leaf-%02d", i)), model.NodeFunction, fmt.Sprintf("leaf%02d", i))
	}

	// edges are declared in a stable order; RelationIDs are assigned from it so
	// the keyset order is reproducible from the source alone.
	type edge struct {
		from model.NodeID
		kind model.RelationKind
		to   model.NodeID
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
		edges = append(edges, edge{"n-hub", model.RelCalls, model.NodeID(fmt.Sprintf("n-leaf-%02d", i))})
	}
	// n-a calls the hub, so the fan-out is reachable from the same seed as the cycle.
	edges = append(edges, edge{"n-a", model.RelCalls, "n-hub"})

	for i, e := range edges {
		id := model.RelationID(fmt.Sprintf("rel-%04d", i))
		f.relations = append(f.relations, model.Relation{ID: id, From: e.from, Kind: e.kind, To: e.to})
		// One occurrence per edge, except n-a -> n-b, which carries two: the
		// §9.2 relation-count vs occurrence-count distinction needs a relation
		// that is one relation and two occurrences.
		f.evidence[id] = []model.EvidenceID{model.EvidenceID(fmt.Sprintf("ev-%04d-0", i))}
		if e.from == "n-a" && e.to == "n-b" && e.kind == model.RelCalls {
			f.evidence[id] = append(f.evidence[id], model.EvidenceID(fmt.Sprintf("ev-%04d-1", i)))
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

// Edges returns the visible edges touching any of nodes, keyset-ordered by
// RelationID after `after`, at most limit rows. It follows the Adjacency
// contract: an empty kinds slice is "no kind filter", and a non-positive limit
// is treated as unbounded here only because a test may omit it -- a real
// implementation may reject it.
func (f *graphFixture) Edges(ctx context.Context, nodes []model.NodeID, direction model.Direction,
	kinds []model.RelationKind, after model.RelationID, limit int) ([]model.Relation, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	f.EdgeCalls++
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
		if limit > 0 && len(out) == limit {
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

// EvidenceFor hydrates the evidence backing a page of relations in one call.
func (f *graphFixture) EvidenceFor(ctx context.Context, relations []model.RelationID, limit int) (map[model.RelationID][]model.EvidenceID, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	out := make(map[model.RelationID][]model.EvidenceID, len(relations))
	for _, id := range relations {
		ev := f.evidence[id]
		if len(ev) == 0 {
			continue
		}
		if limit > 0 && len(ev) > limit {
			ev = ev[:limit]
		}
		out[id] = append([]model.EvidenceID(nil), ev...)
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

		// L2 PATH rows

		// L3 IMPACT rows
		{
			// Protects the Section 14.3 entry contract: an impact answer whose
			// entries carry no reason, or no incoming/outgoing discriminator, is
			// an unexplained ranking a caller cannot act on -- and a cyclic graph
			// (n-a -> n-b -> n-c -> n-a) that admits a node twice would double the
			// same symbol and inflate its apparent impact. It also holds the
			// deferred-dependence disclosure, which no real index can produce
			// (a completed run drains the deferred queue), so this fixture is its
			// only proof.
			name: "impact entries are explained, directed and deduplicated across a cycle",
			run: func(t *testing.T, f *graphFixture) {
				// expand is lane L1's; against L0's typed stub the walk admits
				// nothing, so this row waits for traverse.go rather than passing
				// vacuously. It runs in full the moment that lane lands.
				// The probe passes inert but VALID options: a zero expandOptions
				// has a nil Budget, which the real body may dereference before it
				// notices there are no seeds.
				probe := expandOptions{
					Direction: model.DirectionOutgoing, MaxDepth: 1, BatchSize: adjacencyBatch,
					Budget: &budget{visited: 1, edges: 1, deadline: time.Now().Add(time.Minute)},
				}
				if typed, ok := expand(context.Background(), f, nil, probe,
					func(frontierState, model.Relation) error { return nil }).(*model.Error); ok &&
					typed.Code == model.CodeInternal && typed.Details["operation"] == "expand" {
					t.Skip("expand is still lane L1's stub; this row runs once traverse.go lands")
				}
				engine, err := New(Options{Adjacency: f, Limits: fixtureLimits()})
				if err != nil {
					t.Fatalf("New: %v", err)
				}
				res, err := engine.Impact(context.Background(), model.ImpactRequest{
					Start:     []model.NodeID{"n-a"},
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
					if entry.NodeID == "n-a" {
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
					// edge that contributed it; a reason that names neither is an
					// unexplained ranking. Checked by suffix rather than with a
					// new import, because an import line in this shared file is a
					// merge conflict with every other lane.
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
				if !seen["n-c"] {
					t.Error("n-c is missing; it is reachable only through the cycle")
				}
				// The default impact allowlist includes reads and writes, so the
				// deferred dependence row must be disclosed unchanged and the
				// answer must not claim to be exhaustive.
				if !res.Meta.Truncated || res.Meta.TruncationReason != pendingTruncationReason {
					t.Errorf("meta = (%v, %q), want truncated with %q",
						res.Meta.Truncated, res.Meta.TruncationReason, pendingTruncationReason)
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

		// L7 REFS rows
	}
	for _, sc := range scenarios {
		t.Run(sc.name, func(t *testing.T) {
			sc.run(t, newGraphFixture(t))
		})
	}
}
