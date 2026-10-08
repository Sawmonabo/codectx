package worker

import (
	"cmp"
	"math"
	"slices"
	"strings"
	"sync"

	"github.com/Sawmonabo/codectx/internal/provider/treesitter/flow"
	"github.com/Sawmonabo/codectx/internal/provider/treesitter/wire"
)

// javaScriptLinker builds the link tables of one file of the ECMAScript
// family (language is "javascript", "typescript" or "tsx").
//
// # Sites
//
// A site is sent for exactly the calls the structural query captures as call
// references, so every site joins one: a call whose callee is an identifier
// (`f()`), a call whose callee is a member with a property name (`x.f()`,
// `this.f()`, `super.f()`, `x?.f()`), and `new C()` of an identifier. A test
// declaration's call (`it("…", …)`, `test`, `describe` with a leading
// string), which the extraction does not report as a call, is not a site.
// The shape is read from the callee and from what the receiver resolves to:
//
//   - `new C()` is ShapeConstructor;
//   - `f()` is ShapeBare when f names a declaration (a function declaration,
//     a variable initialized with a function or class, an import binding, or
//     a name the file does not bind), and ShapeIndirect when it is a
//     parameter or a variable holding some other value, with that value's
//     recovered type as Receiver and its declared function type as
//     Signature; a bare call of a named or default import carries the
//     import's TypeModule (see Imports) as Receiver;
//   - `x.f()` is ShapeStatic when x names a class, an enum, a TypeScript
//     namespace or a named or default import (Qualifier is x as written, and
//     Receiver the import's TypeModule for an import), and ShapeMember
//     otherwise, Receiver being x's recovered type: a namespace import's or
//     a required module's TypeModule, `this` the enclosing class, `super`
//     its supertypes (TypeSuper).
//
// # Imports
//
// The table carries no import list, so an import's origin travels in the
// types: a named import `import {f as g} from "./a"` is TypeModule
// "./a#f", the module as written, '#' and the exported name (`default`
// for a default import); a namespace import `import * as ns from "./a"`,
// `const m = require("./a")` and `import m = require("./a")` are TypeModule
// "./a". TypeParam i and Args[i] count source parameters and arguments from
// 0; a TypeScript `this` parameter is never one.
//
// # Type recovery
//
// Inside each callable (function) a value's type is traced through the
// callable's assignments over its def-use chains: an identifier resolves to
// the variable a flow node using it reads (the innermost node spanning it
// that uses a variable declared by that name), and its type is the merge of
// what each definition reaching that node assigns. A definition is read back
// from the syntax at its node's span: a declarator's annotation or
// initializer, an assignment's right side, a parameter (its annotation, else
// TypeParam by position), an import (its TypeModule, see Imports), a
// destructured property (TypeField of its key),
// a function declaration (TypeFunction). A may-definition through a property
// write, an update or a delete keeps the type the variable already held, so
// the definitions behind it are read instead; a closure's may-definition
// says nothing of the value and is unknown. Expressions give TypeNamed for
// `new C()`, an `as` or `<T>` assertion and an annotation, TypeResult for a
// call site's value, TypeModule for `require("m")`, TypeField for a property
// read, and the operand's type through await, `!`, `satisfies`, an
// assignment, a sequence's last element, and both arms of a conditional or
// a short-circuit operator. Definitions or arms that disagree merge to
// TypeUnknown: one Type is sent, and it is never a guess between two.
//
// A name a callable reads but does not declare resolves to the variable of
// that name in the innermost enclosing callable that declares one, with the
// merge of every definition that callable makes of it; a parameter of an
// enclosing callable is no parameter of this one, so its TypeParam is not
// carried in.
//
// # Values
//
// Every value written to a property (`o.f = v`, an object literal's member,
// a class field's initializer) whose type is recovered, and every function
// or imported value passed as an argument, assigned, or placed in an array
// literal, is a Value. A function is TypeFunction of its name as written: a
// function declaration's or function-valued variable's name, an object
// literal method's or a function-valued class field's name; an imported
// value is its TypeModule (see Imports).
type javaScriptLinker struct {
	language string
	// k and x are the grammar's kind tables and l its lowering, resolved on
	// the first call (ready); src is the file's bytes.
	k   *jsSyntax
	x   *jsLinkSyntax
	l   *Lowering
	src []byte

	// sites are the sites function recovered, by their callee token's first
	// byte; a TypeResult among them names its site by that byte too, until
	// file numbers the sites.
	sites map[uint32]wire.Site
	// returns are the types the callables return, by the callable's first
	// byte.
	returns map[uint32]wire.Type
	// values are the file's values in the order function found them.
	values []wire.Value
	// vars are every analysed callable's named variables by name, each with
	// the callable's span and the merge of its definitions, in the order the
	// callables were analysed: an enclosing callable before those it holds.
	vars map[string][]jsVar

	// The callable being analysed: its node, graph, the uses of its named
	// variables (uses, with ends the running maximum of each name's ends),
	// its def-use pairs keyed by the using node (reach), and the definitions
	// one query has visited (seen). g is nil outside function.
	fn    Node
	g     *flow.Graph
	uses  []jsUse
	ends  []uint32
	reach []uint64
	seen  map[uint64]bool
}

// jsUse is node n's use of the named variable v, declared by name, n
// spanning [start, end).
type jsUse struct {
	name       string
	start, end uint32
	v, n       int32
}

// jsVar is one named variable of the callable spanning at.
type jsVar struct {
	at  flow.Span
	val jsVal
}

// jsVal is what a name or a definition says of a value: its type, its
// declared function type (sig), whether anything was said (set: a
// definition seen twice in one query, or a variable nothing defines, says
// nothing), whether the name denotes a declaration a call binds by name
// (decl), and whether it names a class, enum, namespace or import that a
// member call goes through statically (static).
type jsVal struct {
	t                 wire.Type
	sig               string
	set, decl, static bool
}

// jsUnknown is a value something was said of and nothing recovered.
var jsUnknown = jsVal{set: true}

// typed is the value of type t.
func typed(t wire.Type) jsVal { return jsVal{t: t, set: true} }

// merge is the value a and b both describe: the one said of a value when the
// other says nothing, else their common type, signature and flags, a type
// or signature they disagree on being unknown.
func (a jsVal) merge(b jsVal) jsVal {
	switch {
	case !a.set:
		return b
	case !b.set:
		return a
	}
	if a.t != b.t {
		a.t = wire.Type{}
	}
	if a.sig != b.sig {
		a.sig = ""
	}
	a.decl = a.decl && b.decl
	a.static = a.static && b.static
	return a
}

// mergeType is merge over two types.
func mergeType(a, b wire.Type) wire.Type {
	if a != b {
		return wire.Type{}
	}
	return a
}

func newJavaScriptLinker(language string) *javaScriptLinker {
	return &javaScriptLinker{language: language}
}

// ready resolves the grammar's tables on the first call, inside the pass's
// recovery, and takes the file's bytes.
func (k *javaScriptLinker) ready(src []byte) {
	k.src = src
	if k.k != nil {
		return
	}
	switch k.language {
	case "javascript":
		k.k = jsJavaScript.syntax()
	case "typescript":
		k.k = jsTypeScript.syntax()
	case "tsx":
		k.k = jsTSX.syntax()
	default:
		panic("worker: " + k.language + " is not a language of the ECMAScript family")
	}
	k.x = jsLinkTables[k.language].syntax()
	k.l, _ = LoweringFor(k.language)
	k.sites = map[uint32]wire.Site{}
	k.returns = map[uint32]wire.Type{}
	k.vars = map[string][]jsVar{}
	k.seen = map[uint64]bool{}
}

// syntax is the grammar's lowering table, resolved once.
func (g *jsGrammar) syntax() *jsSyntax {
	g.once.Do(func() { g.s = resolveJSSyntax(g.language) })
	return g.s
}

func (k *javaScriptLinker) function(fn Node, g *flow.Graph, du flow.Edges, src []byte) {
	k.ready(src)
	k.fn, k.g = fn, g
	k.index(g, du)
	kk := k.k
	var ret jsVal
	if body := fn.ChildByFieldId(kk.fBody); jsKindOf(body) != jsNoKind && body.KindId() != kk.statementBlock && fn.KindId() == k.x.arrowFunction {
		k.query()
		ret = typed(k.typeOf(body))
	}
	k.own(fn, func(n Node) {
		switch n.KindId() {
		case kk.callExpression, kk.newExpression:
			if tok, ok := k.callee(n); ok {
				k.query()
				k.sites[uint32(tok.StartByte())] = k.site(n, tok)
			}
		case k.x.returnStatement:
			k.query()
			ret = ret.merge(typed(k.typeOf(firstNamed(n))))
		case kk.assignmentExpression:
			k.assigned(n)
		case kk.variableDeclarator:
			k.valued(n.ChildByFieldId(kk.fValue))
		case k.x.arguments, k.x.array:
			for c := n.first(); !c.IsNull(); c = c.next() {
				if c.IsNamed() && !c.IsExtra() {
					k.valued(c)
				}
			}
		case k.x.pair:
			if field := k.propertyName(n.ChildByFieldId(kk.fKey)); field != "" {
				k.written(n.ChildByFieldId(kk.fValue), field)
			}
		case k.x.shorthandPropertyIdentifier:
			k.query()
			if v := k.resolve(n); v.t.Kind != wire.TypeUnknown {
				k.values = append(k.values, wire.Value{At: wireSpan(spanOf(n)), Field: string(textOf(k.src, n)), Type: v.t})
			}
		case kk.methodDefinition:
			if n != fn && jsKindOf(n.Parent()) == k.x.object {
				if name := n.ChildByFieldId(kk.fName); jsKindOf(name) == k.x.propertyIdentifier {
					text := string(textOf(k.src, name))
					k.values = append(k.values, wire.Value{At: wireSpan(spanOf(n)), Field: text,
						Type: wire.Type{Kind: wire.TypeFunction, Name: text}})
				}
			}
		case kk.fieldDefinition, kk.publicFieldDefinition:
			k.field(n)
		}
	})
	if ret.set && ret.t.Kind != wire.TypeUnknown {
		k.returns[uint32(fn.StartByte())] = ret.t
	}
	k.summarize(fn, g)
	k.fn, k.g = Node{}, nil
}

// own calls visit with every node of fn's own code, in preorder: fn and its
// subtree, a nested callable or an ambient declaration visited but not
// entered, since its code is its own callable's or runs nowhere.
func (k *javaScriptLinker) own(fn Node, visit func(Node)) {
	c := fn.Walk()
	defer c.Close()
	for {
		n := c.Node()
		visit(n)
		if (n == fn || !k.l.isCallable(n) && !k.l.isAmbient(n)) && c.GotoFirstChild() {
			continue
		}
		for !c.GotoNextSibling() {
			if !c.GotoParent() {
				return
			}
		}
	}
}

// index builds the callable's lookup tables from g and du.
func (k *javaScriptLinker) index(g *flow.Graph, du flow.Edges) {
	k.uses = k.uses[:0]
	for u := range int32(g.Len()) {
		s := g.Span(u)
		for _, v := range g.Uses(u) {
			if d, named := g.Declared(v); named {
				k.uses = append(k.uses, jsUse{name: view(k.src[d.Start:d.End]), start: s.Start, end: s.End, v: v, n: u})
			}
		}
	}
	slices.SortFunc(k.uses, func(a, b jsUse) int {
		return cmp.Or(strings.Compare(a.name, b.name), cmp.Compare(a.start, b.start), cmp.Compare(b.end, a.end))
	})
	k.ends = slices.Grow(k.ends[:0], len(k.uses))[:len(k.uses)]
	for i, e := range k.uses {
		k.ends[i] = e.end
		if i > 0 && k.uses[i-1].name == e.name {
			k.ends[i] = max(k.ends[i-1], e.end)
		}
	}
	k.reach = k.reach[:0]
	for i := range du.Len() {
		p, u := du.At(i)
		k.reach = append(k.reach, uint64(uint32(u))<<32|uint64(uint32(p)))
	}
	slices.Sort(k.reach)
}

// query starts one recovery: no definition has been visited.
func (k *javaScriptLinker) query() { clear(k.seen) }

// useAt is the innermost node of the callable spanning s that uses a
// variable declared by name, and that variable.
func (k *javaScriptLinker) useAt(name []byte, s flow.Span) (n, v int32, ok bool) {
	key := view(name)
	at := func(e jsUse, start uint32) int {
		return cmp.Or(strings.Compare(e.name, key), cmp.Compare(e.start, start))
	}
	lo, _ := slices.BinarySearchFunc(k.uses, 0, at)
	hi, _ := slices.BinarySearchFunc(k.uses, s.Start+1, at)
	for i := hi - 1; i >= lo; i-- {
		if k.ends[i] < s.End {
			break
		}
		if e := k.uses[i]; e.end >= s.End {
			return e.n, e.v, true
		}
	}
	return 0, 0, false
}

// resolve is what the identifier id names: the variable of this callable a
// node using it reads, else the variable of that name an enclosing callable
// declares, else nothing (a global or a name the file does not bind).
func (k *javaScriptLinker) resolve(id Node) jsVal {
	name := textOf(k.src, id)
	if k.g != nil {
		if u, v, ok := k.useAt(name, spanOf(id)); ok {
			return k.reaching(u, v)
		}
	}
	return k.captured(view(name), spanOf(id))
}

// reaching merges what every definition of v reaching node u assigns.
func (k *javaScriptLinker) reaching(u, v int32) jsVal {
	var r jsVal
	lo, _ := slices.BinarySearch(k.reach, uint64(uint32(u))<<32)
	for i := lo; i < len(k.reach) && int32(k.reach[i]>>32) == u; i++ {
		p := int32(uint32(k.reach[i]))
		switch {
		case holds(k.g.Defs(p), v):
			r = r.merge(k.defined(p, v, false))
		case holds(k.g.MayDefs(p), v):
			r = r.merge(k.defined(p, v, true))
		}
	}
	return r
}

// defined is what node p's definition of v assigns, read from the syntax at
// p's span; may reports a may-definition. A definition already visited in
// this query says nothing more.
func (k *javaScriptLinker) defined(p, v int32, may bool) jsVal {
	key := uint64(uint32(p))<<32 | uint64(uint32(v))
	if k.seen[key] {
		return jsVal{}
	}
	k.seen[key] = true
	n := k.exact(k.g.Span(p))
	if n.IsNull() {
		return jsUnknown
	}
	if !may {
		return k.def(n)
	}
	kk := k.k
	switch n.KindId() {
	case kk.augmentedAssignmentExpression:
		// A logical or compound assignment to the name itself assigns it its
		// right side's value, or keeps the one it held.
		prior := k.reaching(p, v)
		if jsKindOf(k.l.unparen(n.ChildByFieldId(kk.fLeft))) == kk.identifier {
			return prior.merge(typed(k.typeOf(n.ChildByFieldId(kk.fRight))))
		}
		return prior
	case kk.assignmentExpression, kk.updateExpression, kk.unaryExpression, kk.memberExpression, kk.subscriptExpression:
		// A write through a property, an update, a delete, or a property
		// target of a for…in or for…of keeps the value the variable holds.
		return k.reaching(p, v)
	case kk.functionDeclaration, kk.generatorFunctionDeclaration:
		// The declaration's creation keeps its own name's function; any
		// other variable it may define is one its body assigns.
		if d, _ := k.g.Declared(v); spanOf(n.ChildByFieldId(kk.fName)) == d {
			return k.reaching(p, v)
		}
	}
	return jsUnknown
}

// exact is the innermost node of the callable spanning exactly s, or null.
func (k *javaScriptLinker) exact(s flow.Span) Node {
	var hit Node
	for n := k.fn; !n.IsNull(); {
		if spanOf(n) == s {
			hit = n
		}
		next := Node{}
		for c := n.first(); !c.IsNull(); c = c.next() {
			if uint32(c.StartByte()) <= s.Start && uint32(c.EndByte()) >= s.End && c.StartByte() < c.EndByte() {
				next = c
				break
			}
		}
		n = next
	}
	return hit
}

// def is what the killing definition whose node spans n assigns.
func (k *javaScriptLinker) def(n Node) jsVal {
	kk := k.k
	switch n.KindId() {
	case kk.identifier, kk.shorthandPropertyIdentifierPattern:
		return k.bound(n)
	case kk.variableDeclarator:
		return k.declarator(n)
	case kk.assignmentExpression:
		return typed(k.typeOf(n.ChildByFieldId(kk.fRight)))
	case kk.classDeclaration, kk.abstractClassDeclaration:
		return jsVal{set: true, decl: true, static: true}
	case kk.requiredParameter, kk.optionalParameter:
		// A parameter's default: the Branch spanning the whole parameter.
		return k.bound(n.ChildByFieldId(kk.fPattern))
	case kk.assignmentPattern:
		return k.bound(n.ChildByFieldId(kk.fLeft))
	}
	// A default's own node spans the default expression.
	return typed(k.typeOf(n))
}

// bound is what the identifier id, a name a definition binds, is bound to,
// read from where it stands.
func (k *javaScriptLinker) bound(id Node) jsVal {
	kk, x := k.k, k.x
	if jsKindOf(id) == jsNoKind {
		return jsUnknown
	}
	if id.KindId() == kk.shorthandPropertyIdentifierPattern {
		return typed(wire.Type{Kind: wire.TypeField, Name: string(textOf(k.src, id))})
	}
	if id.KindId() != kk.identifier {
		return jsUnknown
	}
	if i, wrapper, ok := k.paramOf(id); ok {
		return k.param(i, wrapper)
	}
	p := id.Parent()
	switch jsKindOf(p) {
	case kk.functionDeclaration, kk.generatorFunctionDeclaration:
		return jsVal{t: wire.Type{Kind: wire.TypeFunction, Name: string(textOf(k.src, id))}, set: true, decl: true}
	case kk.importClause:
		return jsVal{t: wire.Type{Kind: wire.TypeModule, Name: k.importSource(p) + "#default"}, set: true, decl: true, static: true}
	case kk.imports.importSpecifier:
		exported := p.ChildByFieldId(kk.imports.fName)
		name := string(textOf(k.src, exported))
		if exported.KindId() == x.str {
			name = k.unquote(exported)
		}
		return jsVal{t: wire.Type{Kind: wire.TypeModule, Name: k.importSource(p) + "#" + name}, set: true, decl: true, static: true}
	case kk.imports.namespaceImport:
		return jsVal{t: wire.Type{Kind: wire.TypeModule, Name: k.importSource(p)}, set: true, decl: true}
	case kk.importRequireClause:
		return jsVal{t: wire.Type{Kind: wire.TypeModule, Name: k.unquote(kindChild(p, x.str))}, set: true, decl: true}
	case kk.internalModule, kk.nestedIdentifier, kk.enumDeclaration:
		return jsVal{set: true, decl: true, static: true}
	case kk.variableDeclarator:
		return k.declarator(p)
	case kk.pairPattern:
		if p.ChildByFieldId(kk.fValue) == id {
			if key := k.propertyName(p.ChildByFieldId(kk.fKey)); key != "" {
				return typed(wire.Type{Kind: wire.TypeField, Name: key})
			}
		}
	case x.arrowFunction:
		if p == k.fn && p.ChildByFieldId(kk.fParameter) == id {
			return typed(wire.Type{Kind: wire.TypeParam})
		}
	}
	return jsUnknown
}

// paramOf reports whether id is a parameter of the callable being analysed:
// its position among the parameters, a TypeScript `this` parameter not
// counted, and the parameter node, the wrapper carrying its annotation.
func (k *javaScriptLinker) paramOf(id Node) (index uint32, wrapper Node, ok bool) {
	kk := k.k
	w, p := id, id.Parent()
	if jsKindOf(p) == kk.assignmentPattern && p.ChildByFieldId(kk.fLeft) == w {
		w, p = p, p.Parent()
	}
	if pk := jsKindOf(p); (pk == kk.requiredParameter || pk == kk.optionalParameter) && p.ChildByFieldId(kk.fPattern) == w {
		w, p = p, p.Parent()
	}
	if jsKindOf(p) != k.x.formalParameters || p.Parent() != k.fn {
		return 0, Node{}, false
	}
	for c := p.first(); !c.IsNull() && c != w; c = c.next() {
		if c.IsNamed() && !c.IsExtra() && !k.thisParam(c) {
			index++
		}
	}
	return index, w, true
}

// param is the value of parameter i: its annotation's type, else whatever
// its callers pass (TypeParam), with its declared function type.
func (k *javaScriptLinker) param(i uint32, wrapper Node) jsVal {
	v := k.annotated(kindChild(wrapper, k.x.typeAnnotation))
	if v.t.Kind == wire.TypeUnknown {
		v.t = wire.Type{Kind: wire.TypeParam, Index: i}
	}
	v.set = true
	return v
}

// thisParam reports a TypeScript `this` parameter, which declares the type
// of `this` and takes no argument.
func (k *javaScriptLinker) thisParam(p Node) bool {
	return p.KindId() == k.k.requiredParameter && jsKindOf(p.ChildByFieldId(k.k.fPattern)) == k.x.this
}

// declarator is what a variable declarator binds its name to: its
// annotation's type, else its initializer's; a function or class initializer
// makes the name a declaration.
func (k *javaScriptLinker) declarator(d Node) jsVal {
	kk := k.k
	v := k.annotated(kindChild(d, k.x.typeAnnotation))
	val := d.ChildByFieldId(kk.fValue)
	if jsKindOf(val) == jsNoKind {
		v.set = true
		return v
	}
	switch k.l.unparen(val).KindId() {
	case k.x.arrowFunction, kk.functionExpression, kk.generatorFunction:
		if name := d.ChildByFieldId(kk.fName); jsKindOf(name) == kk.identifier {
			return jsVal{t: wire.Type{Kind: wire.TypeFunction, Name: string(textOf(k.src, name))}, sig: k.signature(k.l.unparen(val)), set: true, decl: true}
		}
	case kk.class:
		return jsVal{set: true, decl: true, static: true}
	}
	if v.t.Kind == wire.TypeUnknown {
		v.t = k.typeOf(val)
	}
	v.set = true
	return v
}

// annotated is what a TypeScript annotation says: TypeNamed of a named type,
// the signature of a function type; nothing when there is none.
func (k *javaScriptLinker) annotated(a Node) jsVal {
	if jsKindOf(a) == jsNoKind {
		return jsVal{}
	}
	t := firstNamed(a)
	if jsKindOf(t) == k.x.functionType {
		return jsVal{set: true, sig: k.functionType(t)}
	}
	if name := k.namedType(t); name != "" {
		return typed(wire.Type{Kind: wire.TypeNamed, Name: name})
	}
	return jsVal{}
}

// captured is the variable name an enclosing callable of the position s
// declares: the merge of the innermost such callable's variables of that
// name, a parameter's TypeParam left out.
func (k *javaScriptLinker) captured(name string, s flow.Span) jsVal {
	vs := k.vars[name]
	var r jsVal
	var in flow.Span
	found := false
	for i := len(vs) - 1; i >= 0; i-- {
		e := vs[i]
		if found && e.at != in {
			break
		}
		if e.at.Start <= s.Start && s.End <= e.at.End {
			found, in = true, e.at
			r = r.merge(e.val)
		}
	}
	if r.t.Kind == wire.TypeParam {
		r.t = wire.Type{}
	}
	return r
}

// summarize records the callable's named variables with the merge of their
// definitions, for the callables nested in it.
func (k *javaScriptLinker) summarize(fn Node, g *flow.Graph) {
	vals := make([]jsVal, g.Vars())
	for u := range int32(g.Len()) {
		for _, v := range g.Defs(u) {
			if _, named := g.Declared(v); named {
				k.query()
				vals[v] = vals[v].merge(k.defined(u, v, false))
			}
		}
		for _, v := range g.MayDefs(u) {
			if _, named := g.Declared(v); named {
				k.query()
				vals[v] = vals[v].merge(k.defined(u, v, true))
			}
		}
	}
	at := spanOf(fn)
	for v := range int32(g.Vars()) {
		if d, named := g.Declared(v); named {
			name := string(k.src[d.Start:d.End])
			k.vars[name] = append(k.vars[name], jsVar{at: at, val: vals[v]})
		}
	}
}

// typeOf is the type of the expression e's value.
func (k *javaScriptLinker) typeOf(e Node) wire.Type {
	e = k.l.unparen(e)
	if e.IsNull() {
		return wire.Type{}
	}
	kk, x := k.k, k.x
	switch e.KindId() {
	case kk.identifier:
		return k.resolve(e).t
	case x.this:
		return k.thisAt(e)
	case kk.newExpression:
		if c := firstNamed(e); jsKindOf(c) == kk.identifier || jsKindOf(c) == kk.memberExpression {
			return wire.Type{Kind: wire.TypeNamed, Name: k.spell(c)}
		}
	case kk.callExpression:
		if tok, ok := k.callee(e); ok {
			if m, ok := k.required(e); ok {
				return wire.Type{Kind: wire.TypeModule, Name: m}
			}
			return wire.Type{Kind: wire.TypeResult, Index: uint32(tok.StartByte())}
		}
	case kk.memberExpression:
		if p := e.ChildByFieldId(kk.fProperty); jsKindOf(p) == x.propertyIdentifier {
			return wire.Type{Kind: wire.TypeField, Name: string(textOf(k.src, p))}
		}
	case kk.awaitExpression, kk.nonNullExpression:
		return k.typeOf(firstNamed(e))
	case kk.satisfiesExpression:
		return k.typeOf(firstNamed(e))
	case kk.asExpression:
		if name := k.namedType(e.NamedChild(1)); name != "" {
			return wire.Type{Kind: wire.TypeNamed, Name: name}
		}
		return k.typeOf(firstNamed(e))
	case kk.typeAssertion:
		if name := k.namedType(firstOf(kindChild(e, kk.typeArguments))); name != "" {
			return wire.Type{Kind: wire.TypeNamed, Name: name}
		}
		return k.typeOf(lastNamed(e))
	case kk.assignmentExpression:
		return k.typeOf(e.ChildByFieldId(kk.fRight))
	case kk.ternaryExpression:
		return mergeType(k.typeOf(e.ChildByFieldId(kk.fConsequence)), k.typeOf(e.ChildByFieldId(kk.fAlternative)))
	case kk.binaryExpression:
		if op := e.ChildByFieldId(kk.fOperator); !op.IsNull() {
			if o := op.KindId(); o == kk.and || o == kk.or || o == kk.nullish {
				return mergeType(k.typeOf(e.ChildByFieldId(kk.fLeft)), k.typeOf(e.ChildByFieldId(kk.fRight)))
			}
		}
	case kk.sequenceExpression:
		return k.typeOf(lastNamed(e))
	}
	return wire.Type{}
}

// thisAt is the type of `this` at n: the class whose method, field
// initializer or static block holds n, an arrow function taking its
// enclosing callable's `this`; unknown in any other function.
func (k *javaScriptLinker) thisAt(n Node) wire.Type {
	kk := k.k
	for p := n.Parent(); !p.IsNull(); p = p.Parent() {
		switch p.KindId() {
		case kk.classBody:
			return k.named(k.className(p))
		case kk.methodDefinition:
			if jsKindOf(p.Parent()) == kk.classBody {
				return k.named(k.className(p.Parent()))
			}
			return wire.Type{}
		case kk.functionDeclaration, kk.functionExpression, kk.generatorFunction, kk.generatorFunctionDeclaration, kk.program:
			return wire.Type{}
		}
	}
	return wire.Type{}
}

// named is TypeNamed of name, or unknown for no name.
func (k *javaScriptLinker) named(name string) wire.Type {
	if name == "" {
		return wire.Type{}
	}
	return wire.Type{Kind: wire.TypeNamed, Name: name}
}

// className is the name of the class whose body is body: its own name, or
// the variable an anonymous class expression initializes; empty for none.
func (k *javaScriptLinker) className(body Node) string {
	kk := k.k
	c := body.Parent()
	switch jsKindOf(c) {
	case kk.classDeclaration, kk.abstractClassDeclaration, kk.class:
		if name := c.ChildByFieldId(kk.fName); !name.IsNull() {
			return string(textOf(k.src, name))
		}
		if d := c.Parent(); jsKindOf(d) == kk.variableDeclarator {
			if name := d.ChildByFieldId(kk.fName); jsKindOf(name) == kk.identifier {
				return string(textOf(k.src, name))
			}
		}
	}
	return ""
}

// callee is the callee token of n when n is a call the structural query
// captures as a call reference.
func (k *javaScriptLinker) callee(n Node) (Node, bool) {
	kk := k.k
	switch n.KindId() {
	case kk.callExpression:
		f := n.ChildByFieldId(kk.fFunction)
		switch jsKindOf(f) {
		case kk.identifier:
			return f, true
		case kk.memberExpression:
			if p := f.ChildByFieldId(kk.fProperty); jsKindOf(p) == k.x.propertyIdentifier {
				return p, true
			}
		}
	case kk.newExpression:
		if c := firstNamed(n); jsKindOf(c) == kk.identifier {
			return c, true
		}
	}
	return Node{}, false
}

// testCall reports a call the extraction reports as a test declaration and
// not as a call: `it`, `test` or `describe` with a leading string argument.
func (k *javaScriptLinker) testCall(n Node) bool {
	kk := k.k
	if n.KindId() != kk.callExpression {
		return false
	}
	f := n.ChildByFieldId(kk.fFunction)
	if jsKindOf(f) != kk.identifier {
		return false
	}
	switch string(textOf(k.src, f)) {
	case "it", "test", "describe":
		return jsKindOf(firstOf(n.ChildByFieldId(kk.fArguments))) == k.x.str
	}
	return false
}

// required is the module a `require("m")` call loads.
func (k *javaScriptLinker) required(call Node) (string, bool) {
	kk := k.k
	f := call.ChildByFieldId(kk.fFunction)
	if jsKindOf(f) != kk.identifier || string(textOf(k.src, f)) != "require" {
		return "", false
	}
	if a := firstOf(call.ChildByFieldId(kk.fArguments)); jsKindOf(a) == k.x.str {
		return k.unquote(a), true
	}
	return "", false
}

// site is the call n's site, tok its callee token.
func (k *javaScriptLinker) site(n, tok Node) wire.Site {
	kk, x := k.k, k.x
	s := wire.Site{Name: wireSpan(spanOf(tok))}
	switch f := n.ChildByFieldId(kk.fFunction); {
	case n.KindId() == kk.newExpression:
		s.Shape = wire.ShapeConstructor
	case f == tok:
		s.Shape = wire.ShapeBare
		switch v := k.resolve(tok); {
		case v.set && !v.decl:
			s.Shape, s.Receiver, s.Signature = wire.ShapeIndirect, v.t, v.sig
		case v.t.Kind == wire.TypeModule:
			// A call of an import names its module and exported name.
			s.Receiver = v.t
		}
	default:
		s.Shape = wire.ShapeMember
		obj := k.l.unparen(f.ChildByFieldId(kk.fObject))
		switch jsKindOf(obj) {
		case x.super:
			if t := k.thisAt(obj); t.Kind == wire.TypeNamed {
				s.Receiver = wire.Type{Kind: wire.TypeSuper, Name: t.Name}
			}
		case kk.identifier:
			v := k.resolve(obj)
			if v.static {
				s.Shape, s.Qualifier = wire.ShapeStatic, string(textOf(k.src, obj))
			}
			if !v.static || v.t.Kind == wire.TypeModule {
				s.Receiver = v.t
			}
		case jsNoKind:
		default:
			s.Receiver = k.typeOf(obj)
		}
	}
	if a := n.ChildByFieldId(kk.fArguments); jsKindOf(a) == x.arguments {
		for c := a.first(); !c.IsNull(); c = c.next() {
			switch {
			case !c.IsNamed() || c.IsExtra():
			case c.KindId() == kk.spreadElement:
				s.Args = append(s.Args, wire.Type{})
			default:
				s.Args = append(s.Args, k.typeOf(c))
			}
		}
	}
	return s
}

// assigned records the value an assignment writes to a property, or the
// function it assigns to a variable.
func (k *javaScriptLinker) assigned(n Node) {
	kk := k.k
	left := k.l.unparen(n.ChildByFieldId(kk.fLeft))
	right := n.ChildByFieldId(kk.fRight)
	if jsKindOf(left) == kk.memberExpression {
		if p := left.ChildByFieldId(kk.fProperty); jsKindOf(p) == k.x.propertyIdentifier {
			k.written(right, string(textOf(k.src, p)))
		}
		return
	}
	k.valued(right)
}

// written records the value v written to the property field, when its type
// is recovered.
func (k *javaScriptLinker) written(v Node, field string) {
	if jsKindOf(v) == jsNoKind {
		return
	}
	k.query()
	if t := k.typeOf(v); t.Kind != wire.TypeUnknown {
		k.values = append(k.values, wire.Value{At: wireSpan(spanOf(v)), Field: field, Type: t})
	}
}

// valued records e when its value is a function taken as a value, or a
// value an import binds.
func (k *javaScriptLinker) valued(e Node) {
	if jsKindOf(e) == jsNoKind {
		return
	}
	k.query()
	if t := k.typeOf(e); t.Kind == wire.TypeFunction || t.Kind == wire.TypeModule {
		k.values = append(k.values, wire.Value{At: wireSpan(spanOf(e)), Type: t})
	}
}

// field records a class field's initializer as a value written to the
// field: a function-valued field holds the function the field declares.
func (k *javaScriptLinker) field(n Node) {
	name, value := k.fieldParts(n)
	if name == "" || jsKindOf(value) == jsNoKind {
		return
	}
	switch k.l.unparen(value).KindId() {
	case k.x.arrowFunction, k.k.functionExpression:
		k.values = append(k.values, wire.Value{At: wireSpan(spanOf(value)), Field: name, Type: wire.Type{Kind: wire.TypeFunction, Name: name}})
	default:
		k.written(value, name)
	}
}

// fieldParts is a class field's name, when it is a property name, and its
// initializer.
func (k *javaScriptLinker) fieldParts(n Node) (string, Node) {
	kk := k.k
	key := kk.fName
	if n.KindId() == kk.fieldDefinition {
		key = kk.fProperty
	}
	name := n.ChildByFieldId(key)
	if jsKindOf(name) != k.x.propertyIdentifier {
		return "", Node{}
	}
	return string(textOf(k.src, name)), n.ChildByFieldId(kk.fValue)
}

// propertyName is an object key or a pattern key as written: a property
// name, or a string's contents; empty for a computed or numeric key.
func (k *javaScriptLinker) propertyName(key Node) string {
	switch jsKindOf(key) {
	case k.x.propertyIdentifier:
		return string(textOf(k.src, key))
	case k.x.str:
		return k.unquote(key)
	}
	return ""
}

func (k *javaScriptLinker) file(root Node, src []byte, out *wire.Link) {
	k.ready(src)
	kk, x := k.k, k.x
	numbered := map[uint32]uint32{}
	c := root.Walk()
	defer c.Close()
	for {
		n := c.Node()
		switch n.KindId() {
		case kk.callExpression, kk.newExpression:
			if tok, ok := k.callee(n); ok && !k.testCall(n) {
				at := uint32(tok.StartByte())
				s, ok := k.sites[at]
				if !ok {
					// The callable holding the call was not analysed: the
					// site keeps its syntax and what enclosing callables
					// recovered.
					k.query()
					s = k.site(n, tok)
				}
				numbered[at] = uint32(len(out.Sites))
				out.Sites = append(out.Sites, s)
			}
		case kk.functionDeclaration, kk.generatorFunctionDeclaration:
			if name := n.ChildByFieldId(kk.fName); jsKindOf(name) == kk.identifier {
				k.export(out, name, n, "", true, false)
			}
		case x.functionSignature:
			if name := n.ChildByFieldId(kk.fName); jsKindOf(name) == kk.identifier {
				k.export(out, name, n, "", true, false)
			}
		case kk.methodDefinition, x.methodSignature, x.abstractMethodSignature:
			if name := n.ChildByFieldId(kk.fName); jsKindOf(name) == x.propertyIdentifier {
				abstract := n.KindId() != kk.methodDefinition
				k.export(out, name, n, k.receiverOf(n), !abstract && hasToken(n, kk.staticKw), abstract)
			}
		case kk.variableDeclarator:
			name, value := n.ChildByFieldId(kk.fName), n.ChildByFieldId(kk.fValue)
			if jsKindOf(name) == kk.identifier && (jsKindOf(value) == x.arrowFunction || jsKindOf(value) == kk.functionExpression) {
				k.export(out, name, value, "", true, false)
			}
		case kk.fieldDefinition, kk.publicFieldDefinition:
			if _, value := k.fieldParts(n); jsKindOf(value) == x.arrowFunction || jsKindOf(value) == kk.functionExpression {
				key := kk.fName
				if n.KindId() == kk.fieldDefinition {
					key = kk.fProperty
				}
				k.export(out, n.ChildByFieldId(key), value, k.className(n.Parent()), hasToken(n, kk.staticKw), false)
			}
		case kk.classDeclaration, kk.abstractClassDeclaration, kk.class:
			k.class(out, n)
		case x.interfaceDeclaration:
			k.supertypes(out, n.ChildByFieldId(kk.fName), kindChild(n, x.extendsTypeClause))
		}
		if c.GotoFirstChild() {
			continue
		}
		for !c.GotoNextSibling() {
			if !c.GotoParent() {
				k.finish(out, numbered)
				return
			}
		}
	}
}

// finish numbers every TypeResult by the site it names, a site not sent
// making it unknown, and appends the values in source order.
func (k *javaScriptLinker) finish(out *wire.Link, numbered map[uint32]uint32) {
	number := func(t *wire.Type) {
		if t.Kind != wire.TypeResult {
			return
		}
		if i, ok := numbered[t.Index]; ok {
			t.Index = i
		} else {
			*t = wire.Type{}
		}
	}
	for i := range out.Sites {
		s := &out.Sites[i]
		number(&s.Receiver)
		for j := range s.Args {
			number(&s.Args[j])
		}
	}
	for i := range out.Exports {
		number(&out.Exports[i].Returns)
	}
	slices.SortStableFunc(k.values, func(a, b wire.Value) int {
		return cmp.Or(cmp.Compare(a.At.Start, b.At.Start), cmp.Compare(a.At.End, b.At.End))
	})
	for _, v := range k.values {
		number(&v.Type)
		if v.Type.Kind != wire.TypeUnknown {
			out.Values = append(out.Values, v)
		}
	}
}

// export appends the Export of the declaration named name whose callable is
// fn.
func (k *javaScriptLinker) export(out *wire.Link, name, fn Node, receiver string, static, abstract bool) {
	params, variadic := k.params(fn)
	out.Exports = append(out.Exports, wire.Export{Name: wireSpan(spanOf(name)), Receiver: receiver,
		Signature: k.signature(fn), Params: params, Variadic: variadic, Static: static, Abstract: abstract,
		Returns: k.returnType(fn)})
}

// class appends a class's Export, its constructor's parameters and the
// instance it creates, and its declared supertypes.
func (k *javaScriptLinker) class(out *wire.Link, n Node) {
	kk, x := k.k, k.x
	body := n.ChildByFieldId(kk.fBody)
	if jsKindOf(body) == jsNoKind {
		return
	}
	name := k.className(body)
	if id := n.ChildByFieldId(kk.fName); n.KindId() != kk.class && !id.IsNull() {
		e := wire.Export{Name: wireSpan(spanOf(id)), Returns: k.named(name)}
		for m := body.first(); !m.IsNull(); m = m.next() {
			if m.KindId() == kk.methodDefinition {
				if mn := m.ChildByFieldId(kk.fName); jsKindOf(mn) == x.propertyIdentifier && string(textOf(k.src, mn)) == "constructor" {
					e.Params, e.Variadic = k.params(m)
					e.Signature = k.signature(m)
				}
			}
		}
		out.Exports = append(out.Exports, e)
	}
	if name == "" {
		return
	}
	h := kindChild(n, kk.classHeritage)
	if h.IsNull() {
		return
	}
	for c := h.first(); !c.IsNull(); c = c.next() {
		switch {
		case !c.IsNamed() || c.IsExtra():
		case c.KindId() == kk.extendsClause:
			if v := c.ChildByFieldId(kk.fValue); !v.IsNull() {
				out.Supertypes = append(out.Supertypes, wire.Supertype{Type: name, Super: k.spell(v)})
			}
		case c.KindId() == x.implementsClause:
			k.supertypeList(out, name, c)
		default:
			// The javascript grammar's heritage is the extended expression.
			out.Supertypes = append(out.Supertypes, wire.Supertype{Type: name, Super: k.spell(c)})
		}
	}
}

// supertypes appends an interface's extended interfaces.
func (k *javaScriptLinker) supertypes(out *wire.Link, name, clause Node) {
	if name.IsNull() || clause.IsNull() {
		return
	}
	k.supertypeList(out, string(textOf(k.src, name)), clause)
}

// supertypeList appends each type a heritage clause lists as a supertype of
// name.
func (k *javaScriptLinker) supertypeList(out *wire.Link, name string, clause Node) {
	for t := clause.first(); !t.IsNull(); t = t.next() {
		if t.IsNamed() && !t.IsExtra() {
			if super := k.namedType(t); super != "" {
				out.Supertypes = append(out.Supertypes, wire.Supertype{Type: name, Super: super})
			}
		}
	}
}

// receiverOf is the type a method or method signature is declared on: its
// class, the variable its object literal initializes, its interface, or its
// object type's alias; empty for none.
func (k *javaScriptLinker) receiverOf(m Node) string {
	kk, x := k.k, k.x
	p := m.Parent()
	switch jsKindOf(p) {
	case kk.classBody:
		return k.className(p)
	case x.object:
		if d := p.Parent(); jsKindOf(d) == kk.variableDeclarator {
			if name := d.ChildByFieldId(kk.fName); jsKindOf(name) == kk.identifier {
				return string(textOf(k.src, name))
			}
		}
	case x.interfaceBody, x.objectType:
		if d := p.Parent(); jsKindOf(d) == x.interfaceDeclaration || jsKindOf(d) == x.typeAliasDeclaration {
			if name := d.ChildByFieldId(kk.fName); !name.IsNull() {
				return string(textOf(k.src, name))
			}
		}
	}
	return ""
}

// params is the number of fn's parameters, a TypeScript `this` parameter
// not counted, and whether the last is a rest parameter.
func (k *javaScriptLinker) params(fn Node) (n uint32, variadic bool) {
	kk := k.k
	if fn.KindId() == k.x.arrowFunction && !fn.ChildByFieldId(kk.fParameter).IsNull() {
		return 1, false
	}
	ps := fn.ChildByFieldId(kk.fParameters)
	if ps.IsNull() {
		return 0, false
	}
	for p := ps.first(); !p.IsNull(); p = p.next() {
		if !p.IsNamed() || p.IsExtra() || k.thisParam(p) {
			continue
		}
		n++
		rest := p
		if p.KindId() == kk.requiredParameter {
			rest = p.ChildByFieldId(kk.fPattern)
		}
		variadic = jsKindOf(rest) == kk.restPattern
	}
	return n, variadic
}

// signature is fn's parameter and result types as written, by the rule of
// wire.Export.Signature; empty when fn declares no type, as every
// JavaScript function does.
func (k *javaScriptLinker) signature(fn Node) string {
	ps := fn.ChildByFieldId(k.k.fParameters)
	ret := k.returnAnnotation(fn)
	types, declared := k.paramTypes(ps)
	result := ""
	if !ret.IsNull() {
		result, declared = k.spell(firstNamed(ret)), true
	}
	if !declared {
		return ""
	}
	return "(" + types + ")(" + result + ")"
}

// functionType is a TypeScript function type's signature, spelled as
// signature spells a function's.
func (k *javaScriptLinker) functionType(t Node) string {
	ps := t.ChildByFieldId(k.k.fParameters)
	types, _ := k.paramTypes(ps)
	result := ""
	after := false
	for c := t.first(); !c.IsNull(); c = c.next() {
		switch {
		case !c.IsNamed() || c.IsExtra():
		case c == ps:
			after = true
		case after:
			result = k.spell(c)
			after = false
		}
	}
	return "(" + types + ")(" + result + ")"
}

// paramTypes is the annotated types of the parameter list ps joined by ",",
// a parameter without one spelled empty, and whether any is annotated.
func (k *javaScriptLinker) paramTypes(ps Node) (string, bool) {
	if ps.IsNull() {
		return "", false
	}
	var b strings.Builder
	declared, first := false, true
	for p := ps.first(); !p.IsNull(); p = p.next() {
		if !p.IsNamed() || p.IsExtra() || k.thisParam(p) {
			continue
		}
		if !first {
			b.WriteByte(',')
		}
		first = false
		if a := kindChild(p, k.x.typeAnnotation); !a.IsNull() {
			b.WriteString(k.spell(firstNamed(a)))
			declared = true
		}
	}
	return b.String(), declared
}

// returnAnnotation is fn's declared return type annotation, or null.
func (k *javaScriptLinker) returnAnnotation(fn Node) Node {
	x := k.x
	for c := fn.first(); !c.IsNull(); c = c.next() {
		if id := c.KindId(); c.IsNamed() && (id == x.typeAnnotation || id == x.assertsAnnotation || id == x.typePredicateAnnotation) {
			return c
		}
	}
	return Node{}
}

// returnType is what fn returns: its declared named result type, else the
// type its return statements' values agree on.
func (k *javaScriptLinker) returnType(fn Node) wire.Type {
	if a := k.returnAnnotation(fn); !a.IsNull() {
		return k.named(k.namedType(firstNamed(a)))
	}
	return k.returns[uint32(fn.StartByte())]
}

// namedType is the name a TypeScript type node names: a type name, a
// qualified type name or a predefined type as written, a generic type's
// name without its arguments; empty for any other type.
func (k *javaScriptLinker) namedType(t Node) string {
	x := k.x
	switch jsKindOf(t) {
	case x.genericType:
		return k.namedType(t.ChildByFieldId(k.k.fName))
	case x.typeIdentifier, x.nestedTypeIdentifier, x.predefinedType, k.k.identifier:
		return k.spell(t)
	}
	return ""
}

// importSource is the module an import statement holding n loads.
func (k *javaScriptLinker) importSource(n Node) string {
	for ; !n.IsNull(); n = n.Parent() {
		if n.KindId() == k.k.importStatement {
			return k.unquote(kindChild(n, k.x.str))
		}
	}
	return ""
}

// unquote is a string literal's contents.
func (k *javaScriptLinker) unquote(s Node) string {
	if s.IsNull() {
		return ""
	}
	return strings.Trim(string(textOf(k.src, s)), "\"'`")
}

// spell is n's tokens as written, without whitespace or comments.
func (k *javaScriptLinker) spell(n Node) string {
	var b strings.Builder
	c := n.Walk()
	defer c.Close()
	for {
		if t := c.Node(); !t.IsExtra() {
			if c.GotoFirstChild() {
				continue
			}
			b.Write(textOf(k.src, t))
		}
		for !c.GotoNextSibling() {
			if !c.GotoParent() {
				return b.String()
			}
		}
	}
}

// kindChild is n's first named child of kind id, or null; a null n has
// none.
func kindChild(n Node, id uint16) Node {
	if n.IsNull() || id == 0 {
		return Node{}
	}
	for c := n.first(); !c.IsNull(); c = c.next() {
		if c.IsNamed() && c.KindId() == id {
			return c
		}
	}
	return Node{}
}

// firstOf is firstNamed of n, null for a null n.
func firstOf(n Node) Node {
	if n.IsNull() {
		return n
	}
	return firstNamed(n)
}

// jsNoKind is the kind jsKindOf answers for a null node: no kind of any
// grammar, and not the 0 an optional kind a grammar lacks resolves to.
const jsNoKind = math.MaxUint16 - 1

// jsKindOf is n's kind, or jsNoKind for a null n.
func jsKindOf(n Node) uint16 {
	if n.IsNull() {
		return jsNoKind
	}
	return n.KindId()
}

// jsLinkSyntax is the kinds the link tables read beyond the lowering's
// table. Each is optional: in a grammar without it its id is 0, which no
// named node has, so the construct cannot occur there (the javascript
// grammar has no TypeScript kind).
type jsLinkSyntax struct {
	arguments, formalParameters, propertyIdentifier, this, super, str, pair, object,
	shorthandPropertyIdentifier, array, returnStatement, arrowFunction, typeAnnotation, assertsAnnotation,
	typePredicateAnnotation, typeIdentifier, nestedTypeIdentifier, genericType, predefinedType, functionType,
	implementsClause, extendsTypeClause, interfaceDeclaration, interfaceBody, objectType, typeAliasDeclaration,
	methodSignature, abstractMethodSignature, functionSignature uint16
}

// jsLinkGrammar is one grammar's jsLinkSyntax, resolved once, on first use.
type jsLinkGrammar struct {
	language string
	once     sync.Once
	s        jsLinkSyntax
}

// jsLinkTables are the link tables' kind tables of the ECMAScript grammars.
var jsLinkTables = map[string]*jsLinkGrammar{
	"javascript": {language: "javascript"},
	"typescript": {language: "typescript"},
	"tsx":        {language: "tsx"},
}

// syntax is the grammar's table.
func (g *jsLinkGrammar) syntax() *jsLinkSyntax {
	g.once.Do(func() {
		tl := mustGrammar(g.language)
		k := func(name string) uint16 { return tl.IdForNodeKind(name, true) }
		g.s = jsLinkSyntax{
			arguments: k("arguments"), formalParameters: k("formal_parameters"),
			propertyIdentifier: k("property_identifier"), this: k("this"), super: k("super"), str: k("string"),
			pair: k("pair"), object: k("object"), shorthandPropertyIdentifier: k("shorthand_property_identifier"),
			array: k("array"), returnStatement: k("return_statement"), arrowFunction: k("arrow_function"),
			typeAnnotation: k("type_annotation"), assertsAnnotation: k("asserts_annotation"),
			typePredicateAnnotation: k("type_predicate_annotation"), typeIdentifier: k("type_identifier"),
			nestedTypeIdentifier: k("nested_type_identifier"), genericType: k("generic_type"),
			predefinedType: k("predefined_type"), functionType: k("function_type"),
			implementsClause: k("implements_clause"), extendsTypeClause: k("extends_type_clause"),
			interfaceDeclaration: k("interface_declaration"), interfaceBody: k("interface_body"),
			objectType: k("object_type"), typeAliasDeclaration: k("type_alias_declaration"),
			methodSignature: k("method_signature"), abstractMethodSignature: k("abstract_method_signature"),
			functionSignature: k("function_signature"),
		}
	})
	return &g.s
}
