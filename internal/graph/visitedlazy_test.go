package graph

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Sawmonabo/codectx/internal/model"
	"github.com/Sawmonabo/codectx/internal/pagination"
)

// TestTheMembershipSummaryIsFrozenOnlyByAWalkThatPages is the lazy visited
// filter's invariant (REV-H3c G4).
//
// Every impact, deps and rollup query opens a retained walk whether or not it
// will ever hand a continuation out. The membership summary that walk carries
// exists to make a RESUME cheap -- it is the O(1) state that tells a resumed
// level which of its candidates an earlier page already admitted -- so a query
// that answers in one request and mints no cursor has nothing for it to
// accelerate, and the geometry it would freeze is a function of the configured
// query memory rather than of the answer. At the shipped default that was a
// four-mebibyte slice, a four-mebibyte byte buffer and a four-mebibyte
// zero-filled file, written before the first edge was read, on every such
// query.
//
// Two halves, and both are needed: the cost must disappear for the
// single-request query, and it must still be paid -- ONCE, however many pages
// the walk runs for -- by the walk that actually pages. A freeze repeated per
// page would rewrite the filter file with zeroes and report nodes the walk has
// already admitted as absent, which is a cross-page re-admission.
//
// Mutation, run and pasted in the lane report: the freeze moved back into
// openVisitedStore (eager allocation restored). The counter is written by the
// store itself rather than by the caller precisely so that the mutation is
// caught wherever the allocation moves to.
func TestTheMembershipSummaryIsFrozenOnlyByAWalkThatPages(t *testing.T) {
	// Four mids of four leaves: 21 admitted nodes, small enough that a single
	// request with a generous frontier ceiling cannot split into legs.
	const mids, fanOut = 4, 4
	const admitted = mids*(1+fanOut) + 1

	f := newReachableFixture(t, mids, fanOut)
	signer, err := pagination.OpenSigner(t.TempDir())
	if err != nil {
		t.Fatalf("open signer: %v", err)
	}
	leases := newFixtureLeases()
	spoolRoot := t.TempDir()
	spools, err := pagination.NewSpools(spoolRoot, 0, leases)
	if err != nil {
		t.Fatalf("new spools: %v", err)
	}
	limits := fixtureLimits()
	limits.MaxDepth, limits.MaxVisited, limits.MaxEdges = 0, 0, 0
	limits.MaxPageItems = 200
	limits.QueryTimeout = time.Minute
	limits.FrontierBytes = 1 << 20

	base := model.ImpactRequest{GenerationID: 1,
		Start:     []model.NodeID{fixtureNodeID("bound-seed")},
		Direction: model.DirectionOutgoing, Relations: []model.RelationKind{model.RelCalls}}

	// wantWords is the geometry the budget buys: the filter's share of the
	// frontier ceiling, in 64-bit words. It is read off the engine rather than
	// restated so that the assertion is "exactly one freeze", not "exactly this
	// many bytes".
	newEngine := func(adj Adjacency, now func() time.Time) (*Engine, *heapProbe) {
		e, err := New(Options{Adjacency: adj, Signer: signer, Spools: spools,
			Leases: pagination.NewLeases(leases, limits.CursorTTL), Limits: limits, Now: now})
		if err != nil {
			t.Fatalf("new engine: %v", err)
		}
		probe := &heapProbe{}
		e.probe = probe
		return e, probe
	}

	t.Run("one request, no cursor, no filter", func(t *testing.T) {
		// The counter alone is a proxy: the finding's words were "writes no
		// filter file", and the file is written and then carried away with the
		// walk's scratch directory when the query mints no cursor, so a check
		// made after Impact returns would pass over a walk that DID write one.
		// watch looks while the walk is running -- an eager freeze happens in
		// openVisitedStore, before the first edge is read -- and it records the
		// retained directory it looked in, so "no filter file" cannot be
		// satisfied by having looked nowhere.
		watch := &filterWatch{root: spoolRoot}
		e, probe := newEngine(watchAdjacency{Adjacency: f, watch: watch}, nil)
		res, err := e.Impact(context.Background(), base)
		if err != nil {
			t.Fatalf("impact: %v", err)
		}
		// Non-vacuity: the premise of the assertion is that this query really
		// did answer in one request. A query that minted a cursor would be the
		// OTHER half of the invariant and would be right to freeze.
		if res.Meta.NextCursor != "" {
			t.Fatalf("the query minted a continuation, so it is not the single-request case: "+
				"%d of %d nodes served in one page", len(res.Entries), admitted-1)
		}
		if res.VisitedCount != admitted {
			t.Fatalf("the walk admitted %d nodes; the fixture holds %d", res.VisitedCount, admitted)
		}
		if watch.dirs == 0 {
			t.Fatalf("the walk read %d edge batches but no retained walk directory was open under the "+
				"spool root while it ran, so finding no membership filter there proves nothing",
				watch.calls)
		}
		if watch.filters != 0 {
			t.Fatalf("a query that answered in one request and minted no cursor had already written "+
				"%s in its retained walk directory before the first edge was read; the summary only "+
				"ever accelerates a resume, and this query has none", visitedFilterFile)
		}
		if probe.VisitedFilterWords != 0 {
			t.Fatalf("a query that answered in one request and minted no cursor froze %d words of "+
				"membership summary (%d bytes of heap, the same again on disk). Nothing resumes over "+
				"it, so every word of it is a fixed per-request cost charged for state no page reads",
				probe.VisitedFilterWords, probe.VisitedFilterWords*8)
		}
	})

	t.Run("paged across requests, frozen once", func(t *testing.T) {
		clock := time.Now()
		calls, fired := 0, false
		// The clock passes the engine's deadline on every third adjacency read,
		// so this walk is split across REQUESTS: each page carries the frontier
		// forward in a continuation and reopens the same retained directory.
		slow := slowAdjacency{graphFixture: f, clock: &clock, calls: &calls,
			jump: 2 * time.Minute, fired: &fired, every: 3}
		e, probe := newEngine(slow, func() time.Time { return clock })

		var (
			pages   int
			visited int64
			req     = base
		)
		for {
			pages++
			if pages > 200 {
				t.Fatalf("the deadline-split walk did not terminate after %d pages", pages-1)
			}
			calls = 0
			res, err := e.Impact(context.Background(), req)
			if err != nil {
				t.Fatalf("page %d: %v", pages, err)
			}
			if res.VisitedCount > visited {
				visited = res.VisitedCount
			}
			if res.Meta.NextCursor == "" {
				break
			}
			req.Page, req.GenerationID = model.PageRequest{Cursor: res.Meta.NextCursor}, 0
		}
		if pages < 2 {
			t.Fatalf("the walk answered in one request, so it never paged and proves nothing here")
		}
		if visited != admitted {
			t.Fatalf("the paged walk admitted %d nodes across %d pages; the fixture holds %d",
				visited, pages, admitted)
		}
		// Exactly one freeze: a walk that pages needs the summary, and a walk
		// that froze it again on a later page would have rewritten the filter
		// file with zeroes and lost every bit the pages before it set.
		want := e.visitedFilterBytes() / 8
		if want <= 0 {
			t.Fatalf("the configured budget buys no summary at all (%d bytes), so 'once' is not "+
				"distinguishable from 'never'", e.visitedFilterBytes())
		}
		if probe.VisitedFilterWords != want {
			t.Fatalf("a walk paged over %d requests froze %d words of membership summary; one "+
				"geometry of %d words is what it may freeze. More than one freeze rewrites the "+
				"filter file with zeroes, and a node the walk has admitted then reads as absent",
				pages, probe.VisitedFilterWords, want)
		}
		t.Logf("%d pages, %d nodes, membership summary frozen once at %d words", pages, visited, want)
	})
}

// filterWatch records, while a walk is RUNNING, whether the retained walk
// directory under the spool root carries a membership filter file. It counts
// the directories it found as well: an assertion that no filter file exists is
// vacuous unless the directory it would live in was there to look in.
type filterWatch struct {
	root    string
	calls   int
	dirs    int
	filters int
}

func (w *filterWatch) look() {
	w.calls++
	if w.calls > 1 {
		return
	}
	entries, err := os.ReadDir(w.root)
	if err != nil {
		return
	}
	for _, e := range entries {
		if !e.IsDir() || !strings.HasPrefix(e.Name(), "walkretain-") {
			continue
		}
		w.dirs++
		if _, err := os.Stat(filepath.Join(w.root, e.Name(), visitedFilterFile)); err == nil {
			w.filters++
		}
	}
}

// watchAdjacency runs the watch on the first edge batch, which is the first
// moment the walk is past openVisitedStore and the last one before an eager
// freeze could be mistaken for a lazy one.
type watchAdjacency struct {
	Adjacency
	watch *filterWatch
}

func (a watchAdjacency) Edges(ctx context.Context, nodes []model.NodeID, dir model.Direction,
	kinds []model.RelationKind, after model.RelationID, limit int) ([]model.Relation, error) {
	a.watch.look()
	return a.Adjacency.Edges(ctx, nodes, dir, kinds, after, limit)
}
