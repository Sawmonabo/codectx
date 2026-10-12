package link_test

import "testing"

// TestJavaGoldens runs the Java goldens through the real structural provider and the
// linker (runGoldens). Each case names the wrong answer it catches in fails.
func TestJavaGoldens(t *testing.T) { runGoldens(t, javaGoldens) }

var javaGoldens = []golden{
	{
		name:   "hierarchy-and-super",
		anchor: "Java Language Specification §15.12.4.4 (Locate Method to Invoke), §15.11.2 (Accessing Superclass Members using super)",
		files: map[string]string{
			"Shapes.java": `class Shape {
  int area() { return 0; } // @sarea
  int twice() { return 2 * area(); } // @stwice
}

class Sq extends Shape {
  int area() { return super.area() + 1; } // @qarea
}

class Use {
  int go(Sq q) { return q.area(); } // @go
}
`,
		},
		// Derived twice from the source and the anchor, the second time blind (the sources, the anchors and
		// the binding rules alone); the two agree on every binding.
		want: []string{
			"stwice area -> sarea area virtual",
			"qarea area -> sarea area exact",
			"go area -> qarea area virtual",
		},
		fails: "a call through a declared type published to each override binds `area()` in Shape.twice to Sq.area; a `super.m()` call read as an instance call binds Sq.area to itself or carries virtual",
	},
	{
		name:   "static-private-overriding",
		anchor: "Java Language Specification §8.4.8 (Inheritance, Overriding, and Hiding), §15.12.3 (Compile-Time Step 3), §15.12.4.4",
		files: map[string]string{
			"A.java": `class A {
  static A of() { return null; } // @aof
  private void helper() {} // @ahelp
  void m() {} // @am

  void run() {
    of(); // @r1
    helper(); // @r2
    this.helper(); // @rthis
    m(); // @r4
  }
}

class B extends A {
  static A of() { return null; } // @bof
  void helper() {} // @bhelp
  void m() {} // @bm
}

class Use {
  void go(A a) {
    A.of(); // @g1
    B.of(); // @g2
    a.m(); // @g3
  }
}
`,
		},
		// Derived twice from the source and the anchor, the second time blind (the sources, the anchors and
		// the binding rules alone); the two agree on every binding.
		want: []string{
			"r1 of -> aof of exact",
			"r2 helper -> ahelp helper exact",
			"rthis helper -> ahelp helper exact",
			"r4 m -> am m virtual",
			"g1 of -> aof of exact",
			"g2 of -> bof of exact",
			"g3 m -> am m virtual",
		},
		fails: "a static or private method marked virtual, as if B.of or B.helper could override it; an instance method marked exact; a call through A published to B's override",
	},
	{
		name:   "arity-and-varargs",
		anchor: "Java Language Specification §15.12.2.1–§15.12.2.4 (Identify Potentially Applicable Methods; applicability by arity and variable arity), §15.9.3 (Choosing the Constructor)",
		files: map[string]string{
			"Log.java": `class Log {
  Log() {} // @c0
  Log(int level) {} // @c1
  void w(String s) {} // @w1
  void w(String s, Object o) {} // @w2
  void v(String... xs) {} // @v
}

class Use {
  void go() {
    Log l = new Log(3); // @n1
    Log d = new Log(); // @n0
    l.w("x"); // @s1
    l.w("x", d); // @s2
    l.v(); // @s3
    l.v("a", "b"); // @s4
  }
}
`,
		},
		// Derived twice from the source and the anchor, the second time blind (the sources, the anchors and
		// the binding rules alone); the two agree on every binding.
		want: []string{
			"n1 Log -> c1 Log exact",
			"n0 Log -> c0 Log exact",
			"s1 w -> w1 w virtual",
			"s2 w -> w2 w virtual",
			"s3 v -> v v virtual",
			"s4 v -> v v virtual",
		},
		fails: "a constructor or method family not filtered by argument count makes every overload a candidate; a variable-arity method dropped by the count loses both v calls",
	},
	{
		name:   "anonymous-subtype",
		anchor: "Java Language Specification §15.9.5 (Anonymous Class Declarations), §15.9.5.1 (Anonymous Constructors), §15.12.4.4",
		files: map[string]string{
			"App.java": `abstract class Handler {
  Handler() {} // @hctor
  abstract void handle(); // @habs
}

class Real extends Handler {
  Real() {} // @rctor
  void handle() {} // @real
}

class App {
  Handler make() {
    return new Handler() { // @mk
      void handle() {} // @anon
    };
  }

  void go(Real r, Handler h) {
    r.handle(); // @g1
    h.handle(); // @g2
  }
}
`,
		},
		// Derived twice from the source and the anchor, the second time blind (the sources, the anchors and
		// the binding rules alone); the two agree on every binding.
		want: []string{
			"mk Handler -> hctor Handler exact",
			"g1 handle -> real handle virtual",
			"g2 handle -> habs handle virtual",
		},
		fails: "an anonymous class body registered as the instantiated type's own methods binds `r.handle()` to it; a call through Handler published to each override instead of the abstract method no method reference of its signature implements",
	},
	{
		name:   "enclosing-class",
		anchor: "Java Language Specification §15.12.1 (Compile-Time Step 1: Determine Type to Search), §8.1.3 (Inner Classes and Enclosing Instances)",
		files: map[string]string{
			"Outer.java": `class Outer {
  void ping() {} // @ping

  class Inner {
    void run() {
      ping(); // @in
    }
  }

  void start() {
    Runnable r = new Runnable() {
      public void run() {
        ping(); // @an
      }
    };
  }
}
`,
		},
		// Derived twice from the source and the anchor, the second time blind (the sources, the anchors and
		// the binding rules alone); the two agree on every binding.
		want: []string{
			"in ping -> ping ping virtual",
			"an ping -> ping ping virtual",
		},
		fails: "a bare call in an inner or anonymous class that never walks out to the enclosing class leaves both ping calls unbound",
	},
	{
		name:   "method-reference",
		anchor: "Java Language Specification §15.13 (Method Reference Expressions), §9.8 (Functional Interfaces), §15.12.4.4",
		files: map[string]string{
			"MathOps.java": `interface Op {
  int apply(int x); // @abs
}

class MathOps {
  static int twice(int x) { return 2 * x; } // @twice
  static int neg(int x) { return -x; } // @neg

  int run(int v) {
    Op op = MathOps::twice; // @ref
    return op.apply(v); // @call
  }
}
`,
		},
		// Derived twice from the source and the anchor, the second time blind (the sources, the anchors and
		// the binding rules alone); the two agree on every binding.
		want: []string{
			"call apply -> twice twice exact",
		},
		fails: "a functional-interface call that binds the abstract method beside the referenced one, or that ignores method references, gives the wrong set",
	},
	{
		name:   "unbound-method-reference",
		anchor: "Java Language Specification §15.13.1 (Compile-Time Declaration of a Method Reference: for ReferenceType :: Identifier, an instance method takes the first parameter of the function type as its receiver and the rest as its arguments), §15.13.3, §9.8 (Functional Interfaces)",
		files: map[string]string{
			"Box.java": `interface Measure {
  int of(Box b, int scale); // @abs
}

class Box {
  int size(int scale) { return scale; } // @size
  static int twice(Box b, int scale) { return scale; } // @twice
  int bound(Box b, int scale) { return scale; } // @bound

  int run(Box b, int k) { // @run
    Measure m = Box::size;
    Measure n = Box::twice;
    Measure o = this::bound;
    return m.of(b, k); // @call
  }
}
`,
		},
		// Derived from the source and the anchor; the worker's values, signatures and argument reads are
		// probed. Box::size is unbound: of's first parameter is its receiver and its second size's scale.
		// Box::twice is static and this::bound bound, so each takes of's parameters as its own, in order.
		// All three are of of's function type, so `m.of(b, k)` may call each.
		want: []string{
			"call of -> size size exact set=3",
			"call of -> twice twice exact set=3",
			"call of -> bound bound exact set=3",
		},
		facts: []string{
			"run k flows size scale",
			"run b flows twice b",
			"run k flows twice scale",
			"run b flows bound b",
			"run k flows bound scale",
		},
		// Mutation: pass an unbound reference's first argument to its first parameter -> b flows to size's
		// scale and k to nothing. Mutation: key an unbound reference by its method's own signature -> size
		// is not of of's type and `m.of` binds twice and bound alone.
		fails: "an unbound method reference whose receiver argument binds the first declared parameter, or that joins no call of its function type",
	},
	{
		name:   "imports",
		anchor: "Java Language Specification §6.4.1 (Shadowing), §7.3 (Compilation Units: implicit java.lang import), §7.5.1 (Single-Type-Import Declarations), §15.12.1",
		files: map[string]string{
			"src/util/Process.java": `package util;

public class Process {
  public void waitFor() {} // @wait
}
`,
			"src/app/Main.java": `package app;

class Main {
  void f(Process p) {
    p.waitFor(); // @m1
  }
}
`,
			"src/app/Runner.java": `package app;

import util.Process;

class Runner {
  void f(Process p) {
    p.waitFor(); // @r1
  }
}
`,
		},
		// Derived twice from the source and the anchor, the second time blind (the sources, the anchors and
		// the binding rules alone); the two agree on every binding.
		want: []string{
			"m1 waitFor -> external java.lang.Process.waitFor virtual",
			"r1 waitFor -> wait waitFor virtual",
		},
		fails: "a simple type name looked up unit-wide without the import binds `p.waitFor()` in Main, whose Process is java.lang's; a type outside the project named other than java.lang's leaves it without its external method",
	},
	{
		name:   "fluent-chain",
		anchor: "Java Language Specification §15.12.1, §15.12.3 (the type of a method invocation is its method's return type)",
		files: map[string]string{
			"Builder.java": `class Builder {
  static Builder create() { return null; } // @create
  Builder name(String n) { return this; } // @name
  String build() { return ""; } // @build
}

class Use {
  String go() {
    return Builder.create().name("x").build(); // @go
  }
}
`,
		},
		// Derived twice from the source and the anchor, the second time blind (the sources, the anchors and
		// the binding rules alone); the two agree on every binding.
		want: []string{
			"go create -> create create exact",
			"go name -> name name virtual",
			"go build -> build build virtual",
		},
		fails: "a receiver that is another call's result resolved to nothing leaves name and build unbound",
	},
}
