package graph

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"sort"
	"sync"
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
	// n-d is pkg-app's TOP-LEVEL declaration, attached with `defines` rather
	// than `contains` -- which is how a provider actually emits one
	// (internal/provider/treesitter/facts.go reserves `contains` for a nested
	// declaration). Without one such node the fixture's containers hold only
	// `contains` children and a map counted over `contains` alone passes here
	// while reporting SymbolCount 0 for every package of a real repository.
	addNode("n-d", model.NodeFunction, "n-d")
	addNode("n-var", model.NodeVariable, "n-var")
	addNode("n-sink", model.NodeFunction, "n-sink")
	for i := 0; i < fixtureLeafCount; i++ {
		addNode(fmt.Sprintf("n-leaf-%02d", i), model.NodeFunction, fmt.Sprintf("leaf%02d", i))
	}
	addNode("n-wide", model.NodeFunction, "n-wide")
	for i := 0; i < fixtureWideCount; i++ {
		addNode(fmt.Sprintf("n-wide-leaf-%03d", i), model.NodeFunction, fmt.Sprintf("wide%03d", i))
	}
	// pkg-wide-a and pkg-wide-b make n-wide's fan-out a CROSS-package one, so a
	// rollup seeded at n-wide has 261 distinct endpoints and containerPackages
	// asks containsEdges for a 256-node batch whose containment rows do not fit
	// in one clamped page. Two packages, never one: an edge whose endpoints
	// share a container rolls up to nothing, and an edge BETWEEN the two would
	// give ShortestPath a second route into the leaves.
	addNode("pkg-wide-a", model.NodePackage, "wide-a")
	addNode("pkg-wide-b", model.NodePackage, "wide-b")
	// n-ref is the reference target whose incoming edges outrun one clamped
	// page. Its sources are fresh nodes rather than the n-wide leaves, so no
	// existing scenario's neighbourhood gains an edge it did not have.
	addNode("n-ref", model.NodeFunction, "n-ref")
	addNode("n-ref-caller", model.NodeFunction, "n-ref-caller")
	for i := 0; i < fixtureWideCount; i++ {
		addNode(fmt.Sprintf("n-ref-src-%03d", i), model.NodeFunction, fmt.Sprintf("refsrc%03d", i))
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
	// bare names, by declaration position, the edges that carry NO evidence
	// row. Containment legitimately carries none (rollup.go says so), and an
	// evidence-free relation is the only shape that lets a reference walk
	// advance its keyset across a whole clamped page without filling the page
	// -- which is the only way referencePage's empty-page termination is
	// reached at all.
	bare := map[int]bool{}
	addBare := func(e edge) {
		bare[len(edges)] = true
		edges = append(edges, e)
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
	// Containment for that neighbourhood, again declared after everything that
	// existed before it. 261 incoming contains rows over a 256-node batch is
	// more than one clamped page, which is what makes rollup.go's containsEdges
	// loop observable at all.
	addBare(edge{"pkg-wide-a", model.RelContains, "n-wide"})
	for i := 0; i < fixtureWideCount; i++ {
		addBare(edge{"pkg-wide-b", model.RelContains, fmt.Sprintf("n-wide-leaf-%03d", i)})
	}
	// n-ref's incoming references carry no evidence, so a reference walk reads
	// them, advances its keyset past them and emits nothing for them...
	for i := 0; i < fixtureWideCount; i++ {
		addBare(edge{fmt.Sprintf("n-ref-src-%03d", i), model.RelReferences, "n-ref"})
	}
	// ...and this is the one occurrence-bearing relation, declared last so it
	// sorts beyond the first clamped page. A walk that ended on a short page
	// never reads it and reports a referenced symbol as unreferenced.
	edges = append(edges, edge{"n-ref-caller", model.RelCalls, "n-ref"})
	// Declared last so every relation id above keeps the position it had.
	edges = append(edges, edge{"pkg-app", model.RelDefines, "n-d"})

	// evidenceRow is a whole evidence row, not just its id: an occurrence's
	// precision class, file and byte range live here and nowhere else, so a
	// fixture that carried ids alone could not tell a hydrated occurrence from
	// an unhydrated one. The interval is a ByteRange with a nil Range, which is
	// exactly what a row read back from storage carries -- the evidence table
	// stores start_byte/end_byte and no line or column -- so a hydration path
	// that dropped it could not pass here either.
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
			Bytes:           &model.ByteRange{Start: uint64(i) * 16, End: uint64(i)*16 + 8},
		}
	}
	for i, e := range edges {
		id := fixtureRelationID(i)
		f.relations = append(f.relations, model.Relation{
			ID: id, From: fixtureNodeID(e.from), Kind: e.kind, To: fixtureNodeID(e.to)})
		if bare[i] {
			// A canonical relation with no evidence row at all. References
			// contributes no occurrence for it; every other reader still sees
			// the relation.
			continue
		}
		// One occurrence per edge, except n-a -> n-b, which carries two: the
		// §9.2 relation-count vs occurrence-count distinction needs a relation
		// that is one relation and two occurrences.
		f.evidence[id] = []model.Evidence{evidenceRow(id, i, 0)}
		if e.from == "n-a" && e.to == "n-b" && e.kind == model.RelCalls {
			f.evidence[id] = append(f.evidence[id], evidenceRow(id, i, 1))
		}
	}
	sort.Slice(f.relations, func(i, j int) bool { return f.relations[i].ID < f.relations[j].ID })

	// The generation's capability report, as PinnedReader.Capabilities returns
	// it: one ordinary fresh row, and one deferred dependence row. An answer
	// that traverses a dependence-only kind must disclose the deferred row, and
	// EVERY answer must carry the report itself -- a graph answer that dropped
	// it would report "no capabilities" on a generation whose search answers
	// report them from the same reader.
	f.caps = []model.CapabilityState{{
		ProviderID: "canonical",
		Capability: "relations",
		Scope:      "workspace",
		State:      model.CapabilityFresh,
	}, {
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

// Containers is the optional graph.ContainerReader seam: the visible container
// nodes of kinds, keyset-ordered by NodeID after `after`, clamped exactly as
// the shipped reader clamps a page. The fixture implements it because the app
// adapter must, and because a repository map has no seed to hang off -- an
// Adjacency without it can list no container at all.
func (f *graphFixture) Containers(ctx context.Context, kinds []model.NodeKind,
	after model.NodeID, limit int) ([]model.Node, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	want := make(map[model.NodeKind]bool, len(kinds))
	for _, k := range kinds {
		want[k] = true
	}
	var out []model.Node
	for _, n := range f.nodes {
		if want[n.Kind] && n.ID > after {
			out = append(out, n)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	if l := fixtureEdgePageLimit(limit); len(out) > l {
		out = out[:l]
	}
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

// fixtureLeases is the LeaseStore pagination.Spools consults before it writes
// or replays a spool, and the store the engine mints its cursor leases in.
//
// It tracks liveness for real rather than reporting every lease live, because
// the whole difference a continuation depends on is WHICH lease its token
// names: the pinned reader's query lease is released the moment the request
// returns, and a fake that answered "live" for a released lease could not tell
// a usable cursor from one the next invocation will refuse. A released or
// unknown lease is CTX_CURSOR_INVALID with the storage layer's own wording.
type fixtureLeases struct {
	mu   sync.Mutex
	live map[string]time.Time
}

func newFixtureLeases() *fixtureLeases { return &fixtureLeases{live: map[string]time.Time{}} }

func (l *fixtureLeases) AcquireLease(_ context.Context, lease model.Lease, _ string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.live[lease.ID] = lease.ExpiresAt
	return nil
}

// liveCount is how many leases are still held. A continuation is used once, so
// a walk read to exhaustion must end with none of its own.
func (l *fixtureLeases) liveCount() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.live)
}

func (l *fixtureLeases) RenewLease(_ context.Context, id string, expiresAt time.Time) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if _, ok := l.live[id]; !ok {
		return fixtureLeaseGone()
	}
	l.live[id] = expiresAt
	return nil
}

// ReleaseLease is idempotent, as the storage DELETE is: releasing twice is not
// an error, it just leaves the lease gone.
func (l *fixtureLeases) ReleaseLease(_ context.Context, id string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.live, id)
	return nil
}

func (l *fixtureLeases) LeaseExpiry(_ context.Context, id string) (time.Time, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	expires, ok := l.live[id]
	if !ok {
		return time.Time{}, fixtureLeaseGone()
	}
	return expires, nil
}

// fixtureLeaseGone is storage's refusal, verbatim (sqlite/lease.go): the
// message an operator sees when a continuation names a lease nobody holds.
func fixtureLeaseGone() error {
	return &model.Error{Code: model.CodeCursorInvalid, Message: "lease has expired or was released"}
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
				got, err := e.Neighbors(context.Background(), model.GraphRequest{
					Start:     []model.NodeID{wide},
					Direction: model.DirectionOutgoing,
					Relations: []model.RelationKind{model.RelCalls},
				})
				if err != nil {
					t.Fatalf("Neighbors: %v", err)
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

		{
			// resources.query_memory_bytes is the ONLY bound on how much of one
			// frontier level is held in memory at once, and both the config and
			// docs/queries.md promise it exists. A level that keeps accumulating
			// past it restores the unbounded hub the bound was written to stop,
			// and -- because the walk then still reports a clean finish -- the
			// operator is never told the ceiling was crossed. n-wide's fan-out
			// under a budget that holds only a handful of its rows is the
			// smallest case that separates "stopped at the ceiling and said so"
			// from "ignored the ceiling". The complementary half (the whole
			// fan-out is carried under the 32 MiB default, untruncated) is held
			// by the storage-clamp row above and is not repeated here.
			name: "traverse/a level past the frontier byte budget truncates and says so",
			run: func(t *testing.T, f *graphFixture) {
				limits := fixtureLimits()
				// Neither the page bound nor the edge budget may be what cuts
				// this walk short: the frontier ceiling must be the only one
				// n-wide's fan-out can reach.
				limits.MaxPageItems = fixtureWideCount + 1
				limits.FrontierBytes = 4096
				e, err := New(Options{Adjacency: f, Limits: limits})
				if err != nil {
					t.Fatalf("New: %v", err)
				}
				got, err := e.Neighbors(context.Background(), model.GraphRequest{
					Start:     []model.NodeID{fixtureNodeID("n-wide")},
					Direction: model.DirectionOutgoing,
					Relations: []model.RelationKind{model.RelCalls},
				})
				if err != nil {
					t.Fatalf("Neighbors: %v", err)
				}
				if !got.Meta.Truncated || got.Meta.TruncationReason != reasonFrontierBytes {
					t.Fatalf("truncated=%v reason=%q, want true and %q: the frontier byte ceiling was crossed without disclosure",
						got.Meta.Truncated, got.Meta.TruncationReason, reasonFrontierBytes)
				}
				if len(got.Relations) == 0 || len(got.Relations) >= fixtureWideCount {
					t.Fatalf("returned %d of %d edges under a %d-byte frontier budget: the budget bounded nothing",
						len(got.Relations), fixtureWideCount, limits.FrontierBytes)
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
				// The answer carries the generation's whole capability report,
				// with the deferred row disclosed once inside it: dropping the
				// report leaves the caller unable to tell a complete answer from
				// a degraded one, and publishing the deferred row a second time
				// alongside it both misreports and grows a full report past
				// model.MaxCapabilityStates.
				if len(res.Meta.Completeness) != len(f.caps) {
					t.Fatalf("completeness rows = %d, want the generation's %d capability rows",
						len(res.Meta.Completeness), len(f.caps))
				}
				var row model.CapabilityState
				for _, c := range res.Meta.Completeness {
					if c.ProviderID == "dependence" {
						row = c
					}
				}
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
			e, err := New(Options{Adjacency: f, Signer: signer,
				Leases: pagination.NewLeases(newFixtureLeases(), fixtureLimits().CursorTTL), Limits: fixtureLimits()})
			if err != nil {
				t.Fatalf("new engine: %v", err)
			}
			const endpoint = "graph.neighbors"
			queryHash := traversalQueryHash(model.DirectionOutgoing,
				[]model.RelationKind{model.RelCalls}, []model.NodeID{"n-a"}, 3, 200)

			// Page 1 spent this much of the cumulative budget.
			const spentVisited, spentEdges = 7, 11
			token, err := e.nextTraversalCursor(context.Background(),
				&budget{visited: spentVisited, edges: spentEdges},
				continuation{Endpoint: endpoint, QueryHash: queryHash,
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
				store := newFixtureLeases()
				spools, err := pagination.NewSpools(t.TempDir(), 1<<20, store)
				if err != nil {
					t.Fatalf("new spools: %v", err)
				}
				limits := fixtureLimits()
				// Unlimited depth: this row proves that pages partition the same
				// edge set the one-page walk returns, and a finite depth bound now
				// (correctly) reports reasonDepth on a graph deeper than it, which
				// would make the ground truth a truncated answer rather than the
				// whole one this row compares against.
				limits.MaxDepth = 0
				limits.MaxPageItems = 400
				e, err := New(Options{Adjacency: f, Signer: signer, Spools: spools,
					Leases: pagination.NewLeases(store, limits.CursorTTL), Limits: limits})
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

		{
			// A continuation is state, and state that is never reclaimed is a
			// leak with a 15-minute half-life: every page of every walk in the
			// process mints a cursor lease and spills a spool, and once the page
			// after it has been served neither will be read again. Left to their
			// TTL they pin a generation retention may not collect -- the 118
			// unreleased leases obligation 6 names. This walks a multi-page
			// traversal to exhaustion and asks what it left behind: nothing.
			name: "a walk read to exhaustion releases every continuation it consumed",
			run: func(t *testing.T, f *graphFixture) {
				signer, err := pagination.OpenSigner(t.TempDir())
				if err != nil {
					t.Fatalf("open signer: %v", err)
				}
				store := newFixtureLeases()
				spoolDir := t.TempDir()
				spools, err := pagination.NewSpools(spoolDir, 1<<20, store)
				if err != nil {
					t.Fatalf("new spools: %v", err)
				}
				limits := fixtureLimits()
				limits.MaxDepth = 2
				limits.MaxPageItems = 400
				e, err := New(Options{Adjacency: f, Signer: signer, Spools: spools,
					Leases: pagination.NewLeases(store, limits.CursorTTL), Limits: limits})
				if err != nil {
					t.Fatalf("new engine: %v", err)
				}
				req := model.GraphRequest{GenerationID: 1,
					Start:     []model.NodeID{fixtureNodeID("n-a"), fixtureNodeID("n-wide")},
					Direction: model.DirectionOutgoing, Relations: []model.RelationKind{model.RelCalls},
					Page: model.PageRequest{Limit: 120}}
				pages := 0
				for {
					res, err := e.Neighbors(context.Background(), req)
					if err != nil {
						t.Fatalf("page %d: %v", pages+1, err)
					}
					pages++
					if res.Meta.NextCursor == "" {
						break
					}
					if pages > 8 {
						t.Fatalf("paged walk did not terminate after %d pages", pages)
					}
					req.GenerationID = 0
					req.Page = model.PageRequest{Limit: 120, Cursor: res.Meta.NextCursor}
				}
				if pages < 2 {
					t.Fatalf("the walk answered in %d page(s); this row needs a continuation to consume", pages)
				}
				if live := store.liveCount(); live != 0 {
					t.Fatalf("a walk of %d pages left %d live cursor leases, want 0; each holds a generation against retention until its TTL",
						pages, live)
				}
				left, err := os.ReadDir(spoolDir)
				if err != nil {
					t.Fatalf("read spool dir: %v", err)
				}
				if len(left) != 0 {
					t.Fatalf("a walk of %d pages left %d spool file(s) behind, want 0", pages, len(left))
				}
			},
		},

		{
			// Impact ranks its WHOLE walk and cuts the ranked list, so its page
			// boundary is a rank rather than a keyset position. The failure this
			// protects is a paged impact answer that silently loses the tail of
			// its own ranking: an entry the walk found, ranked, and then never
			// served because page 2 had nothing to replay. The n-wide hub gives
			// a ranked list far longer than the page, and the pages are walked
			// against an Adjacency that fails EVERY read -- so a continuation
			// that touched the graph again could not even complete, which is
			// what makes "no re-walk, no traversal budget spent" an observation
			// rather than an inference.
			name: "a paged impact answer replays its ranked tail without walking again",
			run: func(t *testing.T, f *graphFixture) {
				signer, err := pagination.OpenSigner(t.TempDir())
				if err != nil {
					t.Fatalf("open signer: %v", err)
				}
				store := newFixtureLeases()
				spools, err := pagination.NewSpools(t.TempDir(), 8<<20, store)
				if err != nil {
					t.Fatalf("new spools: %v", err)
				}
				limits := fixtureLimits()
				leases := pagination.NewLeases(store, limits.CursorTTL)
				newEngine := func(a Adjacency) *Engine {
					e, err := New(Options{Adjacency: a, Signer: signer, Spools: spools,
						Leases: leases, Limits: limits})
					if err != nil {
						t.Fatalf("new engine: %v", err)
					}
					return e
				}
				seeds := []model.NodeID{fixtureNodeID("n-hub")}
				const pageLimit = 10

				// Ground truth: the same answer ranked and served in one page.
				whole, err := newEngine(f).Impact(context.Background(), model.ImpactRequest{
					Start: seeds, Direction: model.DirectionBoth,
					Page: model.PageRequest{Limit: model.MaxPageItems}})
				if err != nil {
					t.Fatalf("one-page impact: %v", err)
				}
				if whole.Meta.TruncationReason == reasonPageFull {
					t.Fatalf("the ground-truth answer was itself cut at its page; it cannot be the whole ranking")
				}
				if len(whole.Entries) <= pageLimit {
					t.Fatalf("ground truth holds %d entries; the fixture must rank more than one page of %d",
						len(whole.Entries), pageLimit)
				}

				// Every page after the first is answered over an Adjacency whose
				// every read fails. Only the spooled tail can serve them.
				pages := 0
				var order []model.NodeID
				req := model.ImpactRequest{Start: seeds, Direction: model.DirectionBoth,
					Page: model.PageRequest{Limit: pageLimit}}
				engine := newEngine(f)
				for {
					pages++
					if pages > 32 {
						t.Fatalf("paged impact did not terminate after %d pages", pages-1)
					}
					res, err := engine.Impact(context.Background(), req)
					if err != nil {
						t.Fatalf("page %d: %v", pages, err)
					}
					if err := res.Validate(); err != nil {
						t.Fatalf("page %d does not satisfy its own contract: %v", pages, err)
					}
					for _, entry := range res.Entries {
						order = append(order, entry.NodeID)
					}
					// The walk happened once: its cumulative counters and its
					// rollup describe the whole answer and are carried unchanged.
					if res.VisitedCount != whole.VisitedCount || res.EdgeCount != whole.EdgeCount {
						t.Fatalf("page %d counted (%d visited, %d edges), want the walk's (%d, %d)",
							pages, res.VisitedCount, res.EdgeCount, whole.VisitedCount, whole.EdgeCount)
					}
					if len(res.Packages) != len(whole.Packages) {
						t.Fatalf("page %d carried %d rollup pairs, want the answer's %d",
							pages, len(res.Packages), len(whole.Packages))
					}
					// The capability report is answer-level too, and a
					// continuation reads no facts: a page that dropped it would
					// report a degraded generation as a complete one.
					if len(res.Meta.Completeness) != len(whole.Meta.Completeness) {
						t.Fatalf("page %d carried %d completeness rows, want the answer's %d",
							pages, len(res.Meta.Completeness), len(whole.Meta.Completeness))
					}
					if res.Meta.NextCursor == "" {
						break
					}
					req = model.ImpactRequest{Start: seeds, Direction: model.DirectionBoth,
						Page: model.PageRequest{Limit: pageLimit, Cursor: res.Meta.NextCursor}}
					engine = newEngine(unreadableAdjacency{f})
				}
				if pages < 2 {
					t.Fatalf("the answer was served in %d page(s); the case needs a page boundary", pages)
				}
				// One assertion for three invariants: the union equals the
				// one-page ground truth, nothing repeats, and the rank order
				// survives every page boundary.
				if len(order) != len(whole.Entries) {
					t.Fatalf("the paged answer served %d entries, the one-page answer %d",
						len(order), len(whole.Entries))
				}
				for i, want := range whole.Entries {
					if order[i] != want.NodeID {
						t.Fatalf("entry %d of the paged answer is %s, want %s: the ranked order did not survive paging",
							i, order[i], want.NodeID)
					}
				}
			},
		},

		{
			// The defect this protects is a continuation nobody can use. A
			// token that names the PINNED READER's query lease is dead on
			// arrival: that lease is released when the request returns, so the
			// NEXT invocation presents a cursor whose spool the lease store
			// refuses -- every `callers --cursor` and `impact --cursor` fails
			// with "lease has expired or was released" while the page it names
			// sits on disk. A cursor must own a lease of its own, minted for
			// the same generation and outliving the reader that answered.
			name: "a continuation survives the release of the reader's query lease",
			run: func(t *testing.T, f *graphFixture) {
				ctx := context.Background()
				signer, err := pagination.OpenSigner(t.TempDir())
				if err != nil {
					t.Fatalf("open signer: %v", err)
				}
				store := newFixtureLeases()
				spools, err := pagination.NewSpools(t.TempDir(), 1<<20, store)
				if err != nil {
					t.Fatalf("new spools: %v", err)
				}
				limits := fixtureLimits()
				limits.MaxDepth = 2
				limits.MaxPageItems = 120
				leases := pagination.NewLeases(store, limits.CursorTTL)
				e, err := New(Options{Adjacency: f, Signer: signer, Spools: spools,
					Leases: leases, Limits: limits})
				if err != nil {
					t.Fatalf("new engine: %v", err)
				}

				// The query lease PinGeneration takes for ONE request. It is
				// live while page 1 is answered and gone before page 2 is
				// asked for, exactly as reader.Close() leaves it.
				b := f.Binding()
				readerLease, err := leases.Acquire(ctx, b.GenerationID, b.SnapshotID, model.LeaseQuery)
				if err != nil {
					t.Fatalf("reader query lease: %v", err)
				}

				req := model.GraphRequest{GenerationID: 1,
					Start:     []model.NodeID{fixtureNodeID("n-a"), fixtureNodeID("n-wide")},
					Direction: model.DirectionOutgoing, Relations: []model.RelationKind{model.RelCalls},
					Page: model.PageRequest{Limit: 120}}
				page1, err := e.Neighbors(ctx, req)
				if err != nil {
					t.Fatalf("page 1: %v", err)
				}
				if page1.Meta.NextCursor == "" {
					t.Fatalf("page 1 offered no continuation; the case needs a page boundary")
				}
				if err := store.ReleaseLease(ctx, readerLease.ID); err != nil {
					t.Fatalf("release the reader's query lease: %v", err)
				}

				req.GenerationID = 0
				req.Page = model.PageRequest{Limit: 120, Cursor: page1.Meta.NextCursor}
				page2, err := e.Neighbors(ctx, req)
				if err != nil {
					t.Fatalf("page 2 after the reader's query lease was released: %v", err)
				}
				if len(page2.Relations) == 0 {
					t.Fatalf("page 2 served no edges; the continuation resumed nothing")
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

		// FX-C14c-PROOF row
		{
			// The last two keyset loops that ask Adjacency.Edges for
			// adjacencyBatch (256) rows: rollup.go's containsEdges and
			// references.go's referencePage. The reader clamps every request
			// down to model.MaxPageItems (200), so "the page came back shorter
			// than I asked for" is true of EVERY page, and a loop that ends on
			// it ends after its first one. Both sites are pinned here because
			// both then answer silently wrong rather than failing: the rollup
			// loses the containers of every node past row 200 and under-counts
			// the pair, and the reference walk loses every occurrence past row
			// 200 and reports a referenced symbol as unreferenced.
			name: "keyset walks past the storage page clamp read every page",
			run: func(t *testing.T, f *graphFixture) {
				e, err := New(Options{Adjacency: f, Limits: fixtureLimits()})
				if err != nil {
					t.Fatalf("New: %v", err)
				}
				roll, err := e.PackageDependencies(context.Background(), model.GraphRequest{
					Start:     []model.NodeID{fixtureNodeID("n-wide")},
					Relations: []model.RelationKind{model.RelCalls},
					Direction: model.DirectionOutgoing,
				})
				if err != nil {
					t.Fatalf("PackageDependencies: %v", err)
				}
				if len(roll.Items) != 1 || roll.Items[0].PairCount != int64(fixtureWideCount) {
					t.Fatalf("containsEdges: rollup returned %+v, want one wide-a -> wide-b pair over %d edges; a short-page break loses the containers past the first clamped page",
						roll.Items, fixtureWideCount)
				}
				refs, err := e.References(context.Background(), model.ReferenceRequest{
					NodeID:         fixtureNodeID("n-ref"),
					Operation:      model.ReferenceReferences,
					SemanticSource: model.SemanticCanonical,
				})
				if err != nil {
					t.Fatalf("References: %v", err)
				}
				if len(refs.Items) != 1 || refs.Items[0].FromNodeID != fixtureNodeID("n-ref-caller") {
					t.Fatalf("referencePage: references returned %d occurrences (%+v), want the single one from n-ref-caller; it sorts past the first clamped page of evidence-free relations",
						len(refs.Items), refs.Items)
				}
				// The occurrence must also be locatable and attributable: the
				// stored byte interval has to survive hydration, and the
				// from-node's qualified name has to be filled from the page's
				// one batched node read. Without either, every occurrence of a
				// symbol renders as the same anonymous row.
				if got := refs.Items[0]; got.Bytes == nil || got.FromName != "n-ref-caller" {
					t.Fatalf("occurrence %+v: want the stored byte interval carried through and from_name %q",
						got, "n-ref-caller")
				}
			},
		},

		{
			// The gate wait is admitted work like any other, so it must sit
			// INSIDE the request deadline. If the deadline is installed after
			// the Acquire -- or not at all -- `codectx refs` behind two busy
			// graph slots waits forever: no timeout of its own, no cancellation,
			// no output, and nothing in the answer to tell the operator why. The
			// failure mode this row protects is that hang, which is why the
			// load-bearing assertion is elapsed wall clock rather than the error
			// code. Neighbors rides along because traverse.go installs the same
			// deadline above the same Acquire and can regress the same way.
			name: "a busy gate ends at the query deadline instead of blocking",
			run: func(t *testing.T, f *graphFixture) {
				const timeout = 50 * time.Millisecond
				limits := fixtureLimits()
				limits.QueryTimeout = timeout
				e, err := New(Options{Adjacency: f, Limits: limits, Gate: blockingGate{}})
				if err != nil {
					t.Fatalf("New: %v", err)
				}
				calls := []struct {
					name string
					run  func() error
				}{
					{"References", func() error {
						_, err := e.References(context.Background(), model.ReferenceRequest{
							NodeID:         fixtureNodeID("n-b"),
							Operation:      model.ReferenceReferences,
							SemanticSource: model.SemanticCanonical,
						})
						return err
					}},
					{"Neighbors", func() error {
						_, err := e.Neighbors(context.Background(), model.GraphRequest{
							Start:     []model.NodeID{fixtureNodeID("n-b")},
							Direction: model.DirectionIncoming,
							Relations: []model.RelationKind{model.RelCalls},
						})
						return err
					}},
				}
				for _, c := range calls {
					done := make(chan error, 1)
					start := time.Now()
					go func() { done <- c.run() }()
					select {
					case err := <-done:
						if elapsed := time.Since(start); elapsed > 100*timeout {
							t.Fatalf("%s returned after %s under a %s query timeout: the gate wait outlives the deadline",
								c.name, elapsed, timeout)
						}
						var typed *model.Error
						if !errors.As(err, &typed) || typed.Code != model.CodeResourceLimit {
							t.Fatalf("%s behind a busy gate returned %v, want a %s error",
								c.name, err, model.CodeResourceLimit)
						}
					case <-time.After(10 * time.Second):
						t.Fatalf("%s is still waiting for a gate slot 10s into a %s query timeout: the deadline does not wrap the gate wait",
							c.name, timeout)
					}
				}
			},
		},

		// T20-L5 OVERVIEW rows
		{
			// Protects the silent-omission failure mode: the repository map's
			// per-container aggregates are read through a keyset loop over
			// containment edges, and a loop that stops at the first clamped
			// page -- or at a flat page ceiling -- reports a package with 260
			// members as a package with 200. A repo-map that omits members is
			// not read as incomplete; it is read as the repository's shape.
			name: "overview/a container past the storage page clamp is rolled up completely",
			run: func(t *testing.T, f *graphFixture) {
				e, err := New(Options{Adjacency: f, Limits: fixtureLimits()})
				if err != nil {
					t.Fatalf("New: %v", err)
				}
				got, err := e.Overview(context.Background(), model.OverviewRequest{
					GenerationID: f.binding.GenerationID})
				if err != nil {
					t.Fatalf("Overview: %v", err)
				}
				if err := got.Validate(); err != nil {
					t.Fatalf("result does not satisfy its own contract: %v", err)
				}
				if got.Meta.Truncated {
					t.Fatalf("a map the fixture fits inside every bound reports truncated: %q",
						got.Meta.TruncationReason)
				}
				byID := map[model.NodeID]model.OverviewItem{}
				for _, item := range got.Items {
					byID[item.NodeID] = item
				}
				wide, ok := byID[fixtureNodeID("pkg-wide-b")]
				if !ok {
					t.Fatalf("pkg-wide-b is absent from a map of %d containers", len(got.Items))
				}
				if wide.SymbolCount != fixtureWideCount {
					t.Fatalf("pkg-wide-b holds %d symbols, want %d: the containment read stopped early",
						wide.SymbolCount, fixtureWideCount)
				}
				app, ok := byID[fixtureNodeID("pkg-app")]
				if !ok {
					t.Fatalf("pkg-app is absent from the map")
				}
				// pkg-app contains n-a, n-b, n-hub and n-p, DEFINES the
				// top-level n-d, and imports pkg-lib: an aggregate that counted
				// the import would report structure the container does not
				// hold, and one that read `contains` alone would drop n-d --
				// which is every top-level declaration of a real repository,
				// and so the difference between a measured symbol column and a
				// confident zero.
				if app.SymbolCount != 5 || app.FileCount != 0 {
					t.Fatalf("pkg-app holds %d symbols and %d files, want 5 and 0",
						app.SymbolCount, app.FileCount)
				}
				// The other half of the same failure mode: when the edge budget
				// cannot cover one page's children, the counts that WERE
				// accumulated are short by an unknown amount, so the map must
				// refuse rather than hand back numbers no caller can tell from
				// measured ones.
				starved := fixtureLimits()
				starved.MaxEdges = 1
				se, err := New(Options{Adjacency: f, Limits: starved})
				if err != nil {
					t.Fatalf("New: %v", err)
				}
				page, err := se.Overview(context.Background(), model.OverviewRequest{
					GenerationID: f.binding.GenerationID})
				var typed *model.Error
				if !errors.As(err, &typed) || typed.Code != model.CodeResourceLimit {
					t.Fatalf("a map whose containment budget is spent returned (%+v, %v), want a %s refusal",
						page.Items, err, model.CodeResourceLimit)
				}
			},
		},
		{
			// Protects the straddled-generation failure mode: every page of one
			// map must be bound to the generation page 1 pinned, and page 2 must
			// start where the cursor said and nowhere else. A continuation that
			// re-pinned, or that restarted the keyset, would splice two
			// repositories into one map without saying so.
			name: "overview/page 2 keeps page 1's generation and starts at its keyset",
			run: func(t *testing.T, f *graphFixture) {
				signer, err := pagination.OpenSigner(t.TempDir())
				if err != nil {
					t.Fatalf("open signer: %v", err)
				}
				limits := fixtureLimits()
				e, err := New(Options{Adjacency: f, Signer: signer,
					Leases: pagination.NewLeases(newFixtureLeases(), limits.CursorTTL), Limits: limits})
				if err != nil {
					t.Fatalf("New: %v", err)
				}
				const perPage = 2
				first, err := e.Overview(context.Background(), model.OverviewRequest{
					GenerationID: f.binding.GenerationID,
					Page:         model.PageRequest{Limit: perPage}})
				if err != nil {
					t.Fatalf("page 1: %v", err)
				}
				if len(first.Items) != perPage || first.Meta.NextCursor == "" {
					t.Fatalf("page 1 returned %d items and cursor %q, want %d items and a continuation",
						len(first.Items), first.Meta.NextCursor, perPage)
				}
				second, err := e.Overview(context.Background(), model.OverviewRequest{
					Page: model.PageRequest{Limit: perPage, Cursor: first.Meta.NextCursor}})
				if err != nil {
					t.Fatalf("page 2: %v", err)
				}
				if err := second.Validate(); err != nil {
					t.Fatalf("page 2 does not satisfy its own contract: %v", err)
				}
				if second.Meta.Binding != first.Meta.Binding {
					t.Fatalf("page 2 is bound to %+v, page 1 to %+v: the map straddles a generation change",
						second.Meta.Binding, first.Meta.Binding)
				}
				last := first.Items[len(first.Items)-1].NodeID
				for _, item := range second.Items {
					if item.NodeID <= last {
						t.Fatalf("page 2 lists %s at or before page 1's last container %s: the keyset restarted",
							item.NodeID, last)
					}
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

// unreadableAdjacency answers every fact read with a failure while still
// reporting the binding and the retention lease a continuation is bound to. An
// answer served over it read nothing from the graph, which is how the impact
// paging case observes that a continuation replays its spool instead of
// walking again.
type unreadableAdjacency struct{ *graphFixture }

func (unreadableAdjacency) Edges(context.Context, []model.NodeID, model.Direction,
	[]model.RelationKind, model.RelationID, int) ([]model.Relation, error) {
	return nil, errUnreadableAdjacency
}

func (unreadableAdjacency) NodesByID(context.Context, []model.NodeID) ([]model.Node, error) {
	return nil, errUnreadableAdjacency
}

func (unreadableAdjacency) EvidenceFor(context.Context, []model.RelationID, int) (map[model.RelationID][]model.EvidenceID, error) {
	return nil, errUnreadableAdjacency
}

func (unreadableAdjacency) Capabilities(context.Context) ([]model.CapabilityState, error) {
	return nil, errUnreadableAdjacency
}

var errUnreadableAdjacency = errors.New("this adjacency answers no read")

// blockingGate is the process gate with every slot permanently busy: Acquire
// waits for the caller's deadline and reports it as the same CTX_RESOURCE_LIMIT
// the shipped graphGate reports (internal/app/query.go). A context carrying no
// deadline therefore waits here forever, which is exactly the hang an engine
// entry that acquires outside its deadline would inflict on `codectx refs`.
// Release is unreachable -- Acquire never returns nil -- and exists only to
// satisfy the graph.Gate interface.
type blockingGate struct{}

func (blockingGate) Acquire(ctx context.Context) error {
	<-ctx.Done()
	return &model.Error{Code: model.CodeResourceLimit, Retryable: true,
		Message:     "every graph query slot was busy for the whole request deadline",
		Remediation: "retry when fewer queries are running, or raise resources.max_concurrent_graph_queries"}
}

func (blockingGate) Release() {}
