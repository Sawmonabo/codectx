package worker

import (
	"testing"

	"github.com/Sawmonabo/codectx/internal/provider/treesitter/wire"
)

// javaFactsSource is compile-checked Java: fields of declared and generic
// types, an overriding method marked each way, and calls whose arguments
// and receivers read locals and fields.
const javaFactsSource = `import java.util.List;

class Base {
    void run(int a) {}
}

class Node {
    Node next;
    void go() {}
}

class Holder {
    static Node shared;
}

class C extends Base {
    List<String> items;
    Node head;
    int count;

    void take(int a, int b, int c) {}

    int id(int v) { return v; }

    @Override
    void run(int a) {
        int x = a;
        int y = 2;
        take(x + 1, y, id(a));
        take(1, 2, 3);
        this.head.go();
        head.go();
        Node n = new Node();
        n.next.go();
        Holder.shared.go();
    }

    @java.lang.Override
    public String toString() { return ""; }

    void plain() {}
}
`

// inner is the span of the len bytes at off within s.
func inner(s wire.Span, off, n uint32) wire.Span {
	return wire.Span{Start: s.Start + off, End: s.Start + off + n}
}

// TestJavaArgumentsReadTheirOwnVariables checks that each argument of a Java
// call reads the variables its own expression names, a call result and a
// literal none, every read being a variable the function messages name.
//
// Mutation: Reads from the whole statement instead of the argument -> every
// argument of the first take reads both x and y. Mutation: a call result's
// arguments counted as the argument's reads -> id(a) reads a.
func TestJavaArgumentsReadTheirOwnVariables(t *testing.T) {
	src := javaFactsSource
	l, fns := linkOf(t, "java", "C.java", src)
	x := inner(textAt(t, src, "int x", 0), 4, 1)
	y := inner(textAt(t, src, "int y", 0), 4, 1)
	wantArguments(t, src, fns, siteNamed(l, textAt(t, src, "take", 1)), []argWant{
		{at: "x + 1", reads: []wire.Span{x}}, {at: "y", reads: []wire.Span{y}}, {at: "id(a)"}})
	// Mutation: a literal reads a variable -> the second take's arguments
	// read something.
	wantArguments(t, src, fns, siteNamed(l, textAt(t, src, "take", 2)), []argWant{{at: "1"}, {at: "2"}, {at: "3"}})
}

// TestJavaOverrideMarksOnlyTheAnnotatedMethods checks that @Override and
// @java.lang.Override mark their methods and nothing else is marked.
//
// Mutation: mark every method -> plain, take and Base.run are marked.
// Mutation: read only the simple annotation name -> toString is unmarked.
func TestJavaOverrideMarksOnlyTheAnnotatedMethods(t *testing.T) {
	src := javaFactsSource
	l, _ := linkOf(t, "java", "C.java", src)
	for _, c := range []struct {
		name     string
		nth      int
		override bool
	}{{"run", 0, false}, {"run", 1, true}, {"toString", 0, true}, {"plain", 0, false}, {"take", 0, false}, {"go", 0, false}} {
		e := exportNamed(l, textAt(t, src, c.name, c.nth))
		if e == nil {
			t.Fatalf("no export for %s #%d", c.name, c.nth)
		}
		if e.Override != c.override {
			t.Fatalf("%s #%d override %v, want %v", c.name, c.nth, e.Override, c.override)
		}
	}
}

// TestJavaFieldExportsAndFieldReceivers checks that each field of a declared
// class type is an export typed by that type without its type arguments, a
// primitive field none, and that a receiver reading a field is spelled
// "Owner.field": this.f and a bare f as the enclosing class's, x.f as the
// field of x's declared type, T.f as the field of the type as written.
//
// Mutation: keep the type arguments -> items returns "List<String>".
// Mutation: send a field read as its declared type or a bare field name ->
// the receivers are TypeNamed Node or TypeField "head".
func TestJavaFieldExportsAndFieldReceivers(t *testing.T) {
	src := javaFactsSource
	l, _ := linkOf(t, "java", "C.java", src)
	for _, c := range []struct{ field, typ string }{{"items", "List"}, {"head", "Node"}, {"next", "Node"}, {"shared", "Node"}} {
		e := exportNamed(l, textAt(t, src, c.field, 0))
		if e == nil || e.Returns != (wire.Type{Kind: wire.TypeNamed, Name: c.typ}) {
			t.Fatalf("field %s exports %+v, want Returns TypeNamed %s", c.field, e, c.typ)
		}
	}
	if e := exportNamed(l, textAt(t, src, "count", 0)); e != nil {
		t.Fatalf("a primitive field sent an export %+v", *e)
	}
	for _, c := range []struct {
		call  string
		nth   int
		field string
	}{{"this.head.go", 0, "C.head"}, {"head.go", 1, "C.head"}, {"n.next.go", 0, "Node.next"}, {"Holder.shared.go", 0, "Holder.shared"}} {
		at := textAt(t, src, c.call, c.nth)
		s := siteNamed(l, wire.Span{Start: at.End - 2, End: at.End})
		if s == nil {
			t.Fatalf("%s sent no site", c.call)
		}
		if s.Shape != wire.ShapeMember || s.Receiver != (wire.Type{Kind: wire.TypeField, Name: c.field}) {
			t.Fatalf("%s: shape %d receiver %+v, want a member call on TypeField %s", c.call, s.Shape, s.Receiver, c.field)
		}
	}
}
