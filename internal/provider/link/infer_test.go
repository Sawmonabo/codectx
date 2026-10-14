package link

import (
	"cmp"
	"context"
	"slices"
	"testing"

	"github.com/Sawmonabo/codectx/internal/model"
	"github.com/Sawmonabo/codectx/internal/provider/treesitter/wire"
)

// fakeTables serves hand-built tables, the way the structural provider
// stores what the worker sends, so each golden states exactly the tables the
// worker and its parent produce for the case's source.
type fakeTables map[string]*Table

func (f fakeTables) Table(_ context.Context, _ model.GenerationID, fv model.FileVersion) (*Table, bool, error) {
	t, ok := f[fv.Path]
	return t, ok, nil
}

// goldenUnit is a unit over tables, member ordinals in the order given.
func goldenUnit(t *testing.T, tables ...*Table) *unit {
	t.Helper()
	ft := fakeTables{}
	for _, tb := range tables {
		ft[tb.Path] = tb
	}
	return &unit{tables: ft, dir: t.TempDir(), members: func(yield func(model.FileVersion) error) error {
		for _, tb := range tables {
			if err := yield(model.FileVersion{ID: model.FileID(tb.Path), Path: tb.Path, Language: tb.Language}); err != nil {
				return err
			}
		}
		return nil
	}}
}

func decl(kind model.NodeKind, name string, parent int32) Decl {
	return Decl{Endpoint: Endpoint{Kind: kind, Name: name, Qualified: name}, Parent: parent, Described: true}
}

func method(name string, class int32, receiver string) Decl {
	d := decl(model.NodeMethod, name, class)
	d.Receiver = receiver
	return d
}

func exported(d Decl) Decl { d.Exported = true; return d }

func returning(d Decl, ty wire.Type) Decl { d.Returns = ty; return d }

func js(path string, decls []Decl, sites []Site, values []Value, supers ...wire.Supertype) *Table {
	return &Table{Path: path, Language: "javascript", Module: Endpoint{Kind: model.NodeModule, Name: path},
		Decls: decls, Sites: sites, Values: values, Supertypes: supers}
}

// TestInferGoldens protects the ECMAScript resolver's call facts for each
// technique: a site bound to a declaration the source does not call, or a
// call the source makes left unbound, publishes a wrong `calls` edge or
// loses one, and a call with the wrong dispatch says an override can or
// cannot run where the language says otherwise. Every case's expected
// bindings were derived twice from its source and the technique
// definitions, once independently of the implementation. The last case
// reads the class hierarchy's type lookup (hierarchy.types) for names
// written in the source; the others bind through the tables' own
// candidates, members, exports and fields.
func TestInferGoldens(t *testing.T) {
	named := func(n string) wire.Type { return wire.Type{Kind: wire.TypeNamed, Name: n} }
	result := func(i uint32) wire.Type { return wire.Type{Kind: wire.TypeResult, Index: i} }
	param := func(i uint32) wire.Type { return wire.Type{Kind: wire.TypeParam, Index: i} }
	b := func(sf, si, tf, ti int32, detail string, d model.Dispatch) Binding {
		return Binding{Site: Ref{File: sf, Index: si}, Target: Ref{File: tf, Index: ti}, Detail: detail, Dispatch: d}
	}
	exact, virtual := model.DispatchExact, model.DispatchVirtual
	cases := []struct {
		name string
		// anchor is the language reference, by standard title and section,
		// the expected bindings are read from.
		anchor string
		source string
		tables []*Table
		want   []Binding
		// mutation names the change to the resolver that fails the case.
		mutation string
	}{{
		name: "assignment flow through a field",
		// Derived twice from the source and the anchor, the second time blind (the source, the anchor and
		// the binding rules alone); the two agree on every binding.
		anchor: "ECMA-262 (15th edition, 2024) §13.3.2 (Property Accessors), §13.3.5 (The new Operator), §13.3.6 (Function Calls)",
		source: `class Engine { start() {} }
class Car {
  constructor() { this.engine = new Engine(); }
  drive() { this.engine.start(); }
}`,
		tables: []*Table{js("src/a.js",
			[]Decl{decl(model.NodeClass, "Engine", -1), method("start", 0, "Engine"),
				decl(model.NodeClass, "Car", -1), method("constructor", 2, "Car"), method("drive", 2, "Car")},
			[]Site{
				{Caller: 3, Callee: "Engine", Shape: wire.ShapeConstructor, Candidates: []int32{0}},
				{Caller: 4, Callee: "start", Shape: wire.ShapeMember, Receiver: wire.Type{Kind: wire.TypeField, Name: "engine"}},
			},
			[]Value{{Caller: 3, Field: "engine", Type: result(0)}})},
		// Engine declares no constructor, so `new Engine()` binds the class;
		// the constructed instance written to `engine` reaches the read in
		// drive, exactly an Engine.
		want:     []Binding{b(0, 0, 0, 0, DetailDirect, exact), b(0, 1, 0, 1, DetailReceiverValue, exact)},
		mutation: "a field write that adds no edge into the field's variable leaves `this.engine.start()` unbound",
	}, {
		name: "parameter flow across an import",
		// Derived twice from the source and the anchor, the second time blind (the source, the anchor and
		// the binding rules alone); the two agree on every binding.
		anchor: "ECMA-262 (15th edition, 2024) §16.2.2 (Imports: NameSpaceImport), §13.3.6 (Function Calls: argument binding)",
		source: `// src/a.js
import * as b from './b.js';
class Job { exec() {} }
export function main() { b.run(new Job()); }
// src/b.js
export function run(task) { task.exec(); }`,
		tables: []*Table{
			js("src/a.js",
				[]Decl{decl(model.NodeClass, "Job", -1), method("exec", 0, "Job"), exported(decl(model.NodeFunction, "main", -1))},
				[]Site{
					{Caller: 2, Callee: "run", Shape: wire.ShapeMember,
						Receiver: wire.Type{Kind: wire.TypeModule, Name: "./b.js"}, Args: []wire.Type{result(1)}},
					{Caller: 2, Callee: "Job", Shape: wire.ShapeConstructor, Candidates: []int32{0}},
				}, nil),
			js("src/b.js",
				[]Decl{exported(decl(model.NodeFunction, "run", -1))},
				[]Site{{Caller: 0, Callee: "exec", Shape: wire.ShapeMember, Receiver: param(0)}}, nil),
		},
		want:     []Binding{b(0, 0, 1, 0, DetailDirect, exact), b(0, 1, 0, 0, DetailDirect, exact), b(1, 0, 0, 1, DetailReceiverValue, exact)},
		mutation: "connecting an argument to a parameter of the caller instead of the callee leaves `task.exec()` unbound",
	}, {
		name: "return flow",
		// Derived twice from the source and the anchor, the second time blind (the source, the anchor and
		// the binding rules alone); the two agree on every binding.
		anchor: "ECMA-262 (15th edition, 2024) §13.3.6 (Function Calls), §14.10 (The return Statement)",
		source: `class Logger { write() {} }
function make() { return new Logger(); }
function use() { const l = make(); l.write(); }`,
		tables: []*Table{js("src/a.js",
			[]Decl{decl(model.NodeClass, "Logger", -1), method("write", 0, "Logger"),
				returning(decl(model.NodeFunction, "make", -1), result(0)), decl(model.NodeFunction, "use", -1)},
			[]Site{
				{Caller: 2, Callee: "Logger", Shape: wire.ShapeConstructor, Candidates: []int32{0}},
				{Caller: 3, Callee: "make", Shape: wire.ShapeBare, Candidates: []int32{2}},
				{Caller: 3, Callee: "write", Shape: wire.ShapeMember, Receiver: result(1)},
			}, nil)},
		want:     []Binding{b(0, 0, 0, 0, DetailDirect, exact), b(0, 1, 0, 2, DetailDirect, exact), b(0, 2, 0, 1, DetailFlow, exact)},
		mutation: "dropping a bound target's returns from the site's result leaves `l.write()` unbound",
	}, {
		name: "field-based resolution",
		// Derived twice from the source and the anchor, the second time blind (the source, the anchor and
		// the binding rules alone); the two agree on every binding. The second derivation noted that
		// nothing in the case passes `fire` an argument, so flow alone binds nothing; both take the field-
		// based answer, onClick, which is the technique the case is for.
		anchor: "ECMA-262 (15th edition, 2024) §13.3.2 (Property Accessors), §13.15.2 (Assignment to a property)",
		source: `function onClick() {}
const handlers = {};
handlers.click = onClick;
export function fire(h) { h.click(); }`,
		tables: []*Table{js("src/a.js",
			[]Decl{decl(model.NodeFunction, "onClick", -1), decl(model.NodeConstant, "handlers", -1),
				exported(decl(model.NodeFunction, "fire", -1))},
			[]Site{{Caller: 2, Callee: "click", Shape: wire.ShapeMember, Receiver: param(0)}},
			[]Value{{Caller: -1, Field: "click", Type: wire.Type{Kind: wire.TypeFunction, Name: "onClick"}}})},
		want:     []Binding{b(0, 0, 0, 0, DetailField, exact)},
		mutation: "a member site that watches only its receiver, not the field it calls, leaves `h.click()` unbound",
	}, {
		name: "an imported function written to a field",
		// Derived twice from the source and the anchor, the second time blind (the source, the anchor and
		// the binding rules alone); the two agree on every binding.
		anchor: "ECMA-262 (15th edition, 2024) §16.2.2 (Imports: ImportSpecifier `as`), §13.3.2 (Property Accessors)",
		source: `// src/a.js
import { onSave as save } from './h.js';
const actions = {};
actions.save = save;
export function run(x) { x.save(); }
// src/h.js
export function onSave() {}
export function save() {}`,
		tables: []*Table{
			js("src/a.js",
				[]Decl{decl(model.NodeConstant, "actions", -1), exported(decl(model.NodeFunction, "run", -1))},
				[]Site{{Caller: 1, Callee: "save", Shape: wire.ShapeMember, Receiver: param(0)}},
				[]Value{{Caller: -1, Field: "save", Type: wire.Type{Kind: wire.TypeModule, Name: "./h.js#onSave"}}}),
			js("src/h.js",
				[]Decl{exported(decl(model.NodeFunction, "onSave", -1)), exported(decl(model.NodeFunction, "save", -1))},
				nil, nil),
		},
		// The value is the declaration h.js exports as onSave; the local
		// name `save` an import renamed it to names nothing in h.js.
		want:     []Binding{b(0, 0, 1, 0, DetailField, exact)},
		mutation: "evaluating `./h.js#onSave` as a module path leaves `x.save()` unbound; looking up the local name binds h.js save",
	}, {
		name: "declared type and super",
		// Derived twice from the source and the anchor, the second time blind (the source, the anchor and
		// the binding rules alone); the two agree on every binding.
		anchor: "ECMA-262 (15th edition, 2024) §15.7 (Class Definitions: extends), §13.3.7 (The super Keyword)",
		source: `class Shape {
  area() { return 0; }
  describe() { return this.area(); }
}
class Square extends Shape { area() { return super.area() + 1; } }
class Circle extends Shape {}`,
		tables: []*Table{js("src/a.js",
			[]Decl{decl(model.NodeClass, "Shape", -1), method("area", 0, "Shape"), method("describe", 0, "Shape"),
				decl(model.NodeClass, "Square", -1), method("area", 3, "Square"), decl(model.NodeClass, "Circle", -1)},
			[]Site{
				{Caller: 2, Callee: "area", Shape: wire.ShapeMember, Receiver: named("Shape")},
				{Caller: 4, Callee: "area", Shape: wire.ShapeMember, Receiver: wire.Type{Kind: wire.TypeSuper, Name: "Square"}},
			}, nil,
			wire.Supertype{Type: "Square", Super: "Shape"}, wire.Supertype{Type: "Circle", Super: "Shape"})},
		// `this` is a Shape or a subclass's instance: the call binds the
		// method Shape declares, virtual, and Square's override is found at
		// query time; `super.area()` is the superclass's method, exact.
		want: []Binding{b(0, 0, 0, 1, DetailFlow, virtual), b(0, 1, 0, 1, DetailDirect, exact)},
		mutation: "walking the declared type's subtypes binds Square.area to `this.area()`; " +
			"a `super` receiver read as an instance makes its call virtual",
	}, {
		name: "field-based resolution for a class that declares nothing",
		// Derived twice from the source and the anchor, the second time blind (the source, the anchor and
		// the binding rules alone); the two agree on every binding: Box declares no close and has no
		// supertype outside the project, so only a property written on the instance can supply the call,
		// and the field that close is written to decides it.
		anchor: "ECMA-262 (15th edition, 2024) §13.3.2 (Property Accessors: a property is looked up on the object and its prototype chain), §13.15.2 (Assignment to a property), §13.3.5 (The new Operator)",
		source: `function shut() {}
class Box {}
const door = {};
door.close = shut;
export function run() { new Box().close(); }`,
		tables: []*Table{js("src/a.js",
			[]Decl{decl(model.NodeFunction, "shut", -1), decl(model.NodeClass, "Box", -1),
				decl(model.NodeConstant, "door", -1), exported(decl(model.NodeFunction, "run", -1))},
			[]Site{
				{Caller: 3, Callee: "Box", Shape: wire.ShapeConstructor, Candidates: []int32{1}},
				{Caller: 3, Callee: "close", Shape: wire.ShapeMember, Receiver: result(0)},
			},
			[]Value{{Caller: -1, Field: "close", Type: wire.Type{Kind: wire.TypeFunction, Name: "shut"}}})},
		want:     []Binding{b(0, 0, 0, 1, DetailDirect, exact), b(0, 1, 0, 0, DetailField, exact)},
		mutation: "the field-based gate closed for every receiver that recovered a value leaves `new Box().close()` unbound",
	}, {
		name: "computed callee",
		// Derived twice from the source and the anchor, the second time blind (the source, the anchor and
		// the binding rules alone); the two agree: a computed member's callee names nothing.
		anchor: "ECMA-262 (15th edition, 2024) §13.3.2 (Property Accessors: `obj[expr]` evaluates expr to a property key at run time)",
		source: `function go() {}
export function run(obj, k) { obj[k](); }`,
		tables: []*Table{js("src/a.js",
			[]Decl{decl(model.NodeFunction, "go", -1), exported(decl(model.NodeFunction, "run", -1))},
			[]Site{{Caller: 1, Callee: "obj[k]", Shape: wire.ShapeIndirect, Computed: true,
				Receiver: wire.Type{Kind: wire.TypeFunction, Name: "go"}}}, nil)},
		mutation: "a computed site recorded like an indirect one binds the value its table names",
	}}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var got []Binding
			if err := resolveECMAScript(context.Background(), goldenUnit(t, c.tables...), func(b Binding) error {
				if b.Kind == FactCall || b.Kind == FactExternalSite {
					got = append(got, b)
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			slices.SortFunc(got, func(a, b Binding) int { return cmpBinding(a, b) })
			if !slices.Equal(got, c.want) {
				t.Fatalf("%s\nsource:\n%s\ngot  %v\nwant %v\n(fails under: %s)", c.anchor, c.source, got, c.want, c.mutation)
			}
		})
	}
}

func cmpBinding(a, b Binding) int {
	if c := compareRef(a.Site, b.Site); c != 0 {
		return c
	}
	if c := cmp.Compare(a.Kind, b.Kind); c != 0 {
		return c
	}
	if c := compareRef(a.Target, b.Target); c != 0 {
		return c
	}
	return cmp.Compare(a.External, b.External)
}
