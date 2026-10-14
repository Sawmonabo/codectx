package delta_test

import "testing"

// TestStaticRefreshWorkFollowsReads refreshes a Go, Java, Rust, C and C++
// project after an edit to one declaration another file's sites read, from
// the base tree, compares each refresh with a full build of the same tree,
// fact for fact (session.check), and compares the sites the refresh
// resolved with those the full build resolved (link.Observer): of a member
// whose table held, a site re-runs only when a key it read changed.
//
//   - a changed parameter list (help gains a parameter) resolves the caller's
//     sites that call help, and not the others beside them in the same
//     function, and no site of the file that reads nothing of help;
//   - a changed result type (make returns a Cat, not a Dog), where the
//     language has methods, resolves the caller's site of make and the site
//     whose receiver is make's result, which now binds Cat's speak, and no
//     site of the file that reads nothing of make.
//
// Mutation: re-run every site of a member any of whose nodes re-runs (the
// member granularity) -> the caller resolves all its sites after each edit.
// Mutation: re-run only the sites whose own reads changed, not those whose
// receiver is a re-run site's result (drop selection.closedUnder) -> the
// receiver site keeps Dog's speak, which the full build does not bind.
// Mutation: record a read that fills the member's shared include closure
// for the site that asked first (drop hierarchy.memberWide in
// cClosure.reaches) -> after a.h includes c.h, g's site keeps b.h's g;
// the same in hierarchy.closedImports -> after the util package is removed,
// b.Run's site keeps Beta.Run.
func TestStaticRefreshWorkFollowsReads(t *testing.T) {
	type work struct {
		edit       edit
		some, none []string
	}
	cases := []struct {
		name  string
		base  map[string]string
		edits []work
	}{{
		name: "go",
		base: map[string]string{
			"go.mod": "module example.com/app\n\ngo 1.22\n",
			"lib.go": "package app\n\nfunc Help(x int) int { return x }\n\ntype Dog struct{}\n\nfunc (d *Dog) Speak() int { return 1 }\n\n" +
				"type Cat struct{}\n\nfunc (c *Cat) Speak() int { return 2 }\n\nfunc Make() *Dog { return &Dog{} }\n",
			"use.go":   "package app\n\nfunc Use() int {\n\ta := Help(1)\n\tb := Other() + Other()\n\treturn a + b + Make().Speak()\n}\n",
			"other.go": "package app\n\nfunc Other() int { return 3 }\n\nfunc Twice() int { return Other() + Other() }\n",
		},
		edits: []work{
			{edit: edit{name: "changed parameter list", replace: []sub{{"lib.go", "func Help(x int) int", "func Help(x, y int) int"}}},
				some: []string{"use.go"}, none: []string{"other.go"}},
			{edit: edit{name: "changed result type", replace: []sub{{"lib.go", "func Make() *Dog { return &Dog{} }", "func Make() *Cat { return &Cat{} }"}}},
				some: []string{"use.go"}, none: []string{"other.go"}},
		},
	}, {
		// Alpha and Beta are declared in no scope use.go reads and named by
		// none of its imports, so each resolves through every type of the
		// unit only while every import of use.go names a package of the unit
		// (closedImports). a.Run's site asks first and b.Run's finds the
		// answer: both read it, so removing the util package re-runs both,
		// and neither binds.
		name: "go closed imports",
		base: map[string]string{
			"go.mod":     "module example.com/app\n\ngo 1.22\n",
			"use.go":     "package app\n\nimport \"example.com/app/util\"\n\nfunc Use(a Alpha, b Beta) int { return a.Run() + b.Run() + util.X() }\n",
			"util/u.go":  "package util\n\nfunc X() int { return 1 }\n",
			"other/o.go": "package other\n\ntype Alpha struct{}\n\nfunc (Alpha) Run() int { return 1 }\n\ntype Beta struct{}\n\nfunc (Beta) Run() int { return 2 }\n",
		},
		edits: []work{{edit: edit{name: "imported package removed", remove: []string{"util/u.go"}}}},
	}, {
		name: "java",
		base: map[string]string{
			"src/app/Lib.java": "package app;\n\npublic class Lib {\n  public static int help(int x) {\n    return x;\n  }\n\n" +
				"  public static Dog make() {\n    return new Dog();\n  }\n}\n",
			"src/app/Dog.java": "package app;\n\npublic class Dog {\n  public int speak() {\n    return 1;\n  }\n}\n",
			"src/app/Cat.java": "package app;\n\npublic class Cat {\n  public int speak() {\n    return 2;\n  }\n}\n",
			"src/app/Use.java": "package app;\n\npublic class Use {\n  static int use() {\n    int a = Lib.help(1);\n" +
				"    int b = Other.other() + Other.other();\n    return a + b + Lib.make().speak();\n  }\n}\n",
			"src/app/Other.java": "package app;\n\npublic class Other {\n  static int other() {\n    return 3;\n  }\n\n" +
				"  static int twice() {\n    return other() + other();\n  }\n}\n",
		},
		edits: []work{
			{edit: edit{name: "changed parameter list", replace: []sub{{"src/app/Lib.java", "help(int x)", "help(int x, int y)"}}},
				some: []string{"src/app/Use.java"}, none: []string{"src/app/Other.java"}},
			{edit: edit{name: "changed result type", replace: []sub{{"src/app/Lib.java",
				"  public static Dog make() {\n    return new Dog();", "  public static Cat make() {\n    return new Cat();"}}},
				some: []string{"src/app/Use.java"}, none: []string{"src/app/Other.java"}},
		},
	}, {
		name: "rust",
		base: map[string]string{
			"Cargo.toml": "[package]\nname = \"app\"\nversion = \"0.1.0\"\nedition = \"2021\"\n",
			"src/lib.rs": "pub mod other;\npub mod usage;\npub mod util;\n",
			"src/util.rs": "pub fn help(x: i32) -> i32 {\n    x\n}\n\npub struct Dog;\n\nimpl Dog {\n    pub fn speak(&self) -> i32 {\n        1\n    }\n}\n\n" +
				"pub struct Cat;\n\nimpl Cat {\n    pub fn speak(&self) -> i32 {\n        2\n    }\n}\n\npub fn make() -> Dog {\n    Dog\n}\n",
			"src/other.rs": "pub fn other() -> i32 {\n    3\n}\n\npub fn twice() -> i32 {\n    other() + other()\n}\n",
			"src/usage.rs": "use crate::other::other;\nuse crate::util::{help, make};\n\npub fn usage() -> i32 {\n" +
				"    help(1) + other() + other() + make().speak()\n}\n",
		},
		edits: []work{
			{edit: edit{name: "changed parameter list", replace: []sub{{"src/util.rs", "pub fn help(x: i32)", "pub fn help(x: i32, y: i32)"}}},
				some: []string{"src/usage.rs"}, none: []string{"src/other.rs"}},
			{edit: edit{name: "changed result type", replace: []sub{{"src/util.rs", "pub fn make() -> Dog {\n    Dog\n}", "pub fn make() -> Cat {\n    Cat\n}"}}},
				some: []string{"src/usage.rs"}, none: []string{"src/other.rs"}},
		},
	}, {
		name: "c",
		base: map[string]string{
			"util.h":  "int help(int x);\nint other(void);\n",
			"util.c":  "#include \"util.h\"\n\nint help(int x) { return x; }\n",
			"other.c": "#include \"util.h\"\n\nint other(void) { return 3; }\n\nint twice(void) { return other() + other(); }\n",
			"use.c":   "#include \"util.h\"\n\nint use(void) { return help(1) + other() + other(); }\n",
		},
		edits: []work{
			{edit: edit{name: "renamed definition", replace: []sub{{"util.c", "int help(int x) {", "int assist(int x) {"}}},
				some: []string{"use.c"}, none: []string{"other.c"}},
		},
	}, {
		// f's site reads the include closure through a.h to b.h first, and
		// g's site then finds b.h already reached: both read the closure,
		// so a.h now including c.h instead re-runs both, and g binds c.h's g.
		name: "c include closure",
		base: map[string]string{
			"a.h":   "#include \"b.h\"\n",
			"b.h":   "static int f(void) { return 1; }\nstatic int g(void) { return 2; }\n",
			"c.h":   "static int f(void) { return 3; }\nstatic int g(void) { return 4; }\n",
			"use.c": "#include \"a.h\"\n\nint use(void) { return f() + g(); }\n",
		},
		edits: []work{{edit: edit{name: "included header moved", replace: []sub{{"a.h", "#include \"b.h\"", "#include \"c.h\""}}}}},
	}, {
		name: "cpp",
		base: map[string]string{
			"lib.hpp": "struct Dog {\n  int speak() { return 1; }\n};\n\nstruct Cat {\n  int speak() { return 2; }\n};\n\n" +
				"int help(int x);\nDog make();\nint other();\n",
			"lib.cpp":   "#include \"lib.hpp\"\n\nint help(int x) { return x; }\n\nDog make() { return Dog(); }\n",
			"other.cpp": "#include \"lib.hpp\"\n\nint other() { return 3; }\n\nint twice() { return other() + other(); }\n",
			"use.cpp":   "#include \"lib.hpp\"\n\nint use() { return help(1) + other() + other() + make().speak(); }\n",
		},
		edits: []work{
			{edit: edit{name: "changed parameter list", replace: []sub{{"lib.hpp", "int help(int x);", "int help(int x, int y);"},
				{"lib.cpp", "int help(int x) {", "int help(int x, int y) {"}}},
				some: []string{"use.cpp"}, none: []string{"other.cpp"}},
			{edit: edit{name: "changed result type", replace: []sub{{"lib.hpp", "Dog make();", "Cat make();"},
				{"lib.cpp", "Dog make() { return Dog(); }", "Cat make() { return Cat(); }"}}},
				some: []string{"use.cpp"}, none: []string{"other.cpp"}},
		},
	}}
	for _, c := range cases {
		for _, w := range c.edits {
			t.Run(c.name+"/"+w.edit.name, func(t *testing.T) {
				s := newSession(t)
				s.check("base", s.advance(c.base))
				b := s.only(s.advance(w.edit.apply(t, c.base)))
				s.check(w.edit.name, []linkBuild{b})
				if b.fullSites == nil {
					t.Fatalf("the session ran no full build of the edited tree, so the work cannot be compared")
				}
				for _, p := range w.none {
					if b.fullSites[p] == 0 {
						t.Fatalf("%s has no site the full build resolves, so resolving none of them proves nothing", p)
					}
					if n := b.sites[p]; n != 0 {
						t.Errorf("the refresh resolved %d of %s's %d sites, want none: nothing they read moved", n, p, b.fullSites[p])
					}
				}
				for _, p := range w.some {
					if n := b.sites[p]; n == 0 || n >= b.fullSites[p] {
						t.Errorf("the refresh resolved %d of %s's %d sites, want some and not all: only some read what moved",
							n, p, b.fullSites[p])
					}
				}
			})
		}
	}
}
