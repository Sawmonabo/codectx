package link_test

import (
	"context"
	"encoding/json"
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
	"github.com/Sawmonabo/codectx/internal/provider/dependence"
	"github.com/Sawmonabo/codectx/internal/provider/link"
	"github.com/Sawmonabo/codectx/internal/provider/providertest"
	"github.com/Sawmonabo/codectx/internal/provider/treesitter"
	"github.com/Sawmonabo/codectx/internal/provider/treesitter/lang"
	"github.com/Sawmonabo/codectx/internal/provider/treesitter/wire"
	"github.com/Sawmonabo/codectx/internal/provider/treesitter/worker"
)

// TestMain makes this test binary its own parser worker, as the codectx
// binary is in production.
func TestMain(m *testing.M) {
	if len(os.Args) > 1 && os.Args[1] == wire.Subcommand {
		os.Exit(worker.Main(context.Background(), os.Stdin, os.Stdout, os.Stderr))
	}
	os.Exit(m.Run())
}

// linkCases is one source per grammar. Each holds one indirect site, a call
// through a value that names no declaration (a function value, a function
// pointer, a call through an interface parameter with one implementation),
// and one ambiguous site, a call two or more declarations of the unit can
// answer (a receiver whose type flows from two constructions, an interface
// or base method with two overrides, a function pointer two address-taken
// functions of its signature can hold).
var linkCases = []struct {
	name string
	// anchor is the language reference the indirect and ambiguous answers
	// are read from.
	anchor string
	files  map[string]string
	// indirect is the indirect site's caller and the declaration it reaches.
	indirect [2]string
	// ambiguous is the ambiguous site's caller and the name every candidate
	// carries.
	ambiguous [2]string
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
`}, indirect: [2]string{"indirect", "seven"}, ambiguous: [2]string{"ambiguous", "Area"}},
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
`}, indirect: [2]string{"indirect", "run"}, ambiguous: [2]string{"ambiguous", "area"}},
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
`}, indirect: [2]string{"indirect", "seven"}, ambiguous: [2]string{"ambiguous", "area"}},
	// Derived twice from the source and the anchor, the second time blind (the source,
	// the anchor and the binding rules alone); the two agree on both sites.
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
`}, indirect: [2]string{"indirect", "seven"}, ambiguous: [2]string{"ambiguous", "area"}},
	// Derived twice from the source and the anchor, the second time blind (the source,
	// the anchor and the binding rules alone); the two agree on both sites.
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
`}, indirect: [2]string{"indirect", "seven"}, ambiguous: [2]string{"ambiguous", "area"}},
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
`}, indirect: [2]string{"indirect", "seven"}, ambiguous: [2]string{"ambiguous", "area"}},
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
`}, indirect: [2]string{"indirect", "seven"}, ambiguous: [2]string{"ambiguous", "area"}},
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
`}, indirect: [2]string{"indirect", "wide"}, ambiguous: [2]string{"ambiguous", ""}},
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
`}, indirect: [2]string{"indirect", "wide"}, ambiguous: [2]string{"ambiguous", "area"}},
}

// TestCallsArriveForEveryGrammar runs the real structural provider over one
// source per grammar, then the linking provider over the unit the dependence
// plan gives that source, and asserts what it published.
//
// Failure modes: `calls` missing for a language whose worker tables and
// resolver both exist; an ambiguous site published as a guess (a direct edge
// to one candidate) or with a candidate dropped; a relation published with
// an endpoint the unit has no node fact for, which a generation refuses; a
// `calls` row claiming fresh coverage it does not have.
//
// Mutation: publish a site's first binding as a direct `calls` edge and skip
// the rest (publish.go, filePublication.site) -> the ambiguous site has no
// callee node with its candidate count, and every case fails. Second
// mutation: skip the target occurrence nodes in joinTargets -> every case
// fails the endpoint check.
// Third mutation: publish a single binding into the ambiguous family (a
// minted callee and one `may_refer_to`) -> no indirect site has its direct
// `calls` edge, and every case fails.
func TestCallsArriveForEveryGrammar(t *testing.T) {
	structural := newStructural(t)
	for _, tc := range linkCases {
		t.Run(tc.name, func(t *testing.T) {
			l := linkSources(t, structural, tc.files)
			if !l.out.reaches(tc.indirect[0], tc.indirect[1]) {
				t.Errorf("the indirect site in %s is not bound to %s alone (%s)", tc.indirect[0], tc.indirect[1], tc.anchor)
			}
			checkAmbiguous(t, l.out, tc.ambiguous[0], tc.ambiguous[1])
		})
	}
}

// linked is what one linking unit published over a case's files.
type linked struct {
	h     *providertest.Harness
	files map[string]string
	out   *capture
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
		if dependence.FamilyOf(srclang.Of(p)) != "" {
			sources = append(sources, p)
		}
	}
	slices.Sort(sources)
	if len(sources) == 0 {
		t.Fatal("the case has no source file")
	}
	var structIDs []model.UnitID
	for _, p := range sources {
		res, id, err := h.Run(t, structural, lang.ScopePrefix+p, []string{p})
		if err != nil {
			t.Fatalf("structural unit of %s: %v", p, err)
		}
		if res.State != model.RunSucceeded {
			t.Fatalf("structural unit of %s ran %s", p, res.State)
		}
		structIDs = append(structIDs, id)
	}
	// Sealing attached the structural units to the staging generation, which
	// is where the linker reads their tables.
	linker := link.New(link.StoreTables(h.Store))
	plan, err := dependence.PlanUnits(ctx, h.View)
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
	checkCapability(t, result, scope)
	checkEndpoints(t, out)
	return linked{h: h, files: files, out: out}
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

// checkCapability asserts the one `calls` row at the unit's scope is fresh:
// every member of a clean source has its table whole.
func checkCapability(t *testing.T, r model.ProviderResult, scope string) {
	t.Helper()
	if len(r.Capabilities) != 1 {
		t.Fatalf("the linking unit reported %d capability rows, want one", len(r.Capabilities))
	}
	c := r.Capabilities[0]
	if c.Capability != string(model.RelCalls) || c.Scope != scope || c.State != model.CapabilityFresh {
		t.Fatalf("the linking unit reported %s %s at %q, want calls fresh at %q (details %v)", c.Capability, c.State, c.Scope, scope, c.Details)
	}
}

// checkEndpoints asserts every relation's endpoints are nodes this unit
// published: the occurrences of the structural nodes, and the ambiguous
// callees it minted.
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

// checkAmbiguous asserts the ambiguous site of caller publishes into the
// ambiguous family: a `calls` edge to a callee node carrying the candidate
// count, and one `may_refer_to` edge at the same site to each of that many
// distinct candidates, every one named name (any name when name is empty).
func checkAmbiguous(t *testing.T, out *capture, caller, name string) {
	t.Helper()
	found := false
	for _, r := range out.rels {
		from := out.nodes[r.Relation.From]
		if r.Relation.Kind != model.RelCalls || from.Name != caller {
			continue
		}
		for _, e := range r.Evidence {
			if e.Detail != link.DetailAmbiguous {
				continue
			}
			found = true
			var meta struct {
				Resolution string `json:"resolution"`
				Candidates int    `json:"candidates"`
			}
			if err := json.Unmarshal(out.nodes[r.Relation.To].Metadata, &meta); err != nil || meta.Resolution != "ambiguous" || meta.Candidates < 2 {
				t.Fatalf("the ambiguous callee of %s carries metadata %s, want resolution ambiguous with two or more candidates",
					caller, out.nodes[r.Relation.To].Metadata)
			}
			candidates := map[model.NodeID]bool{}
			for _, m := range out.rels {
				if m.Relation.Kind != model.RelMayReferTo || m.Relation.From != r.Relation.From {
					continue
				}
				for _, me := range m.Evidence {
					if me.Detail == link.DetailAmbiguousTarget && me.NativeKey == e.NativeKey {
						candidates[m.Relation.To] = true
						if got := out.nodes[m.Relation.To].Name; name != "" && got != name {
							t.Fatalf("a candidate of the ambiguous site in %s is %q, want %q", caller, got, name)
						}
					}
				}
			}
			if len(candidates) != meta.Candidates {
				t.Fatalf("the ambiguous site in %s counts %d candidates and publishes %d", caller, meta.Candidates, len(candidates))
			}
		}
	}
	if !found {
		t.Fatalf("%s has no ambiguous call site", caller)
	}
}

// capture records what the linking unit hands its output.
type capture struct {
	provider.UnitOutput
	nodes map[model.NodeID]model.Node
	rels  []model.RelationFact
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

// reaches reports whether a site of caller is bound to the one declaration
// named callee: a direct `calls` edge to it. A `may_refer_to` edge, or a
// `calls` edge to an ambiguous callee, is one candidate among several and
// never a binding the site makes alone.
func (c *capture) reaches(caller, callee string) bool {
	for _, r := range c.rels {
		if r.Relation.Kind != model.RelCalls || c.nodes[r.Relation.From].Name != caller || c.nodes[r.Relation.To].Name != callee {
			continue
		}
		for _, e := range r.Evidence {
			if e.Detail != link.DetailAmbiguous {
				return true
			}
		}
	}
	return false
}

// golden is one source-based case: files run through the real structural
// provider and the linker, and the complete set of bindings the language
// defines for its call sites.
//
// A binding is written "<site> <callee> -> <target> <name>": site is the label
// of the line the call's callee token is on, callee that token as written,
// target the label of the line the bound declaration's name is on, and name
// that declaration's name. A line is labelled by a trailing comment `// @x`
// or `# @x`. A line with no label is written "<path>:<line>", so a binding to
// a declaration the case did not expect still shows where it went.
//
// What the language defines, under decision 2's techniques and nothing
// beyond them (no points-to, no reachability):
//   - a call of something the case does not declare (a library function, a
//     builtin, a type outside the case) binds nothing;
//   - a call the language dispatches at run time binds the method its
//     receiver's declared or inferred type selects and every override in a
//     subtype the case declares; one it binds statically (static, private,
//     `super`, non-virtual, qualified) binds the one declaration it selects;
//   - a declaration without a body (abstract, interface, prototype) is bound
//     only when the case has no body the call could run instead;
//   - a call through a function value binds, in the static languages, every
//     function whose value the case takes and whose type is the called
//     value's declared type, and in JavaScript and Python the functions that
//     reach the value through assignments, parameters, returns and fields;
//   - a construction binds the constructor that runs, its own or the one it
//     inherits, and the class itself when none in its hierarchy is declared.
//
// One target is a direct `calls` edge; two or more are the ambiguous family,
// a `calls` edge to a minted callee counting them and one `may_refer_to` per
// target. Either way the published set must equal want exactly: a missing
// binding is a lost edge, an extra one a wrong edge.
type golden struct {
	name string
	// anchor is the language reference, by standard title and section, the
	// answer is read from.
	anchor string
	files  map[string]string
	want   []string
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
			got := l.bindings(t)
			want := slices.Clone(c.want)
			slices.Sort(want)
			if !slices.Equal(got, want) {
				t.Fatalf("%s\ngot  %q\nwant %q\n(fails under: %s)", c.anchor, got, want, c.fails)
			}
		})
	}
}

// labelPattern is a line's trailing label comment.
var labelPattern = regexp.MustCompile(`(?://|#)\s*@(\w+)\s*$`)

// bindings is every (site, target) the unit published, in the golden
// spelling, sorted. It checks the ambiguous family's shape on the way: a
// site's ambiguous callee counts exactly the candidates published for it.
func (l linked) bindings(t *testing.T) []string {
	t.Helper()
	paths := map[model.FileID]string{}
	for p := range l.files {
		paths[l.h.File(t, p).ID] = p
	}
	var out []string
	counted := map[string]int{}
	candidates := map[string]map[model.NodeID]bool{}
	for _, r := range l.out.rels {
		for _, e := range r.Evidence {
			switch {
			case r.Relation.Kind == model.RelCalls && e.Detail == link.DetailAmbiguous:
				var meta struct {
					Candidates int `json:"candidates"`
				}
				if err := json.Unmarshal(l.out.nodes[r.Relation.To].Metadata, &meta); err != nil {
					t.Fatalf("ambiguous callee metadata: %v", err)
				}
				counted[e.NativeKey] = meta.Candidates
				continue
			case r.Relation.Kind == model.RelMayReferTo && e.Detail == link.DetailAmbiguousTarget:
				if candidates[e.NativeKey] == nil {
					candidates[e.NativeKey] = map[model.NodeID]bool{}
				}
				candidates[e.NativeKey][r.Relation.To] = true
			case r.Relation.Kind != model.RelCalls:
				continue
			}
			b := l.site(t, e.NativeKey) + " -> " + l.target(paths, l.out.nodes[r.Relation.To])
			if !slices.Contains(out, b) {
				out = append(out, b)
			}
		}
	}
	for key, n := range counted {
		if len(candidates[key]) != n || n < 2 {
			t.Fatalf("the ambiguous site %s counts %d candidates and publishes %d", key, n, len(candidates[key]))
		}
	}
	for key := range candidates {
		if _, ok := counted[key]; !ok {
			t.Fatalf("the site %s publishes candidates without an ambiguous callee", key)
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
// line its name is first written on inside its range.
func (l linked) target(paths map[model.FileID]string, n model.Node) string {
	path := paths[n.FileID]
	src := l.files[path]
	if n.Range == nil || int(n.Range.End.Byte) > len(src) {
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
