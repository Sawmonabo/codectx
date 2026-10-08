package wire

import (
	"encoding/binary"
	"errors"
	"fmt"
	"unicode/utf8"
)

// Link is one file's link tables: what the package-scoped linking provider
// needs of the file to bind its call sites to declarations in other files,
// computed by the worker in the same walk that extracts and analyses the
// file. It is sent as one KindLink message after the file's function
// messages and before KindDone, whenever the dependence pass ran; a file
// whose pass failed outside every function sends none.
//
// Every entry is keyed by a byte range the parent joins to a record it
// already holds: an Export by its declaration's name token (Decl.NameStart,
// Decl.NameEnd), a Site by its callee token (the "call" Ref's NameStart,
// NameEnd). The parent validates every range against the pinned bytes and
// refuses an entry it cannot join. Names and types are spelled as the
// source writes them; the worker resolves nothing outside the file.
type Link struct {
	Exports    []Export
	Supertypes []Supertype
	Sites      []Site
	Values     []Value
}

// Export describes one declaration of the file that a call may bind to, with
// the language's type facts the linker reads. A declaration the worker sends
// no Export for is still a declaration (Decl); it has no type facts.
type Export struct {
	// Name is the declaration's name token.
	Name Span
	// Receiver is the type a method is declared on, as written: a Go
	// receiver's base type, a Rust impl's self type, the class, struct,
	// interface or trait enclosing a Java, C++, ECMAScript, Python or Rust
	// method, the qualifier of a C++ out-of-class definition. It is empty for
	// a free function and for a type.
	Receiver string
	// Signature is the callable's parameter and result types as written,
	// with parameter names, whitespace and comments removed, as
	// "(" types joined by "," ")" then the results the same way: a Go
	// `func(a int, b string) error` is "(int,string)(error)". A method's
	// receiver is not a parameter. It is empty when the language declares no
	// types. Site.Signature is spelled by the same rule, so equal strings are
	// the address-taken × indirect-site join key (ADR-0012 decision 2).
	Signature string
	// Params is the number of declared parameters, Variadic that the last
	// one takes any number of arguments.
	Params   uint32
	Variadic bool
	// Static reports a callable invoked without a receiver value: a static
	// or class method, an associated function, a free function.
	Static bool
	// Abstract reports a callable declared without a body that a subtype
	// supplies: an interface or trait method, an abstract method, a pure
	// virtual function.
	Abstract bool
	// Returns is the type the callable returns: its declared result type,
	// or the type the worker recovered from its return statements inside the
	// file. TypeUnknown when neither says.
	Returns Type
}

// Supertype is one declared subtype relation of the file: Type extends,
// implements or embeds Super, or a Rust `impl Super for Type`. Both are
// spelled as written. Go's implicit interface satisfaction is not declared
// and is never sent; the linker derives it from method sets.
type Supertype struct {
	Type, Super string
}

// Site is one call site: a "call" Ref of the file, with the callee
// expression's shape and what the worker recovered about its receiver and
// arguments inside the file.
type Site struct {
	// Name is the callee token, the call Ref's NameStart and NameEnd.
	Name  Span
	Shape Shape
	// Qualifier is the type, package, namespace or module a ShapeStatic site
	// names, as written (`T` in `T::f()`, `pkg` in `pkg.F()`), and empty for
	// every other shape.
	Qualifier string
	// Receiver is the receiver's type for a ShapeMember site, and the called
	// value's type for a ShapeIndirect site when it is recovered.
	Receiver Type
	// Signature is a ShapeIndirect site's called value's declared function
	// type, spelled as Export.Signature, or empty when it is not declared.
	Signature string
	// Args has one entry per argument, in order, each the argument's type as
	// the worker recovered it, TypeUnknown where it recovered nothing.
	Args []Type
}

// Value is one value stored where a call can later reach it without naming
// its declaration: a function or method taken as a value (address-taken, a
// method value, a function passed or assigned), or any value written to a
// field or property, which field-based resolution reads.
type Value struct {
	// At is the value expression.
	At Span
	// Field is the field or property the value is written to, as written,
	// and empty for a function taken as a value that no field receives.
	Field string
	// Type is the value: TypeFunction for a function taken as a value,
	// otherwise the type recovered for what was written.
	Type Type
}

// Shape is a call site's callee expression form.
type Shape byte

const (
	// ShapeBare is a call of a name: `f()`.
	ShapeBare Shape = 1
	// ShapeMember is a call of a member on a receiver value: `x.f()`,
	// `x->f()`, `self.f()`, `this.f()`, `super.f()`.
	ShapeMember Shape = 2
	// ShapeStatic is a call through a type, package, namespace or module
	// name: `T::f()`, `pkg.F()`, `Class.m()` of a static method, `mod.f()`.
	ShapeStatic Shape = 3
	// ShapeIndirect is a call through a value that names no declaration: a
	// function pointer, a function-typed local, parameter or field, the
	// result of an expression.
	ShapeIndirect Shape = 4
	// ShapeConstructor is a creation through a constructor: `new C()`. A
	// call that only the linker can tell creates (a Python `C()`) is
	// ShapeBare.
	ShapeConstructor Shape = 5
)

// TypeKind says what a Type's Name and Index mean.
type TypeKind byte

const (
	// TypeUnknown carries nothing: the worker recovered no type.
	TypeUnknown TypeKind = 0
	// TypeNamed is an instance of the type Name, as written.
	TypeNamed TypeKind = 1
	// TypeParam is the value of the enclosing callable's parameter Index,
	// zero-based, whatever its callers pass.
	TypeParam TypeKind = 2
	// TypeResult is the value the site Index of this file's Sites returns.
	TypeResult TypeKind = 3
	// TypeModule is the module, package or namespace Name, as imported.
	TypeModule TypeKind = 4
	// TypeSuper is the supertypes of the type Name, the class enclosing a
	// `super` receiver.
	TypeSuper TypeKind = 5
	// TypeField is the value of any field or property Name: what the file's
	// and the package's Values write to that field.
	TypeField TypeKind = 6
	// TypeFunction is the function or method Name, as written, taken as a
	// value.
	TypeFunction TypeKind = 7
)

// Type is a value's type as the worker recovered it inside the file: a name
// as written, or a reference the linker resolves across functions and files
// (a parameter, another site's result, a field).
type Type struct {
	Kind  TypeKind
	Name  string
	Index uint32
}

// AppendLink appends l's encoding to dst. Integers are big-endian uint32, a
// string is its length and UTF-8 bytes, a flag is one byte 0 or 1, and every
// list is its length followed by its elements:
//
//	exports    n × (name start, end; receiver; signature; params;
//	                variadic, static, abstract flags; returns type)
//	supertypes n × (type; super)
//	sites      n × (name start, end; shape byte; qualifier; receiver type;
//	                signature; args n × type)
//	values     n × (at start, end; field; type)
//	type       kind byte; name; index
func AppendLink(dst []byte, l *Link) []byte {
	e := encoder{b: dst}
	e.u32(uint32(len(l.Exports)))
	for _, x := range l.Exports {
		e.span(x.Name)
		e.str(x.Receiver)
		e.str(x.Signature)
		e.u32(x.Params)
		e.flag(x.Variadic)
		e.flag(x.Static)
		e.flag(x.Abstract)
		e.typ(x.Returns)
	}
	e.u32(uint32(len(l.Supertypes)))
	for _, s := range l.Supertypes {
		e.str(s.Type)
		e.str(s.Super)
	}
	e.u32(uint32(len(l.Sites)))
	for _, s := range l.Sites {
		e.span(s.Name)
		e.b = append(e.b, byte(s.Shape))
		e.str(s.Qualifier)
		e.typ(s.Receiver)
		e.str(s.Signature)
		e.u32(uint32(len(s.Args)))
		for _, a := range s.Args {
			e.typ(a)
		}
	}
	e.u32(uint32(len(l.Values)))
	for _, v := range l.Values {
		e.span(v.At)
		e.str(v.Field)
		e.typ(v.Type)
	}
	return e.b
}

// encoder appends the link encoding's primitives.
type encoder struct{ b []byte }

func (e *encoder) u32(v uint32) { e.b = binary.BigEndian.AppendUint32(e.b, v) }
func (e *encoder) span(s Span)  { e.u32(s.Start); e.u32(s.End) }
func (e *encoder) str(s string) { e.u32(uint32(len(s))); e.b = append(e.b, s...) }
func (e *encoder) flag(v bool) {
	b := byte(0)
	if v {
		b = 1
	}
	e.b = append(e.b, b)
}
func (e *encoder) typ(t Type) { e.b = append(e.b, byte(t.Kind)); e.str(t.Name); e.u32(t.Index) }

// ErrMalformedLink reports a link message no writer of this protocol
// produces: a short or overlong payload, an unknown shape, type kind or flag
// byte, or a string that is not UTF-8.
var ErrMalformedLink = errors.New("wire: malformed link message")

// DecodeLink decodes one KindLink payload. Every list's length is checked
// against the bytes that remain before anything is allocated for it. Ranges
// and indices are not checked against the source or the file's other
// records here; the parent does that.
func DecodeLink(payload []byte) (Link, error) {
	d := decoder{b: payload, malformed: ErrMalformedLink}
	var l Link
	// The smallest export is 8 span, 4+4 strings, 4 params, 3 flags and a
	// 9-byte type; a supertype two strings; a site 8 span, 1 shape, 4
	// qualifier, 9 type, 4 signature and 4 args; a value 8 span, 4 field and
	// 9 type.
	l.Exports = make([]Export, 0, d.count(36))
	for range cap(l.Exports) {
		x := Export{Name: d.span(), Receiver: d.str(), Signature: d.str(), Params: d.u32()}
		x.Variadic, x.Static, x.Abstract = d.flag(), d.flag(), d.flag()
		x.Returns = d.typ()
		l.Exports = append(l.Exports, x)
	}
	l.Supertypes = make([]Supertype, 0, d.count(8))
	for range cap(l.Supertypes) {
		l.Supertypes = append(l.Supertypes, Supertype{Type: d.str(), Super: d.str()})
	}
	l.Sites = make([]Site, 0, d.count(30))
	for range cap(l.Sites) {
		s := Site{Name: d.span(), Shape: d.shape(), Qualifier: d.str(), Receiver: d.typ(), Signature: d.str()}
		s.Args = make([]Type, 0, d.count(9))
		for range cap(s.Args) {
			s.Args = append(s.Args, d.typ())
		}
		l.Sites = append(l.Sites, s)
	}
	l.Values = make([]Value, 0, d.count(21))
	for range cap(l.Values) {
		l.Values = append(l.Values, Value{At: d.span(), Field: d.str(), Type: d.typ()})
	}
	if d.err == nil && len(d.b) != 0 {
		d.fail(fmt.Sprintf("%d bytes past the message's end", len(d.b)))
	}
	if d.err != nil {
		return Link{}, d.err
	}
	return l, nil
}

func (d *decoder) span() Span { return Span{d.u32(), d.u32()} }

func (d *decoder) str() string {
	s := string(d.take(d.count(1)))
	if d.err == nil && !utf8.ValidString(s) {
		d.fail("a string that is not UTF-8")
	}
	return s
}

func (d *decoder) flag() bool {
	switch d.u8() {
	case 0:
		return false
	case 1:
		return true
	}
	d.fail("a flag that is neither 0 nor 1")
	return false
}

func (d *decoder) shape() Shape {
	s := Shape(d.u8())
	if d.err == nil && (s < ShapeBare || s > ShapeConstructor) {
		d.fail("an unknown call shape")
	}
	return s
}

func (d *decoder) typ() Type {
	k := TypeKind(d.u8())
	if d.err == nil && k > TypeFunction {
		d.fail("an unknown type kind")
	}
	return Type{Kind: k, Name: d.str(), Index: d.u32()}
}
