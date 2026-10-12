package worker

import (
	"slices"
	"testing"

	"github.com/Sawmonabo/codectx/internal/provider/treesitter/wire"
)

// tokenAt is the span of the first n bytes of the nth occurrence of needle.
func tokenAt(t *testing.T, src, needle string, nth, n int) wire.Span {
	t.Helper()
	s := textAt(t, src, needle, nth)
	return wire.Span{Start: s.Start, End: s.Start + uint32(n)}
}

// TestJavaScriptArgumentsReadTheirOwnVariables protects the argument facts a
// call's flow into its callee's parameters is built from: each argument is
// where it is written, a spread `...xs` is positional at xs, and each reads
// exactly the named variables its own expression names, by the identifier
// that declares them, a shorthand property `{y}` included. A literal reads
// nothing, and neither does a call's result `h(y)` nor a global `o.k`.
//
// Mutation: Reads from the whole statement instead of the argument -> the
// literal `1` of the first call reads p, y and xs.
func TestJavaScriptArgumentsReadTheirOwnVariables(t *testing.T) {
	const src = "function g(a, b, ...rest) { return a; }\n" +
		"function h(v) { return v; }\n" +
		"function f(p) {\n" +
		"  const xs = [p];\n" +
		"  let y = 2;\n" +
		"  g(p + y, ...xs, 1);\n" +
		"  g(1);\n" +
		"  g(h(y), {y}, o.k);\n" +
		"}\n"
	l, fns := linkOf(t, "javascript", "a.js", src)
	p, xs, y := tokenAt(t, src, "p)", 0, 1), tokenAt(t, src, "xs = [p]", 0, 2), tokenAt(t, src, "y = 2", 0, 1)
	wantArguments(t, src, fns, siteNamed(l, tokenAt(t, src, "g(p + y", 0, 1)), []argWant{
		{at: "p + y", reads: []wire.Span{p, y}},
		{at: "xs", spread: wire.SpreadPositional, reads: []wire.Span{xs}},
		{at: "1"},
	})
	wantArguments(t, src, fns, siteNamed(l, tokenAt(t, src, "g(1)", 0, 1)), []argWant{{at: "1"}})
	wantArguments(t, src, fns, siteNamed(l, tokenAt(t, src, "g(h(y)", 0, 1)), []argWant{
		{at: "h(y)"},
		{at: "{y}", reads: []wire.Span{y}},
		{at: "o.k"},
	})
}

// TestJavaScriptDynamicImportsFoldOrKeepFragments protects the open import
// sites: a require or import whose path is an expression sends its literal
// parts, and one every part of which is literal folds to the one path it
// names, an ordinary import. A literal `require("./lit")` or
// `import("./lit")` is the structural query's import record and is not sent
// again.
//
// Mutation: treat every require as open -> `require("./x/" + "y")` is sent
// with fragments and not Constant. Mutation: send a literal require or a
// literal import too -> five entries become six or seven.
func TestJavaScriptDynamicImportsFoldOrKeepFragments(t *testing.T) {
	const src = "const name = process.argv[2];\n" +
		"const a = require(\"./x/\" + name);\n" +
		"const b = require(\"./x/\" + \"y\");\n" +
		"const c = require(\"./lit\");\n" +
		"import(\"./m\");\n" +
		"import(\"./\" + \"n\");\n" +
		"import(`./t/${name}.js`);\n" +
		"require(name);\n"
	l, _ := linkOf(t, "javascript", "a.js", src)
	want := []struct {
		at        string
		fragments []string
		constant  bool
	}{
		{at: "require(\"./x/\" + name)", fragments: []string{"./x/"}},
		{at: "require(\"./x/\" + \"y\")", fragments: []string{"./x/y"}, constant: true},
		{at: "import(\"./\" + \"n\")", fragments: []string{"./n"}, constant: true},
		{at: "import(`./t/${name}.js`)", fragments: []string{"./t/", ".js"}},
		{at: "require(name)"},
	}
	if len(l.DynamicImports) != len(want) {
		t.Fatalf("dynamic imports %+v, want %d", l.DynamicImports, len(want))
	}
	for i, d := range l.DynamicImports {
		w := want[i]
		if got := src[d.At.Start:d.At.End]; got != w.at || d.Constant != w.constant || len(d.Fragments) != len(w.fragments) ||
			(len(w.fragments) > 0 && !slices.Equal(d.Fragments, w.fragments)) {
			t.Fatalf("dynamic import %d is %q %q constant %v, want %q %q constant %v", i, got, d.Fragments, d.Constant,
				w.at, w.fragments, w.constant)
		}
	}
}

// TestTypeScriptOverrideMarksOnlyTheMarkedMethod protects the mark an
// override of an external method is published on: only a method written
// with the `override` modifier carries it. A TypeScript `this` parameter
// takes no position, so the first real parameter is 1 and the rest
// parameter is variadic.
//
// Mutation: mark every method -> A.m, A.n and B.n carry Override. Mutation:
// count the `this` parameter -> a is 2.
func TestTypeScriptOverrideMarksOnlyTheMarkedMethod(t *testing.T) {
	const src = "class A {\n  m(): void {}\n  n(): void {}\n}\n" +
		"class B extends A {\n  override m(): void {}\n  n(): void {}\n}\n" +
		"function h(this: object, a: number, ...r: number[]): void {}\n"
	l, _ := linkOf(t, "typescript", "a.ts", src)
	for _, c := range []struct {
		needle   string
		nth      int
		override bool
	}{{"m(): void", 0, false}, {"n(): void", 0, false}, {"m(): void", 1, true}, {"n(): void", 1, false}} {
		e := exportNamed(l, tokenAt(t, src, c.needle, c.nth, 1))
		if e == nil {
			t.Fatalf("no export for occurrence %d of %q", c.nth, c.needle)
		}
		if e.Override != c.override {
			t.Fatalf("occurrence %d of %q has Override %v, want %v", c.nth, c.needle, e.Override, c.override)
		}
	}
	h := tokenAt(t, src, "h(this", 0, 1)
	var got []wire.Parameter
	for _, p := range l.Parameters {
		if p.Callable == h {
			got = append(got, p)
		}
	}
	want := []wire.Parameter{
		{Callable: h, Name: tokenAt(t, src, "a: number", 0, 1), Index: 1},
		{Callable: h, Name: tokenAt(t, src, "r: number", 0, 1), Index: 2, Variadic: true},
	}
	if !slices.Equal(got, want) {
		t.Fatalf("parameters of h %+v, want %+v", got, want)
	}
}

// TestTSXComputedSitesAreTheCallsNoReferenceNames protects the computed
// sites: a call whose callee names nothing (a subscript, a call's result, a
// function called where it is written, a logical expression) is one
// computed site at its callee expression, with its arguments, and a call
// whose callee a call reference names is a site and never also computed.
//
// Mutation: also send a call that has a reference -> `o.m()`, `f()` and the
// inner `f()` of `f()()` are computed too and the count is seven.
func TestTSXComputedSitesAreTheCallsNoReferenceNames(t *testing.T) {
	const src = "const o: any = {};\n" +
		"const k = \"x\";\n" +
		"function f(): () => void { return () => {}; }\n" +
		"o[k](k);\n" +
		"f()();\n" +
		"(function () {})();\n" +
		"(o.a || o.b)();\n" +
		"o.m();\n" +
		"f();\n"
	l, fns := linkOf(t, "tsx", "a.tsx", src)
	want := []string{"o[k]", "f()", "(function () {})", "(o.a || o.b)"}
	if len(l.Computed) != len(want) {
		t.Fatalf("computed sites %+v, want %q", l.Computed, want)
	}
	for i := range l.Computed {
		c := &l.Computed[i]
		if got := src[c.Name.Start:c.Name.End]; got != want[i] || c.Shape != wire.ShapeIndirect || c.Receiver.Kind != wire.TypeUnknown {
			t.Fatalf("computed site %d is %q shape %d receiver %+v, want %q indirect", i, got, c.Shape, c.Receiver, want[i])
		}
		if siteNamed(l, c.Name) != nil {
			t.Fatalf("computed site %q is also a site", want[i])
		}
	}
	wantArguments(t, src, fns, &l.Computed[0], []argWant{{at: "k", reads: []wire.Span{tokenAt(t, src, "k = ", 0, 1)}}})
	for _, s := range []wire.Span{tokenAt(t, src, "m();", 0, 1), tokenAt(t, src, "f();", 0, 1), tokenAt(t, src, "f()();", 0, 1)} {
		if siteNamed(l, s) == nil {
			t.Fatalf("the call at %v that a reference names sent no site", s)
		}
	}
}
