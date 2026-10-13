package gogrammar

import (
	"testing"

	ts "github.com/tree-sitter/go-tree-sitter"
)

// TestMethodTypeParametersAreTheirOwnField protects the grammar's contract
// for a generic method: its type parameter list is the method's
// type_parameters field, the field function declarations already use, and a
// method without one has no such field, so extraction reads both forms by
// field. Mutation: add the list to method_declaration without the field name
// (or under another), and Apply's type_parameters field is null.
func TestMethodTypeParametersAreTheirOwnField(t *testing.T) {
	const src = "package p\n\nfunc (l List[E]) Apply[F any](f func(E) F) List[F] { return nil }\n\nfunc (l List[E]) Len() int { return len(l) }\n"
	p := ts.NewParser()
	defer p.Close()
	if err := p.SetLanguage(ts.NewLanguage(Language())); err != nil {
		t.Fatal(err)
	}
	tree := p.Parse([]byte(src), nil)
	defer tree.Close()
	root := tree.RootNode()
	if root.HasError() {
		t.Fatalf("the generic method does not parse clean: %s", root.ToSexp())
	}
	want := map[string]string{"Apply": "[F any]", "Len": ""}
	got := map[string]string{}
	for i := range root.NamedChildCount() {
		m := root.NamedChild(i)
		if m.Kind() != "method_declaration" {
			continue
		}
		name := m.ChildByFieldName("name").Utf8Text([]byte(src))
		got[name] = ""
		if tp := m.ChildByFieldName("type_parameters"); tp != nil {
			got[name] = tp.Utf8Text([]byte(src))
		}
	}
	if len(got) != len(want) {
		t.Fatalf("methods %v, want %v", got, want)
	}
	for name, tp := range want {
		if g, ok := got[name]; !ok || g != tp {
			t.Fatalf("method %s has type parameters %q (declared %v), want %q", name, g, ok, tp)
		}
	}
}
