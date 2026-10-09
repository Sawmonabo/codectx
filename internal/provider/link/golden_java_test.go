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
			"stwice area -> sarea area",
			"stwice area -> qarea area",
			"qarea area -> sarea area",
			"go area -> qarea area",
		},
		fails: "a member call that skips the subtype's override, or a `super.m()` call dispatched like an instance call, loses Sq.area or binds it to its own super call",
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
			"r1 of -> aof of",
			"r2 helper -> ahelp helper",
			"rthis helper -> ahelp helper",
			"r4 m -> am m",
			"r4 m -> bm m",
			"g1 of -> aof of",
			"g2 of -> bof of",
			"g3 m -> am m",
			"g3 m -> bm m",
		},
		fails: "dispatching a static or private method into subtypes binds B.of and B.helper; dropping dispatch for an instance method loses B.m",
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
			"n1 Log -> c1 Log",
			"n0 Log -> c0 Log",
			"s1 w -> w1 w",
			"s2 w -> w2 w",
			"s3 v -> v v",
			"s4 v -> v v",
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
			"mk Handler -> hctor Handler",
			"g1 handle -> real handle",
			"g2 handle -> real handle",
			"g2 handle -> anon handle",
		},
		fails: "an anonymous class body registered as the instantiated type's own methods binds `r.handle()` to it, or drops it from `h.handle()`",
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
			"in ping -> ping ping",
			"an ping -> ping ping",
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
			"call apply -> twice twice",
		},
		fails: "a functional-interface call that binds the abstract method beside the referenced one, or that ignores method references, gives the wrong set",
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
			"r1 waitFor -> wait waitFor",
		},
		fails: "a simple type name looked up unit-wide without the import binds `p.waitFor()` in Main, whose Process is java.lang's",
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
			"go create -> create create",
			"go name -> name name",
			"go build -> build build",
		},
		fails: "a receiver that is another call's result resolved to nothing leaves name and build unbound",
	},
}
