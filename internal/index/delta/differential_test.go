package delta_test

import (
	"maps"
	"slices"
	"strings"
	"testing"
)

// TestIncrementalLinkEqualsFullBuild drives one project per language the
// linker handles through a scripted sequence of edits, refreshes the linking
// unit after each from the unit before it, and compares the refresh with a
// full build of the same tree, fact for fact (session.check). Each refresh
// must also have inherited from its predecessor, so an equality reached by
// resolving everything again proves nothing and fails instead.
//
// The edits, where the language has them: a body edit; a body edit adding a
// call; a declaration inserted before the others; a rename, which unbinds
// another file's call; an overload inserted before the existing ones (Java,
// C++, TypeScript); a conditional redefinition, then an edit inside one of
// its arms; a changed returned value (`return Dog()` becomes `return Cat()`)
// that changes the binding of a call in another file (Python, JavaScript,
// TypeScript, TSX); the argument of a value cycle through two untouched
// files changed (`hop_a(Dog())` becomes `hop_a(Cat())`, the Dog passed on
// from ring_a to ring_b to ring_c and back); a call removed; a field write
// moved to another file; a body edit of a
// function another file passes arguments to, which must keep its parameters'
// identities; a file added, deleted and renamed; and for Go a dependency
// version bump, a new nested go.mod that moves a package into a unit of its
// own, and a module path change that unbinds an import of the project's own
// package; for TypeScript a tsconfig `paths` change.
//
// Mutations that fail it: drop each function's inferred values from the
// interface summary (the changed returned value leaves the other file's call
// bound to Dog.speak, and the full build binds Cat.speak); key a
// declaration's identity on its order among its duplicates or siblings (the
// inserted declaration and the inserted overload renumber the existing ones,
// and the calls carried from untouched files name nodes the full build does
// not have); skip the reverse index for a manifest field (the module path
// change leaves the importer's call bound to the package it no longer
// imports); republish only the edited file after a rename or a deletion (the
// carried calls of the old name keep their bindings); re-derive only the
// edited file from the values the others stored (the Dog that cycled
// through ring_b and ring_c back into ring_a keeps ring_b's call bound to
// Dog.speak, and the full build binds Cat.speak alone).
func TestIncrementalLinkEqualsFullBuild(t *testing.T) {
	for _, sc := range differentialScripts {
		t.Run(sc.name, func(t *testing.T) {
			s := newSession(t)
			files := sc.base
			s.check("base", s.advance(files))
			for _, e := range sc.edits {
				files = e.apply(t, files)
				s.check(e.name, s.advance(files))
			}
		})
	}
}

var differentialScripts = []script{goScript, pythonScript, javascriptScript, typescriptScript, tsxScript,
	javaLangScript, cScript, cppScript, rustScript}

// tsxScript is the JavaScript project with every source file a TSX file,
// which links in the ECMAScript family.
var tsxScript = script{name: "tsx", base: renamed(javascriptScript.base, ".js", ".tsx"), edits: ecmascriptEdits(".tsx")}

// renamed is files with each path ending in from ending in to instead.
func renamed(files map[string]string, from, to string) map[string]string {
	out := map[string]string{}
	for p, src := range files {
		if q, ok := strings.CutSuffix(p, from); ok {
			p = q + to
		}
		out[p] = src
	}
	return out
}

// TestIncrementalFlowLinkEqualsFullBuildInAnotherOrder drives the Python
// and ECMAScript projects through their edits in a second order, the value
// cycle and the changed returned value first and the removals in between,
// and compares each refresh with a full build of the same tree, fact for
// fact, as TestIncrementalLinkEqualsFullBuild does. A refresh re-derives a
// region of files from the boundary values the others stored, so which
// values are stored when an edit arrives depends on the edits before it.
//
// Mutation: re-derive only the edited file's sites from every other file's
// stored values (no closure over the cycle through it) -> after the value
// cycle edit, ring_b's speak call keeps Dog.speak, which ring_a no longer
// passes and the full build does not bind. Mutation: leave the second
// stratum's inputs in the first -> a field-based call whose receiver binds
// in the full build also binds through the field.
func TestIncrementalFlowLinkEqualsFullBuildInAnotherOrder(t *testing.T) {
	order := []string{"value cycle through untouched files", "changed returned value", "removed call",
		"body edit keeps the parameters", "moved field write", "body adding a call", "file added", "new declaration",
		"rename", "body only", "file deleted", "file renamed"}
	for _, sc := range []script{pythonScript, javascriptScript, typescriptScript, tsxScript} {
		t.Run(sc.name, func(t *testing.T) {
			byName := map[string]edit{}
			for _, e := range sc.edits {
				byName[e.name] = e
			}
			s := newSession(t)
			files := sc.base
			s.check("base", s.advance(files))
			for _, name := range order {
				e, ok := byName[name]
				if !ok {
					t.Fatalf("the %s script has no edit %q", sc.name, name)
				}
				files = e.apply(t, files)
				s.check(e.name, s.advance(files))
			}
		})
	}
}

var goScript = script{
	name: "go",
	base: map[string]string{
		"go.mod": "module example.com/app\n\ngo 1.22\n\nrequire example.com/dep v1.0.0\n",
		"a.go": `package app

func Run(x int) int { return x * 2 }

func Other() int { return 1 }
`,
		"b.go": `package app

func Twice(x int) int { return Run(x) + Run(x) }
`,
		"c.go": `package app

func Thrice(x int) int { return Twice(x) + Later() }
`,
		"d.go": `package app

import "example.com/app/util"

func Use() int { return util.Help() }
`,
		"util/u.go": `package util

func Help() int { return 1 }
`,
		"z.go": `package app

import "example.com/app/lib"

func Lib() int { return lib.Call() }
`,
		"lib/l.go": `package lib

func Call() int { return 2 }
`,
		"h.go": `package app

type Holder struct{ fn func(int) int }

func seven(n int) int { return n + 7 }

func setup(h *Holder) { h.fn = seven }
`,
		"k.go": `package app

func call(h *Holder) int { return h.fn(1) }
`,
	},
	edits: []edit{
		{name: "body only", replace: []sub{{"a.go", "return x * 2 }", "return x * 20 }"}}},
		{name: "body adding a call", replace: []sub{{"a.go", "return x * 20 }", "return x*20 + Other() }"}}},
		{name: "new declaration", replace: []sub{{"a.go", "func Run(", "func Later() int { return 0 }\n\nfunc Run("}}},
		{name: "rename", replace: []sub{{"a.go", "func Run(", "func Exec("}}},
		{name: "conditional redefinition", write: map[string]string{
			"p_linux.go": "//go:build linux\n\npackage app\n\nfunc Plat() int { return 1 }\n",
			"p_other.go": "//go:build !linux\n\npackage app\n\nfunc Plat() int { return 2 }\n",
			"q.go":       "package app\n\nfunc UsePlat() int { return Plat() }\n",
		}},
		{name: "one arm of the conditional redefinition", replace: []sub{{"p_other.go", "return 2 }", "return 2 + 20 }"}}},
		{name: "moved field write", replace: []sub{
			{"h.go", "\n\nfunc setup(h *Holder) { h.fn = seven }", ""},
			{"k.go", "func call(", "func setup(h *Holder) { h.fn = seven }\n\nfunc call("},
		}},
		{name: "body edit keeps the parameters", replace: []sub{{"b.go", "return Run(x) + Run(x) }", "return Run(x) + Run(x) + x }"}}},
		{name: "file added", write: map[string]string{"f.go": "package app\n\nfunc Fresh() int { return Exec(1) + Twice(2) }\n"}},
		{name: "file deleted", remove: []string{"b.go"}},
		{name: "file renamed", move: [][2]string{{"a.go", "core.go"}}},
		{name: "dependency version bump", replace: []sub{{"go.mod", "v1.0.0", "v1.1.0"}}},
		{name: "new nested go.mod", write: map[string]string{"util/go.mod": "module example.com/app/util\n\ngo 1.22\n"}},
		{name: "module path change", replace: []sub{{"go.mod", "module example.com/app\n", "module example.com/renamed\n"}}},
	},
}

var pythonScript = script{
	name: "python",
	base: map[string]string{
		"pets.py": `class Dog:
    def speak(self):
        return "woof"


class Cat:
    def speak(self):
        return "meow"
`,
		"factory.py": `from pets import Dog, Cat


def make():
    return Dog()


def run(x):
    return x


def feed(pet):
    return pet.speak()
`,
		"main.py": `from factory import make, run, feed, later


def main():
    pet = make()
    pet.speak()
    run(1)
    later()
    feed(make())
`,
		"ui.py": `def on_click():
    return 1


class Button:
    def __init__(self):
        self.handler = on_click

    def press(self):
        self.handler()
`,
		"ring_a.py": `from ring_b import hop_b
from pets import Dog, Cat


def hop_a(v):
    return hop_b(v)


def kick():
    return hop_a(Dog())
`,
		"ring_b.py": `from ring_c import hop_c


def hop_b(v):
    v.speak()
    return hop_c(v)
`,
		"ring_c.py": `from ring_a import hop_a


def hop_c(v):
    return hop_a(v)
`,
	},
	edits: []edit{
		{name: "body only", replace: []sub{{"pets.py", `return "woof"`, `return "woof woof"`}}},
		{name: "body adding a call", replace: []sub{{"factory.py", "def run(x):\n    return x", "def run(x):\n    make()\n    return x"}}},
		{name: "new declaration", replace: []sub{{"factory.py", "\n\ndef make():", "\n\ndef later():\n    return 0\n\n\ndef make():"}}},
		{name: "rename", replace: []sub{{"factory.py", "def run(x):", "def execute(x):"}}},
		{name: "conditional redefinition", replace: []sub{
			{"factory.py", "def feed(pet):\n    return pet.speak()\n",
				"def feed(pet):\n    return pet.speak()\n\n\nimport sys\n\nif sys.platform == \"win32\":\n    def load():\n        return 1\nelse:\n    def load():\n        return 2\n"},
			{"main.py", "from factory import make, run, feed, later\n", "from factory import make, run, feed, later, load\n"},
			{"main.py", "    feed(make())\n", "    feed(make())\n    load()\n"},
		}},
		{name: "one arm of the conditional redefinition", replace: []sub{{"factory.py", "return 2\n", "return 2 + 20\n"}}},
		{name: "changed returned value", replace: []sub{{"factory.py", "return Dog()", "return Cat()"}}},
		{name: "value cycle through untouched files", replace: []sub{{"ring_a.py", "hop_a(Dog())", "hop_a(Cat())"}}},
		{name: "removed call", replace: []sub{{"main.py", "    run(1)\n", ""}}},
		{name: "moved field write", replace: []sub{{"ui.py", "self.handler = on_click", "self.handler = None"}},
			write: map[string]string{"widgets.py": "from ui import on_click\n\n\ndef wire(b):\n    b.handler = on_click\n"}},
		{name: "body edit keeps the parameters", replace: []sub{{"factory.py", "    return pet.speak()\n", "    said = pet.speak()\n    return said\n"}}},
		{name: "file added", write: map[string]string{"extra.py": "from factory import make\n\n\ndef extra():\n    return make().speak()\n"}},
		{name: "file deleted", remove: []string{"pets.py"}},
		{name: "file renamed", move: [][2]string{{"factory.py", "maker.py"}}},
	},
}

// ecmascriptRing is a value cycle through three files with extension ext:
// ring_a passes a Dog to hopA, which hands it to ring_b's hopB, which calls
// speak on it and hands it to ring_c's hopC, which hands it back to hopA.
func ecmascriptRing(ext string) map[string]string {
	return map[string]string{
		"ring_a" + ext: `import { hopB } from "./ring_b";
import { Dog, Cat } from "./pets";

export function hopA(v) {
  return hopB(v);
}

export function kick() {
  return hopA(new Dog());
}
`,
		"ring_b" + ext: `import { hopC } from "./ring_c";

export function hopB(v) {
  v.speak();
  return hopC(v);
}
`,
		"ring_c" + ext: `import { hopA } from "./ring_a";

export function hopC(v) {
  return hopA(v);
}
`,
	}
}

// withFiles is base with every file of extra added.
func withFiles(base map[string]string, extra map[string]string) map[string]string {
	out := maps.Clone(base)
	maps.Copy(out, extra)
	return out
}

// ecmascriptEdits are the JavaScript and TypeScript edits both scripts
// share, over files named with ext.
func ecmascriptEdits(ext string) []edit {
	factory, main, ui := "factory"+ext, "main"+ext, "ui"+ext
	return []edit{
		{name: "body only", replace: []sub{{"pets" + ext, `return "woof";`, `return "woof woof";`}}},
		{name: "body adding a call", replace: []sub{{factory, "  return x;\n", "  make();\n  return x;\n"}}},
		{name: "new declaration", replace: []sub{{factory, "export function make(", "export function later() {\n  return 0;\n}\n\nexport function make("}}},
		{name: "rename", replace: []sub{{factory, "export function run(", "export function execute("}}},
		{name: "conditional redefinition", replace: []sub{
			{factory, "export function feed(", `export function pick(flag) {
  if (flag) {
    function load() {
      return 1;
    }
    return load();
  } else {
    function load() {
      return 2;
    }
    return load();
  }
}

export function feed(`},
			{main, "import { make, run, feed, later", "import { make, run, feed, later, pick"},
			{main, "  feed(make());\n", "  feed(make());\n  pick(true);\n"},
		}},
		{name: "one arm of the conditional redefinition", replace: []sub{{factory, "return 2;", "return 2 + 20;"}}},
		{name: "changed returned value", replace: []sub{{factory, "return new Dog();", "return new Cat();"}}},
		{name: "value cycle through untouched files", replace: []sub{{"ring_a" + ext, "hopA(new Dog())", "hopA(new Cat())"}}},
		{name: "removed call", replace: []sub{{main, "  run(1);\n", ""}}},
		{name: "moved field write", replace: []sub{{ui, "this.handler = onClick;", "this.handler = null;"}},
			write: map[string]string{"widgets" + ext: "import { onClick } from \"./ui\";\n\nexport function wire(b) {\n  b.handler = onClick;\n}\n"}},
		{name: "body edit keeps the parameters", replace: []sub{{factory, "  return pet.speak();\n", "  const said = pet.speak();\n  return said;\n"}}},
		{name: "file added", write: map[string]string{"extra" + ext: "import { make } from \"./factory\";\n\nexport function extra() {\n  return make().speak();\n}\n"}},
		{name: "file deleted", remove: []string{"pets" + ext}},
		{name: "file renamed", move: [][2]string{{factory, "maker" + ext}}},
	}
}

var javascriptScript = script{
	name: "javascript",
	base: withFiles(map[string]string{
		"package.json": "{\n  \"name\": \"app\",\n  \"type\": \"module\"\n}\n",
		"pets.js": `export class Dog {
  speak() {
    return "woof";
  }
}

export class Cat {
  speak() {
    return "meow";
  }
}
`,
		"factory.js": `import { Dog, Cat } from "./pets";

export function make() {
  return new Dog();
}

export function run(x) {
  return x;
}

export function feed(pet) {
  return pet.speak();
}
`,
		"main.js": `import { make, run, feed, later } from "./factory";

export function main() {
  const pet = make();
  pet.speak();
  run(1);
  later();
  feed(make());
}
`,
		"ui.js": `export function onClick() {
  return 1;
}

export class Button {
  constructor() {
    this.handler = onClick;
  }

  press() {
    this.handler();
  }
}
`,
	}, ecmascriptRing(".js")),
	edits: ecmascriptEdits(".js"),
}

var typescriptScript = script{
	name: "typescript",
	base: withFiles(map[string]string{
		"tsconfig.json": "{\n  \"compilerOptions\": {\n    \"baseUrl\": \".\",\n    \"paths\": {\n      \"@lib/*\": [\"lib/*\"]\n    }\n  }\n}\n",
		"pets.ts": `export class Dog {
  speak(): string {
    return "woof";
  }
}

export class Cat {
  speak(): string {
    return "meow";
  }
}
`,
		"factory.ts": `import { Dog, Cat } from "./pets";

export function make() {
  return new Dog();
}

export function run(x: number): number {
  return x;
}

export function feed(pet: Dog): string {
  return pet.speak();
}

export function parse(s: string): number;
export function parse(n: number): number;
export function parse(x: any): number {
  return 1;
}
`,
		"main.ts": `import { make, run, feed, later, parse } from "./factory";
import { help } from "@lib/help";

export function main(): void {
  const pet = make();
  pet.speak();
  run(1);
  later();
  feed(make());
  parse("a");
  parse(2);
  help();
}
`,
		"lib/help.ts": `export function help(): number {
  return 1;
}
`,
		"ui.ts": `export function onClick(): number {
  return 1;
}

export class Button {
  handler: () => number;

  constructor() {
    this.handler = onClick;
  }

  press(): number {
    return this.handler();
  }
}
`,
	}, ecmascriptRing(".ts")),
	// The overload goes in right after the rename, while main.ts still
	// binds both existing overloads and so carries calls naming their nodes.
	edits: append(slices.Insert(ecmascriptEdits(".ts"), 4,
		edit{name: "overload inserted before the existing ones", replace: []sub{{"factory.ts",
			"export function parse(s: string): number;\n",
			"export function parse(b: boolean): number;\nexport function parse(s: string): number;\n"}}}),
		edit{name: "tsconfig paths change", replace: []sub{{"tsconfig.json", `["lib/*"]`, `["src/lib/*"]`}}},
	),
}

var javaLangScript = script{
	name: "java",
	base: map[string]string{
		"src/app/Dog.java": `package app;

public class Dog {
  public String speak() {
    return "woof";
  }
}
`,
		"src/app/Factory.java": `package app;

public class Factory {
  public static Dog make() {
    return new Dog();
  }

  public static int run(int x) {
    return x;
  }

  public static String feed(Dog d) {
    return d.speak();
  }

  public static int parse(String s) {
    return 1;
  }

  public static int parse(int n) {
    return 2;
  }
}
`,
		"src/app/Main.java": `package app;

public class Main {
  public static void main(String[] args) {
    Dog d = Factory.make();
    d.speak();
    Factory.run(1);
    Factory.later();
    Factory.feed(d);
    Factory.parse("a");
    Factory.parse(2);
  }
}
`,
		"src/app/Holder.java": `package app;

import java.util.function.IntUnaryOperator;

public class Holder {
  IntUnaryOperator op;

  void setup() {
    this.op = Factory::run;
  }

  int call() {
    return op.applyAsInt(1);
  }
}
`,
	},
	edits: []edit{
		{name: "body only", replace: []sub{{"src/app/Dog.java", `return "woof";`, `return "woof woof";`}}},
		{name: "body adding a call", replace: []sub{{"src/app/Factory.java", "    return x;\n", "    make();\n    return x;\n"}}},
		{name: "new declaration", replace: []sub{{"src/app/Factory.java", "  public static Dog make() {",
			"  public static int later() {\n    return 0;\n  }\n\n  public static Dog make() {"}}},
		{name: "rename", replace: []sub{{"src/app/Factory.java", "public static int run(", "public static int execute("}}},
		{name: "overload inserted before the existing ones", replace: []sub{{"src/app/Factory.java", "  public static int parse(String s) {",
			"  public static int parse(double d) {\n    return 4;\n  }\n\n  public static int parse(String s) {"}}},
		{name: "conditional redefinition", replace: []sub{{"src/app/Factory.java", "  public static String feed(", `  public static int pick(boolean flag) {
    if (flag) {
      class Impl {
        int v() {
          return 5;
        }
      }
      return new Impl().v();
    } else {
      class Impl {
        int v() {
          return 6;
        }
      }
      return new Impl().v();
    }
  }

  public static String feed(`}}},
		{name: "one arm of the conditional redefinition", replace: []sub{{"src/app/Factory.java", "return 6;", "return 6 + 60;"}}},
		{name: "moved field write", replace: []sub{{"src/app/Holder.java", "  void setup() {\n    this.op = Factory::run;\n  }\n\n", ""}},
			write: map[string]string{"src/app/Wiring.java": "package app;\n\npublic class Wiring {\n  static void wire(Holder h) {\n    h.op = Factory::run;\n  }\n}\n"}},
		{name: "body edit keeps the parameters", replace: []sub{{"src/app/Factory.java", "    return d.speak();\n", "    String said = d.speak();\n    return said;\n"}}},
		{name: "file added", write: map[string]string{"src/app/Extra.java": "package app;\n\npublic class Extra {\n  static String extra() {\n    return Factory.make().speak();\n  }\n}\n"}},
		{name: "file deleted", remove: []string{"src/app/Dog.java"}},
		{name: "file renamed", move: [][2]string{{"src/app/Factory.java", "src/app/core/Factory.java"}}},
	},
}

// cFamilyScript is the C and C++ project, over a source extension and a
// header extension: a header announcing the library, the library, a caller,
// a function pointer written to a field in one file and called in another.
func cFamilyScript(name, src, hdr string, extra map[string]string, overload []edit) script {
	base := map[string]string{
		"util" + hdr: "int add(int a, int b);\nint later(void);\n",
		"util" + src: "#include \"util" + hdr + "\"\n\nint add(int a, int b) { return a + b; }\n",
		"main" + src: `#include "util` + hdr + `"

int run(int x) { return add(x, 1); }

int main(void) {
  run(2);
  return later() + pick();
}
`,
		"cb" + src: `struct S {
  int (*cb)(int);
};

int inc(int x) { return x + 1; }

void setup(struct S *s) { s->cb = inc; }
`,
		"use" + src: `struct S {
  int (*cb)(int);
};

int inc(int x);

int call(struct S *s) { return s->cb(1); }
`,
	}
	for p, s := range extra {
		base[p] = s
	}
	edits := []edit{
		{name: "body only", replace: []sub{{"util" + src, "return a + b; }", "return a + b + 0; }"}}},
		{name: "body adding a call", replace: []sub{{"main" + src, "return add(x, 1); }", "return add(x, 1) + later(); }"}}},
		{name: "new declaration", replace: []sub{{"util" + src, "int add(", "int later(void) { return 0; }\n\nint add("}}},
		{name: "rename", replace: []sub{{"util" + src, "int add(int a, int b) {", "int plus(int a, int b) {"}}},
	}
	edits = append(edits, overload...)
	edits = append(edits,
		edit{name: "conditional redefinition", write: map[string]string{
			"plat" + src: "#ifdef FAST\nint pick(void) { return 1; }\n#else\nint pick(void) { return 2; }\n#endif\n"}},
		edit{name: "one arm of the conditional redefinition", replace: []sub{{"plat" + src, "return 2; }", "return 2 + 20; }"}}},
		edit{name: "moved field write", replace: []sub{
			{"cb" + src, "\n\nvoid setup(struct S *s) { s->cb = inc; }", ""},
			{"use" + src, "int call(", "void setup(struct S *s) { s->cb = inc; }\n\nint call("},
		}},
		edit{name: "body edit keeps the parameters", replace: []sub{{"cb" + src, "return x + 1; }", "return 1 + x + 0; }"}}},
		edit{name: "file added", write: map[string]string{
			"extra" + src: "#include \"util" + hdr + "\"\n\nint extra(void) { return add(3, 4) + plus(1, 2); }\n"}},
		edit{name: "file deleted", remove: []string{"util" + src}},
		edit{name: "file renamed", move: [][2]string{{"main" + src, "app/main" + src}}},
	)
	return script{name: name, base: base, edits: edits}
}

var cScript = cFamilyScript("c", ".c", ".h", nil, nil)

var cppScript = cFamilyScript("cpp", ".cpp", ".hpp",
	map[string]string{
		"parse.cpp": "int parse(const char* s) { return 1; }\n\nint parse(int n) { return 2; }\n",
		"calls.cpp": "int parse(const char* s);\nint parse(int n);\n\nint both() { return parse(\"a\") + parse(2); }\n",
	},
	[]edit{{name: "overload inserted before the existing ones", replace: []sub{{"parse.cpp",
		"int parse(const char* s) {", "int parse(double d) { return 3; }\n\nint parse(const char* s) {"}}}})

var rustScript = script{
	name: "rust",
	base: map[string]string{
		"Cargo.toml": "[package]\nname = \"app\"\nversion = \"0.1.0\"\nedition = \"2021\"\n",
		"src/lib.rs": `pub mod call;
pub mod shapes;
pub mod util;

pub fn run(x: i32) -> i32 {
    util::add(x, 1)
}

pub fn entry() -> i32 {
    run(2) + util::later() + util::plat()
}
`,
		"src/util.rs": `pub fn add(a: i32, b: i32) -> i32 {
    a + b
}
`,
		"src/shapes.rs": `pub struct Holder {
    pub f: fn(i32) -> i32,
}

pub fn seven(n: i32) -> i32 {
    n + 7
}

pub fn setup() -> Holder {
    Holder { f: seven }
}
`,
		"src/call.rs": `use crate::shapes::Holder;

pub fn call(h: &Holder) -> i32 {
    (h.f)(1)
}
`,
	},
	edits: []edit{
		{name: "body only", replace: []sub{{"src/util.rs", "    a + b\n", "    a + b + 0\n"}}},
		{name: "body adding a call", replace: []sub{{"src/lib.rs", "    util::add(x, 1)\n", "    util::add(x, 1) + util::add(x, 2)\n"}}},
		{name: "new declaration", replace: []sub{{"src/util.rs", "pub fn add(", "pub fn later() -> i32 {\n    0\n}\n\npub fn add("}}},
		{name: "rename", replace: []sub{{"src/util.rs", "pub fn add(", "pub fn plus("}}},
		{name: "conditional redefinition", replace: []sub{{"src/util.rs", "    a + b + 0\n}\n",
			"    a + b + 0\n}\n\n#[cfg(unix)]\npub fn plat() -> i32 {\n    1\n}\n\n#[cfg(not(unix))]\npub fn plat() -> i32 {\n    2\n}\n"}}},
		{name: "one arm of the conditional redefinition", replace: []sub{{"src/util.rs", "    2\n}", "    2 + 20\n}"}}},
		{name: "moved field write", replace: []sub{
			{"src/shapes.rs", "\n\npub fn setup() -> Holder {\n    Holder { f: seven }\n}\n", "\n"},
			{"src/call.rs", "use crate::shapes::Holder;\n", "use crate::shapes::{seven, Holder};\n\npub fn setup() -> Holder {\n    Holder { f: seven }\n}\n"},
		}},
		{name: "body edit keeps the parameters", replace: []sub{{"src/shapes.rs", "    n + 7\n", "    7 + n\n"}}},
		{name: "file added", replace: []sub{{"src/lib.rs", "pub mod util;\n", "pub mod util;\npub mod extra;\n"}},
			write: map[string]string{"src/extra.rs": "pub fn extra() -> i32 {\n    crate::util::plus(1, 2)\n}\n"}},
		{name: "file deleted", remove: []string{"src/util.rs"}},
		{name: "file renamed", move: [][2]string{{"src/shapes.rs", "src/shapes/mod.rs"}}},
	},
}
