package worker

import (
	"testing"

	"github.com/Sawmonabo/codectx/internal/provider/treesitter/wire"
)

// rustFactsSource is a trait, its implementation, an inherent impl and calls
// whose arguments read parameters, a local and a field's owner, a call's
// result and literals.
const rustFactsSource = `pub trait Shape {
    fn area(&self) -> f64;
}

pub struct Sq {
    side: f64,
}

impl Shape for Sq {
    fn area(&self) -> f64 {
        self.side * self.side
    }
}

impl Sq {
    pub fn scale(&self, by: f64) -> f64 {
        by * self.side
    }
}

pub fn combine(a: f64, b: f64, c: f64) -> f64 {
    a + b + c
}

pub fn run(sq: &Sq, k: f64) -> f64 {
    let n = 2.0;
    let total = combine(k + n, sq.scale(k), sq.side);
    total + combine(1.0, 2.0, 3.0)
}
`

// TestRustArgumentsReadTheirOwnVariables protects the sources of argument
// flow: each argument reads the variables its own expression names, by the
// identifier that declares them, a call's result reads nothing, and a
// literal argument reads nothing.
//
// Mutation: Reads from the whole statement instead of the argument -> the
// first call's arguments each read k, n and sq.
// Mutation: descend into a call argument -> `sq.scale(k)` reads sq and k.
func TestRustArgumentsReadTheirOwnVariables(t *testing.T) {
	src := rustFactsSource
	l, fns := linkOf(t, "rust", "lib.rs", src)
	// decl is the identifier that starts the first occurrence of context.
	decl := func(context, ident string) wire.Span {
		s := textAt(t, src, context, 0)
		return wire.Span{Start: s.Start, End: s.Start + uint32(len(ident))}
	}
	k, sq, n := decl("k: f64", "k"), decl("sq: &Sq", "sq"), decl("n = 2.0", "n")
	wantArguments(t, src, fns, siteNamed(l, textAt(t, src, "combine", 1)), []argWant{
		{at: "k + n", reads: []wire.Span{k, n}},
		{at: "sq.scale(k)"},
		{at: "sq.side", reads: []wire.Span{sq}},
	})
	wantArguments(t, src, fns, siteNamed(l, textAt(t, src, "scale", 1)), []argWant{{at: "k", reads: []wire.Span{k}}})
	wantArguments(t, src, fns, siteNamed(l, textAt(t, src, "combine", 2)), []argWant{{at: "1.0"}, {at: "2.0"}, {at: "3.0"}})
}

// TestRustTraitImplMethodsAreMarkedOverride protects the override mark the
// hierarchy reads for a method of an `impl Trait for Type` block: it is set
// there and only there, never on an inherent impl's method or the trait's
// own declaration.
//
// Mutation: mark every method -> the inherent `scale` and the trait's `area`
// are marked.
// Mutation: mark nothing -> the trait impl's `area` is unmarked.
func TestRustTraitImplMethodsAreMarkedOverride(t *testing.T) {
	src := rustFactsSource
	l, _ := linkOf(t, "rust", "lib.rs", src)
	for _, c := range []struct {
		needle   string
		nth      int
		override bool
	}{{"area", 0, false}, {"area", 1, true}, {"scale", 0, false}, {"combine", 0, false}} {
		x := exportNamed(l, textAt(t, src, c.needle, c.nth))
		if x == nil {
			t.Fatalf("no export for %s #%d", c.needle, c.nth)
		}
		if x.Override != c.override {
			t.Fatalf("%s #%d: override %v, want %v", c.needle, c.nth, x.Override, c.override)
		}
	}
}
