package link_test

import "testing"

// TestReturnFlowGoldens runs the return-flow goldens through the real
// structural provider and the linker (runGoldens): a parameter that reaches
// the callee's return makes the bound site's argument flow into the site's
// result ("summary:<position>"), and the callee's return node flows into the
// result and into the return node of a callable that returns the call
// ("return"). Each case names the wrong answer it catches in fails.
func TestReturnFlowGoldens(t *testing.T) { runGoldens(t, returnGoldens) }

var returnGoldens = []golden{
	{
		name:   "python-summary-return-and-wrapper",
		anchor: "The Python Language Reference §6.3.4 (Calls: the call's value is the value the function returns), §7.6 (The return statement)",
		files: map[string]string{
			"r.py": `def ident(p):  # @ident
    return p


def keep(p):  # @keep
    print(p)
    return 1


class Box:  # @box
    pass


def make():  # @make
    v = Box()
    return v


def wrap():  # @wrap
    return make()  # @w1


def run(x):  # @run
    a = ident(x)  # @r1
    b = make()  # @r2
    c = wrap()  # @s3
    d = keep(x)  # @r4
    print(make())  # @r5
    return a
`,
		},
		want: []string{"r.py:15 Box -> box Box exact", "r1 ident -> ident ident exact", "r2 make -> make make exact",
			"s3 wrap -> wrap wrap exact", "r4 keep -> keep keep exact", "r5 make -> make make exact", "w1 make -> make make exact"},
		returns: []string{
			"ident return return r1 a",
			"keep return return r4 d",
			"make return return r2 b",
			"make return return wrap return",
			"run x summary:1 r1 a",
			"wrap return return s3 c",
		},
		fails: "a plain return edge from inside the callee gives `make return` a flow into every caller's variable through any parameter; " +
			"a summary for every argument gives `run x` a flow into d through keep, whose parameter reaches no return; " +
			"no Into loses wrap's way back from make; reading the result of a call another call consumes gives r5 a destination",
	},
	{
		name:   "go-direct-and-function-value",
		anchor: "The Go Programming Language Specification, \"Calls\", \"Return statements\", \"Function types\"",
		files: map[string]string{
			"go.mod": "module example.com/p\n\ngo 1.22\n",
			"p.go": `package p

type T struct{}

func ident(p int) int { // @ident
	return p
}

func newT() *T { // @newt
	v := &T{}
	return v
}

func run(x int) int { // @run
	a := ident(x) // @r1
	b := newT()   // @r2
	_ = b
	f := ident
	c := f(x) // @s3
	return a + c
}
`,
		},
		want: []string{"r1 ident -> ident ident exact", "r2 newT -> newt newT exact", "s3 f -> ident ident exact"},
		returns: []string{
			"ident return return r1 a",
			"ident return return s3 c",
			"newt return return r2 b",
			"run x summary:1 r1 a",
			"run x summary:1 s3 c",
		},
		fails: "the signature cross-product carrying no return summary or return node loses s3's flows; " +
			"a summary from a parameter that reaches no return, or none from one that does, changes run x's flows",
	},
	{
		name:   "javascript-expression-body",
		anchor: "ECMA-262 §15.3 (Arrow Function Definitions: an ExpressionBody's value is the function's return value)",
		files: map[string]string{
			"a.js": `const ident = (p) => p; // @ident

function run(x) { // @run
  const a = ident(x); // @r1
  return a;
}
`,
		},
		want:    []string{"r1 ident -> ident ident exact"},
		returns: []string{"ident return return r1 a", "run x summary:1 r1 a"},
		fails:   "an expression body read as returning nothing loses the summary into a",
	},
	{
		name:   "java-static-method",
		anchor: "The Java Language Specification, Java SE 25, §15.12 (Method Invocation Expressions), §14.17 (The return Statement)",
		files: map[string]string{
			"A.java": `class A {
  A() { // @ctor
  }

  static int ident(int p) { // @ident
    return p;
  }

  static int run(int x) { // @run
    int a = ident(x); // @r1
    A o = new A(); // @r2
    return a;
  }
}
`,
		},
		want:    []string{"r1 ident -> ident ident exact", "r2 A -> ctor A exact"},
		returns: []string{"ident return return r1 a", "run x summary:1 r1 a"},
		fails:   "a return statement's node not read as returning loses the summary into a; a constructor given a return node flows it into o, though a constructor returns no value",
	},
	{
		name:   "rust-block-tail",
		anchor: "The Rust Reference, \"Block expressions\" (a block's value is its final operand), \"Call expressions\"",
		files: map[string]string{
			"Cargo.toml": "[package]\nname = \"r\"\nversion = \"0.1.0\"\nedition = \"2021\"\n",
			"src/lib.rs": `fn ident(p: i32) -> i32 { // @ident
    p
}

pub fn run(x: i32) -> i32 { // @run
    let a = ident(x); // @r1
    a
}
`,
		},
		want:    []string{"r1 ident -> ident ident exact"},
		returns: []string{"ident return return r1 a", "run x summary:1 r1 a"},
		fails:   "a block's tail not read as returning loses the summary into a",
	},
	{
		name:   "c-function",
		anchor: "ISO/IEC 9899:2018 §6.5.2.2 (Function calls), §6.8.6.4 (The return statement)",
		files: map[string]string{
			"a.c": `int ident(int p) { // @ident
  return p;
}

int run(int x) { // @run
  int a = ident(x); // @r1
  return a;
}
`,
		},
		want:    []string{"r1 ident -> ident ident exact"},
		returns: []string{"ident return return r1 a", "run x summary:1 r1 a"},
		fails:   "a return statement's node not read as returning loses the summary into a",
	},
}
