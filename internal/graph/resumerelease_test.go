package graph

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Sawmonabo/codectx/internal/model"
	"github.com/Sawmonabo/codectx/internal/pagination"
)

// A RETRYABLE failure on a resumed traversal page must leave the continuation
// adoptable. `traverse` consumes the cursor's spool, its retained state and its
// lease through one deferred `resume.Release()`, and an unguarded release runs
// on the error return too: the caller is told CTX_WORKSPACE_BUSY -- an error
// whose whole remedy is "present this cursor again" -- and the retry is then
// refused as CTX_CURSOR_INVALID, so a long walk is unrecoverable after one
// contended edge read. The guard is `terminalOutcome`, the same one impact and
// the shortest path already apply.
//
// Mutation proof: drop the guard in traverse.go (`defer resume.Release()`) and
// this fails on the retry with
//
//	the retry of a busy page was refused: CTX_CURSOR_INVALID
type busyOnceAdjacency struct {
	Adjacency
	fail bool
}

func (b *busyOnceAdjacency) Edges(ctx context.Context, nodes []model.NodeID, direction model.Direction,
	kinds []model.RelationKind, after model.RelationID, limit int) ([]model.Relation, error) {
	if b.fail {
		b.fail = false
		return nil, &model.Error{Code: model.CodeWorkspaceBusy, Retryable: true,
			Message: "the adjacency store was contended for this read"}
	}
	return b.Adjacency.Edges(ctx, nodes, direction, kinds, after, limit)
}

func TestBusyResumedTraversalPageKeepsItsContinuation(t *testing.T) {
	f := newGraphFixture(t)
	adj := &busyOnceAdjacency{Adjacency: f}
	signer, err := pagination.OpenSigner(t.TempDir())
	if err != nil {
		t.Fatalf("open signer: %v", err)
	}
	store := newFixtureLeases()
	spools, err := pagination.NewSpools(t.TempDir(), 64<<20, store)
	if err != nil {
		t.Fatalf("new spools: %v", err)
	}
	limits := fixtureLimits()
	// One relation per page, so the walk is spread over many continuations and
	// the page under test is a RESUMED one.
	limits.MaxPageItems = 1
	limits.QueryTimeout = time.Minute
	e, err := New(Options{Adjacency: adj, Reader: memGraphFor(f), Signer: signer, Spools: spools,
		Leases: pagination.NewLeases(store, limits.CursorTTL), Limits: limits})
	if err != nil {
		t.Fatalf("new engine: %v", err)
	}

	req := model.GraphRequest{GenerationID: 1, Start: []model.NodeID{fixtureNodeID("n-a")},
		Direction: model.DirectionOutgoing, Relations: []model.RelationKind{model.RelCalls}}
	first, err := e.Neighbors(context.Background(), req)
	if err != nil {
		t.Fatalf("first page: %v", err)
	}
	if first.Meta.NextCursor == "" {
		t.Fatalf("the first page minted no continuation: this proof needs a RESUMED page")
	}

	// The resumed page fails one edge read, retryably.
	adj.fail = true
	req.Page, req.GenerationID = model.PageRequest{Cursor: first.Meta.NextCursor}, 0
	if _, err = e.Neighbors(context.Background(), req); err == nil {
		t.Fatalf("the contended resumed page succeeded: the failure was never injected")
	}
	var typed *model.Error
	if !errors.As(err, &typed) || typed.Code != model.CodeWorkspaceBusy || !typed.Retryable {
		t.Fatalf("the contended page reported %v, not a retryable CTX_WORKSPACE_BUSY", err)
	}

	// The retry the error invites: the SAME cursor, against a healthy store.
	retry, err := e.Neighbors(context.Background(), req)
	if err != nil {
		if errors.As(err, &typed) && typed.Code == model.CodeCursorInvalid {
			t.Fatalf("the retry of a busy page was refused: %s -- the continuation "+
				"was released on a non-terminal outcome, so the walk behind it is unrecoverable",
				typed.Code)
		}
		t.Fatalf("the retry of a busy page failed: %v", err)
	}
	if err := retry.Validate(); err != nil {
		t.Fatalf("the retried page does not satisfy its own contract: %v", err)
	}
	if len(retry.Relations) == 0 {
		t.Fatalf("the retried page served no relation: the resumed walk lost its frontier")
	}
}
