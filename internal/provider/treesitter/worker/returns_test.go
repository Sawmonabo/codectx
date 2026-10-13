package worker

import (
	"slices"
	"testing"

	"github.com/Sawmonabo/codectx/internal/provider/treesitter/wire"
)

// TestReturningNodes pins which nodes a callable returns its value from and
// which variables reach them: a return statement's node, the expression body
// of an arrow or lambda, a Rust block's tail; never a statement body's last
// statement, a Rust statement ended by `;`, or the end of a Rust let-else
// block, which diverges. The goldens come from the pinned grammars.
func TestReturningNodes(t *testing.T) {
	cases := []struct {
		name, lang, path, src, fn string
		returns, returned         []string
	}{
		// mutation: drop Graph.Returns from returning (a return statement
		// returns nothing).
		{"python return", "python", "a.py", "def f(p):\n    k = 1\n    y = p + k\n    return y\n",
			"def f(p):\n    k = 1\n    y = p + k\n    return y", []string{"return y"}, []string{"y"}},
		// mutation: ignore the falls of an expression body (the lambda
		// returns nothing).
		{"python lambda", "python", "a.py", "g = lambda a: a * 2\n", "lambda a: a * 2", []string{"a * 2"}, []string{"a"}},
		{"arrow expression body", "javascript", "a.js", "const g = (a) => a * 2;\n", "(a) => a * 2",
			[]string{"a * 2"}, []string{"a"}},
		// mutation: treat every callable's falls as returning (a block
		// body's last statement would return b).
		{"arrow block body", "javascript", "a.js", "const h = (b) => { b; };\n", "(b) => { b; }", nil, nil},
		{"java lambda", "java", "A.java", "class A { java.util.function.IntUnaryOperator g = (a) -> a * 2; }\n",
			"(a) -> a * 2", []string{"a * 2"}, []string{"a"}},
		{"rust tail", "rust", "a.rs", "fn f(p: i32) -> i32 { let y = p + 1; y }\n",
			"fn f(p: i32) -> i32 { let y = p + 1; y }", []string{"y"}, []string{"y"}},
		// mutation: count an expression statement with its `;` as the tail.
		{"rust statement", "rust", "a.rs", "fn g(p: i32) { let y = p; y; }\n", "fn g(p: i32) { let y = p; y; }", nil, nil},
		// mutation: let-else's else block returns (Diverge replaced by
		// Return), so the panic's node would return. The pattern's test
		// decides whether the tail runs, so o reaches it too.
		{"rust let-else", "rust", "a.rs", "fn e(o: Option<i32>) -> i32 { let Some(x) = o else { panic!(\"{}\", o) }; x }\n",
			"fn e(o: Option<i32>) -> i32 { let Some(x) = o else { panic!(\"{}\", o) }; x }", []string{"x"}, []string{"o", "x"}},
		// mutation: leave the deciding nodes' variables out of Returned (p
		// and q, which only choose the value returned, would reach no
		// return); a deciding node shared by both returns is cited once.
		{"python deciding", "python", "a.py", "def c(p, q):\n    if p:\n        if q:\n            return 1\n    return 2\n",
			"def c(p, q):\n    if p:\n        if q:\n            return 1\n    return 2", []string{"return 1", "return 2"},
			[]string{"p", "q"}},
		// A Go bare return reads the named results.
		{"go bare return", "go", "a.go", "package p\nfunc f(p int) (r int) { r = p; return }\n",
			"func f(p int) (r int) { r = p; return }", []string{"return"}, []string{"r"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, fns := linkOf(t, c.lang, c.path, c.src)
			i := slices.IndexFunc(fns, func(f wire.Function) bool { return c.src[f.Span.Start:f.Span.End] == c.fn })
			if i < 0 {
				t.Fatalf("no function message spans %q", c.fn)
			}
			f := fns[i]
			var returns, returned []string
			for _, r := range f.Returns {
				returns = append(returns, c.src[f.Nodes[r].Start:f.Nodes[r].End])
			}
			for _, a := range f.Returned {
				if !slices.Contains(f.Returns, a.Node) {
					t.Fatalf("variable %d reaches node %d, which is not a returning node", a.Var, a.Node)
				}
				returned = append(returned, c.src[f.Vars[a.Var].Start:f.Vars[a.Var].End])
			}
			if !slices.Equal(returns, c.returns) || !slices.Equal(returned, c.returned) {
				t.Fatalf("returning nodes %q reached by %q, want %q reached by %q", returns, returned, c.returns, c.returned)
			}
		})
	}
}
