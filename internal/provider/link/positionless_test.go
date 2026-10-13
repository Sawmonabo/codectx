package link

import (
	"reflect"
	"testing"

	"github.com/Sawmonabo/codectx/internal/model"
)

// TestPositionlessClearsEveryPosition fills every field of a Table, walking
// its type so that a field added later is filled too: one element in each
// slice, each range and name range set, each canonical key and callee
// offset non-zero. positionless must clear every range, name range,
// canonical key and callee offset of the copy, and leave the table it
// copies untouched.
//
// Mutations that fail it: a range field positionless leaves set (a new
// field of the table with a range in it included); positionless writing
// through to the table it copies (the original loses a range).
func TestPositionlessClearsEveryPosition(t *testing.T) {
	var tab Table
	fill(reflect.ValueOf(&tab).Elem())
	tab.Language = "go"
	p := tab.positionless()
	for _, at := range positions(reflect.ValueOf(p).Elem(), "Table") {
		t.Errorf("positionless left a position at %s", at)
	}
	if got := len(positions(reflect.ValueOf(&tab).Elem(), "Table")); got == 0 {
		t.Error("positionless cleared the table it copies")
	}
}

var rangeType = reflect.TypeFor[model.SourceRange]()

// fill sets every field v reaches: each slice to one element, each pointer
// to a value, each range to a non-zero one, each string, number and flag to
// a non-zero value.
func fill(v reflect.Value) {
	switch {
	case v.Type() == rangeType:
		v.Set(reflect.ValueOf(model.SourceRange{Start: model.Position{Byte: 3, Line: 1, Column: 2},
			End: model.Position{Byte: 9, Line: 1, Column: 8}}))
		return
	}
	switch v.Kind() {
	case reflect.Struct:
		for i := 0; i < v.NumField(); i++ {
			if v.Type().Field(i).IsExported() {
				fill(v.Field(i))
			}
		}
	case reflect.Slice:
		s := reflect.MakeSlice(v.Type(), 1, 1)
		fill(s.Index(0))
		v.Set(s)
	case reflect.Pointer:
		p := reflect.New(v.Type().Elem())
		fill(p.Elem())
		v.Set(p)
	case reflect.String:
		v.SetString("x")
	case reflect.Bool:
		v.SetBool(true)
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		v.SetInt(1)
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		v.SetUint(1)
	}
}

// positions is the path of every byte position v still holds: a non-zero
// range, a set name range, a non-empty canonical key or callee offset.
func positions(v reflect.Value, at string) []string {
	if v.Type() == rangeType {
		if !v.IsZero() {
			return []string{at}
		}
		return nil
	}
	var out []string
	switch v.Kind() {
	case reflect.Struct:
		for i := 0; i < v.NumField(); i++ {
			f := v.Type().Field(i)
			if !f.IsExported() {
				continue
			}
			switch f.Name {
			case "NameRange", "CanonicalKey", "NameStart", "NameEnd":
				if !v.Field(i).IsZero() {
					out = append(out, at+"."+f.Name)
				}
				continue
			}
			out = append(out, positions(v.Field(i), at+"."+f.Name)...)
		}
	case reflect.Slice:
		for i := 0; i < v.Len(); i++ {
			out = append(out, positions(v.Index(i), at+"[]")...)
		}
	case reflect.Pointer:
		if !v.IsNil() {
			out = append(out, positions(v.Elem(), at)...)
		}
	}
	return out
}

// TestJavaAnonymousNamesHoldAcrossMoves reads two Java tables whose
// anonymous bodies are the same but sit at other offsets: one body with a
// declaration (an enum constant) and two without. Each body must resolve to
// the same Ref in both (anonymousType), and both tables must have one
// positionless form.
//
// Mutations that fail it: number a synthetic type by its offset (the two
// bodies without a declaration get other Refs); keep the offset in the
// positionless names (the forms differ).
func TestJavaAnonymousNamesHoldAcrossMoves(t *testing.T) {
	table := func(constant, a, b string, at uint64) *Table {
		return &Table{Path: "A.java", Language: "java", Decls: []Decl{
			{Node: "c", Kind: model.NodeConstant, Name: "PLUS",
				NameRange: &model.SourceRange{Start: model.Position{Byte: at}}, Parent: -1},
			{Node: "m1", Kind: model.NodeMethod, Name: "apply", Parent: -1, Receiver: constant},
			{Node: "m2", Kind: model.NodeMethod, Name: "run", Parent: -1, Receiver: a},
			{Node: "m3", Kind: model.NodeMethod, Name: "run", Parent: -1, Receiver: b},
		}}
	}
	before := table("#40", "#100", "#200", 40)
	after := table("#41", "#150", "#260", 41)
	fb, fa := contextOf(0, before), contextOf(0, after)
	for i := 1; i < 4; i++ {
		rb, okb := anonymousType(fb, before.Decls[i].Receiver)
		ra, oka := anonymousType(fa, after.Decls[i].Receiver)
		if !okb || !oka || rb != ra {
			t.Errorf("the body of %s is %v before the move and %v after it", before.Decls[i].Name, rb, ra)
		}
	}
	if rb, _ := anonymousType(fb, "#100"); rb == (Ref{File: 0, Index: 0}) || rb.Index >= -1 {
		t.Errorf("a body with no declaration is %v, want a synthetic type below the module", rb)
	}
	pb, pa := before.positionless(), after.positionless()
	if !reflect.DeepEqual(pb.Decls, pa.Decls) {
		t.Errorf("the positionless declarations differ after the move:\n%+v\n%+v", pb.Decls, pa.Decls)
	}
}
