package worker

import (
	"slices"
	"testing"

	"github.com/Sawmonabo/codectx/internal/provider/treesitter/wire"
)

// goArgsSource calls g with positional, spread, literal, computed and
// call-result arguments, k with a map literal keyed by a variable, from a function body and from a closure that reads
// the enclosing function's local.
const goArgsSource = `package p

func g(a int, rest ...int) int { return a }

func f(n int, xs []int) int {
	m := n + 1
	g(m, xs...)
	g(1)
	g(n*2, m, len(xs))
	h := func() int { return g(m) }
	p := pt{x: m}
	g(p.x, pt{x: n}.x)
	k(map[int]int{m: 1})
	return h()
}

func k(v map[int]int) {}

type pt struct{ x int }

type T struct{}

func (r *T) M(a, b int, c ...string) {}
`

// TestGoArgumentsReadTheirOwnVariables protects the sources of argument
// flow: each argument of a Go call names where it is, whether it spreads
// (`xs...`, the marker left out of its range), and exactly the variables its
// own expression reads, declared where the function messages declare them;
// a literal and a call's result read nothing, and a closure's argument reads
// the enclosing function's local.
//
// Mutation: Reads from the whole statement instead of the argument -> `n*2`
// also reads m and xs. Mutation: keep the `...` in the argument's range or
// drop the spread -> the second argument of `g(m, xs...)` differs. Mutation:
// descend into a call's arguments -> `len(xs)` reads xs. Mutation: skip a
// keyed element's bare-name key (taking it for a struct field) -> the map
// literal `map[int]int{m: 1}` reads nothing, although its key is the
// variable m, which the lowering counts as a use.
func TestGoArgumentsReadTheirOwnVariables(t *testing.T) {
	src := goArgsSource
	l, fns := linkOf(t, "go", "p.go", src)
	tok := func(needle string, nth int) wire.Span {
		s := textAt(t, src, needle, nth)
		s.End = s.Start + 1
		return s
	}
	m, n, xs, p := tok("m := n", 0), tok("n int, xs", 0), textAt(t, src, "xs", 0), tok("p := pt", 0)
	cases := []struct {
		call string
		want []argWant
	}{
		{"g(m, xs...)", []argWant{{at: "m", reads: []wire.Span{m}}, {at: "xs", spread: wire.SpreadPositional, reads: []wire.Span{xs}}}},
		{"g(1)", []argWant{{at: "1"}}},
		{"g(n*2, m, len(xs))", []argWant{{at: "n*2", reads: []wire.Span{n}}, {at: "m", reads: []wire.Span{m}}, {at: "len(xs)"}}},
		{"g(m) }", []argWant{{at: "m", reads: []wire.Span{m}}}},
		{"g(p.x, pt{x: n}.x)", []argWant{{at: "p.x", reads: []wire.Span{p}}, {at: "pt{x: n}.x", reads: []wire.Span{n}}}},
		{"k(map[int]int{m: 1})", []argWant{{at: "map[int]int{m: 1}", reads: []wire.Span{m}}}},
	}
	for _, c := range cases {
		t.Run(c.call, func(t *testing.T) {
			wantArguments(t, src, fns, siteNamed(l, tok(c.call, 0)), c.want)
		})
	}
}

// TestGoParametersCarryTheirPositions protects the parameter marks the
// argument-to-parameter match reads: a method's receiver is position 0, the
// declared parameters count from 1 with one position per name of a grouped
// declaration, and `c ...T` collects the rest.
//
// Mutation: number the receiver 1 or a grouped declaration once -> M's
// indices shift. Mutation: drop the variadic mark -> rest and c are fixed
// parameters.
func TestGoParametersCarryTheirPositions(t *testing.T) {
	src := goArgsSource
	l, _ := linkOf(t, "go", "p.go", src)
	type param struct {
		name     string
		index    uint32
		variadic bool
	}
	of := func(callable wire.Span) []param {
		var out []param
		for _, x := range l.Parameters {
			if x.Callable == callable {
				out = append(out, param{src[x.Name.Start:x.Name.End], x.Index, x.Variadic})
				if x.KeywordOnly {
					t.Fatalf("Go parameter %s is keyword-only", src[x.Name.Start:x.Name.End])
				}
			}
		}
		return out
	}
	g := textAt(t, src, "g(a int", 0)
	g.End = g.Start + 1
	if got, want := of(g), []param{{"a", 1, false}, {"rest", 2, true}}; !slices.Equal(got, want) {
		t.Fatalf("g's parameters %+v, want %+v", got, want)
	}
	mm := textAt(t, src, "M(a", 0)
	mm.End = mm.Start + 1
	if got, want := of(mm), []param{{"r", 0, false}, {"a", 1, false}, {"b", 2, false}, {"c", 3, true}}; !slices.Equal(got, want) {
		t.Fatalf("M's parameters %+v, want %+v", got, want)
	}
}

// TestGoImportBindsItsConventionalName protects the name a Go import binds
// when its spec names none: the path's last element, past a major-version
// element (`.../chi/v5` binds chi) or a gopkg.in .vN suffix
// (`gopkg.in/yaml.v3` binds yaml), as the extraction names it. A call through
// that name is a call through the package, a static site qualified by it,
// which the linker resolves by the import's path.
//
// Mutation: name the import by the last path element -> `chi.NewRouter()`
// and `yaml.Marshal()` are member calls on an unknown receiver, and `v5` and
// `yaml.v3` are names no call writes.
func TestGoImportBindsItsConventionalName(t *testing.T) {
	src := `package p

import (
	"github.com/go-chi/chi/v5"
	"gopkg.in/yaml.v3"
)

func f() {
	chi.NewRouter()
	yaml.Marshal(nil)
}
`
	l, _ := linkOf(t, "go", "p.go", src)
	for _, c := range []struct{ callee, qualifier string }{{"NewRouter", "chi"}, {"Marshal", "yaml"}} {
		s := siteNamed(l, textAt(t, src, c.callee, 0))
		if s == nil {
			t.Fatalf("no site for %s", c.callee)
		}
		if s.Shape != wire.ShapeStatic || s.Qualifier != c.qualifier {
			t.Errorf("%s.%s() is shape %d qualifier %q, want a static site qualified %q", c.qualifier, c.callee, s.Shape, s.Qualifier, c.qualifier)
		}
	}
}

// TestGoNewOfAnExpression protects new's two operands ("Allocation", Go
// 1.26): new(T) allocates a T, and new(v) of an expression copies v's
// value, so new(v) has v's type and new(fn) passes the function fn as a
// value, although the grammar reads each bare operand as a type name.
//
// Mutation: skip every operand of new as a type -> new(fn) sends no value.
// Mutation: name new(v)'s type by its operand's text -> the first argument
// of g has the type "v". Mutation: read every bare operand as an expression
// -> new(T) has no type.
func TestGoNewOfAnExpression(t *testing.T) {
	src := `package p

type T struct{}

func fn() {}

func g(a any) {}

func f() {
	v := T{}
	g(new(v))
	g(new(T))
	_ = new(fn)
}
`
	l, _ := linkOf(t, "go", "p.go", src)
	call := func(needle string) *wire.Site {
		s := textAt(t, src, needle, 0)
		s.End = s.Start + 1
		return siteNamed(l, s)
	}
	for needle, want := range map[string]string{"g(new(v))": "T", "g(new(T))": "T"} {
		s := call(needle)
		if s == nil || len(s.Args) != 1 || s.Args[0].Kind != wire.TypeNamed || s.Args[0].Name != want {
			t.Fatalf("%s: argument types %+v, want the named type %s", needle, s, want)
		}
	}
	var values []string
	for _, v := range l.Values {
		values = append(values, src[v.At.Start:v.At.End]+" "+v.Type.Name)
	}
	if !slices.Equal(values, []string{"fn fn"}) {
		t.Fatalf("values %q, want new(fn)'s operand fn alone", values)
	}
}
