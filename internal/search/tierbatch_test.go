package search

import (
	"context"
	"fmt"
	"testing"

	"github.com/Sawmonabo/codectx/internal/model"
	"github.com/Sawmonabo/codectx/internal/search/textindex"
	"github.com/Sawmonabo/codectx/internal/storage/sqlite"
)

// countingExact counts the symbol-tier reads a walk makes through it: nodes
// counts keyset reads of a tier (a Nodes page or a WalkTier page), among the
// batched lookups, and probes the one-node reads that ask whether a tier lists
// any node at all.
type countingExact struct {
	exactReader
	nodes, among, probes int
}

func (c *countingExact) WalkTier(ctx context.Context, f sqlite.NodeFilter, from *sqlite.TierSpan, limit int,
	yield func([]sqlite.StoredNode) (int, error)) error {
	if limit == 1 {
		c.probes++
		return c.exactReader.WalkTier(ctx, f, from, limit, yield)
	}
	return c.exactReader.WalkTier(ctx, f, from, limit, func(nodes []sqlite.StoredNode) (int, error) {
		c.nodes++
		return yield(nodes)
	})
}

func (c *countingExact) Nodes(ctx context.Context, f sqlite.NodeFilter, after model.NodeID, limit int) ([]sqlite.StoredNode, error) {
	c.nodes++
	return c.exactReader.Nodes(ctx, f, after, limit)
}

func (c *countingExact) NodesAmong(ctx context.Context, f sqlite.NodeFilter, ids []model.NodeID) ([]sqlite.StoredNode, error) {
	c.among++
	return c.exactReader.NodesAmong(ctx, f, ids)
}

// TestTheFoldAsksAPageOfHitsTogether proves the lexical fold decides a page's
// hits in a few statements, not in point reads per hit: 300 hits whose nodes
// the qualified_name_prefix tier serves are all folded, with one lookup of the
// prefix tier per chunk of model.MaxPageItems ids, no keyset seek, and one
// existence probe each of the prefix and exact qualified-name tiers. The exact
// qualified-name tier lists no node for the query, so it is never asked about
// a chunk.
//
// Mutations: ask the tiers one node at a time (one NodesAmong, or one Nodes
// seek, per hit) and the count grows with the hits; ask a tier that lists
// nothing about every chunk (no existence probe) and the lookups double.
func TestTheFoldAsksAPageOfHitsTogether(t *testing.T) {
	const n = 300
	stub := &stubWideExact{n: n, file: model.FileID(model.H("wide-file")), path: "pkg/wide.go"}
	reads := &countingExact{exactReader: stub}
	rule, err := newSearchRule(context.Background(), reads, "Widget", nil, hitFilter{})
	if err != nil {
		t.Fatal(err)
	}
	hits := make([]textindex.Hit, n)
	for i := range hits {
		hits[i] = textindex.Hit{NodeID: stub.node(n - 1 - i).Node.ID}
	}
	served, err := servedExactly(context.Background(), rule, hits)
	if err != nil {
		t.Fatalf("servedExactly: %v", err)
	}
	for _, h := range hits {
		if !served[h.NodeID] {
			t.Fatalf("the hit of %s is not folded, though the prefix tier serves its node", h.NodeID)
		}
	}
	chunks := (n + model.MaxPageItems - 1) / model.MaxPageItems
	if reads.nodes != 0 || reads.among != chunks || reads.probes != 2 {
		t.Fatalf("folding %d hits made %d keyset seeks, %d lookups and %d probes; want 0, %d (one per %d ids) and 2",
			n, reads.nodes, reads.among, reads.probes, chunks, model.MaxPageItems)
	}
}

// TestAResumedExactPageReadsItsPositionBack proves a continuation inside a
// symbol tier reads the hit its position names in one lookup, not by walking
// the tier from its start to the node: the position is the last of 20 000
// prefix-tier nodes.
//
// Mutation: find the position by walking its tier (exactCandidates over the
// position's span) and the walk makes 100 keyset seeks of 200.
func TestAResumedExactPageReadsItsPositionBack(t *testing.T) {
	const n = 20_000
	stub := &stubWideExact{n: n, file: model.FileID(model.H("wide-file")), path: "pkg/wide.go"}
	reads := &countingExact{exactReader: stub}
	rule, err := newSearchRule(context.Background(), reads, "Widget", nil, hitFilter{})
	if err != nil {
		t.Fatal(err)
	}
	last := stub.node(n - 1)
	pos := searchPosition{phase: phaseExact, rank: model.TierQualifiedNamePrefix.Rank(), node: last.Node.ID}
	key, err := positionKey(context.Background(), reads, rule, hitFilter{}, pos)
	if err != nil {
		t.Fatalf("positionKey: %v", err)
	}
	if key.Node.Node.ID != last.Node.ID || key.Tier != model.TierQualifiedNamePrefix || key.Path != stub.path {
		t.Fatalf("positionKey = %s at %q in %q, want %s at %q in %q", key.Node.Node.ID, key.Tier, key.Path,
			last.Node.ID, model.TierQualifiedNamePrefix, stub.path)
	}
	if reads.nodes != 0 {
		t.Fatalf("reading the position back made %d keyset seeks of its tier; want none", reads.nodes)
	}
	// A position whose node its tier does not serve was not minted for this
	// answer: the exact_name tier never serves a node the prefix tier owns.
	pos.rank = model.TierExactName.Rank()
	if _, err := positionKey(context.Background(), reads, rule, hitFilter{}, pos); err == nil {
		t.Fatal("a position at a tier that does not own its node was accepted")
	} else {
		assertCode(t, "positionKey(owned elsewhere)", err, model.CodeCursorInvalid)
	}
}

// TestResolveServesANodeOnceAtItsEarliestTier proves the symbol endpoint decides
// a node's tier over all of the node's rows, as search does: "Twice" matches
// exact_qualified_name through its unverified row, and exact_name through its
// verified row, whose qualified name ("pkg.Twice") the query does not prefix.
//
// Mutation: decide on the row in hand (the qualified name of the exact_name
// row does not start with the query) and Resolve serves "Twice" a second time
// at exact_name.
func TestResolveServesANodeOnceAtItsEarliestTier(t *testing.T) {
	f := newRowsFixture(t, rowsDecls)
	s := newService(t, f.opts)
	for _, limit := range []int{1, 10} {
		req := model.SymbolRequest{Query: "Twice", Operation: model.SymbolResolve, SemanticSource: model.SemanticCanonical,
			Page: model.PageRequest{Limit: limit}}
		seen := map[model.NodeID]int{}
		for page := 1; ; page++ {
			if page > 20 {
				t.Fatalf("limit %d: the walk did not end", limit)
			}
			got, err := s.Resolve(f.ctx, req)
			if err != nil {
				t.Fatalf("limit %d: Resolve(page %d): %v", limit, page, err)
			}
			for _, n := range got.Items {
				seen[n.ID]++
			}
			if got.Meta.NextCursor == "" {
				break
			}
			req.Page.Cursor = got.Meta.NextCursor
		}
		want := map[model.NodeID]int{f.nodes["Twice"]: 1, f.nodes["Other"]: 1}
		if fmt.Sprint(seen) != fmt.Sprint(want) {
			t.Fatalf("limit %d: Resolve(Twice) served %v, want each of Twice and Other once %v", limit, seen, want)
		}
	}
}

// TestAnExactNameContinuationReadsItsOwnPage proves an equality tier is paged
// by seek: a continuation 10,000 hits into a 20,000-hit exact_name tier serves
// the 200 hits after its position, in order, reading the tier from the
// position (two storage pages: the page and the one hit past it) and sorting
// nothing for a spool.
//
// Mutation: answer the tier with a walk and a heap (heapTier) instead of a
// seek, and the page reads all 100 storage pages of the tier.
func TestAnExactNameContinuationReadsItsOwnPage(t *testing.T) {
	const n, page, at = 20_000, 200, 10_000
	stub := &stubWideExact{n: n, file: model.FileID(model.H("wide-file")), path: "pkg/wide.go", qualifier: "pkg."}
	reads := &countingExact{exactReader: stub}
	ctx := context.Background()
	rule, err := newSearchRule(ctx, reads, "Widget", nil, hitFilter{})
	if err != nil {
		t.Fatal(err)
	}
	s := &Service{}
	pos := searchPosition{phase: phaseExact, rank: model.TierExactName.Rank(), node: stub.node(at).Node.ID}
	var ans searchAnswer
	got, more, err := s.exactPage(ctx, reads, rule, hitFilter{}, pos, page, exactSource{sort: true}, &ans)
	if err != nil {
		t.Fatalf("exactPage: %v", err)
	}
	if len(got) != page || !more || ans.rest != nil {
		t.Fatalf("the page served %d hits (more %v, spool %v); want %d, more, and nothing to spool", len(got), more,
			ans.rest != nil, page)
	}
	for i, e := range got {
		if want := stub.node(at + 1 + i).Node.ID; e.Node.Node.ID != want || e.Tier != model.TierExactName {
			t.Fatalf("hit %d is %s at %s, want %s at exact_name", i, e.Node.Node.ID, e.Tier, want)
		}
	}
	if reads.nodes > 2 {
		t.Fatalf("the continuation read %d storage pages of the tier; want at most 2 (a seek)", reads.nodes)
	}
}
