package link_test

import "testing"

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
			"r1 f -> af f",
			"r2 f -> bf f",
			"rootcall h -> ah h",
			"g1 h -> ah h",
			"g2 f -> af f",
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
    fn handle(&self);
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
			"all handle -> ah handle",
			"all handle -> bh handle",
			"one handle -> ah handle",
		},
		fails: "a Box or Rc receiver read as the type `Box` or `Rc` leaves both calls unbound",
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
			"call bail -> fbail bail",
			"chain new -> new new",
			"chain start -> start start",
			"f1 m -> m m",
			"f2 m -> m m",
		},
		fails: "a macro invocation taken as a call of the same-named function binds `bail!()`; an argument count that excludes the receiver of `S::m(&s, 1)` drops it",
	},
}
