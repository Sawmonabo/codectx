package link_test

import "testing"

// TestPythonSourceGoldens runs the Python goldens through the real structural provider and the
// linker (runGoldens). Each case names the wrong answer it catches in fails.
func TestPythonSourceGoldens(t *testing.T) { runGoldens(t, pythonGoldens) }

var pythonGoldens = []golden{
	{
		name:   "c3-multiple-bases",
		anchor: "The Python Language Reference §3.3 (Special method names; attribute lookup on classes follows the method resolution order); \"The Python 2.3 Method Resolution Order\" (C3 linearisation)",
		files: map[string]string{
			"mro.py": `class A:
    def m(self):  # @am
        pass


class B:
    def m(self):  # @bm
        pass


class C(A, B):  # @c
    pass


class E(C):
    def m(self):  # @em
        pass


def run(c: C):
    C().m()  # @r1
    c.m()  # @r2
`,
		},
		// Derived twice from the source and the anchor, the second time blind (the sources, the anchors and
		// the binding rules alone); the two agree on every binding. The second derivation read the subtype
		// clause as possibly reaching E.m from `C().m()`; both settle on A.m alone, since a construction fixes
		// the receiver's class exactly.
		want: []string{
			"r1 C -> c C",
			"r1 m -> am m",
			"r2 m -> am m",
			"r2 m -> em m",
		},
		fails: "every base walked at the first level that has the method binds B.m beside A.m",
	},
	{
		name:   "super-and-class-objects",
		anchor: "The Python Language Reference §3.3 (Data model: classes, class methods); \"The Python 2.3 Method Resolution Order\"; the Python Library Reference, \"Built-in Functions: super\"",
		files: map[string]string{
			"greet.py": `class A:
    def greet(self):  # @ag
        pass

    @classmethod
    def make(cls):  # @amake
        pass


class B(A):
    def greet(self):  # @bg
        super().greet()  # @bs

    @classmethod
    def make(cls):  # @bmake
        pass


class C(B):
    def greet(self):  # @cg
        pass


def run():
    A.make()  # @r1
    B.make()  # @r2
`,
		},
		// Derived twice from the source and the anchor, the second time blind (the sources, the anchors and
		// the binding rules alone); the two agree on every binding. The second derivation read the subtype
		// clause as possibly reaching B.make from `A.make()`; both settle on A.make alone, since a class
		// object is that class exactly.
		want: []string{
			"bs greet -> ag greet",
			"r1 make -> amake make",
			"r2 make -> bmake make",
		},
		fails: "`super()` dispatched into the enclosing class's subtree binds C.greet; a class object dispatched into subclasses binds B.make from `A.make()`",
	},
	{
		name:   "lexical-scope",
		anchor: "The Python Language Reference §4.2.2 (Resolution of names)",
		files: map[string]string{
			"scope.py": `def a():
    def helper():  # @ha
        return 1
    return helper()  # @a1


def b():
    def helper():  # @hb
        return 2
    return helper()  # @b1


def c():
    return helper()  # @c1
`,
		},
		// Derived twice from the source and the anchor, the second time blind (the sources, the anchors and
		// the binding rules alone); the two agree on every binding.
		want: []string{
			"a1 helper -> ha helper",
			"b1 helper -> hb helper",
		},
		fails: "a candidate kept by name alone, outside the caller's scope, binds each helper call to both inner helpers",
	},
	{
		name:   "renamed-imports",
		anchor: "The Python Language Reference §7.11 (The import statement: `import m as n`, `from m import a as b`), §3.3 (`__init__`)",
		files: map[string]string{
			"models.py": `class User:
    def __init__(self, name):  # @init
        self.name = name

    def save(self):  # @save
        pass


def helper():  # @helper
    pass
`,
			"app.py": `import models as m
from models import User as U


def create():
    u = U("x")  # @c1
    u.save()  # @c2
    m.helper()  # @c3
    v = m.User("y")  # @c4
    v.save()  # @c5
`,
		},
		// Derived twice from the source and the anchor, the second time blind (the sources, the anchors and
		// the binding rules alone); the two agree on every binding.
		want: []string{
			"c1 U -> init __init__",
			"c2 save -> save save",
			"c3 helper -> helper helper",
			"c4 User -> init __init__",
			"c5 save -> save save",
		},
		fails: "an import's local name looked up in the imported module instead of its original name leaves U and its save unbound",
	},
	{
		name:   "method-through-the-class",
		anchor: "The Python Language Reference §3.2 (The standard type hierarchy: instance methods; a function retrieved from a class is called with every argument explicit), §6.3.4 (Calls)",
		files: map[string]string{
			"car.py": `class Engine:  # @eng
    def start(self):  # @es
        pass


class Car:  # @car
    def drive(self, e):  # @cd
        e.start()  # @d


def main():
    c = Car()  # @n1
    Car.drive(c, Engine())  # @call
`,
		},
		// Derived twice from the source and the anchor, the second time blind (the sources, the anchors and
		// the binding rules alone); the two agree on every binding.
		want: []string{
			"n1 Car -> car Car",
			"call drive -> cd drive",
			"call Engine -> eng Engine",
			"d start -> es start",
		},
		fails: "a call through the class that passes its first argument as the second parameter leaves `e.start()` unbound",
	},
	{
		name:   "known-type-over-fields",
		anchor: "The Python Language Reference §3.3 (attribute lookup: an instance's class attribute), §6.3.1 (Attribute references)",
		files: map[string]string{
			"repo.py": `def on_save():  # @onsave
    pass


class Repo:  # @repo
    def save(self):  # @rsave
        pass


class Box:  # @box
    pass


def wire():
    b = Box()  # @w1
    b.save = on_save


def run():
    r = Repo()  # @r1
    r.save()  # @r2
`,
		},
		// Derived twice from the source and the anchor, the second time blind (the sources, the anchors and
		// the binding rules alone); the two agree on every binding.
		want: []string{
			"w1 Box -> box Box",
			"r1 Repo -> repo Repo",
			"r2 save -> rsave save",
		},
		fails: "field-based binding beside a known receiver type binds `r.save()` to on_save as well",
	},
	{
		name:   "cooperative-super",
		anchor: "The Python Language Reference §3.3 (Data model); \"The Python 2.3 Method Resolution Order\" (C3 linearisation); the Python Library Reference, \"Built-in Functions: super\" (the next class after the enclosing one in the instance's method resolution order)",
		files: map[string]string{
			"coop.py": `class A:
    def m(self):  # @am
        pass


class B(A):
    def m(self):  # @bm
        super().m()  # @bs


class C(A):
    def m(self):  # @cm
        pass


class D(B, C):  # @d
    pass


def run():
    D().m()  # @r1
`,
		},
		// Derived twice from the source and the anchor, the second time blind (the sources, the anchors and
		// the binding rules alone); the two agree on every binding. Both read B's `super().m()` by the
		// instance's order: a B continues at A, a D (D, B, C, A) at C, a class that is not B's base.
		want: []string{
			"bs m -> am m",
			"bs m -> cm m",
			"r1 D -> d D",
			"r1 m -> bm m",
		},
		fails: "`super()` resolved by B's own bases alone loses C.m; dispatched like `self.m()` it binds B.m itself",
	},
	{
		name:   "argument-flow-follows-the-mro",
		anchor: "The Python Language Reference §6.3.4 (Calls: arguments bind to the called function's parameters); \"The Python 2.3 Method Resolution Order\" (C3 linearisation)",
		files: map[string]string{
			"flow.py": `class Y:  # @yc
    def ping(self):  # @yp
        pass


class A:
    def m(self, y):  # @am
        y.ping()  # @ap


class B(A):
    pass


class C(A):
    def m(self, y):  # @cm
        y.ping()  # @cp


class D(B, C):  # @dc
    pass


def run():
    D().m(Y())  # @r1
`,
		},
		// Derived twice from the source and the anchor, the second time blind (the sources, the anchors and
		// the binding rules alone); the two agree on every binding.
		want: []string{
			"r1 D -> dc D",
			"r1 m -> cm m",
			"r1 Y -> yc Y",
			"cp ping -> yp ping",
		},
		fails: "arguments sent to every same-named method rather than the one the method resolution order reaches bind A.m's `y.ping()`",
	},
}
