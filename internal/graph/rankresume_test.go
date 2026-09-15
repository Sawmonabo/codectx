package graph

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/Sawmonabo/codectx/internal/model"
	"github.com/Sawmonabo/codectx/internal/pagination"
)

// TestImpactRankResumesTheInterruptedSortItself is ruling P7's proof, and the
// one the pagination.AdoptRuns/Detach seam was built for.
//
// P7 is the deadline that lands after the WALK is complete but before the
// RANKING is. The walk retention (walkretain.go) already made that resumable,
// but only by re-sorting the whole retained input on every resumed request: an
// answer the deadline cut into k legs paid for k rankings. The runs the
// interrupted sort had already spilled are now moved into the retained state
// directory and adopted by the next request, which adds only the records those
// runs do not hold -- so a ranking split across requests does the work of ONE
// ranking and serves the byte-identical answer.
//
// The assertions are therefore two, and neither implies the other:
//
//   - PARITY. The split pages concatenate to the uninterrupted answer, in its
//     order, with no entity listed twice. A ranking that re-sorted from
//     scratch would also satisfy this.
//   - RUN REUSE. The chain adopted runs (AdoptedRuns > 0) and re-added none of
//     the records they held (ReaddedRecords == 0). This is what a re-sorting
//     ranking cannot satisfy.
//
// Mutation proof: make openImpactSort ignore the manifest's runs (take the
// NewExternalSort branch unconditionally) and feedRankPass skip nothing (drop
// the `seen <= adopted` early return) -- i.e. re-sort the whole retained input,
// which is what ruling P7 did before this change. PARITY STILL HOLDS and the
// run-reuse assertion fails:
//
//	rankresume_test.go: the resumed ranking adopted 0 run(s) and re-added 0
//	  record(s): it re-sorted the retained input instead of continuing the sort
func TestImpactRankResumesTheInterruptedSortItself(t *testing.T) {
	// rankStopEvery cuts each request's ranking after this many records, and
	// convergeAfterLegs is when the cutting stops. The two are chosen together
	// so that BOTH passes are cut at least twice before the chain is allowed to
	// converge; the assertions below check that they were, so a fixture change
	// that made one pass fit in a single leg fails the test rather than
	// silently proving less.
	const rankStopEvery, convergeAfterLegs = 100, 6
	f := newGraphFixture(t)
	req := model.ImpactRequest{GenerationID: 1,
		Start:     []model.NodeID{fixtureNodeID("n-a"), fixtureNodeID("n-wide")},
		Direction: model.DirectionOutgoing, Relations: []model.RelationKind{model.RelCalls}}

	// Ground truth: the same query with nothing interrupting the ranking. A
	// frontier ceiling the fixture never reaches, so the WALK cannot split and
	// every continuation below is P7's rank continuation and not P3's.
	whole, wholeDir, _ := rankResumeEngine(t, f, 0)
	all, err := whole.Impact(context.Background(), req)
	if err != nil {
		t.Fatalf("uninterrupted impact: %v", err)
	}
	if all.Meta.Truncated || all.Meta.NextCursor != "" || len(all.Entries) == 0 {
		t.Fatalf("ground truth is truncated (%q), paged (%v) or empty (%d entries): "+
			"this proof needs one request that walks and ranks the whole answer",
			all.Meta.TruncationReason, all.Meta.NextCursor != "", len(all.Entries))
	}
	assertNoRetainedState(t, wholeDir)

	// The interrupted chain: every ranking pass stops after a handful of
	// records, so both passes are cut and resumed several times.
	paged, spoolDir, probe := rankResumeEngine(t, f, rankStopEvery)
	var entries []model.ImpactEntry
	var packages []model.PackageEdge
	// foldLegs and rankLegs are how many requests the deadline cut in pass 1 and
	// in pass 2: Rank.PeakLiveRecords is observed only once pass 1 has produced
	// its sorted output, so it is the discriminator between the two.
	//
	// The hook is switched OFF once pass 2 has been cut twice, and that is what
	// makes the mutation below a fair comparison rather than a hang: a ranking
	// that does not continue its runs makes no progress at all under a deadline
	// that keeps firing, so a chain that never converges would tell us nothing
	// about whether the ANSWER is still right. With the hook off the last
	// request finishes whatever is left, and the mutation's re-sorting resume
	// reaches the same answer -- by re-sorting the whole retained input, which
	// is exactly what the run-reuse assertion catches and parity cannot.
	legs, foldLegs, rankLegs := 0, 0, 0
	for pages := 1; ; pages++ {
		if pages > 200 {
			t.Fatalf("the rank-split impact did not terminate after %d requests", pages-1)
		}
		res, err := paged.Impact(context.Background(), req)
		if err != nil {
			t.Fatalf("request %d: a mid-rank deadline threw the answer away: %v", pages, err)
		}
		if err := res.Validate(); err != nil {
			t.Fatalf("request %d does not satisfy its own contract: %v", pages, err)
		}
		if res.Meta.Truncated && res.Meta.TruncationReason == reasonDeadline {
			if res.Meta.NextCursor == "" {
				t.Fatalf("request %d stopped on the mid-rank deadline with no cursor: "+
					"the ranked answer is unreachable", pages)
			}
			if len(res.Entries) != 0 {
				t.Fatalf("request %d served %d entries off a ranking that had not finished",
					pages, len(res.Entries))
			}
			legs++
			if probe.Rank.PeakLiveRecords == 0 {
				foldLegs++
			} else {
				rankLegs++
			}
			if legs >= convergeAfterLegs {
				paged.rankStopAfter = 0
			}
		}
		entries = append(entries, res.Entries...)
		packages = append(packages, res.Packages...)
		if res.Meta.NextCursor == "" {
			break
		}
		req.Page = model.PageRequest{Cursor: res.Meta.NextCursor}
		req.GenerationID = 0
	}
	if foldLegs < 2 || rankLegs < 2 {
		t.Fatalf("the deadline cut the fold pass %d time(s) and the rank pass %d: this proof "+
			"needs BOTH passes spread over more than one request", foldLegs, rankLegs)
	}

	// PARITY.
	if len(entries) != len(all.Entries) {
		t.Fatalf("the rank-split requests served %d affected entit(ies), the uninterrupted "+
			"answer %d", len(entries), len(all.Entries))
	}
	seen := map[model.NodeID]bool{}
	for i, want := range all.Entries {
		if !reflect.DeepEqual(entries[i], want) {
			t.Fatalf("at rank %d the resumed ranking reports %+v, the uninterrupted one %+v",
				i, entries[i], want)
		}
		if seen[want.NodeID] {
			t.Fatalf("node %s is listed twice across the resumed ranking's pages", want.NodeID)
		}
		seen[want.NodeID] = true
	}
	if len(packages) != len(all.Packages) {
		t.Fatalf("the rank-split requests carried %d package pair(s), the uninterrupted answer %d",
			len(packages), len(all.Packages))
	}
	for i, want := range all.Packages {
		if packages[i] != want {
			t.Fatalf("at rank %d the resumed ranking carries %+v, the uninterrupted one %+v",
				i, packages[i], want)
		}
	}

	// RUN REUSE.
	if probe.AdoptedRuns == 0 || probe.ReaddedRecords != 0 {
		t.Fatalf("the resumed ranking adopted %d run(s) and re-added %d record(s): it re-sorted "+
			"the retained input instead of continuing the sort",
			probe.AdoptedRuns, probe.ReaddedRecords)
	}
	// The retained state -- the input, the adopted runs and the manifest alike
	// -- is continuation state: once the tail is served none of it may survive.
	assertNoRetainedState(t, spoolDir)
}

// rankResumeEngine builds an engine over f whose ranking passes stop after
// stopAfter records of each request (zero = never), and returns it with its
// spool directory and its memory/run-reuse probe.
func rankResumeEngine(t *testing.T, f *graphFixture, stopAfter int) (*Engine, string, *heapProbe) {
	t.Helper()
	signer, err := pagination.OpenSigner(t.TempDir())
	if err != nil {
		t.Fatalf("open signer: %v", err)
	}
	store := newFixtureLeases()
	spoolDir := t.TempDir()
	spools, err := pagination.NewSpools(spoolDir, 64<<20, store)
	if err != nil {
		t.Fatalf("new spools: %v", err)
	}
	limits := fixtureLimits()
	// Unlimited depth, visited and edge budgets and a frontier ceiling the
	// fixture never reaches: the RANKING is the only thing the deadline can
	// interrupt, so every continuation is ruling P7's.
	limits.MaxDepth, limits.MaxVisited, limits.MaxEdges = 0, 0, 0
	limits.MaxPageItems = 100000
	limits.QueryTimeout = time.Minute
	limits.FrontierBytes = 32 << 20
	e, err := New(Options{Adjacency: f, Signer: signer, Spools: spools,
		Leases: pagination.NewLeases(store, limits.CursorTTL), Limits: limits})
	if err != nil {
		t.Fatalf("new engine: %v", err)
	}
	probe := &heapProbe{}
	e.probe, e.rankStopAfter = probe, stopAfter
	return e, spoolDir, probe
}
