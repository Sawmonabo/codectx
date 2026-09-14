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

	for i, e := range edges {
		// Zero-padded hex counters keep the keyset order identical to the
		// declaration order above, so the order is still readable from source.
		id := model.RelationID(fmt.Sprintf("%064x", i))
		f.relations = append(f.relations, model.Relation{
			ID: id, From: fixtureNodeID(e.from), Kind: e.kind, To: fixtureNodeID(e.to)})
		// One occurrence per edge, except n-a -> n-b, which carries two: the
		// §9.2 relation-count vs occurrence-count distinction needs a relation
		// that is one relation and two occurrences.
		f.evidence[id] = []model.EvidenceID{model.EvidenceID(fixtureID(fmt.Sprintf("ev-%04d-0", i)))}
		if e.from == "n-a" && e.to == "n-b" && e.kind == model.RelCalls {
			f.evidence[id] = append(f.evidence[id], model.EvidenceID(fixtureID(fmt.Sprintf("ev-%04d-1", i))))
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
					{"rel-0013", "rel-0014"}, // n-a -> n-p -> n-z
					{"rel-0015", "rel-0016"}, // n-a -> n-q -> n-z
				}
				for run := 0; run < 100; run++ {
					got, err := engine.ShortestPath(context.Background(), model.PathRequest{
						From: "n-a", To: "n-z", Relations: []model.RelationKind{model.RelCalls},
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
					Depth: 1, LastKey: "rel-0003"})
			if err != nil || token == "" {
				t.Fatalf("page 1 cursor: token %q, err %v", token, err)
			}
			// Replaying page 1's cursor restores the SAME allowance every time: a
			// resume that reset it would hand the walk a fresh budget, and one that
			// accumulated would double it on the second replay.
			for attempt := 1; attempt <= 2; attempt++ {
				got, err := e.resumeTraversal(context.Background(), token, endpoint, queryHash)
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
			// A tampered token is never honoured with a budget of its own choosing.
			tampered := token[:len(token)-1] + "A"
			if tampered == token {
				tampered = token[:len(token)-1] + "B"
			}
			_, err = e.resumeTraversal(context.Background(), tampered, endpoint, queryHash)
			var typed *model.Error
			if !errors.As(err, &typed) || typed.Code != model.CodeCursorInvalid {
				t.Fatalf("tampered cursor: err %v; want %s", err, model.CodeCursorInvalid)
			}
		}},

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
