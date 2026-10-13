package worker

import (
	"strings"
	"testing"

	"github.com/Sawmonabo/codectx/internal/provider/treesitter/wire"
)

// declNamed is the nth declaration named name, failing the test when there
// is none.
func declNamed(t *testing.T, decls []wire.Decl, name string, nth int) wire.Decl {
	t.Helper()
	for _, d := range decls {
		if d.Name == name {
			if nth == 0 {
				return d
			}
			nth--
		}
	}
	t.Fatalf("no declaration %s #%d among %+v", name, nth, decls)
	return wire.Decl{}
}

// TestRustItemPositionMacroBodiesAreDeclarations protects the items an
// item-position macro's brace body writes: each is a declaration at the
// file's own byte offsets, inside the branch element of its macro, marked
// as the macro's input, so two `block_on`s written in `cfg_rt!` and
// `cfg_not_rt!` are two entities, and a call inside a recovered body is a
// reference.
//
// Mutation: skip macro bodies -> no block_on is declared.
// Mutation: re-parse a copy of the body -> the offsets are the copy's.
// Mutation: no macro element -> the two block_on share one branch.
// Mutation: no MacroInput -> the mark is empty.
// Mutation: run the query only over the file's own tree -> helper's call is
// missing.
func TestRustItemPositionMacroBodiesAreDeclarations(t *testing.T) {
	src := "cfg_rt! {\n    pub(crate) fn block_on() { helper(1) }\n}\ncfg_not_rt! {\n    pub(crate) fn block_on() {}\n}\n"
	decls, refs := rustFacts(t, src)
	wantBranches(t, decls, "block_on", `macro "cfg_rt"`, `macro "cfg_not_rt"`)
	for i, macro := range []string{"cfg_rt", "cfg_not_rt"} {
		d := declNamed(t, decls, "block_on", i)
		at := textAt(t, src, "pub(crate) fn block_on", i)
		name := textAt(t, src, "block_on", i)
		if d.Start != at.Start || d.NameStart != name.Start || d.NameEnd != name.End {
			t.Fatalf("block_on #%d at %d (name %d-%d), want %d (name %d-%d)", i, d.Start, d.NameStart, d.NameEnd,
				at.Start, name.Start, name.End)
		}
		if d.MacroInput != macro {
			t.Fatalf("block_on #%d is the input of %q, want %q", i, d.MacroInput, macro)
		}
	}
	call := textAt(t, src, "helper", 0)
	found := false
	for _, r := range refs {
		found = found || (r.Kind == "call" && r.Name == "helper" && r.NameStart == call.Start)
	}
	if !found {
		t.Fatalf("the call of helper inside the macro body is no reference: %+v", refs)
	}
}

// TestRustMacroBraceGroupsAreArms protects the one-level fallback for a body
// that is no item list (`cfg_if!`): each brace group whose header holds no
// item keyword is re-parsed on its own, its items in the element `macro
// "<path>" "<header>"`, so the three `imp`s are three entities, and a group
// after an item's head (`struct F: u32 { const A = 1; }`) is never one, so
// its constant does not become an item of the module.
//
// Mutation: no fallback -> no imp is declared.
// Mutation: drop the header from the element -> the two fn imp share one
// branch.
// Mutation: try every group whatever its header -> A is declared.
func TestRustMacroBraceGroupsAreArms(t *testing.T) {
	src := `cfg_if::cfg_if! {
    if #[cfg(unix)] {
        mod imp { pub fn f() {} }
    } else if #[cfg(windows)] {
        fn imp() {}
    } else {
        fn imp() {}
    }
}
bitflags! {
    struct F: u32 {
        const A = 1;
    }
}
`
	decls := rustDecls(t, src)
	wantBranches(t, decls, "imp",
		`macro "cfg_if::cfg_if" "if #[cfg(unix)]"`,
		`macro "cfg_if::cfg_if" "else if #[cfg(windows)]"`,
		`macro "cfg_if::cfg_if" "else"`)
	if f := declNamed(t, decls, "f", 0); f.Qualified != "imp::f" || f.MacroInput != "cfg_if::cfg_if" {
		t.Fatalf("f inside the recovered module is %q, the input of %q", f.Qualified, f.MacroInput)
	}
	for _, d := range decls {
		if d.Name == "A" || d.Name == "F" {
			t.Fatalf("%s of a bitflags body was recovered as an item: %+v", d.Name, d)
		}
	}
}

// TestRustMacroBodiesOnlyInItemPosition protects the scope of the re-parse
// and its nesting: a macro in a function body (an expression or statement)
// is not re-parsed, a macro in an impl's body is, its function a method of
// the impl, an item-position macro inside a recovered body is re-parsed in
// turn with both elements, and a cfg before an invocation is an element
// before its macro's.
//
// Mutation: re-parse every braced invocation -> z is declared.
// Mutation: no nested re-parse -> deep is not declared.
// Mutation: drop the attributes before an invocation -> stat's branch lacks
// its cfg.
func TestRustMacroBodiesOnlyInItemPosition(t *testing.T) {
	src := `fn outer() {
    inner! { fn z() {} }
}
pub struct S;
impl S {
    m! { pub fn a(&self) -> u8 { 1 } }
}
outer! { inner! { fn deep() {} } }
#[cfg(unix)]
s! {
    pub struct stat { pub st_dev: u64 }
}
`
	decls := rustDecls(t, src)
	for _, d := range decls {
		if d.Name == "z" {
			t.Fatalf("an invocation in a function body was re-parsed: %+v", d)
		}
	}
	if a := declNamed(t, decls, "a", 0); a.Kind != "method" || a.Qualified != "S::a" || a.Branch != `macro "m"` {
		t.Fatalf("a in the impl's macro is %s %q in %q, want method S::a in macro \"m\"", a.Kind, a.Qualified, a.Branch)
	}
	if d := declNamed(t, decls, "deep", 0); d.Branch != `macro "outer" / macro "inner"` || d.MacroInput != "inner" {
		t.Fatalf("deep is in %q, the input of %q", d.Branch, d.MacroInput)
	}
	if d := declNamed(t, decls, "stat", 0); d.Branch != `cfg "(unix)" / macro "s"` || !strings.HasPrefix(src[d.Start:], "pub struct stat") {
		t.Fatalf("stat is in %q at %d", d.Branch, d.Start)
	}
}

// TestRustMacroBodiesAreLinkedAndLowered protects the dependence and link
// facts of the items a macro body writes: the re-parse replaces the body's
// tokens in the file's flat array, so a function in it is lowered (one
// function message), its call is a site, and a method written in a macro
// inside an impl is the impl's: its receiver is the self type and, in a
// trait impl, its trait is the block's.
//
// Mutation: no splice -> no export or function message for spawn, and no
// site for take.
// Mutation: read a member's owner without passing the macro -> fmt has no
// receiver and no trait.
func TestRustMacroBodiesAreLinkedAndLowered(t *testing.T) {
	src := `pub struct S { v: u8 }
impl std::fmt::Display for S {
    m! { fn fmt(&self, f: &mut std::fmt::Formatter) -> std::fmt::Result { helper(self.v) } }
}
cfg_rt! {
    pub fn spawn(s: &S) -> u8 { let x = s.v; take(x) }
}
fn take(x: u8) -> u8 { x }
fn helper(x: u8) -> std::fmt::Result { Ok(()) }
`
	l, fns := linkOf(t, "rust", "lib.rs", src)
	name := func(n string) wire.Span {
		s := textAt(t, src, "fn "+n, 0)
		return wire.Span{Start: s.Start + 3, End: s.End}
	}
	fmtX := exportNamed(l, name("fmt"))
	if fmtX == nil || fmtX.Receiver != "S" || !fmtX.Override || fmtX.Trait != "std::fmt::Display" {
		t.Fatalf("fmt in the impl's macro exports %+v, want receiver S implementing std::fmt::Display", fmtX)
	}
	if x := exportNamed(l, name("spawn")); x == nil || x.Receiver != "" || x.Signature != "(&S)(u8)" {
		t.Fatalf("spawn in cfg_rt! exports %+v", x)
	}
	if siteNamed(l, textAt(t, src, "take", 0)) == nil {
		t.Fatal("the call of take inside cfg_rt! is no site")
	}
	spawn := textAt(t, src, "pub fn spawn", 0)
	lowered := false
	for _, f := range fns {
		lowered = lowered || (f.Span.Start == spawn.Start && !f.Failed)
	}
	if !lowered {
		t.Fatalf("spawn was not lowered: %+v", fns)
	}
}

// TestRustMacroInputIsExactOnlyForAPassThroughMacro protects the shape check
// that proves a macro's input items are its expansion's: a macro_rules of
// the file, in textual scope, every rule of which transcribes its item or
// token-tree input once, at the depth it was bound, inside no group.
//
// Mutation: mark every macro input exact -> b, c, e and f are exact.
// Mutation: accept a use inside a group -> b (wrapped in a module) is exact.
// Mutation: accept any number of uses -> c (transcribed twice) is exact.
// Mutation: ignore the definition's module -> f (defined inside m) is exact.
// Mutation: accept item fragments only -> d (a tt pass-through) is not.
func TestRustMacroInputIsExactOnlyForAPassThroughMacro(t *testing.T) {
	src := `macro_rules! cfg_rt { ($($item:item)*) => { $( #[cfg(feature = "rt")] $item )* } }
macro_rules! wrap { ($($t:tt)*) => { mod inner { $($t)* } } }
macro_rules! twice { ($i:item) => { $i $i } }
macro_rules! pass { ($($t:tt)*) => { $($t)* } }
cfg_rt! { fn a() {} }
wrap! { fn b() {} }
twice! { fn c() {} }
pass! { fn d() {} }
other! { fn e() {} }
mod m { macro_rules! hidden { ($($t:tt)*) => { $($t)* } } }
hidden! { fn f() {} }
`
	decls := rustDecls(t, src)
	for name, exact := range map[string]bool{"a": true, "b": false, "c": false, "d": true, "e": false, "f": false} {
		if d := declNamed(t, decls, name, 0); d.MacroInput == "" || d.MacroExact != exact {
			t.Errorf("%s: macro %q exact %v, want exact %v", name, d.MacroInput, d.MacroExact, exact)
		}
	}
}
