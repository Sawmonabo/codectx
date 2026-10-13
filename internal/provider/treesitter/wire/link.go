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
	Reexports  []Reexport
	// Unions are the type sets a TypeUnion names by index: each is every
	// type that reaches one value along some path (two reaching definitions
	// of a receiver that disagree), none of them a TypeUnion.
	Unions [][]Type
	// Parameters are the declared parameters of the file's callables, each
	// keyed by its callable's name token, in declaration order within each
	// callable. An argument at a bound call site flows to the parameter its
	// position, keyword and spread match.
	Parameters []Parameter
	// DynamicImports are the file's imports whose path is an expression:
	// each with the path's literal fragments, or, when the expression folds
	// to a constant, that path (DynamicImport.Constant).
	DynamicImports []DynamicImport
	// Computed are the file's calls whose callee is an expression that names
	// nothing (`obj[k]()`, `f()()`, `(a || b)()`), which no call reference
	// names: each a Site whose Name is the callee expression, Shape
	// ShapeIndirect, with its Arguments. Only the ECMAScript family and
	// Python send them.
	Computed []Site
}

// Export describes one declaration of the file that a call may bind to, a
// type whose linkage decides which files' names reach it, or a Go struct
// field whose declared type types a receiver that reads it, with the
// language's type facts the linker reads. A declaration the worker sends
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
	// receiver is not a parameter, except a Rust method's self parameter,
	// spelled first as the type it declares (`&self` of Foo is "&Foo"):
	// Rust takes a method as a value only through its path, `Foo::m`, whose
	// function type takes the receiver first. It is empty when the language declares no
	// types. Site.Signature is spelled by the same rule, so equal strings are
	// the address-taken × indirect-site join key (ADR-0012 decision 2).
	Signature string
	// Params is the number of declared parameters, Variadic that the last
	// one takes any number of arguments, Optional how many trailing
	// parameters a call may leave out (a C++ default argument, a Python or
	// ECMAScript default value, a TypeScript optional parameter). A call fits
	// the callable when it passes at least Params-Optional arguments and, unless
	// Variadic, at most Params.
	Params   uint32
	Optional uint32
	Variadic bool
	// Static reports a callable invoked without a receiver value: a static
	// or class method, an associated function, a free function.
	Static bool
	// Abstract reports a callable declared without a body that a subtype
	// supplies: an interface or trait method, an abstract method, a pure
	// virtual function.
	Abstract bool
	// Virtual reports a C++ member function declared `virtual`, or with an
	// `override` or `final` virt-specifier. A C++ call through a receiver
	// dispatches into subtypes only from a member that is virtual or
	// overrides a virtual one ([class.virtual]).
	Virtual bool
	// Final reports a method no subtype can override: a Java `final` or
	// `private` method or a method of a `final` class, a C++ member declared
	// `final` or of a `final` class. A call of it never dispatches into a
	// subtype.
	Final bool
	// Internal reports a declaration with internal linkage: a C or C++
	// `static` function at namespace scope, or a function, class, struct,
	// union or enum declared in a C++ unnamed namespace. Only its own file
	// names it.
	Internal bool
	// Returns is the type the callable returns: its declared result type,
	// or the type the worker recovered from its return statements inside the
	// file. TypeUnknown when neither says. For a Go or Java field it is the
	// field's declared type, reduced as a Go receiver is (`*T` is T) and, in
	// Java, without its type arguments (`List<T>` is List). A C++ class
	// type returned by value is marked ExactObject.
	Returns Type
	// Override reports a method the language marks as overriding or
	// implementing a supertype's: Java `@Override`, a C++ `override` or
	// `final` virt-specifier, a TypeScript `override` modifier, a Python
	// method decorated `typing.override` (`@override` imported from typing
	// or typing_extensions, or written qualified), any method of a Rust
	// `impl Trait for Type` block.
	Override bool
	// Alias is, for a C or C++ object-like macro whose replacement list is
	// one identifier (`#define LOG log_impl`), that identifier; empty for
	// every other declaration.
	Alias string
	// Property reports an ECMAScript function stored under a property path
	// rather than bound to a name (`a.b = () => …`, `{ k: () => … }`): a
	// bare name in its scope never denotes it.
	Property bool
	// Exported reports a declaration its module exports under its own name
	// that no `export` statement marks: an ECMAScript function stored as a
	// CommonJS export (`module.exports.run = …`, `exports.run = …`, a key of
	// `module.exports = { run: … }`).
	Exported bool
	// Visibility is, for a C or C++ object-like macro, the symbol visibility
	// its replacement list states (one of the Visibility values); empty when the replacement
	// is no specifier sequence or is one identifier (Alias).
	Visibility string
	// Macros are, for a C or C++ declaration written with macros among its
	// specifiers (`SQLITE_API int f(void);`), their names in source order,
	// then the identifiers that may be its type or a macro when the file
	// alone does not decide (Decl.ReturnTypes), each followed by one space:
	// an identifier holds none.
	Macros string
}

// Supertype is one declared subtype relation of the file: Type extends,
// implements or embeds Super, or a Rust `impl Super for Type`. Both are
// spelled as written. Go's implicit interface satisfaction is not declared
// and is never sent; the linker derives it from method sets.
type Supertype struct {
	Type, Super string
	// Interface reports a Super the declaration proves an interface: a type
	// a Java or TypeScript `implements` clause lists, or one a Java
	// interface's `extends` clause lists (JLS §9.1.3).
	Interface bool
}

// Reexport is one name the file's module exports under a name of its own or
// from another module, for the ECMAScript family: `export default foo;`,
// `export { a as b }`, `export { x } from './m'`, `export * from './m'`.
type Reexport struct {
	// Name is the name the module exports: "default" for a default export,
	// "*" for `export * from`, the alias of a namespace re-export
	// (`export * as ns from './m'`).
	Name string
	// Original is the name it denotes: a declaration of this file when From
	// is empty, else a name From exports; "*" for a whole module.
	Original string
	// From is the module specifier as written, empty for a local export.
	From string
}

// Site is one call site: a "call" Ref of the file, with the callee
// expression's shape and what the worker recovered about its receiver and
// arguments inside the file.
type Site struct {
	// Name is the callee token, the call Ref's NameStart and NameEnd.
	Name  Span
	Shape Shape
	// Qualifier is the type, package, namespace or module a ShapeStatic site
	// names, as written (`T` in `T::f()`, `pkg` in `pkg.F()`); for a
	// ShapeConstructor site, the type it creates when the callee token alone
	// does not name it (`a.b.T` in `new a.b.T()`, the class a `this(...)` or
	// `super(...)` invocation constructs); empty for every other shape.
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
	// Arguments has one entry per argument, in order, each where the argument
	// is and how it is passed: Arguments[i] and Args[i] describe the same
	// argument. It is empty when the worker sent no argument positions.
	Arguments []Argument
}

// Argument is one argument of a call site.
type Argument struct {
	// At is the argument expression, without its keyword or spread marker.
	At Span
	// Keyword is the parameter name a keyword argument names, as written
	// (`timeout` in `f(timeout=3)`); empty for a positional argument.
	Keyword string
	Spread  Spread
	// Reads are the named variables the argument expression reads, each as
	// the byte range of the identifier that declares it, spelled as
	// Function.Vars spells a variable: a parameter or local of the calling
	// callable or of a callable enclosing it. A literal, or an expression
	// that reads no named variable, has none. They are the sources of the
	// argument's flow into the parameter it binds.
	Reads []Span
}

// Spread is how an argument expands into several parameters.
type Spread byte

const (
	// SpreadNone is an argument passed as one value.
	SpreadNone Spread = 0
	// SpreadPositional expands into the remaining positional parameters:
	// Python `*xs`, ECMAScript `...xs`, Go `xs...`.
	SpreadPositional Spread = 1
	// SpreadKeyword expands into keyword parameters: Python `**kw`.
	SpreadKeyword Spread = 2
)

// Parameter is one declared parameter of a callable.
type Parameter struct {
	// Callable is the declaring callable's name token, the Export.Name of
	// its export.
	Callable Span
	// Name is the parameter's name token, where the dependence pass declares
	// the parameter's variable.
	Name Span
	// Index is the parameter's position: 0 for an explicit receiver or self
	// parameter (a Go receiver, Python `self` or `cls`, a Rust `self`), 1 for
	// the first declared parameter after it, and so on; a callable without a
	// receiver parameter starts at 1.
	Index uint32
	// Variadic is a parameter that collects the remaining arguments: Python
	// `*args`, ECMAScript `...rest`, Go `xs ...T`, C `...`. A variadic
	// keyword-only parameter collects the remaining keyword arguments
	// (Python `**kwargs`).
	Variadic bool
	// KeywordOnly is a parameter that only a keyword argument fills: one
	// declared after Python's `*` or `*args`, or `**kwargs`.
	KeywordOnly bool
}

// DynamicImport is one import whose path is an expression:
// `require("./x/" + name)`, `import(expr)`, `importlib.import_module(expr)`,
// `__import__(expr)`, `#include MACRO`.
type DynamicImport struct {
	// At is the import expression or directive.
	At Span
	// Fragments are the path's literal parts, in source order; empty when
	// the path has none. When Constant, the one fragment is the whole path.
	Fragments []string
	// Constant reports a path that folds to a constant (`require("./x/" +
	// "y")`, `import("./" + "m")`, `#include NAME` with NAME defined in the
	// file as a string or header name): an ordinary import of Fragments[0].
	// An ECMAScript `require` or `import()` of one string literal is an
	// import record of the structural query and is never sent here.
	Constant bool
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
	// Declares is, for a function the value's own expression declares (an
	// ECMAScript `a.b = () => …` or `{ k: () => … }`), that declaration's
	// name token, which names the value exactly; empty for any other.
	Declares Span
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
	// TypeNamed is an instance of the type Name, as written. Index is
	// ExactObject when the value is an object of exactly that type, else 0.
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
	// and the package's Values write to that field. In Go, a field of a
	// receiver whose type is known is spelled "Owner.field", Owner as
	// written: the field a selector of that name denotes on Owner, its own or
	// promoted, whose declared type (Export.Returns) types the value. A field
	// of such a field is spelled "Owner.field.next", and so on, each step
	// read on the type the step before it holds; Owner is two components
	// when the first is an import (`pkg.T`). Java spells a field read on a
	// receiver of known type the same way ("Owner.field", Owner as written,
	// a qualified name when written qualified); `this.f` is the enclosing
	// class's field. A bare name is a field of an unknown owner and types
	// nothing. ECMAScript and Python send the bare field name.
	TypeField TypeKind = 6
	// TypeFunction is the function or method Name, as written, taken as a
	// value. Index is the receiver parameters the value takes ahead of the
	// callable's own: 1 for a Go method expression `T.M` (spelled "*T.M"
	// for `(*T).M`) and for a Python method taken through its class object
	// (`C.m`, whose call passes the receiver first unless the method is a
	// static or class method, which the linker reads from its declaration),
	// else 0.
	TypeFunction TypeKind = 7
	// TypeClass is the type Name itself as a value, not an instance of it: a
	// class object (`A.make()` in Python, `C.m()` in ECMAScript). A call on
	// it binds Name's own member or the nearest inherited one, never a
	// subtype's override.
	TypeClass TypeKind = 8
	// TypeUnion is every type of Link.Unions[Index]: a value two or more
	// disagreeing types reach.
	TypeUnion TypeKind = 9
)

// ExactObject is the Index of a TypeNamed value that is an object of
// exactly the type Name: a C++ object declared by value (a local, a
// parameter, a data member, a file-scope object or an array element),
// neither a pointer nor a reference, whose dynamic type is its declared
// type, so a virtual call on it runs that type's function ([class.virtual]:
// the call is virtual only through a pointer or reference). The result of
// a function that returns the type by value (Export.Returns) is one too.
// Only the C++ worker sends it.
const ExactObject uint32 = 1

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
//	exports    n × (name start, end; receiver; signature; params; optional;
//	                variadic, static, abstract, virtual, final, internal
//	                flags; returns type; override flag; alias; property,
//	                exported flags; visibility; macros)
//	supertypes n × (type; super; interface flag)
//	sites      n × (name start, end; shape byte; qualifier; receiver type;
//	                signature; args n × type; arguments n × (at start, end;
//	                keyword; spread byte; reads n × (start, end)))
//	values     n × (at start, end; field; type; declares start, end)
//	reexports  n × (name; original; from)
//	unions     n × (n × type)
//	parameters n × (callable start, end; name start, end; index; variadic,
//	                keyword-only flags)
//	dynamic    n × (at start, end; fragments n × string; constant flag)
//	computed   n × site, as in sites
//	type       kind byte; name; index
func AppendLink(dst []byte, l *Link) []byte {
	e := encoder{b: dst}
	e.u32(uint32(len(l.Exports)))
	for _, x := range l.Exports {
		e.span(x.Name)
		e.str(x.Receiver)
		e.str(x.Signature)
		e.u32(x.Params)
		e.u32(x.Optional)
		e.flag(x.Variadic)
		e.flag(x.Static)
		e.flag(x.Abstract)
		e.flag(x.Virtual)
		e.flag(x.Final)
		e.flag(x.Internal)
		e.typ(x.Returns)
		e.flag(x.Override)
		e.str(x.Alias)
		e.flag(x.Property)
		e.flag(x.Exported)
		e.str(x.Visibility)
		e.str(x.Macros)
	}
	e.u32(uint32(len(l.Supertypes)))
	for _, s := range l.Supertypes {
		e.str(s.Type)
		e.str(s.Super)
		e.flag(s.Interface)
	}
	e.u32(uint32(len(l.Sites)))
	for i := range l.Sites {
		e.site(&l.Sites[i])
	}
	e.u32(uint32(len(l.Values)))
	for _, v := range l.Values {
		e.span(v.At)
		e.str(v.Field)
		e.typ(v.Type)
		e.span(v.Declares)
	}
	e.u32(uint32(len(l.Reexports)))
	for _, r := range l.Reexports {
		e.str(r.Name)
		e.str(r.Original)
		e.str(r.From)
	}
	e.u32(uint32(len(l.Unions)))
	for _, u := range l.Unions {
		e.u32(uint32(len(u)))
		for _, t := range u {
			e.typ(t)
		}
	}
	e.u32(uint32(len(l.Parameters)))
	for _, p := range l.Parameters {
		e.span(p.Callable)
		e.span(p.Name)
		e.u32(p.Index)
		e.flag(p.Variadic)
		e.flag(p.KeywordOnly)
	}
	e.u32(uint32(len(l.DynamicImports)))
	for _, x := range l.DynamicImports {
		e.span(x.At)
		e.u32(uint32(len(x.Fragments)))
		for _, f := range x.Fragments {
			e.str(f)
		}
		e.flag(x.Constant)
	}
	e.u32(uint32(len(l.Computed)))
	for i := range l.Computed {
		e.site(&l.Computed[i])
	}
	return e.b
}

// site appends one site's encoding.
func (e *encoder) site(s *Site) {
	e.span(s.Name)
	e.b = append(e.b, byte(s.Shape))
	e.str(s.Qualifier)
	e.typ(s.Receiver)
	e.str(s.Signature)
	e.u32(uint32(len(s.Args)))
	for _, a := range s.Args {
		e.typ(a)
	}
	e.u32(uint32(len(s.Arguments)))
	for _, a := range s.Arguments {
		e.span(a.At)
		e.str(a.Keyword)
		e.b = append(e.b, byte(a.Spread))
		e.u32(uint32(len(a.Reads)))
		for _, r := range a.Reads {
			e.span(r)
		}
	}
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
// produces: a short or overlong payload, an unknown shape, type kind, spread
// or flag byte, or a string that is not UTF-8.
var ErrMalformedLink = errors.New("wire: malformed link message")

// DecodeLink decodes one KindLink payload. Every list's length is checked
// against the bytes that remain before anything is allocated for it. Ranges
// and indices are not checked against the source or the file's other
// records here; the parent does that.
func DecodeLink(payload []byte) (Link, error) {
	d := decoder{b: payload, malformed: ErrMalformedLink}
	var l Link
	// The smallest export is 8 span, 4+4 strings, 4 params, 4 optional, 6
	// flags, a 9-byte type, 1 flag, 4 alias, 2 flags, 4 visibility and 4
	// macros; a supertype two strings and
	// 1 flag; a site 8 span, 1 shape, 4 qualifier, 9 type, 4 signature, 4
	// args and 4 arguments; an argument 8 span, 4 keyword, 1 spread and 4
	// reads; a read 8 span; a value 8 span, 4 field, 9 type and 8 span; a reexport
	// three strings; a union its count; a parameter two 8 spans, 4 index and 2 flags; a
	// dynamic import 8 span, 4 fragments and 1 flag; a fragment its length.
	l.Exports = make([]Export, 0, d.count(54))
	for range cap(l.Exports) {
		x := Export{Name: d.span(), Receiver: d.str(), Signature: d.str(), Params: d.u32(), Optional: d.u32()}
		x.Variadic, x.Static, x.Abstract = d.flag(), d.flag(), d.flag()
		x.Virtual, x.Final, x.Internal = d.flag(), d.flag(), d.flag()
		x.Returns = d.typ()
		x.Override, x.Alias = d.flag(), d.str()
		x.Property, x.Exported = d.flag(), d.flag()
		x.Visibility, x.Macros = d.str(), d.str()
		l.Exports = append(l.Exports, x)
	}
	l.Supertypes = make([]Supertype, 0, d.count(9))
	for range cap(l.Supertypes) {
		x := Supertype{Type: d.str(), Super: d.str()}
		x.Interface = d.flag()
		l.Supertypes = append(l.Supertypes, x)
	}
	l.Sites = make([]Site, 0, d.count(34))
	for range cap(l.Sites) {
		l.Sites = append(l.Sites, d.site())
	}
	l.Values = make([]Value, 0, d.count(29))
	for range cap(l.Values) {
		l.Values = append(l.Values, Value{At: d.span(), Field: d.str(), Type: d.typ(), Declares: d.span()})
	}
	l.Reexports = make([]Reexport, 0, d.count(12))
	for range cap(l.Reexports) {
		l.Reexports = append(l.Reexports, Reexport{Name: d.str(), Original: d.str(), From: d.str()})
	}
	l.Unions = make([][]Type, 0, d.count(4))
	for range cap(l.Unions) {
		u := make([]Type, 0, d.count(9))
		for range cap(u) {
			u = append(u, d.typ())
		}
		l.Unions = append(l.Unions, u)
	}
	l.Parameters = make([]Parameter, 0, d.count(22))
	for range cap(l.Parameters) {
		p := Parameter{Callable: d.span(), Name: d.span(), Index: d.u32()}
		p.Variadic, p.KeywordOnly = d.flag(), d.flag()
		l.Parameters = append(l.Parameters, p)
	}
	l.DynamicImports = make([]DynamicImport, 0, d.count(13))
	for range cap(l.DynamicImports) {
		x := DynamicImport{At: d.span()}
		x.Fragments = make([]string, 0, d.count(4))
		for range cap(x.Fragments) {
			x.Fragments = append(x.Fragments, d.str())
		}
		x.Constant = d.flag()
		l.DynamicImports = append(l.DynamicImports, x)
	}
	l.Computed = make([]Site, 0, d.count(34))
	for range cap(l.Computed) {
		l.Computed = append(l.Computed, d.site())
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

// site decodes one site.
func (d *decoder) site() Site {
	s := Site{Name: d.span(), Shape: d.shape(), Qualifier: d.str(), Receiver: d.typ(), Signature: d.str()}
	s.Args = make([]Type, 0, d.count(9))
	for range cap(s.Args) {
		s.Args = append(s.Args, d.typ())
	}
	s.Arguments = make([]Argument, 0, d.count(17))
	for range cap(s.Arguments) {
		a := Argument{At: d.span(), Keyword: d.str(), Spread: d.spread()}
		a.Reads = make([]Span, 0, d.count(8))
		for range cap(a.Reads) {
			a.Reads = append(a.Reads, d.span())
		}
		s.Arguments = append(s.Arguments, a)
	}
	return s
}

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

func (d *decoder) spread() Spread {
	s := Spread(d.u8())
	if d.err == nil && s > SpreadKeyword {
		d.fail("an unknown argument spread")
	}
	return s
}

func (d *decoder) typ() Type {
	k := TypeKind(d.u8())
	if d.err == nil && k > TypeUnion {
		d.fail("an unknown type kind")
	}
	return Type{Kind: k, Name: d.str(), Index: d.u32()}
}

// The symbol visibility a C or C++ attribute states: the GCC `visibility`
// attribute's four values, the MSVC `__declspec` storage classes, none
// (VisibilityUnspecified), and, for a macro replacement, one that names
// another macro among other tokens, which is not expanded
// (VisibilityUnresolved).
const (
	VisibilityDefault     = "default"
	VisibilityHidden      = "hidden"
	VisibilityProtected   = "protected"
	VisibilityInternal    = "internal"
	VisibilityDllexport   = "dllexport"
	VisibilityDllimport   = "dllimport"
	VisibilityUnspecified = "unspecified"
	VisibilityUnresolved  = "unresolved"
)
