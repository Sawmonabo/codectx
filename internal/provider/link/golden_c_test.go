package link_test

import "testing"

// TestCGoldens runs the C and C++ goldens through the real structural provider and the
// linker (runGoldens). Each case names the wrong answer it catches in fails.
func TestCGoldens(t *testing.T) { runGoldens(t, cGoldens) }

var cGoldens = []golden{
	{
		name:   "c-include-closure",
		anchor: "ISO/IEC 9899 (C) §6.10.2 (Source file inclusion: the directive is replaced by the entire contents of the file, whose own directives are processed), §6.10.3 (Macro replacement), §6.2.2 (Linkages of identifiers)",
		files: map[string]string{
			"inner.h": `static int helper(void) { return 1; } // @inner
#define CALL run_impl
`,
			"mid.h": `#include "inner.h"
int run_impl(void);
`,
			"other.h": `static int helper(void) { return 2; } // @other
#define CALL other_impl
`,
			"impl.c": `int run_impl(void) { return 0; } // @impl
int other_impl(void) { return 9; } // @oimpl
`,
			"a.c": `#include "mid.h"

int a(void) {
  CALL(); // @a1
  return helper(); // @a2
}
`,
			"b.c": `#include "gen.h"

int b(void) {
  return helper(); // @b1
}
`,
			"c.c": `int c(void) {
  return helper(); // @c1
}
`,
		},
		// Derived from the source and the anchor, then confirmed by a probe of the pinned grammar: a.c reads
		// inner.h through mid.h, so CALL is replaced by run_impl and helper is inner.h's, the one function of
		// internal linkage in its translation unit; other.h is in no translation unit here. b.c includes a
		// header no member file answers (generated), so inner.h and other.h may both be in it: the set of two.
		// c.c includes nothing, so no helper is in its translation unit and the call binds nothing.
		want: []string{
			"a1 CALL -> impl run_impl exact",
			"a2 helper -> inner helper exact",
			"b1 helper -> inner helper exact set=2",
			"b1 helper -> other helper exact set=2",
		},
		// Mutation: read only direct includes -> CALL binds nothing and a2 is the set of both helpers.
		// Mutation: never open the closure -> b1 binds nothing.
		// Mutation: keep every header's helper when the closure is closed -> c1 binds both helpers.
		fails: "a macro or a function of internal linkage in a header reached only through another header not seen, a header outside the translation unit read into it, or a directive the unit cannot follow treated as following nothing",
	},
	{
		name:   "linkage-and-prototypes",
		anchor: "ISO/IEC 9899 (C) §6.2.2 (Linkages of identifiers), §6.9 (External definitions), §6.5.2.2 (Function calls)",
		files: map[string]string{
			"util.h": `int add(int a, int b);
`,
			"util.c": `#include "util.h"

int add(int a, int b) { return a + b; } // @add
`,
			"a.c": `#include "util.h"

static void usage(void) {} // @ausage

int a(void) {
  usage(); // @a1
  return add(1, 2); // @a2
}
`,
			"b.c": `static void usage(void) {} // @busage

void b(void) {
  usage(); // @b1
}
`,
		},
		// Derived twice from the source and the anchor, the second time blind (the sources, the anchors and
		// the binding rules alone); the two agree on every binding.
		want: []string{
			"a1 usage -> ausage usage exact",
			"a2 add -> add add exact",
			"b1 usage -> busage usage exact",
		},
		fails: "a static function looked up across the unit makes each usage call ambiguous; a prototype not yielding to its definition binds the header",
	},
	{
		name:   "function-pointers",
		anchor: "ISO/IEC 9899 (C) §6.5.2.2 (Function calls: the called expression has pointer-to-function type), §6.3.2.1 (function designators), §6.7.6.3 (Function declarators)",
		files: map[string]string{
			"calls.c": `struct S {
  int (*cb)(int);
};

static int inc(int x) { return x + 1; } // @inc
static int dec(int x) { return x - 1; } // @dec
static long wide(long x) { return x; } // @wide

int run(struct S *s, struct S o) {
  s->cb = inc;
  o.cb = dec;
  return (*s->cb)(1) // @p1
    + (*o.cb)(2) // @p2
    + s->cb(3); // @p3
}
`,
		},
		// Derived twice from the source and the anchor, the second time blind (the sources, the anchors and
		// the binding rules alone); the two agree on every binding.
		want: []string{
			"p1 cb -> inc inc exact set=2",
			"p1 cb -> dec dec exact set=2",
			"p2 cb -> inc inc exact set=2",
			"p2 cb -> dec dec exact set=2",
			"p3 cb -> inc inc exact set=2",
			"p3 cb -> dec dec exact set=2",
		},
		fails: "a call through a dereferenced field pointer not captured, or not joined on the field's declared type, leaves the calls unbound",
	},
	{
		name:   "virtual-and-non-virtual",
		anchor: "ISO/IEC 14882 (C++) [class.virtual], [expr.call] (a non-virtual or qualified call calls the named function; otherwise the final overrider), [class.member.lookup]",
		files: map[string]string{
			"shapes.cpp": `struct A {
  void f() {} // @af
  virtual void g() {} // @ag
  void h() {
    f(); // @h1
    g(); // @h2
  }
};

struct B : A {
  void f() {} // @bf
  void g() override {} // @bg
  void k() {
    A::g(); // @k1
  }
};

struct D : B {
  void f() {} // @df
  void g() {} // @dg
};

void use(B* b, A& a) {
  b->g(); // @u1
  b->f(); // @u2
  a.f(); // @u3
}
`,
		},
		// Derived twice from the source and the anchor, the second time blind (the sources, the anchors and
		// the binding rules alone); the two agree on every binding.
		want: []string{
			"h1 f -> af f exact",
			"h2 g -> ag g virtual",
			"k1 g -> ag g exact",
			"u1 g -> bg g virtual",
			"u2 f -> bf f exact",
			"u3 f -> af f exact",
		},
		fails: "a non-virtual call marked virtual; a qualified call marked virtual; a virtual call published to each override (B::g, D::g) instead of the member the static type names",
	},
	{
		name:   "virtual-by-an-external-base",
		anchor: "ISO/IEC 14882 (C++) [class.virtual]/2 (a member function with the name and parameter list of a virtual member function of a base class overrides it and is virtual, whether or not it is so declared), [class.derived]",
		files: map[string]string{
			"w.cpp": `#include <widgets/widget.h>
class MyWidget : public ui::Widget { protected: void paintEvent(int e); }; // @mypaint
class Fancy : public MyWidget { protected: void paintEvent(int e) override; }; // @fancypaint
void draw(MyWidget* w, int e) { w->paintEvent(e); } // @draw
`,
		},
		// Derived from the source and the anchor. ui::Widget is outside the project, so whether it declares
		// paintEvent virtual is unknown; Fancy's `override` proves that MyWidget::paintEvent is virtual
		// ([class.virtual]/2 through ui::Widget). The call may dispatch, and Fancy::paintEvent overrides
		// MyWidget::paintEvent.
		want: []string{
			"draw paintEvent -> mypaint paintEvent virtual",
		},
		facts: []string{
			"fancypaint Fancy extends mypaint MyWidget",
			"mypaint MyWidget extends external ui::Widget",
			"fancypaint paintEvent overrides mypaint paintEvent",
		},
		// Mutation: read virtualness from the unit's own declarations only -> draw's call is exact and Fancy's
		// paintEvent overrides nothing.
		fails: "a member that may be virtual through a base outside the project called exact, and its override in the project dropped",
	},
	{
		name:   "by-value-receivers",
		anchor: "ISO/IEC 14882 (C++) [class.virtual] (a virtual call runs the final overrider in the dynamic type of the object), [intro.object] (an object declared of a class type, a member subobject and an array element have that type as their dynamic type), [expr.ref], [dcl.fct] (a parameter of array type is a pointer)",
		files: map[string]string{
			"receivers.cpp": `struct Animal { // @tanimal
    virtual ~Animal() {}
    virtual void sound() {} // @asound
};

struct Dog : Animal { // @tdog
    void sound() override {} // @dsound
    void bark() { // @fbark
        this->sound(); // @t1
    }
};

struct Owner { // @towner
    Dog pet;
    void call() { // @fcall
        pet.sound(); // @o1
    }
};

void byValue(Animal a) { // @fbyvalue
    a.sound(); // @v1
}

void byPointer(Animal* p) { // @fbypointer
    p->sound(); // @p1
}

void byReference(Animal& r) { // @fbyreference
    r.sound(); // @r1
}

int main() {
    Dog d;
    d.sound(); // @l1
    Dog pack[2];
    pack[0].sound(); // @a1
    byValue(d); // @m1
    byPointer(&d); // @m2
    byReference(d); // @m3
    return 0;
}
`,
		},
		// Derived twice from the source and the anchor, the second time blind (the sources, the anchors and
		// the binding rules alone); the two agree on every binding. A by-value parameter, a local, a data
		// member and an array element are objects of exactly their declared type, so their call runs that
		// type's function; a pointer, a reference and `this` may denote a derived object. Checked against
		// the pinned grammar's tree: the receivers of `pack[0]` and of `pet` are Dog objects by value.
		want: []string{
			"t1 sound -> dsound sound virtual",
			"o1 sound -> dsound sound exact",
			"v1 sound -> asound sound exact",
			"p1 sound -> asound sound virtual",
			"r1 sound -> asound sound virtual",
			"l1 sound -> dsound sound exact",
			"a1 sound -> dsound sound exact",
			"m1 byValue -> fbyvalue byValue exact",
			"m2 byPointer -> fbypointer byPointer exact",
			"m3 byReference -> fbyreference byReference exact",
		},
		// Mutation: read every C++ receiver as virtual -> `a.sound()`, `d.sound()`, `pet.sound()` and
		// `pack[0].sound()` carry virtual. Mutation: mark a pointer or reference receiver as an object ->
		// `p->sound()`, `r.sound()` and `this->sound()` carry exact.
		fails: "a call on an object declared by value marked virtual, or a call through a pointer, a reference or `this` marked exact",
	},
	{
		name:   "arity-defaults-ellipsis",
		anchor: "ISO/IEC 14882 (C++) [over.match.viable] (viable functions by parameter count, default arguments and ellipsis), [dcl.fct.default], [expr.call]",
		files: map[string]string{
			"put.cpp": `void put(int a, int b = 0) {} // @p1
void put(int a, int b, int c) {} // @p3
void dot(const char* f, ...) {} // @dv

int main() {
  put(1); // @c1
  put(1, 2); // @c2
  put(1, 2, 3); // @c3
  dot("x"); // @c4
  dot("x", 1, 2); // @c5
  return 0;
}
`,
		},
		// Derived twice from the source and the anchor, the second time blind (the sources, the anchors and
		// the binding rules alone); the two agree on every binding.
		want: []string{
			"c1 put -> p1 put exact",
			"c2 put -> p1 put exact",
			"c3 put -> p3 put exact",
			"c4 dot -> dv dot exact",
			"c5 dot -> dv dot exact",
		},
		// The two arguments c5 passes to the ellipsis have no parameter node
		// to flow into: counted, not dropped.
		lost:  map[string]string{"data_flows_to": "2"},
		fails: "an overload family not filtered by argument count, default arguments and ellipsis makes every put call ambiguous",
	},
	{
		name:   "internal-linkage",
		anchor: "ISO/IEC 14882 (C++) [basic.link] (internal linkage: namespace-scope static, unnamed namespace), [namespace.unnamed]",
		files: map[string]string{
			"a.cpp": `namespace {
int helper() { return 1; } // @ah
}

int fa() { return helper(); } // @fa
`,
			"b.cpp": `static int helper() { return 2; } // @bh

int fb() { return helper(); } // @fb
`,
			"c.cpp": `int helper(); // @cproto

int fc() { return helper(); } // @fc
`,
		},
		// Derived twice from the source and the anchor, the second time blind (the sources, the anchors and
		// the binding rules alone); the two agree on every binding.
		want: []string{
			"fa helper -> ah helper exact",
			"fb helper -> bh helper exact",
			"fc helper -> cproto helper exact",
		},
		fails: "an internal-linkage function bound from another file makes each helper call ambiguous and binds c.cpp's external call to them",
	},
	{
		name:   "extern-c-prototype",
		anchor: "ISO/IEC 14882 (C++) [dcl.link] (linkage specifications), [basic.link], [expr.call]",
		files: map[string]string{
			"lib.h": `extern "C" int cfun(void);
`,
			"lib.cpp": `#include "lib.h"

int cfun(void) { return 7; } // @def
`,
			"main.cpp": `#include "lib.h"

int go() { return cfun(); } // @go
`,
		},
		// Derived twice from the source and the anchor, the second time blind (the sources, the anchors and
		// the binding rules alone); the two agree on every binding.
		want: []string{
			"go cfun -> def cfun exact",
		},
		fails: "a braceless linkage-specification prototype not captured, or bound instead of its definition, leaves the call on the header",
	},
	{
		name:   "unnamed-namespace-types",
		anchor: "ISO/IEC 14882 (C++) [basic.link] (a name in an unnamed namespace has internal linkage, so each translation unit's `Impl` is a distinct type), [namespace.unnamed], [class.mfct]",
		files: map[string]string{
			"a.cpp": `namespace {
struct Impl {
  void run() {} // @arun
};
}

void fa() {
  Impl i;
  i.run(); // @fa
}
`,
			"b.cpp": `namespace {
struct Impl {
  void run() {} // @brun
};
}

void fb() {
  Impl i;
  i.run(); // @fb
}
`,
		},
		// Derived twice from the source and the anchor, the second time blind (the sources, the anchors and
		// the binding rules alone); the two agree on every binding.
		want: []string{
			"fa run -> arun run exact",
			"fb run -> brun run exact",
		},
		fails: "two unnamed-namespace types of one name merged across files bind each `i.run()` to both",
	},
}
