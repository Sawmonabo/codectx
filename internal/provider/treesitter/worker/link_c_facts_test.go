package worker

import (
	"strings"
	"testing"

	"github.com/Sawmonabo/codectx/internal/provider/treesitter/wire"
)

// cTokenAt is the span of the first len(token) bytes of the nth occurrence of
// context in src: a token located by the text around it.
func cTokenAt(t *testing.T, src, context string, nth int, token string) wire.Span {
	t.Helper()
	if !strings.HasPrefix(context, token) {
		t.Fatalf("context %q does not start with %q", context, token)
	}
	s := textAt(t, src, context, nth)
	return wire.Span{Start: s.Start, End: s.Start + uint32(len(token))}
}

// TestCArgumentsReadTheirOwnVariables checks that each argument of a C call
// reads the variables its own expression names, resolved to the identifiers
// that declare them: a parameter and a local through an operator, a struct
// local through a member access, and nothing through a call's result or a
// literal.
//
// Mutation: Reads from the whole statement instead of the argument -> `s.f`
// also reads x and argc, and `h()` reads all three.
// Mutation: read the identifiers inside a call argument -> `h(x)` reads x.
func TestCArgumentsReadTheirOwnVariables(t *testing.T) {
	const src = `int g(int a, int b, int c);
int h(int v);
struct S { int f; };
int main(int argc, char **argv) {
	int x = 1;
	struct S s;
	s.f = 2;
	return g(x + argc, s.f, h(x));
}
void lit(void) {
	g(1, 2, 3);
}
`
	l, fns := linkOf(t, "c", "x.c", src)
	x := cTokenAt(t, src, "x = 1", 0, "x")
	argc := cTokenAt(t, src, "argc", 0, "argc")
	s := cTokenAt(t, src, "s;", 0, "s")
	wantArguments(t, src, fns, siteNamed(l, cTokenAt(t, src, "g(x + argc", 0, "g")), []argWant{
		{at: "x + argc", reads: []wire.Span{x, argc}},
		{at: "s.f", reads: []wire.Span{s}},
		{at: "h(x)"},
	})
	wantArguments(t, src, fns, siteNamed(l, cTokenAt(t, src, "g(1, 2, 3)", 0, "g")), []argWant{{at: "1"}, {at: "2"}, {at: "3"}})
}

// TestCppArgumentsSkipLambdasAndCalls checks C++ arguments: a lambda passed
// as a value reads nothing at the call (its body runs later), a member
// call's result reads nothing, and an arithmetic argument reads the
// parameter it names.
//
// Mutation: Reads from the whole statement instead of the argument -> the
// lambda and `d.plain()` read y and d.
// Mutation: enter a lambda's body -> the lambda argument reads y.
func TestCppArgumentsSkipLambdasAndCalls(t *testing.T) {
	const src = `struct D { int plain() { return 2; } };
template <class F> int apply(F f, int v) { return f(v); }
int k(int a, int b);
int use(D &d, int y) {
	int r = apply([&](int z) { return z + y; }, y);
	return k(y * 2, d.plain()) + r;
}
`
	l, fns := linkOf(t, "cpp", "x.cpp", src)
	y := cTokenAt(t, src, "y) {", 0, "y")
	wantArguments(t, src, fns, siteNamed(l, cTokenAt(t, src, "apply([&]", 0, "apply")), []argWant{
		{at: "[&](int z) { return z + y; }"},
		{at: "y", reads: []wire.Span{y}},
	})
	wantArguments(t, src, fns, siteNamed(l, cTokenAt(t, src, "k(y * 2", 0, "k")), []argWant{
		{at: "y * 2", reads: []wire.Span{y}},
		{at: "d.plain()"},
	})
}

// TestCVariadicParameterIsSent checks that a C `...` is a Variadic parameter
// at its own token, after the named parameters it follows.
//
// Mutation: skip the `...` as a parameter -> the variadic parameter is
// missing.
func TestCVariadicParameterIsSent(t *testing.T) {
	const src = "int pr(const char *fmt, ...);\n"
	l, _ := linkOf(t, "c", "x.c", src)
	callable := cTokenAt(t, src, "pr(", 0, "pr")
	want := []wire.Parameter{
		{Callable: callable, Name: cTokenAt(t, src, "fmt", 0, "fmt"), Index: 1},
		{Callable: callable, Name: cTokenAt(t, src, "...", 0, "..."), Index: 2, Variadic: true},
	}
	if len(l.Parameters) != len(want) {
		t.Fatalf("parameters %+v, want %+v", l.Parameters, want)
	}
	for i := range want {
		if l.Parameters[i] != want[i] {
			t.Fatalf("parameter %d is %+v, want %+v", i, l.Parameters[i], want[i])
		}
	}
}

// TestCIncludeOfAMacro checks `#include NAME`: a name defined before the
// directive as a string literal folds to that path without its quotes, as
// the structural import record spells `#include "a.h"`; one defined as a
// header name keeps its brackets; an undefined name is an open import with
// no fragments.
//
// Mutation: treat every include of a macro as open -> the defined ones are
// not Constant.
// Mutation: fold an undefined name to itself -> the undefined one is
// Constant.
func TestCIncludeOfAMacro(t *testing.T) {
	cases := []struct {
		name, src, directive string
		constant             bool
		fragments            []string
	}{
		{name: "a string literal", src: "#define HEADER \"a.h\"\n#include HEADER\n", directive: "#include HEADER",
			constant: true, fragments: []string{"a.h"}},
		{name: "a header name", src: "#define SYS <stddef.h>\n#include SYS\n", directive: "#include SYS",
			constant: true, fragments: []string{"<stddef.h>"}},
		{name: "an undefined name", src: "#include HEADER\nint x;\n", directive: "#include HEADER"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			l, _ := linkOf(t, "c", "x.c", tc.src)
			if len(l.DynamicImports) != 1 {
				t.Fatalf("dynamic imports %+v, want one", l.DynamicImports)
			}
			d := l.DynamicImports[0]
			at := textAt(t, tc.src, tc.directive, 0)
			if d.At.Start != at.Start || strings.TrimSpace(tc.src[d.At.Start:d.At.End]) != tc.directive {
				t.Fatalf("the import is at %q, want the directive %q", tc.src[d.At.Start:d.At.End], tc.directive)
			}
			if d.Constant != tc.constant || len(d.Fragments) != len(tc.fragments) {
				t.Fatalf("import %+v, want constant %v fragments %q", d, tc.constant, tc.fragments)
			}
			for i := range tc.fragments {
				if d.Fragments[i] != tc.fragments[i] {
					t.Fatalf("fragments %q, want %q", d.Fragments, tc.fragments)
				}
			}
		})
	}
}

// TestCppOverrideMarks checks that exactly the members written with an
// `override` or `final` virt-specifier are marked Override: not a virtual
// member of the base, not an unmarked member that overrides one, not a
// non-virtual member.
//
// Mutation: mark every method -> B::m, D::q and plain are marked.
// Mutation: mark only `override` -> D::n is not marked.
func TestCppOverrideMarks(t *testing.T) {
	const src = `struct B {
	virtual int m(int v) { return v; }
	virtual int n() { return 0; }
	virtual int q() { return 1; }
	int plain() { return 2; }
};
struct D : B {
	int m(int v) override { return v + 1; }
	int n() final { return 3; }
	int q() { return 4; }
};
`
	l, _ := linkOf(t, "cpp", "x.cpp", src)
	for _, tc := range []struct {
		context, token string
		nth            int
		override       bool
	}{
		{"m(int v) {", "m", 0, false},
		{"n() {", "n", 0, false},
		{"q() { return 1", "q", 0, false},
		{"plain()", "plain", 0, false},
		{"m(int v) override", "m", 0, true},
		{"n() final", "n", 0, true},
		{"q() { return 4", "q", 0, false},
	} {
		e := exportNamed(l, cTokenAt(t, src, tc.context, tc.nth, tc.token))
		if e == nil {
			t.Fatalf("no export for %q", tc.context)
		}
		if e.Override != tc.override {
			t.Fatalf("%q is marked override %v, want %v", tc.context, e.Override, tc.override)
		}
	}
}

// TestCMacroAlias checks that an object-like macro whose replacement list is
// one identifier exports that identifier as its Alias, and that no other
// macro does: not a function-like macro, a literal or two identifiers.
//
// Mutation: alias a function-like macro -> MAX carries an Alias.
// Mutation: take the replacement's first identifier -> TWO is aliased.
func TestCMacroAlias(t *testing.T) {
	const src = `void log_impl(const char *m);
#define LOG log_impl /* the sink */
#define MAX(a, b) log_impl
#define LIMIT 10
#define TWO log_impl log_impl
`
	l, _ := linkOf(t, "c", "x.c", src)
	if e := exportNamed(l, cTokenAt(t, src, "LOG", 0, "LOG")); e == nil || e.Alias != "log_impl" {
		t.Fatalf("LOG exports %+v, want the alias log_impl", e)
	}
	if e := exportNamed(l, cTokenAt(t, src, "MAX", 0, "MAX")); e == nil || e.Alias != "" {
		t.Fatalf("the function-like MAX exports %+v, want an export with no alias", e)
	}
	for _, name := range []string{"LIMIT", "TWO"} {
		if e := exportNamed(l, cTokenAt(t, src, name, 0, name)); e != nil {
			t.Fatalf("%s exports %+v, want none", name, e)
		}
	}
}
