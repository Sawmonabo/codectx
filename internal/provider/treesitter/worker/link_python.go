package worker

import (
	"cmp"
	"slices"
	"strconv"
	"strings"

	"github.com/Sawmonabo/codectx/internal/provider/treesitter/flow"
	"github.com/Sawmonabo/codectx/internal/provider/treesitter/wire"
)

// pythonLinker builds the link tables of one Python file.
//
// Each callable's own code (pythonLowering's callables: the module, a def, a
// lambda, a class body, a comprehension) is read once, in function, with its
// flow graph: every call the structural query captures (a callee that is a
// name, or an attribute whose name is an identifier) becomes a site, and its
// callee, receiver and arguments are traced through the callable's
// assignments over its def-use chains to what defines them. The parts of a
// nested callable that the enclosing callable evaluates (decorators,
// defaults, annotations, bases, a comprehension's first iterable) are the
// enclosing callable's code, as the lowering treats them.
//
// What a value is, as the trace recovers it:
//
//   - a parameter: its annotation (TypeNamed, a string forward reference
//     unquoted), or TypeParam with its index; a method's first parameter
//     (self, or cls of a classmethod) is the receiver, the enclosing class
//     (TypeNamed), and is not counted, so a site's Args[i] and TypeParam i
//     both name the i-th parameter after the receiver; a keyword-only or
//     splat parameter is TypeUnknown;
//   - an assignment or `:=`: its annotation, else the value's type;
//   - a call that is a site: TypeResult of that site (the linker decides
//     whether it constructs a class);
//   - `x.f` read as a value: a method value when x is a class of the file,
//     or an instance of one (self included), whose body declares a def f
//     that is no property: TypeFunction of that def's qualified name (`C.f`
//     for a top-level class C), the name the declaration table carries;
//     otherwise TypeField f; `super()` as a receiver: TypeSuper of the
//     enclosing class;
//   - a name `import a.b` binds (a) or `import a.b as c` binds (c): the
//     module (TypeModule "a" or "a.b"); a name `from m import f as g` binds:
//     TypeModule "m.f", the imported name spelled through its module so an
//     alias is unwound (a relative module keeps its dots: ".b.f");
//   - a def: TypeFunction of its name, where it is taken as a value;
//   - a class named as a value or a receiver: TypeClass of the class, the
//     class object itself, whose call of a method binds the method the
//     class declares or inherits and never a subclass's override.
//
// A name that is not a variable of the callable (a free name of a nested
// callable, a builtin) is read from the enclosing function, lambda and
// comprehension scopes, innermost first, and then from the module's own
// bindings; a class body is no enclosing scope, and a name the callable
// declares global is read from the module alone. What an enclosing scope's
// variable is, is every definition of it joined, recorded when that scope
// was read (it is read first: the callables come in preorder); a closure
// reads its free variables when it is called, so any of them may reach it.
// An enclosing parameter's index means nothing to the closure, so a
// recovered TypeParam is recorded as a value of unknown type. Where
// the definitions reaching a read disagree the type is a TypeUnion of every
// type they recover (Link.Unions, never nested); a definition that recovers
// nothing does not count against one that does.
//
// Shapes: a name callee is ShapeIndirect when it is a variable the callable,
// an enclosing scope or the module assigns a value to (a parameter, an
// assignment, a function or method assigned as a value), carrying the
// value's type as Receiver, and ShapeBare otherwise (a def, a class, an
// import, a builtin), a from-imported name with its TypeModule. An attribute
// callee is ShapeStatic exactly when its object is an imported module (`mod.f()`,
// `a.b.f()`), Qualifier the object as written and Receiver the module, and
// ShapeMember otherwise, with the object's type as Receiver.
//
// Values: every function written to an attribute or to a name in a class
// body, and every function passed or assigned as a value. A def a class body
// declares is written to no attribute a value is read from, so it is no
// Value: a call reaches a method through its receiver's type, never through
// its name alone.
type pythonLinker struct {
	k *pySyntax
	// moduleNames are the names the module's own code binds, read once on
	// the first callable: what a nested callable's free name refers to.
	moduleNames map[string]pyVal
	modulesRead bool
	// scopes are what each read function, lambda or comprehension that
	// nests a callable binds, by the callable's span: what a free name of
	// the callables nested in it refers to.
	scopes map[flow.Span]map[string]pyRead
	// methods maps `C.m`, a class's name and the name of a def its body
	// declares (no property), to the qualified names of every such def in
	// the file; built on the first method value read.
	methods     map[string][]string
	methodsRead bool
	// sites and values are the file's, in the order function found them;
	// a TypeResult indexes sites.
	sites  []wire.Site
	values []wire.Value
	// returns are the types the def whose name token starts at the key
	// returns, recovered from its return statements.
	returns map[uint32]wire.Type
	// unions are the file's type sets a TypeUnion indexes, each sorted and
	// distinct; unionAt finds a set's index by its members' spelling.
	unions  [][]wire.Type
	unionAt map[string]uint32

	// The callable being read; every field below is reset per callable and
	// holds nothing past function.
	g     *flow.Graph
	src   []byte
	fn    Node
	class string // the class enclosing fn's code as self sees it, or ""
	// names maps a named variable's spelling to the variable.
	names map[string]int32
	// uses are (variable, node) pairs of g's reads sorted by variable then by
	// the node's span; defs the def-use pairs packed use<<32|def, sorted.
	uses []pyUse
	defs []uint64
	// binds are the definitions the trace reads, by the defining node's span.
	binds map[flow.Span]pyBind
	// memo is each defining node's traced value; state 1 is in progress.
	memo []pyMemo
	// calls, assigns and rets are fn's own sites, assignments and return
	// values; callAt maps a site's call span to its index in sites.
	calls   []Node
	assigns []Node
	rets    []Node
	callAt  map[flow.Span]uint32
	// nests reports that fn's code creates a nested callable.
	nests bool
	// globals are the names fn declares global, read on the first free
	// name looked up past fn.
	globals     map[string]bool
	globalsRead bool
}

func newPythonLinker() *pythonLinker {
	return &pythonLinker{returns: map[uint32]wire.Type{}, unionAt: map[string]uint32{}, names: map[string]int32{},
		binds: map[flow.Span]pyBind{}, callAt: map[flow.Span]uint32{}, scopes: map[flow.Span]map[string]pyRead{},
		globals: map[string]bool{}}
}

// pyOrigin says what kind of thing a pyVal is.
type pyOrigin byte

const (
	// pyUnknown is a value nothing was recovered for.
	pyUnknown pyOrigin = iota
	// pyValue is a value whose type t holds.
	pyValue
	// pyModule is a module, t its TypeModule.
	pyModule
	// pyImported is a name an import-from binds, t its TypeModule.
	pyImported
	// pyClass and pyFunction are a class or a def named as a value, t.Name
	// its name.
	pyClass
	pyFunction
)

// pyVal is what the trace recovered for an expression.
type pyVal struct {
	o pyOrigin
	t wire.Type
}

// pyBind is one definition: its value when it is known without the trace
// (a parameter, an import, a def), or the expression whose value it binds
// and the annotation that overrides it.
type pyBind struct {
	val       pyVal
	expr, ann Node
}

type pyUse struct {
	v, node    int32
	start, end uint32
}

// pyRead is what a read of a variable refers to: the definitions' values
// joined, and whether a value is assigned to it (a parameter, an assignment,
// anything but a def, class or import statement), which makes a call of it
// indirect.
type pyRead struct {
	val      pyVal
	variable bool
}

// reach folds definition p of a variable into r.
func (k *pythonLinker) reach(r pyRead, p int32) pyRead {
	x := k.defined(p)
	if x.o == pyValue || x.o == pyUnknown || (x.o == pyFunction && !k.binds[k.g.Span(p)].expr.IsNull()) {
		r.variable = true
	}
	r.val = k.join(r.val, x)
	return r
}

// read is what the read refers to, and whether it is a variable: a module,
// an imported name or a class is never called indirectly.
func (r pyRead) read() (pyVal, bool) {
	switch r.val.o {
	case pyValue, pyUnknown, pyFunction:
		return r.val, r.variable
	}
	return r.val, false
}

type pyMemo struct {
	state byte
	val   pyVal
}

// join merges two recovered values: equal ones stay, one that recovers no
// type yields to the other, and two that disagree are the union of every
// type either recovers, a value of the file's unions.
func (k *pythonLinker) join(a, b pyVal) pyVal {
	switch {
	case a.o == pyUnknown:
		return b
	case b.o == pyUnknown || a == b:
		return a
	}
	members := k.members(a, nil)
	n := len(members)
	members = k.members(b, members)
	slices.SortFunc(members, compareType)
	members = slices.Compact(members)
	switch {
	case len(members) == 0:
		return pyVal{}
	case len(members) == 1 && n == 1:
		return a
	case len(members) == 1:
		return b
	}
	return k.union(members)
}

// members appends the types v recovers to dst: a union's members, another
// value's own type, and nothing for a value of unknown type.
func (k *pythonLinker) members(v pyVal, dst []wire.Type) []wire.Type {
	t := v.asValue()
	switch t.Kind {
	case wire.TypeUnknown:
		return dst
	case wire.TypeUnion:
		return append(dst, k.unions[t.Index]...)
	}
	return append(dst, t)
}

// union is the value whose type is every one of members, sorted and
// distinct, two or more and none a union: the file's union of them, added
// on first use.
func (k *pythonLinker) union(members []wire.Type) pyVal {
	var key strings.Builder
	for _, m := range members {
		key.WriteByte(byte(m.Kind))
		key.WriteString(strconv.FormatUint(uint64(m.Index), 10))
		key.WriteByte(0)
		key.WriteString(m.Name)
		key.WriteByte(0)
	}
	i, ok := k.unionAt[key.String()]
	if !ok {
		i = uint32(len(k.unions))
		k.unions = append(k.unions, slices.Clone(members))
		k.unionAt[key.String()] = i
	}
	return pyVal{o: pyValue, t: wire.Type{Kind: wire.TypeUnion, Index: i}}
}

func compareType(a, b wire.Type) int {
	return cmp.Or(cmp.Compare(a.Kind, b.Kind), cmp.Compare(a.Index, b.Index), strings.Compare(a.Name, b.Name))
}

// withoutParams is v without the TypeParam it recovers, alone or in a union:
// what a nested callable reads of an enclosing callable's variable, to which
// the enclosing callable's parameter indices mean nothing.
func (k *pythonLinker) withoutParams(v pyVal) pyVal {
	if v.o != pyValue {
		return v
	}
	switch v.t.Kind {
	case wire.TypeParam:
		return pyVal{o: pyValue}
	case wire.TypeUnion:
		kept := slices.DeleteFunc(slices.Clone(k.unions[v.t.Index]), func(t wire.Type) bool { return t.Kind == wire.TypeParam })
		switch len(kept) {
		case 0:
			return pyVal{o: pyValue}
		case 1:
			return pyVal{o: pyValue, t: kept[0]}
		}
		return k.union(kept)
	}
	return v
}

// asValue is v's type where it is passed, assigned or returned.
func (v pyVal) asValue() wire.Type {
	switch v.o {
	case pyValue, pyModule, pyImported:
		return v.t
	case pyFunction:
		return wire.Type{Kind: wire.TypeFunction, Name: v.t.Name}
	case pyClass:
		return wire.Type{Kind: wire.TypeClass, Name: v.t.Name}
	}
	return wire.Type{}
}

func (k *pythonLinker) function(fn Node, g *flow.Graph, du flow.Edges, src []byte) {
	if k.k == nil {
		k.k = pySyntaxOf()
	}
	k.g, k.src, k.fn = g, src, fn
	defer k.release()
	if !k.modulesRead {
		k.modulesRead = true
		k.readModule(fn)
	}
	k.index(du)
	k.class = ""
	kn := k.k
	switch fn.KindId() {
	case kn.module:
		k.visit(fn)
	case kn.functionDefinition, kn.lambda:
		if c, method := k.enclosingClass(fn); method {
			k.class = c
		}
		k.params(fn)
		k.visit(fn.ChildByFieldId(kn.fBody))
	case kn.classDefinition:
		k.visit(fn.ChildByFieldId(kn.fBody))
	default:
		k.comprehension(fn)
	}
	base := uint32(len(k.sites))
	for i, c := range k.calls {
		k.callAt[spanOf(c)] = base + uint32(i)
	}
	for _, c := range k.calls {
		k.sites = append(k.sites, k.site(c))
	}
	for _, a := range k.assigns {
		k.assignValues(a)
	}
	if k.nests && fn.KindId() != kn.module && fn.KindId() != kn.classDefinition {
		k.recordScope(fn)
	}
	if fn.KindId() == kn.functionDefinition && fn.ChildByFieldId(kn.fReturnType).IsNull() {
		var r pyVal
		for _, e := range k.rets {
			r = k.join(r, k.typeOf(e))
		}
		if t := r.asValue(); t.Kind != wire.TypeUnknown {
			k.returns[uint32(fn.ChildByFieldId(kn.fName).StartByte())] = t
		}
	}
}

// release drops every reference function took to the callable's graph and
// nodes, keeping the lists' capacity.
func (k *pythonLinker) release() {
	k.g, k.src, k.fn = nil, nil, Node{}
	k.nests, k.globalsRead = false, false
	clear(k.globals)
	clear(k.names)
	clear(k.binds)
	clear(k.callAt)
	clear(k.calls)
	clear(k.assigns)
	clear(k.rets)
	k.uses, k.defs, k.memo = k.uses[:0], k.defs[:0], k.memo[:0]
	k.calls, k.assigns, k.rets = k.calls[:0], k.assigns[:0], k.rets[:0]
}

// index builds the callable's variable names, reads and def-use pairs.
func (k *pythonLinker) index(du flow.Edges) {
	g := k.g
	for v := range int32(g.Vars()) {
		if s, named := g.Declared(v); named {
			k.names[string(k.src[s.Start:s.End])] = v
		}
	}
	for n := range int32(g.Len()) {
		s := g.Span(n)
		for _, v := range g.Uses(n) {
			k.uses = append(k.uses, pyUse{v: v, node: n, start: s.Start, end: s.End})
		}
	}
	slices.SortFunc(k.uses, func(a, b pyUse) int {
		return cmp.Or(cmp.Compare(a.v, b.v), cmp.Compare(a.start, b.start), cmp.Compare(b.end, a.end))
	})
	for i := range du.Len() {
		p, u := du.At(i)
		k.defs = append(k.defs, uint64(uint32(u))<<32|uint64(uint32(p)))
	}
	slices.Sort(k.defs)
	k.memo = slices.Grow(k.memo[:0], g.Len())[:g.Len()]
	clear(k.memo)
}

// useNode is the innermost node of the graph that reads v and spans s, or -1.
func (k *pythonLinker) useNode(v int32, s flow.Span) int32 {
	lo, _ := slices.BinarySearchFunc(k.uses, v, func(u pyUse, v int32) int { return cmp.Compare(u.v, v) })
	hi, _ := slices.BinarySearchFunc(k.uses, v+1, func(u pyUse, v int32) int { return cmp.Compare(u.v, v) })
	run := k.uses[lo:hi]
	i, _ := slices.BinarySearchFunc(run, s.Start+1, func(u pyUse, x uint32) int { return cmp.Compare(u.start, x) })
	for i--; i >= 0; i-- {
		if run[i].end >= s.End {
			return run[i].node
		}
	}
	return -1
}

// readModule reads the names the module's own code binds, from the root
// above fn: imports, defs and classes for what they are, and any other
// binding as a variable of unknown value. A name bound two ways is a
// variable.
func (k *pythonLinker) readModule(fn Node) {
	root := fn
	for p := root.Parent(); !p.IsNull(); p = p.Parent() {
		root = p
	}
	k.moduleNames = map[string]pyVal{}
	bind := func(name string, v pyVal) {
		if old, ok := k.moduleNames[name]; ok && old != v {
			v = pyVal{o: pyValue}
		}
		k.moduleNames[name] = v
	}
	kn := k.k
	var walk func(n Node)
	walk = func(n Node) {
		for c := n.first(); !c.IsNull(); c = c.next() {
			if !c.IsNamed() {
				continue
			}
			switch c.KindId() {
			case kn.importStatement, kn.importFromStatement:
				k.imports(c, func(name Node, v pyVal) { bind(string(textOf(k.src, name)), v) })
				continue
			case kn.functionDefinition, kn.classDefinition:
				bind(string(textOf(k.src, c.ChildByFieldId(kn.fName))), k.defVal(c))
				continue
			case kn.assignment:
				k.targets(c, func(t Node) { bind(string(textOf(k.src, t)), pyVal{o: pyValue}) })
			}
			if !pythonLowering.isCallable(c) {
				walk(c)
			}
		}
	}
	walk(root)
}

// targets calls f with every identifier an assignment chain rooted at a
// binds, unpacking included.
func (k *pythonLinker) targets(a Node, f func(Node)) {
	kn := k.k
	var each func(t Node)
	each = func(t Node) {
		switch t.KindId() {
		case kn.identifier:
			f(t)
		case kn.patternList, kn.tuplePattern, kn.listPattern, kn.tuple, kn.list, kn.expressionList,
			kn.parenthesizedExpression, kn.listSplatPattern, kn.listSplat:
			for c := t.first(); !c.IsNull(); c = c.next() {
				if c.IsNamed() {
					each(c)
				}
			}
		}
	}
	for r := a; !r.IsNull() && r.KindId() == kn.assignment; r = r.ChildByFieldId(kn.fRight) {
		if t := r.ChildByFieldId(kn.fLeft); !t.IsNull() {
			each(t)
		}
	}
}

// imports calls f with each local name import statement n binds and what it
// binds it to.
func (k *pythonLinker) imports(n Node, f func(name Node, v pyVal)) {
	kn := k.k
	mod := n.ChildByFieldId(kn.fModuleName)
	for c := n.first(); !c.IsNull(); c = c.next() {
		if !c.IsNamed() || (!mod.IsNull() && c == mod) {
			continue
		}
		var name, path Node
		switch c.KindId() {
		case kn.aliasedImport:
			name, path = c.ChildByFieldId(kn.fAlias), c.ChildByFieldId(kn.fName)
		case kn.dottedName:
			name, path = firstNamed(c), c
		default:
			continue
		}
		if name.IsNull() || path.IsNull() {
			continue
		}
		if mod.IsNull() {
			p := path
			if c.KindId() == kn.dottedName {
				p = name // `import a.b` binds a, the module a
			}
			f(name, pyVal{o: pyModule, t: wire.Type{Kind: wire.TypeModule, Name: compact(p, k.src)}})
			continue
		}
		m := compact(mod, k.src)
		if strings.Trim(m, ".") != "" {
			m += "."
		}
		f(name, pyVal{o: pyImported, t: wire.Type{Kind: wire.TypeModule, Name: m + compact(path, k.src)}})
	}
}

// defVal is a def or class statement's value.
func (k *pythonLinker) defVal(d Node) pyVal {
	o := pyFunction
	if d.KindId() == k.k.classDefinition {
		o = pyClass
	}
	return pyVal{o: o, t: wire.Type{Name: string(textOf(k.src, d.ChildByFieldId(k.k.fName)))}}
}

// enclosingClass is the name of the class whose body holds def fn, and
// whether fn is a method (a def directly in a class body).
func (k *pythonLinker) enclosingClass(fn Node) (string, bool) {
	kn := k.k
	if fn.KindId() != kn.functionDefinition {
		return "", false
	}
	p := fn.Parent()
	if !p.IsNull() && p.KindId() == kn.decoratedDefinition {
		p = p.Parent()
	}
	if p.IsNull() || p.KindId() != kn.block {
		return "", false
	}
	c := p.Parent()
	if c.IsNull() || c.KindId() != kn.classDefinition {
		return "", false
	}
	return string(textOf(k.src, c.ChildByFieldId(kn.fName))), true
}

// decorated reports whether def d carries a decorator whose name (the last
// component of a dotted one) is name.
func (k *pythonLinker) decorated(d Node, name string) bool {
	kn := k.k
	p := d.Parent()
	if p.IsNull() || p.KindId() != kn.decoratedDefinition {
		return false
	}
	for c := p.first(); !c.IsNull(); c = c.next() {
		if c.KindId() != kn.decorator {
			continue
		}
		e := firstNamed(c)
		if !e.IsNull() && e.KindId() == kn.attribute {
			e = lastNamed(e)
		}
		if !e.IsNull() && e.KindId() == kn.identifier && string(textOf(k.src, e)) == name {
			return true
		}
	}
	return false
}

// pyParam is one parameter of a callable: its bound name (null for a
// pattern), its annotation, its index after the receiver (-1 when a caller
// cannot pass it by position) and its role.
type pyParam struct {
	name, ann Node
	index     int
	receiver  bool
	splat     bool // *args
	keywords  bool // **kwargs
	optional  bool // it has a default value
}

// eachParam calls f with every parameter of callable fn in order.
func (k *pythonLinker) eachParam(fn Node, f func(pyParam)) {
	kn := k.k
	ps := fn.ChildByFieldId(kn.fParameters)
	if ps.IsNull() {
		return
	}
	_, method := k.enclosingClass(fn)
	receiver := method && !k.decorated(fn, "staticmethod")
	index, keywordOnly := 0, false
	for c := ps.first(); !c.IsNull(); c = c.next() {
		if !c.IsNamed() || c.IsExtra() {
			continue
		}
		p := pyParam{index: -1}
		x := c
		switch x.KindId() {
		case kn.typedParameter:
			p.ann = x.ChildByFieldId(kn.fType)
			x = firstNamed(x)
		case kn.defaultParameter:
			p.optional = true
			x = x.ChildByFieldId(kn.fName)
		case kn.typedDefaultParameter:
			p.ann, p.optional = x.ChildByFieldId(kn.fType), true
			x = x.ChildByFieldId(kn.fName)
		}
		if x.IsNull() {
			continue
		}
		switch x.KindId() {
		case kn.listSplatPattern:
			p.splat, p.name, keywordOnly = true, firstNamed(x), true
		case kn.dictionarySplatPattern:
			p.keywords, p.name = true, firstNamed(x)
		case kn.identifier, kn.tuplePattern:
			if x.KindId() == kn.identifier {
				p.name = x
			}
			switch {
			case receiver:
				p.receiver, receiver = true, false
			case !keywordOnly:
				p.index = index
				index++
			}
		default:
			// A separator: `*` begins the keyword-only parameters, `/` ends
			// the positional-only ones.
			if string(textOf(k.src, x)) == "*" {
				keywordOnly = true
			}
			continue
		}
		f(p)
	}
}

// params binds fn's parameters as definitions.
func (k *pythonLinker) params(fn Node) {
	k.eachParam(fn, func(p pyParam) {
		if p.name.IsNull() || p.name.KindId() != k.k.identifier {
			return
		}
		var v pyVal
		switch {
		case p.receiver:
			if k.class != "" {
				v = pyVal{o: pyValue, t: wire.Type{Kind: wire.TypeNamed, Name: k.class}}
			}
		case p.splat || p.keywords:
		case !p.ann.IsNull():
			v = pyVal{o: pyValue, t: wire.Type{Kind: wire.TypeNamed, Name: annotation(p.ann, k.src)}}
		case p.index >= 0:
			v = pyVal{o: pyValue, t: wire.Type{Kind: wire.TypeParam, Index: uint32(p.index)}}
		}
		k.binds[spanOf(p.name)] = pyBind{val: v}
	})
}

// comprehension reads comprehension fn's own code: everything but its first
// iterable, which the enclosing callable evaluates.
func (k *pythonLinker) comprehension(fn Node) {
	kn := k.k
	first := true
	for c := fn.first(); !c.IsNull(); c = c.next() {
		if !c.IsNamed() {
			continue
		}
		if c.KindId() == kn.forInClause && first {
			first = false
			k.visit(c.ChildByFieldId(kn.fLeft))
			continue
		}
		k.visit(c)
	}
}

// visit reads n and its subtree as the callable's own code, entering a
// nested callable only for the parts the callable evaluates (header).
func (k *pythonLinker) visit(n Node) {
	if n.IsNull() {
		return
	}
	kn := k.k
	if pythonLowering.isCallable(n) && n != k.fn {
		k.header(n)
		return
	}
	switch n.KindId() {
	case kn.call:
		if !k.calleeName(n).IsNull() {
			k.calls = append(k.calls, n)
		}
	case kn.assignment:
		if p := n.Parent(); p.IsNull() || p.KindId() != kn.assignment {
			k.assignment(n)
		}
	case kn.namedExpression:
		k.binds[spanOf(n)] = pyBind{expr: n.ChildByFieldId(kn.fValue)}
	case kn.returnStatement:
		if e := firstNamed(n); !e.IsNull() {
			k.rets = append(k.rets, e)
		}
	case kn.importStatement, kn.importFromStatement:
		k.imports(n, func(name Node, v pyVal) { k.binds[spanOf(name)] = pyBind{val: v} })
		return
	}
	for c := n.first(); !c.IsNull(); c = c.next() {
		if c.IsNamed() {
			k.visit(c)
		}
	}
}

// header reads the parts of nested callable n its creator evaluates, and
// binds a def or class statement's name.
func (k *pythonLinker) header(n Node) {
	kn := k.k
	k.nests = true
	switch n.KindId() {
	case kn.functionDefinition, kn.classDefinition, kn.lambda:
		if n.KindId() != kn.lambda {
			at := n
			if p := n.Parent(); !p.IsNull() && p.KindId() == kn.decoratedDefinition {
				at = p
			}
			k.binds[spanOf(at)] = pyBind{val: k.defVal(n)}
		}
		body := n.ChildByFieldId(kn.fBody)
		for c := n.first(); !c.IsNull(); c = c.next() {
			if c.IsNamed() && c != body {
				k.visit(c)
			}
		}
	default:
		for c := n.first(); !c.IsNull(); c = c.next() {
			if c.KindId() != kn.forInClause {
				continue
			}
			left := c.ChildByFieldId(kn.fLeft)
			for x := c.first(); !x.IsNull(); x = x.next() {
				if x.IsNamed() && x != left {
					k.visit(x)
				}
			}
			return
		}
	}
}

// assignment binds the targets of the assignment chain rooted at a: the one
// target of `t = e` and `t: T = e` at the statement's span, each identifier
// target of a chain at its own span; an unpacking binds nothing the trace
// can read.
func (k *pythonLinker) assignment(a Node) {
	kn := k.k
	k.assigns = append(k.assigns, a)
	value := a
	n := 0
	for value.KindId() == kn.assignment {
		value = value.ChildByFieldId(kn.fRight)
		n++
		if value.IsNull() {
			return
		}
	}
	if n == 1 {
		if t := pythonLowering.unparen(a.ChildByFieldId(kn.fLeft)); !t.IsNull() && t.KindId() == kn.identifier {
			k.binds[spanOf(a)] = pyBind{expr: value, ann: a.ChildByFieldId(kn.fType)}
		}
		return
	}
	for r := a; r.KindId() == kn.assignment; r = r.ChildByFieldId(kn.fRight) {
		if t := pythonLowering.unparen(r.ChildByFieldId(kn.fLeft)); !t.IsNull() && t.KindId() == kn.identifier {
			k.binds[spanOf(t)] = pyBind{expr: value}
		}
	}
}

// assignValues records the values assignment chain a writes where a call
// can reach them without naming them: every value written to an attribute,
// or to a name in a class body (a class attribute), under that field, and a
// function assigned to a name.
func (k *pythonLinker) assignValues(a Node) {
	kn := k.k
	value := a
	for value.KindId() == kn.assignment {
		value = value.ChildByFieldId(kn.fRight)
		if value.IsNull() {
			return
		}
	}
	t := k.typeOf(value).asValue()
	if t.Kind == wire.TypeUnknown {
		return
	}
	class := k.fn.KindId() == kn.classDefinition
	named := false
	for r := a; r.KindId() == kn.assignment; r = r.ChildByFieldId(kn.fRight) {
		x := pythonLowering.unparen(r.ChildByFieldId(kn.fLeft))
		switch {
		case x.IsNull():
		case x.KindId() == kn.attribute:
			if f := lastNamed(x); !f.IsNull() {
				k.values = append(k.values, wire.Value{At: wireSpan(spanOf(value)), Field: string(textOf(k.src, f)), Type: t})
			}
		case x.KindId() == kn.identifier && class:
			k.values = append(k.values, wire.Value{At: wireSpan(spanOf(value)), Field: string(textOf(k.src, x)), Type: t})
		case x.KindId() == kn.identifier && t.Kind == wire.TypeFunction && !named:
			named = true
			k.values = append(k.values, wire.Value{At: wireSpan(spanOf(value)), Type: t})
		}
	}
}

// calleeName is call c's callee token as the structural query captures it:
// a name callee, or the name of an attribute callee; null for any other.
func (k *pythonLinker) calleeName(c Node) Node {
	kn := k.k
	f := c.ChildByFieldId(kn.fFunction)
	switch {
	case f.IsNull():
	case f.KindId() == kn.identifier:
		return f
	case f.KindId() == kn.attribute:
		if a := lastNamed(f); !a.IsNull() && a.KindId() == kn.identifier && a != f.ChildByFieldId(kn.fObject) {
			return a
		}
	}
	return Node{}
}

// site is call c's site.
func (k *pythonLinker) site(c Node) wire.Site {
	kn := k.k
	name := k.calleeName(c)
	s := wire.Site{Name: wireSpan(spanOf(name)), Shape: wire.ShapeBare}
	if f := c.ChildByFieldId(kn.fFunction); f.KindId() == kn.identifier {
		v, variable := k.nameOf(f)
		switch {
		case v.o == pyImported:
			s.Receiver = v.t
		case variable:
			s.Shape, s.Receiver = wire.ShapeIndirect, v.asValue()
		}
	} else {
		obj := f.ChildByFieldId(kn.fObject)
		v := k.receiver(obj)
		switch v.o {
		case pyModule:
			s.Shape, s.Qualifier, s.Receiver = wire.ShapeStatic, string(textOf(k.src, obj)), v.t
		case pyClass:
			s.Shape, s.Receiver = wire.ShapeMember, v.asValue()
		case pyFunction:
			s.Shape = wire.ShapeMember
		default:
			s.Shape, s.Receiver = wire.ShapeMember, v.t
		}
	}
	args := c.ChildByFieldId(kn.fArguments)
	if args.IsNull() {
		return s
	}
	if pythonLowering.isCallable(args) {
		// `f(x for x in y)`: one generator argument.
		s.Args = append(s.Args, wire.Type{})
		return s
	}
	positional := true
	for a := args.first(); !a.IsNull(); a = a.next() {
		if !a.IsNamed() || a.IsExtra() {
			continue
		}
		var t wire.Type
		switch a.KindId() {
		case kn.listSplat, kn.dictionarySplat:
			positional = false
		case kn.keywordArgument:
			positional = false
			k.passed(a.ChildByFieldId(kn.fValue))
		default:
			t = k.passed(a)
			if !positional {
				t = wire.Type{}
			}
		}
		s.Args = append(s.Args, t)
	}
	return s
}

// passed is the type of argument a, recording a function passed as a value.
func (k *pythonLinker) passed(a Node) wire.Type {
	if a.IsNull() {
		return wire.Type{}
	}
	t := k.typeOf(a).asValue()
	if t.Kind == wire.TypeFunction {
		k.values = append(k.values, wire.Value{At: wireSpan(spanOf(a)), Type: t})
	}
	return t
}

// receiver is what the object of an attribute callee is: `super()` is the
// enclosing class's supertypes.
func (k *pythonLinker) receiver(obj Node) pyVal {
	kn := k.k
	if o := pythonLowering.unparen(obj); !o.IsNull() && o.KindId() == kn.call {
		f := o.ChildByFieldId(kn.fFunction)
		if !f.IsNull() && f.KindId() == kn.identifier && string(textOf(k.src, f)) == "super" {
			if _, local := k.names["super"]; !local {
				if c := k.classOf(k.fn); c != "" {
					return pyVal{o: pyValue, t: wire.Type{Kind: wire.TypeSuper, Name: c}}
				}
			}
		}
	}
	return k.typeOf(obj)
}

// classOf is the name of the class nearest around n, or "".
func (k *pythonLinker) classOf(n Node) string {
	for p := n.Parent(); !p.IsNull(); p = p.Parent() {
		if p.KindId() == k.k.classDefinition {
			return string(textOf(k.src, p.ChildByFieldId(k.k.fName)))
		}
	}
	return ""
}

// typeOf is what expression e evaluates to, as far as the callable shows.
func (k *pythonLinker) typeOf(e Node) pyVal {
	kn := k.k
	e = pythonLowering.unparen(e)
	if e.IsNull() {
		return pyVal{}
	}
	switch e.KindId() {
	case kn.identifier:
		v, _ := k.nameOf(e)
		return v
	case kn.call:
		if i, ok := k.callAt[spanOf(e)]; ok {
			return pyVal{o: pyValue, t: wire.Type{Kind: wire.TypeResult, Index: i}}
		}
	case kn.attribute:
		f := lastNamed(e)
		if f.IsNull() || f == e.ChildByFieldId(kn.fObject) {
			return pyVal{}
		}
		name := string(textOf(k.src, f))
		o := k.typeOf(e.ChildByFieldId(kn.fObject))
		switch {
		case o.o == pyModule || o.o == pyImported:
			return pyVal{o: o.o, t: wire.Type{Kind: wire.TypeModule, Name: o.t.Name + "." + name}}
		case o.o == pyClass || (o.o == pyValue && o.t.Kind == wire.TypeNamed):
			if m := k.method(o.t.Name, name); m != "" {
				return pyVal{o: pyFunction, t: wire.Type{Name: m}}
			}
		}
		return pyVal{o: pyValue, t: wire.Type{Kind: wire.TypeField, Name: name}}
	case kn.namedExpression:
		return k.typeOf(e.ChildByFieldId(kn.fValue))
	case kn.await:
		return k.typeOf(firstNamed(e))
	case kn.booleanOperator:
		return k.join(k.typeOf(e.ChildByFieldId(kn.fLeft)), k.typeOf(e.ChildByFieldId(kn.fRight)))
	case kn.conditionalExpression:
		// `a if c else b`: the middle operand is the condition.
		return k.join(k.typeOf(e.NamedChild(0)), k.typeOf(e.NamedChild(2)))
	}
	return pyVal{}
}

// nameOf is what identifier n refers to, and whether it is a variable a
// value is assigned to: of the callable, through the definitions that reach
// the read; otherwise of the module, through its own bindings.
func (k *pythonLinker) nameOf(n Node) (pyVal, bool) {
	text := string(textOf(k.src, n))
	v, local := k.names[text]
	if !local {
		if k.fn.KindId() == k.k.module {
			return pyVal{}, false
		}
		if r, ok := k.enclosing(text); ok {
			return r.read()
		}
		m, ok := k.moduleNames[text]
		return m, ok && m.o == pyValue
	}
	u := k.useNode(v, spanOf(n))
	if u < 0 {
		return pyVal{}, true
	}
	var r pyRead
	lo, _ := slices.BinarySearch(k.defs, uint64(uint32(u))<<32)
	for _, d := range k.defs[lo:] {
		if int32(d>>32) != u {
			break
		}
		if p := int32(uint32(d)); holds(k.g.Defs(p), v) {
			r = k.reach(r, p)
		}
	}
	return r.read()
}

// recordScope records what each named variable of fn, a function, lambda
// or comprehension that nests a callable, is over all its definitions: what
// the nested callables read it as.
func (k *pythonLinker) recordScope(fn Node) {
	g := k.g
	reads := make([]pyRead, g.Vars())
	for p := range int32(g.Len()) {
		for _, v := range g.Defs(p) {
			reads[v] = k.reach(reads[v], p)
		}
	}
	scope := make(map[string]pyRead, len(k.names))
	for name, v := range k.names {
		r := reads[v]
		r.val = k.withoutParams(r.val)
		scope[name] = r
	}
	k.scopes[spanOf(fn)] = scope
}

// enclosing is what free name text of fn refers to in the scopes enclosing
// fn, innermost first: every function, lambda and comprehension around it
// but a comprehension whose first iterable holds fn, which its enclosing
// callable evaluates. A class body is no scope of the callables it nests,
// and a name fn declares global is the module's.
func (k *pythonLinker) enclosing(text string) (pyRead, bool) {
	kn := k.k
	if !k.globalsRead {
		k.globalsRead = true
		if body := k.fn.ChildByFieldId(kn.fBody); k.fn.KindId() != kn.lambda && !body.IsNull() {
			k.declaredGlobal(body)
		}
	}
	if k.globals[text] {
		return pyRead{}, false
	}
	at := spanOf(k.fn)
	for p := k.fn.Parent(); !p.IsNull() && p.KindId() != kn.module; p = p.Parent() {
		if !pythonLowering.isCallable(p) || p.KindId() == kn.classDefinition || k.inFirstIterable(p, at) {
			continue
		}
		if r, ok := k.scopes[spanOf(p)][text]; ok {
			return r, true
		}
	}
	return pyRead{}, false
}

// inFirstIterable reports whether span at lies in the first iterable of
// comprehension c; false when c is no comprehension.
func (k *pythonLinker) inFirstIterable(c Node, at flow.Span) bool {
	kn := k.k
	switch c.KindId() {
	case kn.functionDefinition, kn.lambda:
		return false
	}
	for x := c.first(); !x.IsNull(); x = x.next() {
		if x.KindId() != kn.forInClause {
			continue
		}
		in := func(n Node) bool { s := spanOf(n); return s.Start <= at.Start && at.End <= s.End }
		left := x.ChildByFieldId(kn.fLeft)
		return in(x) && (left.IsNull() || !in(left))
	}
	return false
}

// declaredGlobal records the names the global statements of n, a part of
// fn's own code, declare.
func (k *pythonLinker) declaredGlobal(n Node) {
	kn := k.k
	for c := n.first(); !c.IsNull(); c = c.next() {
		switch {
		case !c.IsNamed() || pythonLowering.isCallable(c):
		case c.KindId() == kn.globalStatement:
			for x := c.first(); !x.IsNull(); x = x.next() {
				if x.KindId() == kn.identifier {
					k.globals[string(textOf(k.src, x))] = true
				}
			}
		default:
			k.declaredGlobal(c)
		}
	}
}

// method is the qualified name of the def m that the body of the file's one
// class named class declares, or "" when no class or several declare one,
// or the def is a property (its read calls it).
func (k *pythonLinker) method(class, m string) string {
	if !k.methodsRead {
		k.methodsRead = true
		k.methods = map[string][]string{}
		root := k.fn
		for p := root.Parent(); !p.IsNull(); p = p.Parent() {
			root = p
		}
		k.indexMethods(root, "")
	}
	if q := k.methods[class+"."+m]; len(q) == 1 {
		return q[0]
	}
	return ""
}

// indexMethods indexes the methods of every class under n, whose enclosing
// defs and classes qualify a declaration by prefix.
func (k *pythonLinker) indexMethods(n Node, prefix string) {
	kn := k.k
	for c := n.first(); !c.IsNull(); c = c.next() {
		if !c.IsNamed() {
			continue
		}
		id := c.KindId()
		if id != kn.functionDefinition && id != kn.classDefinition {
			k.indexMethods(c, prefix)
			continue
		}
		name := string(textOf(k.src, c.ChildByFieldId(kn.fName)))
		inner := prefix + name + "."
		if id == kn.classDefinition {
			for s := c.ChildByFieldId(kn.fBody).first(); !s.IsNull(); s = s.next() {
				d := s
				if d.KindId() == kn.decoratedDefinition {
					d = d.ChildByFieldId(kn.fDefinition)
				}
				if d.IsNull() || d.KindId() != kn.functionDefinition || k.property(d) {
					continue
				}
				m := string(textOf(k.src, d.ChildByFieldId(kn.fName)))
				k.methods[name+"."+m] = append(k.methods[name+"."+m], inner+m)
			}
		}
		k.indexMethods(c.ChildByFieldId(kn.fBody), inner)
	}
}

// property reports whether def d is a property or one of its accessors,
// which a read of the attribute calls rather than yields.
func (k *pythonLinker) property(d Node) bool {
	for _, p := range []string{"property", "cached_property", "getter", "setter", "deleter"} {
		if k.decorated(d, p) {
			return true
		}
	}
	return false
}

// defined is the value defining node p binds.
func (k *pythonLinker) defined(p int32) pyVal {
	m := &k.memo[p]
	switch m.state {
	case 1:
		return pyVal{}
	case 2:
		return m.val
	}
	m.state = 1
	var v pyVal
	if b, ok := k.binds[k.g.Span(p)]; ok {
		switch {
		case !b.ann.IsNull():
			v = pyVal{o: pyValue, t: wire.Type{Kind: wire.TypeNamed, Name: annotation(b.ann, k.src)}}
		case !b.expr.IsNull():
			v = k.typeOf(b.expr)
		default:
			v = b.val
		}
	}
	m = &k.memo[p]
	m.state, m.val = 2, v
	return v
}

func (k *pythonLinker) file(root Node, src []byte, out *wire.Link) {
	if k.k == nil {
		k.k = pySyntaxOf()
	}
	k.src = src
	defer func() { k.src = nil }()
	kn := k.k
	// prefix is the enclosing defs' and classes' names, each followed by a
	// dot: what a declaration's qualified name starts with.
	var walk func(n Node, prefix string)
	walk = func(n Node, prefix string) {
		for c := n.first(); !c.IsNull(); c = c.next() {
			if !c.IsNamed() {
				continue
			}
			inner := prefix
			switch c.KindId() {
			case kn.functionDefinition:
				if x, ok := k.export(c); ok {
					out.Exports = append(out.Exports, x)
					k.parameters(c, x.Name, out)
				}
				inner = prefix + string(textOf(src, c.ChildByFieldId(kn.fName))) + "."
			case kn.classDefinition:
				k.supertypes(c, out)
				inner = prefix + string(textOf(src, c.ChildByFieldId(kn.fName))) + "."
			}
			walk(c, inner)
		}
	}
	walk(root, "")
	out.Sites = append(out.Sites, k.sites...)
	out.Values = append(out.Values, k.values...)
	out.Unions = append(out.Unions, k.unions...)
}

// export is def d's export.
func (k *pythonLinker) export(d Node) (wire.Export, bool) {
	kn := k.k
	name := d.ChildByFieldId(kn.fName)
	if name.IsNull() {
		return wire.Export{}, false
	}
	class, method := k.enclosingClass(d)
	x := wire.Export{
		Name:     wireSpan(spanOf(name)),
		Receiver: class,
		Static:   !method || k.decorated(d, "staticmethod") || k.decorated(d, "classmethod"),
		Abstract: k.decorated(d, "abstractmethod"),
	}
	var types []string
	typed := true
	k.eachParam(d, func(p pyParam) {
		switch {
		case p.receiver:
			return
		case p.splat:
			x.Variadic = true
			typed = false
			return
		case p.keywords:
			typed = false
			return
		}
		x.Params++
		if p.optional {
			x.Optional++
		}
		if p.ann.IsNull() {
			typed = false
		} else {
			types = append(types, annotation(p.ann, k.src))
		}
	})
	if ret := d.ChildByFieldId(kn.fReturnType); !ret.IsNull() {
		r := annotation(ret, k.src)
		x.Returns = wire.Type{Kind: wire.TypeNamed, Name: r}
		if typed {
			x.Signature = "(" + strings.Join(types, ",") + ")(" + r + ")"
		}
	} else {
		x.Returns = k.returns[uint32(name.StartByte())]
	}
	return x, true
}

// parameters appends a Parameter for each parameter def d, whose export is
// named callable, declares by a name, as the lowering declares its
// variables: the receiver (`self`, `cls`) at 0, then every other parameter
// in order from 1, `*args`, keyword-only parameters and `**kwargs`
// included. A pattern parameter takes its position and is not sent, since
// no one name declares it.
func (k *pythonLinker) parameters(d Node, callable wire.Span, out *wire.Link) {
	var index uint32
	k.eachParam(d, func(p pyParam) {
		at := uint32(0)
		if !p.receiver {
			index++
			at = index
		}
		if p.name.IsNull() || p.name.KindId() != k.k.identifier {
			return
		}
		out.Parameters = append(out.Parameters, wire.Parameter{Callable: callable, Name: wireSpan(spanOf(p.name)), Index: at,
			Variadic:    p.splat || p.keywords,
			KeywordOnly: p.keywords || !p.receiver && !p.splat && p.index < 0})
	})
}

// supertypes appends class c's declared bases: each positional base, a
// subscripted one (`Generic[T]`) as the name it subscripts.
func (k *pythonLinker) supertypes(c Node, out *wire.Link) {
	kn := k.k
	bases := c.ChildByFieldId(kn.fSuperclasses)
	name := c.ChildByFieldId(kn.fName)
	if bases.IsNull() || name.IsNull() {
		return
	}
	typ := string(textOf(k.src, name))
	for b := bases.first(); !b.IsNull(); b = b.next() {
		if !b.IsNamed() || b.IsExtra() {
			continue
		}
		x := b
		if x.KindId() == kn.subscript {
			x = x.ChildByFieldId(kn.fValue)
		}
		if !x.IsNull() && (x.KindId() == kn.identifier || x.KindId() == kn.attribute) {
			out.Supertypes = append(out.Supertypes, wire.Supertype{Type: typ, Super: compact(x, k.src)})
		}
	}
}

// annotation is a type annotation's spelling: its tokens without whitespace
// or comments, a string forward reference as the name it quotes.
func annotation(t Node, src []byte) string {
	e := t
	if c := firstNamed(t); !c.IsNull() && t.NamedChildCount() == 1 {
		e = c
	}
	if e.KindId() == pySyntaxOf().stringLit {
		s := strings.TrimLeft(string(textOf(src, e)), "rRuUbBfF")
		return strings.Join(strings.Fields(strings.Trim(s, "\"'")), "")
	}
	return compact(t, src)
}

// compact is n's source text without whitespace or comments: its leaf
// tokens joined.
func compact(n Node, src []byte) string {
	var b strings.Builder
	var walk func(x Node)
	walk = func(x Node) {
		c := x.first()
		if c.IsNull() {
			b.Write(textOf(src, x))
			return
		}
		for ; !c.IsNull(); c = c.next() {
			if !c.IsExtra() {
				walk(c)
			}
		}
	}
	walk(n)
	return b.String()
}
