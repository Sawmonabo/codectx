package treesitter_test

import (
	"context"
	"encoding/json"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/Sawmonabo/codectx/internal/model"
	"github.com/Sawmonabo/codectx/internal/provider"
	"github.com/Sawmonabo/codectx/internal/provider/providertest"
	"github.com/Sawmonabo/codectx/internal/provider/treesitter"
	"github.com/Sawmonabo/codectx/internal/provider/treesitter/lang"
)

// extracted runs the structural provider over one file and returns every
// fact it published.
func extracted(t *testing.T, p *treesitter.Provider, file, src string) *capture {
	t.Helper()
	c, _ := extractedRun(t, p, file, src)
	return c
}

// extractedRun is extracted with the run's result, whose capability rows it
// reports.
func extractedRun(t *testing.T, p *treesitter.Provider, file, src string) (*capture, model.ProviderResult) {
	t.Helper()
	h := providertest.New(t, map[string]string{file: src})
	u := h.Plan(t, p, lang.ScopePrefix+file, []string{file})
	c := &capture{UnitOutput: h.Begin(t, u, []string{file})}
	result, err := provider.RunUnit(context.Background(), p, u.Request, c, providertest.Limits, h.Pool)
	if err != nil {
		t.Fatalf("RunUnit(%s): %v", file, err)
	}
	if result.State != model.RunSucceeded {
		t.Fatalf("run state for %s = %s, want succeeded", file, result.State)
	}
	return c, result
}

// structureRow is the run's structure capability row.
func structureRow(t *testing.T, result model.ProviderResult) model.CapabilityState {
	t.Helper()
	for _, row := range result.Capabilities {
		if row.Capability == "structure" {
			return row
		}
	}
	t.Fatal("no structure capability row")
	return model.CapabilityState{}
}

// node is the one published node of kind named name, failing on none or
// several.
func (c *capture) node(t *testing.T, kind model.NodeKind, name string) model.Node {
	t.Helper()
	got := c.all(kind, name)
	if len(got) != 1 {
		t.Fatalf("%d %s nodes named %q, want 1: %v", len(got), kind, name, got)
	}
	return got[0]
}

// all is every published node of kind named name.
func (c *capture) all(kind model.NodeKind, name string) []model.Node {
	var out []model.Node
	for _, n := range c.nodes {
		if n.Node.Kind == kind && n.Node.Name == name {
			out = append(out, n.Node)
		}
	}
	return out
}

// byID is the published node with id.
func (c *capture) byID(id model.NodeID) (model.Node, bool) {
	for _, n := range c.nodes {
		if n.Node.ID == id {
			return n.Node, true
		}
	}
	return model.Node{}, false
}

// moduleNode is the file's own module node, the one carrying its path.
func (c *capture) moduleNode(t *testing.T, file string) model.Node {
	t.Helper()
	for _, n := range c.nodes {
		if n.Node.Kind == model.NodeModule && n.Node.QualifiedName == file {
			return n.Node
		}
	}
	t.Fatalf("no module node for %s", file)
	return model.Node{}
}

// targets is the name of every node a relation of kind from from reaches,
// sorted.
func (c *capture) targets(kind model.RelationKind, from model.NodeID) []string {
	var out []string
	for _, r := range c.rels {
		if r.Relation.Kind != kind || r.Relation.From != from {
			continue
		}
		if n, ok := c.byID(r.Relation.To); ok {
			out = append(out, n.Name)
		}
	}
	slices.Sort(out)
	return out
}

// count is the number of relations of kind from from.
func (c *capture) count(kind model.RelationKind, from model.NodeID) int {
	n := 0
	for _, r := range c.rels {
		if r.Relation.Kind == kind && r.Relation.From == from {
			n++
		}
	}
	return n
}

// metadata decodes a node's metadata.
func metadata(t *testing.T, n model.Node) map[string]any {
	t.Helper()
	m := map[string]any{}
	if len(n.Metadata) > 0 {
		if err := json.Unmarshal(n.Metadata, &m); err != nil {
			t.Fatalf("node %s metadata: %v", n.Name, err)
		}
	}
	return m
}

// TestFunctionValuedPropertiesAreDeclarations indexes the two shapes the
// ECMAScript grammars' own reference tags define as function definitions, a
// function assigned to a member and a function-valued pair, in JavaScript
// and TypeScript, and asserts each is a function declaration named by its
// property, with the path it is stored under ahead of its name: `a.b`, the
// object binding's path followed by the key, a member of a nested object,
// and a pair of an object assigned to `module.exports`. A property assigned
// twice is two declarations, told apart by content, and the binding
// `contains` its pair's function.
//
// Mutation it catches: capture only the variable declarator's function (drop
// the assignment and pair patterns) -> no `b`, `k`, `deep` or `run`
// declaration is published.
func TestFunctionValuedPropertiesAreDeclarations(t *testing.T) {
	p := newProvider(t)
	const src = `const obj = { k: () => 1, nested: { deep: function () { return 2; } } };
a.b = () => obj.k();
a.b = () => 3;
module.exports = { run: () => 4 };
`
	for _, file := range []string{"props.js", "props.ts"} {
		t.Run(file, func(t *testing.T) {
			c := extracted(t, p, file, src)
			for name, qualified := range map[string]string{"k": "obj.k", "deep": "obj.nested.deep", "run": "module.exports.run"} {
				if n := c.node(t, model.NodeFunction, name); n.QualifiedName != qualified {
					t.Errorf("%s is qualified %q, want %q", name, n.QualifiedName, qualified)
				}
			}
			bs := c.all(model.NodeFunction, "b")
			if len(bs) != 2 || bs[0].ID == bs[1].ID {
				t.Fatalf("a.b assigned twice is %d nodes %v, want two distinct", len(bs), bs)
			}
			for _, b := range bs {
				if b.QualifiedName != "a.b" {
					t.Errorf("b is qualified %q, want a.b", b.QualifiedName)
				}
			}
			var obj model.Node
			for _, kind := range []model.NodeKind{model.NodeConstant, model.NodeVariable} {
				if got := c.all(kind, "obj"); len(got) == 1 {
					obj = got[0]
				}
			}
			if obj.ID == "" {
				t.Fatal("no declaration of obj")
			}
			if got := c.targets(model.RelContains, obj.ID); !slices.Contains(got, "k") {
				t.Errorf("obj contains %v, want k among them", got)
			}
		})
	}
}

// TestPythonAndCPublishExports asserts the export rules of the two languages
// that have no export keyword: a Python module with a static `__all__`
// exports exactly the names its static forms leave in it (an assignment,
// `+=`, `.append`, `.remove`), a Python module whose `__all__` is computed
// falls back to the underscore rule, and a C file exports its definitions
// with external linkage: not a `static` function or object, not a prototype,
// not an `extern` declaration.
//
// Mutations it catches: treat a computed `__all__` as empty (shown is not
// exported); ignore `__all__` (hidden and extra are exported); export a C
// `static` definition (helper and hidden_counter are exported).
func TestPythonAndCPublishExports(t *testing.T) {
	p := newProvider(t)
	cases := []struct {
		file, src string
		want      []string
	}{
		{"static_all.py", `__all__ = ["visible", "Klass", "extra"]
__all__ += ["more"]
__all__.append("late")
__all__.remove("extra")

def visible(): pass
def hidden(): pass
def _private(): pass
class Klass: pass
def late(): pass
def extra(): pass
def more(): pass
`, []string{"Klass", "late", "more", "visible"}},
		{"computed_all.py", `__all__ = [name for name in dir() if not name.startswith("_")]

def shown(): pass
def _kept_out(): pass
`, []string{"shown"}},
		{"linkage.c", `static int helper(int x) { return x; }
int api(int x) { return helper(x); }
int counter = 0;
int tentative;
static int hidden_counter;
extern int elsewhere;
int proto(void);
`, []string{"api", "counter", "tentative"}},
	}
	for _, tc := range cases {
		t.Run(tc.file, func(t *testing.T) {
			c := extracted(t, p, tc.file, tc.src)
			mod := c.moduleNode(t, tc.file)
			if got := c.targets(model.RelExports, mod.ID); !slices.Equal(got, tc.want) {
				t.Errorf("exports %v, want %v", got, tc.want)
			}
		})
	}
}

// TestEveryInFileCandidateIsKeptAndMemberCallsPublishNone asserts the two
// halves of the structural provider's candidate rule: a bare call of a name
// defined twenty times in the file keeps all twenty definitions as
// `may_refer_to` candidates, with the count on its callee reference; and a
// call of a member on a receiver value, which this provider does not type,
// publishes no in-file candidate however many methods share the name, its
// callee reference saying it is unresolved.
//
// Mutations it catches: put a fixed candidate cap back (sixteen candidates);
// keep the member-call fan-out (two `may_refer_to` from use).
func TestEveryInFileCandidateIsKeptAndMemberCallsPublishNone(t *testing.T) {
	p := newProvider(t)
	var src strings.Builder
	const defs = 20
	for i := range defs {
		src.WriteString("def f():\n    return " + strconv.Itoa(i) + "\n\n")
	}
	src.WriteString(`f()

class A:
    def m(self): pass

class B:
    def m(self): pass

def use(x):
    x.m()
`)
	c := extracted(t, p, "candidates.py", src.String())
	var fs int
	var ref model.Node
	for _, n := range c.nodes {
		switch {
		case n.Node.Kind == model.NodeFunction && n.Node.Name == "f" && n.Node.Range != nil:
			fs++
		case metadata(t, n.Node)["callee"] == "f":
			ref = n.Node
		}
	}
	if fs != defs {
		t.Fatalf("%d declarations of f, want %d", fs, defs)
	}
	mod := c.moduleNode(t, "candidates.py")
	if got := c.count(model.RelMayReferTo, mod.ID); got != defs {
		t.Errorf("the call of f keeps %d candidates, want %d", got, defs)
	}
	if m := metadata(t, ref); m["resolution"] != "ambiguous" || m["candidates"] != float64(defs) {
		t.Errorf("the callee reference of f says %v, want ambiguous with %d candidates", m, defs)
	}
	use := c.node(t, model.NodeFunction, "use")
	if got := c.count(model.RelMayReferTo, use.ID); got != 0 {
		t.Errorf("the member call x.m() publishes %d in-file candidates, want none", got)
	}
	for _, n := range c.nodes {
		if m := metadata(t, n.Node); m["callee"] == "m" && m["resolution"] != "unresolved" {
			t.Errorf("the callee reference of x.m says %v, want unresolved", m)
		}
	}
}

// TestAComputedAllIsDisclosedOnTheStructureRow asserts that a Python module
// whose `__all__` is computed says so on its structure capability row, while
// one with a static `__all__` and one with none say nothing, and none of them
// is partial for it: the underscore rule is the documented rule for such a
// module, not missing coverage.
//
// Mutation it catches: never send the computed bit (no detail on the
// computed module's row); send it for any `__all__` (a detail on the static
// one's).
func TestAComputedAllIsDisclosedOnTheStructureRow(t *testing.T) {
	p := newProvider(t)
	for _, tc := range []struct {
		file, src string
		disclosed bool
	}{
		{"computed.py", "__all__ = sorted(globals())\n\ndef f(): pass\n", true},
		{"static.py", "__all__ = [\"f\"]\n\ndef f(): pass\n", false},
		{"none.py", "def f(): pass\n", false},
	} {
		t.Run(tc.file, func(t *testing.T) {
			_, result := extractedRun(t, p, tc.file, tc.src)
			row := structureRow(t, result)
			_, got := row.Details["exports"]
			if got != tc.disclosed {
				t.Errorf("structure row details %v, want the exports detail %v", row.Details, tc.disclosed)
			}
			if row.State != model.CapabilityFresh {
				t.Errorf("structure row is %s, want fresh", row.State)
			}
		})
	}
}

// TestAMemberCallWithOneInFileMethodBindsNothingHere asserts that a call of a
// member on a receiver value binds no in-file declaration even when exactly
// one method of the file has the name: a name never binds a call (ADR-0015
// decision 1), so the call names a callee reference that says it is
// unresolved, which the linker binds or opens.
//
// Mutation it catches: bind when exactly one in-file method has the name
// (use calls A.only).
func TestAMemberCallWithOneInFileMethodBindsNothingHere(t *testing.T) {
	p := newProvider(t)
	c := extracted(t, p, "one.py", `class A:
    def only(self): pass

def use(x):
    x.only()
`)
	use := c.node(t, model.NodeFunction, "use")
	var method model.Node
	for _, n := range c.all(model.NodeMethod, "only") {
		if n.Range != nil {
			method = n
		}
	}
	if method.ID == "" {
		t.Fatal("no declaration of the method only")
	}
	for _, r := range c.rels {
		if r.Relation.Kind == model.RelCalls && r.Relation.From == use.ID && r.Relation.To == method.ID {
			t.Fatal("x.only() binds the one in-file method only")
		}
	}
	var found bool
	for _, n := range c.nodes {
		if m := metadata(t, n.Node); m["callee"] == "only" {
			found = true
			if m["resolution"] != "unresolved" {
				t.Errorf("the callee reference of x.only says %v, want unresolved", m)
			}
		}
	}
	if !found {
		t.Error("x.only() names no callee reference")
	}
}

// TestCallsInAnInlineHandlerAreMadeFromIt registers a route whose handler is
// an arrow function written in place and asserts the endpoint routes to that
// function's node and the calls in its body are made from it, so a walk from
// the endpoint reaches what the handler calls; the module, where the route is
// registered, makes no call to helper. The route is registered by a call, so
// it needs the worker's call-argument positions (wire.Site.Arguments).
//
// Mutation it catches: attribute the calls inside the handler to the
// enclosing declaration (the module calls helper and the handler calls
// nothing).
func TestCallsInAnInlineHandlerAreMadeFromIt(t *testing.T) {
	p := newProvider(t)
	c := extracted(t, p, "inline.js", `const express = require("express");
const app = express();

function helper() { return 1; }

app.get("/inline", (req, res) => { helper(); });
`)
	ep := c.node(t, model.NodeEndpoint, "GET /inline")
	handlers := c.targets(model.RelRoutesTo, ep.ID)
	if len(handlers) != 1 || handlers[0] != "GET /inline" {
		t.Fatalf("the endpoint routes to %v, want its handler literal", handlers)
	}
	var literal model.Node
	for _, r := range c.rels {
		if r.Relation.Kind == model.RelRoutesTo && r.Relation.From == ep.ID {
			literal, _ = c.byID(r.Relation.To)
		}
	}
	if literal.Kind != model.NodeFunction {
		t.Fatalf("the handler is a %s, want a function", literal.Kind)
	}
	if got := c.targets(model.RelCalls, literal.ID); !slices.Equal(got, []string{"helper"}) {
		t.Errorf("the handler calls %v, want [helper]", got)
	}
	if got := c.targets(model.RelCalls, c.moduleNode(t, "inline.js").ID); slices.Contains(got, "helper") {
		t.Errorf("the module calls %v; the handler's call of helper is the handler's", got)
	}
}

// TestAFunctionInAMainGuardKeepsItsName asserts that a function defined inside
// a `__main__` guard is a declaration of the module named by its own name,
// told apart from a top-level function of the same name by the branch it sits
// in, not by a `__main__` prefix, and that it is not exported; the guard routes
// to it when it calls it.
//
// Mutations it catches: qualify it under the guard (`__main__.f`); export it
// by the underscore rule; drop the branch from its identity (the two f's are
// one node).
func TestAFunctionInAMainGuardKeepsItsName(t *testing.T) {
	p := newProvider(t)
	c := extracted(t, p, "guarded.py", `def f():
    return 1

if __name__ == "__main__":
    def f():
        return 2
    f()
`)
	fs := c.all(model.NodeFunction, "f")
	var located []model.Node
	for _, n := range fs {
		if n.Range != nil {
			located = append(located, n)
		}
	}
	if len(located) != 2 || located[0].ID == located[1].ID {
		t.Fatalf("%d located f nodes %v, want two distinct", len(located), located)
	}
	mod := c.moduleNode(t, "guarded.py")
	defined := c.targets(model.RelDefines, mod.ID)
	n := 0
	for _, name := range defined {
		if name == "f" {
			n++
		}
	}
	if n != 2 {
		t.Errorf("the module defines %v, want both f", defined)
	}
	for _, f := range located {
		if f.QualifiedName != "f" {
			t.Errorf("f is qualified %q, want f", f.QualifiedName)
		}
	}
	if got := c.targets(model.RelExports, mod.ID); !slices.Equal(got, []string{"f"}) {
		t.Errorf("exports %v, want only the top-level f", got)
	}
	guard := c.node(t, model.NodeEndpoint, "__main__")
	if got := c.targets(model.RelRoutesTo, guard.ID); !slices.Contains(got, "f") {
		t.Errorf("the guard routes to %v, want f", got)
	}
}

// TestImportsBindTheirConventionalNames asserts the local names two import
// forms bind, through the receiver of a call: a Go import whose path ends in
// a major version (`.../chi/v5`) or a gopkg.in version suffix (`yaml.v3`)
// binds the package's name, so `chi.NewRouter()` and `yaml.Marshal()` are
// calls through an import; and a `require(...)` bound to a name or
// destructured binds those names, so `x.go()` and `c.run()` are too.
//
// Mutations it catches: take the path's last element as the Go name (`v5`,
// `yaml.v3`); bind no name for a `require` declarator, or ignore a
// destructured pair's local name.
func TestImportsBindTheirConventionalNames(t *testing.T) {
	p := newProvider(t)
	for _, tc := range []struct {
		file, src string
		callees   []string
	}{
		{"main.go", `package main

import (
	"github.com/go-chi/chi/v5"
	"gopkg.in/yaml.v3"
)

func main() {
	chi.NewRouter()
	yaml.Marshal(nil)
}
`, []string{"NewRouter", "Marshal"}},
		{"req.js", `const x = require("y");
const { a, b: c } = require("z");

function f() {
	x.go();
	a.run();
	c.run();
}
`, []string{"go", "run"}},
	} {
		t.Run(tc.file, func(t *testing.T) {
			c := extracted(t, p, tc.file, tc.src)
			for _, name := range tc.callees {
				var seen bool
				for _, n := range c.nodes {
					if m := metadata(t, n.Node); m["callee"] == name {
						seen = true
						if m["resolution"] != "import" {
							t.Errorf("%s: the callee reference %s says %v, want import", tc.file, n.Node.Name, m)
						}
					}
				}
				if !seen {
					t.Errorf("%s: no callee reference for %s", tc.file, name)
				}
			}
		})
	}
}
