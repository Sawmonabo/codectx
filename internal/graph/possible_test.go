package graph

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/Sawmonabo/codectx/internal/model"
)

// The possible rows of call and import answers (ADR-0015 decisions 3 and 6):
// open sites the linker publishes, their candidates computed per query.

func openSite(shape model.CallShape, arguments int64) json.RawMessage {
	raw, _ := json.Marshal(model.OpenSiteMetadata{Resolution: model.ResolutionUnresolved,
		Reason: model.ReasonReceiverUntyped, Shape: shape, Arguments: arguments})
	return raw
}

func site(at uint64) []fixtureOccurrence {
	return []fixtureOccurrence{{Bytes: [2]uint64{at, at + 10}, Dispatch: model.DispatchExact}}
}

// untypedCall is `def run(obj): obj.handle()` in app.py, with the linker's
// open site for the call, a method `handle` it may be, and a function
// `lonely` nothing calls by any name.
func untypedCall() *callFixture {
	f := newCallFixture("untyped_call", fixtureScenario{
		Nodes: []fixtureDecl{
			{Name: "run", Kind: model.NodeFunction, File: "app.py", Bytes: [2]uint64{0, 40}},
			{Key: "site", Name: "handle", Kind: model.NodeFunction, File: "app.py", Bytes: [2]uint64{20, 30},
				Metadata: openSite(model.CallShapeMember, 0)},
			{Name: "handle", Kind: model.NodeMethod, File: "plugins/p.py", Bytes: [2]uint64{0, 30}},
			{Name: "lonely", Kind: model.NodeFunction, File: "lib.py", Bytes: [2]uint64{0, 20}},
		},
		Relations: []fixtureFact{{From: "run", Kind: model.RelCalls, To: "site", Occurrences: site(20)}},
	})
	f.caps = []model.CapabilityState{{ProviderID: "linker", Capability: "calls", Scope: "repository",
		State: model.CapabilityFresh,
		Details: map[string]string{"sites": "12", "bound_sites": "11", "open_sites": "1", "computed_sites": "0",
			"external_sites": "0"}}}
	return f
}

// TestCallersOfAFunctionCalledOnlyByNameListPossibleCallers pins ADR-0015's
// "never a bare zero": a method called only through `obj.handle()` with
// unknown `obj` has no precise caller, and the answer lists `run` as a
// possible caller, marked name only, located at the call, counted, and said
// in a notice.
//
// Mutation caught: dropping the candidates (the answer reports no caller).
func TestCallersOfAFunctionCalledOnlyByNameListPossibleCallers(t *testing.T) {
	f := untypedCall()
	res := callAnswer(t, f, "handle", model.DirectionIncoming)
	want := model.NewRelationID(f.binding.RepositoryID, f.id(t, "run"), model.RelMayCall, f.id(t, "handle"))
	if !hasRelation(res, want) {
		t.Fatalf("callers(handle) = %v, want run as a possible caller through obj.handle()", res.Relations)
	}
	if ms := marksOf(res, want); len(ms) != 1 || ms[0].Kind != model.MarkNameOnly {
		t.Errorf("run's possible call is marked %+v, want name_only", ms)
	}
	if res.Counts.MayCall != 1 || res.Counts.OpenSites != 1 || res.Counts.Direct != 0 {
		t.Errorf("counts = %+v, want may_call 1 and open_sites 1 beside no direct call", res.Counts)
	}
	var located bool
	for _, o := range res.Occurrences {
		located = located || (o.RelationID == want && o.Bytes.Start == 20)
	}
	if !located {
		t.Errorf("the possible call carries no occurrence at the call site: %v", res.Occurrences)
	}
	if !slices.ContainsFunc(res.Meta.Notices, func(n string) bool { return strings.Contains(n, "by name only") }) {
		t.Errorf("notices %q do not say the callers are listed by name only", res.Meta.Notices)
	}
}

// TestNoCallerAtAllQuotesTheProjectsSiteCounts pins the zero case: with no
// precise caller and no candidate the notice states how many of the
// project's call sites are bound and open.
//
// Mutation caught: answering a bare zero with no notice.
func TestNoCallerAtAllQuotesTheProjectsSiteCounts(t *testing.T) {
	f := untypedCall()
	res := callAnswer(t, f, "lonely", model.DirectionIncoming)
	if len(res.Relations) != 0 {
		t.Fatalf("callers(lonely) = %v, want none", res.Relations)
	}
	if !slices.ContainsFunc(res.Meta.Notices, func(n string) bool {
		return strings.Contains(n, "bound sites 11") && strings.Contains(n, "open sites 1")
	}) {
		t.Errorf("notices %q do not quote the bound and open site counts", res.Meta.Notices)
	}
}

// TestPathsUseANameOnlyHopOnlyWithoutAPreciseRoute pins the path rule: a
// pair joined only by a call no analysis bound gets that route, marked
// Possible and costed as the dearest relation, and a pair with a precise
// route never gets a possible one.
//
// Mutation caught: admitting `may_call` at the precise cost (the possible
// route would be offered beside, or cheaper than, a precise one).
func TestPathsUseANameOnlyHopOnlyWithoutAPreciseRoute(t *testing.T) {
	f := newCallFixture("possible_path", fixtureScenario{
		Nodes: []fixtureDecl{
			{Name: "run", Kind: model.NodeFunction, File: "app.py", Bytes: [2]uint64{0, 40}},
			{Name: "direct", Kind: model.NodeFunction, File: "app.py", Bytes: [2]uint64{50, 90}},
			{Key: "site_run", Name: "handle", Kind: model.NodeFunction, File: "app.py", Bytes: [2]uint64{20, 30},
				Metadata: openSite(model.CallShapeBare, 0)},
			{Key: "site_direct", Name: "handle", Kind: model.NodeFunction, File: "app.py", Bytes: [2]uint64{60, 70},
				Metadata: openSite(model.CallShapeBare, 0)},
			{Name: "handle", Kind: model.NodeFunction, File: "lib.py", Bytes: [2]uint64{0, 30}},
		},
		Relations: []fixtureFact{
			{From: "run", Kind: model.RelCalls, To: "site_run", Occurrences: site(20)},
			{From: "direct", Kind: model.RelCalls, To: "site_direct", Occurrences: site(60)},
			{From: "direct", Kind: model.RelCalls, To: "handle", Occurrences: site(75)},
		},
	})
	e := f.engine(t)
	path := func(from string) model.PathResult {
		res, err := e.ShortestPath(context.Background(), model.PathRequest{From: f.id(t, from), To: f.id(t, "handle"),
			Relations: []model.RelationKind{model.RelCalls}})
		if err != nil {
			t.Fatalf("path(%s, handle): %v", from, err)
		}
		return res
	}
	res := path("run")
	if len(res.Paths) != 1 || !res.Paths[0].Possible || res.Paths[0].CostUnits != Cost(model.RelMayCall) {
		t.Fatalf("path(run, handle) = %+v, want one possible route over the name-only call", res.Paths)
	}
	res = path("direct")
	if len(res.Paths) != 1 || res.Paths[0].Possible {
		t.Errorf("path(direct, handle) = %+v, want the precise call alone", res.Paths)
	}
}

// TestImpactCountsOpenSitesAndDoesNotWalkThem pins impact's rule: the open
// call sites naming an affected node are counted at its depth, and their
// callers are not listed as affected.
//
// Mutation caught: expanding the open sites (run would be affected).
func TestImpactCountsOpenSitesAndDoesNotWalkThem(t *testing.T) {
	f := untypedCall()
	res, err := f.engine(t).Impact(context.Background(), model.ImpactRequest{
		Start: []model.NodeID{f.id(t, "handle")}, Direction: model.DirectionIncoming})
	if err != nil {
		t.Fatalf("impact(handle): %v", err)
	}
	for _, en := range res.Entries {
		if en.NodeID == f.id(t, "run") {
			t.Errorf("impact(handle) lists run, whose call is name only and is counted, not walked")
		}
	}
	// handle itself is the seed; the count is of the sites naming the nodes
	// the walk reached, and here it reached none.
	if len(res.OpenSitesByDepth) != 0 {
		t.Errorf("impact(handle) counts %v open sites for a walk that reached nothing", res.OpenSitesByDepth)
	}

	g := newCallFixture("impact_open", fixtureScenario{
		Nodes: []fixtureDecl{
			{Name: "base", Kind: model.NodeFunction, File: "lib.py", Bytes: [2]uint64{0, 30}},
			{Name: "handle", Kind: model.NodeFunction, File: "lib.py", Bytes: [2]uint64{40, 70}},
			{Name: "run", Kind: model.NodeFunction, File: "app.py", Bytes: [2]uint64{0, 40}},
			{Key: "site", Name: "handle", Kind: model.NodeFunction, File: "app.py", Bytes: [2]uint64{20, 30},
				Metadata: openSite(model.CallShapeBare, 0)},
		},
		Relations: []fixtureFact{
			{From: "handle", Kind: model.RelCalls, To: "base", Occurrences: site(50)},
			{From: "run", Kind: model.RelCalls, To: "site", Occurrences: site(20)},
		},
	})
	res, err = g.engine(t).Impact(context.Background(), model.ImpactRequest{
		Start: []model.NodeID{g.id(t, "base")}, Direction: model.DirectionIncoming})
	if err != nil {
		t.Fatalf("impact(base): %v", err)
	}
	if !slices.Equal(res.OpenSitesByDepth, []int64{1}) {
		t.Errorf("impact(base) counts open sites %v, want [1]: run's handle() may call handle at depth 1",
			res.OpenSitesByDepth)
	}
	for _, en := range res.Entries {
		if en.NodeID == g.id(t, "run") {
			t.Errorf("impact(base) lists run; an open site is counted, not expanded")
		}
	}
}

// TestCalleesListTheOpenSiteOnceWithItsCandidates pins the callees half of
// open sites: a call the linker left open is one row, to its open-site node -- the
// structural placeholder for the same site is not a second callee -- and the
// candidates of the site follow as possible callees.
//
// Mutation caught: also listing the `call:<name>` placeholder.
func TestCalleesListTheOpenSiteOnceWithItsCandidates(t *testing.T) {
	placeholder, _ := json.Marshal(map[string]any{"resolution": "unresolved", "candidates": 0, "callee": "handle"})
	f := newCallFixture("open_callee", fixtureScenario{
		Nodes: []fixtureDecl{
			{Name: "run", Kind: model.NodeFunction, File: "app.py", Bytes: [2]uint64{0, 40}},
			{Key: "site", Name: "handle", Kind: model.NodeFunction, File: "app.py", Bytes: [2]uint64{20, 30},
				Metadata: openSite(model.CallShapeMember, 0)},
			{Key: "placeholder", Name: "handle", Kind: model.NodeMethod, File: "app.py", Bytes: [2]uint64{20, 30},
				Metadata: placeholder},
			{Key: "handle_a", Name: "handle", Kind: model.NodeMethod, File: "a.py", Bytes: [2]uint64{0, 30}},
			{Key: "handle_b", Name: "handle", Kind: model.NodeMethod, File: "b.py", Bytes: [2]uint64{0, 30}},
		},
		Relations: []fixtureFact{
			{From: "run", Kind: model.RelCalls, To: "site", Occurrences: site(20)},
			{From: "run", Kind: model.RelCalls, To: "placeholder", Occurrences: site(20)},
		},
	})
	res := callAnswer(t, f, "run", model.DirectionOutgoing)
	if hasRelation(res, f.rel(t, "run", model.RelCalls, "placeholder")) {
		t.Errorf("callees(run) lists the placeholder for the site the open site already stands for")
	}
	for _, n := range res.Nodes {
		if n.ID == f.id(t, "placeholder") {
			t.Errorf("callees(run) carries the placeholder node")
		}
	}
	if !hasRelation(res, f.rel(t, "run", model.RelCalls, "site")) {
		t.Fatalf("callees(run) omits the open site: %v", res.Relations)
	}
	for _, cand := range []string{"handle_a", "handle_b"} {
		id := model.NewRelationID(f.binding.RepositoryID, f.id(t, "run"), model.RelMayCall, f.id(t, cand))
		if !hasRelation(res, id) {
			t.Errorf("callees(run) omits candidate %s of the open site", cand)
		}
	}
	if res.Counts.MayCall != 2 || res.Counts.OpenSites != 1 {
		t.Errorf("counts = %+v, want 2 candidates of 1 open site", res.Counts)
	}
}

// TestOpenImportSiteListsTheFilesItMayImport pins the open import: an import whose path
// does not fold lists the files matching its literal fragments as possible
// imports, after the precise ones, and the walk does not go on through them.
//
// Mutation caught: walking the possible imports by default (d.js, which only
// a possible import's file imports, would be listed).
func TestOpenImportSiteListsTheFilesItMayImport(t *testing.T) {
	open, _ := json.Marshal(model.OpenImportSiteMetadata{Resolution: model.ResolutionUnresolved,
		Reason: model.ReasonComputed, Fragments: []string{"./plugins/"}})
	file := func(p string) fixtureDecl {
		return fixtureDecl{Key: p, Name: p[strings.LastIndexByte(p, '/')+1:], Kind: model.NodeFile, File: p,
			Bytes: [2]uint64{0, 100}, Language: "javascript"}
	}
	f := newCallFixture("open_import", fixtureScenario{
		Nodes: []fixtureDecl{
			file("src/app.js"), file("src/plugins/a.js"), file("src/plugins/b.js"), file("src/other/c.js"),
			file("src/lib/d.js"),
			{Key: "import_site", Name: "require", Kind: model.NodeModule, File: "src/app.js", Bytes: [2]uint64{10, 30},
				Language: "javascript", Metadata: open},
		},
		Relations: []fixtureFact{
			{From: "src/app.js", Kind: model.RelImports, To: "import_site", Occurrences: site(10)},
			{From: "src/plugins/a.js", Kind: model.RelImports, To: "src/lib/d.js", Occurrences: site(5)},
		},
	})
	res, err := f.engine(t).Neighbors(context.Background(), model.GraphRequest{
		Start: []model.NodeID{f.id(t, "src/app.js")}, Direction: model.DirectionOutgoing,
		Relations: []model.RelationKind{model.RelImports}, MaxDepth: 3})
	if err != nil {
		t.Fatalf("imports of app.js: %v", err)
	}
	var possible []string
	for _, r := range res.Relations {
		if r.Kind == model.RelMayImport {
			possible = append(possible, f.name(r.To))
		}
	}
	if !slices.Equal(possible, []string{"a.js", "b.js"}) {
		t.Errorf("possible imports = %v, want a.js and b.js (c.js is outside ./plugins/)", possible)
	}
	for _, r := range res.Relations {
		if r.To == f.id(t, "src/lib/d.js") {
			t.Errorf("the walk went on through a possible import to d.js")
		}
	}
	if !slices.ContainsFunc(res.Meta.Notices, func(n string) bool { return strings.Contains(n, "the 2 files") }) {
		t.Errorf("notices %q do not count the possible imports", res.Meta.Notices)
	}
}

// TestCandidatesOfANameWithThousandsOfDefinitions pins the no-cap rule: a
// name declared thousands of times has an exact total and a ranked first
// page, nearest first, and the pages after it list every other candidate
// exactly once.
//
// The pages are the widest a request may ask for (model.MaxPageItems): a page
// size with a continuation is the lossless form ADR-0001 keeps (row A), so
// the list is read through fifteen of them.
//
// Mutation caught: a fixed candidate cap (the total or the pages would stop
// short of 3000).
func TestCandidatesOfANameWithThousandsOfDefinitions(t *testing.T) {
	const n = 3000
	nodes := []fixtureDecl{
		{Name: "run", Kind: model.NodeFunction, File: "app/main.py", Bytes: [2]uint64{0, 40}},
		{Key: "site", Name: "handle", Kind: model.NodeFunction, File: "app/main.py", Bytes: [2]uint64{20, 30},
			Metadata: openSite(model.CallShapeBare, 1)},
		{Key: "handle_here", Name: "handle", Kind: model.NodeFunction, File: "app/main.py", Bytes: [2]uint64{50, 80}},
	}
	for i := range n - 1 {
		nodes = append(nodes, fixtureDecl{Key: fmt.Sprintf("handle_%d", i), Name: "handle", Kind: model.NodeFunction,
			File: fmt.Sprintf("pkg%d/m.py", i), Bytes: [2]uint64{0, 30}})
	}
	f := newCallFixture("many_names", fixtureScenario{Nodes: nodes,
		Relations: []fixtureFact{{From: "run", Kind: model.RelCalls, To: "site", Occurrences: site(20)}}})
	r := f.reader()
	seen := map[model.NodeID]bool{}
	cursor := ""
	for page := 0; ; page++ {
		got, err := Candidates(context.Background(), r, model.CandidateRequest{Site: f.id(t, "site"),
			Page: model.PageRequest{Limit: model.MaxPageItems, Cursor: cursor}})
		if err != nil {
			t.Fatalf("candidates page %d: %v", page, err)
		}
		if got.Total != n {
			t.Fatalf("candidates total = %d, want %d exactly", got.Total, n)
		}
		if page == 0 {
			if len(got.Candidates) != model.MaxPageItems || got.Candidates[0].NodeID != f.id(t, "handle_here") ||
				got.Candidates[0].Closeness != model.ClosenessSameFile {
				t.Fatalf("first page starts %+v, want the same-file declaration first", got.Candidates[:1])
			}
		}
		for _, c := range got.Candidates {
			if seen[c.NodeID] {
				t.Fatalf("candidate %s listed twice", c.NodeID)
			}
			seen[c.NodeID] = true
			if c.Resolution != model.ResolutionNameOnly {
				t.Fatalf("candidate %s resolved %q, want name_only among %d", c.NodeID, c.Resolution, n)
			}
		}
		if got.Cursor == "" {
			break
		}
		cursor = got.Cursor
	}
	if len(seen) != n {
		t.Errorf("the pages listed %d candidates, want all %d", len(seen), n)
	}
}

// TestStaticFamiliesMatchArgumentCounts pins the arity rule of the static
// families, read off the signature after the name: a Go receiver before the
// name is not a parameter, a variadic takes any number, and a declaration
// that cannot take the site's arguments is no candidate. A dynamic family
// keeps every same-name declaration.
//
// Mutation caught: counting the Go receiver as a parameter, or ignoring the
// argument count.
func TestStaticFamiliesMatchArgumentCounts(t *testing.T) {
	gosite, _ := json.Marshal(model.OpenSiteMetadata{Resolution: model.ResolutionUnresolved,
		Reason: model.ReasonReceiverUntyped, Shape: model.CallShapeMember, Arguments: 2})
	f := newCallFixture("arity", fixtureScenario{Nodes: []fixtureDecl{
		{Key: "site", Name: "Handle", Kind: model.NodeFunction, File: "main.go", Bytes: [2]uint64{20, 30},
			Language: "go", Metadata: gosite},
		{Key: "two", Name: "Handle", Kind: model.NodeMethod, File: "a.go", Bytes: [2]uint64{0, 30}, Language: "go",
			Signature: "func (s *Server) Handle(w Writer, r *Request)"},
		{Key: "grouped", Name: "Handle", Kind: model.NodeMethod, File: "b.go", Bytes: [2]uint64{0, 30}, Language: "go",
			Signature: "func (p Proxy) Handle(a, b int)"},
		{Key: "variadic", Name: "Handle", Kind: model.NodeMethod, File: "c.go", Bytes: [2]uint64{0, 30}, Language: "go",
			Signature: "func (l Log) Handle(args ...any)"},
		{Key: "one", Name: "Handle", Kind: model.NodeMethod, File: "d.go", Bytes: [2]uint64{0, 30}, Language: "go",
			Signature: "func (o Other) Handle(x int)"},
		{Key: "python", Name: "Handle", Kind: model.NodeMethod, File: "e.py", Bytes: [2]uint64{0, 30},
			Signature: "def Handle(self, x)"},
	}})
	got, err := Candidates(context.Background(), f.reader(), model.CandidateRequest{Site: f.id(t, "site")})
	if err != nil {
		t.Fatalf("candidates: %v", err)
	}
	var names []string
	for _, c := range got.Candidates {
		names = append(names, f.keyOf(c.NodeID))
	}
	slices.Sort(names)
	if !slices.Equal(names, []string{"grouped", "two", "variadic"}) {
		t.Errorf("candidates of a two-argument Go call = %v, want grouped, two and variadic", names)
	}
}

// TestAPlaceholderIsDroppedWhenItsOpenSiteIsOnAnotherPage pins that the
// one-row-per-site rule is decided from the pinned generation: a page that
// holds a placeholder's call but not the open site for the same call (that
// row is on another page) still drops the placeholder.
//
// Mutation caught: deciding from the open sites on the page alone.
func TestAPlaceholderIsDroppedWhenItsOpenSiteIsOnAnotherPage(t *testing.T) {
	placeholder, _ := json.Marshal(map[string]any{"resolution": "unresolved", "candidates": 0, "callee": "handle"})
	f := newCallFixture("split_site", fixtureScenario{
		Nodes: []fixtureDecl{
			{Name: "run", Kind: model.NodeFunction, File: "app.py", Bytes: [2]uint64{0, 40}},
			{Key: "site", Name: "handle", Kind: model.NodeFunction, File: "app.py", Bytes: [2]uint64{20, 30},
				Metadata: openSite(model.CallShapeMember, 0)},
			{Key: "placeholder", Name: "handle", Kind: model.NodeMethod, File: "app.py", Bytes: [2]uint64{20, 30},
				Metadata: placeholder},
		},
		Relations: []fixtureFact{
			{From: "run", Kind: model.RelCalls, To: "site", Occurrences: site(20)},
			{From: "run", Kind: model.RelCalls, To: "placeholder", Occurrences: site(20)},
		},
	})
	e := f.engine(t)
	r := f.reader()
	ctx := context.Background()
	toPlaceholder := f.rel(t, "run", model.RelCalls, "placeholder")
	page := []model.Relation{{ID: toPlaceholder, From: f.id(t, "run"), Kind: model.RelCalls, To: f.id(t, "placeholder")}}
	nodes, err := f.NodesByID(ctx, []model.NodeID{f.id(t, "run"), f.id(t, "placeholder")})
	if err != nil {
		t.Fatalf("nodes: %v", err)
	}
	where, err := e.locate(ctx, []model.NodeID{f.id(t, "run"), f.id(t, "placeholder")}, page)
	if err != nil {
		t.Fatalf("locate: %v", err)
	}
	_, rels, dropped, err := e.dropPlaceholders(ctx, r, nodes, page, &where)
	if err != nil {
		t.Fatalf("drop placeholders: %v", err)
	}
	if len(rels) != 0 || !dropped[toPlaceholder] {
		t.Errorf("the page keeps %v: the open site for the same call is in the generation, on another page", rels)
	}
}

// TestImpactWalksOpenSitesWhenAsked pins the other half of impact's rule: a
// request naming `may_call` lists the callers of the open sites an affected
// node is a candidate of, one depth deeper, on a route marked possible.
//
// Mutation caught: never expanding open sites (run is not listed).
func TestImpactWalksOpenSitesWhenAsked(t *testing.T) {
	f := newCallFixture("impact_through", fixtureScenario{
		Nodes: []fixtureDecl{
			{Name: "base", Kind: model.NodeFunction, File: "lib.py", Bytes: [2]uint64{0, 30}},
			{Name: "handle", Kind: model.NodeFunction, File: "lib.py", Bytes: [2]uint64{40, 70}},
			{Name: "run", Kind: model.NodeFunction, File: "app.py", Bytes: [2]uint64{0, 40}},
			{Key: "site", Name: "handle", Kind: model.NodeFunction, File: "app.py", Bytes: [2]uint64{20, 30},
				Metadata: openSite(model.CallShapeBare, 0)},
		},
		Relations: []fixtureFact{
			{From: "handle", Kind: model.RelCalls, To: "base", Occurrences: site(50)},
			{From: "run", Kind: model.RelCalls, To: "site", Occurrences: site(20)},
		},
	})
	res, err := f.engine(t).Impact(context.Background(), model.ImpactRequest{
		Start: []model.NodeID{f.id(t, "base")}, Direction: model.DirectionIncoming,
		Relations: []model.RelationKind{model.RelCalls, model.RelMayCall}})
	if err != nil {
		t.Fatalf("impact(base): %v", err)
	}
	var found bool
	for _, en := range res.Entries {
		if en.NodeID != f.id(t, "run") {
			continue
		}
		found = true
		if en.Depth != 2 || len(en.Paths) != 1 || !en.Paths[0].Possible {
			t.Errorf("run is affected at depth %d with paths %+v, want depth 2 on a possible route", en.Depth, en.Paths)
		}
	}
	if !found {
		t.Errorf("impact(base) naming may_call omits run, whose handle() may call handle")
	}
}

// TestAPossibleRouteHasPreciseLegsAroundTheHop pins the path search through
// one name-only call: main calls run precisely, run's `obj.handle()` may call
// handle, and handle calls leaf precisely, so path(main, leaf) is that route
// of three relations, possible, at the legs' cost plus the dearest one.
//
// Mutation caught: offering only a route that is the name-only hop alone.
func TestAPossibleRouteHasPreciseLegsAroundTheHop(t *testing.T) {
	f := newCallFixture("possible_legs", fixtureScenario{
		Nodes: []fixtureDecl{
			{Name: "main", Kind: model.NodeFunction, File: "main.py", Bytes: [2]uint64{0, 30}},
			{Name: "run", Kind: model.NodeFunction, File: "app.py", Bytes: [2]uint64{0, 40}},
			{Key: "site", Name: "handle", Kind: model.NodeFunction, File: "app.py", Bytes: [2]uint64{20, 30},
				Metadata: openSite(model.CallShapeMember, 0)},
			{Name: "handle", Kind: model.NodeMethod, File: "lib.py", Bytes: [2]uint64{0, 40}},
			{Name: "leaf", Kind: model.NodeFunction, File: "lib.py", Bytes: [2]uint64{50, 80}},
		},
		Relations: []fixtureFact{
			{From: "main", Kind: model.RelCalls, To: "run", Occurrences: site(5)},
			{From: "run", Kind: model.RelCalls, To: "site", Occurrences: site(20)},
			{From: "handle", Kind: model.RelCalls, To: "leaf", Occurrences: site(10)},
		},
	})
	res, err := f.engine(t).ShortestPath(context.Background(), model.PathRequest{From: f.id(t, "main"),
		To: f.id(t, "leaf"), Relations: []model.RelationKind{model.RelCalls}})
	if err != nil {
		t.Fatalf("path(main, leaf): %v", err)
	}
	if len(res.Paths) != 1 {
		t.Fatalf("path(main, leaf) = %+v, want one possible route", res.Paths)
	}
	p := res.Paths[0]
	hop := model.NewRelationID(f.binding.RepositoryID, f.id(t, "run"), model.RelMayCall, f.id(t, "handle"))
	want := []model.RelationID{f.rel(t, "main", model.RelCalls, "run"), hop, f.rel(t, "handle", model.RelCalls, "leaf")}
	if !p.Possible || !slices.Equal(p.Relations, want) ||
		p.CostUnits != 2*Cost(model.RelCalls)+Cost(model.RelMayCall) {
		t.Errorf("path(main, leaf) = %+v, want main->run, the name-only hop, handle->leaf", p)
	}
}

// TestAPossibleRouteTakesAsManyNameOnlyHopsAsItNeeds pins that the search,
// not a fixed count, decides the name-only hops: main's `obj.handle()` may
// call handle, and handle's `obj.finish()` may call finish, so path(main,
// finish) is the two hops, possible, at twice the dearest cost, and the
// answer counts both.
//
// Mutation caught: a fixed count of one hop (no route at all).
func TestAPossibleRouteTakesAsManyNameOnlyHopsAsItNeeds(t *testing.T) {
	f := newCallFixture("possible_hops", fixtureScenario{
		Nodes: []fixtureDecl{
			{Name: "main", Kind: model.NodeFunction, File: "main.py", Bytes: [2]uint64{0, 40}},
			{Key: "first", Name: "handle", Kind: model.NodeFunction, File: "main.py", Bytes: [2]uint64{20, 30},
				Metadata: openSite(model.CallShapeMember, 0)},
			{Name: "handle", Kind: model.NodeMethod, File: "lib.py", Bytes: [2]uint64{0, 40}},
			{Key: "second", Name: "finish", Kind: model.NodeFunction, File: "lib.py", Bytes: [2]uint64{20, 30},
				Metadata: openSite(model.CallShapeMember, 0)},
			{Name: "finish", Kind: model.NodeMethod, File: "end.py", Bytes: [2]uint64{0, 30}},
		},
		Relations: []fixtureFact{
			{From: "main", Kind: model.RelCalls, To: "first", Occurrences: site(20)},
			{From: "handle", Kind: model.RelCalls, To: "second", Occurrences: site(20)},
		},
	})
	res, err := f.engine(t).ShortestPath(context.Background(), model.PathRequest{From: f.id(t, "main"),
		To: f.id(t, "finish"), Relations: []model.RelationKind{model.RelCalls}})
	if err != nil {
		t.Fatalf("path(main, finish): %v", err)
	}
	repo := f.binding.RepositoryID
	want := []model.RelationID{
		model.NewRelationID(repo, f.id(t, "main"), model.RelMayCall, f.id(t, "handle")),
		model.NewRelationID(repo, f.id(t, "handle"), model.RelMayCall, f.id(t, "finish"))}
	if len(res.Paths) != 1 || !res.Paths[0].Possible || !slices.Equal(res.Paths[0].Relations, want) ||
		res.Paths[0].CostUnits != 2*Cost(model.RelMayCall) {
		t.Fatalf("path(main, finish) = %+v, want the two name-only hops %v", res.Paths, want)
	}
	counted := false
	for _, n := range res.Meta.Notices {
		if strings.Contains(n, "takes 2 calls no analysis bound") {
			counted = true
		}
	}
	if !counted {
		t.Errorf("notices %q do not count the route's two name-only hops", res.Meta.Notices)
	}
}
