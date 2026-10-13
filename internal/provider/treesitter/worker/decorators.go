package worker

import (
	"slices"

	ts "github.com/tree-sitter/go-tree-sitter"

	"github.com/Sawmonabo/codectx/internal/provider/treesitter/wire"
)

// decoSyntax holds the kind and field ids a grammar's decorators,
// annotations and attributes are read by, resolved once per grammar. Each
// language reads only its own entries; an entry another language's grammar
// lacks is 0, which no node has.
type decoSyntax struct {
	// Python (argumentList is shared with C's name).
	decoratedDefinition, decorator, call uint16
	// Java.
	modifiers, annotation, markerAnnotation, annotationArguments uint16
	// ECMAScript (decorator is shared with Python's name).
	exportStatement, classBody, callExpression, arguments, classExpr uint16
	// Rust.
	attributeItem, attribute, tokenTree uint16
	// C and C++ (attribute and callExpression are shared).
	attributeSpecifier, attributeDeclaration, argumentList, functionDeclarator, attributedDeclarator uint16
	typeQualifier, alignasQualifier                                                                  uint16
	// Parameters: TypeScript's, Java's; Rust's inner attributes.
	formalParameters, requiredParameter, optionalParameter, formalParameter, spreadParameter uint16
	innerAttributeItem                                                                       uint16
	// Fields.
	fFunction, fArguments, fName, fValue, fDeclarator, fParameters, fPattern, fType, fBody, fRight uint16
}

// decoSyntax is the decorator table of g, the grammar registered as
// language, resolved once. A kind or field the language reads that its
// grammar does not define panics, so a misspelt name can never silently read
// no decorator.
func (g *grammar) decoSyntax(language string) *decoSyntax {
	g.decoOnce.Do(func() {
		tl := g.tsLanguage()
		kind := func(name string) uint16 { return mustKind(tl, language, name, true) }
		field := func(name string) uint16 {
			v := tl.FieldIdForName(name)
			if v == 0 {
				panic("worker: the " + language + " grammar has no field " + name)
			}
			return v
		}
		s := &g.deco
		switch language {
		case "python":
			s.decoratedDefinition, s.decorator, s.call = kind("decorated_definition"), kind("decorator"), kind("call")
			s.argumentList = kind("argument_list")
			s.fFunction, s.fArguments = field("function"), field("arguments")
		case "java":
			s.modifiers, s.annotation, s.markerAnnotation = kind("modifiers"), kind("annotation"), kind("marker_annotation")
			s.annotationArguments = kind("annotation_argument_list")
			s.formalParameters, s.formalParameter, s.spreadParameter = kind("formal_parameters"), kind("formal_parameter"), kind("spread_parameter")
			s.fName, s.fArguments, s.fParameters, s.fType = field("name"), field("arguments"), field("parameters"), field("type")
		case "javascript", "typescript", "tsx":
			s.decorator, s.exportStatement, s.classBody = kind("decorator"), kind("export_statement"), kind("class_body")
			s.callExpression, s.arguments = kind("call_expression"), kind("arguments")
			s.formalParameters, s.classExpr = kind("formal_parameters"), kind("class")
			s.fFunction, s.fArguments, s.fValue, s.fRight, s.fParameters = field("function"), field("arguments"), field("value"), field("right"), field("parameters")
			if language != "javascript" {
				// A parameter decorator is TypeScript's alone.
				s.requiredParameter, s.optionalParameter = kind("required_parameter"), kind("optional_parameter")
				s.fPattern = field("pattern")
			}
		case "rust":
			s.attributeItem, s.attribute, s.tokenTree = kind("attribute_item"), kind("attribute"), kind("token_tree")
			s.innerAttributeItem = kind("inner_attribute_item")
			s.fArguments, s.fValue, s.fBody = field("arguments"), field("value"), field("body")
		case "c", "cpp":
			s.attributeSpecifier, s.attributeDeclaration, s.attribute = kind("attribute_specifier"), kind("attribute_declaration"), kind("attribute")
			s.argumentList, s.callExpression = kind("argument_list"), kind("call_expression")
			s.functionDeclarator, s.attributedDeclarator = kind("function_declarator"), kind("attributed_declarator")
			s.typeQualifier, s.alignasQualifier = kind("type_qualifier"), kind("alignas_qualifier")
			s.fFunction, s.fArguments, s.fName, s.fDeclarator = field("function"), field("arguments"), field("name"), field("declarator")
		}
	})
	return &g.deco
}

// decoration builds one decorator: its name as written, whitespace runs
// folded to one space, the whole decorator's range and each argument's.
func decoration(src []byte, whole, name ts.Node, args []ts.Node) wire.Decorator {
	d := wire.Decorator{Name: collapseSpace(name.Utf8Text(src)), Start: uint32(whole.StartByte()), End: uint32(whole.EndByte())}
	for _, a := range args {
		d.Arguments = append(d.Arguments, wire.Span{Start: uint32(a.StartByte()), End: uint32(a.EndByte())})
	}
	return d
}

// entries is every named child of n that is not an extra (a comment), in
// source order: the arguments of an argument list. The walk is one cursor,
// linear in the list's length.
func entries(n *ts.Node) []ts.Node {
	if n == nil {
		return nil
	}
	cur := n.Walk()
	defer cur.Close()
	var out []ts.Node
	for ok := nextEntry(cur, true); ok; ok = nextEntry(cur, false) {
		out = append(out, *cur.Node())
	}
	return out
}

// callDecoration is a decorator whose expression is expr: a call names its
// callee and lists its arguments (a keyword argument's range includes its
// keyword), any other expression is the name as written with no arguments.
func callDecoration(src []byte, whole, expr ts.Node, call, list, fFunction, fArguments uint16) wire.Decorator {
	if expr.KindId() == call {
		if fn := expr.ChildByFieldId(fFunction); fn != nil {
			var args []ts.Node
			if a := expr.ChildByFieldId(fArguments); a != nil && a.KindId() == list {
				args = entries(a)
			}
			return decoration(src, whole, *fn, args)
		}
	}
	return decoration(src, whole, expr, nil)
}

// pyDecorators are the decorators of a function or class definition, the
// `decorator` children of the decorated_definition around it, in order. A
// decorator call whose argument is a bare generator expression
// (`@d(x for x in y)`) has no argument list and lists no arguments.
func pyDecorators(e *extraction, d *decl) []wire.Decorator {
	s := e.g.decoSyntax(e.l.Name)
	p := d.node.Parent()
	if p == nil || p.KindId() != s.decoratedDefinition {
		return nil
	}
	var out []wire.Decorator
	for _, c := range entries(p) {
		if c.KindId() != s.decorator {
			continue
		}
		if expr := entries(&c); len(expr) > 0 {
			out = append(out, callDecoration(e.src, c, expr[0], s.call, s.argumentList, s.fFunction, s.fArguments))
		}
	}
	return out
}

// javaDecorators are the annotations in a declaration's modifiers, marker
// and normal, in order: the name as written (`Override`, `org.junit.Test`)
// and each element of the argument list, an element-value pair's range
// including its key (`path = "/x"`).
func javaDecorators(e *extraction, d *decl) []wire.Decorator {
	s := e.g.decoSyntax(e.l.Name)
	m := childOfKind(d.node, s.modifiers)
	if m == nil {
		return nil
	}
	var out []wire.Decorator
	for _, a := range entries(m) {
		if dec, ok := javaAnnotation(e, s, a); ok {
			out = append(out, dec)
		}
	}
	return out
}

// javaAnnotation is one annotation node as a decorator, and false for any
// other node.
func javaAnnotation(e *extraction, s *decoSyntax, a ts.Node) (wire.Decorator, bool) {
	if a.KindId() != s.annotation && a.KindId() != s.markerAnnotation {
		return wire.Decorator{}, false
	}
	name := a.ChildByFieldId(s.fName)
	if name == nil {
		return wire.Decorator{}, false
	}
	var args []ts.Node
	if l := a.ChildByFieldId(s.fArguments); l != nil && l.KindId() == s.annotationArguments {
		args = entries(l)
	}
	return decoration(e.src, a, *name, args), true
}

// esDecorators are the decorators of an ECMAScript class or class member, in
// source order, wherever the grammar places them: on an export statement
// that wraps the class (`@d export class C {}`), as the declaration's own
// `decorator` children (a class, a JavaScript method or field, a TypeScript
// field), and, in TypeScript, as the `decorator` siblings that precede a
// method in the class body.
func esDecorators(e *extraction, d *decl) []wire.Decorator {
	s := e.g.decoSyntax(e.l.Name)
	var nodes []ts.Node
	if p := d.node.Parent(); p != nil {
		switch p.KindId() {
		case s.exportStatement:
			for _, c := range entries(p) {
				if c.KindId() == s.decorator && c.StartByte() < d.node.StartByte() {
					nodes = append(nodes, c)
				}
			}
		case s.classBody:
			var before []ts.Node
			for q := d.node.PrevNamedSibling(); q != nil; q = q.PrevNamedSibling() {
				if e.k.comments.has(q) {
					continue
				}
				if q.KindId() != s.decorator {
					break
				}
				before = append(before, *q)
			}
			slices.Reverse(before)
			nodes = append(nodes, before...)
		}
	}
	own := []ts.Node{d.node}
	// A class expression's decorators are on the class, the value the
	// declaration's binding or assignment holds (`const C = @d class {}`).
	for _, f := range []uint16{s.fValue, s.fRight} {
		if v := d.node.ChildByFieldId(f); v != nil && v.KindId() == s.classExpr {
			own = append(own, *v)
		}
	}
	for _, n := range own {
		for _, c := range entries(&n) {
			if c.KindId() == s.decorator {
				nodes = append(nodes, c)
			}
		}
	}
	var out []wire.Decorator
	for _, c := range nodes {
		out = append(out, esDecoration(e, s, c)...)
	}
	return out
}

// esDecoration is one ECMAScript decorator node as a decorator, or none
// when it holds no expression.
func esDecoration(e *extraction, s *decoSyntax, c ts.Node) []wire.Decorator {
	if expr := entries(&c); len(expr) > 0 {
		return []wire.Decorator{callDecoration(e.src, c, expr[0], s.callExpression, s.arguments, s.fFunction, s.fArguments)}
	}
	return nil
}

// rustDecorators are the outer attributes of an item, the `#[...]` items
// that precede it (comments between them skipped), in source order: the
// attribute's path as written (`derive`, `tokio::main`) and its arguments,
// the token tree split at its top-level commas, or the value of the
// `#[name = value]` form as its one argument, then the inner attributes
// (`#![...]`) its body opens with, which apply to it too.
func rustDecorators(e *extraction, d *decl) []wire.Decorator {
	s := e.g.decoSyntax(e.l.Name)
	var items []ts.Node
	for p := d.node.PrevNamedSibling(); p != nil; p = p.PrevNamedSibling() {
		if e.k.comments.has(p) {
			continue
		}
		if p.KindId() != s.attributeItem {
			break
		}
		items = append(items, *p)
	}
	slices.Reverse(items)
	// An inner attribute (`#![...]`) in the item's body applies to the
	// item, as an outer one does (the Rust Reference, Attributes).
	if body := d.node.ChildByFieldId(s.fBody); body != nil {
		items = append(items, innerAttributes(s, *body)...)
	}
	var out []wire.Decorator
	for _, item := range items {
		if dec, ok := rustAttribute(e, s, item); ok {
			out = append(out, dec)
		}
	}
	return out
}

// innerAttributes are the inner attribute items among n's direct children,
// in source order.
func innerAttributes(s *decoSyntax, n ts.Node) []ts.Node {
	var out []ts.Node
	for _, c := range entries(&n) {
		if c.KindId() == s.innerAttributeItem {
			out = append(out, c)
		}
	}
	return out
}

// rustAttribute is one attribute item, outer or inner, as a decorator: its
// path as written and its arguments.
func rustAttribute(e *extraction, s *decoSyntax, item ts.Node) (wire.Decorator, bool) {
	attr := childOfKind(item, s.attribute)
	if attr == nil {
		return wire.Decorator{}, false
	}
	path := attr.NamedChild(0)
	if path == nil {
		return wire.Decorator{}, false
	}
	dec := decoration(e.src, item, *path, nil)
	if tt := attr.ChildByFieldId(s.fArguments); tt != nil && tt.KindId() == s.tokenTree {
		dec.Arguments = splitTokenTree(tt, e.src)
	} else if v := attr.ChildByFieldId(s.fValue); v != nil {
		dec.Arguments = []wire.Span{{Start: uint32(v.StartByte()), End: uint32(v.EndByte())}}
	}
	return dec, true
}

// splitTokenTree is the arguments of an attribute's delimited token tree:
// the tokens between its delimiters, split at its own commas, each argument
// from its first token to its last. A comma inside a nested token tree is
// that tree's, so `cfg(all(a, b))` is one argument. Comments are skipped.
func splitTokenTree(tt *ts.Node, src []byte) []wire.Span {
	cur := tt.Walk()
	defer cur.Close()
	if !cur.GotoFirstChild() {
		return nil
	}
	var out []wire.Span
	var arg wire.Span
	open := false
	flush := func() {
		if open {
			out = append(out, arg)
		}
		open = false
	}
	// The first child is the opening delimiter.
	for cur.GotoNextSibling() {
		n := cur.Node()
		if n.IsExtra() {
			continue
		}
		start, end := uint32(n.StartByte()), uint32(n.EndByte())
		if !n.IsNamed() && end == start+1 {
			switch src[start] {
			case ',':
				flush()
				continue
			case ')', ']', '}':
				// The closing delimiter is the last child; a lone closing
				// token inside the tree is always its own token tree.
				if n.NextSibling() == nil {
					flush()
					return out
				}
			}
		}
		if !open {
			arg, open = wire.Span{Start: start}, true
		}
		arg.End = end
	}
	flush()
	return out
}

// cDecorators are the attributes of a C or C++ declaration where the grammar
// attaches them: the attribute specifiers among the declaration's own
// children, and those of the declarator chain down to its name (the
// trailing `__attribute__((...))` of a function declarator, the `[[...]]`
// of an attributed declarator), in source order. Each attribute is one
// decorator: `[[nodiscard, gnu::cold]]` is `nodiscard` and `gnu::cold`, and
// `__attribute__((noreturn, format(printf, 1, 2)))` is `noreturn` and
// `format` with its three arguments.
func cDecorators(e *extraction, d *decl) []wire.Decorator {
	s := e.g.decoSyntax(e.l.Name)
	specs := attributeChildren(d.node, s)
	for n := d.declarator; n != nil; {
		switch id := n.KindId(); {
		case id == s.functionDeclarator, id == s.attributedDeclarator:
			specs = append(specs, attributeChildren(*n, s)...)
		case !e.k.declaratorChain.has(n):
			n = nil
			continue
		}
		if c := n.ChildByFieldId(s.fDeclarator); c != nil {
			n = c
		} else {
			n = firstNamedChild(n)
		}
	}
	specs = append(specs, alignasQualifiers(d.node, s)...)
	slices.SortFunc(specs, func(a, b ts.Node) int { return cmpUint(a.StartByte(), b.StartByte()) })
	var out []wire.Decorator
	for _, spec := range specs {
		switch spec.KindId() {
		case s.alignasQualifier:
			// `alignas(16)` and `alignas(T)`: the specifier's keyword as
			// written (`alignas`, `_Alignas`) and its one operand.
			kw := spec.Child(0)
			if kw == nil {
				continue
			}
			dec := decoration(e.src, spec, *kw, nil)
			dec.Arguments = spans(entries(&spec))
			out = append(out, dec)
		case s.attributeDeclaration:
			for _, a := range entries(&spec) {
				if a.KindId() != s.attribute {
					continue
				}
				name := a.ChildByFieldId(s.fName)
				if name == nil {
					continue
				}
				dec := decoration(e.src, a, *name, nil)
				// The name as written runs from a prefix (`gnu::`) through
				// the name.
				dec.Name = collapseSpace(string(e.src[a.StartByte():name.EndByte()]))
				if l := childOfKind(a, s.argumentList); l != nil {
					dec.Arguments = spans(entries(l))
				}
				out = append(out, dec)
			}
		case s.attributeSpecifier:
			for _, a := range entries(childOfKind(spec, s.argumentList)) {
				out = append(out, callDecoration(e.src, a, a, s.callExpression, s.argumentList, s.fFunction, s.fArguments))
			}
		}
	}
	return out
}

// alignasQualifiers are the alignment specifiers among n's direct children,
// bare (`struct alignas(8) S`) or as a type qualifier (`alignas(16) int x;`).
func alignasQualifiers(n ts.Node, s *decoSyntax) []ts.Node {
	var out []ts.Node
	for _, c := range entries(&n) {
		switch c.KindId() {
		case s.alignasQualifier:
			out = append(out, c)
		case s.typeQualifier:
			if a := childOfKind(c, s.alignasQualifier); a != nil {
				out = append(out, *a)
			}
		}
	}
	return out
}

// attributeChildren is every attribute specifier and attribute declaration
// among n's direct children.
func attributeChildren(n ts.Node, s *decoSyntax) []ts.Node {
	var out []ts.Node
	for _, c := range entries(&n) {
		if c.KindId() == s.attributeSpecifier || c.KindId() == s.attributeDeclaration {
			out = append(out, c)
		}
	}
	return out
}

// spans is the range of each node.
func spans(nodes []ts.Node) []wire.Span {
	out := make([]wire.Span, 0, len(nodes))
	for _, n := range nodes {
		out = append(out, wire.Span{Start: uint32(n.StartByte()), End: uint32(n.EndByte())})
	}
	return out
}
