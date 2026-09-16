// Package graphtest carries the graph.GraphReader conformance suite and the
// fixture it runs on, so every implementation of the port -- the in-heap
// graph.MemoryGraph and the store's packed per-generation reader -- is held to
// one behaviour (ADR-0005).
//
// It is a separate package, like providertest, so the testing import never
// reaches the production graph package.
package graphtest

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/Sawmonabo/codectx/internal/graph"
	"github.com/Sawmonabo/codectx/internal/model"
)

// FixtureRepository is the repository every fixture id is derived from. The
// ids are content-derived through the real constructors, so a store that
// ingests Nodes and Relations computes exactly these ids.
const FixtureRepository = model.RepositoryID(
	"00000000000000000000000000000000000000000000000000000000000000a1")

func nodeID(kind model.NodeKind, key string) model.NodeID {
	return model.NewNodeID(FixtureRepository, kind, NodeKey(key))
}

// NodeKey is the canonical key one fixture id derives from. A node identity is
// derived from a 32-byte canonical key, never from free text -- model.NodeFact
// refuses a key that is not a digest -- so an implementation backed by a store
// must publish this key to compute the ids the suite resolves. It is exported
// for exactly that: the store's fixture writes NodeKey("pkg/a") as the
// canonical key of PkgA and the store then derives PkgA itself.
func NodeKey(key string) string { return model.CanonicalNodeKey(key, "") }

// The fixture's node identities. They are exported because an implementation
// backed by a store must build its generation from the same facts and then
// resolve these ids to its OWN surrogates: surrogate values are not part of
// the port's contract and the two implementations do not agree on them.
var (
	// PkgA and PkgB both claim Hub through a contains edge; the container rule
	// picks the lower canonical id, so which one wins is a fact of the ids.
	PkgA = nodeID(model.NodePackage, "pkg/a")
	PkgB = nodeID(model.NodePackage, "pkg/b")
	// DirD also claims Hub, but a directory is not a rollup container and must
	// never win the slot.
	DirD = nodeID(model.NodeDirectory, "dir/d")
	// Hub has edges of several kinds in both directions.
	Hub = nodeID(model.NodeFunction, "pkg/a#hub")
	// Leaf is reached from Hub twice, by two kinds.
	Leaf = nodeID(model.NodeFunction, "pkg/b#leaf")
	// Orphan has no edge at all.
	Orphan = nodeID(model.NodeVariable, "pkg/a#orphan")
	// FileF carries a source size in its metadata.
	FileF = nodeID(model.NodeFile, "pkg/a/f.go")
	// ModM is the container-kind node published FOR FixtureFile, so it is the
	// container every node in that file falls to when nothing claims it
	// directly.
	ModM = nodeID(model.NodeModule, "mod/m")
	// TopT is a top-level declaration: its module attaches it with `defines`
	// and nothing `contains` it, which is the shape a real provider emits.
	TopT = nodeID(model.NodeFunction, "mod/m#top")
	// NestN is a nested declaration: its `contains` parent is TopT, which is
	// not a container kind and must never take the slot.
	NestN = nodeID(model.NodeFunction, "mod/m#top.nested")
	// Invisible is a well-formed id the fixture does not carry: Resolve must
	// report 0 for it rather than inventing a surrogate.
	Invisible = nodeID(model.NodeFunction, "pkg/z#absent")
)

// FileSourceBytes is the size FileF carries in its metadata.
const FileSourceBytes = int64(1234)

// FixtureFilePath is the repository path every file-bearing fixture node names,
// and FixtureFile is the identity derived from it. The derivation is
// deterministic, so an implementation backed by a store publishes the same file
// and computes the same id.
const FixtureFilePath = "pkg/a/f.go"

// FixtureFile is FixtureFilePath's identity.
var FixtureFile = model.NewFileID(FixtureRepository, FixtureFilePath)

// Nodes is the fixture's node set. Every node validates, so a store can ingest
// it unchanged.
func Nodes() []model.Node {
	return []model.Node{
		{ID: PkgA, Kind: model.NodePackage, Name: "a", QualifiedName: "pkg/a"},
		{ID: PkgB, Kind: model.NodePackage, Name: "b", QualifiedName: "pkg/b"},
		{ID: DirD, Kind: model.NodeDirectory, Name: "d", QualifiedName: "dir/d"},
		{ID: Hub, Kind: model.NodeFunction, Name: "hub", QualifiedName: "pkg/a#hub",
			FileID: FixtureFile},
		{ID: Leaf, Kind: model.NodeFunction, Name: "leaf", QualifiedName: "pkg/b#leaf"},
		{ID: Orphan, Kind: model.NodeVariable, Name: "orphan", QualifiedName: "pkg/a#orphan"},
		{ID: FileF, Kind: model.NodeFile, Name: "f.go", QualifiedName: "pkg/a/f.go",
			Metadata: []byte(`{"size":1234}`)},
		{ID: ModM, Kind: model.NodeModule, Name: "m", QualifiedName: "mod/m", FileID: FixtureFile},
		{ID: TopT, Kind: model.NodeFunction, Name: "top", QualifiedName: "mod/m#top",
			FileID: FixtureFile},
		{ID: NestN, Kind: model.NodeFunction, Name: "nested", QualifiedName: "mod/m#top.nested",
			FileID: FixtureFile},
	}
}

func relation(from model.NodeID, kind model.RelationKind, to model.NodeID) model.Relation {
	return model.Relation{ID: model.NewRelationID(FixtureRepository, from, kind, to), From: from, Kind: kind, To: to}
}

// Relations is the fixture's relation set. The last entry names a node the
// fixture does not carry, so every implementation must drop it: visibility is
// resolved once, at build time, and an invisible endpoint never reaches a walk.
func Relations() []model.Relation {
	return []model.Relation{
		relation(PkgA, model.RelContains, Hub),
		relation(PkgB, model.RelContains, Hub),
		relation(DirD, model.RelContains, Hub),
		relation(PkgA, model.RelContains, FileF),
		relation(Hub, model.RelCalls, Leaf),
		relation(Hub, model.RelReferences, Leaf),
		relation(Hub, model.RelImports, FileF),
		relation(Leaf, model.RelCalls, Hub),
		relation(ModM, model.RelDefines, TopT),
		relation(TopT, model.RelContains, NestN),
		relation(Hub, model.RelCalls, Invisible),
	}
}

// VisibleRelations is Relations without the one naming an absent node.
func VisibleRelations() []model.Relation {
	all := Relations()
	return all[:len(all)-1]
}

// hubContainer is the package that claims Hub by `contains`: PkgA and PkgB both
// do and DirD does too, so the lowest canonical id among the CONTAINER-KIND
// claimants wins and the attribution is a fact of the ids alone.
func hubContainer(refs map[model.NodeID]graph.NodeRef) graph.NodeRef {
	if PkgB < PkgA {
		return refs[PkgB]
	}
	return refs[PkgA]
}

// edge is one delivered entry written in canonical ids, which is the only
// vocabulary two implementations share.
type edge struct {
	Owner, Neighbour model.NodeID
	Rel              model.RelationID
	Kind             model.RelationKind
	Outgoing         bool
}

// RunConformance exercises every GraphReader method against the fixture. open
// must return a reader built from Nodes() and Relations(); the suite resolves
// every id it needs through the reader, so an implementation is free to assign
// surrogates however it likes.
func RunConformance(t *testing.T, open func(t *testing.T) graph.GraphReader) {
	t.Helper()
	ctx := context.Background()
	g := open(t)

	refs := resolveAll(t, g, []model.NodeID{PkgA, PkgB, DirD, Hub, Leaf, Orphan, FileF,
		ModM, TopT, NestN, Invisible})
	for id, ref := range refs {
		if id == Invisible {
			if ref != 0 {
				t.Fatalf("Resolve(%s) = %d, want 0 for an id the generation does not carry", id, ref)
			}
			continue
		}
		if ref == 0 {
			t.Fatalf("Resolve(%s) = 0, want a surrogate", id)
		}
		if ref > g.MaxNode() {
			t.Fatalf("Resolve(%s) = %d exceeds MaxNode %d, so a visited bitset sized from MaxNode would be short",
				id, ref, g.MaxNode())
		}
	}

	t.Run("kind dictionary round-trips", func(t *testing.T) {
		if _, ok := g.Kinds().Kind(0); ok {
			t.Fatal("Kinds().Kind(0) resolved; code 0 must be unused so a zero value is never a kind")
		}
		// The expectation is READ OFF the visible relations rather than
		// written out, so a fixture that grows a relation kind cannot leave the
		// dictionary asserting the old width.
		want := map[model.RelationKind]bool{}
		for _, r := range VisibleRelations() {
			want[r.Kind] = true
		}
		for k := range want {
			code, ok := g.Kinds().Code(k)
			if !ok || code == 0 {
				t.Fatalf("Kinds().Code(%s) = %d, %v; want a dense non-zero code", k, code, ok)
			}
			back, ok := g.Kinds().Kind(code)
			if !ok || back != k {
				t.Fatalf("Kinds().Kind(%d) = %s, %v; want %s", code, back, ok, k)
			}
		}
		if n := g.Kinds().Len(); n != len(want) {
			t.Fatalf("Kinds().Len() = %d, want %d (the kinds the visible relations use)", n, len(want))
		}
	})

	t.Run("surrogates map back to canonical ids", func(t *testing.T) {
		want := []model.NodeID{Hub, Leaf, PkgA}
		got, err := g.NodeIDs(ctx, []graph.NodeRef{refs[Hub], refs[Leaf], refs[PkgA]})
		if err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(got, want) {
			t.Fatalf("NodeIDs = %v, want %v aligned with the refs asked", got, want)
		}
	})

	t.Run("outgoing list is ascending and complete", func(t *testing.T) {
		got := scan(t, g, []graph.NodeRef{refs[Hub]}, model.DirectionOutgoing, nil)
		want := expected(Hub, true, nil)
		assertEdges(t, got, want)
		assertAscending(t, g, ctx, []graph.NodeRef{refs[Hub]}, model.DirectionOutgoing, nil)
	})

	t.Run("an invisible relation is absent", func(t *testing.T) {
		got := scan(t, g, []graph.NodeRef{refs[Hub]}, model.DirectionOutgoing, nil)
		for _, e := range got {
			if e.Neighbour == Invisible || e.Neighbour == "" {
				t.Fatalf("Neighbours delivered %+v: a relation naming a node the generation does not "+
					"carry must be dropped at build time, not surfaced to a walk", e)
			}
		}
	})

	t.Run("kind filter selects stored entries", func(t *testing.T) {
		code, _ := g.Kinds().Code(model.RelCalls)
		got := scan(t, g, []graph.NodeRef{refs[Hub]}, model.DirectionOutgoing, []graph.KindCode{code})
		assertEdges(t, got, expected(Hub, true, []model.RelationKind{model.RelCalls}))
	})

	t.Run("both visits outgoing then incoming per owner", func(t *testing.T) {
		// Asserted STRUCTURALLY, on the surrogates the reader delivered, not
		// against a precomputed order: the port sorts each list by neighbour
		// SURROGATE, and an implementation is free to assign surrogates in an
		// order that is not the canonical-id order, so a canonical-id oracle
		// would fail a correct reader.
		raw := scanRaw(t, g, []graph.NodeRef{refs[Hub]}, model.DirectionBoth, nil)
		split := len(raw)
		for i, e := range raw {
			if !e.Outgoing {
				split = i
				break
			}
		}
		for _, e := range raw[split:] {
			if e.Outgoing {
				t.Fatalf("an outgoing entry follows an incoming one: DirectionBoth must visit the "+
					"owner's whole outgoing list and only then its incoming one, got %+v", raw)
			}
		}
		assertBlockAscends(t, raw[:split])
		assertBlockAscends(t, raw[split:])
		assertEdges(t, canonicalAll(t, g, raw),
			append(expected(Hub, true, nil), expected(Hub, false, nil)...))
	})

	t.Run("several owners are visited in ref order", func(t *testing.T) {
		owners := []graph.NodeRef{refs[Hub], refs[Leaf]}
		slices.Sort(owners)
		raw := scanRaw(t, g, owners, model.DirectionOutgoing, nil)
		var prev graph.NodeRef
		for _, e := range raw {
			if e.Owner < prev {
				t.Fatalf("owner %d delivered after %d; owners are visited in ascending ref order so "+
					"the offset and edge parts are touched sequentially", e.Owner, prev)
			}
			prev = e.Owner
		}
		assertEdges(t, canonicalAll(t, g, raw),
			append(expected(Hub, true, nil), expected(Leaf, true, nil)...))
	})

	t.Run("a position is stable whatever kinds a later scan asks for", func(t *testing.T) {
		// EdgePos.Index counts STORED entries, before kind filtering. A reader
		// that counts DELIVERED entries instead reports a filtered index, and
		// the next page -- which the walk may run with different kind codes --
		// resumes in the wrong place: entries repeat or vanish. This is the
		// clause the cursor payload depends on, so it is tested directly.
		full := scan(t, g, []graph.NodeRef{refs[Hub]}, model.DirectionOutgoing, nil)
		if len(full) < 2 {
			t.Fatalf("fixture degenerated: hub has %d outgoing entries", len(full))
		}
		last := full[len(full)-1]
		for _, e := range full[:len(full)-1] {
			if e.Kind == last.Kind {
				t.Fatalf("fixture degenerated: kind %s is not unique in hub's outgoing list", last.Kind)
			}
		}
		code, ok := g.Kinds().Code(last.Kind)
		if !ok {
			t.Fatalf("Kinds().Code(%s) missing", last.Kind)
		}
		pos, err := g.Neighbours(ctx, []graph.NodeRef{refs[Hub]}, model.DirectionOutgoing,
			[]graph.KindCode{code}, graph.EdgePos{}, func(graph.Edge) error { return graph.ErrStopScan })
		if err != nil {
			t.Fatal(err)
		}
		var rest []edge
		if _, err := g.Neighbours(ctx, []graph.NodeRef{refs[Hub]}, model.DirectionOutgoing, nil, pos,
			func(e graph.Edge) error { rest = append(rest, canonical(t, g, e)); return nil }); err != nil {
			t.Fatal(err)
		}
		// The stopped entry is the LAST stored entry, so an unfiltered resume
		// from its position delivers exactly it.
		assertEdgesOrdered(t, rest, full[len(full)-1:])
	})

	t.Run("a scan the context ends resumes past what it delivered", func(t *testing.T) {
		// The port's OWN deadline check ends this scan, not the callback: the
		// callback returns nil throughout and the context is cancelled under it.
		// The position a reader reports there must be a boundary past the last
		// DELIVERED entry -- report the delivered entry's own index instead and
		// the resume hands that entry to the caller a second time, which a walk
		// turns into an entity served twice.
		//
		// Where the cancellation is noticed is a reader's own business: the
		// in-heap reader checks between owners, a reader that decodes from
		// storage may notice mid-list. The invariant is the same either way and
		// is asserted as one: what was delivered, followed by what the resume
		// delivers, is exactly the uninterrupted scan.
		owners := []graph.NodeRef{refs[Hub], refs[Leaf]}
		slices.Sort(owners)
		full := scan(t, g, owners, model.DirectionBoth, nil)

		cancelCtx, cancel := context.WithCancel(context.Background())
		var delivered []edge
		pos, err := g.Neighbours(cancelCtx, owners, model.DirectionBoth, nil, graph.EdgePos{},
			func(e graph.Edge) error {
				delivered = append(delivered, canonical(t, g, e))
				cancel()
				return nil
			})
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("a scan cancelled under the callback returned %v, want context.Canceled; "+
				"this case proves nothing unless the reader itself ends the scan", err)
		}
		if len(delivered) == 0 || len(delivered) >= len(full) {
			t.Fatalf("fixture degenerated: the cancelled scan delivered %d of %d entries; "+
				"it must deliver some and leave some", len(delivered), len(full))
		}

		var rest []edge
		if _, err := g.Neighbours(ctx, owners, model.DirectionBoth, nil, pos,
			func(e graph.Edge) error { rest = append(rest, canonical(t, g, e)); return nil }); err != nil {
			t.Fatal(err)
		}
		assertEdgesOrdered(t, append(append([]edge(nil), delivered...), rest...), full)
	})

	t.Run("a node with no edges yields none", func(t *testing.T) {
		got := scan(t, g, []graph.NodeRef{refs[Orphan]}, model.DirectionBoth, nil)
		if len(got) != 0 {
			t.Fatalf("Neighbours(orphan) = %v, want none", got)
		}
	})

	t.Run("a stopped scan resumes at the entry it did not deliver", func(t *testing.T) {
		owners := []graph.NodeRef{refs[Hub], refs[Leaf]}
		slices.Sort(owners)
		full := scan(t, g, owners, model.DirectionBoth, nil)
		for stopAfter := 0; stopAfter < len(full); stopAfter++ {
			seen := 0
			var first []edge
			pos, err := g.Neighbours(ctx, owners, model.DirectionBoth, nil, graph.EdgePos{},
				func(e graph.Edge) error {
					if seen == stopAfter {
						return graph.ErrStopScan
					}
					seen++
					first = append(first, canonical(t, g, e))
					return nil
				})
			if err != nil {
				t.Fatalf("stopAfter=%d: %v", stopAfter, err)
			}
			var rest []edge
			if _, err := g.Neighbours(ctx, owners, model.DirectionBoth, nil, pos,
				func(e graph.Edge) error {
					rest = append(rest, canonical(t, g, e))
					return nil
				}); err != nil {
				t.Fatalf("stopAfter=%d resume: %v", stopAfter, err)
			}
			// Exactly the whole scan, once: a resume that re-delivers an entry
			// reports an entity twice across pages, and one that skips an entry
			// silently truncates the answer.
			assertEdgesOrdered(t, append(first, rest...), full)
		}
	})

	t.Run("a completed scan resumes with nothing left", func(t *testing.T) {
		owners := []graph.NodeRef{refs[Hub]}
		pos, err := g.Neighbours(ctx, owners, model.DirectionBoth, nil, graph.EdgePos{},
			func(graph.Edge) error { return nil })
		if err != nil {
			t.Fatal(err)
		}
		var rest []edge
		if _, err := g.Neighbours(ctx, owners, model.DirectionBoth, nil, pos,
			func(e graph.Edge) error { rest = append(rest, canonical(t, g, e)); return nil }); err != nil {
			t.Fatal(err)
		}
		if len(rest) != 0 {
			t.Fatalf("resuming a completed scan delivered %v, want nothing", rest)
		}
	})

	t.Run("a callback error surfaces", func(t *testing.T) {
		sentinel := errors.New("boom")
		if _, err := g.Neighbours(ctx, []graph.NodeRef{refs[Hub]}, model.DirectionOutgoing, nil,
			graph.EdgePos{}, func(graph.Edge) error { return sentinel }); !errors.Is(err, sentinel) {
			t.Fatalf("Neighbours error = %v, want the callback's own error", err)
		}
	})

	t.Run("side arrays align with refs", func(t *testing.T) {
		ask := []graph.NodeRef{refs[Hub], refs[PkgA], refs[DirD], refs[FileF], refs[Orphan]}
		kinds, err := g.NodeKinds(ctx, ask)
		if err != nil {
			t.Fatal(err)
		}
		wantKinds := []model.NodeKind{model.NodeFunction, model.NodePackage, model.NodeDirectory,
			model.NodeFile, model.NodeVariable}
		if !slices.Equal(kinds, wantKinds) {
			t.Fatalf("NodeKinds = %v, want %v", kinds, wantKinds)
		}

		bytes, err := g.SourceBytes(ctx, ask)
		if err != nil {
			t.Fatal(err)
		}
		wantBytes := []int64{0, 0, 0, FileSourceBytes, 0}
		if !slices.Equal(bytes, wantBytes) {
			t.Fatalf("SourceBytes = %v, want %v (only a file node carries a size)", bytes, wantBytes)
		}

		containers, err := g.Containers(ctx, ask)
		if err != nil {
			t.Fatal(err)
		}
		// PkgA and PkgB both contain Hub and DirD also does; the lowest
		// canonical id among the CONTAINER-KIND claimants wins, so a directory
		// never takes the slot and the attribution is a fact of the ids.
		wantContainers := []graph.NodeRef{hubContainer(refs), refs[PkgA], 0, refs[PkgA], 0}
		if !slices.Equal(containers, wantContainers) {
			got, _ := g.NodeIDs(ctx, containers)
			t.Fatalf("Containers = %v (%v), want %v", containers, got, wantContainers)
		}
	})

	t.Run("the container slot follows the node's file", func(t *testing.T) {
		// The settled rule: a node belongs to the container-kind node that owns
		// its own FILE, and a container-kind node that claims it directly by
		// `contains` overrides that. Read over `contains` alone the slot is
		// empty on a real repository -- a provider attaches a TOP-LEVEL
		// declaration to its module with `defines` and reserves `contains` for
		// a NESTED one, whose parent is the enclosing declaration.
		ask := []graph.NodeRef{refs[ModM], refs[TopT], refs[NestN], refs[Hub], refs[DirD]}
		containers, err := g.Containers(ctx, ask)
		if err != nil {
			t.Fatal(err)
		}
		want := []graph.NodeRef{
			// A container is its own container.
			refs[ModM],
			// Top-level: only `defines` reaches it, so the file's module is
			// what answers.
			refs[ModM],
			// Nested: its `contains` parent is a function, which is no
			// container, so the file's module answers here too.
			refs[ModM],
			// Hub shares that file, but a package claims it directly by
			// `contains`, and the direct claim overrides the file's.
			hubContainer(refs),
			// A directory has no file and no container-kind claimant.
			0,
		}
		if !slices.Equal(containers, want) {
			got, _ := g.NodeIDs(ctx, containers)
			t.Fatalf("Containers = %v (%v), want %v", containers, got, want)
		}
	})

	t.Run("evidence counts align with relation refs", func(t *testing.T) {
		var rels []graph.RelRef
		if _, err := g.Neighbours(ctx, []graph.NodeRef{refs[Hub]}, model.DirectionOutgoing, nil,
			graph.EdgePos{}, func(e graph.Edge) error { rels = append(rels, e.Rel); return nil }); err != nil {
			t.Fatal(err)
		}
		counts, err := g.EvidenceCounts(ctx, append(rels, 0))
		if err != nil {
			t.Fatal(err)
		}
		if len(counts) != len(rels)+1 {
			t.Fatalf("EvidenceCounts returned %d values for %d refs", len(counts), len(rels)+1)
		}
		for i, c := range counts[:len(rels)] {
			if c < 1 {
				t.Fatalf("EvidenceCounts[%d] = %d; a visible relation is backed by at least one occurrence", i, c)
			}
		}
		if counts[len(rels)] != 0 {
			t.Fatalf("EvidenceCounts(0) = %d, want 0 for the never-a-relation surrogate", counts[len(rels)])
		}
	})

	t.Run("delivery reads answer", func(t *testing.T) {
		nodes, err := g.NodesByID(ctx, []model.NodeID{Hub, Invisible})
		if err != nil {
			t.Fatal(err)
		}
		if len(nodes) != 1 || nodes[0].ID != Hub {
			t.Fatalf("NodesByID = %v, want only the visible hub", nodes)
		}
		rel := model.NewRelationID(FixtureRepository, Hub, model.RelCalls, Leaf)
		if _, err := g.EvidenceFor(ctx, []model.RelationID{rel}, 4); err != nil {
			t.Fatal(err)
		}
		if _, err := g.Capabilities(ctx); err != nil {
			t.Fatal(err)
		}
		if err := g.Binding().Validate(); err != nil {
			t.Fatalf("Binding is not a pinned generation: %v", err)
		}
	})

	t.Run("unsorted refs are refused", func(t *testing.T) {
		bad := []graph.NodeRef{refs[Hub], refs[Hub]}
		if _, err := g.Neighbours(ctx, bad, model.DirectionOutgoing, nil, graph.EdgePos{},
			func(graph.Edge) error { return nil }); err == nil {
			t.Fatal("Neighbours accepted repeated refs; a duplicate owner delivers its list twice")
		}
	})
}

func resolveAll(t *testing.T, g graph.GraphReader, ids []model.NodeID) map[model.NodeID]graph.NodeRef {
	t.Helper()
	got, err := g.Resolve(context.Background(), ids)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(ids) {
		t.Fatalf("Resolve returned %d refs for %d ids; results must align", len(got), len(ids))
	}
	out := make(map[model.NodeID]graph.NodeRef, len(ids))
	for i, id := range ids {
		out[id] = got[i]
	}
	return out
}

func scan(t *testing.T, g graph.GraphReader, refs []graph.NodeRef, dir model.Direction,
	kinds []graph.KindCode) []edge {
	t.Helper()
	return canonicalAll(t, g, scanRaw(t, g, refs, dir, kinds))
}

// scanRaw keeps the delivered entries as the reader produced them, surrogates
// and all, for the assertions that are about ORDER: order is a property of the
// surrogates, which no two implementations need agree on.
func scanRaw(t *testing.T, g graph.GraphReader, refs []graph.NodeRef, dir model.Direction,
	kinds []graph.KindCode) []graph.Edge {
	t.Helper()
	var out []graph.Edge
	if _, err := g.Neighbours(context.Background(), refs, dir, kinds, graph.EdgePos{},
		func(e graph.Edge) error { out = append(out, e); return nil }); err != nil {
		t.Fatal(err)
	}
	return out
}

func canonicalAll(t *testing.T, g graph.GraphReader, raw []graph.Edge) []edge {
	t.Helper()
	out := make([]edge, 0, len(raw))
	for _, e := range raw {
		out = append(out, canonical(t, g, e))
	}
	return out
}

// assertBlockAscends checks one owner-and-direction block: neighbour
// surrogates never go backwards inside it. It is deliberately per BLOCK,
// because DirectionBoth concatenates two lists and the join between them is
// the one place the sequence legitimately drops back.
func assertBlockAscends(t *testing.T, block []graph.Edge) {
	t.Helper()
	var prev graph.NodeRef
	for _, e := range block {
		if e.Neighbour < prev {
			t.Fatalf("neighbour %d delivered after %d inside one list; a list must ascend",
				e.Neighbour, prev)
		}
		prev = e.Neighbour
	}
}

// canonical rewrites a delivered entry into canonical ids so two
// implementations with different surrogate assignments can be compared.
func canonical(t *testing.T, g graph.GraphReader, e graph.Edge) edge {
	t.Helper()
	ctx := context.Background()
	nodes, err := g.NodeIDs(ctx, []graph.NodeRef{e.Owner, e.Neighbour})
	if err != nil {
		t.Fatal(err)
	}
	rels, err := g.RelationIDs(ctx, []graph.RelRef{e.Rel})
	if err != nil {
		t.Fatal(err)
	}
	kind, ok := g.Kinds().Kind(e.Kind)
	if !ok {
		t.Fatalf("edge %+v carries kind code %d, which the generation's dictionary does not know", e, e.Kind)
	}
	return edge{Owner: nodes[0], Neighbour: nodes[1], Rel: rels[0], Kind: kind, Outgoing: e.Outgoing}
}

// expected is the fixture's own answer for one owner and direction, in the
// order the port promises: ascending neighbour, ties broken by relation id.
func expected(owner model.NodeID, outgoing bool, kinds []model.RelationKind) []edge {
	var out []edge
	for _, r := range VisibleRelations() {
		if len(kinds) > 0 && !slices.Contains(kinds, r.Kind) {
			continue
		}
		switch {
		case outgoing && r.From == owner:
			out = append(out, edge{Owner: owner, Neighbour: r.To, Rel: r.ID, Kind: r.Kind, Outgoing: true})
		case !outgoing && r.To == owner:
			out = append(out, edge{Owner: owner, Neighbour: r.From, Rel: r.ID, Kind: r.Kind, Outgoing: false})
		}
	}
	slices.SortFunc(out, func(a, b edge) int {
		if a.Neighbour != b.Neighbour {
			if a.Neighbour < b.Neighbour {
				return -1
			}
			return 1
		}
		if a.Rel == b.Rel {
			return 0
		}
		if a.Rel < b.Rel {
			return -1
		}
		return 1
	})
	return out
}

// assertEdges compares as sets, for a case whose order is asserted separately.
func assertEdges(t *testing.T, got, want []edge) {
	t.Helper()
	g := append([]edge{}, got...)
	w := append([]edge{}, want...)
	slices.SortFunc(g, compareEdge)
	slices.SortFunc(w, compareEdge)
	if !slices.Equal(g, w) {
		t.Fatalf("edges = %+v, want %+v", g, w)
	}
}

func assertEdgesOrdered(t *testing.T, got, want []edge) {
	t.Helper()
	if !slices.Equal(got, want) {
		t.Fatalf("edges = %+v, want %+v in this exact order", got, want)
	}
}

func compareEdge(a, b edge) int {
	switch {
	case a.Owner != b.Owner:
		if a.Owner < b.Owner {
			return -1
		}
		return 1
	case a.Rel != b.Rel:
		if a.Rel < b.Rel {
			return -1
		}
		return 1
	case a.Outgoing != b.Outgoing:
		if a.Outgoing {
			return -1
		}
		return 1
	}
	return 0
}

// assertAscending checks the port's central promise directly on the surrogates
// the reader delivered: within one owner's list the neighbour surrogates never
// go backwards, which is what lets a walk touch offset and edge parts
// sequentially instead of seeking.
//
// It resets per OWNER, so it is valid for a single-direction scan only. Use
// assertBlockAscends for DirectionBoth, whose two concatenated lists each
// ascend on their own.
func assertAscending(t *testing.T, g graph.GraphReader, ctx context.Context, refs []graph.NodeRef,
	dir model.Direction, kinds []graph.KindCode) {
	t.Helper()
	var owner graph.NodeRef
	var prev graph.NodeRef
	if _, err := g.Neighbours(ctx, refs, dir, kinds, graph.EdgePos{}, func(e graph.Edge) error {
		if e.Owner != owner {
			owner, prev = e.Owner, 0
		}
		if e.Neighbour < prev {
			t.Fatalf("owner %d delivered neighbour %d after %d; the list must ascend", owner, e.Neighbour, prev)
		}
		prev = e.Neighbour
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
