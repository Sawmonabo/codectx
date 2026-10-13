package link_test

import (
	"context"
	"encoding/json"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/Sawmonabo/codectx/internal/admission"
	srclang "github.com/Sawmonabo/codectx/internal/lang"
	"github.com/Sawmonabo/codectx/internal/model"
	"github.com/Sawmonabo/codectx/internal/process"
	"github.com/Sawmonabo/codectx/internal/provider"
	"github.com/Sawmonabo/codectx/internal/provider/link"
	"github.com/Sawmonabo/codectx/internal/provider/providertest"
	"github.com/Sawmonabo/codectx/internal/provider/treesitter"
	"github.com/Sawmonabo/codectx/internal/provider/treesitter/lang"
	"github.com/Sawmonabo/codectx/internal/provider/treesitter/wire"
	"github.com/Sawmonabo/codectx/internal/provider/treesitter/worker"
	"github.com/Sawmonabo/codectx/internal/testenv"
)

// TestMain makes this test binary its own parser worker, as the codectx
// binary is in production.
func TestMain(m *testing.M) {
	if len(os.Args) > 1 && os.Args[1] == wire.Subcommand {
		os.Exit(worker.Main(context.Background(), os.Stdin, os.Stdout, os.Stderr))
	}
	done := testenv.GoHost()
	code := m.Run()
	done()
	os.Exit(code)
}

// linkCases is one source per grammar. Each holds one indirect site, a call
// through a value that names no declaration (a function value, a function
// pointer, a call through an interface parameter with one implementation),
// and one several-target site: a receiver whose type flows from two
// constructions (two `calls`, one per class's method, each with set size 2),
// a call through an interface or base type (one `calls`, to the method the
// type declares, virtual), or a function pointer two address-taken functions
// of its signature can hold (two `calls`, exact, set size 2).
var linkCases = []struct {
	name string
	// anchor is the language reference the indirect and ambiguous answers
	// are read from.
	anchor string
	files  map[string]string
	// indirect is the indirect site's caller and the declaration it reaches.
	indirect [2]string
	// several is the several-target site's caller and the name every target
	// carries; targets how many `calls` it publishes, and dispatch theirs.
	several  [2]string
	targets  int
	dispatch model.Dispatch
}{
	// Derived twice from the source and the anchor, the second time blind (the source,
	// the anchor and the binding rules alone); the two agree on both sites.
	{name: "go", anchor: `The Go Programming Language Specification, "Calls", "Function types", "Interface types", "Method sets"`, files: map[string]string{"calls.go": `package p

type Shape interface{ Area() int }

type Square struct{}

func (Square) Area() int { return 4 }

type Circle struct{}

func (Circle) Area() int { return 3 }

func ambiguous(s Shape) int { return s.Area() }

func seven(n int) int { return n + 7 }

func indirect() int {
	var f func(int) int = seven
	return f(1)
}
`}, indirect: [2]string{"indirect", "seven"}, several: [2]string{"ambiguous", "Area"}, targets: 1, dispatch: model.DispatchVirtual},
	// Derived twice from the source and the anchor, the second time blind (the source,
	// the anchor and the binding rules alone); the two agree on both sites.
	{name: "java", anchor: `Java Language Specification §15.12.4.4 (Locate Method to Invoke), §9.8 (Functional Interfaces)`, files: map[string]string{"Calls.java": `interface Shape { int area(); }
class Square implements Shape { public int area() { return 4; } }
class Circle implements Shape { public int area() { return 3; } }
interface Op { int run(int x); }
class Twice implements Op { public int run(int x) { return 2 * x; } }
class Calls {
  int ambiguous(Shape s) { return s.area(); }
  int indirect(Op op) { return op.run(1); }
}
`}, indirect: [2]string{"indirect", "run"}, several: [2]string{"ambiguous", "area"}, targets: 1, dispatch: model.DispatchVirtual},
	// Derived twice from the source and the anchor, the second time blind (the source,
	// the anchor and the binding rules alone); the two agree on both sites.
	{name: "javascript", anchor: `ECMA-262 §13.3.6 (Function Calls), §13.3.2 (Property Accessors)`, files: map[string]string{"calls.js": `class Square { area() { return 4; } }
class Circle { area() { return 3; } }
function seven(n) { return n + 7; }
function ambiguous(flag) {
  let s = new Square();
  if (flag) { s = new Circle(); }
  return s.area();
}
function indirect() {
  const f = seven;
  return f(1);
}
`}, indirect: [2]string{"indirect", "seven"}, several: [2]string{"ambiguous", "area"}, targets: 2, dispatch: model.DispatchExact},
	// Derived twice from the source and the anchor, the second time blind (the source,
	// the anchor and the binding rules alone); the two agree on both sites. The
	// TypeScript compiler (strict) reports no diagnostic; it resolves `f(1)` to seven
	// and `s.area()` to Square's area only, the type it infers from s's initializer,
	// to which a Circle is assignable. s carries no written type, so both classes'
	// methods are its targets by value flow (ADR-0015 decision 1).
	{name: "typescript", anchor: `ECMA-262 §13.3.6 (Function Calls), §13.3.2 (Property Accessors)`, files: map[string]string{"calls.ts": `class Square { area(): number { return 4; } }
class Circle { area(): number { return 3; } }
function seven(n: number): number { return n + 7; }
function ambiguous(flag: boolean): number {
  let s = new Square();
  if (flag) { s = new Circle(); }
  return s.area();
}
function indirect(): number {
  const f = seven;
  return f(1);
}
`}, indirect: [2]string{"indirect", "seven"}, several: [2]string{"ambiguous", "area"}, targets: 2, dispatch: model.DispatchExact},
	// Derived twice from the source and the anchor, the second time blind (the source,
	// the anchor and the binding rules alone); the two agree on both sites. The
	// TypeScript compiler (strict) reports no diagnostic; it resolves `f(1)` to seven
	// and `s.area()` to Square's area only, the type it infers from s's initializer,
	// to which a Circle is assignable. s carries no written type, so both classes'
	// methods are its targets by value flow (ADR-0015 decision 1).
	{name: "tsx", anchor: `ECMA-262 §13.3.6 (Function Calls), §13.3.2 (Property Accessors)`, files: map[string]string{"calls.tsx": `class Square { area(): number { return 4; } }
class Circle { area(): number { return 3; } }
function seven(n: number): number { return n + 7; }
function ambiguous(flag: boolean): number {
  let s = new Square();
  if (flag) { s = new Circle(); }
  return s.area();
}
function indirect(): number {
  const f = seven;
  return f(1);
}
`}, indirect: [2]string{"indirect", "seven"}, several: [2]string{"ambiguous", "area"}, targets: 2, dispatch: model.DispatchExact},
	// Derived twice from the source and the anchor, the second time blind (the source,
	// the anchor and the binding rules alone); the two agree on both sites.
	{name: "python", anchor: `The Python Language Reference §6.3.4 (Calls), §3.3 (attribute lookup on a class)`, files: map[string]string{"calls.py": `class Square:
    def area(self):
        return 4


class Circle:
    def area(self):
        return 3


def seven(n):
    return n + 7


def ambiguous(flag):
    s = Square()
    if flag:
        s = Circle()
    return s.area()


def indirect():
    f = seven
    return f(1)
`}, indirect: [2]string{"indirect", "seven"}, several: [2]string{"ambiguous", "area"}, targets: 2, dispatch: model.DispatchExact},
	// Derived twice from the source and the anchor, the second time blind (the source,
	// the anchor and the binding rules alone); the two agree on both sites.
	{name: "rust", anchor: `The Rust Reference, "Call expressions" (function pointer types), "Method-call expressions", "Trait objects"`, files: map[string]string{
		"Cargo.toml": "[package]\nname = \"calls\"\nversion = \"0.1.0\"\nedition = \"2021\"\n",
		"src/lib.rs": `pub trait Shape {
    fn area(&self) -> i32;
}

pub struct Square;

impl Shape for Square {
    fn area(&self) -> i32 { 4 }
}

pub struct Circle;

impl Shape for Circle {
    fn area(&self) -> i32 { 3 }
}

pub fn ambiguous(s: &dyn Shape) -> i32 {
    s.area()
}

fn seven(n: i32) -> i32 {
    n + 7
}

pub fn indirect() -> i32 {
    let f: fn(i32) -> i32 = seven;
    f(1)
}
`}, indirect: [2]string{"indirect", "seven"}, several: [2]string{"ambiguous", "area"}, targets: 1, dispatch: model.DispatchVirtual},
	// Derived twice from the source and the anchor, the second time blind (the source,
	// the anchor and the binding rules alone); the two agree on both sites.
	{name: "c", anchor: `ISO C §6.5.2.2 (Function calls through a pointer-to-function), §6.2.2 (Linkages of identifiers)`, files: map[string]string{"calls.c": `static int seven(int n) { return n + 7; }
static int eight(int n) { return n + 8; }
static long wide(long n) { return n; }

int ambiguous(int which) {
  int (*g)(int) = seven;
  if (which) { g = eight; }
  return g(2);
}

int indirect(void) {
  long (*f)(long) = wide;
  return (int)f(1);
}
`}, indirect: [2]string{"indirect", "wide"}, several: [2]string{"ambiguous", ""}, targets: 2, dispatch: model.DispatchExact},
	// Derived twice from the source and the anchor, the second time blind (the source,
	// the anchor and the binding rules alone); the two agree on both sites.
	{name: "cpp", anchor: `ISO C++ [expr.call], [class.virtual]`, files: map[string]string{"calls.cpp": `struct Shape { virtual int area() = 0; };
struct Square : Shape { int area() override { return 4; } };
struct Circle : Shape { int area() override { return 3; } };

int ambiguous(Shape* s) { return s->area(); }

static long wide(long n) { return n; }

int indirect() {
  long (*f)(long) = wide;
  return (int)f(1);
}
`}, indirect: [2]string{"indirect", "wide"}, several: [2]string{"ambiguous", "area"}, targets: 1, dispatch: model.DispatchVirtual},
}

// TestCallsArriveForEveryGrammar runs the real structural provider over one
// source per grammar, then the linking provider over the unit the project
// plan gives that source, and asserts what it published.
//
// Failure modes: `calls` missing for a language whose worker tables and
// resolver both exist; a call through a declared type published to every
// override or to a stand-in node instead of the one method the type
// declares; a several-target site published without its set size or with a
// target dropped; a relation published with an endpoint the unit has no node
// fact for, which a generation refuses; a capability row claiming fresh
// coverage it does not have.
//
// Mutation: publish a site's first binding alone (publish.go) -> the
// several-target sites of JavaScript, TypeScript, TSX, Python and C publish
// one target. Second mutation: skip the target occurrence nodes -> every case
// fails the endpoint check. Third mutation: walk a declared type's overrides
// at publication -> the Go, Java, Rust and C++ sites publish two targets.
func TestCallsArriveForEveryGrammar(t *testing.T) {
	structural := newStructural(t)
	for _, tc := range linkCases {
		t.Run(tc.name, func(t *testing.T) {
			l := linkSources(t, structural, tc.files)
			if got := l.out.calls(tc.indirect[0], tc.indirect[1]); len(got) != 1 {
				t.Errorf("the indirect site in %s publishes %d calls to %s, want one (%s)", tc.indirect[0], len(got), tc.indirect[1], tc.anchor)
			}
			got := l.out.calls(tc.several[0], tc.several[1])
			if len(got) != tc.targets {
				t.Fatalf("the several-target site in %s publishes %d calls, want %d (%s)", tc.several[0], len(got), tc.targets, tc.anchor)
			}
			for _, e := range got {
				want := int64(0)
				if tc.targets > 1 {
					want = int64(tc.targets)
				}
				if e.Dispatch != tc.dispatch || e.SetSize != want {
					t.Errorf("a call of the several-target site in %s carries dispatch %q set size %d, want %q %d",
						tc.several[0], e.Dispatch, e.SetSize, tc.dispatch, want)
				}
			}
		})
	}
}

// linked is what one linking unit published over a case's files.
type linked struct {
	h     *providertest.Harness
	files map[string]string
	out   *capture
	// structural is what the structural units handed their outputs.
	structural *capture
	// calls is the unit's `calls` capability row, and rows every row.
	calls model.CapabilityState
	rows  []model.CapabilityState
	// lost is each row's facts_without_node count, by capability, for the
	// rows that have one.
	lost map[string]string
}

// linkSources runs the real structural provider over every source file of
// files, each its own unit as the coordinator plans them, then the linking
// provider over the one unit the dependence plan gives those sources, and
// returns what it published after checking its capability row and that every
// relation names a node it published. Manifests (go.mod, Cargo.toml) are
// inputs of the linking unit, never structural units.
func linkSources(t *testing.T, structural *treesitter.Provider, files map[string]string) linked {
	t.Helper()
	ctx := context.Background()
	h := providertest.New(t, files)
	var sources []string
	for p := range files {
		if link.FamilyOf(srclang.Of(p)) != "" {
			sources = append(sources, p)
		}
	}
	slices.Sort(sources)
	if len(sources) == 0 {
		t.Fatal("the case has no source file")
	}
	var structIDs []model.UnitID
	structOut := &capture{}
	for _, p := range sources {
		su := h.Plan(t, structural, lang.ScopePrefix+p, []string{p})
		structOut.UnitOutput = h.Begin(t, su, []string{p})
		res, err := provider.RunUnit(ctx, structural, su.Request, stateCapture{structOut}, providertest.Limits, h.Pool)
		if cerr := h.Store.CompleteProviderRun(ctx, res, provider.CodeOf(err)); cerr != nil {
			t.Fatalf("CompleteProviderRun: %v", cerr)
		}
		if err != nil {
			t.Fatalf("structural unit of %s: %v", p, err)
		}
		if res.State != model.RunSucceeded {
			t.Fatalf("structural unit of %s ran %s", p, res.State)
		}
		structIDs = append(structIDs, su.Build.Spec.ID)
	}
	// Sealing attached the structural units to the staging generation, which
	// is where the linker reads their tables.
	linker := link.New(link.StoreTables(h.Store))
	plan, err := link.PlanUnits(ctx, h.View)
	if err != nil {
		t.Fatalf("PlanUnits: %v", err)
	}
	var scope string
	var inputs []string
	for _, pu := range plan.Units {
		if !pu.OwnsInput(sources[0]) {
			continue
		}
		scope = pu.ScopeKey
		for p := range files {
			if pu.OwnsInput(p) {
				inputs = append(inputs, p)
			}
		}
	}
	if scope == "" {
		t.Fatalf("the dependence plan gives %s no unit", sources[0])
	}
	for _, p := range sources {
		if !slices.Contains(inputs, p) {
			t.Fatalf("the dependence plan puts %s and %s in different units; a case is one unit", sources[0], p)
		}
	}
	slices.Sort(inputs)
	u := h.Plan(t, linker, scope, inputs, structIDs...)
	out := &capture{UnitOutput: h.Begin(t, u, inputs)}
	result, err := provider.RunUnit(ctx, linker, u.Request, out, providertest.Limits, h.Pool)
	if err != nil {
		t.Fatalf("linking unit: %v", err)
	}
	lost := checkCapability(t, result, scope)
	checkEndpoints(t, out)
	l := linked{h: h, files: files, out: out, structural: structOut, lost: lost, rows: result.Capabilities}
	for _, c := range result.Capabilities {
		if c.Capability == string(model.RelCalls) {
			l.calls = c
		}
	}
	return l
}

// newStructural is the structural provider with this binary as its worker.
func newStructural(t *testing.T) *treesitter.Provider {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	if exe, err = filepath.EvalSymlinks(exe); err != nil {
		t.Fatal(err)
	}
	runner, err := process.NewRunner(process.Limits{MaxConcurrent: 4, MemoryBudgetBytes: 4 << 30, DiskBudgetBytes: 1 << 30})
	if err != nil {
		t.Fatal(err)
	}
	room, err := admission.NewLedger(4<<30, 0)
	if err != nil {
		t.Fatal(err)
	}
	p, err := treesitter.New(treesitter.Options{
		MaxWorkers: 2, Rederive: func(int64) {}, Admission: room,
		Worker: treesitter.WorkerCommand{Path: exe, Args: []string{wire.Subcommand}},
		Runner: runner, WorkDir: t.TempDir(),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(p.Close)
	return p
}

// checkCapability asserts every capability row the linking unit reports is
// at the unit's scope and fresh (every member of a clean source has its
// table whole; an open site is disclosed by the counts, never missing), or
// partial for facts whose target has no node alone, whose counts it answers
// by capability; and that the `calls` row is among them with site counts
// that add up: sites = bound + open + computed + external.
func checkCapability(t *testing.T, r model.ProviderResult, scope string) map[string]string {
	t.Helper()
	var calls *model.CapabilityState
	lost := map[string]string{}
	for i, c := range r.Capabilities {
		if n, ok := c.Details["facts_without_node"]; ok && c.State == model.CapabilityPartial &&
			len(c.Details) == len(onlyLost(c.Details)) {
			lost[c.Capability] = n
			c.State = model.CapabilityFresh
		}
		if c.Scope != scope || c.State != model.CapabilityFresh {
			t.Fatalf("the linking unit reported %s %s at %q, want fresh at %q (details %v)", c.Capability, c.State, c.Scope, scope, c.Details)
		}
		if c.Capability == string(model.RelCalls) {
			calls = &r.Capabilities[i]
		}
	}
	if calls == nil {
		t.Fatalf("the linking unit reported no calls row among %d rows", len(r.Capabilities))
	}
	count := func(k string) int64 {
		v, err := strconv.ParseInt(calls.Details[k], 10, 64)
		if err != nil {
			t.Fatalf("the calls row has no count %s (details %v)", k, calls.Details)
		}
		return v
	}
	if sites, sum := count("sites"), count("bound_sites")+count("open_sites")+count("computed_sites")+count("external_sites"); sites != sum {
		t.Fatalf("the calls row counts %d sites and %d bound, open, computed and external (details %v)", sites, sum, calls.Details)
	}
	return lost
}

// onlyLost is the details of a row other than its own counts that a row
// partial for facts without a node alone carries.
func onlyLost(details map[string]string) map[string]string {
	out := map[string]string{}
	for k, v := range details {
		switch k {
		case "facts_without_node", "members", "sites", "bound_sites", "open_sites", "computed_sites", "external_sites",
			"resolved_imports", "open_import_sites":
			out[k] = v
		}
	}
	return out
}

// checkEndpoints asserts every relation's endpoints are nodes this unit
// published: the occurrences of the structural nodes, the open sites and
// external nodes it minted.
func checkEndpoints(t *testing.T, out *capture) {
	t.Helper()
	if len(out.rels) == 0 {
		t.Fatal("the linking unit published no relation")
	}
	for _, r := range out.rels {
		for _, id := range []model.NodeID{r.Relation.From, r.Relation.To} {
			if _, ok := out.nodes[id]; !ok {
				t.Fatalf("a %s relation names node %s, which the unit published no fact for", r.Relation.Kind, id)
			}
		}
	}
}

// capture records what the linking unit hands its output.
type capture struct {
	provider.UnitOutput
	nodes  map[model.NodeID]model.Node
	rels   []model.RelationFact
	search []model.SearchUnit
}

func (c *capture) PutSearchUnits(ctx context.Context, docs []model.SearchUnit) error {
	c.search = append(c.search, docs...)
	return c.UnitOutput.PutSearchUnits(ctx, docs)
}

func (c *capture) PutNodes(ctx context.Context, facts []model.NodeFact) error {
	if c.nodes == nil {
		c.nodes = map[model.NodeID]model.Node{}
	}
	for _, f := range facts {
		c.nodes[f.Node.ID] = f.Node
	}
	return c.UnitOutput.PutNodes(ctx, facts)
}

func (c *capture) PutRelations(ctx context.Context, facts []model.RelationFact) error {
	c.rels = append(c.rels, facts...)
	return c.UnitOutput.PutRelations(ctx, facts)
}

// stateCapture is a capture of a structural unit, which stores its link
// table as provider state beside its facts.
type stateCapture struct{ *capture }

func (c stateCapture) PutState(ctx context.Context, kind string, payload []byte) error {
	s, ok := c.UnitOutput.(provider.StateSink)
	if !ok {
		return &model.Error{Code: model.CodeInternal, Message: "the captured unit output does not store provider state"}
	}
	return s.PutState(ctx, kind, payload)
}

// calls is the evidence of every `calls` occurrence from a declaration
// named caller to a declaration named callee (any name when callee is
// empty), never to an open site or an external node.
func (c *capture) calls(caller, callee string) []model.Evidence {
	var out []model.Evidence
	for _, r := range c.rels {
		to := c.nodes[r.Relation.To]
		if r.Relation.Kind != model.RelCalls || c.nodes[r.Relation.From].Name != caller || len(to.Metadata) != 0 {
			continue
		}
		if callee != "" && to.Name != callee {
			continue
		}
		out = append(out, r.Evidence...)
	}
	return out
}

// nodeResolution reads a node's metadata `resolution`, empty for a
// declaration.
func nodeResolution(n model.Node) model.NameResolution {
	var m struct {
		Resolution model.NameResolution `json:"resolution"`
	}
	if len(n.Metadata) == 0 || json.Unmarshal(n.Metadata, &m) != nil {
		return ""
	}
	return m.Resolution
}

// golden is one source-based case: files run through the real structural
// provider and the linker, and the complete set of facts the language
// defines for them.
//
// want is every `calls` occurrence to a declaration or an external method,
// written "<site> <callee> -> <target> <name> <dispatch>", with " set=<n>"
// appended when the site has n >= 2 targets: site is the label of the line
// the call's callee token is on, callee that token as written, target the
// label of the line the bound declaration's name is on and name that
// declaration's name, or "external <qualified>" for an external method's
// node; dispatch is the occurrence's `virtual` or `exact`. A line is labelled
// by a trailing comment `// @x` or `# @x`; a line with no label is written
// "<path>:<line>", so a binding to a declaration the case did not expect
// still shows where it went.
//
// sites, when set, is every open-site node, written "<site> <callee> open
// <reason> <shape> <arguments>"; facts, when set, every other relation:
// "<from> imports <to>" (file paths; "<from> imports open <fragments>" for
// an open import site, the fragments joined by "|"), "<label> <name>
// extends|implements|overrides <label> <name>" (or "external <qualified>"),
// "<label> <name> flows <label> <name>" for a `data_flows_to` from an
// argument's variable to a parameter, each with " set=<n>" when it carries a
// set size. returns, when set, is every return flow: "<label> <name>
// summary:<position> <label> <name>" for an argument's variable flowing into
// a variable the site's result is written into through the callee's return
// summary, and "<label> return return <label> <name>" for a callee's return
// node flowing into such a variable or into the return node of the callable
// that returns the call. counts, when set, are details the `calls` row must
// carry.
//
// What the language defines, under ADR-0015 and decision 2's techniques and
// nothing beyond them (no points-to, no reachability):
//   - a call on a receiver of a declared or annotated type calls the method
//     that type declares or inherits, once, `virtual` unless the language
//     says no override can run; its overrides are found at query time;
//   - `super`, a qualified call, a constructed object, a final, static,
//     private or non-virtual method and a function are `exact`;
//   - a receiver whose classes are known only by value flow calls each
//     class's method, each with the set size;
//   - a call through a function value binds, in the static languages, every
//     function whose value the case takes and whose type is the called
//     value's declared type, and in JavaScript and Python the functions that
//     reach the value through assignments, parameters, returns and fields,
//     each `exact` with the set size when there are several;
//   - a construction binds the constructor that runs, its own or the one it
//     inherits, and the class itself when none in its hierarchy is declared;
//   - a call no analysis binds is an open site, and a call of something
//     outside the project is external, named by no `calls` unless its
//     receiver's type is an external type.
//
// Each published set must equal its list exactly: a missing fact is a lost
// edge, an extra one a wrong edge.
type golden struct {
	name string
	// anchor is the language reference, by standard title and section, the
	// answer is read from.
	anchor  string
	files   map[string]string
	want    []string
	sites   []string
	facts   []string
	returns []string
	counts  map[string]string
	// lost is each capability's count of facts whose target has no node
	// (facts_without_node), none when nil.
	lost map[string]string
	// fails names the wrong answer the case catches and the change to the
	// linker that would produce it.
	fails string
}

// runGoldens checks every case against what the linker publishes.
func runGoldens(t *testing.T, cases []golden) {
	t.Helper()
	structural := newStructural(t)
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			l := linkSources(t, structural, c.files)
			check := func(what string, got, want []string) {
				t.Helper()
				want = slices.Clone(want)
				slices.Sort(want)
				if !slices.Equal(got, want) {
					t.Fatalf("%s: %s\ngot  %q\nwant %q\n(fails under: %s)", what, c.anchor, got, want, c.fails)
				}
			}
			check("calls", l.bindings(t), c.want)
			if c.sites != nil {
				check("open sites", l.openSites(t), c.sites)
			}
			if c.facts != nil {
				check("facts", l.facts(t), c.facts)
			}
			if c.returns != nil {
				check("returns", l.returns(t), c.returns)
			}
			if want := c.lost; !maps.Equal(l.lost, want) && (len(want) > 0 || len(l.lost) > 0) {
				t.Fatalf("facts without a node by capability: %v, want %v (fails under: %s)", l.lost, want, c.fails)
			}
			for k, v := range c.counts {
				if got := l.calls.Details[k]; got != v {
					t.Fatalf("the calls row's %s is %q, want %q (details %v; fails under: %s)", k, got, v, l.calls.Details, c.fails)
				}
			}
		})
	}
}

// labelPattern is a line's trailing label comment.
var labelPattern = regexp.MustCompile(`(?://|#)\s*@(\w+)\s*$`)

// paths is each case file's path by its FileID.
func (l linked) paths(t *testing.T) map[model.FileID]string {
	t.Helper()
	paths := map[model.FileID]string{}
	for p := range l.files {
		paths[l.h.File(t, p).ID] = p
	}
	return paths
}

// setSuffix is " set=<n>" for an occurrence carrying a set size.
func setSuffix(e model.Evidence) string {
	if e.SetSize == 0 {
		return ""
	}
	return " set=" + strconv.FormatInt(e.SetSize, 10)
}

// endpoint spells a relation's endpoint: "external <qualified>" for an
// external node, else "<label> <name>" (target).
func (l linked) endpoint(paths map[model.FileID]string, n model.Node) string {
	if nodeResolution(n) == model.ResolutionExternal {
		return "external " + n.QualifiedName
	}
	return l.target(paths, n)
}

// bindings is every `calls` occurrence to a declaration or an external node,
// in the golden spelling, sorted and each once.
func (l linked) bindings(t *testing.T) []string {
	t.Helper()
	paths := l.paths(t)
	var out []string
	for _, r := range l.out.rels {
		if r.Relation.Kind != model.RelCalls {
			continue
		}
		to := l.out.nodes[r.Relation.To]
		if res := nodeResolution(to); res != "" && res != model.ResolutionExternal {
			continue
		}
		for _, e := range r.Evidence {
			b := l.site(t, e.NativeKey) + " -> " + l.endpoint(paths, to) + " " + string(e.Dispatch) + setSuffix(e)
			if !slices.Contains(out, b) {
				out = append(out, b)
			}
		}
	}
	slices.Sort(out)
	return out
}

// openSites is every open-site node, in the golden spelling, sorted.
func (l linked) openSites(t *testing.T) []string {
	t.Helper()
	paths := l.paths(t)
	var out []string
	for _, n := range l.out.nodes {
		var m model.OpenSiteMetadata
		if nodeResolution(n) != model.ResolutionUnresolved || json.Unmarshal(n.Metadata, &m) != nil || !m.Shape.Valid() {
			continue
		}
		if err := m.Validate(); err != nil {
			t.Fatalf("open site %s: %v", n.ID, err)
		}
		path := paths[n.FileID]
		label := path + ":?"
		if n.Range != nil {
			label = labelAt(path, l.files[path], int(n.Range.Start.Byte))
		}
		out = append(out, label+" "+n.Name+" open "+string(m.Reason)+" "+string(m.Shape)+" "+strconv.FormatInt(m.Arguments, 10))
	}
	slices.Sort(out)
	return out
}

// facts is every relation other than `calls`, in the golden spelling,
// sorted and each once.
func (l linked) facts(t *testing.T) []string {
	t.Helper()
	paths := l.paths(t)
	var out []string
	add := func(f string) {
		if !slices.Contains(out, f) {
			out = append(out, f)
		}
	}
	for _, r := range l.out.rels {
		from, to := l.out.nodes[r.Relation.From], l.out.nodes[r.Relation.To]
		for _, e := range r.Evidence {
			switch r.Relation.Kind {
			case model.RelCalls:
			case model.RelImports:
				var m model.OpenImportSiteMetadata
				if nodeResolution(to) == model.ResolutionUnresolved && json.Unmarshal(to.Metadata, &m) == nil {
					add(paths[from.FileID] + " imports open " + strings.Join(m.Fragments, "|"))
					continue
				}
				add(paths[from.FileID] + " imports " + paths[to.FileID] + setSuffix(e))
			case model.RelDataFlowsTo:
				if e.Detail == link.DetailArgument {
					add(l.target(paths, from) + " flows " + l.target(paths, to) + setSuffix(e))
				}
			default:
				add(l.endpoint(paths, from) + " " + string(r.Relation.Kind) + " " + l.endpoint(paths, to) + setSuffix(e))
			}
		}
	}
	slices.Sort(out)
	return out
}

// returns is every return flow (`data_flows_to` of the summary and return
// details), in the golden spelling, sorted and each once.
func (l linked) returns(t *testing.T) []string {
	t.Helper()
	paths := l.paths(t)
	var out []string
	for _, r := range l.out.rels {
		if r.Relation.Kind != model.RelDataFlowsTo {
			continue
		}
		from, to := l.out.nodes[r.Relation.From], l.out.nodes[r.Relation.To]
		for _, e := range r.Evidence {
			if e.Detail == link.DetailArgument {
				continue
			}
			f := l.target(paths, from) + " " + e.Detail + " " + l.target(paths, to)
			if !slices.Contains(out, f) {
				out = append(out, f)
			}
		}
	}
	slices.Sort(out)
	return out
}

// site spells the call-site key "callsite:<path>:<first>-<last>" (one-based,
// inclusive bytes of the callee token) as "<label> <callee>".
func (l linked) site(t *testing.T, key string) string {
	t.Helper()
	rest, ok := strings.CutPrefix(key, "callsite:")
	i := strings.LastIndex(rest, ":")
	if !ok || i < 0 {
		t.Fatalf("call-site key %q", key)
	}
	path := rest[:i]
	first, last, ok := strings.Cut(rest[i+1:], "-")
	a, err1 := strconv.Atoi(first)
	b, err2 := strconv.Atoi(last)
	src := l.files[path]
	if !ok || err1 != nil || err2 != nil || a < 1 || b < a || b > len(src) {
		t.Fatalf("call-site key %q does not fit %s", key, path)
	}
	return labelAt(path, src, a-1) + " " + src[a-1:b]
}

// target spells a bound declaration as "<label> <name>": the label of the
// line its name is first written on inside its range. A return node, which
// no token names, takes the label of its callable's name: the callable is
// the structural node of a callable kind at the same range.
func (l linked) target(paths map[model.FileID]string, n model.Node) string {
	path := paths[n.FileID]
	src := l.files[path]
	if n.Range == nil || int(n.Range.End.Byte) > len(src) {
		return path + ":? " + n.Name
	}
	if n.Kind == model.NodeReturn {
		for _, c := range l.structural.nodes {
			if (c.Kind == model.NodeFunction || c.Kind == model.NodeMethod || c.Kind == model.NodeTest) &&
				c.FileID == n.FileID && c.Range != nil && *c.Range == *n.Range && c.NameRange != nil {
				return labelAt(path, src, int(c.NameRange.Start.Byte)) + " " + n.Name
			}
		}
		return path + ":? " + n.Name
	}
	start := int(n.Range.Start.Byte)
	at := start
	body := src[start:n.Range.End.Byte]
	for off := 0; ; {
		j := strings.Index(body[off:], n.Name)
		if j < 0 || n.Name == "" {
			break
		}
		j += off
		if !identByte(body, j-1) && !identByte(body, j+len(n.Name)) {
			at = start + j
			break
		}
		off = j + 1
	}
	return labelAt(path, src, at) + " " + n.Name
}

// labelAt is the label of the line holding byte at of src, or "<path>:<line>".
func labelAt(path, src string, at int) string {
	line := strings.Count(src[:at], "\n")
	text := strings.Split(src, "\n")[line]
	if m := labelPattern.FindStringSubmatch(text); m != nil {
		return m[1]
	}
	return path + ":" + strconv.Itoa(line+1)
}

// identByte reports whether s[i] exists and can continue an identifier.
func identByte(s string, i int) bool {
	if i < 0 || i >= len(s) {
		return false
	}
	c := s[i]
	return c == '_' || c == '$' || c >= '0' && c <= '9' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z'
}
