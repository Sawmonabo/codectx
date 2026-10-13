package graph

import (
	"context"
	"testing"
	"time"

	"github.com/Sawmonabo/codectx/internal/model"
	"github.com/Sawmonabo/codectx/internal/pagination"
)

// inlineEngine is chainEngine with its lease store and a heap probe in reach.
func inlineEngine(t *testing.T, f *graphFixture, limit int) (*Engine, string, *fixtureLeases, *heapProbe) {
	t.Helper()
	signer, err := pagination.OpenSigner(t.TempDir())
	if err != nil {
		t.Fatalf("open signer: %v", err)
	}
	store := newFixtureLeases()
	root := t.TempDir()
	spools, err := pagination.NewSpools(root, 0, store)
	if err != nil {
		t.Fatalf("new spools: %v", err)
	}
	limits := fixtureLimits()
	limits.MaxDepth, limits.MaxVisited, limits.MaxEdges = 0, 0, 0
	limits.MaxPageItems = limit
	limits.QueryTimeout = time.Minute
	limits.CursorTTL = time.Hour
	e, err := New(Options{Adjacency: f, Reader: memGraphFor(f), Signer: signer, Spools: spools,
		Leases: pagination.NewLeases(store, limits.CursorTTL), Limits: limits})
	if err != nil {
		t.Fatalf("new engine: %v", err)
	}
	probe := &heapProbe{}
	e.probe = probe
	return e, root, store, probe
}

// twoKindChain is a chain of hops calls in which every hop also imports the
// next node, so every entry has two reasons.
func twoKindChain(hops int) (*graphFixture, []model.NodeID) {
	f, chain := newChainFixture(hops)
	for i := 0; i < hops; i++ {
		f.relations = append(f.relations, model.Relation{ID: fixtureRelationID(1000 + i),
			From: chain[i], Kind: model.RelImports, To: chain[i+1]})
	}
	return f, chain
}

// TestAOnePageAnswerIsServedWholeAndRetainsNothing pins the inline answer: when
// one page of every view the request reads holds that whole view, the one
// response carries the entries, every reason and every route, and nothing is
// retained -- no spool written, no spool or route table left in the spool
// store, no lease taken, no answer token and no continuation (the walk's own
// scratch is freed through the pace, as every walk's is). Whatever view the
// request names, it is served the same whole answer.
//
// Mutations caught: serveRankedRun spooling every answer again (the inline
// hook removed), so the probe sees spool bytes, a lease is held and a token is
// minted; serveInline leaving out the reasons or the routes, so the served
// counts fall short of what the entries name; and answerFitsOnePage counting
// the reasons of a request that does not read them, so a request for the
// entries and routes whose reasons overflow a page is spooled.
func TestAOnePageAnswerIsServedWholeAndRetainsNothing(t *testing.T) {
	const hops = 6
	cases := []struct {
		name  string
		limit int
		view  model.ImpactView
		views []model.ImpactView
	}{
		{name: "every view, asked for its entries", limit: 12},
		{name: "every view, asked for its routes", limit: 12, view: model.ImpactViewRoutes},
		// Twelve reasons overflow a page of eight, but the request does not
		// read them.
		{name: "entries and routes, reasons past a page", limit: 8,
			views: []model.ImpactView{model.ImpactViewEntries, model.ImpactViewRoutes}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f, chain := twoKindChain(hops)
			e, root, leases, probe := inlineEngine(t, f, tc.limit)
			req := chainRequest(chain, tc.limit)
			req.Relations = []model.RelationKind{model.RelCalls, model.RelImports}
			req.View, req.Views = tc.view, tc.views
			res, err := e.Impact(context.Background(), req)
			if err != nil {
				t.Fatalf("impact: %v", err)
			}
			if res.Answer != "" || res.Meta.NextCursor != "" {
				t.Fatalf("a one-page answer minted answer %q and next %q, want neither", res.Answer, res.Meta.NextCursor)
			}
			if probe.SpoolBytesWritten != 0 || leases.liveCount() != 0 {
				t.Fatalf("a one-page answer wrote %d spool bytes and holds %d leases, want none",
					probe.SpoolBytesWritten, leases.liveCount())
			}
			if left := spoolEntries(t, root); len(left) != 0 {
				t.Fatalf("a one-page answer left %v in the spool store", left)
			}
			if len(res.Entries) != hops {
				t.Fatalf("served %d entries, want %d", len(res.Entries), hops)
			}
			reads := req.Reads(model.ImpactViewReasons)
			if !reads && len(res.Reasons) != 0 {
				t.Fatalf("a request that does not read the reasons was served %d", len(res.Reasons))
			}
			reasons, routes := map[int64]int64{}, map[int64]int64{}
			for _, x := range res.Reasons {
				reasons[x.Entry]++
			}
			for _, x := range res.Routes {
				routes[x.Entry]++
				if want := chain[len(x.Relations)]; res.Entries[x.Entry].NodeID != want {
					t.Fatalf("entry %d was served a %d-relation route, which ends elsewhere", x.Entry, len(x.Relations))
				}
			}
			for _, en := range res.Entries {
				if en.ReasonCount != 2 || (reads && reasons[en.Ordinal] != en.ReasonCount) ||
					routes[en.Ordinal] != en.RouteCount || en.RouteCount != 1 {
					t.Fatalf("entry %d names %d reasons and %d routes and was served %d and %d inline", en.Ordinal,
						en.ReasonCount, en.RouteCount, reasons[en.Ordinal], routes[en.Ordinal])
				}
			}
		})
	}
}

// TestAnAnswerOverflowingOneViewIsSpooledAndPagesExactly pins the other side
// of the rule: an answer whose entries fit one page but whose reasons do not
// is spooled exactly as a longer answer is, its first page serves the asked
// view alone, and its views read through the answer token serve every reason
// and route once.
//
// Mutation caught: answerFitsOnePage dropping the reasons term, so twelve
// reasons are served inline on a page of eight and nothing is spooled.
func TestAnAnswerOverflowingOneViewIsSpooledAndPagesExactly(t *testing.T) {
	const hops, limit = 6, 8
	f, chain := twoKindChain(hops)
	e, root, _, probe := inlineEngine(t, f, limit)
	req := chainRequest(chain, limit)
	req.Relations = []model.RelationKind{model.RelCalls, model.RelImports}
	res, err := e.Impact(context.Background(), req)
	if err != nil {
		t.Fatalf("impact: %v", err)
	}
	if res.Answer == "" || probe.SpoolBytesWritten == 0 {
		t.Fatalf("an answer of %d reasons on a page of %d minted answer %q and wrote %d spool bytes, want it spooled",
			2*hops, limit, res.Answer, probe.SpoolBytesWritten)
	}
	if len(res.Entries) != hops || len(res.Reasons) != 0 || len(res.Routes) != 0 {
		t.Fatalf("the first page served %d entries, %d reasons, %d routes; want the %d entries alone",
			len(res.Entries), len(res.Reasons), len(res.Routes), hops)
	}
	reasons, routes := explainImpact(t, e, req, res.Answer)
	for _, en := range res.Entries {
		if int64(len(reasons[en.Ordinal])) != en.ReasonCount || int64(len(routes[en.Ordinal])) != en.RouteCount {
			t.Fatalf("entry %d names %d reasons and %d routes; its views served %d and %d", en.Ordinal,
				en.ReasonCount, en.RouteCount, len(reasons[en.Ordinal]), len(routes[en.Ordinal]))
		}
	}
	if left := spoolEntries(t, root); len(left) != 0 {
		t.Fatalf("an answer read through all three views left %v behind", left)
	}
}
