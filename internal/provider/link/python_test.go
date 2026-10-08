package link

import (
	"context"
	"slices"
	"strconv"
	"testing"

	"github.com/Sawmonabo/codectx/internal/model"
	"github.com/Sawmonabo/codectx/internal/provider/treesitter/wire"
)

// pyTables serves hand-built tables by path, in the order a unit lists them.
type pyTables []*Table

func (ts pyTables) Table(_ context.Context, _ model.GenerationID, fv model.FileVersion) (*Table, bool, error) {
	for _, t := range ts {
		if t.Path == fv.Path {
			return t, true, nil
		}
	}
	return nil, false, nil
}

func pyDecl(kind model.NodeKind, qualified string, parent int32) Decl {
	name := qualified
	for i := len(qualified) - 1; i >= 0; i-- {
		if qualified[i] == '.' {
			name = qualified[i+1:]
			break
		}
	}
	return Decl{Endpoint: Endpoint{Kind: kind, Name: name, Qualified: qualified}, Parent: parent}
}

func pyClass(q string) Decl               { return pyDecl(model.NodeClass, q, -1) }
func pyFunc(q string) Decl                { return pyDecl(model.NodeFunction, q, -1) }
func pyMethod(q string, class int32) Decl { return pyDecl(model.NodeMethod, q, class) }

// TestResolvePythonGoldens binds each case's sites to the declarations a
// reading of the case's source under the language reference gives, derived
// twice independently. Each case's tables are what the worker and the parent
// produce for the source in its comment: sites in source order, a bare call's
// in-file candidates, the receiver types recovered inside the file.
//
// Failure modes it protects, and the mutation that fails each case:
//   - import: `mod.f()` binds nothing, or a same-named module outside the
//     importer's source root (drop the near-root preference in
//     settlePythonSite → "same-named module elsewhere" fails);
//   - construction: a class call binds the class beside its `__init__`
//     (drop the initFrom check → "construction" and "constructed across
//     files" fail);
//   - hierarchy: `self.m()` misses a subtype's override (bind only the
//     receiver's own method → "self and subtypes" fails); `super().m()`
//     binds the enclosing class's own override (drop the Parent filter in
//     resolvePythonSuper → "super" fails);
//   - flow: a receiver recovered through an assignment, a parameter or a
//     return binds nothing (skip types.receivers → the three flow cases
//     fail);
//   - field: a function stored on an attribute is not what the attribute's
//     call binds (skip types.functions → "field-based" fails);
//   - no name-only join: a bare call of a name the file neither declares nor
//     imports binds a same-named function elsewhere (index lookups by callee
//     alone → "no import" fails);
//   - relative import: `from .b import f` binds a b module outside the
//     package (treat it as absolute → "relative import" fails).
func TestResolvePythonGoldens(t *testing.T) {
	type want = Binding
	cases := []struct {
		name   string
		tables []*Table
		want   []want
	}{
		{
			// util.py: def helper(): return 1
			// main.py: import util / def run(): util.helper()
			name: "module import",
			tables: []*Table{
				{Path: "util.py", Decls: []Decl{pyFunc("helper")}},
				{Path: "main.py", Decls: []Decl{pyFunc("run")}, Sites: []Site{
					{Caller: 0, Callee: "helper", Shape: wire.ShapeStatic, Qualifier: "util", QualifierIsImport: true}}},
			},
			want: []want{{Site: Ref{1, 0}, Target: Ref{0, 0}, Detail: DetailDirect}},
		},
		{
			// class Circle: def __init__(self, r): self.r = r
			// class Square: pass
			// def make(): Circle(1); Square()
			name: "construction",
			tables: []*Table{{Path: "shapes.py",
				Decls: []Decl{pyClass("Circle"), pyMethod("Circle.__init__", 0), pyClass("Square"), pyFunc("make")},
				Sites: []Site{
					{Caller: 3, Callee: "Circle", Shape: wire.ShapeBare, Candidates: []int32{0},
						Args: []wire.Type{{}}},
					{Caller: 3, Callee: "Square", Shape: wire.ShapeBare, Candidates: []int32{2}},
				}}},
			want: []want{
				{Site: Ref{0, 0}, Target: Ref{0, 1}, Detail: DetailDirect},
				{Site: Ref{0, 1}, Target: Ref{0, 2}, Detail: DetailDirect},
			},
		},
		{
			// class Base: def area(self): return 0 / def show(self): self.area()
			// class Sq(Base): def area(self): return 4
			name: "self and subtypes",
			tables: []*Table{{Path: "shapes.py",
				Decls: []Decl{pyClass("Base"), pyMethod("Base.area", 0), pyMethod("Base.show", 0),
					pyClass("Sq"), pyMethod("Sq.area", 3)},
				Supertypes: []wire.Supertype{{Type: "Sq", Super: "Base"}},
				Sites: []Site{{Caller: 2, Callee: "area", Shape: wire.ShapeMember,
					Receiver: wire.Type{Kind: wire.TypeNamed, Name: "Base"}}}}},
			want: []want{
				{Site: Ref{0, 0}, Target: Ref{0, 1}, Detail: DetailHierarchy},
				{Site: Ref{0, 0}, Target: Ref{0, 4}, Detail: DetailHierarchy},
			},
		},
		{
			// class Dog: def bark(self): pass
			// def main(): d = Dog(); d.bark()
			name: "assignment flow",
			tables: []*Table{{Path: "pets.py",
				Decls: []Decl{pyClass("Dog"), pyMethod("Dog.bark", 0), pyFunc("main")},
				Sites: []Site{
					{Caller: 2, Callee: "Dog", Shape: wire.ShapeBare, Candidates: []int32{0}},
					{Caller: 2, Callee: "bark", Shape: wire.ShapeMember, Receiver: wire.Type{Kind: wire.TypeResult, Index: 0}},
				}}},
			want: []want{
				{Site: Ref{0, 0}, Target: Ref{0, 0}, Detail: DetailDirect},
				{Site: Ref{0, 1}, Target: Ref{0, 1}, Detail: DetailFlow},
			},
		},
		{
			// class Cat: def meow(self): pass
			// def speak(c): c.meow()
			// def main(): speak(Cat())
			name: "parameter flow",
			tables: []*Table{{Path: "pets.py",
				Decls: []Decl{pyClass("Cat"), pyMethod("Cat.meow", 0), {Endpoint: Endpoint{Kind: model.NodeFunction,
					Name: "speak", Qualified: "speak"}, Parent: -1, Described: true, Params: 1}, pyFunc("main")},
				Sites: []Site{
					{Caller: 2, Callee: "meow", Shape: wire.ShapeMember, Receiver: wire.Type{Kind: wire.TypeParam, Index: 0}},
					{Caller: 3, Callee: "speak", Shape: wire.ShapeBare, Candidates: []int32{2},
						Args: []wire.Type{{Kind: wire.TypeResult, Index: 2}}},
					{Caller: 3, Callee: "Cat", Shape: wire.ShapeBare, Candidates: []int32{0}},
				}}},
			want: []want{
				{Site: Ref{0, 0}, Target: Ref{0, 1}, Detail: DetailFlow},
				{Site: Ref{0, 1}, Target: Ref{0, 2}, Detail: DetailDirect},
				{Site: Ref{0, 2}, Target: Ref{0, 0}, Detail: DetailDirect},
			},
		},
		{
			// class Conn: def close(self): pass
			// def open_conn(): return Conn()
			// def main(): c = open_conn(); c.close()
			name: "return flow",
			tables: []*Table{{Path: "db.py",
				Decls: []Decl{pyClass("Conn"), pyMethod("Conn.close", 0), {Endpoint: Endpoint{Kind: model.NodeFunction,
					Name: "open_conn", Qualified: "open_conn"}, Parent: -1, Described: true,
					Returns: wire.Type{Kind: wire.TypeResult, Index: 0}}, pyFunc("main")},
				Sites: []Site{
					{Caller: 2, Callee: "Conn", Shape: wire.ShapeBare, Candidates: []int32{0}},
					{Caller: 3, Callee: "open_conn", Shape: wire.ShapeBare, Candidates: []int32{2}},
					{Caller: 3, Callee: "close", Shape: wire.ShapeMember, Receiver: wire.Type{Kind: wire.TypeResult, Index: 1}},
				}}},
			want: []want{
				{Site: Ref{0, 0}, Target: Ref{0, 0}, Detail: DetailDirect},
				{Site: Ref{0, 1}, Target: Ref{0, 2}, Detail: DetailDirect},
				{Site: Ref{0, 2}, Target: Ref{0, 1}, Detail: DetailFlow},
			},
		},
		{
			// def on_click(): pass
			// class Button: def __init__(self): self.handler = on_click
			//               def press(self): self.handler()
			name: "field-based",
			tables: []*Table{{Path: "ui.py",
				Decls:  []Decl{pyFunc("on_click"), pyClass("Button"), pyMethod("Button.__init__", 1), pyMethod("Button.press", 1)},
				Values: []Value{{Caller: 2, Field: "handler", Type: wire.Type{Kind: wire.TypeFunction, Name: "on_click"}}},
				Sites: []Site{{Caller: 3, Callee: "handler", Shape: wire.ShapeMember,
					Receiver: wire.Type{Kind: wire.TypeNamed, Name: "Button"}}}}},
			want: []want{{Site: Ref{0, 0}, Target: Ref{0, 0}, Detail: DetailField}},
		},
		{
			// class A: def greet(self): pass
			// class B(A): def greet(self): super().greet()
			name: "super",
			tables: []*Table{{Path: "greet.py",
				Decls:      []Decl{pyClass("A"), pyMethod("A.greet", 0), pyClass("B"), pyMethod("B.greet", 2)},
				Supertypes: []wire.Supertype{{Type: "B", Super: "A"}},
				Sites: []Site{
					{Caller: 3, Callee: "super", Shape: wire.ShapeBare},
					{Caller: 3, Callee: "greet", Shape: wire.ShapeMember, Receiver: wire.Type{Kind: wire.TypeSuper, Name: "B"}},
				}}},
			want: []want{{Site: Ref{0, 1}, Target: Ref{0, 1}, Detail: DetailHierarchy}},
		},
		{
			// util.py: def helper(): return 1
			// main.py: from util import helper / def run(): helper()
			name: "from-import",
			tables: []*Table{
				{Path: "util.py", Decls: []Decl{pyFunc("helper")}},
				{Path: "main.py", Decls: []Decl{pyFunc("run")}, Sites: []Site{{Caller: 0, Callee: "helper",
					Shape: wire.ShapeBare, Receiver: wire.Type{Kind: wire.TypeModule, Name: "util.helper"}}}},
			},
			want: []want{{Site: Ref{1, 0}, Target: Ref{0, 0}, Detail: DetailDirect}},
		},
		{
			// a.py: def helper(): return 1
			// b.py: def run(): helper()
			name: "no import",
			tables: []*Table{
				{Path: "a.py", Decls: []Decl{pyFunc("helper")}},
				{Path: "b.py", Decls: []Decl{pyFunc("run")}, Sites: []Site{{Caller: 0, Callee: "helper", Shape: wire.ShapeBare}}},
			},
		},
		{
			// models.py: class User: def __init__(self, name): self.name = name / def save(self): pass
			// app.py: import models / def create(): u = models.User("x"); u.save()
			name: "constructed across files",
			tables: []*Table{
				{Path: "models.py", Decls: []Decl{pyClass("User"), pyMethod("User.__init__", 0), pyMethod("User.save", 0)}},
				{Path: "app.py", Decls: []Decl{pyFunc("create")}, Sites: []Site{
					{Caller: 0, Callee: "User", Shape: wire.ShapeStatic, Qualifier: "models", QualifierIsImport: true,
						Args: []wire.Type{{}}},
					{Caller: 0, Callee: "save", Shape: wire.ShapeMember, Receiver: wire.Type{Kind: wire.TypeResult, Index: 0}},
				}},
			},
			want: []want{
				{Site: Ref{1, 0}, Target: Ref{0, 1}, Detail: DetailDirect},
				{Site: Ref{1, 1}, Target: Ref{0, 2}, Detail: DetailFlow},
			},
		},
		{
			// tests/util.py: def helper(): return 2
			// util.py: def helper(): return 1
			// main.py: import util / def run(): util.helper()
			name: "same-named module elsewhere",
			tables: []*Table{
				{Path: "tests/util.py", Decls: []Decl{pyFunc("helper")}},
				{Path: "util.py", Decls: []Decl{pyFunc("helper")}},
				{Path: "main.py", Decls: []Decl{pyFunc("run")}, Sites: []Site{
					{Caller: 0, Callee: "helper", Shape: wire.ShapeStatic, Qualifier: "util", QualifierIsImport: true}}},
			},
			want: []want{{Site: Ref{2, 0}, Target: Ref{1, 0}, Detail: DetailDirect}},
		},
		{
			// pkg/__init__.py: (empty)
			// pkg/b.py: def f(): pass
			// lib/b.py: def f(): pass
			// pkg/a.py: from .b import f / def g(): f()
			name: "relative import",
			tables: []*Table{
				{Path: "pkg/__init__.py"},
				{Path: "pkg/b.py", Decls: []Decl{pyFunc("f")}},
				{Path: "lib/b.py", Decls: []Decl{pyFunc("f")}},
				{Path: "pkg/a.py", Decls: []Decl{pyFunc("g")}, Sites: []Site{{Caller: 0, Callee: "f",
					Shape: wire.ShapeBare, Receiver: wire.Type{Kind: wire.TypeModule, Name: ".b.f"}}}},
			},
			want: []want{{Site: Ref{3, 0}, Target: Ref{1, 0}, Detail: DetailDirect}},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			for _, tab := range tc.tables {
				tab.Language = "python"
			}
			u := &unit{tables: pyTables(tc.tables), dir: t.TempDir(),
				members: func(yield func(model.FileVersion) error) error {
					for i, tab := range tc.tables {
						if err := yield(model.FileVersion{ID: model.FileID("f" + strconv.Itoa(i)), Path: tab.Path, Language: "python"}); err != nil {
							return err
						}
					}
					return nil
				}}
			var got []Binding
			if err := resolvePython(context.Background(), u, func(b Binding) error {
				if !slices.Contains(got, b) {
					got = append(got, b)
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			order := func(a, b Binding) int {
				if c := compareRef(a.Site, b.Site); c != 0 {
					return c
				}
				return compareRef(a.Target, b.Target)
			}
			slices.SortFunc(got, order)
			want := slices.Clone(tc.want)
			slices.SortFunc(want, order)
			if !slices.Equal(got, want) {
				t.Fatalf("bindings\n got %v\nwant %v", got, want)
			}
		})
	}
}
