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
// Every graph entry point used to compute its own deadline as
// now+query_timeout and never look at the context it was handed, so a
// `--timeout 120s` the operator typed was silently clamped to the configured
// 10s: the answer came back out of time at a twelfth of the time asked for,
// and no bound the operator could set would widen it.
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
	}{
		{"impact", func(t *testing.T, e *Engine, ctx context.Context) (bool, int) {
			res, err := e.Impact(ctx, model.ImpactRequest{GenerationID: 1,
				Start:     []model.NodeID{fixtureNodeID("n-a")},
				Direction: model.DirectionOutgoing, Relations: []model.RelationKind{model.RelCalls}})
			if err != nil {
				return deadlineError(t, err), 0
			}
			return res.Meta.Truncated && res.Meta.TruncationReason == reasonDeadline, len(res.Entries)
		}},
		{"neighbors", func(t *testing.T, e *Engine, ctx context.Context) (bool, int) {
			res, err := e.Neighbors(ctx, model.GraphRequest{GenerationID: 1,
				Start:     []model.NodeID{fixtureNodeID("n-a")},
				Direction: model.DirectionOutgoing, Relations: []model.RelationKind{model.RelCalls}})
			if err != nil {
				return deadlineError(t, err), 0
			}
			return res.Meta.Truncated && res.Meta.TruncationReason == reasonDeadline, len(res.Relations)
		}},
		{"references", func(t *testing.T, e *Engine, ctx context.Context) (bool, int) {
			page, err := e.References(ctx, model.ReferenceRequest{GenerationID: 1,
				NodeID:         fixtureNodeID("n-b"),
				Operation:      model.ReferenceReferences,
				SemanticSource: model.SemanticCanonical})
			if err != nil {
				return deadlineError(t, err), 0
			}
			return page.Meta.Truncated && page.Meta.TruncationReason == reasonDeadline, len(page.Items)
		}},
		{"path", func(t *testing.T, e *Engine, ctx context.Context) (bool, int) {
			res, err := e.ShortestPath(ctx, model.PathRequest{GenerationID: 1,
				From: fixtureNodeID("n-a"), To: fixtureNodeID("n-z"),
				Relations: []model.RelationKind{model.RelCalls}})
			if err != nil {
				return deadlineError(t, err), 0
			}
			return res.Meta.Truncated, len(res.Paths)
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// No caller deadline: the configured default applies, measured on
			// the engine clock, and it has already passed.
			e, _ := callerDeadlineEngine(t)
			limited, size := tc.run(t, e, context.Background())
			if !limited {
				t.Fatalf("with no caller deadline the configured query_timeout did not bound the "+
					"request: it answered %d record(s) untruncated", size)
			}

			// The same query, with the deadline the caller set. It is longer
			// than query_timeout, and it is the one that must apply.
			e, _ = callerDeadlineEngine(t)
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
// past over an adjacency that honours the context, so a deadline derived from
// query_timeout is already expired and one taken from the caller is not.
func callerDeadlineEngine(t *testing.T) (*Engine, *ctxAdjacency) {
	t.Helper()
	adj := &ctxAdjacency{graphFixture: newGraphFixture(t)}
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
	past := time.Now().Add(-time.Hour)
	e, err := New(Options{Adjacency: adj, Signer: signer, Spools: spools,
		Leases: pagination.NewLeases(store, limits.CursorTTL), Limits: limits,
		Now: func() time.Time { return past }})
	if err != nil {
		t.Fatalf("new engine: %v", err)
	}
	return e, adj
}
