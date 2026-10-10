package link_test

import (
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"github.com/Sawmonabo/codectx/internal/model"
)

// TestStaticDispatchGoldens runs the dispatch, hierarchy, external, macro,
// include, argument-flow and open-site goldens of the static family
// through the real structural provider and the linker (runGoldens).
func TestStaticDispatchGoldens(t *testing.T) { runGoldens(t, staticGoldens) }

// fixture is every file under testdata/dir, by its path relative to it: a
// case's sources kept as files, so a compiler checks them as written.
func fixture(dir string) map[string]string {
	root := filepath.Join("testdata", dir)
	files := map[string]string{}
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, p)
		files[filepath.ToSlash(rel)] = string(b)
		return err
	})
	if err != nil {
		panic("fixture " + dir + ": " + err.Error())
	}
	return files
}

var staticGoldens = []golden{
	{
		name:   "go-interface-dispatch",
		anchor: "The Go Programming Language Specification, \"Interface types\", \"Method sets\", \"Calls\" (a call through an interface value runs the dynamic type's method)",
		files:  fixture("dispatch-go"),
		// Derived twice from the source and the anchor, the second time blind (the sources, the anchors and
		// the binding rules alone); the two agree on every fact.
		want: []string{
			"speak Sound -> spec Sound virtual",
			"direct Sound -> dog Sound exact",
			"m1 speak -> fspeak speak exact",
			"m2 speak -> fspeak speak exact",
			"m3 direct -> fdirect direct exact",
		},
		facts: []string{
			"tdog Dog implements tanimal Animal",
			"tcat Cat implements tanimal Animal",
			"dog Sound overrides spec Sound",
			"cat Sound overrides spec Sound",
		},
		sites: []string{},
		// Mutation: publish a call through an interface per implementation -> `a.Sound()` binds dog and cat.
		// Mutation: read dispatch from the technique -> `d.Sound()` on the concrete Dog carries virtual.
		// Mutation: keep the method-set satisfaction out of the hierarchy facts -> no implements, no overrides.
		fails: "a call through Animal published to Dog.Sound and Cat.Sound; a concrete receiver's call marked virtual; Go's method-set satisfaction left out of implements and overrides",
	},
	{
		name:   "java-class-and-interface-dispatch",
		anchor: "Java Language Specification §15.12.4.4 (Locate Method to Invoke), §8.4.8.1 (Overriding by Instance Methods), §8.1.4, §8.1.5, §8.8.9 (Default Constructor)",
		files:  fixture("dispatch-java"),
		// Derived twice from the source and the anchor, the second time blind (the sources, the anchors and
		// the binding rules alone); the two agree on every fact. A construction of a class that declares no
		// constructor binds the class, whose default constructor runs.
		want: []string{
			"speak sound -> asound sound virtual",
			"direct sound -> dsound sound virtual",
			"chat talk -> stalk talk virtual",
			"m1 speak -> fspeak speak exact",
			"m1 Dog -> tdog Dog exact",
			"m2 speak -> fspeak speak exact",
			"m2 Cat -> tcat Cat exact",
			"m3 direct -> fdirect direct exact",
			"m3 Dog -> tdog Dog exact",
			"m4 chat -> fchat chat exact",
			"m4 Parrot -> tparrot Parrot exact",
			"m5 chat -> fchat chat exact",
			"m5 Robot -> trobot Robot exact",
		},
		facts: []string{
			"tdog Dog extends tanimal Animal",
			"tcat Cat extends tanimal Animal",
			"tparrot Parrot implements tspeaker Speaker",
			"trobot Robot implements tspeaker Speaker",
			"dsound sound overrides asound sound",
			"csound sound overrides asound sound",
			"ptalk talk overrides stalk talk",
			"rtalk talk overrides stalk talk",
		},
		// Mutation: publish a call per override -> `a.sound()` binds Dog.sound and Cat.sound and `s.talk()`
		// Parrot.talk and Robot.talk.
		// Mutation: keep the stand-in node for a several-target site -> the bindings name no declaration.
		fails: "a call through Animal or Speaker published to each override; a class implementing an interface published as extends",
	},
	{
		name:   "java-super-and-constructed",
		anchor: "Java Language Specification §15.12.4.4, §15.11.2 (super), §15.9.4 (the class instance creation's object is exactly its class), §8.4.8.1",
		files:  fixture("dispatch-deep-java"),
		// Derived twice from the source and the anchor, the second time blind (the sources, the anchors and
		// the binding rules alone); the two agree on every fact.
		want: []string{
			"super sound -> dsound sound exact",
			"speak sound -> asound sound virtual",
			"walk sound -> dsound sound virtual",
			"m1 speak -> fspeak speak exact",
			"m1 Puppy -> tpuppy Puppy exact",
			"m2 walk -> fwalk walk exact",
			"m2 Puppy -> tpuppy Puppy exact",
			"m3 Dog -> tdog Dog exact",
			"m3 sound -> dsound sound exact",
		},
		facts: []string{
			"tdog Dog extends tanimal Animal",
			"tpuppy Puppy extends tdog Dog",
			"dsound sound overrides asound sound",
			"psound sound overrides dsound sound",
		},
		// Mutation: read dispatch from the technique -> `super.sound()` and `new Dog().sound()` carry virtual.
		// Mutation: publish every overridden method along the supertypes -> Puppy.sound also overrides
		// Animal.sound.
		fails: "a super call or a constructed object's call marked virtual; an override published beyond the method it directly overrides",
	},
	{
		name:   "rust-trait-objects-and-bounds",
		anchor: "The Rust Reference, \"Trait objects\" (dynamic dispatch), \"Trait and lifetime bounds\", \"Method-call expressions\", \"Implementations\"",
		files:  fixture("dispatch-rust"),
		// Derived twice from the source and the anchor, the second time blind (the sources, the anchors and
		// the binding rules alone); the two agree on every fact. A generic bounded by one trait is read as
		// that trait, whose implementations any instantiation may run.
		want: []string{
			"speak sound -> spec sound virtual",
			"speakg sound -> spec sound virtual",
			"direct sound -> dog sound exact",
			"m1 speak -> fspeak speak exact",
			"m2 speak -> fspeak speak exact",
			"m3 speak_g -> fspeakg speak_g exact",
			"m4 speak_g -> fspeakg speak_g exact",
			"m5 direct -> fdirect direct exact",
		},
		facts: []string{
			"tdog Dog implements tanimal Animal",
			"tcat Cat implements tanimal Animal",
			"dog sound overrides spec sound",
			"cat sound overrides spec sound",
		},
		// Mutation: publish per override -> `a.sound()` binds Dog's and Cat's sound.
		// Mutation: read dispatch from the technique -> `d.sound()` on the concrete Dog carries virtual.
		fails: "a trait object's or bounded generic's call published to each impl; a concrete receiver's call marked virtual",
	},
	{
		name:   "cpp-virtual-qualified-constructed",
		anchor: "ISO/IEC 14882 (C++) [class.virtual] (the final overrider), [expr.call] (a qualified call calls the named function), [expr.type.conv], [class.derived]",
		files:  fixture("dispatch-cpp"),
		// Derived twice from the source and the anchor, the second time blind (the sources, the anchors and
		// the binding rules alone); the two agree on every fact.
		want: []string{
			"speak sound -> asound sound virtual",
			"walk sound -> dsound sound virtual",
			"exact sound -> asound sound exact",
			"m1 speak -> fspeak speak exact",
			"m2 walk -> fwalk walk exact",
			"m3 exact -> fexact exact exact",
			"m4 Dog -> tdog Dog exact",
			"m4 sound -> dsound sound exact",
		},
		facts: []string{
			"tdog Dog extends tanimal Animal",
			"tpuppy Puppy extends tdog Dog",
			"dsound sound overrides asound sound",
			"psound sound overrides dsound sound",
			"p p flows fspeak a",
			"p p flows fwalk d",
			"p p flows fexact a",
		},
		// Mutation: read dispatch from the technique -> `a.Animal::sound()` and `Dog().sound()` carry
		// virtual. Mutation: publish per override -> `a.sound()` binds Dog's and Puppy's sound.
		fails: "a qualified call or a temporary's call marked virtual; a virtual call published to each override",
	},
	{
		name:   "cpp-template-dependent-receiver",
		anchor: "ISO/IEC 14882 (C++) [temp.dep.type] (a type that depends on a template parameter is unknown until instantiation), [class.virtual]",
		files:  fixture("dispatch-cpp-template"),
		// Derived twice from the source and the anchor, the second time blind (the sources, the anchors and
		// the binding rules alone); the two agree on every fact. The linker follows no instantiation, so the
		// call on the dependent `T&` is a name-only site.
		want: []string{
			"speak sound -> asound sound virtual",
			"direct sound -> dsound sound virtual",
			"m1 speak -> fspeak speak exact",
			"m2 speak -> fspeak speak exact",
			"m3 direct -> fdirect direct exact",
			"m4 speak_t -> fspeakt speak_t exact",
			"m5 speak_t -> fspeakt speak_t exact",
		},
		sites: []string{
			"speakt sound open receiver_untyped member 0",
		},
		// Mutation: name a simple type the unit does not declare as written -> `a.sound()` on T is an
		// external call to T::sound instead of an open site.
		fails: "a template parameter read as a type outside the project, or the dependent call bound to Animal::sound",
	},
	{
		name:   "c-function-pointers",
		anchor: "ISO/IEC 9899 (C) §6.5.2.2 (a call through a pointer to function), §6.3.2.1 (function designators), §6.7.9 (initialization)",
		files:  fixture("dispatch-c"),
		// Derived twice from the source and the anchor, the second time blind (the sources, the anchors and
		// the binding rules alone); the two agree on every fact. f and h are the only functions whose
		// address is taken; every pointer of their type may hold either.
		want: []string{
			"c1 fp -> f f exact set=2",
			"c1 fp -> h h exact set=2",
			"c2 gfp -> f f exact set=2",
			"c2 gfp -> h h exact set=2",
			"c3 cb -> f f exact set=2",
			"c3 cb -> h h exact set=2",
			"c4 run -> run run exact",
			"c5 f -> f f exact",
			"m1 g -> g g exact",
			"m2 g2 -> g2 g2 exact",
			"m3 k -> k k exact",
		},
		sites: []string{},
		// Mutation: keep the stand-in node for a several-target site -> each pointer call binds no function
		// and is an open site.
		fails: "a call through a function pointer bound to a stand-in, or to one of the two functions its type admits",
	},
	{
		name:   "java-external-override-and-call",
		anchor: "Java Language Specification §8.1.4 (a class with no superclass extends Object), §9.6.4.4 (@Override), §4.3.2 (The Class Object), §15.12.4.4",
		files: map[string]string{
			"Zoo.java": `class Animal { // @tanimal
  void sound() {} // @asound
}

class Dog extends Animal { // @tdog
  @Override
  void sound() {} // @dsound

  @Override
  public String toString() { return "dog"; } // @dstr
}

class Use {
  String show(Object o, Dog d) {
    d.sound(); // @u1
    return o.toString() // @u2
        + d.hashCode(); // @u3
  }
}
`,
		},
		// Derived twice from the source and the anchor, the second time blind (the sources, the anchors and
		// the binding rules alone); the two agree on every fact. Dog's toString overrides Object's, outside
		// the project; hashCode, which neither Dog nor Animal declares, is Object's.
		want: []string{
			"u1 sound -> dsound sound virtual",
			"u2 toString -> external java.lang.Object.toString virtual",
			"u3 hashCode -> external java.lang.Object.hashCode virtual",
		},
		facts: []string{
			"tdog Dog extends tanimal Animal",
			"dsound sound overrides asound sound",
			"dstr toString overrides external java.lang.Object.toString",
		},
		// Mutation: keep the hierarchy internal -> Dog.toString overrides nothing and `d.hashCode()` is open.
		fails: "an override of a method outside the project dropped; an inherited method of java.lang.Object left open",
	},
	{
		name:   "java-external-implements-and-extends",
		anchor: "Java Language Specification §8.1.4 (Superclasses and Subclasses), §8.1.5 (Superinterfaces), §9.1.3 (Superinterfaces and Subinterfaces), §8.4.8.1 (Overriding by Instance Methods), §9.6.4.4 (@Override)",
		files: map[string]string{
			"Tasks.java": `class A implements Runnable { // @ta
  @Override
  public void run() {} // @arun
}

class B extends Thread { // @tb
  @Override
  public void run() {} // @brun
}

interface Task extends Runnable { // @ttask
}
`,
		},
		// Derived twice from the source and the anchor, the second time blind (the sources, the anchors and
		// the binding rules alone); the two agree on every fact. Runnable and Thread are java.lang's, outside
		// the project: a class's `implements` clause names an interface, its `extends` clause a class, and an
		// interface's `extends` clause an interface.
		want: []string{},
		facts: []string{
			"ta A implements external java.lang.Runnable",
			"arun run overrides external java.lang.Runnable.run",
			"tb B extends external java.lang.Thread",
			"brun run overrides external java.lang.Thread.run",
			"ttask Task extends external java.lang.Runnable",
		},
		// Mutation: read every external supertype of a class as a class -> `A implements Runnable` is
		// published as `A extends Runnable`.
		fails: "a class's `implements` clause naming a type outside the project published as `extends` of an external class",
	},
	{
		name:   "c-macro-aliases",
		anchor: "ISO/IEC 9899 (C) §6.10.3 (Macro replacement: an object-like macro's name is replaced by its replacement list), §6.10.3.4 (Rescanning), §6.10.1 (Conditional inclusion), §6.10.2 (Source file inclusion)",
		files: map[string]string{
			"log.h": `void log_impl(void);
#define HLOG log_impl
`,
			"log.c": `#include "log.h"

void log_impl(void) {} // @impl
void log_a(void) {} // @la
void log_b(void) {} // @lb

#define LOG log_impl
#if defined(USE_A)
#define PICK log_a
#else
#define PICK log_b
#endif

void run(void) {
  LOG(); // @c1
  PICK(); // @c2
  HLOG(); // @c3
}
`,
		},
		// Derived twice from the source and the anchor, the second time blind (the sources, the anchors and
		// the binding rules alone); the two agree on every binding. PICK is defined in two arms, so either
		// function may run; a macro the included header defines is replaced in the including file.
		want: []string{
			"c1 LOG -> impl log_impl exact",
			"c2 PICK -> la log_a exact set=2",
			"c2 PICK -> lb log_b exact set=2",
			"c3 HLOG -> impl log_impl exact",
		},
		// Mutation: bind only a macro's first definition -> `PICK()` binds log_a alone, without a set size.
		// Mutation: read only the file's own macros -> `HLOG()` is open.
		fails: "a macro read as a call of its own name, one of its `#if` arms dropped, or a header's macro not seen by the file including it",
	},
	{
		name:   "c-include-matching-two-headers",
		anchor: "ISO/IEC 9899 (C) §6.10.2 (Source file inclusion: a quoted header is searched for in an implementation-defined manner, then as an angled one)",
		files: map[string]string{
			"a/util.h": `int ua(void);
`,
			"b/util.h": `int ub(void);
`,
			"main.c": `#include "util.h"

int main(void) { return 0; }
`,
		},
		// Derived twice from the source and the anchor, the second time blind (the sources, the anchors and
		// the binding rules alone); the two agree on every fact. Which util.h an include directory supplies
		// the unit does not say, so the include may name either.
		want: []string{},
		facts: []string{
			"main.c imports a/util.h set=2",
			"main.c imports b/util.h set=2",
		},
		// Mutation: keep the include's first match only -> one `imports`, without a set size.
		fails: "a quoted include bound to one of the headers it may name, or to none",
	},
	{
		name:   "go-external-interface-embedding",
		anchor: "The Go Programming Language Specification, \"Interface types\" (embedded interfaces), \"Struct types\" (embedded fields), \"Method sets\", \"Calls\"",
		files: map[string]string{
			"p.go": `package p

import "io"

type ReadCloser interface { // @rc
	io.Reader
	Close() error // @rcclose
}

type File struct { // @file
	io.Reader
}

func (File) Close() error { return nil } // @fclose

type Plain struct{} // @plain

func (Plain) Close() error { return nil } // @pclose

func use(r ReadCloser) {
	r.Read(nil) // @u1
	r.Close()   // @u2
}
`,
		},
		// Derived twice from the source and the anchor, the second time blind (the sources, the anchors and
		// the binding rules alone); the two agree on every fact. io.Reader's method set is in no table, so a
		// type satisfies ReadCloser only by embedding io.Reader too.
		want: []string{
			"u1 Read -> external io.Reader.Read virtual",
			"u2 Close -> rcclose Close virtual",
		},
		facts: []string{
			"rc ReadCloser extends external io.Reader",
			"file File implements rc ReadCloser",
			"fclose Close overrides rcclose Close",
		},
		// Mutation: satisfy an interface by its known methods alone -> Plain implements ReadCloser and its
		// Close overrides ReadCloser's.
		// Mutation: leave the external embedding out of the method set -> `r.Read(nil)` is open.
		fails: "a type that lacks the embedded external interface taken to satisfy ReadCloser; a call of the external interface's method left open",
	},
	{
		name:   "java-field-receivers",
		anchor: "Java Language Specification §8.3 (Field Declarations: a subclass inherits its superclass's fields), §15.11.1 (Field Access Using a Primary), §6.5.6.1, §8.1.3 (Inner Classes and Enclosing Instances)",
		files: map[string]string{
			"Repo.java": `class Repo {
  void save(String s) {} // @save
}
`,
			"Base.java": `class Base {
  Repo repo;
}
`,
			"Service.java": `class Service extends Base {
  void run() {
    this.repo.save("x"); // @u1
  }

  class Inner {
    void go() {
      repo.save("y"); // @u2
    }
  }
}
`,
		},
		// Derived twice from the source and the anchor, the second time blind (the sources, the anchors and
		// the binding rules alone); the two agree on every binding. repo is Base's field, inherited by
		// Service and read from Service's inner class.
		want: []string{
			"u1 save -> save save virtual",
			"u2 save -> save save virtual",
		},
		// Mutation: read a Java field receiver on its owner alone -> both calls, whose field Service
		// inherits, are open.
		fails: "a field receiver not typed through the inherited field's declaration in another file, or not read from the enclosing class",
	},
	{
		name:   "go-argument-flows",
		anchor: "The Go Programming Language Specification, \"Calls\" (arguments are assigned to the parameters; a final `...` argument passes the slice as the variadic parameter), \"Passing arguments to ... parameters\"",
		files: map[string]string{
			"p.go": `package p

import "strconv"

func g(p int) {} // @g

func h(xs ...int) {} // @h

func f(x int) { // @f
	g(x)              // @c1
	h(x, x)           // @c2
	ys := []int{x}    // @ys
	h(ys...)          // @c3
	_ = strconv.Itoa(x) // @c4
}
`,
		},
		// Derived twice from the source and the anchor, the second time blind (the sources, the anchors and
		// the binding rules alone); the two agree on every fact. strconv is outside the unit: its call binds
		// nothing and carries no flow.
		want: []string{
			"c1 g -> g g exact",
			"c2 h -> h h exact",
			"c3 h -> h h exact",
		},
		facts: []string{
			"f x flows g p",
			"f x flows h xs",
			"ys ys flows h xs",
		},
		// Mutation: bind arguments by position alone -> `h(x, x)`'s second argument and `h(ys...)` reach no
		// parameter. Mutation: emit flows on a site bound to nothing -> strconv.Itoa's argument flows.
		fails: "an argument into a variadic parameter, or a spread argument, left without its flow; a flow published at an unbound site",
	},
	{
		name:   "go-open-and-external-sites",
		anchor: "ADR-0015 decision 1 (open sites and their reasons); The Go Programming Language Specification, \"Calls\", \"Import declarations\"",
		files: map[string]string{
			"p.go": `package p

import (
	"fmt"
	"strings"
)

type T struct{}

func (T) M() {} // @m

func run(f func()) {
	var t T
	t.M()                         // @b1
	fmt.Println("x")              // @e1
	f()                           // @o1
	_ = strings.NewReader("").Len() // @o2
}
`,
		},
		// Derived twice from the source and the anchor, the second time blind (the sources, the anchors and
		// the binding rules alone); the two agree on every fact. fmt and strings are packages outside the
		// unit; the result of strings.NewReader has a type the unit declares nothing of, so `.Len()` binds
		// nothing, and no function of f's type is taken as a value.
		want: []string{
			"b1 M -> m M exact",
		},
		sites: []string{
			"o1 f open value_untracked value 0",
			"o2 Len open receiver_untyped member 0",
		},
		counts: map[string]string{
			"sites":          "5",
			"bound_sites":    "1",
			"external_sites": "2",
			"open_sites":     "2",
			"computed_sites": "0",
		},
		// Mutation: drop the sites whose receiver is a call's result -> `.Len()` has no open site and the
		// counts lose one. Mutation: count an external call as open -> open_sites is 4.
		fails: "a receiver computed by a call that binds nothing left without its open site; a call into a package outside the unit counted open",
	},
}

// TestExternalOverrideMeetsExternalCall checks that an override of a method
// outside the project and a call of it on a receiver of that type name one
// node: `Dog.toString` overrides the node `o.toString()` calls.
//
// Mutation: key the external method node of an override differently from a
// call's (externalMethod) -> the two relations name two nodes.
func TestExternalOverrideMeetsExternalCall(t *testing.T) {
	var c golden
	for _, g := range staticGoldens {
		if g.name == "java-external-override-and-call" {
			c = g
		}
	}
	l := linkSources(t, newStructural(t), c.files)
	var called, overridden []model.NodeID
	for _, r := range l.out.rels {
		to := l.out.nodes[r.Relation.To]
		if nodeResolution(to) != model.ResolutionExternal || to.Name != "toString" {
			continue
		}
		switch r.Relation.Kind {
		case model.RelCalls:
			called = append(called, to.ID)
		case model.RelOverrides:
			overridden = append(overridden, to.ID)
		}
	}
	if len(called) == 0 || len(overridden) == 0 || called[0] != overridden[0] {
		t.Fatalf("the call of toString reaches %v and the override %v, want one external node", called, overridden)
	}
}

// TestExternalSupertypeKinds checks the kind of each external type a
// hierarchy relation names: an `implements` clause's type and a Java
// interface's extended type are one interface node, an `extends` clause's
// type of a class a class node, in Java and in TypeScript.
//
// Mutation: read every external supertype as a class (drop
// wire.Supertype.Interface in the hierarchy) -> Runnable is a class node,
// and A's Runnable and Task's Runnable are two nodes.
func TestExternalSupertypeKinds(t *testing.T) {
	cases := []struct {
		golden string
		from   []golden
		// want is each external type's kind by qualified name.
		want map[string]model.NodeKind
	}{
		{"java-external-implements-and-extends", staticGoldens,
			map[string]model.NodeKind{"java.lang.Runnable": model.NodeInterface, "java.lang.Thread": model.NodeClass}},
		{"typescript-external-implements-and-extends", ecmascriptGoldens,
			map[string]model.NodeKind{"tasks.Runnable": model.NodeInterface, "threads.Thread": model.NodeClass}},
	}
	structural := newStructural(t)
	for _, c := range cases {
		t.Run(c.golden, func(t *testing.T) {
			var files map[string]string
			for _, g := range c.from {
				if g.name == c.golden {
					files = g.files
				}
			}
			if files == nil {
				t.Fatalf("no golden %s", c.golden)
			}
			l := linkSources(t, structural, files)
			got := map[string][]model.NodeKind{}
			ids := map[string]map[model.NodeID]bool{}
			for _, r := range l.out.rels {
				if r.Relation.Kind != model.RelExtends && r.Relation.Kind != model.RelImplements {
					continue
				}
				to := l.out.nodes[r.Relation.To]
				if nodeResolution(to) != model.ResolutionExternal {
					continue
				}
				if ids[to.QualifiedName] == nil {
					ids[to.QualifiedName] = map[model.NodeID]bool{}
				}
				if !ids[to.QualifiedName][to.ID] {
					ids[to.QualifiedName][to.ID] = true
					got[to.QualifiedName] = append(got[to.QualifiedName], to.Kind)
				}
			}
			for q, kind := range c.want {
				if len(got[q]) != 1 || got[q][0] != kind {
					t.Fatalf("the external type %s is published as %v, want one %s node (all: %v)", q, got[q], kind, got)
				}
			}
			if len(got) != len(c.want) {
				t.Fatalf("external supertypes %v, want %v", got, c.want)
			}
		})
	}
}
