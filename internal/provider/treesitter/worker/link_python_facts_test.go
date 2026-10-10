package worker

import (
	"slices"
	"testing"

	"github.com/Sawmonabo/codectx/internal/provider/treesitter/wire"
)

// pyTok is the span of n bytes at offset off into the nth occurrence of
// needle in src: one token of a longer, unambiguous needle.
func pyTok(t *testing.T, src, needle string, nth, off, n int) wire.Span {
	t.Helper()
	s := textAt(t, src, needle, nth)
	return wire.Span{Start: s.Start + uint32(off), End: s.Start + uint32(off+n)}
}

// TestPythonArgumentsReadTheirOwnVariables pins what each argument of a
// Python call sends: the expression without its keyword or splat marker, the
// keyword, the splat, and the parameters or locals that expression alone
// reads, spelled as the function messages spell them. A call result, a
// literal, an attribute's name, a lambda's body and a comprehension's own
// targets read nothing of the caller; a lambda's default and a
// comprehension's first iterable are read where the argument is evaluated;
// a closure reads the enclosing function's parameter.
//
// Mutation: Reads from the whole statement instead of the argument -> the
// first argument `x + 1` reads xs, n and kw too. Mutation: read an
// attribute's name or descend into a call -> `n.attr` or `len(x)` reads more.
func TestPythonArgumentsReadTheirOwnVariables(t *testing.T) {
	const src = `def g(a, *rest, key=None, **opts):
    return a


def f(x, xs, kw, n):
    g(x + 1, *xs, key=n.attr, **kw)
    g(1)
    g(len(x), x.y)
    g(lambda v=x: v, [z for z in xs])


def h(y):
    def inner():
        g(y)
    return inner
`
	l, fns := linkOf(t, "python", "m.py", src)
	params := "f(x, xs, kw, n)"
	x, xs, kw, n := pyTok(t, src, params, 0, 2, 1), pyTok(t, src, params, 0, 5, 2), pyTok(t, src, params, 0, 9, 2),
		pyTok(t, src, params, 0, 13, 1)
	y := pyTok(t, src, "h(y)", 0, 2, 1)
	call := func(needle string) *wire.Site { return siteNamed(l, pyTok(t, src, needle, 0, 0, 1)) }

	wantArguments(t, src, fns, call("g(x + 1"), []argWant{
		{at: "x + 1", reads: []wire.Span{x}},
		{at: "xs", spread: wire.SpreadPositional, reads: []wire.Span{xs}},
		{at: "n.attr", keyword: "key", reads: []wire.Span{n}},
		{at: "kw", spread: wire.SpreadKeyword, reads: []wire.Span{kw}},
	})
	wantArguments(t, src, fns, call("g(1)"), []argWant{{at: "1"}})
	wantArguments(t, src, fns, call("g(len"), []argWant{{at: "len(x)"}, {at: "x.y", reads: []wire.Span{x}}})
	wantArguments(t, src, fns, call("g(lambda"), []argWant{
		{at: "lambda v=x: v", reads: []wire.Span{x}},
		{at: "[z for z in xs]", reads: []wire.Span{xs}},
	})
	wantArguments(t, src, fns, call("g(y)"), []argWant{{at: "y", reads: []wire.Span{y}}})
}

// TestPythonParameterMarks pins each declared parameter's position and
// marks: self and cls at 0, the first parameter after them at 1, `*args`
// variadic, a parameter after `*args` or a bare `*` keyword-only, `**kw`
// variadic and keyword-only, and a static method's first parameter at 1.
//
// Mutation: count the bare `*` separator as a parameter -> q is at 3.
// Mutation: drop the keyword-only mark of `**kw` -> kw is positional.
func TestPythonParameterMarks(t *testing.T) {
	const src = `class C:
    def m(self, a, *args, b, **kw):
        pass

    @classmethod
    def c(cls, p, *, q):
        pass

    @staticmethod
    def s(u):
        pass
`
	l, _ := linkOf(t, "python", "m.py", src)
	m, c, s := pyTok(t, src, "m(self", 0, 0, 1), pyTok(t, src, "c(cls", 0, 0, 1), pyTok(t, src, "s(u)", 0, 0, 1)
	tok := func(needle string, off, n int) wire.Span { return pyTok(t, src, needle, 0, off, n) }
	want := []wire.Parameter{
		{Callable: m, Name: tok("self, a", 0, 4), Index: 0},
		{Callable: m, Name: tok("a, *args", 0, 1), Index: 1},
		{Callable: m, Name: tok("args, b", 0, 4), Index: 2, Variadic: true},
		{Callable: m, Name: tok("b, **kw", 0, 1), Index: 3, KeywordOnly: true},
		{Callable: m, Name: tok("kw)", 0, 2), Index: 4, Variadic: true, KeywordOnly: true},
		{Callable: c, Name: tok("cls, p", 0, 3), Index: 0},
		{Callable: c, Name: tok("p, *, q", 0, 1), Index: 1},
		{Callable: c, Name: tok("q)", 0, 1), Index: 2, KeywordOnly: true},
		{Callable: s, Name: tok("u)", 0, 1), Index: 1},
	}
	if !slices.Equal(l.Parameters, want) {
		t.Fatalf("parameters\n got %+v\nwant %+v", l.Parameters, want)
	}
}

// TestPythonDynamicImports pins the imports whose module name is an
// expression: importlib.import_module through the module and through a name
// imported from it, and the builtin __import__. The literal fragments of a
// `+` concatenation, an f-string and a `%` format are sent in order; a
// literal path folds to a constant; a bare variable sends no fragment.
//
// Mutation: treat every import_module call as open -> the literal
// "pkg.fixed" is sent without Constant. Mutation: fold `"pkg." + n` as a
// constant -> its import is Constant.
func TestPythonDynamicImports(t *testing.T) {
	const src = `import importlib
from importlib import import_module


def load(n):
    importlib.import_module("pkg." + n)
    importlib.import_module("pkg.fixed")
    import_module(f"plugins.{n}.main")
    __import__("%s.mod" % n)
    importlib.import_module(n)
`
	l, _ := linkOf(t, "python", "m.py", src)
	type want struct {
		at        string
		fragments []string
		constant  bool
	}
	wants := []want{
		{`importlib.import_module("pkg." + n)`, []string{"pkg."}, false},
		{`importlib.import_module("pkg.fixed")`, []string{"pkg.fixed"}, true},
		{`import_module(f"plugins.{n}.main")`, []string{"plugins.", ".main"}, false},
		{`__import__("%s.mod" % n)`, []string{".mod"}, false},
		{`importlib.import_module(n)`, []string{}, false},
	}
	if len(l.DynamicImports) != len(wants) {
		t.Fatalf("%d dynamic imports %+v, want %d", len(l.DynamicImports), l.DynamicImports, len(wants))
	}
	for i, w := range wants {
		d := l.DynamicImports[i]
		if d.At != textAt(t, src, w.at, 0) || !slices.Equal(d.Fragments, w.fragments) || d.Constant != w.constant {
			t.Fatalf("dynamic import %d is %q %q constant %v, want %q %q constant %v", i, src[d.At.Start:d.At.End],
				d.Fragments, d.Constant, w.at, w.fragments, w.constant)
		}
	}
}

// TestPythonOverrideMarks pins the methods marked as overriding: a method
// decorated with `override` imported from typing_extensions, or with
// `typing.override` through the typing module, and no other: an unmarked
// method, a supertype's method, and a decorator named override that comes
// from elsewhere.
//
// Mutation: mark every method -> Base.run is marked. Mutation: mark a bare
// `@override` whatever binds it -> D.run is marked.
func TestPythonOverrideMarks(t *testing.T) {
	const src = `import typing
from typing_extensions import override


class Base:
    def run(self):
        pass

    def stop(self):
        pass


class Child(Base):
    @override
    def run(self):
        pass

    @typing.override
    def stop(self):
        pass

    def plain(self):
        pass
`
	l, _ := linkOf(t, "python", "m.py", src)
	cases := []struct {
		name     wire.Span
		override bool
	}{
		{pyTok(t, src, "run(self)", 0, 0, 3), false},
		{pyTok(t, src, "stop(self)", 0, 0, 4), false},
		{pyTok(t, src, "run(self)", 1, 0, 3), true},
		{pyTok(t, src, "stop(self)", 1, 0, 4), true},
		{pyTok(t, src, "plain(self)", 0, 0, 5), false},
	}
	for i, c := range cases {
		x := exportNamed(l, c.name)
		if x == nil || x.Override != c.override {
			t.Fatalf("method %d (%s): export %+v, want override %v", i, src[c.name.Start:c.name.End], x, c.override)
		}
	}

	const other = `from mylib import override


class D:
    @override
    def run(self):
        pass
`
	l, _ = linkOf(t, "python", "d.py", other)
	if x := exportNamed(l, pyTok(t, other, "run(self)", 0, 0, 3)); x == nil || x.Override {
		t.Fatalf("a method decorated with an override not from typing: export %+v, want no override mark", x)
	}
}

// TestPythonComputedCalls pins the calls whose callee names nothing: a
// subscript, a call's result, a parenthesized boolean expression and a
// lambda called where it is written are each one computed site, its name the
// callee expression, indirect, with its arguments; a call of a name or of an
// attribute, which the structural query references, is a site and never
// computed.
//
// Mutation: also send a call that has a reference -> `f(a)` or `obj.m(a)`
// is computed too.
func TestPythonComputedCalls(t *testing.T) {
	const src = `def f(obj, k, a, b):
    obj[k](a)
    f(a)(b)
    (a or b)()
    (lambda: 0)()
    obj.m(a)
`
	l, fns := linkOf(t, "python", "m.py", src)
	params := "f(obj, k, a, b)"
	a, b := pyTok(t, src, params, 0, 10, 1), pyTok(t, src, params, 0, 13, 1)
	names := []string{"obj[k]", "f(a)", "(a or b)", "(lambda: 0)"}
	if len(l.Computed) != len(names) {
		t.Fatalf("%d computed sites %+v, want %d", len(l.Computed), l.Computed, len(names))
	}
	for i, want := range names {
		c := l.Computed[i]
		if c.Name != textAt(t, src, want, 0) || c.Shape != wire.ShapeIndirect || c.Receiver != (wire.Type{}) {
			t.Fatalf("computed site %d is %q shape %d receiver %+v, want %q indirect with no receiver", i,
				src[c.Name.Start:c.Name.End], c.Shape, c.Receiver, want)
		}
	}
	wantArguments(t, src, fns, &l.Computed[0], []argWant{{at: "a", reads: []wire.Span{a}}})
	wantArguments(t, src, fns, &l.Computed[1], []argWant{{at: "b", reads: []wire.Span{b}}})
	wantArguments(t, src, fns, &l.Computed[2], nil)
	for _, s := range []wire.Span{pyTok(t, src, "f(a)", 0, 0, 1), pyTok(t, src, "m(a)", 0, 0, 1)} {
		if siteNamed(l, s) == nil {
			t.Fatalf("the referenced call at %q sent no site", src[s.Start:s.End])
		}
	}
}
