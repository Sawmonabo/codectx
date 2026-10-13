package worker

import (
	"sync"

	ts "github.com/tree-sitter/go-tree-sitter"

	"github.com/Sawmonabo/codectx/internal/provider/treesitter/flow"
	"github.com/Sawmonabo/codectx/internal/provider/treesitter/lang"
	"github.com/Sawmonabo/codectx/internal/provider/treesitter/wire"
)

// rustLinker builds the link tables of one Rust file in one walk of its flat
// tree. Rust declares the type of every parameter and of a binding where it
// matters, so the types a site's receiver and arguments carry are read from
// declarations, struct literals and other sites' results in the file's walk
// itself; function reads nothing of the def-use chains.
//
// Type names are spelled as written with references, pointers, `dyn`,
// `impl` and generic arguments removed (`&mut Foo<T>` is `Foo`), so a
// receiver, an impl's self type and a supertype name the type declaration
// itself. A declared value type also loses a smart pointer around it
// (unwrap: `Box<dyn Handler>` is Handler), since a method call reaches the
// owned value through it; an impl's self type and a trait bound do not.
// `Self` is spelled as the impl's self type, or the trait inside a
// trait. A signature is spelled by the rule of wire.Export.Signature from
// the type's tokens, comments and lifetimes left out and one space kept
// only between two word tokens (`&mut Foo`), so a declaration's signature
// and a function type's are equal exactly when they write the same types.
type rustLinker struct {
	linkReads
	k   *rsLinkSyntax
	src []byte
	out *wire.Link
	// owners are the enclosing impls and traits, innermost last; a function
	// item that is not their member pushes an empty owner, since `Self`
	// does not reach into it.
	owners []rsOwner
	// locals are the bindings in scope, innermost last; base is the first a
	// lookup may see, the first of the function item being walked, since an
	// item captures nothing of its enclosing function.
	locals []rsLocal
	base   int
	// generics are the type parameters in scope, innermost last.
	generics []rsGeneric
	// fields maps a struct declared in the file and one of its fields,
	// "struct\x00field", to the field's declared type.
	fields map[string]rsField
	// buf is the scratch a type's text is spelled into.
	buf []byte
}

// rsField is a struct field's declared type: the type its value carries
// (unwrap) and whether a smart pointer owns that value.
type rsField struct {
	typ string
	ptr bool
}

// rsOwner is one enclosing impl or trait: the name `Self` means, and, for
// an `impl Trait for Type` block, every method of which implements the
// trait's, the trait as typeName spells it (wire.Export.Trait); empty for
// an inherent impl, a trait, or a trait path typeName cannot spell.
type rsOwner struct {
	self      string
	traitImpl bool
	trait     string
}

// rsLocal is one binding: its name, the type its value carries, whether it
// is declared as a smart pointer owning a value of that type, and the
// signature of its declared function type, if it has one.
type rsLocal struct {
	name string
	typ  wire.Type
	ptr  bool
	sig  string
}

// rsGeneric is one type parameter: its name, the one trait its bounds name
// (empty when they name none or several), and the signature of a function
// trait among its bounds.
type rsGeneric struct {
	name   string
	bound  string
	bounds int
	sig    string
}

func newRustLinker() *rustLinker { return &rustLinker{} }

// function reads nothing: the file's walk recovers every type a Rust site
// carries from declarations (see rustLinker).
func (k *rustLinker) function(fn Node, g *flow.Graph, du flow.Edges, src []byte) {}

func (k *rustLinker) file(root Node, src []byte, out *wire.Link) {
	k.k, k.src, k.out = rsLinkSyntaxOf(), src, out
	k.owners, k.locals, k.generics, k.base = k.owners[:0], k.locals[:0], k.generics[:0], 0
	k.fields = map[string]rsField{}
	k.structFields(root)
	k.walk(root)
	k.src, k.out, k.fields = nil, nil, nil
}

// structFields records the declared type of every named field of every
// struct the file declares.
func (k *rustLinker) structFields(root Node) {
	s := k.k
	c := root.Walk()
	for {
		n := c.Node()
		if n.KindId() == s.structItem {
			name, body := n.ChildByFieldId(s.fName), n.ChildByFieldId(s.fBody)
			if !name.IsNull() && !body.IsNull() && body.KindId() == s.fieldDeclarationList {
				for f := body.first(); !f.IsNull(); f = f.next() {
					if f.KindId() != s.fieldDeclaration {
						continue
					}
					fn, ft := f.ChildByFieldId(s.fName), f.ChildByFieldId(s.fType)
					if fn.IsNull() || ft.IsNull() {
						continue
					}
					inner, ptr := k.unwrap(ft)
					if t := k.typeName(inner); t != "" {
						k.fields[k.str(name)+"\x00"+k.str(fn)] = rsField{typ: t, ptr: ptr}
					}
				}
			}
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

// walk records what n and everything under it say about the file's tables
// and returns the type n's value carries, TypeUnknown where it is not
// recovered. A call's sites under it are recorded before its own, so a site
// whose receiver or argument is another call's result names a site already
// numbered. A null n, a field an erroneous parse left out, carries nothing.
func (k *rustLinker) walk(n Node) wire.Type {
	s := k.k
	if n.IsNull() {
		return wire.Type{}
	}
	switch n.KindId() {
	case s.functionItem:
		k.functionItem(n)
	case s.functionSignatureItem:
		k.export(n)
	case s.implItem:
		k.implItem(n)
	case s.traitItem:
		k.traitItem(n)
	case s.closureExpression:
		mark := len(k.locals)
		if ps := n.ChildByFieldId(s.fParameters); !ps.IsNull() {
			for p := ps.first(); !p.IsNull(); p = p.next() {
				switch {
				case p.KindId() == s.parameter:
					k.param(p)
				case p.IsNamed() && !p.IsExtra():
					k.bindAll(p)
				}
			}
		}
		k.value(n.ChildByFieldId(s.fBody), "")
		k.locals = k.locals[:mark]
	case s.block:
		// A block's value is its tail expression, its last named child when
		// that is not a statement; a function path there is taken as a value.
		mark := len(k.locals)
		var tail Node
		for c := n.first(); !c.IsNull(); c = c.next() {
			if c.IsNamed() && !c.IsExtra() {
				tail = c
			}
		}
		for c := n.first(); !c.IsNull(); c = c.next() {
			switch {
			case c == tail:
				k.value(c, "")
			case !c.IsExtra() || c.IsError():
				k.walk(c)
			}
		}
		k.locals = k.locals[:mark]
	case s.ifExpression, s.whileExpression:
		mark := len(k.locals)
		k.children(n)
		k.locals = k.locals[:mark]
	case s.letDeclaration:
		k.let(n)
	case s.letCondition:
		k.walk(n.ChildByFieldId(s.fValue))
		k.bindAll(n.ChildByFieldId(s.fPattern))
	case s.forExpression:
		k.walk(n.ChildByFieldId(s.fValue))
		mark := len(k.locals)
		k.bindAll(n.ChildByFieldId(s.fPattern))
		k.walk(n.ChildByFieldId(s.fBody))
		k.locals = k.locals[:mark]
	case s.matchArm:
		mark := len(k.locals)
		if mp := n.ChildByFieldId(s.fPattern); !mp.IsNull() {
			guard := mp.ChildByFieldId(s.fCondition)
			for p := mp.first(); !p.IsNull(); p = p.next() {
				if guard.IsNull() || p != guard {
					k.bindAll(p)
				}
			}
			if !guard.IsNull() {
				k.walk(guard)
			}
		}
		k.value(n.ChildByFieldId(s.fValue), "")
		k.locals = k.locals[:mark]
	case s.callExpression:
		return k.call(n)
	case s.macroInvocation, s.tokenTree, s.useDeclaration, s.attributeItem:
		// A macro's arguments are tokens the parse does not read as code, and
		// no call the structural queries capture lies inside one.
	case s.structExpression:
		return k.structLiteral(n)
	case s.assignmentExpression:
		left := n.ChildByFieldId(s.fLeft)
		k.walk(left)
		field := ""
		if !left.IsNull() && left.KindId() == s.fieldExpression {
			if f := k.kid(left, s.fField); !f.IsNull() && f.KindId() == s.fieldIdentifier {
				field = k.str(f)
			}
		}
		t := k.value(n.ChildByFieldId(s.fRight), field)
		if !left.IsNull() && left.KindId() == s.identifier {
			if i := k.lookup(left); i >= 0 {
				k.locals[i].typ = t
			}
		}
	case s.parenthesizedExpression:
		return k.walk(firstNamed(n))
	case s.referenceExpression:
		return k.walk(n.ChildByFieldId(s.fValue))
	case s.typeCastExpression:
		k.walk(n.ChildByFieldId(s.fValue))
	case s.constItem, s.staticItem:
		k.value(n.ChildByFieldId(s.fValue), "")
	case s.returnExpression, s.arrayExpression, s.tupleExpression:
		for c := n.first(); !c.IsNull(); c = c.next() {
			if c.IsNamed() && !c.IsExtra() {
				k.value(c, "")
			}
		}
	case s.identifier:
		if i := k.lookup(n); i >= 0 {
			return k.locals[i].typ
		}
	case s.self:
		if self := k.self(); self != "" {
			return wire.Type{Kind: wire.TypeNamed, Name: self}
		}
	case s.fieldExpression:
		recv := k.walk(n.ChildByFieldId(s.fValue))
		f := k.kid(n, s.fField)
		if recv.Kind == wire.TypeNamed && !f.IsNull() && f.KindId() == s.fieldIdentifier {
			if fl, ok := k.fields[recv.Name+"\x00"+view(textOf(k.src, f))]; ok {
				return wire.Type{Kind: wire.TypeNamed, Name: fl.typ}
			}
		}
	default:
		k.children(n)
	}
	return wire.Type{}
}

// children walks n's children.
func (k *rustLinker) children(n Node) {
	for c := n.first(); !c.IsNull(); c = c.next() {
		if !c.IsExtra() || c.IsError() {
			k.walk(c)
		}
	}
}

// functionItem records a function's export and walks its body with its type
// parameters and parameters in scope and its enclosing function's bindings
// out of it.
func (k *rustLinker) functionItem(n Node) {
	s := k.k
	k.export(n)
	member := k.member(n)
	if !member {
		k.owners = append(k.owners, rsOwner{})
	}
	locals, base, generics := len(k.locals), k.base, len(k.generics)
	k.base = locals
	k.typeParams(n)
	if ps := n.ChildByFieldId(s.fParameters); !ps.IsNull() {
		for p := ps.first(); !p.IsNull(); p = p.next() {
			if p.KindId() == s.parameter {
				k.param(p)
			}
		}
	}
	k.walk(n.ChildByFieldId(s.fBody))
	k.locals, k.base, k.generics = k.locals[:locals], base, k.generics[:generics]
	if !member {
		k.owners = k.owners[:len(k.owners)-1]
	}
}

// member reports a function item or signature declared in an impl or trait
// body, whose `Self` and receiver are the innermost owner.
func (k *rustLinker) member(n Node) bool {
	p := n.Parent()
	if p.IsNull() || p.KindId() != k.k.declarationList {
		return false
	}
	o := p.Parent()
	return !o.IsNull() && (o.KindId() == k.k.implItem || o.KindId() == k.k.traitItem)
}

// export records the Export of a function item or signature whose name is
// an identifier, the declarations the structural queries capture. A self
// parameter is the receiver, never one of Params, and its presence is what
// makes the callable not Static; a path call `T::m(x, a)` of such a method
// passes the receiver x as its first argument. A foreign function's `...`
// is one of Params, with Variadic set.
func (k *rustLinker) export(n Node) {
	s := k.k
	name := n.ChildByFieldId(s.fName)
	if name.IsNull() || name.KindId() != s.identifier {
		return
	}
	x := wire.Export{Name: wire.Span{Start: uint32(name.StartByte()), End: uint32(name.EndByte())}, Static: true}
	inTrait := false
	if k.member(n) {
		x.Receiver = k.self()
		inTrait = n.Parent().Parent().KindId() == s.traitItem
		if !inTrait && len(k.owners) > 0 {
			o := k.owners[len(k.owners)-1]
			x.Override, x.Trait = o.traitImpl, o.trait
		}
	}
	x.Abstract = inTrait && n.KindId() == s.functionSignatureItem
	ps := n.ChildByFieldId(s.fParameters)
	ret := k.kid(n, s.fReturnType)
	if !ps.IsNull() {
		for p := ps.first(); !p.IsNull(); p = p.next() {
			switch {
			case k.isSelf(p):
				x.Static = false
			case p.KindId() == s.variadicParameter:
				x.Params++
				x.Variadic = true
			case !k.paramType(p).IsNull():
				x.Params++
			}
		}
	}
	x.Signature = k.signature(ps, ret)
	if !ret.IsNull() {
		// A smart pointer stays in the result type as written, since a
		// method the pointer answers itself is called on the pointer, not
		// on what it owns; the linker removes it for every other method.
		inner, ptr := k.unwrap(ret)
		t := k.typeName(inner)
		if ptr {
			outer := ret
			for outer.KindId() == s.referenceType || outer.KindId() == s.pointerType {
				outer = outer.ChildByFieldId(s.fType)
			}
			b := k.spell(k.buf[:0], outer)
			k.buf, t = b, string(b)
		}
		if t != "" {
			x.Returns = wire.Type{Kind: wire.TypeNamed, Name: t}
		}
	}
	k.out.Exports = append(k.out.Exports, x)
	k.parameters(x.Name, ps)
}

// parameters appends a Parameter for each parameter of the list ps of the
// callable named callable that one name declares, at the token the lowering
// declares its variable by: the self parameter's `self` at 0, then each
// parameter Params counts, from 1. A pattern of several names, an anonymous
// parameter and a `...` take their positions and are not sent.
func (k *rustLinker) parameters(callable wire.Span, ps Node) {
	s := k.k
	if ps.IsNull() {
		return
	}
	add := func(name Node, index uint32) {
		k.out.Parameters = append(k.out.Parameters, wire.Parameter{Callable: callable,
			Name: wire.Span{Start: uint32(name.StartByte()), End: uint32(name.EndByte())}, Index: index})
	}
	var index uint32
	for p := ps.first(); !p.IsNull(); p = p.next() {
		switch {
		case p.KindId() == s.selfParameter:
			if self := kindChild(p, s.self); !self.IsNull() {
				add(self, 0)
			}
		case k.isSelf(p):
			add(p.ChildByFieldId(s.fPattern), 0)
		case p.KindId() == s.variadicParameter:
			index++
		case !k.paramType(p).IsNull():
			index++
			if p.KindId() == s.parameter {
				if pat := p.ChildByFieldId(s.fPattern); !pat.IsNull() && pat.KindId() == s.identifier {
					add(pat, index)
				}
			}
		}
	}
}

// isSelf reports a method's self parameter: `self`, `&mut self`, or
// `self: Box<Self>`.
func (k *rustLinker) isSelf(p Node) bool {
	s := k.k
	if p.KindId() == s.selfParameter {
		return true
	}
	if p.KindId() != s.parameter {
		return false
	}
	pat := p.ChildByFieldId(s.fPattern)
	return !pat.IsNull() && pat.KindId() == s.self
}

// paramType is the type a parameter list entry declares: a parameter's type,
// or the entry itself in a function type's list of bare types; null for a
// self parameter, a variadic parameter, an attribute or a comment.
func (k *rustLinker) paramType(p Node) Node {
	s := k.k
	switch {
	case !p.IsNamed() || p.IsExtra() || k.isSelf(p):
		return Node{}
	case p.KindId() == s.parameter:
		return p.ChildByFieldId(s.fType)
	case p.KindId() == s.variadicParameter, p.KindId() == s.attributeItem:
		return Node{}
	}
	return p
}

// signature spells a parameter list ps and result type ret (either null) by
// the rule of wire.Export.Signature. A method's self parameter is spelled
// first, as the type it declares (selfType): a Rust method is taken as a
// value only through its path, `Foo::m`, whose function type takes the
// receiver first (the Rust Reference's "Associated Items"), so its
// signature joins the indirect sites of type `fn(&Foo, …)`.
func (k *rustLinker) signature(ps, ret Node) string {
	b := append(k.buf[:0], '(')
	first := true
	if !ps.IsNull() {
		for p := ps.first(); !p.IsNull(); p = p.next() {
			if k.isSelf(p) {
				if !first {
					b = append(b, ',')
				}
				b, first = k.selfType(b, p), false
				continue
			}
			t := k.paramType(p)
			if p.KindId() == k.k.variadicParameter {
				if !first {
					b = append(b, ',')
				}
				b, first = append(b, "..."...), false
				continue
			}
			if t.IsNull() {
				continue
			}
			if !first {
				b = append(b, ',')
			}
			b, first = k.spell(b, t), false
		}
	}
	b = append(b, ")("...)
	if !ret.IsNull() {
		b = k.spell(b, ret)
	}
	b = append(b, ')')
	k.buf = b
	return string(b)
}

// spell appends the tokens of the type t to b: comments and lifetimes left
// out, `Self` spelled as its owner, one space between two word tokens.
func (k *rustLinker) spell(b []byte, t Node) []byte {
	s := k.k
	c := t.Walk()
	for {
		n := c.Node()
		descend := true
		switch {
		case n.IsExtra() || n.KindId() == s.lifetime:
			descend = false
		case n.first().IsNull():
			w := textOf(k.src, n)
			if n.KindId() == s.typeIdentifier && string(w) == "Self" {
				if self := k.self(); self != "" {
					w = []byte(self)
				}
			}
			if len(w) > 0 && len(b) > 0 && rsWord(b[len(b)-1]) && rsWord(w[0]) {
				b = append(b, ' ')
			}
			b = append(b, w...)
		}
		if descend && c.GotoFirstChild() {
			continue
		}
		for !c.GotoNextSibling() {
			if !c.GotoParent() {
				return b
			}
		}
	}
}

// selfType appends the type a method's self parameter declares, `Self`
// spelled as its owner: `self` and `mut self` are Foo, `&self` &Foo, `&mut
// self` &mut Foo, `self: Box<Self>` Box<Foo>.
func (k *rustLinker) selfType(b []byte, p Node) []byte {
	s := k.k
	if p.KindId() == s.parameter {
		return k.spell(b, p.ChildByFieldId(s.fType))
	}
	self := k.self()
	if self == "" {
		self = "Self"
	}
	ref := false
	for c := p.first(); !c.IsNull(); c = c.next() {
		switch w := string(textOf(k.src, c)); {
		case c.IsExtra() || c.KindId() == s.lifetime:
		case w == "&":
			ref, b = true, append(b, '&')
		case w == "mut" && ref:
			b = append(b, "mut"...)
		case c.KindId() == s.self:
			if len(b) > 0 && rsWord(b[len(b)-1]) {
				b = append(b, ' ')
			}
			b = append(b, self...)
		}
	}
	return b
}

// rsWord reports a byte of an identifier or keyword.
func rsWord(b byte) bool {
	return b == '_' || b >= '0' && b <= '9' || b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z' || b >= 0x80
}

// implItem records an `impl Trait for Type` as a supertype and walks the
// impl's body with its self type, and its trait, as the owner.
func (k *rustLinker) implItem(n Node) {
	s := k.k
	generics := len(k.generics)
	k.typeParams(n)
	self := ""
	if t := n.ChildByFieldId(s.fType); !t.IsNull() {
		self = k.typeName(t)
	}
	tr, trait := k.kid(n, s.fTrait), ""
	if !tr.IsNull() {
		trait = k.typeName(tr)
	}
	if trait != "" && self != "" && k.generic(self) < 0 {
		k.out.Supertypes = append(k.out.Supertypes, wire.Supertype{Type: self, Super: trait})
	}
	k.owners = append(k.owners, rsOwner{self: self, traitImpl: !tr.IsNull(), trait: trait})
	k.walk(n.ChildByFieldId(s.fBody))
	k.owners = k.owners[:len(k.owners)-1]
	k.generics = k.generics[:generics]
}

// traitItem records a trait's supertraits, from its bounds and its where
// clause's bounds on `Self`, and walks its body with the trait as the owner.
func (k *rustLinker) traitItem(n Node) {
	s := k.k
	name := n.ChildByFieldId(s.fName)
	if name.IsNull() {
		return
	}
	trait := k.str(name)
	super := func(bounds Node) {
		for b := bounds.first(); !b.IsNull(); b = b.next() {
			if !b.IsNamed() || b.IsExtra() || b.KindId() == s.lifetime {
				continue
			}
			if t := k.typeName(b); t != "" {
				k.out.Supertypes = append(k.out.Supertypes, wire.Supertype{Type: trait, Super: t})
			}
		}
	}
	k.owners = append(k.owners, rsOwner{self: trait})
	if b := k.kid(n, s.fBounds); !b.IsNull() {
		super(b)
	}
	for c := n.first(); !c.IsNull(); c = c.next() {
		if c.KindId() != s.whereClause {
			continue
		}
		for p := c.first(); !p.IsNull(); p = p.next() {
			if p.KindId() != s.wherePredicate {
				continue
			}
			left, bounds := p.ChildByFieldId(s.fLeft), k.kid(p, s.fBounds)
			if !left.IsNull() && !bounds.IsNull() && left.KindId() == s.typeIdentifier && string(textOf(k.src, left)) == "Self" {
				super(bounds)
			}
		}
	}
	generics := len(k.generics)
	k.typeParams(n)
	k.walk(n.ChildByFieldId(s.fBody))
	k.generics = k.generics[:generics]
	k.owners = k.owners[:len(k.owners)-1]
}

// typeParams brings an item's type parameters into scope with their bounds,
// from its parameter list and its where clause.
func (k *rustLinker) typeParams(n Node) {
	s := k.k
	from := len(k.generics)
	if tps := k.kid(n, s.fTypeParameters); !tps.IsNull() {
		for tp := tps.first(); !tp.IsNull(); tp = tp.next() {
			if tp.KindId() != s.typeParameter {
				continue
			}
			name := tp.ChildByFieldId(s.fName)
			if name.IsNull() {
				continue
			}
			k.generics = append(k.generics, rsGeneric{name: k.str(name)})
			if b := k.kid(tp, s.fBounds); !b.IsNull() {
				k.bound(len(k.generics)-1, b)
			}
		}
	}
	for c := n.first(); !c.IsNull(); c = c.next() {
		if c.KindId() != s.whereClause {
			continue
		}
		for p := c.first(); !p.IsNull(); p = p.next() {
			if p.KindId() != s.wherePredicate {
				continue
			}
			left, bounds := p.ChildByFieldId(s.fLeft), k.kid(p, s.fBounds)
			if left.IsNull() || bounds.IsNull() || left.KindId() != s.typeIdentifier {
				continue
			}
			if i := k.generic(view(textOf(k.src, left))); i >= from {
				k.bound(i, bounds)
			}
		}
	}
}

// bound adds the trait bounds b to the type parameter i.
func (k *rustLinker) bound(i int, b Node) {
	s := k.k
	g := &k.generics[i]
	for t := b.first(); !t.IsNull(); t = t.next() {
		if !t.IsNamed() || t.IsExtra() || t.KindId() == s.lifetime {
			continue
		}
		if t.KindId() == s.higherRankedTraitBound {
			t = t.ChildByFieldId(s.fType)
		}
		if t.IsNull() {
			continue
		}
		if t.KindId() == s.functionType {
			g.sig = k.signature(t.ChildByFieldId(s.fParameters), k.kid(t, s.fReturnType))
			continue
		}
		if name := k.typeName(t); name != "" {
			g.bounds++
			g.bound = name
		}
	}
}

// generic is the index of the innermost type parameter named name, or -1.
func (k *rustLinker) generic(name string) int {
	for i := len(k.generics) - 1; i >= 0; i-- {
		if k.generics[i].name == name {
			return i
		}
	}
	return -1
}

// param binds a parameter's names, a single name with its declared type.
func (k *rustLinker) param(p Node) {
	s := k.k
	pat, t := p.ChildByFieldId(s.fPattern), p.ChildByFieldId(s.fType)
	if pat.IsNull() || pat.KindId() == s.self {
		return
	}
	if pat.KindId() != s.identifier || t.IsNull() {
		k.bindAll(pat)
		return
	}
	typ, ptr := k.declared(t)
	k.locals = append(k.locals, rsLocal{name: view(textOf(k.src, pat)), typ: typ, ptr: ptr, sig: k.fnSig(t)})
}

// declared is the type a value of the declared type t carries (unwrap): the
// type it names, or the one trait a type parameter's bounds name; and
// whether a smart pointer owns that value.
func (k *rustLinker) declared(t Node) (wire.Type, bool) {
	t, ptr := k.unwrap(t)
	if t.IsNull() {
		return wire.Type{}, ptr
	}
	if t.KindId() == k.k.typeIdentifier {
		if i := k.generic(view(textOf(k.src, t))); i >= 0 {
			if g := k.generics[i]; g.bounds == 1 {
				return wire.Type{Kind: wire.TypeNamed, Name: g.bound}, ptr
			}
			return wire.Type{}, ptr
		}
	}
	if name := k.typeName(t); name != "" {
		return wire.Type{Kind: wire.TypeNamed, Name: name}, ptr
	}
	return wire.Type{}, ptr
}

// unwrap is the type t declares a value of once references, raw pointers
// and smart pointers (lang.RustPointer) are removed (`&Rc<Foo>` is Foo), and
// whether a smart pointer was removed.
func (k *rustLinker) unwrap(t Node) (Node, bool) {
	s := k.k
	ptr := false
	for !t.IsNull() {
		switch t.KindId() {
		case s.referenceType, s.pointerType:
			t = t.ChildByFieldId(s.fType)
			continue
		case s.genericType:
			args := k.kid(t, s.fTypeArguments)
			if !args.IsNull() && args.NamedChildCount() == 1 && lang.RustPointer(k.typeName(t.ChildByFieldId(s.fType))) {
				t, ptr = firstNamed(args), true
				continue
			}
		}
		return t, ptr
	}
	return t, ptr
}

// pointer reports that the receiver expression n holds a smart pointer
// (lang.RustPointer) by its declaration: a binding, or a field of a binding or of
// `self`, declared as one.
func (k *rustLinker) pointer(n Node) bool {
	s := k.k
	switch n.KindId() {
	case s.identifier:
		i := k.lookup(n)
		return i >= 0 && k.locals[i].ptr
	case s.fieldExpression:
		v, f := n.ChildByFieldId(s.fValue), k.kid(n, s.fField)
		if v.IsNull() || f.IsNull() || f.KindId() != s.fieldIdentifier {
			return false
		}
		owner := ""
		switch v.KindId() {
		case s.self:
			owner = k.self()
		case s.identifier:
			if i := k.lookup(v); i >= 0 && k.locals[i].typ.Kind == wire.TypeNamed {
				owner = k.locals[i].typ.Name
			}
		}
		return owner != "" && k.fields[owner+"\x00"+view(textOf(k.src, f))].ptr
	}
	return false
}

// fnSig is the signature of the function type a value of the declared type
// t is called through, or empty when t declares none: a function pointer
// type, a function trait (`impl Fn(i32)`, `&dyn FnMut()`), a type parameter
// bounded by one, or a single-argument wrapper of one (`Box<dyn Fn()>`),
// which a call reaches through its dereference.
func (k *rustLinker) fnSig(t Node) string {
	s := k.k
	for !t.IsNull() {
		switch t.KindId() {
		case s.functionType:
			return k.signature(t.ChildByFieldId(s.fParameters), k.kid(t, s.fReturnType))
		case s.referenceType, s.pointerType, s.higherRankedTraitBound:
			t = t.ChildByFieldId(s.fType)
		case s.dynamicType, s.abstractType:
			t = k.kid(t, s.fTrait)
		case s.genericType:
			args := k.kid(t, s.fTypeArguments)
			if args.IsNull() || args.NamedChildCount() != 1 {
				return ""
			}
			t = firstNamed(args)
		case s.typeIdentifier:
			if i := k.generic(view(textOf(k.src, t))); i >= 0 {
				return k.generics[i].sig
			}
			return ""
		default:
			return ""
		}
	}
	return ""
}

// let walks a let declaration's value and binds its pattern: a single name
// with its declared type, or else the type its value carries.
func (k *rustLinker) let(n Node) {
	s := k.k
	t := k.value(n.ChildByFieldId(s.fValue), "")
	if alt := n.ChildByFieldId(s.fAlternative); !alt.IsNull() {
		k.walk(alt)
	}
	pat, declared := n.ChildByFieldId(s.fPattern), n.ChildByFieldId(s.fType)
	if pat.IsNull() {
		return
	}
	if pat.KindId() != s.identifier {
		k.bindAll(pat)
		return
	}
	l := rsLocal{name: view(textOf(k.src, pat)), typ: t}
	if !declared.IsNull() {
		d, ptr := k.declared(declared)
		if d.Kind != wire.TypeUnknown {
			l.typ = d
		}
		l.ptr = ptr
		l.sig = k.fnSig(declared)
	}
	k.locals = append(k.locals, l)
}

// bindAll binds every name the pattern p binds, with no type: each
// identifier and shorthand field of it except the paths it matches against
// (`Some` in `Some(x)`).
func (k *rustLinker) bindAll(p Node) {
	s := k.k
	if p.IsNull() {
		return
	}
	c := p.Walk()
	for {
		n := c.Node()
		descend := true
		switch {
		case n.IsExtra() || n.KindId() == s.scopedIdentifier || (n != p && uint16(n.rec().field) == s.fType):
			descend = false
		case n.KindId() == s.identifier, n.KindId() == s.shorthandFieldIdentifier:
			k.locals = append(k.locals, rsLocal{name: view(textOf(k.src, n))})
		}
		if descend && c.GotoFirstChild() {
			continue
		}
		for !c.GotoNextSibling() {
			if !c.GotoParent() {
				return
			}
		}
	}
}

// lookup is the index of the binding the identifier n names, or -1.
func (k *rustLinker) lookup(n Node) int {
	name := view(textOf(k.src, n))
	for i := len(k.locals) - 1; i >= k.base; i-- {
		if k.locals[i].name == name {
			return i
		}
	}
	return -1
}

// value walks an expression in a value position (an argument, a let or
// assignment's value, a field initializer, a returned or collected element)
// and returns the type it carries. A function or method path there that
// names no binding is taken as a value: it is recorded as a Value, with the
// field it is written to, and carries TypeFunction.
func (k *rustLinker) value(n Node, field string) wire.Type {
	s := k.k
	if n.IsNull() {
		return wire.Type{}
	}
	u := n
	for !u.IsNull() {
		switch u.KindId() {
		case s.parenthesizedExpression:
			u = firstNamed(u)
			continue
		case s.referenceExpression, s.typeCastExpression:
			u = u.ChildByFieldId(s.fValue)
			continue
		}
		break
	}
	if !u.IsNull() {
		if name := k.path(u); name != "" {
			t := wire.Type{Kind: wire.TypeFunction, Name: name}
			k.out.Values = append(k.out.Values, wire.Value{At: wire.Span{Start: uint32(u.StartByte()), End: uint32(u.EndByte())}, Field: field, Type: t})
			return t
		}
	}
	return k.walk(n)
}

// path is the name a function or method path expression is written with,
// `Self` spelled as its owner, or empty when n is not one: a name no binding
// in scope holds, a scoped path `T::m`, either with turbofish arguments.
func (k *rustLinker) path(n Node) string {
	s := k.k
	if n.KindId() == s.genericFunction {
		n = n.ChildByFieldId(s.fFunction)
		if n.IsNull() {
			return ""
		}
	}
	switch n.KindId() {
	case s.identifier:
		if k.lookup(n) >= 0 {
			return ""
		}
		return k.str(n)
	case s.scopedIdentifier:
		name := n.ChildByFieldId(s.fName)
		if name.IsNull() || name.KindId() != s.identifier {
			return ""
		}
		q := k.qualifier(n)
		if q == "" {
			return k.str(name)
		}
		return q + "::" + k.str(name)
	}
	return ""
}

// qualifier is the path a scoped identifier's name is qualified by, as
// written: `Self` as its owner, a bracketed `<T as Trait>` as `T`, a
// generic path without its arguments; empty for `::f`.
func (k *rustLinker) qualifier(n Node) string {
	s := k.k
	p := k.kid(n, s.fPath)
	if p.IsNull() {
		return ""
	}
	switch p.KindId() {
	case s.identifier:
		if string(textOf(k.src, p)) == "Self" {
			if self := k.self(); self != "" {
				return self
			}
		}
		return k.str(p)
	case s.bracketedType, s.genericType:
		return k.typeName(p)
	}
	b := k.spell(k.buf[:0], p)
	k.buf = b
	return string(b)
}

// call records a call expression's site when its callee is one the
// structural queries capture (`f()`, `f::<T>()`, `x.m()`, `T::f()`), after
// the sites inside its callee and arguments, and returns its result.
func (k *rustLinker) call(n Node) wire.Type {
	s := k.k
	fn, args := n.ChildByFieldId(s.fFunction), n.ChildByFieldId(s.fArguments)
	site := wire.Site{}
	var name Node
	switch {
	case fn.IsNull():
	case fn.KindId() == s.identifier:
		name = fn
		if i := k.lookup(fn); i >= 0 {
			site.Shape, site.Receiver, site.Signature = wire.ShapeIndirect, k.locals[i].typ, k.locals[i].sig
		} else {
			site.Shape = wire.ShapeBare
		}
	case fn.KindId() == s.genericFunction:
		if f := fn.ChildByFieldId(s.fFunction); !f.IsNull() && f.KindId() == s.identifier {
			name, site.Shape = f, wire.ShapeBare
		} else {
			k.walk(fn)
		}
	case fn.KindId() == s.fieldExpression:
		value := fn.ChildByFieldId(s.fValue)
		recv := k.walk(value)
		if f := k.kid(fn, s.fField); !f.IsNull() && f.KindId() == s.fieldIdentifier {
			name, site.Shape, site.Receiver = f, wire.ShapeMember, recv
			if !value.IsNull() && lang.RustPointerMethod(view(textOf(k.src, f))) && k.pointer(value) {
				// The smart pointer's own method, which no table declares.
				site.Receiver = wire.Type{}
			}
		}
	case fn.KindId() == s.scopedIdentifier:
		if f := fn.ChildByFieldId(s.fName); !f.IsNull() && f.KindId() == s.identifier {
			name, site.Shape, site.Qualifier = f, wire.ShapeStatic, k.qualifier(fn)
		}
	default:
		k.walk(fn)
	}
	if !args.IsNull() {
		for a := args.first(); !a.IsNull(); a = a.next() {
			if !a.IsNamed() || a.IsExtra() || a.KindId() == s.attributeItem {
				continue
			}
			t := k.value(a, "")
			if !name.IsNull() {
				site.Args = append(site.Args, t)
				site.Arguments = append(site.Arguments, wire.Argument{At: wire.Span{Start: uint32(a.StartByte()), End: uint32(a.EndByte())},
					Reads: k.reads().argReads(a, k.readName, k.readSkip, nil)})
			}
		}
	}
	if name.IsNull() {
		return wire.Type{}
	}
	site.Name = wire.Span{Start: uint32(name.StartByte()), End: uint32(name.EndByte())}
	k.out.Sites = append(k.out.Sites, site)
	return wire.Type{Kind: wire.TypeResult, Index: uint32(len(k.out.Sites) - 1)}
}

// readName reports a name an argument reads: an identifier or `self`. A
// segment of a scoped path (`a::b`) names an item; readSkip leaves it out.
func (k *rustLinker) readName(n Node) bool {
	return n.KindId() == k.k.identifier || n.KindId() == k.k.self
}

// readSkip reports a subtree whose names an argument does not read: a call's
// or a macro's result, a closure, whose body runs later, and a scoped path.
func (k *rustLinker) readSkip(n Node) bool {
	switch n.KindId() {
	case k.k.callExpression, k.k.macroInvocation, k.k.closureExpression, k.k.scopedIdentifier:
		return true
	}
	return false
}

// structLiteral walks a struct expression, recording each function its
// fields are initialized with, and returns the struct's type.
func (k *rustLinker) structLiteral(n Node) wire.Type {
	s := k.k
	if body := n.ChildByFieldId(s.fBody); !body.IsNull() {
		for f := body.first(); !f.IsNull(); f = f.next() {
			switch f.KindId() {
			case s.fieldInitializer:
				field := ""
				if fi := k.kid(f, s.fField); !fi.IsNull() && fi.KindId() == s.fieldIdentifier {
					field = k.str(fi)
				}
				k.value(f.ChildByFieldId(s.fValue), field)
			case s.shorthandFieldInitializer:
				for id := f.first(); !id.IsNull(); id = id.next() {
					if id.KindId() == s.identifier {
						k.value(id, k.str(id))
					}
				}
			default:
				if f.IsNamed() && !f.IsExtra() {
					k.walk(f)
				}
			}
		}
	}
	if name := n.ChildByFieldId(s.fName); !name.IsNull() {
		if t := k.typeName(name); t != "" {
			return wire.Type{Kind: wire.TypeNamed, Name: t}
		}
	}
	return wire.Type{}
}

// typeName is the name of the type t denotes, spelled as written without
// references, pointers, `dyn`, `impl` or generic arguments, `Self` as its
// owner; empty for a type that names no declaration (a tuple, an array, a
// function type, several bounds).
func (k *rustLinker) typeName(t Node) string {
	s := k.k
	for !t.IsNull() {
		switch t.KindId() {
		case s.typeIdentifier, s.identifier:
			if string(textOf(k.src, t)) == "Self" {
				if self := k.self(); self != "" {
					return self
				}
			}
			return k.str(t)
		case s.primitiveType:
			return k.str(t)
		case s.scopedTypeIdentifier, s.scopedIdentifier:
			b := k.spell(k.buf[:0], t)
			k.buf = b
			return string(b)
		case s.genericType, s.genericTypeWithTurbofish, s.referenceType, s.pointerType, s.qualifiedType,
			s.higherRankedTraitBound:
			t = t.ChildByFieldId(s.fType)
		case s.bracketedType:
			t = firstNamed(t)
		case s.dynamicType, s.abstractType:
			t = k.kid(t, s.fTrait)
		default:
			return ""
		}
	}
	return ""
}

// self is the name `Self` means here, or empty.
func (k *rustLinker) self() string {
	if len(k.owners) == 0 {
		return ""
	}
	return k.owners[len(k.owners)-1].self
}

// kid is n's first child under the field id, read from the child's own
// field: for the fields the lowering's registry does not list, which
// ChildByFieldId does not answer.
func (k *rustLinker) kid(n Node, field uint16) Node {
	for c := n.first(); !c.IsNull(); c = c.next() {
		if uint16(c.rec().field) == field {
			return c
		}
	}
	return Node{}
}

// str is n's text as a string the tables may keep.
func (k *rustLinker) str(n Node) string { return string(textOf(k.src, n)) }

// rsLinkSyntax holds the kind and field ids the Rust linker matches,
// resolved once by name against the pinned grammar. A name the grammar does
// not define panics, so a misspelt kind can never silently match nothing.
type rsLinkSyntax struct {
	functionItem, functionSignatureItem, implItem, traitItem, declarationList, closureExpression, parameter,
	selfParameter, variadicParameter, attributeItem, self, identifier, block, ifExpression, whileExpression,
	letDeclaration, letCondition, forExpression, matchArm, callExpression, macroInvocation, tokenTree,
	useDeclaration, structExpression, fieldInitializer, shorthandFieldInitializer, assignmentExpression,
	referenceExpression, parenthesizedExpression, typeCastExpression, returnExpression, arrayExpression,
	tupleExpression, fieldExpression, fieldIdentifier, scopedIdentifier, genericFunction, structItem,
	fieldDeclarationList, fieldDeclaration, typeIdentifier, primitiveType, scopedTypeIdentifier, genericType,
	genericTypeWithTurbofish, referenceType, pointerType, qualifiedType, bracketedType, dynamicType,
	abstractType, functionType, lifetime, typeParameter, whereClause, wherePredicate,
	shorthandFieldIdentifier, higherRankedTraitBound, constItem, staticItem uint16

	// Fields the lowering's registry lists, which ChildByFieldId answers.
	fAlternative, fArguments, fBody, fCondition, fFunction, fLeft, fName, fParameters, fPattern, fRight, fType,
	fValue uint16
	// Fields it does not list, read from each child's own field (kid).
	fBounds, fField, fPath, fReturnType, fTrait, fTypeArguments, fTypeParameters uint16
}

var (
	rsLinkSyntaxOnce  sync.Once
	rsLinkSyntaxTable *rsLinkSyntax
)

func rsLinkSyntaxOf() *rsLinkSyntax {
	rsLinkSyntaxOnce.Do(func() {
		const language = "rust"
		tl := mustGrammar(language)
		kind := func(name string) uint16 { return mustKind(tl, language, name, true) }
		field := func(name string) uint16 { return mustField(tl, language, name) }
		s := &rsLinkSyntax{}
		for p, name := range map[*uint16]string{
			&s.functionItem: "function_item", &s.functionSignatureItem: "function_signature_item",
			&s.implItem: "impl_item", &s.traitItem: "trait_item", &s.declarationList: "declaration_list",
			&s.closureExpression: "closure_expression", &s.parameter: "parameter",
			&s.selfParameter: "self_parameter", &s.variadicParameter: "variadic_parameter",
			&s.attributeItem: "attribute_item", &s.self: "self", &s.identifier: "identifier", &s.block: "block",
			&s.ifExpression: "if_expression", &s.whileExpression: "while_expression",
			&s.letDeclaration: "let_declaration", &s.letCondition: "let_condition",
			&s.forExpression: "for_expression", &s.matchArm: "match_arm", &s.callExpression: "call_expression",
			&s.macroInvocation: "macro_invocation", &s.tokenTree: "token_tree",
			&s.useDeclaration: "use_declaration", &s.structExpression: "struct_expression",
			&s.fieldInitializer: "field_initializer", &s.shorthandFieldInitializer: "shorthand_field_initializer",
			&s.assignmentExpression: "assignment_expression", &s.referenceExpression: "reference_expression",
			&s.parenthesizedExpression: "parenthesized_expression", &s.typeCastExpression: "type_cast_expression",
			&s.returnExpression: "return_expression", &s.arrayExpression: "array_expression",
			&s.tupleExpression: "tuple_expression", &s.fieldExpression: "field_expression",
			&s.fieldIdentifier: "field_identifier", &s.scopedIdentifier: "scoped_identifier",
			&s.genericFunction: "generic_function", &s.structItem: "struct_item",
			&s.fieldDeclarationList: "field_declaration_list", &s.fieldDeclaration: "field_declaration",
			&s.typeIdentifier: "type_identifier", &s.primitiveType: "primitive_type",
			&s.scopedTypeIdentifier: "scoped_type_identifier", &s.genericType: "generic_type",
			&s.genericTypeWithTurbofish: "generic_type_with_turbofish", &s.referenceType: "reference_type",
			&s.pointerType: "pointer_type", &s.qualifiedType: "qualified_type", &s.bracketedType: "bracketed_type",
			&s.dynamicType: "dynamic_type", &s.abstractType: "abstract_type", &s.functionType: "function_type",
			&s.lifetime: "lifetime", &s.typeParameter: "type_parameter", &s.whereClause: "where_clause",
			&s.wherePredicate: "where_predicate", &s.shorthandFieldIdentifier: "shorthand_field_identifier",
			&s.higherRankedTraitBound: "higher_ranked_trait_bound", &s.constItem: "const_item",
			&s.staticItem: "static_item",
		} {
			*p = kind(name)
		}
		for p, name := range map[*uint16]string{
			&s.fAlternative: "alternative", &s.fArguments: "arguments", &s.fBody: "body",
			&s.fCondition: "condition", &s.fFunction: "function", &s.fLeft: "left", &s.fName: "name",
			&s.fParameters: "parameters", &s.fPattern: "pattern", &s.fRight: "right", &s.fType: "type",
			&s.fValue: "value",
		} {
			*p = field(name)
		}
		for p, name := range map[*uint16]string{
			&s.fBounds: "bounds", &s.fField: "field", &s.fPath: "path", &s.fReturnType: "return_type",
			&s.fTrait: "trait", &s.fTypeArguments: "type_arguments", &s.fTypeParameters: "type_parameters",
		} {
			*p = rsAnyField(tl, language, name)
		}
		rsLinkSyntaxTable = s
	})
	return rsLinkSyntaxTable
}

// rsAnyField is the id of the field name in tl, whether or not the
// lowering's registry lists it; a name the grammar does not define panics.
func rsAnyField(tl *ts.Language, language, name string) uint16 {
	v := tl.FieldIdForName(name)
	if v == 0 {
		panic("worker: the " + language + " grammar has no field " + name)
	}
	return v
}
