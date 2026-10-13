package worker

import ts "github.com/tree-sitter/go-tree-sitter"

// rustConstructor is wire.Decl.Constructor of the Rust declaration node n:
// for a struct item or enum variant, "unit" when it has no body, "tuple"
// when its body is an ordered field list; empty otherwise (a body of named
// fields, any other item).
func rustConstructor(n ts.Node) string {
	switch n.Kind() {
	case "struct_item", "enum_variant":
	default:
		return ""
	}
	body := n.ChildByFieldName("body")
	switch {
	case body == nil:
		return "unit"
	case body.Kind() == "ordered_field_declaration_list":
		return "tuple"
	}
	return ""
}
