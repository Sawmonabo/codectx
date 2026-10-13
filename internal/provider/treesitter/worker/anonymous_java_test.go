package worker

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"slices"
	"strconv"
	"testing"

	ts "github.com/tree-sitter/go-tree-sitter"

	"github.com/Sawmonabo/codectx/internal/provider/treesitter/wire"
)

// TestJavaAnonymousBodiesAreClasses checks that each Java anonymous class
// body is a class declaration named `new T`, T the created type's last
// simple name, under the declaration that holds it, with its methods as its
// members; that an enum constant's body is the constant's own; and that the
// link table names each body `#n`, n the offset of its declaration's name
// token, in its methods' receivers and its supertype row, so the linker
// reads the body as that declaration.
//
// Mutation: drop the anonymous capture from java.scm -> the bodies' methods
// are members of start and r, and no `new` class exists.
// Mutation: name the body by its type alone -> `Runnable`, a name a written
// type spells.
// Mutation: take the qualified type as written -> `new java.util.Comparator`.
// Mutation: name a body by a file-wide ordinal -> the receivers are `#1`,
// `#2`, …, which name no declaration.
func TestJavaAnonymousBodiesAreClasses(t *testing.T) {
	const src = "package p;\nclass Sample {\n  Runnable r = new Runnable() { public void run() {} };\n" +
		"  void start() {\n    new Thread() { public void run() {} }.start();\n" +
		"    new java.util.Comparator<String>() { public int compare(String a, String b) { return 0; } };\n  }\n}\n" +
		"enum Color { RED { void m() {} }, BLUE; void m() {} }\n"
	w := &state{parsers: map[string]*ts.Parser{}, queries: map[string]*ts.Query{}}
	defer w.close()
	var out bytes.Buffer
	if _, fail, err := w.parse(&out, wire.Request{Language: "java", Path: "Sample.java", SourceBytes: uint64(len(src))}, []byte(src)); err != nil || fail != nil {
		t.Fatalf("parse: %v, %+v", err, fail)
	}
	var decls []wire.Decl
	var l wire.Link
	for {
		kind, payload, err := wire.ReadMessage(&out, 0)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		switch kind {
		case wire.KindDecl:
			var d wire.Decl
			if err := json.Unmarshal(payload, &d); err != nil {
				t.Fatal(err)
			}
			decls = append(decls, d)
		case wire.KindLink:
			if l, err = wire.DecodeLink(payload); err != nil {
				t.Fatal(err)
			}
		}
	}
	at := map[string]uint32{}
	var got []string
	for _, d := range decls {
		got = append(got, d.Kind+" "+d.Qualified)
		at[d.Qualified] = d.NameStart
	}
	want := []string{"class p.Sample", "field p.Sample.r", "class p.Sample.r.new Runnable",
		"method p.Sample.r.new Runnable.run", "method p.Sample.start", "class p.Sample.start.new Thread",
		"method p.Sample.start.new Thread.run", "class p.Sample.start.new Comparator",
		"method p.Sample.start.new Comparator.compare", "enum p.Color", "constant p.Color.RED", "method p.Color.RED.m",
		"constant p.Color.BLUE", "method p.Color.m"}
	if !slices.Equal(got, want) {
		t.Fatalf("declarations:\n got %q\nwant %q", got, want)
	}
	body := func(q string) string { return "#" + strconv.FormatUint(uint64(at[q]), 10) }
	var supers []wire.Supertype
	for _, s := range l.Supertypes {
		supers = append(supers, wire.Supertype{Type: s.Type, Super: s.Super})
	}
	wantSupers := []wire.Supertype{{Type: body("p.Sample.r.new Runnable"), Super: "Runnable"},
		{Type: body("p.Sample.start.new Thread"), Super: "Thread"},
		{Type: body("p.Sample.start.new Comparator"), Super: "java.util.Comparator"},
		{Type: body("p.Color.RED"), Super: "Color"}}
	if !slices.Equal(supers, wantSupers) {
		t.Errorf("supertypes:\n got %+v\nwant %+v", supers, wantSupers)
	}
	receivers := map[string]string{}
	for _, e := range l.Exports {
		if e.Receiver != "" {
			receivers[src[e.Name.Start:e.Name.End]+"@"+strconv.Itoa(int(e.Name.Start))] = e.Receiver
		}
	}
	for q, owner := range map[string]string{"p.Sample.r.new Runnable.run": "p.Sample.r.new Runnable",
		"p.Sample.start.new Thread.run":         "p.Sample.start.new Thread",
		"p.Sample.start.new Comparator.compare": "p.Sample.start.new Comparator", "p.Color.RED.m": "p.Color.RED"} {
		name := q[len(owner)+1:]
		if got := receivers[name+"@"+strconv.Itoa(int(at[q]))]; got != body(owner) {
			t.Errorf("%s receiver %q, want %q", q, got, body(owner))
		}
	}
}
