package worker

import (
	"slices"
	"strings"
	"sync"

	ts "github.com/tree-sitter/go-tree-sitter"

	"github.com/Sawmonabo/codectx/internal/provider/treesitter/flow"
	"github.com/Sawmonabo/codectx/internal/provider/treesitter/wire"
)

// cLinker builds the link tables of one C or C++ (language is "c" or "cpp")
// file. Both languages declare every type a call needs, so the tables are
// read from declarations alone, in one pass over the flat tree in file: a
// prepass collects the file-scope names, records and typedefs, and the walk
// tracks the block-scoped locals of each callable as it goes.
//
// What each table holds:
//
//   - Exports: every function definition, every C++ member function
//     defined without a body (pure virtual, which is Abstract, defaulted or
//     deleted), every prototype the structural extraction captures (a
//     declaration at file scope, cLinkSyntax.fileScope: under the
//     translation unit, a C++ namespace body or class body, through any
//     preprocessor conditional arm, linkage specification or C++ template,
//     and a C++ member declaration) and every function-like macro, keyed by
//     the token the extraction names the declaration by. Each says how many
//     arguments fit it (cArity), whether a namespace-scope function has
//     internal linkage (declared `static` anywhere in the file, or in an
//     unnamed namespace) and whether a C++ member is virtual or final as its
//     declaration says. A class, struct, union or enum definition in an
//     unnamed namespace is exported too, with Internal alone.
//   - Supertypes: each base of a C++ class, struct or union.
//   - Sites: every call the structural queries capture, keyed by its callee
//     token: `f()` (bare, or indirect when f is a local, a parameter, a
//     file-scope object or a data member of the enclosing class),
//     `x.f()`/`x->f()` (member, or indirect through a data member: always in
//     C, which has no member functions), `N::f()` (static), `f<T>()`
//     (bare), `(*fp)()` (indirect through fp, bare when fp names a
//     function), `(*x->f)()` and `(*x.f)()` (indirect through the data
//     member f) and `tbl[i]()` (always indirect, typed by the element type
//     tbl declares). A call is listed after the calls inside it, so the call an
//     argument or a receiver is the result of has the lower index.
//   - Values: every function taken as a value: `&f` and `&C::m`, and a name
//     in a position that stores or passes its value (an argument, an
//     initializer, a member's default value, the right side of `=`, a
//     return value) that no local, member, object, constant or type of the
//     file declares, with the field it is written to by `x.f = g`,
//     `x->f = g`, `{ .f = g }` or a member declared `= g`.
//
// Spelling. A type name in a Receiver, Supertype, Qualifier or TypeNamed is
// the name as written with every template argument list dropped (`V<T>` is
// V, `ns::V<T>` is ns::V): the declaration a call binds to is named without
// them. An in-class member's Receiver is the chain of its enclosing classes
// (`A::B`), an out-of-class definition's the qualifier it is written with.
// A Signature follows wire.Export.Signature: each parameter's type and the
// result's as written, with the name, whitespace, comments and default
// value removed (`const char *s` is const char*, a function-pointer
// parameter `void (*cb)(int x)` is void(*)(int)), so `int f(char *s)` is
// "(char*)(int)", `(void)` is no parameter, `...` is one, and a constructor
// has no result. A parameter list of the old identifier form declares no
// types and gives no Signature. A typedef or alias of a function type
// declared in the file, and `std::function<R(A)>`, are expanded to the
// signature they name, so a call through one joins the functions whose
// address is taken.
//
// Scoping. A local is in scope from its declaration to the end of its block
// (a compound statement, and each selection, iteration and handler
// statement); a callable's parameters for its body. A nested function
// definition sees no enclosing local; a lambda sees them all, which is what
// it captures. Inside a method, a name no local declares is looked up among
// its class's members when the class is declared in the file.
type cLinker struct {
	language string
	s        *cSyntax
	x        *cLinkSyntax
	src      []byte
	out      *wire.Link

	// File-scope tables, filled by declare before the walk; every key is a
	// view of src, and file drops them all before it returns.
	functions  map[string]bool
	objects    map[string]cDecl
	others     map[string]bool
	typedefs   map[string]cDecl
	records    map[string]Node
	namespaces map[string]bool
	members    map[Node]map[string]cMember
	// statics holds, under linkageKey, every namespace-scope function a
	// declaration of the file declares `static`: every declaration of it
	// has internal linkage, a later one without the specifier included
	// (ISO C 6.2.2p5, [dcl.stc]).
	statics map[string]bool

	// names and locals are the scope chain of the walk, one local per
	// binding; base is where the innermost callable's bindings begin.
	names  scope
	locals []cLocal
	base   int
	// classes is the chain of class bodies the walk is inside; self is the
	// class the innermost function is a member of, or empty.
	classes []string
	self    string
	// depth counts the callables the walk is inside.
	depth int
	// sites maps a call expression to its index in out.Sites.
	sites map[uintptr]uint32
	buf   []byte
}

// cDecl is one declarator and the declaration that owns its specifiers: a
// declaration, a parameter, a field, a typedef, a type descriptor.
type cDecl struct{ owner, decl Node }

// cMember is a member of a class or struct, fn when it is a member function.
type cMember struct {
	cDecl
	fn bool
}

// cLocal is a local's declaration; fn marks a block-scope function
// prototype, which names a function, not a variable, and busy marks a local
// whose type is being recovered from its own initializer.
type cLocal struct {
	cDecl
	fn, busy bool
}

func newCLinker(language string) *cLinker { return &cLinker{language: language} }

// function reads nothing: a C or C++ callable's sites are typed from
// declarations, which file reads with the rest of the tables.
func (k *cLinker) function(fn Node, g *flow.Graph, du flow.Edges, src []byte) {}

func (k *cLinker) file(root Node, src []byte, out *wire.Link) {
	*k = cLinker{language: k.language, s: cSyntaxOf(k.language), x: cLinkSyntaxOf(k.language), src: src, out: out,
		functions: map[string]bool{}, objects: map[string]cDecl{}, others: map[string]bool{},
		typedefs: map[string]cDecl{}, records: map[string]Node{}, namespaces: map[string]bool{},
		members: map[Node]map[string]cMember{}, statics: map[string]bool{}, sites: map[uintptr]uint32{}}
	k.declare(root)
	k.classes = k.classes[:0]
	k.walk(root)
	*k = cLinker{language: k.language}
}

// declare collects the file-scope names under n: functions, objects,
// constants, typedefs and aliases, records and namespaces. It does not enter
// a callable's body.
func (k *cLinker) declare(n Node) {
	s, x := k.s, k.x
	switch id := n.KindId(); {
	case !n.IsNamed() || id == s.compoundStatement || id == s.lambdaExpression:
		return
	case id == x.functionDefinition:
		if c := k.chain(n.ChildByFieldId(s.fDeclarator)); !c.name.IsNull() {
			k.functions[k.text(c.name)] = true
			k.declareStatic(n, c)
		}
	case id == s.declaration || id == x.fieldDeclaration:
		k.declarators(n, func(d Node) {
			c := k.chain(d)
			switch {
			case c.name.IsNull():
			case c.function:
				k.functions[k.text(c.name)] = true
				if id == s.declaration {
					k.declareStatic(n, c)
				}
			case id == s.declaration && len(k.classes) == 0:
				k.objects[k.text(c.name)] = cDecl{owner: n, decl: d}
			}
		})
	case id == x.typeDefinition:
		k.declarators(n, func(d Node) {
			c := k.chain(d)
			if c.name.IsNull() {
				return
			}
			name := k.text(c.name)
			k.typedefs[name], k.others[name] = cDecl{owner: n, decl: d}, true
			if t := n.ChildByFieldId(s.fType); k.record(t) && t.ChildByFieldId(s.fName).IsNull() {
				k.addRecord(name, t.ChildByFieldId(s.fBody))
			}
		})
	case id == x.aliasDeclaration:
		if name := n.ChildByFieldId(s.fName); !name.IsNull() {
			if t := n.ChildByFieldId(s.fType); !t.IsNull() {
				k.typedefs[k.text(name)] = cDecl{owner: t, decl: t.ChildByFieldId(s.fDeclarator)}
			}
			k.others[k.text(name)] = true
		}
	case k.record(n):
		if name, body := n.ChildByFieldId(s.fName), n.ChildByFieldId(s.fBody); !name.IsNull() && !body.IsNull() {
			k.addRecord(k.className(name), body)
			if bare := k.typeName(name); bare != k.className(name) {
				k.addRecord(bare, body)
			}
			k.others[k.typeName(name)] = true
		}
	case id == x.fieldDeclarationList:
		k.classes = append(k.classes, k.typeName(n.Parent().ChildByFieldId(s.fName)))
		k.declareChildren(n)
		k.classes = k.classes[:len(k.classes)-1]
		return
	case id == x.namespaceDefinition:
		if name := n.ChildByFieldId(s.fName); !name.IsNull() {
			k.namespaces[k.typeName(name)] = true
		}
	case id == x.enumerator || id == x.preprocDef:
		if name := n.ChildByFieldId(s.fName); !name.IsNull() {
			k.others[k.text(name)] = true
		}
	}
	k.declareChildren(n)
}

// declareStatic records the function chain c of the declaration owner
// declares in statics when owner declares it `static` at namespace scope; a
// `static` member of a class is a static member function, which has the
// class's linkage.
func (k *cLinker) declareStatic(owner Node, c cChain) {
	if len(k.classes) == 0 && k.storage(owner, "static") {
		if key, ok := k.linkageKey(owner, c); ok {
			k.statics[key] = true
		}
	}
}

// linkageKey is the key a namespace-scope function the chain c of the
// declaration owner declares is recorded under in statics: the namespace
// it is a member of (its enclosing namespaces, then its qualifier) and its
// name. ok is false for a member of a class (an out-of-class definition
// `void A::f() {}`), whose linkage is its class's.
func (k *cLinker) linkageKey(owner Node, c cChain) (string, bool) {
	q := ""
	if !c.scope.IsNull() {
		q = k.qualifier(c.scope)
		if !k.namespaces[q] {
			return "", false
		}
	}
	ns, _ := k.enclosingNamespaces(owner)
	if ns != "" && q != "" {
		ns += "::"
	}
	return ns + q + "\x00" + k.text(c.name), true
}

// enclosingNamespaces is the chain of namespaces enclosing n, innermost
// last, and whether one of them is an unnamed namespace, whose members have
// internal linkage ([basic.link]).
func (k *cLinker) enclosingNamespaces(n Node) (chain string, unnamed bool) {
	if k.x.namespaceDefinition == 0 {
		return "", false
	}
	var names []string
	for p := n.Parent(); !p.IsNull(); p = p.Parent() {
		if p.KindId() != k.x.namespaceDefinition {
			continue
		}
		if name := p.ChildByFieldId(k.s.fName); name.IsNull() {
			unnamed = true
		} else {
			names = append(names, k.typeName(name))
		}
	}
	slices.Reverse(names)
	return strings.Join(names, "::"), unnamed
}

func (k *cLinker) declareChildren(n Node) {
	for c := n.first(); !c.IsNull(); c = c.next() {
		k.declare(c)
	}
}

// addRecord records body as the members of the record name; a second,
// different body for one name makes the name ambiguous, which answers no
// record.
func (k *cLinker) addRecord(name string, body Node) {
	if name == "" || body.IsNull() {
		return
	}
	if prev, ok := k.records[name]; ok && prev != body {
		body = Node{}
	}
	k.records[name] = body
}

// record reports whether n is a class, struct or union specifier.
func (k *cLinker) record(n Node) bool {
	if n.IsNull() || !n.IsNamed() {
		return false
	}
	id := n.KindId()
	return id == k.x.structSpecifier || id == k.x.unionSpecifier || id == k.x.classSpecifier && id != 0
}

// className is a class named by name as an in-class member's Receiver names
// it: the enclosing classes, then the name.
func (k *cLinker) className(name Node) string {
	n := k.typeName(name)
	if len(k.classes) == 0 {
		return n
	}
	return strings.Join(k.classes, "::") + "::" + n
}

// declarators calls visit with each declarator of the declaration n.
func (k *cLinker) declarators(n Node, visit func(d Node)) {
	c := n.Walk()
	defer c.Close()
	if !c.GotoFirstChild() {
		return
	}
	for {
		if c.FieldId() == k.s.fDeclarator {
			visit(c.Node())
		}
		if !c.GotoNextSibling() {
			return
		}
	}
}

// walk reads the tables under n.
func (k *cLinker) walk(n Node) {
	s, x := k.s, k.x
	if !n.IsNamed() || n.IsExtra() {
		return
	}
	switch id := n.KindId(); {
	case id == s.compoundStatement || id == s.forStatement || id == s.ifStatement || id == s.whileStatement ||
		id == s.doStatement || id == s.switchStatement || id == s.forRangeLoop && id != 0 || id == s.catchClause && id != 0:
		m := k.names.mark()
		switch id {
		case s.forRangeLoop:
			k.bindDecl(n, n.ChildByFieldId(s.fDeclarator))
		case s.catchClause:
			k.bindParams(n.ChildByFieldId(s.fParameters))
		}
		k.children(n)
		k.unbind(m)
		return
	case id == x.functionDefinition:
		k.definition(n)
		return
	case id == s.lambdaExpression && id != 0:
		m := k.names.mark()
		if d := n.ChildByFieldId(s.fDeclarator); !d.IsNull() {
			k.bindParams(d.ChildByFieldId(s.fParameters))
		}
		k.depth++
		k.children(n)
		k.depth--
		k.unbind(m)
		return
	case id == s.declaration:
		k.declaration(n)
	case id == x.fieldDeclaration:
		receiver := k.receiver(Node{})
		k.declarators(n, func(d Node) { k.export(n, d, receiver) })
	case id == x.fieldDeclarationList:
		k.classes = append(k.classes, k.typeName(n.Parent().ChildByFieldId(s.fName)))
		k.children(n)
		k.classes = k.classes[:len(k.classes)-1]
		return
	case k.record(n):
		k.supertypes(n)
		k.internalType(n)
	case id == s.enumSpecifier:
		k.internalType(n)
	case id == x.preprocFunctionDef:
		k.macro(n)
		return
	case id == s.callExpression:
		k.children(n)
		k.call(n)
		return
	case id == s.identifier || id == s.qualifiedIdentifier && id != 0:
		k.value(n, n)
		return
	case id == s.pointerExpression:
		if cKind(n.ChildByFieldId(s.fOperator)) == s.amp {
			if a := k.unparen(n.ChildByFieldId(s.fArgument)); cKind(a) == s.identifier || cKind(a) == s.qualifiedIdentifier && s.cpp {
				k.value(n, a)
			}
		}
	}
	k.children(n)
}

func (k *cLinker) children(n Node) {
	for c := n.first(); !c.IsNull(); c = c.next() {
		k.walk(c)
	}
}

// definition reads a function definition: its Export, then its parameters
// and body with the parameters in scope and no enclosing local.
func (k *cLinker) definition(n Node) {
	s := k.s
	d := n.ChildByFieldId(s.fDeclarator)
	c := k.chain(d)
	receiver := k.receiver(c.scope)
	if !n.ChildByFieldId(s.fBody).IsNull() || k.bodiless(n) {
		k.export(n, d, receiver)
	}
	m, base, self := k.names.mark(), k.base, k.self
	k.base, k.self = m, receiver
	if !c.fn.IsNull() {
		k.bindParams(c.fn.ChildByFieldId(s.fParameters))
	}
	k.depth++
	k.children(n)
	k.depth--
	k.unbind(m)
	k.base, k.self = base, self
}

// declaration reads a declaration: a prototype's Export where the
// extraction captures one, a local's binding inside a callable, and the
// sites and values of its initializers.
func (k *cLinker) declaration(n Node) {
	x := k.x
	p := n.Parent()
	for !p.IsNull() && x.in(x.conditional, p.KindId()) {
		p = p.Parent()
	}
	if x.fileScope(cKind(p)) {
		receiver := k.receiver(Node{})
		k.declarators(n, func(d Node) { k.export(n, d, receiver) })
		return
	}
	if k.depth > 0 {
		k.declarators(n, func(d Node) { k.bindDecl(n, d) })
	}
}

// bindDecl brings the name declarator d declares into scope.
func (k *cLinker) bindDecl(owner, d Node) {
	c := k.chain(d)
	if c.name.IsNull() || c.name.KindId() != k.s.identifier {
		return
	}
	k.names.push(textOf(k.src, c.name), int32(len(k.locals)))
	k.locals = append(k.locals, cLocal{cDecl: cDecl{owner: owner, decl: d}, fn: c.function})
}

// bindParams brings each named parameter of the parameter list ps into
// scope.
func (k *cLinker) bindParams(ps Node) {
	if ps.IsNull() {
		return
	}
	for p := ps.first(); !p.IsNull(); p = p.next() {
		if k.parameter(p) {
			k.bindDecl(p, p.ChildByFieldId(k.s.fDeclarator))
		}
	}
}

// parameter reports whether n is a parameter declaration.
func (k *cLinker) parameter(n Node) bool {
	id, x := n.KindId(), k.x
	return n.IsNamed() && (id == x.parameterDeclaration || id != 0 && (id == x.optionalParameterDeclaration || id == x.variadicParameterDeclaration))
}

func (k *cLinker) unbind(m int) {
	k.names.truncate(m)
	k.locals = k.locals[:m]
}

// local is the innermost local named name visible in the innermost
// callable.
func (k *cLinker) local(name []byte) (*cLocal, bool) {
	if i := k.names.find(name, k.base); i >= 0 {
		return &k.locals[k.names.at(i).v], true
	}
	return nil, false
}

// receiver is the Receiver of a member declared with the qualified name
// scope (null for an unqualified one): the qualifier as written, unless it
// names a namespace of the file, or else the enclosing classes.
func (k *cLinker) receiver(scope Node) string {
	if !scope.IsNull() {
		q := k.qualifier(scope)
		if k.namespaces[q] {
			return ""
		}
		return q
	}
	return strings.Join(k.classes, "::")
}

// export appends the Export of the function that declarator d of the
// declaration owner declares, if d declares a function.
func (k *cLinker) export(owner, d Node, receiver string) {
	s := k.s
	c := k.chain(d)
	if c.name.IsNull() || !c.function {
		return
	}
	e := wire.Export{Name: wireSpan(spanOf(c.name)), Receiver: receiver}
	sig, a := k.signature(owner, d, c.fn)
	if a.typed {
		e.Signature = sig
	}
	e.Params, e.Optional, e.Variadic = a.params, a.optional, a.variadic
	// An empty parameter list outside a definition declares nothing about
	// the parameters in C (ISO C 6.7.6.3p14): a call may pass any number.
	if a.empty && !s.cpp && owner.KindId() != k.x.functionDefinition {
		e.Variadic = true
	}
	structor := owner.ChildByFieldId(s.fType).IsNull() && c.name.KindId() != k.x.destructorName && c.name.KindId() != s.operatorCast
	e.Static = receiver == "" || structor || k.storage(owner, "static")
	e.Abstract = k.pure(owner, d)
	if receiver != "" && s.cpp {
		e.Virtual, e.Final = k.virtual(owner, c.fn, receiver)
	} else if key, ok := k.linkageKey(owner, c); ok && len(k.classes) == 0 {
		_, unnamed := k.enclosingNamespaces(owner)
		e.Internal = unnamed || k.statics[key]
	}
	e.Returns = k.returns(owner, d, c.fn)
	k.out.Exports = append(k.out.Exports, e)
}

// virtual reports whether the member function of the class receiver that
// the function declarator fn of the declaration owner declares is virtual
// (declared `virtual`, or with an `override` or `final` virt-specifier) and
// whether it is final (declared `final`, or a member of a class declared
// `final`), as this declaration says it. An out-of-class definition repeats
// neither `virtual` nor a virt-specifier ([class.virtual]): its class's
// declaration of the member carries them.
func (k *cLinker) virtual(owner, fn Node, receiver string) (virtual, final bool) {
	x := k.x
	for c := owner.first(); !c.IsNull(); c = c.next() {
		if c.KindId() == x.virtual && !c.IsNamed() {
			virtual = true
		}
	}
	for c := cFirst(fn); !c.IsNull(); c = c.next() {
		if c.KindId() == x.virtualSpecifier && c.IsNamed() {
			virtual = true
			final = final || string(textOf(k.src, c)) == "final"
		}
	}
	// The class body: the field declaration list an in-class member sits in
	// (through templates and preprocessor arms), else the record the
	// receiver names in the file.
	body := owner.Parent()
	for !body.IsNull() && body.KindId() != x.fieldDeclarationList && !k.record(body) && body.KindId() != x.translationUnit {
		body = body.Parent()
	}
	if len(k.classes) == 0 || cKind(body) != x.fieldDeclarationList {
		body = k.recordOf(receiver)
	}
	if !body.IsNull() {
		for c := cFirst(body.Parent()); !c.IsNull(); c = c.next() {
			if c.KindId() == x.virtualSpecifier && c.IsNamed() && string(textOf(k.src, c)) == "final" {
				final = true
			}
		}
	}
	return virtual, final
}

// bodiless reports a C++ function definition that ends with a clause
// instead of a body: `= 0`, `= default` or `= delete`.
func (k *cLinker) bodiless(n Node) bool {
	for c := n.first(); !c.IsNull(); c = c.next() {
		if k.x.in(k.x.clause, c.KindId()) && c.IsNamed() {
			return true
		}
	}
	return false
}

// pure reports a pure virtual member function, declarator d of owner: a
// definition ending with `= 0`, or a member declaration whose declarator is
// followed by the literal 0, which is how the grammar may read the same
// `virtual int f() = 0;`.
func (k *cLinker) pure(owner, d Node) bool {
	if !k.s.cpp {
		return false
	}
	switch owner.KindId() {
	case k.x.functionDefinition:
		for c := owner.first(); !c.IsNull(); c = c.next() {
			if c.KindId() == k.x.pureVirtualClause && c.IsNamed() {
				return true
			}
		}
	case k.x.fieldDeclaration:
		c := owner.Walk()
		defer c.Close()
		after := false
		for ok := c.GotoFirstChild(); ok; ok = c.GotoNextSibling() {
			n := c.Node()
			switch {
			case c.FieldId() == k.s.fDeclarator:
				if after {
					return false
				}
				after = n == d
			case after && n.IsNamed() && !n.IsExtra():
				return n.KindId() == k.s.numberLiteral && string(textOf(k.src, n)) == "0"
			}
		}
	}
	return false
}

// macro appends the Export of a function-like macro: its parameter count,
// and no types. Every parameter is optional: an argument of a macro
// invocation may be empty (`G()` passes G one empty argument, ISO C
// 6.10.3p4), and an empty argument is no expression of the call's argument
// list.
func (k *cLinker) macro(n Node) {
	name := n.ChildByFieldId(k.s.fName)
	if name.IsNull() {
		return
	}
	e := wire.Export{Name: wireSpan(spanOf(name)), Static: true}
	ps := n.ChildByFieldId(k.s.fParameters)
	for p := cFirst(ps); !p.IsNull(); p = p.next() {
		switch {
		case p.KindId() == k.s.identifier && p.IsNamed():
			e.Params++
		case p.KindId() == k.x.ellipsis && !p.IsNamed():
			e.Params++
			e.Variadic = true
		}
	}
	e.Optional = e.Params
	k.out.Exports = append(k.out.Exports, e)
}

// storage reports whether the declaration owner has the storage class
// specifier word.
func (k *cLinker) storage(owner Node, word string) bool {
	for c := owner.first(); !c.IsNull(); c = c.next() {
		if c.KindId() == k.s.storageClassSpecifier && c.IsNamed() && string(textOf(k.src, c)) == word {
			return true
		}
	}
	return false
}

// internalType appends the Export of the class, struct, union or enum
// definition n when an unnamed namespace encloses it: the type has internal
// linkage ([basic.link]), so only its own file names it. Nothing else of a
// type is exported.
func (k *cLinker) internalType(n Node) {
	name, body := n.ChildByFieldId(k.s.fName), n.ChildByFieldId(k.s.fBody)
	if cKind(name) != k.s.typeIdentifier || body.IsNull() {
		return
	}
	if _, unnamed := k.enclosingNamespaces(n); unnamed {
		k.out.Exports = append(k.out.Exports, wire.Export{Name: wireSpan(spanOf(name)), Internal: true})
	}
}

// supertypes appends each base class of the record n.
func (k *cLinker) supertypes(n Node) {
	if k.x.baseClassClause == 0 {
		return
	}
	name := n.ChildByFieldId(k.s.fName)
	if name.IsNull() {
		return
	}
	t := k.className(name)
	for c := n.first(); !c.IsNull(); c = c.next() {
		if c.KindId() != k.x.baseClassClause {
			continue
		}
		for b := c.first(); !b.IsNull(); b = b.next() {
			if id := b.KindId(); b.IsNamed() && (id == k.s.typeIdentifier || id == k.s.qualifiedIdentifier || id == k.x.templateType) {
				k.out.Supertypes = append(k.out.Supertypes, wire.Supertype{Type: t, Super: k.typeName(b)})
			}
		}
	}
}

// call appends the Site of the call n when the structural queries capture
// its callee, after the sites of its subtree.
func (k *cLinker) call(n Node) {
	s := k.s
	callee := firstNamed(n)
	if callee.IsNull() {
		return
	}
	var site wire.Site
	switch callee.KindId() {
	case s.identifier:
		site.Name, site.Shape = wireSpan(spanOf(callee)), wire.ShapeBare
		if d, ok := k.object(textOf(k.src, callee)); ok {
			site.Shape, site.Receiver, site.Signature = wire.ShapeIndirect, k.declType(d), k.funcSig(d, 0)
		}
	case s.fieldExpression:
		if !k.fieldCall(&site, callee, true) {
			return
		}
	case s.qualifiedIdentifier:
		name, scope := callee.ChildByFieldId(s.fName), firstNamed(callee)
		if cKind(name) != s.identifier || scope == name {
			return
		}
		site.Name, site.Shape, site.Qualifier = wireSpan(spanOf(name)), wire.ShapeStatic, k.typeName(scope)
	case s.templateFunction:
		name := callee.ChildByFieldId(s.fName)
		if cKind(name) != s.identifier {
			return
		}
		site.Name, site.Shape = wireSpan(spanOf(name)), wire.ShapeBare
	case k.x.paren:
		// `(*fp)(x)` and `(*s->cb)(x)`: the patterns the queries capture, so
		// no other parenthesized callee has a call reference to join.
		deref := firstNamed(callee)
		if cKind(deref) != s.pointerExpression || cKind(deref.ChildByFieldId(s.fOperator)) != s.star {
			return
		}
		switch ptr := deref.ChildByFieldId(s.fArgument); cKind(ptr) {
		case s.identifier:
			site.Name, site.Shape = wireSpan(spanOf(ptr)), wire.ShapeBare
			if d, ok := k.object(textOf(k.src, ptr)); ok {
				site.Shape, site.Receiver, site.Signature = wire.ShapeIndirect, k.declType(d), k.funcSig(d, 0)
			}
		case s.fieldExpression:
			if !k.fieldCall(&site, ptr, false) {
				return
			}
		default:
			return
		}
	case s.subscriptExpression:
		// `tbl[i](x)` calls an element, never a function named tbl. funcSig
		// reads the function type below the declarator's arrays and
		// pointers, the element's type.
		tbl := callee.ChildByFieldId(s.fArgument)
		if cKind(tbl) != s.identifier {
			return
		}
		site.Name, site.Shape = wireSpan(spanOf(tbl)), wire.ShapeIndirect
		if d, ok := k.object(textOf(k.src, tbl)); ok {
			site.Signature = k.funcSig(d, 0)
		}
	default:
		return
	}
	for c := n.first(); !c.IsNull(); c = c.next() {
		if c.KindId() == k.x.argumentList && c.IsNamed() {
			for a := c.first(); !a.IsNull(); a = a.next() {
				if a.IsNamed() && !a.IsExtra() {
					site.Args = append(site.Args, k.recover(a))
				}
			}
		}
	}
	k.sites[n.Id()] = uint32(len(k.out.Sites))
	k.out.Sites = append(k.out.Sites, site)
}

// fieldCall fills site for a call through the field expression fe, `x.f()`
// or `x->f()`, or, dereferenced, `(*x->f)()`: an indirect call through the
// data member f, typed by its declaration when the receiver's record
// declares it, or, when member allows it, a C++ call of the member function
// f on the receiver. ok is false when fe names no field.
func (k *cLinker) fieldCall(site *wire.Site, fe Node, member bool) (ok bool) {
	s := k.s
	field := lastNamed(fe)
	if cKind(field) != s.fieldIdentifier {
		return false
	}
	site.Name = wireSpan(spanOf(field))
	recv := k.recover(fe.ChildByFieldId(s.fArgument))
	m, found := k.member(recv, textOf(k.src, field))
	if member && s.cpp && (!found || m.fn) {
		site.Shape, site.Receiver = wire.ShapeMember, recv
		return true
	}
	site.Shape, site.Receiver = wire.ShapeIndirect, wire.Type{Kind: wire.TypeField, Name: string(textOf(k.src, field))}
	if found {
		site.Signature = k.funcSig(m.cDecl, 0)
	}
	return true
}

// object is the declaration of the variable a called name names: a local,
// a data member of the enclosing class, or a file-scope object. A name that
// names a function, or nothing the file declares, is no object.
func (k *cLinker) object(name []byte) (cDecl, bool) {
	if l, ok := k.local(name); ok {
		return l.cDecl, !l.fn
	}
	if m, ok := k.member(wire.Type{Kind: wire.TypeNamed, Name: k.self}, name); ok {
		return m.cDecl, !m.fn
	}
	d, ok := k.objects[view(name)]
	return d, ok
}

// value appends the Value of n, which takes the function named by name as a
// value, when n stores or passes it or takes its address.
func (k *cLinker) value(n, name Node) {
	field, stored := k.position(n)
	addr := n != name
	if !stored && !addr {
		return
	}
	if name.KindId() == k.s.qualifiedIdentifier {
		last := name
		for cKind(last) == k.s.qualifiedIdentifier {
			last = last.ChildByFieldId(k.s.fName)
		}
		if !addr && (last.IsNull() || !k.functions[view(textOf(k.src, last))]) {
			return
		}
	} else {
		t := textOf(k.src, name)
		if l, ok := k.local(t); ok && !l.fn {
			return
		}
		if m, ok := k.member(wire.Type{Kind: wire.TypeNamed, Name: k.self}, t); ok && !m.fn {
			return
		}
		if _, ok := k.objects[view(t)]; ok || k.others[view(t)] {
			return
		}
	}
	k.out.Values = append(k.out.Values, wire.Value{At: wireSpan(spanOf(n)), Field: field,
		Type: wire.Type{Kind: wire.TypeFunction, Name: k.typeName(name)}})
}

// position reports whether n's value is stored or passed: an argument, an
// initializer, a member's default value, the right side of `=`, a return
// value, an arm of a conditional in such a position, through any
// parentheses and casts. field is the field it is written to, when it is.
func (k *cLinker) position(n Node) (field string, ok bool) {
	s, x := k.s, k.x
	p := n.Parent()
	for !p.IsNull() && (p.KindId() == x.paren || p.KindId() == s.castExpression) {
		n, p = p, p.Parent()
	}
	if p.IsNull() {
		return "", false
	}
	switch p.KindId() {
	case x.argumentList, s.initializerList, s.returnStatement:
		return "", true
	case s.initDeclarator:
		return "", p.ChildByFieldId(s.fValue) == n
	case s.assignmentExpression:
		if p.ChildByFieldId(s.fRight) != n || cKind(p.ChildByFieldId(s.fOperator)) != s.assign {
			return "", false
		}
		if l := k.unparen(p.ChildByFieldId(s.fLeft)); cKind(l) == s.fieldExpression {
			if f := lastNamed(l); cKind(f) == s.fieldIdentifier {
				return string(textOf(k.src, f)), true
			}
		}
		return "", true
	case s.initializerPair:
		v := p.ChildByFieldId(s.fValue)
		if v != n {
			return "", false
		}
		for c := p.first(); !c.IsNull() && c != v; c = c.next() {
			switch c.KindId() {
			case x.fieldDesignator:
				if f := firstNamed(c); !f.IsNull() {
					field = string(textOf(k.src, f))
				}
			case s.fieldIdentifier:
				field = string(textOf(k.src, c))
			}
		}
		return field, true
	case s.conditionalExpression:
		if p.ChildByFieldId(s.fConsequence) == n || p.ChildByFieldId(s.fAlternative) == n {
			return k.position(p)
		}
	case x.fieldDeclaration:
		// A member's default value (`void (*cb)(int) = h;`): an expression
		// child of a field declaration is one, since its declarators are no
		// expression, written to the declarator before it.
		c := p.Walk()
		defer c.Close()
		for ok := c.GotoFirstChild(); ok && c.Node() != n; ok = c.GotoNextSibling() {
			if c.FieldId() == s.fDeclarator {
				if name := k.chain(c.Node()).name; !name.IsNull() {
					field = string(textOf(k.src, name))
				}
			}
		}
		return field, field != ""
	}
	return "", false
}

// recover is the type of the expression e as the file declares it.
func (k *cLinker) recover(e Node) wire.Type {
	s := k.s
	e = k.unparen(e)
	if e.IsNull() {
		return wire.Type{}
	}
	switch e.KindId() {
	case s.identifier:
		t := textOf(k.src, e)
		if l, ok := k.local(t); ok {
			// Nothing is bound while a type is recovered, so l stays valid.
			if l.busy {
				return wire.Type{}
			}
			l.busy = true
			r := k.declType(l.cDecl)
			l.busy = false
			return r
		}
		if m, ok := k.member(wire.Type{Kind: wire.TypeNamed, Name: k.self}, t); ok {
			return k.declType(m.cDecl)
		}
		if d, ok := k.objects[view(t)]; ok {
			return k.declType(d)
		}
		if k.functions[view(t)] {
			return wire.Type{Kind: wire.TypeFunction, Name: string(t)}
		}
	case s.this:
		if k.self != "" && s.this != 0 {
			return wire.Type{Kind: wire.TypeNamed, Name: k.self}
		}
	case s.pointerExpression:
		a := k.unparen(e.ChildByFieldId(s.fArgument))
		if cKind(e.ChildByFieldId(s.fOperator)) == s.amp {
			if cKind(a) == s.qualifiedIdentifier && s.cpp {
				return wire.Type{Kind: wire.TypeFunction, Name: k.typeName(a)}
			}
			if cKind(a) == s.identifier {
				if t := k.recover(a); t.Kind == wire.TypeFunction {
					return t
				}
			}
		}
		return k.recover(a)
	case s.fieldExpression:
		if f := lastNamed(e); cKind(f) == s.fieldIdentifier {
			if m, ok := k.member(k.recover(e.ChildByFieldId(s.fArgument)), textOf(k.src, f)); ok && !m.fn {
				return k.declType(m.cDecl)
			}
		}
	case s.subscriptExpression:
		return k.recover(firstNamed(e))
	case s.callExpression:
		if i, ok := k.sites[e.Id()]; ok {
			return wire.Type{Kind: wire.TypeResult, Index: i}
		}
	case s.newExpression:
		return cNamed(k.typeName(e.ChildByFieldId(s.fType)))
	case s.castExpression:
		if t := e.ChildByFieldId(s.fType); !t.IsNull() {
			return cNamed(k.typeName(t.ChildByFieldId(s.fType)))
		}
	}
	return wire.Type{}
}

// cNamed is an instance of the type name, or nothing for no name.
func cNamed(name string) wire.Type {
	if name == "" {
		return wire.Type{}
	}
	return wire.Type{Kind: wire.TypeNamed, Name: name}
}

// declType is the type a declarator declares: the function it names, an
// instance of the named type below its pointers, references and arrays, the
// type of an `auto` declarator's initializer, or nothing for a function
// pointer, which has no named type.
func (k *cLinker) declType(d cDecl) wire.Type {
	s := k.s
	c := k.chain(d.decl)
	switch {
	case c.function && !c.name.IsNull():
		return wire.Type{Kind: wire.TypeFunction, Name: k.typeName(c.name)}
	case !c.fn.IsNull():
		return wire.Type{}
	}
	t := d.owner.ChildByFieldId(s.fType)
	if cKind(t) == k.x.placeholderTypeSpecifier && k.x.placeholderTypeSpecifier != 0 {
		if init := k.initializer(d.decl); !init.IsNull() {
			return k.recover(init)
		}
		return wire.Type{}
	}
	return cNamed(k.typeName(t))
}

// initializer is the value of an init declarator d, or null.
func (k *cLinker) initializer(d Node) Node {
	if cKind(d) == k.s.initDeclarator {
		return d.ChildByFieldId(k.s.fValue)
	}
	return Node{}
}

// member is the member name of the class or struct the type t names, when
// the file declares it.
func (k *cLinker) member(t wire.Type, name []byte) (cMember, bool) {
	if t.Kind != wire.TypeNamed || t.Name == "" {
		return cMember{}, false
	}
	body := k.recordOf(t.Name)
	if body.IsNull() {
		return cMember{}, false
	}
	ms, ok := k.members[body]
	if !ok {
		ms = map[string]cMember{}
		k.collectMembers(body, ms)
		k.members[body] = ms
	}
	m, ok := ms[view(name)]
	return m, ok
}

// collectMembers adds the members the class body n declares to ms,
// through preprocessor arms and templates, never into a nested class.
func (k *cLinker) collectMembers(n Node, ms map[string]cMember) {
	s, x := k.s, k.x
	for c := n.first(); !c.IsNull(); c = c.next() {
		switch id := c.KindId(); {
		case !c.IsNamed() || c.IsExtra() || id == x.fieldDeclarationList || id == s.compoundStatement:
		case id == x.fieldDeclaration || id == s.declaration:
			k.declarators(c, func(d Node) {
				if ch := k.chain(d); !ch.name.IsNull() {
					ms[k.text(ch.name)] = cMember{cDecl: cDecl{owner: c, decl: d}, fn: ch.function}
				}
			})
		case id == x.functionDefinition:
			if ch := k.chain(c.ChildByFieldId(s.fDeclarator)); !ch.name.IsNull() {
				ms[k.text(ch.name)] = cMember{cDecl: cDecl{owner: c, decl: c.ChildByFieldId(s.fDeclarator)}, fn: true}
			}
		case k.record(c):
		default:
			k.collectMembers(c, ms)
		}
	}
}

// recordOf is the body of the record the type name names, through the
// file's typedefs and aliases, or null. A name qualified by a namespace the
// file's records are not keyed under falls back to its last component.
func (k *cLinker) recordOf(name string) Node {
	for range len(k.typedefs) + 1 {
		if body, ok := k.records[name]; ok {
			return body
		}
		if i := strings.LastIndex(name, "::"); i >= 0 {
			if body, ok := k.records[name[i+2:]]; ok {
				return body
			}
		}
		td, ok := k.typedefs[name]
		if !ok {
			return Node{}
		}
		name = k.typeName(td.owner.ChildByFieldId(k.s.fType))
	}
	return Node{}
}

// funcSig is the signature of the function type declarator d declares: a
// function, a pointer or reference to one, a typedef or alias of the file
// naming one, or `std::function<R(A)>`; empty for any other type. depth
// counts the typedefs expanded, which a typedef cycle in broken code would
// otherwise repeat.
func (k *cLinker) funcSig(d cDecl, depth int) string {
	s := k.s
	top := d.decl
	if cKind(top) == s.initDeclarator {
		top = top.ChildByFieldId(s.fDeclarator)
	}
	if c := k.chain(top); !c.fn.IsNull() {
		if sig, a := k.signature(d.owner, top, c.fn); a.typed {
			return sig
		}
		return ""
	}
	t := d.owner.ChildByFieldId(s.fType)
	if cKind(t) == k.x.templateType && k.x.templateType != 0 {
		name := k.typeName(t)
		if name != "function" && !strings.HasSuffix(name, "::function") {
			return ""
		}
		args := t.ChildByFieldId(s.fArguments)
		if args.IsNull() {
			return ""
		}
		if desc := firstNamed(args); cKind(desc) == s.typeDescriptor {
			return k.funcSig(cDecl{owner: desc, decl: desc.ChildByFieldId(s.fDeclarator)}, depth)
		}
		return ""
	}
	if depth > len(k.typedefs) {
		return ""
	}
	if td, ok := k.typedefs[k.typeName(t)]; ok {
		return k.funcSig(td, depth+1)
	}
	return ""
}

// cArity is what a parameter list says of the calls that fit it: params
// counts every parameter, `...` and a parameter pack included, as every
// other family's worker counts its variadic parameter; optional counts the
// trailing ones with a default argument, which a call may leave out;
// variadic reports a final `...` or pack. typed is false for a parameter list of the old identifier form,
// which declares no types; empty reports a list with nothing in it, `()`,
// which `(void)` is not.
type cArity struct {
	params, optional       uint32
	variadic, typed, empty bool
}

// signature is the Signature of the function declarator fn below the
// declarator top of the declaration owner, by the rule of cLinker, with
// its parameter list's arity.
func (k *cLinker) signature(owner, top, fn Node) (string, cArity) {
	b := append(k.buf[:0], '(')
	b, a := k.params(b, fn.ChildByFieldId(k.s.fParameters))
	b = append(b, ")("...)
	if r := k.trailing(fn); !r.IsNull() {
		b = k.leaves(b, r, Node{})
	} else {
		b = k.specifiers(b, owner)
		b = k.leaves(b, top, fn)
	}
	b = append(b, ')')
	k.buf = b
	return string(b), a
}

// trailing is the type descriptor of fn's trailing return type, or null.
func (k *cLinker) trailing(fn Node) Node {
	if k.x.trailingReturnType == 0 {
		return Node{}
	}
	for c := fn.first(); !c.IsNull(); c = c.next() {
		if c.KindId() == k.x.trailingReturnType {
			return firstNamed(c)
		}
	}
	return Node{}
}

// params appends the parameter list ps's types, joined by ",", to b, and
// reads its arity.
func (k *cLinker) params(b []byte, ps Node) ([]byte, cArity) {
	s, x := k.s, k.x
	a := cArity{typed: true, empty: true}
	start := len(b)
	var only Node
	for c := cFirst(ps); !c.IsNull(); c = c.next() {
		id := c.KindId()
		ellipsis := !c.IsNamed() && id == x.ellipsis || c.IsNamed() && id == x.variadicParameter && id != 0
		knr := c.IsNamed() && id == s.identifier
		if c.IsExtra() || !ellipsis && !knr && !k.parameter(c) {
			continue
		}
		if a.params > 0 {
			b = append(b, ',')
		}
		a.empty = false
		switch {
		case ellipsis:
			b = append(b, "..."...)
			a.variadic = true
		case knr:
			a.typed = false
		default:
			b = k.param(b, c)
			a.variadic = a.variadic || id == x.variadicParameterDeclaration && id != 0
			if id == x.optionalParameterDeclaration && id != 0 {
				a.optional++
			}
			only = c
		}
		a.params++
	}
	// `(void)` declares no parameter (C17 6.7.6.3p10, [dcl.fct]p4).
	if a.params == 1 && !only.IsNull() && only.ChildByFieldId(s.fDeclarator).IsNull() && string(b[start:]) == "void" {
		b, a.params = b[:start], 0
	}
	return b, a
}

// param appends one parameter's type to b: its specifiers and declarator
// without its name or default value.
func (k *cLinker) param(b []byte, p Node) []byte {
	name := k.chain(p.ChildByFieldId(k.s.fDeclarator)).name
	for c := p.first(); !c.IsNull(); c = c.next() {
		if !c.IsNamed() && c.KindId() == k.s.assign {
			break
		}
		b = k.leaves(b, c, name)
	}
	return b
}

// specifiers appends the result type the declaration owner's specifiers
// spell: its type qualifiers and its type, never its storage class,
// attributes or function specifiers.
func (k *cLinker) specifiers(b []byte, owner Node) []byte {
	c := owner.Walk()
	defer c.Close()
	if !c.GotoFirstChild() {
		return b
	}
	for {
		if n := c.Node(); c.FieldId() == k.s.fType || n.KindId() == k.s.typeQualifier && n.IsNamed() {
			b = k.leaves(b, n, Node{})
		}
		if !c.GotoNextSibling() {
			return b
		}
	}
}

// leaves appends the text of every token under n to b, without whitespace
// and comments and without the subtree skip; a nested parameter is
// appended as param appends it, without its name.
func (k *cLinker) leaves(b []byte, n, skip Node) []byte {
	if n.IsNull() || n == skip || n.IsExtra() {
		return b
	}
	if k.parameter(n) {
		return k.param(b, n)
	}
	c := n.first()
	if c.IsNull() {
		return append(b, textOf(k.src, n)...)
	}
	for ; !c.IsNull(); c = c.next() {
		b = k.leaves(b, c, skip)
	}
	return b
}

// returns is the type a function declared by fn below top returns: the
// named type of its result below pointers and references, nothing for
// void, a function pointer or no declared type.
func (k *cLinker) returns(owner, top, fn Node) wire.Type {
	s := k.s
	t := owner.ChildByFieldId(s.fType)
	if r := k.trailing(fn); !r.IsNull() {
		t, top = r.ChildByFieldId(s.fType), fn
	}
	for d := top; d != fn && !d.IsNull(); d = k.inner(d) {
		if d.KindId() == s.functionDeclarator {
			return wire.Type{}
		}
	}
	if name := k.typeName(t); name != "void" {
		return cNamed(name)
	}
	return wire.Type{}
}

// cChain is what a declarator chain declares: the name at its bottom (null
// for an abstract declarator), the outermost qualified name it is written
// with (null when unqualified), and the function declarator nearest the
// name, with function set when that declarator is the type constructor
// nearest the name, so the chain declares a function rather than an object
// (`int g(int)` against `int (*fp)(int)`).
type cChain struct {
	name, scope, fn Node
	function        bool
}

// chain walks the declarator d to its name as the structural extraction
// does, through function, pointer, array, reference, init, attributed and
// parenthesized declarators and qualified names, abstract ones included.
func (k *cLinker) chain(d Node) cChain {
	s, x := k.s, k.x
	var c cChain
	for !d.IsNull() {
		switch id := d.KindId(); {
		case id == s.functionDeclarator || id == x.abstractFunctionDeclarator:
			c.fn, c.function = d, true
			d = d.ChildByFieldId(s.fDeclarator)
		case id == s.pointerDeclarator || id == s.arrayDeclarator || id == s.abstractPointerDeclarator ||
			id == s.abstractArrayDeclarator || id == s.referenceDeclarator && id != 0 || id == x.abstractReferenceDeclarator && id != 0:
			c.function = false
			d = k.inner(d)
		case id == s.initDeclarator || id == s.attributedDeclarator:
			d = k.inner(d)
		case id == s.parenthesizedDeclarator || id == s.abstractParenthesizedDeclarator || id == s.variadicDeclarator && id != 0:
			d = k.inner(d)
		case id == s.qualifiedIdentifier && id != 0:
			if c.scope.IsNull() {
				c.scope = d
			}
			d = d.ChildByFieldId(s.fName)
		default:
			c.name = d
			return c
		}
	}
	return c
}

// inner is the declarator the declarator d wraps: its declarator field, or
// its first named child that is no attribute, calling-convention modifier
// or type qualifier, or null.
func (k *cLinker) inner(d Node) Node {
	s := k.s
	if n := d.ChildByFieldId(s.fDeclarator); !n.IsNull() {
		return n
	}
	for c := d.first(); !c.IsNull(); c = c.next() {
		if id := c.KindId(); c.IsNamed() && !c.IsExtra() && id != s.attributeDeclaration && id != s.callModifier && id != s.typeQualifier {
			return c
		}
	}
	return Node{}
}

// qualifier is the scope of the qualified name q, each component spelled
// as typeName spells it: `ns::C` for `ns::C<T>::m`.
func (k *cLinker) qualifier(q Node) string {
	var parts []string
	for cKind(q) == k.s.qualifiedIdentifier && k.s.cpp {
		name := q.ChildByFieldId(k.s.fName)
		if sc := firstNamed(q); sc != name && !sc.IsNull() {
			parts = append(parts, k.typeName(sc))
		}
		q = name
	}
	return strings.Join(parts, "::")
}

// typeName is the type or name n as cLinker spells it: as written without
// whitespace, comments or template arguments; empty for `auto`, `decltype`
// and an anonymous record.
func (k *cLinker) typeName(n Node) string {
	s, x := k.s, k.x
	if n.IsNull() {
		return ""
	}
	switch id := n.KindId(); {
	case id == x.templateType && id != 0 || id == s.templateFunction && id != 0:
		return k.typeName(n.ChildByFieldId(s.fName))
	case id == s.qualifiedIdentifier && id != 0:
		last := n
		for cKind(last) == s.qualifiedIdentifier {
			last = last.ChildByFieldId(s.fName)
		}
		if q := k.qualifier(n); q != "" {
			return q + "::" + k.typeName(last)
		}
		return k.typeName(last)
	case k.record(n) || id == s.enumSpecifier:
		return k.typeName(n.ChildByFieldId(s.fName))
	case id == x.placeholderTypeSpecifier && id != 0:
		return ""
	}
	return string(k.leaves(nil, n, Node{}))
}

// unparen strips the parentheses around e.
func (k *cLinker) unparen(e Node) Node {
	for !e.IsNull() && e.KindId() == k.x.paren {
		e = firstNamed(e)
	}
	return e
}

// text is n's source text as a view, for a table key.
func (k *cLinker) text(n Node) string { return view(textOf(k.src, n)) }

// cKind is n's kind, or 0, which no node has, for the null node.
func cKind(n Node) uint16 {
	if n.IsNull() {
		return 0
	}
	return n.KindId()
}

// cFirst is n's first child, or null for the null node or a leaf.
func cFirst(n Node) Node {
	if n.IsNull() {
		return Node{}
	}
	return n.first()
}

// cLinkSyntax holds the kind ids the C-family linker matches beyond the
// lowering's (cSyntax), resolved once per grammar. A kind the language does
// not have is 0, which no node has.
type cLinkSyntax struct {
	translationUnit, functionDefinition, fieldDeclaration, fieldDeclarationList, typeDefinition,
	structSpecifier, unionSpecifier, enumerator, preprocFunctionDef, preprocDef, parameterDeclaration,
	variadicParameter, argumentList, fieldDesignator, abstractFunctionDeclarator, paren, ellipsis,
	linkageSpecification, declarationList uint16

	// C++ only.
	classSpecifier, baseClassClause, templateType, trailingReturnType, placeholderTypeSpecifier,
	namespaceDefinition, aliasDeclaration, optionalParameterDeclaration, variadicParameterDeclaration,
	abstractReferenceDeclarator, destructorName, pureVirtualClause, virtualSpecifier, virtual uint16

	// declaration is the declaration kind; object marks the declarators
	// that make an object of their operand's type (pointer, array,
	// reference), which cRefine reads.
	declaration uint16
	object      []bool
	// conditional marks, by kind id, the nodes a file-scope declaration may
	// sit in without leaving file scope: every preprocessor conditional arm,
	// and in C++ a template declaration. clause marks the C++ clauses that
	// end a member function defined without a body (`= 0`, `= default`,
	// `= delete`).
	conditional, clause []bool
}

// fileScope reports whether a declaration whose nearest ancestor outside
// every conditional (cLinkSyntax.conditional) has the kind id is at file
// scope as the structural queries capture it: directly under the
// translation unit, under a linkage specification (`extern "C" int f(void);`)
// or its braced body, and in C++ also under a namespace body or a class
// body. cRefine and cLinker.declaration both decide by it, so a prototype the
// extraction declares is the one the linker exports.
func (x *cLinkSyntax) fileScope(id uint16) bool {
	return id == x.translationUnit || id == x.linkageSpecification || id == x.declarationList ||
		x.classSpecifier != 0 && id == x.fieldDeclarationList
}

// in reports whether id is marked in set.
func (x *cLinkSyntax) in(set []bool, id uint16) bool { return int(id) < len(set) && set[id] }

var (
	cLinkOnce, cppLinkOnce   sync.Once
	cLinkTable, cppLinkTable *cLinkSyntax
)

// cLinkSyntaxOf resolves language's table once; a name the grammar does not
// define panics, as cSyntaxOf's do.
func cLinkSyntaxOf(language string) *cLinkSyntax { return cLinkSyntaxIn(language, mustGrammar) }

// cLinkSyntaxIn is cLinkSyntaxOf with the grammar supplied by tl, which the
// extraction's declarator hook (cRefine) passes from the grammar it parses
// with: the grammar table holds that hook, so it may not look itself up.
func cLinkSyntaxIn(language string, tl func(string) *ts.Language) *cLinkSyntax {
	if language == "cpp" {
		cppLinkOnce.Do(func() { cppLinkTable = resolveCLinkSyntax(language, tl(language)) })
		return cppLinkTable
	}
	cLinkOnce.Do(func() { cLinkTable = resolveCLinkSyntax(language, tl(language)) })
	return cLinkTable
}

func resolveCLinkSyntax(language string, tl *ts.Language) *cLinkSyntax {
	kind := func(name string) uint16 { return mustKind(tl, language, name, true) }
	x := &cLinkSyntax{
		translationUnit:            kind("translation_unit"),
		functionDefinition:         kind("function_definition"),
		fieldDeclaration:           kind("field_declaration"),
		fieldDeclarationList:       kind("field_declaration_list"),
		typeDefinition:             kind("type_definition"),
		structSpecifier:            kind("struct_specifier"),
		unionSpecifier:             kind("union_specifier"),
		enumerator:                 kind("enumerator"),
		preprocFunctionDef:         kind("preproc_function_def"),
		preprocDef:                 kind("preproc_def"),
		parameterDeclaration:       kind("parameter_declaration"),
		argumentList:               kind("argument_list"),
		fieldDesignator:            kind("field_designator"),
		abstractFunctionDeclarator: kind("abstract_function_declarator"),
		paren:                      kind("parenthesized_expression"),
		ellipsis:                   mustKind(tl, language, "...", false),
		linkageSpecification:       kind("linkage_specification"),
		declarationList:            kind("declaration_list"),
		declaration:                kind("declaration"),
		object:                     make([]bool, tl.NodeKindCount()),
		conditional:                make([]bool, tl.NodeKindCount()),
		clause:                     make([]bool, tl.NodeKindCount()),
	}
	x.object[kind("pointer_declarator")], x.object[kind("array_declarator")] = true, true
	for _, name := range []string{"preproc_if", "preproc_ifdef", "preproc_else", "preproc_elif", "preproc_elifdef"} {
		x.conditional[kind(name)] = true
	}
	if language == "cpp" {
		x.classSpecifier, x.baseClassClause, x.templateType = kind("class_specifier"), kind("base_class_clause"), kind("template_type")
		x.trailingReturnType, x.placeholderTypeSpecifier = kind("trailing_return_type"), kind("placeholder_type_specifier")
		x.namespaceDefinition, x.aliasDeclaration = kind("namespace_definition"), kind("alias_declaration")
		x.optionalParameterDeclaration = kind("optional_parameter_declaration")
		x.variadicParameterDeclaration = kind("variadic_parameter_declaration")
		x.abstractReferenceDeclarator, x.destructorName = kind("abstract_reference_declarator"), kind("destructor_name")
		x.pureVirtualClause = kind("pure_virtual_clause")
		x.virtualSpecifier, x.virtual = kind("virtual_specifier"), mustKind(tl, language, "virtual", false)
		x.object[kind("reference_declarator")] = true
		x.conditional[kind("template_declaration")] = true
		for _, name := range []string{"pure_virtual_clause", "default_method_clause", "delete_method_clause"} {
			x.clause[kind(name)] = true
		}
	} else {
		x.variadicParameter = kind("variadic_parameter")
	}
	return x
}
