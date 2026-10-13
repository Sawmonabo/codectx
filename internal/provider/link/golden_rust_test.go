package link_test

import (
	"slices"
	"testing"

	"github.com/Sawmonabo/codectx/internal/model"
	"github.com/Sawmonabo/codectx/internal/provider/link"
)

// TestRustGoldens runs the Rust goldens through the real structural provider and the
// linker (runGoldens). Each case names the wrong answer it catches in fails.
func TestRustGoldens(t *testing.T) { runGoldens(t, rustGoldens) }

var rustGoldens = []golden{
	{
		name:   "module-paths",
		anchor: "The Rust Reference, \"Paths\" (path qualifiers `crate`, `self`, `super`), \"Modules\" (module source filenames), \"Use declarations\"",
		files: map[string]string{
			"Cargo.toml": `[package]
name = "paths"
version = "0.1.0"
edition = "2021"
`,
			"src/lib.rs": `pub mod a;
pub mod b;

use crate::a::util;

pub fn run() {
    util::f(); // @r1
    crate::b::util::f(); // @r2
    a::h(); // @rootcall
}
`,
			"src/a.rs": `pub mod util;

pub fn h() {} // @ah
`,
			"src/a/util.rs": `pub fn f() {} // @af

pub fn g() {
    super::h(); // @g1
    self::f(); // @g2
}
`,
			"src/b.rs": `pub mod util;
`,
			"src/b/util.rs": `pub fn f() {} // @bf
`,
		},
		// Derived twice from the source and the anchor, the second time blind (the sources, the anchors and
		// the binding rules alone); the two agree on every binding.
		want: []string{
			"r1 f -> af f exact",
			"r2 f -> bf f exact",
			"rootcall h -> ah h exact",
			"g1 h -> ah h exact",
			"g2 f -> af f exact",
		},
		fails: "a module path collapsed to its last component binds util::f to both util files; crate, self and super left unresolved lose their calls",
	},
	{
		name:   "smart-pointers-and-trait-objects",
		anchor: "The Rust Reference, \"Method-call expressions\" (receiver candidate types by auto-deref), \"Trait objects\"",
		files: map[string]string{
			"Cargo.toml": `[package]
name = "handlers"
version = "0.1.0"
edition = "2021"
`,
			"src/lib.rs": `use std::rc::Rc;

pub trait Handler {
    fn handle(&self); // @hspec
}

pub struct A;

impl Handler for A {
    fn handle(&self) {} // @ah
}

pub struct B;

impl Handler for B {
    fn handle(&self) {} // @bh
}

pub fn all(h: Box<dyn Handler>) {
    h.handle(); // @all
}

pub fn one(a: Rc<A>) {
    a.handle(); // @one
}
`,
		},
		// Derived twice from the source and the anchor, the second time blind (the sources, the anchors and
		// the binding rules alone); the two agree on every binding.
		want: []string{
			"all handle -> hspec handle virtual",
			"one handle -> ah handle exact",
		},
		fails: "a Box or Rc receiver read as the type `Box` or `Rc` leaves both calls unbound; a trait object's call published to each impl instead of the trait's method; a concrete receiver's call marked virtual",
	},
	{
		name:   "macros-and-call-forms",
		anchor: "The Rust Reference, \"Macro invocation\", \"Call expressions\", \"Method-call expressions\", \"Associated items\" (methods called as associated functions)",
		files: map[string]string{
			"Cargo.toml": `[package]
name = "forms"
version = "0.1.0"
edition = "2021"
`,
			"src/lib.rs": `macro_rules! bail {
    () => {};
}

fn bail() {} // @fbail

pub struct S;

impl S {
    pub fn new() -> S { S } // @new
    pub fn m(&self, a: i32) -> i32 { a } // @m
    pub fn start(&self) {} // @start
}

pub fn run(s: S) -> i32 {
    bail!(); // @mac
    bail(); // @call
    S::new().start(); // @chain
    let x = S::m(&s, 1); // @f1
    x + s.m(2) // @f2
}
`,
		},
		// Derived twice from the source and the anchor, the second time blind (the sources, the anchors and
		// the binding rules alone); the two agree on every binding.
		want: []string{
			"call bail -> fbail bail exact",
			"chain new -> new new exact",
			"chain start -> start start exact",
			"f1 m -> m m exact",
			"f2 m -> m m exact",
		},
		fails: "a macro invocation taken as a call of the same-named function binds `bail!()`; an argument count that excludes the receiver of `S::m(&s, 1)` drops it",
	},
	{
		name:   "inline-module-scope",
		anchor: "The Rust Reference, \"Paths\" (`super`), \"Names: Scopes\" (items of a module are in scope in that module only, not in nested or enclosing modules), \"Modules\"",
		files: map[string]string{
			"Cargo.toml": `[package]
name = "inline"
version = "0.1.0"
edition = "2021"
`,
			"src/lib.rs": `pub fn h() {} // @top

pub mod m {
    pub fn h() {} // @mh

    pub fn g() { // @mg
        h(); // @g1
    }

    pub mod inner {
        pub fn k() {
            super::h(); // @k1
        }
    }
}

pub fn run() {
    h(); // @r1
    m::g(); // @r2
}
`,
		},
		// Derived twice from the source and the anchor, the second time blind (the sources, the anchors and
		// the binding rules alone); the two agree on every binding.
		want: []string{
			"g1 h -> mh h exact",
			"k1 h -> mh h exact",
			"r1 h -> top h exact",
			"r2 g -> mg g exact",
		},
		fails: "a bare name looked up at the file's top level from inside `mod m`, or `super` read from the file, binds the crate root's h",
	},
	{
		name:   "glob-use-of-the-parent",
		anchor: "The Rust Reference, \"Use declarations\" (glob imports), \"Paths\" (`super`)",
		files: map[string]string{
			"Cargo.toml": `[package]
name = "globs"
version = "0.1.0"
edition = "2021"
`,
			"src/lib.rs": `pub mod a;
`,
			"src/a.rs": `pub fn h() {} // @ah

#[cfg(test)]
mod tests {
    use super::*;

    fn t() {
        h(); // @t1
    }
}
`,
		},
		// Derived twice from the source and the anchor, the second time blind (the sources, the anchors and
		// the binding rules alone); the two agree on every binding. The tests module exists only under
		// `#[cfg(test)]`; both read the call as the test build defines it.
		want: []string{
			"t1 h -> ah h exact",
		},
		fails: "a glob `use super::*` not followed leaves `h()` in the tests module unbound",
	},
	{
		name:   "sibling-crate-paths",
		anchor: "The Rust Reference, \"Paths\" (a path's first segment naming an extern crate), \"Crates and source files\" (crate names: `-` becomes `_`)",
		files: map[string]string{
			"Cargo.toml": `[workspace]
members = ["app", "qlib"]
`,
			"qlib/Cargo.toml": `[package]
name = "q-lib"
version = "0.1.0"
edition = "2021"
`,
			"qlib/src/lib.rs": `pub mod x;
`,
			"qlib/src/x.rs": `pub fn f() {} // @qf
`,
			"app/Cargo.toml": `[package]
name = "app"
version = "0.1.0"
edition = "2021"

[dependencies]
q-lib = { path = "../qlib" }
`,
			"app/src/lib.rs": `pub mod x;

pub fn run() {
    q_lib::x::f(); // @r1
    crate::x::f(); // @r2
}
`,
			"app/src/x.rs": `pub fn f() {} // @af
`,
		},
		// Derived twice from the source and the anchor, the second time blind (the sources, the anchors and
		// the binding rules alone); the two agree on every binding.
		want: []string{
			"r1 f -> qf f exact",
			"r2 f -> af f exact",
		},
		fails: "a first path segment that names another crate of the workspace, read as a module of this crate, binds app's f or nothing",
	},
	{
		name:   "pointer-results-and-method-values",
		anchor: "The Rust Reference, \"Method-call expressions\" (the first receiver type with a matching method wins: `Rc<Foo>` has its own `clone`), \"Call expressions\", \"Function pointer types\", \"Paths\" (a method path as a function value)",
		files: map[string]string{
			"Cargo.toml": `[package]
name = "pointers"
version = "0.1.0"
edition = "2021"
`,
			"src/lib.rs": `use std::rc::Rc;

pub struct Foo;

impl Clone for Foo {
    fn clone(&self) -> Foo { Foo } // @fclone
}

impl Foo {
    pub fn m(&self, x: i32) -> i32 { x } // @fm
    pub fn n(&self, x: i32) -> i32 { x + 1 } // @fn2
}

fn make() -> Rc<Foo> { Rc::new(Foo) } // @mk

fn other(_f: &Foo, x: i32) -> i32 { x } // @other

pub fn run(foo: Foo) -> i32 {
    let _a = make().clone(); // @c1
    let _b = foo.clone(); // @c2
    let g: fn(&Foo, i32) -> i32 = Foo::m;
    let h: fn(&Foo, i32) -> i32 = other;
    g(&foo, 1) // @c3
        + h(&foo, 2) // @c4
}
`,
		},
		// Derived twice from the source and the anchor, the second time blind (the sources, the anchors and
		// the binding rules alone); the two agree on every binding.
		want: []string{
			"c1 make -> mk make exact",
			"c2 clone -> fclone clone exact",
			"c3 g -> fm m exact set=2",
			"c3 g -> other other exact set=2",
			"c4 h -> fm m exact set=2",
			"c4 h -> other other exact set=2",
		},
		fails: "a pointer result unwrapped before its own methods binds `make().clone()` to Foo::clone; a method path spelled without its self parameter misses the fn(&Foo, i32) sites",
	},
}

// TestRustMacroInputIsGeneratedAcrossFiles protects the crate-wide match of a
// macro its invoking file does not define (the Rust Reference, "Macros By
// Example", scoping, exporting and importing: a `#[macro_use]` module's
// macros are in textual scope in the modules declared after it, and an
// exported one is named `crate::m`). Each item a body of such a macro writes
// gets one `generates` from every macro_rules of that name in the crate's
// other files, with their count as its set size when there are several, the detail saying whether
// that definition transcribes its item input unchanged; a path through
// another crate and a name no file defines publish nothing.
//
// Mutation: match no other file -> no generates.
// Mutation: mark every definition as transcribing -> wrapped's edge reads
// macro_transcribes.
// Mutation: keep only the first of several definitions -> doubled has one
// edge and no set.
// Mutation: read a path's last segment -> foreign gains cfgrt's edge.
// Mutation: drop the crate:: root -> rooted has no edge.
func TestRustMacroInputIsGeneratedAcrossFiles(t *testing.T) {
	files := map[string]string{
		"Cargo.toml": `[package]
name = "macros"
version = "0.1.0"
edition = "2021"
`,
		"src/lib.rs": `#[macro_use]
mod macros;
#[macro_use]
mod more;
mod rt;
`,
		"src/macros.rs": `macro_rules! cfg_rt { // @cfgrt
    ($($item:item)*) => { $( #[cfg(feature = "rt")] $item )* }
}
macro_rules! wrap { // @wrap
    ($($t:tt)*) => { mod inner { $($t)* } }
}
macro_rules! twice { // @twice1
    ($($item:item)*) => { $( $item )* }
}
`,
		"src/more.rs": `macro_rules! twice { // @twice2
    ($i:item) => { $i $i }
}
`,
		"src/rt.rs": `cfg_rt! {
    pub fn spawn() {} // @spawn
}
crate::cfg_rt! {
    pub fn rooted() {} // @rooted
}
wrap! {
    pub fn wrapped() {} // @wrapped
}
twice! {
    pub fn doubled() {} // @doubled
}
other::cfg_rt! {
    pub fn foreign() {} // @foreign
}
nowhere! {
    pub fn lost() {} // @lost
}
`,
	}
	l := linkSources(t, newStructural(t), files)
	paths := l.paths(t)
	var got []string
	for _, r := range l.out.rels {
		if r.Relation.Kind != model.RelGenerates {
			continue
		}
		for _, e := range r.Evidence {
			g := l.target(paths, l.out.nodes[r.Relation.From]) + " generates " + l.target(paths, l.out.nodes[r.Relation.To]) +
				" " + e.Detail + setSuffix(e)
			if !slices.Contains(got, g) {
				got = append(got, g)
			}
		}
	}
	slices.Sort(got)
	want := []string{
		"cfgrt cfg_rt generates rooted rooted " + link.DetailMacroTranscribes,
		"cfgrt cfg_rt generates spawn spawn " + link.DetailMacroTranscribes,
		"twice1 twice generates doubled doubled " + link.DetailMacroTranscribes + " set=2",
		"twice2 twice generates doubled doubled " + link.DetailMacroInput + " set=2",
		"wrap wrap generates wrapped wrapped " + link.DetailMacroInput,
	}
	if !slices.Equal(got, want) {
		t.Fatalf("generates:\n got %q\nwant %q", got, want)
	}
}
