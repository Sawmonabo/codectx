package worker

import (
	"strconv"
	"strings"
	"sync"
	"unicode"
	"unicode/utf8"

	"github.com/Sawmonabo/codectx/internal/provider/treesitter/flow"
	"github.com/Sawmonabo/codectx/internal/provider/treesitter/wire"
)

// javaLinker builds the link tables of one Java file in one walk of the flat
// tree, in source order, holding the names in scope as it goes.
//
// Java declares the static type of every field, local, parameter and pattern
// variable, and class-hierarchy analysis dispatches on that static type, so
// the walk recovers a receiver's type from declarations alone: a name's
// declared type, `this` (the enclosing type), `new T()`, a cast and another
// call's result. The def-use chains would recover the dynamic types the
// analysis does not read, so function adds nothing.
//
// A type is sent as its erasure (JLS §4.6): the name as written, qualified
// as written, with its type arguments and annotations dropped, and a type
// variable replaced by the erasure of its first bound, or unknown when it
// has none. Arrays and primitive types carry no method a declaration of the
// repository supplies and are sent as unknown. Signatures keep the types as
// written (wire.Export.Signature).
//
// A field read is sent as the field it reads, TypeField "Owner.field"
// (wire.TypeField): `this.f` is the enclosing type's field, a bare name a
// field in scope is its declaring type's, a bare name nothing of the file
// binds and no type spells is the enclosing type's, as `this.f` is, `x.f`
// the field of x's type and
// `T.f` the field of the type T as written; a field of such a field appends
// its name. Each field, constant and enum constant whose type the file
// declares is an Export whose Returns is that type, so the linker types the
// read from the declaration, its own or a supertype's, in whichever file
// declares it. A record component is no declaration the structural query
// captures, so a read of one keeps its declared type.
type javaLinker struct {
	linkReads
	k   *javaSyntax
	x   *javaLinkSyntax
	src []byte
	out *wire.Link
	// types holds every type name the file declares, at any depth, and every
	// simple name a single-type import introduces: a qualifier among them
	// names a type rather than a value.
	types map[string]bool
	// vars binds every field, enum constant, record component, local,
	// parameter and pattern variable in scope to its entry in bound; fields
	// binds the enclosing types' fields alone, which `this.f` names; tparams
	// binds the type variables in scope to their erasure.
	vars, fields, tparams scope
	bound                 []javaBound
	// frames are the enclosing type bodies, innermost last.
	frames []javaFrame
	// siteAt maps a call node to its index in out.Sites, which a later
	// site's TypeResult names.
	siteAt map[uintptr]uint32
}

// javaBound is what a name in scope is known to hold.
type javaBound struct {
	typ   wire.Type
	field bool
	// owner is the type that declares a field sent as an Export, whose read
	// is TypeField "owner.name"; empty for every other binding.
	owner string
}

// javaFrame is one enclosing type body: name is the type `this` denotes,
// named false for an anonymous class body or an enum constant's body, whose
// type has no name in the source and is sent under the name `#n`, n the
// byte offset of the token its structural declaration is named by (the
// created type's last simple name, `new T`, or the enum constant's name): a
// name no Java identifier can spell (JLS §3.8), which the linker reads as
// that declaration, declared a subtype of the type it instantiates (JLS
// §15.9.5, §8.9.1).
type javaFrame struct {
	name  string
	named bool
	// super is the class a named class extends, as erased, or the type an
	// anonymous body instantiates; empty when it names none.
	super string
	// final reports a body no class extends, whose methods no subtype
	// overrides: a `final` class, a record (JLS §8.10), an enum none of
	// whose constants has a body (JLS §8.9), an anonymous body.
	final bool
}

// javaMark is where the three scopes stood, the point restore returns to.
type javaMark struct{ vars, fields, tparams int }

func newJavaLinker() *javaLinker {
	return &javaLinker{k: javaSyntaxOf(), x: javaLinkSyntaxOf(), types: map[string]bool{}, siteAt: map[uintptr]uint32{}}
}

// function adds nothing for Java: see javaLinker.
func (k *javaLinker) function(fn Node, g *flow.Graph, du flow.Edges, src []byte) {}

// file appends the file's exports (every method and constructor), the
// supertypes its type declarations name, its call sites (every method
// invocation, every creation and every explicit constructor invocation
// that names a class, the call references the structural query captures)
// and its values (every method reference, and every lambda written to a
// field).
func (k *javaLinker) file(root Node, src []byte, out *wire.Link) {
	k.src, k.out = src, out
	defer func() { k.src, k.out = nil, nil }()
	k.collectTypes(root)
	k.visit(root)
}

// collectTypes fills k.types before the walk, since a type may be used
// before the file declares it.
func (k *javaLinker) collectTypes(root Node) {
	c := root.Walk()
	for {
		switch n := c.Node(); {
		case k.x.isType(n.KindId()):
			if name := n.ChildByFieldId(k.k.fName); !name.IsNull() {
				k.types[view(k.text(name))] = true
			}
		case n.KindId() == k.x.importDecl:
			k.importType(n)
		}
		if c.GotoFirstChild() {
			continue
		}
		for !c.GotoNextSibling() {
			if !c.GotoParent() {
				return
			}
		}
	}
}

// importType records the simple name a single-type import introduces. A
// static import introduces members and an on-demand import no name.
func (k *javaLinker) importType(n Node) {
	var path Node
	for c := n.first(); !c.IsNull(); c = c.next() {
		switch {
		case !c.IsNamed() && string(k.text(c)) == "static", c.KindId() == k.x.asterisk:
			return
		case c.KindId() == k.x.scopedIdentifier:
			path = c.ChildByFieldId(k.k.fName)
		case c.KindId() == k.k.identifier:
			path = c
		}
	}
	if !path.IsNull() {
		k.types[view(k.text(path))] = true
	}
}

// visit walks n, appending what it declares, calls and stores.
func (k *javaLinker) visit(n Node) {
	j, x := k.k, k.x
	switch id := n.KindId(); {
	case x.isType(id):
		k.typeDecl(n)
	case id == j.methodDecl || id == j.ctorDecl:
		k.callable(n)
	case id == j.lambda:
		k.lambda(n)
	case id == j.methodInvocation:
		k.invocation(n)
	case id == j.objectCreation:
		k.creation(n)
	case id == x.explicitConstructor:
		k.constructorCall(n)
	case id == j.enumConstant:
		k.enumConstant(n)
	case id == j.methodReference:
		field, _ := k.fieldOf(n)
		k.out.Values = append(k.out.Values, wire.Value{At: javaSpan(n), Field: field,
			Type: wire.Type{Kind: wire.TypeFunction, Name: k.compact(n)}, Target: k.referenceTarget(n)})
		k.children(n)
	case id == j.localVarDecl:
		k.locals(n)
	case id == j.resource:
		k.resource(n)
	case id == j.enhancedFor:
		k.enhancedFor(n)
	case id == j.catchFormalParameter:
		k.catchParam(n)
	case id == j.instanceofExpr:
		k.children(n)
		if name := n.ChildByFieldId(j.fName); !name.IsNull() {
			k.bind(&k.vars, name, k.named(n.ChildByFieldId(j.fRight)), false)
		}
	case id == j.typePattern || id == j.recordPatternComponent:
		k.children(n)
		k.pattern(n)
	default:
		k.children(n)
	}
}

// children visits n's named children, extras left out but an ERROR node the
// parser made an extra kept, inside a scope of its own when n opens one.
func (k *javaLinker) children(n Node) {
	scoped := k.x.opensScope(n.KindId())
	var m javaMark
	if scoped {
		m = k.mark()
	}
	for c := n.first(); !c.IsNull(); c = c.next() {
		if c.IsNamed() && (!c.IsExtra() || c.IsError()) {
			k.visit(c)
		}
	}
	if scoped {
		k.restore(m)
	}
}

// typeDecl walks a class, interface, enum, record or annotation type
// declaration: its supertypes, then its body with its type variables, its
// fields and its record components in scope.
func (k *javaLinker) typeDecl(n Node) {
	j, x := k.k, k.x
	m := k.mark()
	k.typeParams(n)
	var name, super string
	if nm := n.ChildByFieldId(j.fName); !nm.IsNull() {
		name = string(k.text(nm))
	}
	for c := n.first(); !c.IsNull(); c = c.next() {
		switch c.KindId() {
		case x.superclass:
			k.supertypes(name, c, false)
			super = k.erased(firstNamed(c))
		case x.superInterfaces, x.extendsInterfaces:
			for l := c.first(); !l.IsNull(); l = l.next() {
				if l.KindId() == x.typeList {
					k.supertypes(name, l, true)
				}
			}
		}
	}
	if n.KindId() == j.recordDecl {
		k.params(n.ChildByFieldId(j.fParameters), true)
	}
	final := n.KindId() == j.recordDecl || k.hasModifier(x.modifiersOf(n), "final") ||
		n.KindId() == j.enumDecl && !k.constantBodies(n.ChildByFieldId(j.fBody))
	k.frames = append(k.frames, javaFrame{name: name, named: true, super: super, final: final})
	k.members(n.ChildByFieldId(j.fBody))
	k.children(n)
	k.frames = k.frames[:len(k.frames)-1]
	k.restore(m)
}

// constantBodies reports an enum body with a constant that has a class
// body.
func (k *javaLinker) constantBodies(body Node) bool {
	for c := javaFirst(body); !c.IsNull(); c = c.next() {
		if c.KindId() == k.k.enumConstant && !k.x.kid(c, k.k.classBody).IsNull() {
			return true
		}
	}
	return false
}

// supertypes appends one supertype of the type name for every type listed
// under list, each marked an interface when iface says the list is a class's
// `implements` clause or an interface's `extends` clause (JLS §8.1.5,
// §9.1.3).
func (k *javaLinker) supertypes(name string, list Node, iface bool) {
	if name == "" {
		return
	}
	for t := list.first(); !t.IsNull(); t = t.next() {
		if !t.IsNamed() || t.IsExtra() {
			continue
		}
		if s := k.erased(t); s != "" {
			k.out.Supertypes = append(k.out.Supertypes, wire.Supertype{Type: name, Super: s, Interface: iface})
		}
	}
}

// members binds the fields, constants and enum constants a type body
// declares, which are in scope throughout the body (JLS §6.3).
func (k *javaLinker) members(body Node) {
	if body.IsNull() {
		return
	}
	j := k.k
	for c := body.first(); !c.IsNull(); c = c.next() {
		switch c.KindId() {
		case j.fieldDecl, j.constantDecl:
			typ := k.named(k.x.child(c, k.x.fType))
			for d := c.first(); !d.IsNull(); d = d.next() {
				if d.KindId() != j.variableDeclarator {
					continue
				}
				t := typ
				if !k.x.child(d, k.x.fDimensions).IsNull() {
					t = wire.Type{}
				}
				if name := d.ChildByFieldId(j.fName); !name.IsNull() && name.KindId() == j.identifier {
					k.memberField(name, t)
				}
			}
		case j.enumConstant:
			if name := c.ChildByFieldId(j.fName); !name.IsNull() && name.KindId() == j.identifier {
				k.memberField(name, k.thisType())
			}
		case j.enumBodyDecls:
			k.members(c)
		}
	}
}

// memberField binds a field, constant or enum constant name of type t that
// the innermost type body declares, and appends its export when the file
// declares t: the declaration a read of it is typed by.
func (k *javaLinker) memberField(name Node, t wire.Type) {
	var owner string
	if f := len(k.frames) - 1; t.Kind == wire.TypeNamed && f >= 0 && k.frames[f].name != "" {
		owner = k.frames[f].name
		k.out.Exports = append(k.out.Exports, wire.Export{Name: javaSpan(name), Returns: t})
	}
	for _, s := range []*scope{&k.vars, &k.fields} {
		k.bound = append(k.bound, javaBound{typ: t, field: true, owner: owner})
		s.push(k.text(name), int32(len(k.bound)-1))
	}
}

// callable appends the export of a method or constructor declaration and
// walks it with its type variables and parameters in scope.
func (k *javaLinker) callable(n Node) {
	j, x := k.k, k.x
	m := k.mark()
	k.typeParams(n)
	ps := n.ChildByFieldId(j.fParameters)
	if name := n.ChildByFieldId(j.fName); !name.IsNull() {
		e := wire.Export{Name: javaSpan(name), Receiver: k.thisType().Name}
		var sig strings.Builder
		sig.WriteByte('(')
		for p := javaFirst(ps); !p.IsNull(); p = p.next() {
			switch p.KindId() {
			case j.formalParameter:
				if e.Params > 0 {
					sig.WriteByte(',')
				}
				k.typeText(&sig, x.child(p, x.fType))
				k.typeText(&sig, x.child(p, x.fDimensions))
				e.Params++
			case j.spreadParameter:
				if e.Params > 0 {
					sig.WriteByte(',')
				}
				k.typeText(&sig, k.spreadType(p))
				sig.WriteString("...")
				e.Params++
				e.Variadic = true
			}
		}
		sig.WriteString(")(")
		if n.KindId() == j.ctorDecl {
			e.Static, e.Returns = true, k.thisType()
		} else {
			mods := x.modifiersOf(n)
			e.Static = k.hasModifier(mods, "static")
			e.Final = k.hasModifier(mods, "final") || k.hasModifier(mods, "private") ||
				len(k.frames) > 0 && k.frames[len(k.frames)-1].final
			e.Abstract = n.ChildByFieldId(j.fBody).IsNull() && !k.hasModifier(mods, "native")
			e.Override = k.overrides(mods)
			if rt := x.child(n, x.fType); !rt.IsNull() && rt.KindId() != x.voidType {
				k.typeText(&sig, rt)
				k.typeText(&sig, x.child(n, x.fDimensions))
				if x.child(n, x.fDimensions).IsNull() {
					e.Returns = k.named(rt)
				}
			}
		}
		sig.WriteByte(')')
		e.Signature = sig.String()
		k.out.Exports = append(k.out.Exports, e)
		k.parameters(e.Name, ps)
	}
	k.params(ps, false)
	k.children(n)
	k.restore(m)
}

// overrides reports modifiers that annotate a method `@Override` or
// `@java.lang.Override` (JLS §9.6.4.4).
func (k *javaLinker) overrides(mods Node) bool {
	j := k.k
	for c := javaFirst(mods); !c.IsNull(); c = c.next() {
		if c.KindId() != j.markerAnnotation && c.KindId() != j.annotation {
			continue
		}
		if name := k.compact(c.ChildByFieldId(j.fName)); name == "Override" || name == "java.lang.Override" {
			return true
		}
	}
	return false
}

// parameters appends a Parameter for each named parameter of the list ps of
// the callable named callable, at the name the lowering declares its
// variable by. A receiver parameter (`Foo this`) is the receiver at 0 and
// declares no name, so it is never sent; the formal and variable-arity
// parameters are numbered from 1.
func (k *javaLinker) parameters(callable wire.Span, ps Node) {
	j, x := k.k, k.x
	var index uint32
	for p := javaFirst(ps); !p.IsNull(); p = p.next() {
		var name Node
		switch p.KindId() {
		case j.formalParameter:
			name = p.ChildByFieldId(j.fName)
		case j.spreadParameter:
			if d := x.kid(p, j.variableDeclarator); !d.IsNull() {
				name = d.ChildByFieldId(j.fName)
			}
		default:
			continue
		}
		index++
		if name.IsNull() || name.KindId() != j.identifier {
			continue
		}
		k.out.Parameters = append(k.out.Parameters, wire.Parameter{Callable: callable, Name: javaSpan(name), Index: index,
			Variadic: p.KindId() == j.spreadParameter})
	}
}

// lambda appends a lambda written to a field as a value, then walks it with
// its parameters in scope; an inferred parameter's type is not declared.
func (k *javaLinker) lambda(n Node) {
	j := k.k
	if field, typ := k.fieldOf(n); field != "" {
		k.out.Values = append(k.out.Values, wire.Value{At: javaSpan(n), Field: field, Type: typ})
	}
	m := k.mark()
	switch ps := n.ChildByFieldId(j.fParameters); {
	case ps.IsNull():
	case ps.KindId() == j.identifier:
		k.bind(&k.vars, ps, wire.Type{}, false)
	case ps.KindId() == j.inferredParameters:
		for c := ps.first(); !c.IsNull(); c = c.next() {
			if c.KindId() == j.identifier {
				k.bind(&k.vars, c, wire.Type{}, false)
			}
		}
	default:
		k.params(ps, false)
	}
	k.children(n)
	k.restore(m)
}

// params binds the parameters of the list ps: as fields for a record's
// components, as locals otherwise. A receiver parameter declares no name.
func (k *javaLinker) params(ps Node, field bool) {
	if ps.IsNull() {
		return
	}
	j, x := k.k, k.x
	for p := ps.first(); !p.IsNull(); p = p.next() {
		var name Node
		var typ wire.Type
		switch p.KindId() {
		case j.formalParameter:
			name = p.ChildByFieldId(j.fName)
			if x.child(p, x.fDimensions).IsNull() {
				typ = k.named(x.child(p, x.fType))
			}
		case j.spreadParameter:
			if d := x.kid(p, j.variableDeclarator); !d.IsNull() {
				name = d.ChildByFieldId(j.fName)
			}
		}
		if name.IsNull() || name.KindId() != j.identifier {
			continue
		}
		k.bind(&k.vars, name, typ, field)
		if field {
			k.bind(&k.fields, name, typ, true)
		}
	}
}

// spreadType is a variable-arity parameter's element type: its one named
// child that is neither its modifiers, an annotation nor its declarator.
func (k *javaLinker) spreadType(p Node) Node {
	j, x := k.k, k.x
	for c := p.first(); !c.IsNull(); c = c.next() {
		switch id := c.KindId(); {
		case !c.IsNamed() || c.IsExtra(), id == x.modifiers, id == j.annotation, id == j.markerAnnotation,
			id == j.variableDeclarator:
		default:
			return c
		}
	}
	return Node{}
}

// typeParams binds the type variables of n's type parameter list, each to
// the erasure of its first bound (JLS §4.6), in order, so a bound may name
// an earlier variable.
func (k *javaLinker) typeParams(n Node) {
	x := k.x
	tps := x.kid(n, x.typeParameters)
	if tps.IsNull() {
		return
	}
	for tp := tps.first(); !tp.IsNull(); tp = tp.next() {
		if tp.KindId() != x.typeParameter {
			continue
		}
		name := x.kid(tp, x.typeIdentifier)
		if name.IsNull() {
			continue
		}
		var typ wire.Type
		if b := x.kid(tp, x.typeBound); !b.IsNull() {
			typ = k.named(firstNamed(b))
		}
		k.bind(&k.tparams, name, typ, false)
	}
}

// invocation appends a method invocation's site, after the sites of its
// receiver and arguments, which it may name as TypeResult.
func (k *javaLinker) invocation(n Node) {
	j := k.k
	k.children(n)
	name := n.ChildByFieldId(j.fName)
	if name.IsNull() {
		return
	}
	s := wire.Site{Name: javaSpan(name)}
	s.Args, s.Arguments = k.args(n.ChildByFieldId(j.fArguments))
	obj := n.ChildByFieldId(j.fObject)
	switch {
	case obj.IsNull():
		s.Shape = wire.ShapeBare
	case obj.KindId() == j.super:
		s.Shape, s.Receiver = wire.ShapeMember, k.superOf(len(k.frames)-1)
	case k.qualifiedSuper(n):
		s.Shape, s.Receiver = wire.ShapeMember, k.outerSuper(obj)
	default:
		if q, ok := k.qualifier(obj); ok {
			s.Shape, s.Qualifier = wire.ShapeStatic, q
		} else if i, ok := k.constructed(obj); ok {
			s.Shape, s.Receiver = wire.ShapeMember, wire.Type{Kind: wire.TypeResult, Index: i}
		} else {
			s.Shape, s.Receiver = wire.ShapeMember, k.exprType(obj)
		}
	}
	k.addSite(n, s)
}

// constructed is the constructor site of the receiver n when n, inside any
// parentheses, creates an object of a named class with no class body
// (`new Dog().sound()`): that object is exactly the class it creates
// (JLS §15.9.4), so the linker binds the call exactly (its TypeResult names
// a ShapeConstructor site). An anonymous class body creates an object of a
// subclass, which may override the method, and a variable's type is never
// read this way, since a later assignment may give it any subtype.
func (k *javaLinker) constructed(n Node) (uint32, bool) {
	j := k.k
	for n.KindId() == j.parenthesized {
		if n = firstNamed(n); n.IsNull() {
			return 0, false
		}
	}
	if n.KindId() != j.objectCreation {
		return 0, false
	}
	for c := n.first(); !c.IsNull(); c = c.next() {
		if c.KindId() == j.classBody {
			return 0, false
		}
	}
	i, ok := k.siteAt[n.Id()]
	return i, ok
}

// qualifiedSuper reports `X.super.m()`: a `super` child beside the object.
func (k *javaLinker) qualifiedSuper(n Node) bool {
	for c := n.first(); !c.IsNull(); c = c.next() {
		if c.KindId() == k.k.super && uint16(c.rec().field) != k.k.fObject {
			return true
		}
	}
	return false
}

// superOf is the receiver of `super.m()` in frame f: the supertypes of its
// type, named or anonymous.
func (k *javaLinker) superOf(f int) wire.Type {
	if f < 0 || k.frames[f].name == "" {
		return wire.Type{}
	}
	return wire.Type{Kind: wire.TypeSuper, Name: k.frames[f].name}
}

// outerSuper is the receiver of `X.super.m()`: the supertypes of X when X
// is an enclosing class (JLS §15.12.1), otherwise the interface X whose
// method it invokes.
func (k *javaLinker) outerSuper(obj Node) wire.Type {
	name := k.compact(obj)
	for f := len(k.frames) - 1; f >= 0; f-- {
		if k.frames[f].named && k.frames[f].name == name {
			return k.superOf(f)
		}
	}
	return wire.Type{Kind: wire.TypeNamed, Name: name}
}

// qualifier reports a receiver that names a type rather than a value (JLS
// §6.5.2), with that name as written: a name no variable in scope binds
// that the file declares or imports as a type or that is spelled as a type
// is, or such a name qualified by package or enclosing type names.
func (k *javaLinker) qualifier(obj Node) (string, bool) {
	j, x := k.k, k.x
	last := obj
	root := obj
	for root.KindId() == j.fieldAccess {
		root = root.ChildByFieldId(j.fObject)
		if root.IsNull() {
			return "", false
		}
	}
	if root.KindId() != j.identifier || k.lookupVar(root) {
		return "", false
	}
	for n := obj; n.KindId() == j.fieldAccess; n = n.ChildByFieldId(j.fObject) {
		if f := x.child(n, x.fField); f.IsNull() || f.KindId() != j.identifier {
			return "", false
		}
	}
	if last.KindId() == j.fieldAccess {
		last = x.child(last, x.fField)
	}
	text := k.text(last)
	if !k.types[view(text)] && !javaTypeLike(text) {
		return "", false
	}
	return k.compact(obj), true
}

// lookupVar reports whether a variable in scope binds the identifier name.
func (k *javaLinker) lookupVar(name Node) bool { return k.vars.innermost(k.text(name)) >= 0 }

// creation appends the site of a creation, the call reference the
// structural query captures, after the sites of its outer instance and
// arguments, then walks an anonymous class body with the type it
// instantiates as its `this`. Its callee token is the created type's last
// simple name; a qualified or generic type is the Qualifier as erased
// (`a.b.T` for `new a.b.T<X>()`).
func (k *javaLinker) creation(n Node) {
	j, x := k.k, k.x
	var body Node
	for c := n.first(); !c.IsNull(); c = c.next() {
		switch {
		case c.KindId() == j.classBody:
			body = c
		case c.IsNamed() && (!c.IsExtra() || c.IsError()):
			k.visit(c)
		}
	}
	t := x.child(n, x.fType)
	if name := k.createdName(t); !name.IsNull() {
		s := wire.Site{Name: javaSpan(name), Shape: wire.ShapeConstructor}
		s.Args, s.Arguments = k.args(n.ChildByFieldId(j.fArguments))
		if name != t {
			s.Qualifier = k.erased(t)
		}
		k.addSite(n, s)
	}
	if !body.IsNull() {
		at := n.StartByte()
		if name := k.createdName(t); !name.IsNull() {
			at = name.StartByte()
		}
		k.anonymous(k.erased(t), body, at)
	}
}

// createdName is the token a creation of the type t is called by, as the
// structural query captures it: a simple type name itself, the last simple
// name of a scoped one, and either below type arguments; null for any
// other type.
func (k *javaLinker) createdName(t Node) Node {
	x := k.x
	if t.IsNull() {
		return Node{}
	}
	switch t.KindId() {
	case x.typeIdentifier:
		return t
	case x.genericType:
		if c := firstNamed(t); !c.IsNull() && (c.KindId() == x.typeIdentifier || c.KindId() == x.scopedTypeIdentifier) {
			return k.createdName(c)
		}
	case x.scopedTypeIdentifier:
		if last := lastNamed(t); !last.IsNull() && last.KindId() == x.typeIdentifier {
			return last
		}
	}
	return Node{}
}

// constructorCall appends the site of an explicit constructor invocation,
// after the sites of its outer instance and arguments: `this(...)` creates
// through a constructor of the enclosing class, `super(...)` through one of
// the class it extends, which the site's Qualifier names. One with no
// class to name (`super(...)` in a class that extends none, whose
// superclass the file does not write) is no site.
func (k *javaLinker) constructorCall(n Node) {
	k.children(n)
	ctor := k.x.child(n, k.x.fConstructor)
	f := len(k.frames) - 1
	if ctor.IsNull() || f < 0 || !k.frames[f].named {
		return
	}
	q := k.frames[f].name
	if ctor.KindId() == k.k.super {
		q = k.frames[f].super
	}
	if q == "" {
		return
	}
	s := wire.Site{Name: javaSpan(ctor), Shape: wire.ShapeConstructor, Qualifier: q}
	s.Args, s.Arguments = k.args(n.ChildByFieldId(k.k.fArguments))
	k.addSite(n, s)
}

// enumConstant walks an enum constant's arguments, then its body, whose
// type is an anonymous subclass of the enum (JLS §8.9.1).
func (k *javaLinker) enumConstant(n Node) {
	var body Node
	for c := n.first(); !c.IsNull(); c = c.next() {
		switch {
		case c.KindId() == k.k.classBody:
			body = c
		case c.IsNamed() && (!c.IsExtra() || c.IsError()):
			k.visit(c)
		}
	}
	if !body.IsNull() {
		at := n.StartByte()
		if name := n.ChildByFieldId(k.k.fName); !name.IsNull() {
			at = name.StartByte()
		}
		k.anonymous(k.thisType().Name, body, at)
	}
}

// anonymous walks the body of an anonymous class of the type super, under
// the name `#at` (javaFrame), at the offset of the token its declaration is
// named by, sent as a subtype of super: the methods it declares are its own,
// which a call on super may reach as overrides and a call on another
// subtype of super never does.
func (k *javaLinker) anonymous(super string, body Node, at uint) {
	m := k.mark()
	name := "#" + strconv.FormatUint(uint64(at), 10)
	if super != "" {
		k.out.Supertypes = append(k.out.Supertypes, wire.Supertype{Type: name, Super: super})
	}
	k.frames = append(k.frames, javaFrame{name: name, super: super, final: true})
	k.members(body)
	k.visit(body)
	k.frames = k.frames[:len(k.frames)-1]
	k.restore(m)
}

// locals walks a local variable declaration, binding each declarator after
// its initializer; `var` takes the initializer's type (JLS §14.4.1).
func (k *javaLinker) locals(n Node) {
	j, x := k.k, k.x
	t := x.child(n, x.fType)
	inferred := k.isVar(t)
	declared := k.named(t)
	for d := n.first(); !d.IsNull(); d = d.next() {
		if d.KindId() != j.variableDeclarator {
			if d.IsNamed() && (!d.IsExtra() || d.IsError()) {
				k.visit(d)
			}
			continue
		}
		v := d.ChildByFieldId(j.fValue)
		if !v.IsNull() {
			k.visit(v)
		}
		typ := declared
		switch {
		case !x.child(d, x.fDimensions).IsNull():
			typ = wire.Type{}
		case inferred && !v.IsNull():
			typ = k.exprType(v)
		}
		if name := d.ChildByFieldId(j.fName); !name.IsNull() && name.KindId() == j.identifier {
			k.bind(&k.vars, name, typ, false)
		}
	}
}

// resource walks a try resource; one that declares a variable binds it after
// its initializer, in the statement's scope.
func (k *javaLinker) resource(n Node) {
	j, x := k.k, k.x
	name := n.ChildByFieldId(j.fName)
	if name.IsNull() {
		k.children(n)
		return
	}
	v := n.ChildByFieldId(j.fValue)
	if !v.IsNull() {
		k.visit(v)
	}
	t := x.child(n, x.fType)
	typ := k.named(t)
	if k.isVar(t) && !v.IsNull() {
		typ = k.exprType(v)
	}
	if name.KindId() == j.identifier {
		k.bind(&k.vars, name, typ, false)
	}
}

// enhancedFor walks an enhanced for: the iterated expression, then the body
// with the loop variable in scope. A `var` loop variable's element type is
// not recovered.
func (k *javaLinker) enhancedFor(n Node) {
	j, x := k.k, k.x
	m := k.mark()
	if v := n.ChildByFieldId(j.fValue); !v.IsNull() {
		k.visit(v)
	}
	if name := n.ChildByFieldId(j.fName); !name.IsNull() && name.KindId() == j.identifier {
		var typ wire.Type
		if x.child(n, x.fDimensions).IsNull() {
			typ = k.named(x.child(n, x.fType))
		}
		k.bind(&k.vars, name, typ, false)
	}
	if body := n.ChildByFieldId(j.fBody); !body.IsNull() {
		k.visit(body)
	}
	k.restore(m)
}

// catchParam binds a catch parameter, in its clause's scope, to its type
// when it catches one type; a multi-catch's type is their least upper bound
// (JLS §14.20), which the file does not declare.
func (k *javaLinker) catchParam(n Node) {
	j := k.k
	k.children(n)
	name := n.ChildByFieldId(j.fName)
	if name.IsNull() || name.KindId() != j.identifier {
		return
	}
	var typ wire.Type
	if ct := k.x.kid(n, j.catchType); !ct.IsNull() && ct.NamedChildCount() == 1 {
		typ = k.named(firstNamed(ct))
	}
	k.bind(&k.vars, name, typ, false)
}

// pattern binds a type pattern's or record pattern component's variable to
// its type. Its scope is the enclosing scope's rest, which over-approximates
// the flow scoping of JLS §6.3.1.
func (k *javaLinker) pattern(n Node) {
	j := k.k
	var name, typ Node
	for c := n.first(); !c.IsNull(); c = c.next() {
		switch {
		case !c.IsNamed() || c.IsExtra():
		case c.KindId() == j.identifier:
			name = c
		case typ.IsNull():
			typ = c
		}
	}
	if !name.IsNull() {
		t := k.named(typ)
		if k.isVar(typ) {
			t = wire.Type{}
		}
		k.bind(&k.vars, name, t, false)
	}
}

// fieldOf is the field a lambda or method reference n is written to, by a
// field's initializer or an assignment to a field, with the field's declared
// type when the file declares it; "" when n is written to no field.
func (k *javaLinker) fieldOf(n Node) (string, wire.Type) {
	j, x := k.k, k.x
	p := n.Parent()
	if p.IsNull() {
		return "", wire.Type{}
	}
	switch p.KindId() {
	case j.variableDeclarator:
		if g := p.Parent(); g.IsNull() || (g.KindId() != j.fieldDecl && g.KindId() != j.constantDecl) {
			return "", wire.Type{}
		}
		if name := p.ChildByFieldId(j.fName); !name.IsNull() && p.ChildByFieldId(j.fValue) == n {
			return string(k.text(name)), k.boundType(&k.fields, name)
		}
	case j.assignment:
		if p.ChildByFieldId(j.fRight) != n {
			return "", wire.Type{}
		}
		switch left := p.ChildByFieldId(j.fLeft); left.KindId() {
		case j.identifier:
			// A name no local binds is a field: of an enclosing type, or
			// inherited.
			if i := k.vars.innermost(k.text(left)); i >= 0 && !k.bound[k.vars.at(i).v].field {
				return "", wire.Type{}
			}
			return string(k.text(left)), k.boundType(&k.vars, left)
		case j.fieldAccess:
			f := x.child(left, x.fField)
			if f.IsNull() || f.KindId() != j.identifier {
				return "", wire.Type{}
			}
			var typ wire.Type
			if obj := left.ChildByFieldId(j.fObject); !obj.IsNull() && obj.KindId() == j.this {
				typ = k.boundType(&k.fields, f)
			}
			return string(k.text(f)), typ
		}
	}
	return "", wire.Type{}
}

// referenceTarget is the target type of the method reference n where its
// context writes one (JLS §15.13.2, §5.2, §5.5, §14.17): the declared type
// of the local, field or constant it initializes, of the variable or `this`
// field it is assigned to, the type of the cast it is the operand of, or
// the return type of the method whose `return` gives it, through
// parentheses and either arm of a conditional. Unknown in every other
// context (an argument, whose target type is a parameter of the method
// overload resolution picks; a lambda's result), and for an array type.
func (k *javaLinker) referenceTarget(n Node) wire.Type {
	j, x := k.k, k.x
	for {
		p := n.Parent()
		if p.IsNull() {
			return wire.Type{}
		}
		switch p.KindId() {
		case j.parenthesized:
			n = p
			continue
		case j.ternary:
			if p.ChildByFieldId(j.fCondition) == n {
				return wire.Type{}
			}
			n = p
			continue
		case j.variableDeclarator:
			g := p.Parent()
			if p.ChildByFieldId(j.fValue) != n || !x.child(p, x.fDimensions).IsNull() || g.IsNull() {
				return wire.Type{}
			}
			switch g.KindId() {
			case j.localVarDecl, j.fieldDecl, j.constantDecl:
				return k.named(x.child(g, x.fType))
			}
		case j.assignment:
			if p.ChildByFieldId(j.fRight) != n {
				return wire.Type{}
			}
			switch left := p.ChildByFieldId(j.fLeft); left.KindId() {
			case j.identifier:
				return k.boundType(&k.vars, left)
			case j.fieldAccess:
				f := x.child(left, x.fField)
				if obj := left.ChildByFieldId(j.fObject); !f.IsNull() && !obj.IsNull() && obj.KindId() == j.this {
					return k.boundType(&k.fields, f)
				}
			}
		case j.cast:
			return k.named(x.child(p, x.fType))
		case j.returnStmt:
			for m := p.Parent(); !m.IsNull(); m = m.Parent() {
				switch m.KindId() {
				case j.lambda:
					return wire.Type{}
				case j.methodDecl:
					if !x.child(m, x.fDimensions).IsNull() {
						return wire.Type{}
					}
					return k.named(x.child(m, x.fType))
				}
			}
		}
		return wire.Type{}
	}
}

// exprType is the static type of the expression n as the file declares it,
// TypeUnknown where it declares none.
func (k *javaLinker) exprType(n Node) wire.Type {
	j, x := k.k, k.x
	switch n.KindId() {
	case j.parenthesized:
		if e := firstNamed(n); !e.IsNull() {
			return k.exprType(e)
		}
	case j.identifier:
		if i := k.vars.innermost(k.text(n)); i >= 0 {
			if b := k.bound[k.vars.at(i).v]; b.owner != "" {
				return wire.Type{Kind: wire.TypeField, Name: b.owner + "." + string(k.text(n))}
			}
			return k.bound[k.vars.at(i).v].typ
		}
		// A name no variable of the file binds that no type spells
		// (qualifier) is a field the enclosing class inherits or an
		// enclosing class's (JLS §6.5.6.1, §8.1.3), read as `this.f` is:
		// the linker walks the class's supertypes, then its enclosing
		// classes.
		if text := k.text(n); !k.types[view(text)] && !javaTypeLike(text) {
			if t := k.thisType(); t.Name != "" {
				return wire.Type{Kind: wire.TypeField, Name: t.Name + "." + string(text)}
			}
		}
	case j.this:
		return k.thisType()
	case j.objectCreation, j.cast:
		return k.named(x.child(n, x.fType))
	case j.methodInvocation:
		if i, ok := k.siteAt[n.Id()]; ok {
			return wire.Type{Kind: wire.TypeResult, Index: i}
		}
	case j.fieldAccess:
		obj, f := n.ChildByFieldId(j.fObject), x.child(n, x.fField)
		switch {
		case obj.IsNull() || f.IsNull():
		case f.KindId() == j.this:
			// `Outer.this` is the enclosing instance of the type Outer.
			return wire.Type{Kind: wire.TypeNamed, Name: k.compact(obj)}
		case f.KindId() != j.identifier:
		case obj.KindId() == j.this:
			// A record component the file declares keeps its declared type:
			// no declaration of it carries the type.
			if i := k.fields.innermost(k.text(f)); i >= 0 && k.bound[k.fields.at(i).v].owner == "" {
				return k.bound[k.fields.at(i).v].typ
			}
			if t := k.thisType(); t.Name != "" {
				return wire.Type{Kind: wire.TypeField, Name: t.Name + "." + string(k.text(f))}
			}
		default:
			if q, ok := k.qualifier(obj); ok {
				return wire.Type{Kind: wire.TypeField, Name: q + "." + string(k.text(f))}
			}
			switch t := k.exprType(obj); t.Kind {
			case wire.TypeNamed:
				return wire.Type{Kind: wire.TypeField, Name: t.Name + "." + string(k.text(f))}
			case wire.TypeField:
				if strings.Contains(t.Name, ".") {
					return wire.Type{Kind: wire.TypeField, Name: t.Name + "." + string(k.text(f))}
				}
			}
		}
	}
	return wire.Type{}
}

// args is the type and the argument of each argument in list, in order:
// its expression and the variables it reads (readName, readSkip).
func (k *javaLinker) args(list Node) ([]wire.Type, []wire.Argument) {
	if list.IsNull() {
		return nil, nil
	}
	var types []wire.Type
	var args []wire.Argument
	for c := list.first(); !c.IsNull(); c = c.next() {
		if c.IsNamed() && !c.IsExtra() {
			types = append(types, k.exprType(c))
			args = append(args, wire.Argument{At: javaSpan(c), Reads: k.reads().argReads(c, k.readName, k.readSkip, nil)})
		}
	}
	return types, args
}

// readName reports an identifier an argument reads as a value: not the
// field a field access names, nor the method a method reference names, nor
// a segment of a qualified name.
func (k *javaLinker) readName(n Node) bool {
	j := k.k
	if n.KindId() != j.identifier {
		return false
	}
	switch p := n.Parent(); {
	case p.IsNull():
	case p.KindId() == j.fieldAccess:
		return k.x.child(p, k.x.fField) != n
	case p.KindId() == j.methodReference:
		return firstNamed(p) == n
	case p.KindId() == k.x.scopedIdentifier:
		return false
	}
	return true
}

// readSkip reports a subtree whose identifiers an argument does not read: a
// method invocation's or creation's result, a lambda, which reads its
// captures only when it runs, and an anonymous class body.
func (k *javaLinker) readSkip(n Node) bool {
	j := k.k
	switch n.KindId() {
	case j.methodInvocation, j.objectCreation, j.lambda, j.classBody:
		return true
	}
	return false
}

// addSite appends s, the site of the call node n.
func (k *javaLinker) addSite(n Node, s wire.Site) {
	k.siteAt[n.Id()] = uint32(len(k.out.Sites))
	k.out.Sites = append(k.out.Sites, s)
}

// thisType is the type `this` denotes in the innermost type body.
func (k *javaLinker) thisType() wire.Type {
	if len(k.frames) == 0 || k.frames[len(k.frames)-1].name == "" {
		return wire.Type{}
	}
	return wire.Type{Kind: wire.TypeNamed, Name: k.frames[len(k.frames)-1].name}
}

// named is the type t denotes as a TypeNamed of its erasure, or TypeUnknown
// for an array, a primitive, `var` or an unbounded type variable.
func (k *javaLinker) named(t Node) wire.Type {
	if s := k.erased(t); s != "" {
		return wire.Type{Kind: wire.TypeNamed, Name: s}
	}
	return wire.Type{}
}

// erased is the erasure of the class or interface type t, its name as
// written with type arguments and annotations dropped, or "".
func (k *javaLinker) erased(t Node) string {
	if t.IsNull() {
		return ""
	}
	x := k.x
	switch t.KindId() {
	case x.typeIdentifier:
		if k.isVar(t) {
			return ""
		}
		if i := k.tparams.innermost(k.text(t)); i >= 0 {
			return k.bound[k.tparams.at(i).v].typ.Name
		}
		return string(k.text(t))
	case x.scopedTypeIdentifier, x.genericType:
		var b strings.Builder
		k.qualified(&b, t)
		return b.String()
	case x.annotatedType:
		for c := t.first(); !c.IsNull(); c = c.next() {
			if id := c.KindId(); c.IsNamed() && !c.IsExtra() && id != k.k.annotation && id != k.k.markerAnnotation {
				return k.erased(c)
			}
		}
	}
	return ""
}

// qualified writes the erasure of a scoped or generic type to b: the simple
// names joined by ".", without type arguments or annotations.
func (k *javaLinker) qualified(b *strings.Builder, t Node) {
	x := k.x
	for c := t.first(); !c.IsNull(); c = c.next() {
		switch c.KindId() {
		case x.typeIdentifier:
			if b.Len() > 0 {
				b.WriteByte('.')
			}
			b.Write(k.text(c))
		case x.scopedTypeIdentifier, x.genericType:
			k.qualified(b, c)
		}
	}
}

// isVar reports the reserved type name `var` of a local variable.
func (k *javaLinker) isVar(t Node) bool {
	return !t.IsNull() && t.KindId() == k.x.typeIdentifier && string(k.text(t)) == "var"
}

// typeText writes the type t as written to b, its tokens without whitespace,
// comments or annotations (wire.Export.Signature).
func (k *javaLinker) typeText(b *strings.Builder, t Node) {
	if t.IsNull() || t.IsExtra() || t.KindId() == k.k.annotation || t.KindId() == k.k.markerAnnotation {
		return
	}
	c := t.first()
	if c.IsNull() {
		b.Write(k.text(t))
		return
	}
	for ; !c.IsNull(); c = c.next() {
		k.typeText(b, c)
	}
}

// compact is n as written without whitespace, comments or annotations.
func (k *javaLinker) compact(n Node) string {
	var b strings.Builder
	k.typeText(&b, n)
	return b.String()
}

// hasModifier reports whether the modifiers mods hold the keyword word.
func (k *javaLinker) hasModifier(mods Node, word string) bool {
	for c := javaFirst(mods); !c.IsNull(); c = c.next() {
		if !c.IsNamed() && string(k.text(c)) == word {
			return true
		}
	}
	return false
}

func (k *javaLinker) text(n Node) []byte { return textOf(k.src, n) }

// bind binds name in s to typ.
func (k *javaLinker) bind(s *scope, name Node, typ wire.Type, field bool) {
	k.bound = append(k.bound, javaBound{typ: typ, field: field})
	s.push(k.text(name), int32(len(k.bound)-1))
}

// boundType is the type the innermost binding of name in s holds.
func (k *javaLinker) boundType(s *scope, name Node) wire.Type {
	if i := s.innermost(k.text(name)); i >= 0 {
		return k.bound[s.at(i).v].typ
	}
	return wire.Type{}
}

func (k *javaLinker) mark() javaMark {
	return javaMark{k.vars.mark(), k.fields.mark(), k.tparams.mark()}
}

func (k *javaLinker) restore(m javaMark) {
	k.vars.truncate(m.vars)
	k.fields.truncate(m.fields)
	k.tparams.truncate(m.tparams)
}

// javaTypeLike reports a simple name spelled as a type is by the naming
// convention of JLS §6.1: an upper-case first letter and, unless it is one
// letter, a lower-case one, which a constant's name lacks.
func javaTypeLike(name []byte) bool {
	r, n := utf8.DecodeRune(name)
	if !unicode.IsUpper(r) {
		return false
	}
	if n == len(name) {
		return true
	}
	for _, r := range string(name[n:]) {
		if unicode.IsLower(r) {
			return true
		}
	}
	return false
}

// javaFirst is n's first child, or null for a null n.
func javaFirst(n Node) Node {
	if n.IsNull() {
		return Node{}
	}
	return n.first()
}

// javaSpan is n's byte range on the wire.
func javaSpan(n Node) wire.Span { return wireSpan(spanOf(n)) }

// javaLinkSyntax holds the kind and field ids the Java linker matches beyond
// those of the lowering (javaSyntax), resolved once by name against the
// pinned grammar. The fields here are outside the lowering's field registry,
// so they are read from each node's own field (child), never through
// ChildByFieldId.
type javaLinkSyntax struct {
	typeIdentifier, scopedTypeIdentifier, genericType, annotatedType, voidType, superclass, superInterfaces,
	extendsInterfaces, typeList, typeParameters, typeParameter, typeBound, modifiers, importDecl,
	scopedIdentifier, asterisk, explicitConstructor uint16

	fType, fField, fDimensions, fConstructor uint16

	// types marks the type declaration kinds, scope the kinds whose
	// declarations end with them, by kind id.
	types, scope []bool
}

func (x *javaLinkSyntax) isType(id uint16) bool     { return int(id) < len(x.types) && x.types[id] }
func (x *javaLinkSyntax) opensScope(id uint16) bool { return int(id) < len(x.scope) && x.scope[id] }

// child is n's first child under field f, read from each child's own field.
func (x *javaLinkSyntax) child(n Node, f uint16) Node {
	if n.IsNull() {
		return Node{}
	}
	for c := n.first(); !c.IsNull(); c = c.next() {
		if uint16(c.rec().field) == f {
			return c
		}
	}
	return Node{}
}

// kid is n's first child of kind id.
func (x *javaLinkSyntax) kid(n Node, id uint16) Node {
	if n.IsNull() {
		return Node{}
	}
	for c := n.first(); !c.IsNull(); c = c.next() {
		if c.KindId() == id {
			return c
		}
	}
	return Node{}
}

// modifiersOf is a declaration's modifiers, or null.
func (x *javaLinkSyntax) modifiersOf(n Node) Node { return x.kid(n, x.modifiers) }

var (
	javaLinkSyntaxOnce  sync.Once
	javaLinkSyntaxTable *javaLinkSyntax
)

// javaLinkSyntaxOf resolves the table once. A name the grammar does not
// define is a linker defect and panics.
func javaLinkSyntaxOf() *javaLinkSyntax {
	javaLinkSyntaxOnce.Do(func() {
		const language = "java"
		tl := mustGrammar(language)
		kind := func(name string) uint16 { return mustKind(tl, language, name, true) }
		field := func(name string) uint16 {
			v := tl.FieldIdForName(name)
			if v == 0 {
				panic("worker: the " + language + " grammar has no field " + name)
			}
			return v
		}
		x := &javaLinkSyntax{}
		x.typeIdentifier, x.scopedTypeIdentifier = kind("type_identifier"), kind("scoped_type_identifier")
		x.genericType, x.annotatedType, x.voidType = kind("generic_type"), kind("annotated_type"), kind("void_type")
		x.superclass, x.superInterfaces = kind("superclass"), kind("super_interfaces")
		x.extendsInterfaces, x.typeList = kind("extends_interfaces"), kind("type_list")
		x.typeParameters, x.typeParameter = kind("type_parameters"), kind("type_parameter")
		x.typeBound, x.modifiers, x.importDecl = kind("type_bound"), kind("modifiers"), kind("import_declaration")
		x.scopedIdentifier, x.asterisk = kind("scoped_identifier"), kind("asterisk")
		x.explicitConstructor = kind("explicit_constructor_invocation")
		x.fType, x.fField, x.fDimensions = field("type"), field("field"), field("dimensions")
		x.fConstructor = field("constructor")
		x.types = make([]bool, tl.NodeKindCount())
		for _, name := range []string{"class_declaration", "interface_declaration", "enum_declaration",
			"record_declaration", "annotation_type_declaration"} {
			x.types[kind(name)] = true
		}
		x.scope = make([]bool, tl.NodeKindCount())
		for _, name := range []string{"block", "constructor_body", "switch_block", "switch_block_statement_group",
			"switch_rule", "for_statement", "catch_clause", "try_with_resources_statement",
			"compact_constructor_declaration", "static_initializer"} {
			x.scope[kind(name)] = true
		}
		javaLinkSyntaxTable = x
	})
	return javaLinkSyntaxTable
}
