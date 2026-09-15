package graph

import (
	"context"
	"testing"
	"time"

	"github.com/Sawmonabo/codectx/internal/model"
)

// TestRankedImpactWithoutContinuationMachinerySaysItIsCut is the anti-silent-
// truncation proof for the ranked page. An engine built without a signer, a
// lease store or a spool store can rank the whole walk but cannot hand back a
// token for the remainder, so the page it serves is a PREFIX of the ranking.
// It used to be served with Truncated=false and no cursor, which reads as a
// complete answer and is the one thing a truncated answer may never do.
//
// Mutation (restore the bare `return "", nil` in canContinue's nil-guard,
// dropping the markTruncated): this FAILS on the untruncated answer below.
func TestRankedImpactWithoutContinuationMachinerySaysItIsCut(t *testing.T) {
	// A walk far wider than one page, so the ranking always has a remainder.
	f := newReachableFixture(t, 8, 40)
	limits := fixtureLimits()
	limits.MaxDepth, limits.MaxVisited, limits.MaxEdges = 0, 0, 0
	limits.MaxPageItems = 20
	limits.QueryTimeout = time.Minute
	// No Signer, no Spools, no Leases: the workspace offers no continuations.
	e, err := New(Options{Adjacency: f, Reader: memGraphFor(f), Limits: limits})
	if err != nil {
		t.Fatalf("new engine: %v", err)
	}

	res, err := e.Impact(context.Background(), model.ImpactRequest{
		GenerationID: 1, Start: []model.NodeID{fixtureNodeID("bound-seed")},
		Direction: model.DirectionOutgoing,
		Relations: []model.RelationKind{model.RelCalls}})
	if err != nil {
		t.Fatalf("impact: %v", err)
	}
	if res.Meta.NextCursor != "" {
		t.Fatal("an engine with no continuation machinery cannot mint a cursor")
	}
	if len(res.Entries) != limits.MaxPageItems {
		t.Fatalf("the fixture must overflow one page; the answer served %d of %d",
			len(res.Entries), limits.MaxPageItems)
	}
	if !res.Meta.Truncated {
		t.Fatal("a ranked answer cut to one page with no continuation must be marked truncated")
	}
	if res.Meta.TruncationReason != reasonNoContinuation {
		t.Fatalf("the cut must name the continuation machinery, got %q", res.Meta.TruncationReason)
	}
}
