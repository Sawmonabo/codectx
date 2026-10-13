package link_test

import (
	"testing"

	"github.com/Sawmonabo/codectx/internal/model"
	"github.com/Sawmonabo/codectx/internal/provider/link"
)

// TestOneFunctionValueKeepsItsTechnique pins, in every language with function
// values, that a call through a value bound to the one function the project
// gives it carries the technique that found it: address_taken for a site the
// static family joins to the functions of its type whose address the
// project takes, function_value for a site the flow family binds to the
// function values that reach it. The technique is what marks such a call as
// one of the project's own candidates (graph value_set), never a direct call.
//
// Mutations caught: binding a flowed function value under the receiver
// technique flow (the JavaScript, TypeScript and Python cases read flow);
// publishing an address-taken join under direct.
func TestOneFunctionValueKeepsItsTechnique(t *testing.T) {
	structural := newStructural(t)
	cargo := "[package]\nname = \"r\"\nversion = \"0.1.0\"\nedition = \"2021\"\n"
	cases := []struct {
		name   string
		files  map[string]string
		site   string
		detail string
	}{
		{"c", map[string]string{"c.c": "void f(int x) {} // @f\nvoid (*on_exit)(int) = f;\nvoid call(void) { on_exit(1); } // @call\n"},
			"call on_exit", link.DetailAddressTaken},
		{"cpp", map[string]string{"c.cpp": "void f(int x) {} // @f\nvoid (*on_exit)(int) = f;\nvoid call() { on_exit(1); } // @call\n"},
			"call on_exit", link.DetailAddressTaken},
		{"go", map[string]string{"go.mod": "module example.com/p\n\ngo 1.22\n",
			"p.go": "package p\n\nfunc f(x int) {} // @f\n\nvar g func(int) = f\n\nfunc call() { g(1) } // @call\n"},
			"call g", link.DetailAddressTaken},
		{"rust", map[string]string{"Cargo.toml": cargo,
			"src/lib.rs": "fn f(x: i32) {} // @f\nfn run(g: fn(i32)) { g(1) } // @call\nfn top() { run(f) }\n"},
			"call g", link.DetailAddressTaken},
		{"javascript", map[string]string{"c.js": "function f(x) {} // @f\nfunction run(cb) { cb(1); } // @call\nrun(f);\n"},
			"call cb", link.DetailFunctionValue},
		{"typescript", map[string]string{"c.ts": "function f(x: number) {} // @f\nfunction run(cb: (x: number) => void) { cb(1); } // @call\nrun(f);\n"},
			"call cb", link.DetailFunctionValue},
		{"python", map[string]string{"c.py": "def f(x):  # @f\n    pass\n\n\ndef run(cb):\n    cb(1)  # @call\n\n\nrun(f)\n"},
			"call cb", link.DetailFunctionValue},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			l := linkSources(t, structural, c.files)
			var got []string
			for _, r := range l.out.rels {
				if r.Relation.Kind != model.RelCalls {
					continue
				}
				for _, e := range r.Evidence {
					if l.site(t, e.NativeKey) == c.site {
						got = append(got, e.Detail+" "+string(e.Dispatch)+setSuffix(e))
					}
				}
			}
			if len(got) != 1 || got[0] != c.detail+" exact" {
				t.Fatalf("%s binds %q, want the one function f by %s, exact", c.site, got, c.detail)
			}
		})
	}
}
