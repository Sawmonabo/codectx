package link_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/Sawmonabo/codectx/internal/admission"
	"github.com/Sawmonabo/codectx/internal/model"
	"github.com/Sawmonabo/codectx/internal/process"
	"github.com/Sawmonabo/codectx/internal/provider"
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
	name   string
	source string
	files  map[string]string
	// indirect is the indirect site's caller and the declaration it reaches.
	indirect [2]string
	// ambiguous is the ambiguous site's caller and the name every candidate
	// carries.
	ambiguous [2]string
}{
	{name: "go", source: "calls.go", files: map[string]string{"calls.go": `package p

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
	{name: "java", source: "Calls.java", files: map[string]string{"Calls.java": `interface Shape { int area(); }
class Square implements Shape { public int area() { return 4; } }
class Circle implements Shape { public int area() { return 3; } }
interface Op { int run(int x); }
class Twice implements Op { public int run(int x) { return 2 * x; } }
class Calls {
  int ambiguous(Shape s) { return s.area(); }
  int indirect(Op op) { return op.run(1); }
}
`}, indirect: [2]string{"indirect", "run"}, ambiguous: [2]string{"ambiguous", "area"}},
	{name: "javascript", source: "calls.js", files: map[string]string{"calls.js": `class Square { area() { return 4; } }
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
	{name: "typescript", source: "calls.ts", files: map[string]string{"calls.ts": `class Square { area(): number { return 4; } }
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
	{name: "tsx", source: "calls.tsx", files: map[string]string{"calls.tsx": `class Square { area(): number { return 4; } }
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
	{name: "python", source: "calls.py", files: map[string]string{"calls.py": `class Square:
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
	{name: "rust", source: "src/lib.rs", files: map[string]string{
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
	{name: "c", source: "calls.c", files: map[string]string{"calls.c": `static int seven(int n) { return n + 7; }
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
	{name: "cpp", source: "calls.cpp", files: map[string]string{"calls.cpp": `struct Shape { virtual int area() = 0; };
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
// source per grammar, then the linking provider over the unit the project
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
func TestCallsArriveForEveryGrammar(t *testing.T) {
	structural := newStructural(t)
	for _, tc := range linkCases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			h := providertest.New(t, tc.files)
			res, structID, err := h.Run(t, structural, lang.ScopePrefix+tc.source, []string{tc.source})
			if err != nil {
				t.Fatalf("structural unit: %v", err)
			}
			if res.State != model.RunSucceeded {
				t.Fatalf("structural unit ran %s", res.State)
			}
			// Sealing attached the structural unit to the staging generation,
			// which is where the linker reads its tables.
			linker := link.New(link.StoreTables(h.Store))

			plan, err := link.PlanUnits(ctx, h.View)
			if err != nil {
				t.Fatalf("PlanUnits: %v", err)
			}
			var scope string
			var inputs []string
			for _, pu := range plan.Units {
				if pu.OwnsInput(tc.source) {
					scope = pu.ScopeKey
					for p := range tc.files {
						if pu.OwnsInput(p) {
							inputs = append(inputs, p)
						}
					}
				}
			}
			if scope == "" {
				t.Fatalf("the project plan gives %s no unit", tc.source)
			}
			u := h.Plan(t, linker, scope, inputs, structID)
			out := &capture{UnitOutput: h.Begin(t, u, inputs)}
			result, err := provider.RunUnit(ctx, linker, u.Request, out, providertest.Limits, h.Pool)
			if err != nil {
				t.Fatalf("linking unit: %v", err)
			}
			checkCapability(t, result, scope)
			checkEndpoints(t, out)
			if !out.reaches(tc.indirect[0], tc.indirect[1]) {
				t.Errorf("the indirect site in %s does not reach %s", tc.indirect[0], tc.indirect[1])
			}
			checkAmbiguous(t, out, tc.ambiguous[0], tc.ambiguous[1])
		})
	}
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

// reaches reports whether a site of caller binds a declaration named callee:
// a `calls` edge to it, or a `may_refer_to` edge to it as one candidate.
func (c *capture) reaches(caller, callee string) bool {
	for _, r := range c.rels {
		if r.Relation.Kind != model.RelCalls && r.Relation.Kind != model.RelMayReferTo {
			continue
		}
		if c.nodes[r.Relation.From].Name == caller && c.nodes[r.Relation.To].Name == callee {
			return true
		}
	}
	return false
}
