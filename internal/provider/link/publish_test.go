package link

import (
	"context"
	"encoding/json"
	"slices"
	"testing"

	"github.com/Sawmonabo/codectx/internal/model"
	"github.com/Sawmonabo/codectx/internal/pagination"
	"github.com/Sawmonabo/codectx/internal/provider"
	"github.com/Sawmonabo/codectx/internal/provider/treesitter/wire"
)

// The publication tests hand the publisher tables and facts named by
// identity, as a resolution leaves them, and read what reaches the sink.

// span is a one-line range [start,end).
func span(start, end uint64) model.SourceRange {
	return model.SourceRange{Start: model.Position{Byte: start, Line: 1, Column: uint32(start)},
		End: model.Position{Byte: end, Line: 1, Column: uint32(end)}}
}

// node is an endpoint of the structural node id, declared at [start,end).
func node(id string, kind model.NodeKind, name string, start uint64) Endpoint {
	return Endpoint{Node: model.NodeID(id), CanonicalKey: "key:" + id, Kind: kind, Name: name, Qualified: name,
		Range: span(start, start+uint64(len(name)))}
}

// table is a member's table of path, with its module node "mod:"+path.
func table(path, language string, decls []Decl, sites []Site) *Table {
	return &Table{Path: path, Language: language, Module: node("mod:"+path, model.NodeModule, path, 0),
		Decls: decls, Sites: sites}
}

// declared is a declaration of the node id.
func declared(id string, kind model.NodeKind, name string, start uint64, params ...Parameter) Decl {
	return Decl{Endpoint: node(id, kind, name, start), Parent: -1, Described: true, Parameters: params}
}

// callAt is a call site of caller whose callee token is at [start,end).
func callAt(caller int32, callee string, shape wire.Shape, start uint64, args ...Argument) Site {
	end := start + uint64(len(callee))
	return Site{Caller: caller, Range: span(start, end+2), NameStart: uint32(start), NameEnd: uint32(end), Callee: callee,
		Shape: shape, Described: true, Arguments: args, Args: make([]wire.Type, len(args))}
}

// testUnit is a unit over tables, each a member whose FileID is its path.
func testUnit(t *testing.T, tables ...*Table) *unit {
	t.Helper()
	ft := fakeTables{}
	for _, tb := range tables {
		ft[tb.Path] = tb
	}
	return &unit{tables: ft, dir: t.TempDir(), members: func(yield func(model.FileVersion) error) error {
		for _, tb := range tables {
			if err := yield(model.FileVersion{ID: model.FileID(tb.Path), Path: tb.Path, Language: tb.Language,
				ContentHash: "hash:" + tb.Path}); err != nil {
				return err
			}
		}
		return nil
	}}
}

// testResolver mints every candidate's node from its scope, native key,
// kind and qualified name alone, as the store's resolver keys a candidate
// with no file: the same candidate is the same node in every unit.
type testResolver struct{}

func (testResolver) Resolve(_ context.Context, c model.NodeCandidate) (model.Resolution, error) {
	key := model.H("link.test_node", c.ScopeKey, c.NativeKey, string(c.Kind), c.QualifiedName)
	return model.Resolution{Node: model.Node{ID: model.NodeID(key), Kind: c.Kind, Language: c.Language, Name: c.Name,
		QualifiedName: c.QualifiedName}, Basis: model.MatchUnresolved, CanonicalKey: key}, nil
}

// testSink records what a unit publishes and fails the test for a relation
// that reaches it before a fact of each node it names.
type testSink struct {
	t     *testing.T
	nodes map[model.NodeID]model.NodeFact
	rels  []model.RelationFact
}

func (s *testSink) PutNodes(_ context.Context, facts []model.NodeFact) error {
	for _, f := range facts {
		s.nodes[f.Node.ID] = f
	}
	return nil
}

func (s *testSink) PutRelations(_ context.Context, facts []model.RelationFact) error {
	for _, f := range facts {
		for _, id := range []model.NodeID{f.Relation.From, f.Relation.To} {
			if _, ok := s.nodes[id]; !ok {
				s.t.Errorf("a %s relation names node %s before any fact of it reached the sink", f.Relation.Kind, id)
			}
		}
	}
	s.rels = append(s.rels, facts...)
	return nil
}

func (s *testSink) PutAliases(context.Context, []model.NativeAlias) error    { return nil }
func (s *testSink) PutSearchUnits(context.Context, []model.SearchUnit) error { return nil }
func (s *testSink) edges(kind model.RelationKind) (out []model.RelationFact) {
	for _, r := range s.rels {
		if r.Relation.Kind == kind {
			out = append(out, r)
		}
	}
	return out
}

// published is what one publication handed its sink, with its records and
// capability rows.
type published struct {
	*testSink
	records []record
	rows    []model.CapabilityState
}

// row is the capability row of capability.
func (p published) row(t *testing.T, capability model.RelationKind) model.CapabilityState {
	t.Helper()
	for _, r := range p.rows {
		if r.Capability == string(capability) {
			return r
		}
	}
	t.Fatalf("no %s capability row among %v", capability, p.rows)
	return model.CapabilityState{}
}

// sortedRun is vs as a sorted run of order in dir.
func sortedRun[T any](t *testing.T, s *pagination.ExternalSort[T], err error, vs []T) *pagination.SortedRun[T] {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	for _, v := range vs {
		if err := s.Add(v); err != nil {
			t.Fatal(err)
		}
	}
	run, err := s.Sorted()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { run.Close() })
	return run
}

// publishFacts publishes facts over u as unit unitID: every member, or, with
// republished set, those members alone, the others keeping prev's records.
func publishFacts(t *testing.T, u *unit, unitID string, facts []idBinding, republished []model.FileID, prev []record) published {
	t.Helper()
	s, err := newIDBindingSort(u.dir)
	bindings := sortedRun(t, s, err, facts)
	targets, err := targetsOf(u.dir, bindings)
	if err != nil {
		t.Fatal(err)
	}
	defer targets.Close()
	externals, err := externalsOf(u.dir, bindings)
	if err != nil {
		t.Fatal(err)
	}
	defer externals.Close()
	sink := &testSink{t: t, nodes: map[model.NodeID]model.NodeFact{}}
	req := provider.UnitRequest{Unit: model.UnitSpec{ID: model.UnitID(model.H("link.test_unit", unitID)), ProviderID: ProviderID,
		ProviderVersion: Version, ScopeKey: "test:" + unitID}, Resolver: testResolver{}}
	p := &publisher{ctx: context.Background(), req: req, sink: sink, u: u}
	if republished != nil {
		f, err := newFileIDSort(u.dir)
		p.republished = sortedRun(t, f, err, republished)
		r, err := newRecordSort(u.dir)
		p.prev = sortedRun(t, r, err, prev)
	}
	records, err := p.publish(bindings, targets, externals)
	if err != nil {
		t.Fatalf("publish: %v", err)
	}
	defer records.Close()
	out := published{testSink: sink}
	if err := records.Each(func(r record) error {
		out.records = append(out.records, r)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	out.rows = p.capabilities(records.Len())
	return out
}

// callFact is a call fact of site site of file to the declaration target of
// file to, or, with to empty, to the external key target.
func callFact(file string, site int32, to, target string, dispatch model.Dispatch) idBinding {
	b := idBinding{Kind: FactCall, SiteFile: model.FileID(file), Site: site, Detail: DetailHierarchy, Dispatch: dispatch}
	if to == "" {
		b.External = target
	} else {
		b.TargetFile, b.Target = model.FileID(to), model.NodeID(target)
	}
	return b
}

// TestSeveralTargetsPublishOneCallEach publishes a site bound to two
// declarations and a site bound to one: the first publishes one `calls` per
// target, each with its own dispatch and set size 2; the second one `calls`
// with no set size.
//
// Mutation: publish a site's first target alone -> one `calls` from the
// several-target site. Mutation: set the set size from the facts rather than
// the distinct targets -> a repeated target counts twice.
func TestSeveralTargetsPublishOneCallEach(t *testing.T) {
	a := table("a.py", "python",
		[]Decl{declared("run", model.NodeFunction, "run", 0), declared("Sq.area", model.NodeMethod, "area", 20),
			declared("Ci.area", model.NodeMethod, "area", 40)},
		[]Site{callAt(0, "area", wire.ShapeMember, 5), callAt(0, "area", wire.ShapeMember, 12)})
	u := testUnit(t, a)
	got := publishFacts(t, u, "u", []idBinding{
		callFact("a.py", 0, "a.py", "Sq.area", model.DispatchExact),
		callFact("a.py", 0, "a.py", "Ci.area", model.DispatchVirtual),
		callFact("a.py", 1, "a.py", "Sq.area", model.DispatchExact),
	}, nil, nil)
	byTarget := map[model.NodeID][]model.Evidence{}
	for _, r := range got.edges(model.RelCalls) {
		if r.Relation.From != "run" {
			t.Errorf("a call publishes from %s, want the caller run", r.Relation.From)
		}
		byTarget[r.Relation.To] = append(byTarget[r.Relation.To], r.Evidence...)
	}
	sq, ci := byTarget["Sq.area"], byTarget["Ci.area"]
	if len(sq) != 2 || len(ci) != 1 {
		t.Fatalf("Sq.area has %d occurrences and Ci.area %d, want 2 and 1", len(sq), len(ci))
	}
	if ci[0].SetSize != 2 || ci[0].Dispatch != model.DispatchVirtual {
		t.Errorf("the several-target site's call of Ci.area carries set size %d dispatch %q, want 2 virtual", ci[0].SetSize, ci[0].Dispatch)
	}
	sets := []int64{sq[0].SetSize, sq[1].SetSize}
	slices.Sort(sets)
	if !slices.Equal(sets, []int64{0, 2}) {
		t.Errorf("the calls of Sq.area carry set sizes %v, want 0 (the one-target site) and 2", sets)
	}
	if row := got.row(t, model.RelCalls); row.Details["bound_sites"] != "2" || row.Details["sites"] != "2" {
		t.Errorf("the calls row is %v, want two sites, both bound", row.Details)
	}
}

// TestUnboundSitesPublishOpenSites publishes a member site and a computed
// site that no fact binds, and a member site a FactOpenSite gives its own
// reason: each publishes one open-site node at the site, with the reason its
// shape gives or the fact's, its shape and argument count, and the caller's
// `calls` to it.
//
// Mutation: drop Computed sites from the open sites -> the computed site
// publishes no node. Mutation: ignore a FactOpenSite's reason -> the third
// site's reason is receiver_untyped.
func TestUnboundSitesPublishOpenSites(t *testing.T) {
	member := callAt(0, "area", wire.ShapeMember, 5, Argument{Range: span(10, 11)}, Argument{Range: span(12, 13)})
	computed := Site{Caller: 0, Range: span(20, 30), Callee: "obj[k]", Shape: wire.ShapeIndirect, Computed: true}
	other := callAt(0, "size", wire.ShapeMember, 40)
	a := table("a.js", "javascript", []Decl{declared("run", model.NodeFunction, "run", 0)},
		[]Site{member, computed, other})
	u := testUnit(t, a)
	got := publishFacts(t, u, "u", []idBinding{
		{Kind: FactOpenSite, SiteFile: "a.js", Site: 2, Reason: model.ReasonValueUntracked},
	}, nil, nil)
	want := map[string]model.OpenSiteMetadata{
		"area":   {Resolution: model.ResolutionUnresolved, Reason: model.ReasonReceiverUntyped, Shape: model.CallShapeMember, Arguments: 2},
		"obj[k]": {Resolution: model.ResolutionUnresolved, Reason: model.ReasonComputed, Shape: model.CallShapeValue},
		"size":   {Resolution: model.ResolutionUnresolved, Reason: model.ReasonValueUntracked, Shape: model.CallShapeMember},
	}
	calls := got.edges(model.RelCalls)
	if len(calls) != len(want) {
		t.Fatalf("the unit publishes %d calls, want one to each of %d open sites", len(calls), len(want))
	}
	seen := map[string]bool{}
	for _, r := range calls {
		n := got.nodes[r.Relation.To].Node
		var m model.OpenSiteMetadata
		if err := json.Unmarshal(n.Metadata, &m); err != nil {
			t.Fatalf("open site %s metadata %s: %v", n.Name, n.Metadata, err)
		}
		if err := m.Validate(); err != nil {
			t.Errorf("open site %s: %v", n.Name, err)
		}
		if w, ok := want[n.Name]; !ok || m != w {
			t.Errorf("open site %s has metadata %+v, want %+v", n.Name, m, w)
		}
		if r.Relation.From != "run" || n.FileID != "a.js" || n.Range == nil {
			t.Errorf("open site %s is called from %s and located in %q at %v, want called from run and located in a.js",
				n.Name, r.Relation.From, n.FileID, n.Range)
		}
		seen[n.Name] = true
	}
	if len(seen) != len(want) {
		t.Errorf("the open sites are %v, want %d distinct", seen, len(want))
	}
}

// TestSiteCountsAddUp publishes one site of each kind: bound to a
// declaration, an external call fact, a FactExternalSite, open, computed,
// and a site with both a declaration and an external target. The `calls`
// row counts each once: sites = bound + open + computed + external.
//
// Mutation: count computed sites as open too -> open_sites is 2 and the sum
// is one more than sites. Mutation: count a site with an external call fact
// and a declaration target as external -> bound_sites is 1.
func TestSiteCountsAddUp(t *testing.T) {
	ext := externalMethod("java", "java.util.List", "add")
	a := table("A.java", "java", []Decl{declared("A.run", model.NodeMethod, "run", 0), declared("A.go", model.NodeMethod, "go", 10)},
		[]Site{callAt(0, "go", wire.ShapeBare, 20), callAt(0, "add", wire.ShapeMember, 30), callAt(0, "println", wire.ShapeMember, 40),
			callAt(0, "x", wire.ShapeMember, 50), {Caller: 0, Range: span(60, 70), Callee: "f()", Computed: true},
			callAt(0, "go", wire.ShapeMember, 80)})
	u := testUnit(t, a)
	got := publishFacts(t, u, "u", []idBinding{
		callFact("A.java", 0, "A.java", "A.go", model.DispatchExact),
		callFact("A.java", 1, "", ext, model.DispatchVirtual),
		{Kind: FactExternalSite, SiteFile: "A.java", Site: 2},
		callFact("A.java", 5, "A.java", "A.go", model.DispatchVirtual),
		callFact("A.java", 5, "", ext, model.DispatchVirtual),
	}, nil, nil)
	want := map[string]string{"sites": "6", "bound_sites": "2", "external_sites": "2", "open_sites": "1", "computed_sites": "1"}
	row := got.row(t, model.RelCalls)
	for k, v := range want {
		if row.Details[k] != v {
			t.Errorf("the calls row's %s is %q, want %q (details %v)", k, row.Details[k], v, row.Details)
		}
	}
	if row.State != model.CapabilityFresh {
		t.Errorf("the calls row is %s with open sites, want fresh: an open site is a fact, not missing coverage", row.State)
	}
	if len(got.rows) != len(Capabilities) {
		t.Errorf("the unit reports %d capability rows, want one per capability (%v)", len(got.rows), Capabilities)
	}
}

// TestExternalNodeIsOneNodeAndAlwaysPublished publishes a call from one file
// and an override from another to one external method, in two units, then
// refreshes the first unit republishing only the overriding file, so the
// calling file, the only other file naming the node, is carried. The node
// is the same in both files and both units, has no file and is published on
// the refresh.
//
// Mutation: mint the external node in the unit's scope -> the two units'
// nodes differ. Mutation: publish an external node only from the republished
// files' facts -> the refresh publishes none, and the carried call names a
// node the unit has no fact for.
func TestExternalNodeIsOneNodeAndAlwaysPublished(t *testing.T) {
	ext := externalMethod("java", "java.lang.Runnable", "run")
	tables := func() []*Table {
		return []*Table{
			table("A.java", "java", []Decl{declared("A.main", model.NodeMethod, "main", 0)},
				[]Site{callAt(0, "run", wire.ShapeMember, 10)}),
			table("B.java", "java", []Decl{declared("B", model.NodeClass, "B", 0), declared("B.run", model.NodeMethod, "run", 10)}, nil),
		}
	}
	facts := []idBinding{
		callFact("A.java", 0, "", ext, model.DispatchVirtual),
		{Kind: FactOverrides, SiteFile: "B.java", Site: 1, External: ext, Detail: DetailSupertype},
	}
	external := func(p published) model.NodeID {
		t.Helper()
		var ids []model.NodeID
		for id, f := range p.nodes {
			if f.Node.QualifiedName == "java.lang.Runnable.run" {
				ids = append(ids, id)
				if f.Node.FileID != "" || len(f.Evidence) != 1 || f.Evidence[0].FileID != "" {
					t.Errorf("the external node is in file %q with evidence %+v, want no file", f.Node.FileID, f.Evidence)
				}
				if string(f.Node.Metadata) != `{"resolution":"external"}` {
					t.Errorf("the external node's metadata is %s", f.Node.Metadata)
				}
			}
		}
		if len(ids) != 1 {
			t.Fatalf("the unit publishes %d external nodes for java.lang.Runnable.run, want one", len(ids))
		}
		return ids[0]
	}
	first := publishFacts(t, testUnit(t, tables()...), "one", facts, nil, nil)
	id := external(first)
	for _, kind := range []model.RelationKind{model.RelCalls, model.RelOverrides} {
		edges := first.edges(kind)
		if len(edges) != 1 || edges[0].Relation.To != id {
			t.Errorf("the %s edges are %+v, want one to the external node %s", kind, edges, id)
		}
	}
	second := publishFacts(t, testUnit(t, tables()...), "two", facts, nil, nil)
	if got := external(second); got != id {
		t.Errorf("the second unit's external node is %s, the first's %s: one external method is one node", got, id)
	}
	refresh := publishFacts(t, testUnit(t, tables()...), "one", facts, []model.FileID{"B.java"}, first.records)
	if got := external(refresh); got != id {
		t.Errorf("the refresh publishes external node %s, want %s", got, id)
	}
	if n := len(refresh.edges(model.RelCalls)); n != 0 {
		t.Errorf("the refresh publishes %d calls from the carried file, want none", n)
	}
}

// TestImportsPublishWithSetSize publishes an import resolved to two
// members' modules and a dynamic import: two `imports` from the file's
// module, each with set size 2 and located at the import, one open import
// site with its fragments and the module's `imports` to it, and the
// `imports` row's counts.
//
// Mutation: publish the first resolved file alone -> one `imports`.
// Mutation: leave the fragments nil when there are none -> the metadata
// spells null.
func TestImportsPublishWithSetSize(t *testing.T) {
	a := table("a.c", "c", nil, nil)
	a.Imports = []Import{{Path: "util.h", Scope: -1, Range: span(0, 18)}}
	a.DynamicImports = []DynamicImport{{Caller: -1, Range: span(20, 36), Fragments: []string{"cfg/", ".h"}},
		{Caller: -1, Range: span(40, 50)}}
	b, c := table("x/util.h", "c", nil, nil), table("y/util.h", "c", nil, nil)
	u := testUnit(t, a, b, c)
	imp := func(to string) idBinding {
		return idBinding{Kind: FactImport, SiteFile: "a.c", Site: 0, TargetFile: model.FileID(to),
			Target: model.NodeID("mod:" + to), Detail: DetailImport}
	}
	got := publishFacts(t, u, "u", []idBinding{imp("x/util.h"), imp("y/util.h")}, nil, nil)
	var resolved, open []model.RelationFact
	for _, r := range got.edges(model.RelImports) {
		if r.Relation.From != "mod:a.c" {
			t.Errorf("an import publishes from %s, want the file's module", r.Relation.From)
		}
		if got.nodes[r.Relation.To].Node.Metadata != nil {
			open = append(open, r)
		} else {
			resolved = append(resolved, r)
		}
	}
	if len(resolved) != 2 {
		t.Fatalf("the import publishes %d imports, want one to each of 2 modules", len(resolved))
	}
	for _, r := range resolved {
		e := r.Evidence[0]
		if e.SetSize != 2 || e.Range == nil || e.Range.Start.Byte != 0 || e.Detail != DetailImport {
			t.Errorf("an import's evidence is %+v, want set size 2 at the import with detail import", e)
		}
	}
	frags := map[string]bool{}
	for _, r := range open {
		var m model.OpenImportSiteMetadata
		raw := got.nodes[r.Relation.To].Node.Metadata
		if err := json.Unmarshal(raw, &m); err != nil || m.Validate() != nil || m.Fragments == nil {
			t.Errorf("an open import site's metadata is %s, want a valid one with a fragment list", raw)
		}
		frags[string(mustJSON(t, m.Fragments))] = true
	}
	if !frags[`["cfg/",".h"]`] || !frags[`[]`] {
		t.Errorf("the open import sites' fragments are %v, want [cfg/ .h] and []", frags)
	}
	row := got.row(t, model.RelImports)
	if row.Details["resolved_imports"] != "1" || row.Details["open_import_sites"] != "2" {
		t.Errorf("the imports row is %v, want one resolved import and two open import sites", row.Details)
	}
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// TestFlowPublishesBothEndpoints publishes a flow from an argument that
// reads two variables into a parameter declared in another file: one
// `data_flows_to` from each read variable to the parameter, with the read
// variables' occurrences in the site's file and the parameter's in the
// callee's, each handed to the sink before the edge.
//
// Mutation: skip the parameter's occurrence in the targets merge -> the
// sink sees an edge to a node with no fact. Mutation: publish the read
// variables' occurrences in the callee's file -> they are located in b.py.
func TestFlowPublishesBothEndpoints(t *testing.T) {
	param := Parameter{Endpoint: node("f.x", model.NodeVariable, "x", 6), Index: 1}
	b := table("b.py", "python", []Decl{declared("f", model.NodeFunction, "f", 0, param)}, nil)
	arg := Argument{Range: span(12, 17), Reads: []Endpoint{node("a.v", model.NodeVariable, "v", 12), node("a.w", model.NodeVariable, "w", 16)}}
	a := table("a.py", "python", []Decl{declared("main", model.NodeFunction, "main", 0)},
		[]Site{callAt(0, "f", wire.ShapeBare, 10, arg)})
	u := testUnit(t, a, b)
	got := publishFacts(t, u, "u", []idBinding{
		callFact("a.py", 0, "b.py", "f", model.DispatchExact),
		{Kind: FactFlow, SiteFile: "a.py", Site: 0, Arg: 0, TargetFile: "b.py", Target: "f.x", Detail: DetailArgument},
	}, nil, nil)
	flows := got.edges(model.RelDataFlowsTo)
	var from []string
	for _, r := range flows {
		if r.Relation.To != "f.x" {
			t.Errorf("a flow publishes to %s, want the parameter f.x", r.Relation.To)
		}
		from = append(from, string(r.Relation.From))
		if v := got.nodes[r.Relation.From].Node; v.FileID != "a.py" {
			t.Errorf("the read variable %s is located in %q, want the site's file a.py", v.ID, v.FileID)
		}
	}
	slices.Sort(from)
	if !slices.Equal(from, []string{"a.v", "a.w"}) {
		t.Errorf("the flows publish from %v, want a.v and a.w", from)
	}
	if p := got.nodes["f.x"].Node; p.FileID != "b.py" {
		t.Errorf("the parameter's occurrence is located in %q, want the callee's file b.py", p.FileID)
	}
}

// TestRecordCountsCarry refreshes a unit republishing one member and
// carrying the other: the rows sum the carried member's counts from its
// record without its table.
//
// Mutation: count only the republished members -> sites is 1.
func TestRecordCountsCarry(t *testing.T) {
	a := table("a.go", "go", []Decl{declared("A", model.NodeFunction, "A", 0)}, []Site{callAt(0, "A", wire.ShapeBare, 5)})
	b := table("b.go", "go", []Decl{declared("B", model.NodeFunction, "B", 0)}, []Site{callAt(0, "C", wire.ShapeBare, 5)})
	carried := testUnit(t, a, b)
	// The carried member's table is not there to read: its counts can come
	// from its record alone.
	delete(carried.tables.(fakeTables), "b.go")
	prev := []record{{File: "b.go", Content: "hash:b.go", Tabled: true, Counts: counts{Sites: 1, Open: 1}}}
	got := publishFacts(t, carried, "u", []idBinding{callFact("a.go", 0, "a.go", "A", model.DispatchExact)},
		[]model.FileID{"a.go"}, prev)
	row := got.row(t, model.RelCalls)
	for k, v := range map[string]string{"sites": "2", "bound_sites": "1", "open_sites": "1"} {
		if row.Details[k] != v {
			t.Errorf("the calls row's %s is %q, want %q: the carried record's counts are summed (details %v)", k, row.Details[k], v, row.Details)
		}
	}
}

// TestOccurrencesCarryTheNameRange publishes a call between two
// declarations whose tables carry each name token's range: the linker's
// occurrence of each endpoint carries that range as its NameRange, so an
// answer served from the linker's occurrence locates the name, not only the
// declaration.
//
// Mutation: leave NameRange off occurrenceFact -> both endpoints publish
// without it.
func TestOccurrencesCarryTheNameRange(t *testing.T) {
	named := func(d Decl, start, end uint64) Decl {
		r := span(start, end)
		d.NameRange = &r
		return d
	}
	callee := named(declared("f", model.NodeFunction, "f", 0), 4, 5)
	callee.Range = span(0, 20)
	caller := named(declared("main", model.NodeFunction, "main", 0), 4, 8)
	caller.Range = span(0, 30)
	b := table("b.py", "python", []Decl{callee}, nil)
	a := table("a.py", "python", []Decl{caller}, []Site{callAt(0, "f", wire.ShapeBare, 10)})
	got := publishFacts(t, testUnit(t, a, b), "u", []idBinding{callFact("a.py", 0, "b.py", "f", model.DispatchExact)}, nil, nil)
	for _, c := range []struct {
		id         model.NodeID
		start, end uint64
	}{{"main", 4, 8}, {"f", 4, 5}} {
		n, ok := got.nodes[c.id]
		if !ok {
			t.Fatalf("no occurrence of %s was published", c.id)
		}
		if n.Node.NameRange == nil {
			t.Fatalf("the occurrence of %s carries no name range", c.id)
		}
		if r := *n.Node.NameRange; r.Start.Byte != c.start || r.End.Byte != c.end {
			t.Errorf("the occurrence of %s names bytes [%d,%d), want [%d,%d)", c.id, r.Start.Byte, r.End.Byte, c.start, c.end)
		}
	}
}
