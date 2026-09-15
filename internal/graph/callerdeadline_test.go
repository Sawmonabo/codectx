package graph

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Sawmonabo/codectx/internal/model"
	"github.com/Sawmonabo/codectx/internal/pagination"
)

// TestTheCallersDeadlineIsTheRequestsDeadline protects the rule that
// resources.query_timeout is a DEFAULT and never a ceiling.
//
// The failure mode: an entry point that computes its own deadline as
// now+query_timeout and never looks at the context it was handed silently
// clamps a `--timeout 120s` the operator typed to the configured 10s -- the
// answer comes back out of time at a twelfth of the time asked for, and no
// bound the operator could set would widen it.
//
// The fixture makes that observable without any wall-clock waiting: the engine
// clock is FROZEN AN HOUR IN THE PAST, so a deadline derived from
// query_timeout is an instant that has already passed and every adjacency read
// refuses immediately (ctxAdjacency returns the context's own error, as the
// SQLite reader does). A caller deadline a minute in the future must therefore
// let exactly the same query run to a complete answer.
func TestTheCallersDeadlineIsTheRequestsDeadline(t *testing.T) {
	cases := []struct {
		name string
		// run reports whether the answer was cut short by the deadline, and how
		// many records it carried.
		run func(t *testing.T, e *Engine, ctx context.Context) (limited bool, size int)
		// cancel runs the same operation and returns the error code it answered
		// a canceled caller with.
		cancel func(e *Engine, ctx context.Context) error
	}{
		{name: "impact", run: func(t *testing.T, e *Engine, ctx context.Context) (bool, int) {
			res, err := e.Impact(ctx, model.ImpactRequest{GenerationID: 1,
				Start:     []model.NodeID{fixtureNodeID("n-a")},
				Direction: model.DirectionOutgoing, Relations: []model.RelationKind{model.RelCalls}})
			if err != nil {
				return deadlineError(t, err), 0
			}
			return res.Meta.Truncated && res.Meta.TruncationReason == reasonDeadline, len(res.Entries)
		}, cancel: func(e *Engine, ctx context.Context) error {
			_, err := e.Impact(ctx, model.ImpactRequest{GenerationID: 1,
				Start:     []model.NodeID{fixtureNodeID("n-a")},
				Direction: model.DirectionOutgoing, Relations: []model.RelationKind{model.RelCalls}})
			return err
		}},
		{name: "neighbors", run: func(t *testing.T, e *Engine, ctx context.Context) (bool, int) {
			res, err := e.Neighbors(ctx, model.GraphRequest{GenerationID: 1,
				Start:     []model.NodeID{fixtureNodeID("n-a")},
				Direction: model.DirectionOutgoing, Relations: []model.RelationKind{model.RelCalls}})
			if err != nil {
				return deadlineError(t, err), 0
			}
			return res.Meta.Truncated && res.Meta.TruncationReason == reasonDeadline, len(res.Relations)
		}, cancel: func(e *Engine, ctx context.Context) error {
			_, err := e.Neighbors(ctx, model.GraphRequest{GenerationID: 1,
				Start:     []model.NodeID{fixtureNodeID("n-a")},
				Direction: model.DirectionOutgoing, Relations: []model.RelationKind{model.RelCalls}})
			return err
		}},
		{name: "references", run: func(t *testing.T, e *Engine, ctx context.Context) (bool, int) {
			page, err := e.References(ctx, model.ReferenceRequest{GenerationID: 1,
				NodeID:         fixtureNodeID("n-b"),
				Operation:      model.ReferenceReferences,
				SemanticSource: model.SemanticCanonical})
			if err != nil {
				return deadlineError(t, err), 0
			}
			return page.Meta.Truncated && page.Meta.TruncationReason == reasonDeadline, len(page.Items)
		}, cancel: func(e *Engine, ctx context.Context) error {
			_, err := e.References(ctx, model.ReferenceRequest{GenerationID: 1,
				NodeID:         fixtureNodeID("n-b"),
				Operation:      model.ReferenceReferences,
				SemanticSource: model.SemanticCanonical})
			return err
		}},
		{name: "path", run: func(t *testing.T, e *Engine, ctx context.Context) (bool, int) {
			res, err := e.ShortestPath(ctx, model.PathRequest{GenerationID: 1,
				From: fixtureNodeID("n-a"), To: fixtureNodeID("n-z"),
				Relations: []model.RelationKind{model.RelCalls}})
			if err != nil {
				return deadlineError(t, err), 0
			}
			return res.Meta.Truncated, len(res.Paths)
		}, cancel: func(e *Engine, ctx context.Context) error {
			_, err := e.ShortestPath(ctx, model.PathRequest{GenerationID: 1,
				From: fixtureNodeID("n-a"), To: fixtureNodeID("n-z"),
				Relations: []model.RelationKind{model.RelCalls}})
			return err
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// No caller deadline: the configured default applies, measured on
			// the engine clock, and it has already passed.
			e := callerDeadlineEngine(t, fixtureLimits().QueryTimeout)
			limited, size := tc.run(t, e, context.Background())
			if !limited {
				t.Fatalf("with no caller deadline the configured query_timeout did not bound the "+
					"request: it answered %d record(s) untruncated", size)
			}

			// query_timeout 0 is NO deadline, and it is the shipped default:
			// the same query, the same stale clock, no caller deadline, and it
			// must run to a complete answer rather than be refused by an
			// instant that has already passed.
			e = callerDeadlineEngine(t, 0)
			limited, size = tc.run(t, e, context.Background())
			if limited {
				t.Fatalf("query_timeout 0 means NO deadline, yet the request was cut short: " +
					"`now + 0` is being applied as a deadline that has already passed")
			}
			if size == 0 {
				t.Fatalf("the unbounded request answered nothing")
			}

			// The same query, with the deadline the caller set. It is longer
			// than query_timeout, and it is the one that must apply.
			e = callerDeadlineEngine(t, fixtureLimits().QueryTimeout)
			ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(time.Minute))
			defer cancel()
			limited, size = tc.run(t, e, ctx)
			if limited {
				t.Fatalf("the caller's deadline is a minute away, yet the request was cut short: " +
					"resources.query_timeout is being applied as a ceiling")
			}
			if size == 0 {
				t.Fatalf("the request answered nothing: this case cannot tell a widened " +
					"deadline from an empty fixture")
			}
		})
	}
}

// deadlineError reports whether err is the query deadline, and fails the test
// on any other error -- a fixture that broke for an unrelated reason must not
// read as a deadline this test is measuring.
func deadlineError(t *testing.T, err error) bool {
	t.Helper()
	var typed *model.Error
	if errors.As(err, &typed) && typed.Code == model.CodeQueryDeadline {
		return true
	}
	if strings.Contains(err.Error(), context.DeadlineExceeded.Error()) {
		return true
	}
	t.Fatalf("unexpected error: %v", err)
	return false
}

// callerDeadlineEngine builds an engine whose clock is frozen an hour in the
// past over an adjacency that honours the context, so a deadline derived from a
// POSITIVE query_timeout is already expired while one taken from the caller --
// or no deadline at all -- is not. timeout 0 is the no-deadline case.
func callerDeadlineEngine(t *testing.T, timeout time.Duration) *Engine {
	t.Helper()
	f := newGraphFixture(t)
	adj := &ctxAdjacency{graphFixture: f}
	signer, err := pagination.OpenSigner(t.TempDir())
	if err != nil {
		t.Fatalf("open signer: %v", err)
	}
	store := &ctxLeases{newFixtureLeases()}
	spools, err := pagination.NewSpools(t.TempDir(), 0, store)
	if err != nil {
		t.Fatalf("new spools: %v", err)
	}
	limits := fixtureLimits()
	limits.MaxDepth, limits.MaxVisited, limits.MaxEdges = 0, 0, 0
	limits.QueryTimeout = timeout
	past := time.Now().Add(-time.Hour)
	e, err := New(Options{Adjacency: adj, Reader: memGraphFor(f), Signer: signer, Spools: spools,
		Leases: pagination.NewLeases(store, limits.CursorTTL), Limits: limits,
		Now: func() time.Time { return past }})
	if err != nil {
		t.Fatalf("new engine: %v", err)
	}
	return e
}
