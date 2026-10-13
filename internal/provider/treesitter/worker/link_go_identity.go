package worker

import (
	"strings"
	"sync"

	"github.com/Sawmonabo/codectx/internal/provider/treesitter/wire"
)

// Go type identity forms (wire.Export.Identity).
//
// A method's signature as written (wire.Export.Signature) spells a type the
// way its file does: `store.ID` in one package is `ID` in another, an import
// may rename its package, and `byte` is `uint8`. Two signatures are the same
// only when every type in them is the same type ("Type identity"), so the
// method sets the linker compares are spelled by a second rule, the identity
// form, whose names the linker resolves to the packages that declare them.
// The worker resolves nothing outside the file: it marks each name by how it
// is written and leaves the package to the linker.
//
// A form is a sequence of tokens joined by single spaces:
//
//   - `.Name`: a type named without a qualifier (a declared type of the
//     file's package, a dot import's, or a predeclared type);
//   - `:pkg.Name`: a type qualified by the package name pkg;
//   - `$i`: the i-th type parameter of the generic type the method is
//     declared on (its receiver's type arguments, an interface's type
//     parameter list), counted from 0, so `func (s *Set[T]) Add(v T)` and
//     `interface[E any]{ Add(E) }` spell their parameter alike;
//   - the punctuation and keywords of a type literal: `*`, `[`, `]`, an
//     array's length when it is an integer literal, `map`, `chan`, `<-`,
//     `func`, `(`, `)`, `,`, `...`, and `struct{}` and `interface{}` for the
//     empty struct and the empty interface.
//
// A signature is `(` its parameters' types, one per name, joined by `,` `)`
// then the results the same way: `func(a, b int) error` is
// `( .int , .int ) ( .error )`. A type the form cannot spell exactly, a
// struct or interface literal with members or an array whose length is not
// an integer literal, makes the whole form "?": the linker then counts the
// method as one no other method matches, never as a guess.

// goIdentityUnknown is the form of a signature or type the form cannot spell
// exactly.
const goIdentityUnknown = "?"

// goIdentitySyntax holds the kind and field ids the identity forms read,
// resolved once by name against the pinned grammar; every field is read
// from each child's own record (goChild).
type goIdentitySyntax struct {
	typeAlias, sliceType, arrayType, mapType, channelType, intLiteral, typeArguments, typeElem,
	typeParameterList, typeParameterDeclaration, argumentList, nilLit, implicitLengthArrayType uint16

	fTypeParameters, fTypeArguments, fLength, fElement, fKey, fValue, fType, fName, fPackage, fOperand,
	fFunction, fArguments, fBody uint16
}

var (
	goIdentityOnce  sync.Once
	goIdentityTable *goIdentitySyntax
)

func goIdentitySyntaxOf() *goIdentitySyntax {
	goIdentityOnce.Do(func() {
		const language = "go"
		tl := mustGrammar(language)
		kind := func(name string) uint16 { return mustKind(tl, language, name, true) }
		field := func(name string) uint16 {
			v := tl.FieldIdForName(name)
			if v == 0 {
				panic("worker: the " + language + " grammar has no field " + name)
			}
			return v
		}
		s := &goIdentitySyntax{}
		s.typeAlias, s.sliceType, s.arrayType = kind("type_alias"), kind("slice_type"), kind("array_type")
		s.mapType, s.channelType, s.intLiteral = kind("map_type"), kind("channel_type"), kind("int_literal")
		s.typeArguments, s.typeElem = kind("type_arguments"), kind("type_elem")
		s.typeParameterList, s.typeParameterDeclaration = kind("type_parameter_list"), kind("type_parameter_declaration")
		s.argumentList, s.nilLit = kind("argument_list"), kind("nil")
		s.implicitLengthArrayType = kind("implicit_length_array_type")
		s.fTypeParameters, s.fTypeArguments = field("type_parameters"), field("type_arguments")
		s.fLength, s.fElement, s.fKey, s.fValue = field("length"), field("element"), field("key"), field("value")
		s.fType, s.fName, s.fPackage, s.fOperand = field("type"), field("name"), field("package"), field("operand")
		s.fFunction, s.fArguments, s.fBody = field("function"), field("arguments"), field("body")
		goIdentityTable = s
	})
	return goIdentityTable
}

// goForm builds one identity form.
type goForm struct {
	x  *goLink
	s  *goIdentitySyntax
	b  []string
	ok bool
	// params are the type parameters in scope, by name, each its position.
	params map[string]int
}

func (x *goLink) newForm(params map[string]int) *goForm {
	return &goForm{x: x, s: goIdentitySyntaxOf(), ok: true, params: params}
}

func (f *goForm) tok(t string) { f.b = append(f.b, t) }

// String is the form, or goIdentityUnknown when any part of it could not be
// spelled exactly.
func (f *goForm) String() string {
	if !f.ok {
		return goIdentityUnknown
	}
	return strings.Join(f.b, " ")
}

// signature appends the form of a signature with parameters params and
// result result.
func (f *goForm) signature(params, result Node) {
	f.list(params)
	result = f.x.unparen(result)
	switch {
	case result.IsNull():
		f.tok("(")
		f.tok(")")
	case result.KindId() == f.x.k.parameterList:
		f.list(result)
	default:
		f.tok("(")
		f.typ(result)
		f.tok(")")
	}
}

// list appends a parameter list's types, one per name, as goLink.params
// visits them.
func (f *goForm) list(l Node) {
	f.tok("(")
	first := true
	f.x.params(l, func(typ Node, variadic bool) {
		if !first {
			f.tok(",")
		}
		first = false
		if variadic {
			f.tok("...")
		}
		f.typ(typ)
	})
	f.tok(")")
}

// typ appends the form of the type t.
func (f *goForm) typ(t Node) {
	x, k, s := f.x, f.x.k, f.s
	t = x.unparen(t)
	if t.IsNull() {
		f.ok = false
		return
	}
	switch t.KindId() {
	case k.typeIdentifier, k.identifier:
		name := x.text(t)
		if i, ok := f.params[name]; ok {
			f.tok("$" + itoa(i))
			return
		}
		f.tok("." + name)
	case k.qualifiedType:
		pkg, name := goChild(t, s.fPackage), goChild(t, s.fName)
		if pkg.IsNull() || name.IsNull() {
			f.ok = false
			return
		}
		f.tok(":" + x.text(pkg) + "." + x.text(name))
	case k.pointerType:
		f.tok("*")
		f.typ(firstNamed(t))
	case s.sliceType:
		f.tok("[")
		f.tok("]")
		f.typ(goChild(t, s.fElement))
	case s.arrayType:
		n := goChild(t, s.fLength)
		if !x.is(n, s.intLiteral) {
			f.ok = false
			return
		}
		f.tok("[")
		f.tok(x.text(n))
		f.tok("]")
		f.typ(goChild(t, s.fElement))
	case s.mapType:
		f.tok("map")
		f.tok("[")
		f.typ(goChild(t, s.fKey))
		f.tok("]")
		f.typ(goChild(t, s.fValue))
	case s.channelType:
		// The direction is the anonymous tokens `chan` and `<-` in order.
		for c := t.first(); !c.IsNull(); c = c.next() {
			switch {
			case c.IsExtra():
			case c.IsNamed():
				f.typ(c)
			default:
				f.tok(x.text(c))
			}
		}
	case k.functionType:
		f.tok("func")
		f.signature(t.ChildByFieldId(k.fParameters), t.ChildByFieldId(k.fResult))
	case k.structType:
		if l := firstNamed(t); !l.IsNull() && !firstNamed(l).IsNull() {
			f.ok = false
			return
		}
		f.tok("struct{}")
	case k.interfaceType:
		if !firstNamed(t).IsNull() {
			f.ok = false
			return
		}
		f.tok("interface{}")
	case k.genericType:
		f.typ(goChild(t, s.fType))
		args := goChild(t, s.fTypeArguments)
		f.tok("[")
		first := true
		goNamed(args, func(e Node) {
			if !first {
				f.tok(",")
			}
			first = false
			var only Node
			n := 0
			goNamed(e, func(c Node) { only = c; n++ })
			if e.KindId() != s.typeElem || n != 1 {
				f.ok = false
				return
			}
			f.typ(only)
		})
		f.tok("]")
	default:
		f.ok = false
	}
}

// itoa is the decimal spelling of a small non-negative int.
func itoa(i int) string {
	if i < 10 {
		return string(rune('0' + i))
	}
	return itoa(i/10) + string(rune('0'+i%10))
}

// typeParams are the positions of the type parameters a type parameter
// list declares, by name; nil for a null list.
func (x *goLink) typeParams(l Node) map[string]int {
	if l.IsNull() {
		return nil
	}
	s := goIdentitySyntaxOf()
	out := map[string]int{}
	goNamed(l, func(d Node) {
		if d.KindId() != s.typeParameterDeclaration {
			return
		}
		for c := d.first(); !c.IsNull(); c = c.next() {
			if uint16(c.rec().field) == s.fName {
				out[x.text(c)] = len(out)
			}
		}
	})
	return out
}

// receiverParams are the type parameters a method's receiver names, by
// position: `(s *Set[K, V])` names K at 0 and V at 1. A receiver whose type
// argument is not a plain name declares none.
func (x *goLink) receiverParams(m Node) map[string]int {
	s := goIdentitySyntaxOf()
	r := m.ChildByFieldId(x.k.fReceiver)
	var out map[string]int
	goNamed(r, func(p Node) {
		t := x.unparen(p.ChildByFieldId(x.k.fType))
		if x.is(t, x.k.pointerType) {
			t = x.unparen(firstNamed(t))
		}
		if !x.is(t, x.k.genericType) {
			return
		}
		goNamed(goChild(t, s.fTypeArguments), func(e Node) {
			if id := firstNamed(e); x.is(id, x.k.typeIdentifier) {
				if out == nil {
					out = map[string]int{}
				}
				out[x.text(id)] = len(out)
			}
		})
	})
	return out
}

// methodIdentity is the identity form of the method or interface method n
// (a method declaration or a method element), with the type parameters in
// scope params.
func (x *goLink) methodIdentity(n Node, params map[string]int) string {
	f := x.newForm(params)
	f.signature(n.ChildByFieldId(x.k.fParameters), n.ChildByFieldId(x.k.fResult))
	return f.String()
}

// aliasExport appends the export of the alias declaration n (`type A = T`),
// its Identity "=" and the form of T: a type alias denotes T itself ("Alias
// declarations"), so the linker reads the alias's name as T wherever a form
// names it. A generic alias's form is "=?": an instantiation of it is a
// type the form cannot spell.
func (x *goLink) aliasExport(n Node) {
	s := goIdentitySyntaxOf()
	name := goChild(n, s.fName)
	if name.IsNull() || x.blankDecl(n, name) {
		return
	}
	form := goIdentityUnknown
	if goChild(n, s.fTypeParameters).IsNull() {
		f := x.newForm(nil)
		f.typ(goChild(n, s.fType))
		form = f.String()
	}
	x.out.Exports = append(x.out.Exports, wire.Export{Name: goSpan(name), Identity: "=" + form})
}

// conversions appends a wire.Conversion for each value of the var spec n
// whose type is declared and whose value's type the expression spells
// (convertedType): the program compiles only if the value is assignable to
// the declared type ("Assignability").
func (x *goLink) conversions(n Node) {
	// Only a type name declares an interface: `*I`, `[]I` and `I[T]` do not
	// convert to I.
	t := x.unparen(n.ChildByFieldId(x.k.fType))
	if !x.is(t, x.k.typeIdentifier) && !x.is(t, x.k.qualifiedType) {
		return
	}
	declared := x.baseName(t)
	for _, v := range x.list(n.ChildByFieldId(x.k.fValue)) {
		if typ := x.convertedType(v); typ != "" {
			x.out.Conversions = append(x.out.Conversions, wire.Conversion{Type: strings.Clone(typ), Interface: strings.Clone(declared)})
		}
	}
}

// convertedType is the named type T a value of the expression e is of, read
// from its syntax alone, as baseName spells it: `(*T)(nil)`, `&T{…}`,
// `T{…}` and `new(T)`; empty for every other expression, whose type is not
// written in it.
func (x *goLink) convertedType(e Node) string {
	k, s := x.k, goIdentitySyntaxOf()
	e = x.unparen(e)
	switch {
	case x.is(e, k.compositeLiteral):
		return x.baseName(goChild(e, s.fType))
	case x.is(e, k.unaryExpression):
		op := e.ChildByFieldId(k.fOperator)
		if op.IsNull() || op.KindId() != k.amp {
			return ""
		}
		if c := x.unparen(goChild(e, s.fOperand)); x.is(c, k.compositeLiteral) {
			return x.baseName(goChild(c, s.fType))
		}
	case x.is(e, k.callExpression):
		fn := goChild(e, s.fFunction)
		args := goChild(e, s.fArguments)
		var only Node
		n := 0
		goNamed(args, func(c Node) { only = c; n++ })
		if n != 1 {
			return ""
		}
		if x.is(fn, k.identifier) && x.text(fn) == "new" && !x.local("new") && x.funcs["new"].typ.Kind == wire.TypeUnknown {
			return x.baseName(only)
		}
		// `(*T)(nil)`: a conversion of nil to the pointer type *T, whose
		// operand the grammar reads as a unary expression.
		if !x.is(fn, k.parenExpression) || !x.is(only, s.nilLit) {
			return ""
		}
		u := firstNamed(fn)
		if !x.is(u, k.unaryExpression) {
			return ""
		}
		if op := u.ChildByFieldId(k.fOperator); op.IsNull() || op.KindId() != k.star {
			return ""
		}
		return x.baseName(goChild(u, s.fOperand))
	}
	return ""
}
