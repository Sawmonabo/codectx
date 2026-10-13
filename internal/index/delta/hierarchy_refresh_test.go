package delta_test

import "testing"

// shapesBase is a Go unit whose hierarchy carries an interface satisfied
// implicitly, a struct embedding another for a promoted method, and callers
// that dispatch through both.
var shapesBase = map[string]string{
	"go.mod":    "module example.com/shapes\n\ngo 1.22\n",
	"shape.go":  "package shapes\n\ntype Shape interface{ Area() int }\n\ntype Base struct{}\n\nfunc (Base) Name() string { return \"base\" }\n",
	"other.go":  "package shapes\n\ntype Other struct{}\n\nfunc (Other) Name() string { return \"other\" }\n",
	"square.go": "package shapes\n\ntype Square struct{ Base }\n\nfunc (Square) Area() int { return 4 }\n",
	"use.go":    "package shapes\n\nfunc total(s Shape) int { return s.Area() }\n\nfunc name(q Square) string { return q.Name() }\n",
}

// TestLayoutBreakHierarchyEqualsFullBuild edits shapesBase so that every
// refresh breaks the layout and rebuilds the hierarchy from the stored
// projections of the members whose digest held and the tables of the others,
// and compares each refresh with a full build of the same tree, fact for fact
// (session.check): a method moves to a new file, a type that satisfies Shape is
// added, its method is removed, the interface gains a method no type has, the
// embedded type changes, and the added type's file is removed.
//
// Mutation that fails it: hand the rebuild the stored projection of every
// member, the moved ones included (heldProjections ignoring its kept run): the
// Area removed from circle.go stays in its stale projection, so Circle still
// satisfies Shape and total's call keeps dispatching to it, which the full
// build does not.
func TestLayoutBreakHierarchyEqualsFullBuild(t *testing.T) {
	s := newSession(t)
	files := shapesBase
	s.check("base", []linkBuild{s.only(s.advance(files))})
	for _, e := range []edit{
		{name: "method moved to a new file",
			replace: []sub{{"square.go", "\nfunc (Square) Area() int { return 4 }\n", ""}},
			write:   map[string]string{"area.go": "package shapes\n\nfunc (Square) Area() int { return 4 }\n"}},
		{name: "type added",
			write: map[string]string{"circle.go": "package shapes\n\ntype Circle struct{}\n\nfunc (Circle) Area() int { return 3 }\n"}},
		{name: "method removed",
			replace: []sub{{"circle.go", "\nfunc (Circle) Area() int { return 3 }\n", ""}}},
		{name: "interface method added",
			replace: []sub{{"shape.go", "type Shape interface{ Area() int }", "type Shape interface {\n\tArea() int\n\tSide() int\n}"}}},
		{name: "embedding changed",
			replace: []sub{{"square.go", "type Square struct{ Base }", "type Square struct{ Other }"}}},
		{name: "type removed", remove: []string{"circle.go"}},
	} {
		files = e.apply(t, files)
		b := s.only(s.advance(files))
		if b.res.Full {
			t.Fatalf("%s: the refresh was a full build: %+v", e.name, b.res)
		}
		s.check(e.name, []linkBuild{b})
	}
}
