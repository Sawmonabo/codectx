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
		// the binding rules alone); the two agree on every binding. Both bind each call to the one method C's
		// order reaches, A.m: `C().m()` exact, since a construction fixes the receiver's class, and `c.m()`
		// virtual, since an instance a subclass E creates may stand for the annotated C.
		want: []string{
			"r1 C -> c C exact",
			"r1 m -> am m exact",
			"r2 m -> am m virtual",
		},
		fails: "every base walked at the first level that has the method binds B.m beside A.m; a declared type's subtypes walked binds E.m from `c.m()`; dispatch read from the technique makes `C().m()` virtual",
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
			"bs greet -> ag greet exact",
			"r1 make -> amake make exact",
			"r2 make -> bmake make exact",
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
			"a1 helper -> ha helper exact",
			"b1 helper -> hb helper exact",
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
			"c1 U -> init __init__ exact",
			"c2 save -> save save exact",
			"c3 helper -> helper helper exact",
			"c4 User -> init __init__ exact",
			"c5 save -> save save exact",
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
		// the binding rules alone); the two agree on every binding and every flow. A function retrieved from
		// the class takes the instance as its first argument, so `c` flows into drive's self; Engine() is a
		// call's result, which reads no variable.
		want: []string{
			"n1 Car -> car Car exact",
			"call drive -> cd drive exact",
			"call Engine -> eng Engine exact",
			"d start -> es start exact",
		},
		facts: []string{
			"n1 c flows cd self",
		},
		fails: "a call through the class that passes its first argument as the second parameter leaves `e.start()` unbound and flows `c` into e",
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
			"w1 Box -> box Box exact",
			"r1 Repo -> repo Repo exact",
			"r2 save -> rsave save exact",
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
		// instance's order: a B continues at A, a D (D, B, C, A) at C, a class that is not B's base; each
		// is exact, and the site has two targets.
		want: []string{
			"bs m -> am m exact set=2",
			"bs m -> cm m exact set=2",
			"r1 D -> d D exact",
			"r1 m -> bm m exact",
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
			"r1 D -> dc D exact",
			"r1 m -> cm m exact",
			"r1 Y -> yc Y exact",
			"cp ping -> yp ping exact",
		},
		fails: "arguments sent to every same-named method rather than the one the method resolution order reaches bind A.m's `y.ping()`, and so does a class-body def sent as a field value of its name, which field-based resolution reads for the untyped `y`",
	},
	{
		name:   "declared-type-and-value-flow",
		anchor: "The Python Language Reference §3.3 (attribute lookup on an instance through its class's method resolution order), §6.3.4 (Calls), §8.7 (Function definitions: parameter annotations); PEP 484 (an annotation admits instances of subclasses)",
		files: map[string]string{
			"animals.py": `class Animal:
    def sound(self):  # @as
        ...


class Dog(Animal):  # @dog
    def sound(self):  # @ds
        ...


class Cat(Animal):  # @cat
    def sound(self):  # @cs
        ...


def speak(a: Animal):  # @speak
    a.sound()  # @s1


def speak_u(a):  # @speaku
    a.sound()  # @s2


def direct(d: Dog):  # @direct
    d.sound()  # @s3


def main():
    speak(Dog())  # @m1
    speak(Cat())  # @m2
    speak_u(Dog())  # @m3
    speak_u(Cat())  # @m4
    direct(Dog())  # @m5
`,
		},
		// Derived twice from the source and the anchor, the second time blind (the sources, the anchors and
		// the binding rules alone); the two agree on every binding. An annotated receiver calls the method its
		// annotation declares, once, virtual; the untyped receiver holds exactly a Dog and a Cat, each
		// constructed, so it calls each one's method, exact, two targets.
		want: []string{
			"s1 sound -> as sound virtual",
			"s2 sound -> ds sound exact set=2",
			"s2 sound -> cs sound exact set=2",
			"s3 sound -> ds sound virtual",
			"m1 speak -> speak speak exact",
			"m1 Dog -> dog Dog exact",
			"m2 speak -> speak speak exact",
			"m2 Cat -> cat Cat exact",
			"m3 speak_u -> speaku speak_u exact",
			"m3 Dog -> dog Dog exact",
			"m4 speak_u -> speaku speak_u exact",
			"m4 Cat -> cat Cat exact",
			"m5 direct -> direct direct exact",
			"m5 Dog -> dog Dog exact",
		},
		fails: "a declared type published per override (the subtype walk) gives `a.sound()` three targets and `d.sound()` none of Animal's; a placeholder for the set binds no method; dispatch read from the technique makes the flow-bound calls virtual",
	},
	{
		name:   "super-and-a-constructed-object",
		anchor: "The Python Language Reference §3.3 (Data model); the Python Library Reference, \"Built-in Functions: super\" (the next class after the enclosing one in the instance's method resolution order); §6.3.4 (Calls of a class)",
		files: map[string]string{
			"deep.py": `class Animal:
    def sound(self):  # @as
        ...


class Dog(Animal):  # @dog
    def sound(self):  # @ds
        ...


class Puppy(Dog):  # @puppy
    def sound(self):
        super().sound()  # @sup


def speak(a: Animal):  # @speak
    a.sound()  # @s1


def walk(d: Dog):  # @walk
    d.sound()  # @s2


speak(Puppy())  # @t1
walk(Puppy())  # @t2
Dog().sound()  # @t3
`,
		},
		// Derived twice from the source and the anchor, the second time blind (the sources, the anchors and
		// the binding rules alone); the two agree on every binding. `super().sound()` in Puppy runs the next
		// class's in Puppy's order, Dog's, and no class below Puppy exists; `Dog().sound()` runs Dog's own,
		// never Puppy's override; both exact.
		want: []string{
			"sup sound -> ds sound exact",
			"s1 sound -> as sound virtual",
			"s2 sound -> ds sound virtual",
			"t1 speak -> speak speak exact",
			"t1 Puppy -> puppy Puppy exact",
			"t2 walk -> walk walk exact",
			"t2 Puppy -> puppy Puppy exact",
			"t3 Dog -> dog Dog exact",
			"t3 sound -> ds sound exact",
		},
		fails: "dispatch read from the technique makes `super().sound()` virtual; a construction's result dispatched into subclasses binds Puppy.sound from `Dog().sound()`",
	},
	{
		name:   "imports-between-files",
		anchor: "The Python Language Reference §7.11 (The import statement: `import m`, `from m import a`), §5.4 (Packages)",
		files: map[string]string{
			"util.py": `def helper():  # @helper
    return 1
`,
			"mod.py": `def go():  # @go
    pass
`,
			"app.py": `from util import helper
import mod


def run():
    helper()  # @r1
    mod.go()  # @r2
`,
		},
		// Derived twice from the source and the anchor, the second time blind (the sources, the anchors and
		// the binding rules alone); the two agree on every binding and every import. Each import reaches the
		// file of the module it names: `from util import helper` the file declaring helper, `import mod` the
		// module's file.
		want: []string{
			"r1 helper -> helper helper exact",
			"r2 go -> go go exact",
		},
		facts: []string{
			"app.py imports util.py",
			"app.py imports mod.py",
		},
		fails: "an import published to the imported declaration's node, or to nothing, instead of the file of its module loses both `imports`",
	},
	{
		name:   "argument-binding",
		anchor: "The Python Language Reference §6.3.4 (Calls: positional arguments fill the leading parameters, a keyword argument the parameter it names or `**kwargs`, `*args` collects the excess positional ones, an omitted parameter takes its default)",
		files: map[string]string{
			"args.py": `def target(a, b=2, *rest, key=None, **extra):  # @tgt
    return a


def caller(p, q, r, s, t, u):  # @caller
    target(p, key=q)  # @c1
    target(r, s, t)  # @c2
    target(*u)  # @c3
    target(p, flag=q)  # @c4
    undefined_name(p)  # @c5
`,
		},
		// Derived twice from the source and the anchor, the second time blind (the sources, the anchors and
		// the binding rules alone); the two agree on every binding and every flow. c1 leaves b at its
		// default and names key; c2 fills a and b and its third argument goes to *rest; c3's spread goes to
		// *rest; c4's flag names no parameter and goes to **extra; c5 calls nothing the unit declares, so
		// its argument flows nowhere.
		want: []string{
			"c1 target -> tgt target exact",
			"c2 target -> tgt target exact",
			"c3 target -> tgt target exact",
			"c4 target -> tgt target exact",
		},
		facts: []string{
			"caller p flows tgt a",
			"caller q flows tgt key",
			"caller r flows tgt a",
			"caller s flows tgt b",
			"caller t flows tgt rest",
			"caller u flows tgt rest",
			"caller q flows tgt extra",
		},
		fails: "arguments matched by position across a keyword argument flow q into b; a spread matched by position flows u into a; an unbound site's argument published as a flow",
	},
	{
		name:   "field-based-only-for-an-untyped-receiver",
		anchor: "The Python Language Reference §3.3 (attribute lookup: a class's bases, `list` among them), §6.3.1 (Attribute references); the Python Library Reference, \"Built-in Types: list\"",
		files: map[string]string{
			"lists.py": `class L(list):
    def f(self):
        self.append(1)  # @a1


def g():  # @g
    pass


def wire(obj):
    obj.append = g


def use(h):
    h.append()  # @u1
`,
		},
		// Derived twice from the source and the anchor, the second time blind (the sources, the anchors and
		// the binding rules alone); the two agree on every binding. `self` is an L, which declares no append
		// and inherits list's, outside the project: the call is list's method, never the function some
		// object's append field holds. `h` recovers nothing, so the field it calls through decides.
		want: []string{
			"a1 append -> external builtins.list.append virtual",
			"u1 append -> g g exact",
		},
		counts: map[string]string{
			"sites": "2", "bound_sites": "1", "external_sites": "1", "open_sites": "0", "computed_sites": "0",
		},
		fails: "the field-based gate opened for a receiver whose class inherits the method from outside the project binds `self.append(1)` to g; the gate closed for every receiver leaves `h.append()` unbound",
	},
	{
		name:   "open-and-computed-sites",
		anchor: "The Python Language Reference §6.3.4 (Calls), §6.3.2 (Subscriptions), §4.2.2 (Resolution of names: an unbound name)",
		files: map[string]string{
			"open.py": `def run(obj, k, cb):
    obj[k]()  # @comp
    cb()  # @ind
    obj.close()  # @mem
    missing()  # @bare
`,
		},
		// Derived twice from the source and the anchor, the second time blind (the sources, the anchors and
		// the binding rules alone); the two agree on every site. Nothing reaches run's parameters and no
		// name is declared: each call is open for the reason its shape gives, the subscription's callee
		// names nothing.
		want: []string{},
		sites: []string{
			"comp obj[k] open computed value 0",
			"ind cb open value_untracked value 0",
			"mem close open receiver_untyped member 0",
			"bare missing open global_unbound bare 0",
		},
		counts: map[string]string{
			"sites": "4", "bound_sites": "0", "open_sites": "3", "computed_sites": "1", "external_sites": "0",
		},
		fails: "a computed callee counted as open, or bound through its subscription's names, breaks the counts; every open site given one reason misnames three",
	},
	{
		name:   "builtins-and-getattr",
		anchor: "The Python Language Reference §4.2.2 (Resolution of names: a name no enclosing, global or local scope binds is looked up in the builtins module); the Python Library Reference, \"Built-in Functions: getattr\" (getattr(x, 'foobar') is equivalent to x.foobar)",
		files: map[string]string{
			"jobs.py": `class Job:
    def run(self):  # @jr
        pass


def go(j: Job, name):
    print("x")  # @pr
    getattr(j, "run")()  # @ga
    getattr(j, "run", None)()  # @gd
    getattr(j, name)()  # @gc
    missing()  # @bare
`,
			"shadow.py": `def open(p):  # @op
    pass


def use():
    open("x")  # @oc
`,
			"star.py": `from tools import *


def use():
    len([])  # @sl
`,
		},
		// Derived twice from the source and the anchors, the second time blind (the sources, the anchors and
		// the binding rules alone); the two agree on every site. `print` and `getattr` are no name of go's
		// scopes or of the module: the builtins module declares them, so each is a call outside the project.
		// `getattr(j, "run")` is `j.run`, so calling it calls Job.run, virtual since j is annotated, with or
		// without a default; a name that is not a constant names no attribute, and its call is computed.
		// `missing` is no builtin and stays unbound; a module's own `open` shadows the builtin; a star import
		// of a module outside the project may supply `len`.
		want: []string{
			"ga run -> jr run virtual",
			"gd run -> jr run virtual",
			"oc open -> op open exact",
		},
		sites: []string{
			"bare missing open global_unbound bare 0",
			"gc getattr(j, name) open computed value 0",
			"sl len open global_unbound bare 1",
		},
		counts: map[string]string{
			"sites": "10", "bound_sites": "3", "external_sites": "4", "open_sites": "2", "computed_sites": "1",
		},
		fails: "a builtin read as an unbound global names print and getattr global_unbound; getattr's constant name not followed leaves `getattr(j, \"run\")()` computed and Job.run without its caller; a builtin taken over a module's own def or a star import binds the builtin",
	},
	{
		name:   "method-values-bound-and-unbound",
		anchor: "The Python Language Reference §3.2 (The standard type hierarchy: Instance methods; a function retrieved from a class is a plain function, one retrieved through an instance a bound method; Static method objects; Class method objects)",
		files: map[string]string{
			"values.py": `class C:  # @c
    def m(self, x):  # @m
        return x

    @staticmethod
    def sm(y):  # @sm
        return y

    @classmethod
    def cm(cls, z):  # @cm
        return z


def run(obj, n, k, p, q):  # @run
    f = C.m
    f(obj, n)  # @fv
    g = C().m  # @mk
    g(k)  # @gv
    h = C.sm
    h(p)  # @hv
    c = C.cm
    c(q)  # @cv
`,
		},
		// Derived from the source and the anchor. C.m is the plain function m, so f(obj, n) passes obj as
		// self and n as x; C().m is bound to the new instance, so g(k) passes k as x. A static method takes
		// no receiver and a class method is bound to its class, so p reaches y and q reaches z.
		want: []string{
			"mk C -> c C exact",
			"fv f -> m m exact",
			"gv g -> m m exact",
			"hv h -> sm sm exact",
			"cv c -> cm cm exact",
		},
		facts: []string{
			"run k flows m x",
			"run n flows m x",
			"run obj flows m self",
			"run p flows sm y",
			"run q flows cm z",
		},
		fails: "a method value of a class treated as a bound method flows obj into x and n nowhere; a bound one treated as unbound flows k into self; a static or class method taken through its class treated as unbound flows p and q nowhere or into cls",
	},
}
