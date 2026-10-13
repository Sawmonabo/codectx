package worker

import (
	ts "github.com/tree-sitter/go-tree-sitter"

	"github.com/Sawmonabo/codectx/internal/provider/treesitter/wire"
)

// parameterList is the formal parameter list of a declaration: its own
// `parameters` field, or that of the function its binding or assignment
// holds (`const f = (x) => …`).
func parameterList(s *decoSyntax, n ts.Node) *ts.Node {
	if p := n.ChildByFieldId(s.fParameters); p != nil {
		return p
	}
	for _, f := range []uint16{s.fValue, s.fRight} {
		if f == 0 {
			continue
		}
		if v := n.ChildByFieldId(f); v != nil {
			if p := v.ChildByFieldId(s.fParameters); p != nil {
				return p
			}
		}
	}
	return nil
}

// tsParameterDecorators are the decorators of a TypeScript declaration's
// parameters (`m(@Body() b: B)`, `constructor(@Inject(X) private x: X)`),
// each with the parameter's pattern, its name token when it is a name.
func tsParameterDecorators(e *extraction, d *decl) []wire.ParameterDecorator {
	s := e.g.decoSyntax(e.l.Name)
	list := parameterList(s, d.node)
	if list == nil || list.KindId() != s.formalParameters {
		return nil
	}
	var out []wire.ParameterDecorator
	for _, p := range entries(list) {
		if p.KindId() != s.requiredParameter && p.KindId() != s.optionalParameter {
			continue
		}
		pat := p.ChildByFieldId(s.fPattern)
		if pat == nil {
			continue
		}
		at := wire.Span{Start: uint32(pat.StartByte()), End: uint32(pat.EndByte())}
		for _, c := range entries(&p) {
			if c.KindId() != s.decorator {
				continue
			}
			for _, dec := range esDecoration(e, s, c) {
				out = append(out, wire.ParameterDecorator{Parameter: at, Decorator: dec})
			}
		}
	}
	return out
}

// javaParameterDecorators are the annotations in the modifiers of a Java
// method's or constructor's formal parameters (`@RequestParam("q") String
// q`), each with the parameter's name token.
func javaParameterDecorators(e *extraction, d *decl) []wire.ParameterDecorator {
	s := e.g.decoSyntax(e.l.Name)
	list := d.node.ChildByFieldId(s.fParameters)
	if list == nil || list.KindId() != s.formalParameters {
		return nil
	}
	var out []wire.ParameterDecorator
	for _, p := range entries(list) {
		if p.KindId() != s.formalParameter && p.KindId() != s.spreadParameter {
			continue
		}
		name := javaParameterName(s, p)
		m := childOfKind(p, s.modifiers)
		if name == nil || m == nil {
			continue
		}
		at := wire.Span{Start: uint32(name.StartByte()), End: uint32(name.EndByte())}
		for _, a := range entries(m) {
			if dec, ok := javaAnnotation(e, s, a); ok {
				out = append(out, wire.ParameterDecorator{Parameter: at, Decorator: dec})
			}
		}
	}
	return out
}

// javaParameterName is a formal parameter's name token: its `name` field,
// or for a variable-arity parameter its declarator's.
func javaParameterName(s *decoSyntax, p ts.Node) *ts.Node {
	if n := p.ChildByFieldId(s.fName); n != nil {
		return n
	}
	for _, c := range entries(&p) {
		if n := c.ChildByFieldId(s.fName); n != nil {
			return n
		}
	}
	return nil
}

// javaTypeAnnotations are the type-use annotations of a Java declaration's
// signature (JLS §9.7.4): every annotation written inside the type of a
// method's result, a field or a variable, or inside a formal parameter's
// type, in source order. The annotations in a modifier list are the
// declaration's or the parameter's own and are read there.
func javaTypeAnnotations(e *extraction, d *decl) []wire.Decorator {
	s := e.g.decoSyntax(e.l.Name)
	var types []ts.Node
	if t := d.node.ChildByFieldId(s.fType); t != nil {
		types = append(types, *t)
	}
	if list := d.node.ChildByFieldId(s.fParameters); list != nil && list.KindId() == s.formalParameters {
		for _, p := range entries(list) {
			for _, c := range entries(&p) {
				if c.KindId() != s.modifiers && c.KindId() != s.annotation && c.KindId() != s.markerAnnotation {
					types = append(types, c)
				}
			}
		}
	}
	var out []wire.Decorator
	for _, t := range types {
		out = append(out, javaAnnotationsIn(e, s, t)...)
	}
	return out
}

// javaAnnotationsIn is every annotation inside n, n itself included, in
// source order; an annotation's own arguments are not searched.
func javaAnnotationsIn(e *extraction, s *decoSyntax, n ts.Node) []wire.Decorator {
	if dec, ok := javaAnnotation(e, s, n); ok {
		return []wire.Decorator{dec}
	}
	var out []wire.Decorator
	for i := uint(0); i < n.NamedChildCount(); i++ {
		if c := n.NamedChild(i); c != nil {
			out = append(out, javaAnnotationsIn(e, s, *c)...)
		}
	}
	return out
}

// moduleDecorators are the attributes a file applies to itself: a Rust
// file's inner attributes at its top level.
func moduleDecorators(e *extraction, root ts.Node) []wire.Decorator {
	if e.l.Name != "rust" {
		return nil
	}
	s := e.g.decoSyntax(e.l.Name)
	var out []wire.Decorator
	for _, item := range innerAttributes(s, root) {
		if dec, ok := rustAttribute(e, s, item); ok {
			out = append(out, dec)
		}
	}
	return out
}
