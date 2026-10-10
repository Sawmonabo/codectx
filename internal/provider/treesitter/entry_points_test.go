package treesitter_test

import (
	"encoding/json"
	"slices"
	"testing"

	"github.com/Sawmonabo/codectx/internal/model"
)

// decoratorsOf decodes the decorators published on the one node of kind
// named name, as each one's name, its whole text and each argument's text.
func decoratorsOf(t *testing.T, c *capture, src string, kind model.NodeKind, name string) [][]string {
	t.Helper()
	raw, err := json.Marshal(metadata(t, c.node(t, kind, name))[model.MetaDecorators])
	if err != nil {
		t.Fatal(err)
	}
	var decs []model.Decorator
	if err := json.Unmarshal(raw, &decs); err != nil {
		t.Fatalf("%s decorators %s: %v", name, raw, err)
	}
	var out [][]string
	for _, d := range decs {
		row := []string{d.Name, src[d.Bytes.Start:d.Bytes.End]}
		for _, a := range d.Arguments {
			row = append(row, src[a.Start:a.End])
		}
		out = append(out, row)
	}
	return out
}

// endpoints is every published endpoint node.
func (c *capture) endpoints() []model.Node {
	var out []model.Node
	for _, n := range c.nodes {
		if n.Node.Kind == model.NodeEndpoint {
			out = append(out, n.Node)
		}
	}
	return out
}

// TestDecoratorsArePublishedWithTheirArgumentRanges indexes a decorated
// Python function, an annotated Java method and a decorated TypeScript class
// and method, and asserts each declaration's decorators are published in
// source order with the name as written, the decorator's whole range and the
// range of each argument, a keyword argument and an element-value pair
// including their keys.
//
// Mutation it catches: drop the arguments (publish the decorator's name and
// range alone) -> every argument list is empty.
func TestDecoratorsArePublishedWithTheirArgumentRanges(t *testing.T) {
	p := newProvider(t)
	cases := []struct {
		file, src string
		kind      model.NodeKind
		name      string
		want      [][]string
	}{
		{"views.py", `@app.route("/users", methods=["GET"])
@login_required
def users():
    pass
`, model.NodeFunction, "users", [][]string{
			{"app.route", `@app.route("/users", methods=["GET"])`, `"/users"`, `methods=["GET"]`},
			{"login_required", "@login_required"},
		}},
		{"Api.java", `class Api {
    @Override
    @GetMapping(path = "/x", produces = "json")
    public String list() { return ""; }
}
`, model.NodeMethod, "list", [][]string{
			{"Override", "@Override"},
			{"GetMapping", `@GetMapping(path = "/x", produces = "json")`, `path = "/x"`, `produces = "json"`},
		}},
		{"widget.ts", `@Component({ selector: "x" })
class Widget {
    @HostListener("click", ["$event"])
    render() {}
}
`, model.NodeClass, "Widget", [][]string{
			{"Component", `@Component({ selector: "x" })`, `{ selector: "x" }`},
		}},
		{"widget.ts", `@Component({ selector: "x" })
class Widget {
    @HostListener("click", ["$event"])
    render() {}
}
`, model.NodeMethod, "render", [][]string{
			{"HostListener", `@HostListener("click", ["$event"])`, `"click"`, `["$event"]`},
		}},
	}
	for _, tc := range cases {
		t.Run(tc.file+"/"+tc.name, func(t *testing.T) {
			c := extracted(t, p, tc.file, tc.src)
			if got := decoratorsOf(t, c, tc.src, tc.kind, tc.name); !slices.EqualFunc(got, tc.want, slices.Equal) {
				t.Errorf("decorators %q, want %q", got, tc.want)
			}
		})
	}
}

// TestMainGuardIsAnEndpointThatRoutesToWhatItCalls asserts a module-level
// `if __name__ == "__main__":`, in either operand order, is one endpoint
// that routes_to the function its block calls and is where that call comes
// from, while a comparison of `__name__` with another string, or by another
// operator, is no endpoint.
//
// Mutation it catches: treat any `if` on `__name__` as a guard -> the
// `"other"` and `!=` files publish an endpoint.
func TestMainGuardIsAnEndpointThatRoutesToWhatItCalls(t *testing.T) {
	p := newProvider(t)
	const defs = "def main():\n    pass\n\ndef other():\n    pass\n\n"
	for _, cond := range []string{`__name__ == "__main__"`, `"__main__" == __name__`} {
		t.Run(cond, func(t *testing.T) {
			c := extracted(t, p, "app.py", defs+"if "+cond+":\n    main()\n")
			eps := c.endpoints()
			if len(eps) != 1 || eps[0].Name != "__main__" {
				t.Fatalf("endpoints %v, want the one __main__ guard", eps)
			}
			if got := c.targets(model.RelRoutesTo, eps[0].ID); !slices.Equal(got, []string{"main"}) {
				t.Errorf("guard routes_to %v, want [main]", got)
			}
			if got := c.targets(model.RelCalls, eps[0].ID); !slices.Equal(got, []string{"main"}) {
				t.Errorf("guard calls %v, want [main]", got)
			}
			if got := c.targets(model.RelCalls, c.moduleNode(t, "app.py").ID); len(got) != 0 {
				t.Errorf("module calls %v, want none: the guard's call is made from the guard", got)
			}
		})
	}
	for _, cond := range []string{`__name__ == "other"`, `__name__ != "__main__"`} {
		t.Run(cond, func(t *testing.T) {
			c := extracted(t, p, "app.py", defs+"if "+cond+":\n    main()\n")
			if eps := c.endpoints(); len(eps) != 0 {
				t.Errorf("endpoints %v, want none", eps)
			}
		})
	}
}

// TestGoMainIsAnEndpointOnlyInPackageMain asserts a top-level `func main`
// in `package main` is a `main` endpoint that routes_to it, and the same
// function in any other package is none.
//
// Mutation it catches: drop the package check -> package tool publishes an
// endpoint.
func TestGoMainIsAnEndpointOnlyInPackageMain(t *testing.T) {
	p := newProvider(t)
	const body = "\n\nfunc main() { run() }\n\nfunc run() {}\n"
	c := extracted(t, p, "cmd/main.go", "package main"+body)
	ep := c.node(t, model.NodeEndpoint, "main")
	if got := c.targets(model.RelRoutesTo, ep.ID); !slices.Equal(got, []string{"main"}) {
		t.Errorf("main routes_to %v, want [main]", got)
	}
	if eps := extracted(t, p, "tool/main.go", "package tool"+body).endpoints(); len(eps) != 0 {
		t.Errorf("package tool endpoints %v, want none", eps)
	}
}

// TestJavaMainFormsAreTheLanguageSpecifications asserts the JLS SE 25
// §12.1.4 forms: an instance `main()` with no parameters is an endpoint, a
// class inheriting a `main` from a superclass of the same file is one that
// routes to the inherited method, and a `private static void main(String[])`
// is none.
//
// Mutations they catch: require `static` -> the instance main is no
// endpoint; drop inheritance -> Child has none; ignore `private` -> the
// private main is one.
func TestJavaMainFormsAreTheLanguageSpecifications(t *testing.T) {
	p := newProvider(t)
	c := extracted(t, p, "Hello.java", "class Hello {\n    void main() { System.out.println(\"hi\"); }\n}\n")
	ep := c.node(t, model.NodeEndpoint, "main")
	if got := c.targets(model.RelRoutesTo, ep.ID); !slices.Equal(got, []string{"main"}) {
		t.Errorf("Hello's main routes_to %v, want [main]", got)
	}

	c = extracted(t, p, "Base.java", "class Base {\n    public static void main(String[] args) {}\n}\nclass Child extends Base {}\n")
	eps := c.endpoints()
	if len(eps) != 2 {
		t.Fatalf("endpoints %v, want Base's and Child's", eps)
	}
	baseMain := c.node(t, model.NodeMethod, "main")
	for _, e := range eps {
		var to []model.NodeID
		for _, r := range c.rels {
			if r.Relation.Kind == model.RelRoutesTo && r.Relation.From == e.ID {
				to = append(to, r.Relation.To)
			}
		}
		if !slices.Equal(to, []model.NodeID{baseMain.ID}) {
			t.Errorf("endpoint %s routes_to %v, want Base.main %s", e.ID, to, baseMain.ID)
		}
	}

	if eps := extracted(t, p, "Hidden.java", "class Hidden {\n    private static void main(String[] a) {}\n}\n").endpoints(); len(eps) != 0 {
		t.Errorf("private main endpoints %v, want none", eps)
	}
}

// TestShebangFileIsAScriptEndpoint asserts a file whose first bytes are
// `#!` is a `script` endpoint that routes_to the file's module, and that a
// Rust inner attribute `#![...]` is no shebang.
//
// Mutation it catches: route the script to nothing, or read `#![` as a
// shebang -> the Rust file publishes a script endpoint.
func TestShebangFileIsAScriptEndpoint(t *testing.T) {
	p := newProvider(t)
	c := extracted(t, p, "bin/run.py", "#!/usr/bin/env python3\nprint(\"x\")\n")
	ep := c.node(t, model.NodeEndpoint, "script")
	if got := c.targets(model.RelRoutesTo, ep.ID); !slices.Equal(got, []string{c.moduleNode(t, "bin/run.py").Name}) {
		t.Errorf("script routes_to %v, want the module", got)
	}
	if got := extracted(t, p, "lib.rs", "#![allow(dead_code)]\nfn helper() {}\n").all(model.NodeEndpoint, "script"); len(got) != 0 {
		t.Errorf("inner attribute read as a shebang: %v", got)
	}
}
