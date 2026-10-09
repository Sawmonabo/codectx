package worker

import (
	"strings"
	"sync"

	"github.com/Sawmonabo/codectx/internal/provider/treesitter/flow"
	"github.com/Sawmonabo/codectx/internal/provider/treesitter/wire"
)

// goLinker builds the link tables of one Go file.
//
// Go is statically typed: a variable's type is fixed where it is declared
// (The Go Programming Language Specification, "Variable declarations",
// "Short variable declarations"), so the receiver and argument types the
// tables carry are read from declarations, never traced over the def-use
// chains. The whole file is read in file, in two walks of the flat array:
// the first numbers the call sites and gathers the file's package-level
// names (imports, functions, methods, types with their fields, variables),
// the second keeps Go's block scopes while it emits the exports,
// supertypes, sites and values.
//
// Types are reduced to a name by one rule everywhere (goLink.baseName): the
// pointer, the parentheses and a generic type's arguments are stripped, so
// `*T`, `(T)` and `T[int]` all name T, and a qualified `pkg.T` keeps its
// qualifier as written. A method value or method expression is spelled
// "T.M" with T so reduced ("*T.M" for a method expression `(*T).M`), and a
// value of package pkg as "pkg.F". A method expression's value is the
// method with its receiver as the first parameter ("Method expressions"):
// its TypeFunction carries Index 1, the receiver parameters it takes ahead
// of the method's own, and every other function value Index 0.
type goLinker struct{}

func newGoLinker() *goLinker { return &goLinker{} }

// function records nothing: the Go tables are read from declarations in
// file (see goLinker).
func (k *goLinker) function(fn Node, g *flow.Graph, du flow.Edges, src []byte) {}

func (k *goLinker) file(root Node, src []byte, out *wire.Link) {
	x := &goLink{k: goLinkSyntaxOf(), src: src, out: out,
		sites: map[uint32]uint32{}, imports: map[string]bool{}, funcs: map[string]goVar{},
		types: map[string]map[string]goVar{}, consts: map[string]bool{},
		tops: map[string]*goTop{}, index: map[string]int32{}}
	x.prepare(root)
	for _, t := range x.tops {
		x.topVar(t)
	}
	x.walk(root)
}

// goVar is what a name in scope says about its value: its type, its
// declared function type spelled by the signature rule (empty when it is
// not a function), and whether it is a bound function literal, which is a
// declaration of its own (extract.go, addBoundFunctions).
type goVar struct {
	typ   wire.Type
	sig   string
	bound bool
}

// goTop is a package-level variable of the file: its spec, its position in
// the spec's names, and its value once computed.
type goTop struct {
	spec  Node
	i     int
	v     goVar
	state uint8 // 0 not computed, 1 being computed, 2 computed
}

// goBind is one local name in scope; prev is the index of the binding of
// the same name it shadows, or -1.
type goBind struct {
	name string
	v    goVar
	prev int32
}

// goLink is the state of one Go file's link tables.
type goLink struct {
	k   *goLinkSyntax
	src []byte
	out *wire.Link
	// sites numbers each call site by its callee token's first byte, in
	// the preorder both walks share.
	sites map[uint32]uint32
	// imports are the file's import names; funcs its package-level
	// functions with their signatures; types each declared type's fields;
	// consts its package-level constants; tops its package-level variables.
	imports map[string]bool
	funcs   map[string]goVar
	types   map[string]map[string]goVar
	consts  map[string]bool
	tops    map[string]*goTop
	// binds, index and marks are the block scopes of the second walk.
	binds []goBind
	index map[string]int32
	marks []int
	// blankEnd is the end of the innermost declaration named `_` being
	// walked: the extraction declares nothing inside it, so no export is
	// sent for what it holds.
	blankEnd uint
}

func (x *goLink) text(n Node) string { return view(textOf(x.src, n)) }

// copied is n's text as a string the tables may keep.
func (x *goLink) copied(n Node) string { return string(textOf(x.src, n)) }

// goSpan is n's byte range on the wire.
func goSpan(n Node) wire.Span {
	return wire.Span{Start: uint32(n.StartByte()), End: uint32(n.EndByte())}
}

// goChild is n's first child in field id, read from the child's own record,
// so a field the registry does not list is read too.
func goChild(n Node, id uint16) Node {
	for c := n.first(); !c.IsNull(); c = c.next() {
		if uint16(c.rec().field) == id {
			return c
		}
	}
	return Node{}
}

// goNamed calls visit with n's named children that are not extras, in order.
func goNamed(n Node, visit func(c Node)) {
	for c := n.first(); !c.IsNull(); c = c.next() {
		if c.IsNamed() && !c.IsExtra() {
			visit(c)
		}
	}
}

// list is n's named children, or n alone when it is not an expression list.
func (x *goLink) list(n Node) []Node {
	var l []Node
	if n.IsNull() {
		return l
	}
	if n.KindId() != x.k.expressionList {
		return append(l, n)
	}
	goNamed(n, func(c Node) { l = append(l, c) })
	return l
}

// is reports a node of kind id; a null node is of no kind.
func (x *goLink) is(n Node, id uint16) bool { return !n.IsNull() && n.KindId() == id }

// unparen strips the parentheses around an expression or a type.
func (x *goLink) unparen(n Node) Node {
	for !n.IsNull() && (n.KindId() == x.k.parenExpression || n.KindId() == x.k.parenType) {
		n = firstNamed(n)
	}
	return n
}

// prepare numbers the call sites and gathers the package-level names.
func (x *goLink) prepare(root Node) {
	k := x.k
	c := root.Walk()
	defer c.Close()
	for {
		n := c.Node()
		switch n.KindId() {
		case k.callExpression:
			if name := x.callee(n); !name.IsNull() {
				x.sites[uint32(name.StartByte())] = uint32(len(x.sites))
			}
		case k.importSpec:
			if name := x.importName(n); name != "" {
				x.imports[name] = true
			}
		case k.functionDeclaration:
			if name := n.ChildByFieldId(k.fName); !name.IsNull() {
				x.funcs[x.text(name)] = goVar{typ: wire.Type{Kind: wire.TypeFunction, Name: x.text(name)},
					sig: x.signature(n.ChildByFieldId(k.fParameters), n.ChildByFieldId(k.fResult))}
			}
		case k.typeSpec:
			x.typeFields(n)
		case k.varSpec, k.constSpec:
			if x.packageLevel(n) {
				i := 0
				x.eachName(n, func(id Node) {
					if n.KindId() == k.constSpec {
						x.consts[x.text(id)] = true
					} else {
						x.tops[x.text(id)] = &goTop{spec: n, i: i}
					}
					i++
				})
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

// packageLevel reports a var or const spec of the package block.
func (x *goLink) packageLevel(spec Node) bool {
	for p := spec.Parent(); !p.IsNull(); p = p.Parent() {
		switch p.KindId() {
		case x.k.sourceFile:
			return true
		case x.k.block, x.k.funcLiteral, x.k.functionDeclaration, x.k.methodDeclaration:
			return false
		}
	}
	return false
}

// eachName calls visit with every name in field "name" of n, `_` included.
func (x *goLink) eachName(n Node, visit func(id Node)) {
	for c := n.first(); !c.IsNull(); c = c.next() {
		if uint16(c.rec().field) == x.k.fName {
			visit(c)
		}
	}
}

// importName is the name an import spec binds: its explicit name, or the
// last segment of its path, as the extraction derives it (grammars.go). A
// blank or dot import binds no name of its own.
func (x *goLink) importName(spec Node) string {
	if name := spec.ChildByFieldId(x.k.fName); !name.IsNull() {
		if name.KindId() != x.k.packageIdentifier {
			return ""
		}
		return x.text(name)
	}
	path := goChild(spec, x.k.fPath)
	if path.IsNull() {
		return ""
	}
	return lastSegment(x.text(path), "/")
}

// typeFields records the fields of a declared type, its embedded fields by
// their type's name included; a type that is not a struct has none.
func (x *goLink) typeFields(spec Node) {
	k := x.k
	name := spec.ChildByFieldId(k.fName)
	if name.IsNull() {
		return
	}
	fields := map[string]goVar{}
	x.types[x.text(name)] = fields
	t := x.unparen(spec.ChildByFieldId(k.fType))
	if t.IsNull() || t.KindId() != k.structType {
		return
	}
	x.structFields(t, func(field Node, typ Node) {
		v := x.typeVar(typ)
		if field.IsNull() {
			// An embedded field's name is its type's unqualified name
			// ("Struct types").
			fields[lastSegment(x.baseName(typ), ".")] = v
		} else {
			fields[x.text(field)] = v
		}
	})
}

// structFields calls visit with every field of a struct type and its type:
// once per name, and once with a null name for an embedded field.
func (x *goLink) structFields(t Node, visit func(field, typ Node)) {
	k := x.k
	goNamed(t, func(l Node) {
		if l.KindId() != k.fieldDeclarationList {
			return
		}
		goNamed(l, func(f Node) {
			if f.KindId() != k.fieldDeclaration {
				return
			}
			typ := f.ChildByFieldId(k.fType)
			if typ.IsNull() {
				return
			}
			found := false
			x.eachName(f, func(id Node) { found = true; visit(id, typ) })
			if !found {
				visit(Node{}, typ)
			}
		})
	})
}

// typeVar is what a declared type says about a value of it.
func (x *goLink) typeVar(t Node) goVar {
	t = x.unparen(t)
	if t.IsNull() {
		return goVar{}
	}
	if t.KindId() == x.k.functionType {
		return goVar{sig: x.signature(t.ChildByFieldId(x.k.fParameters), t.ChildByFieldId(x.k.fResult))}
	}
	if name := x.baseName(t); name != "" {
		return goVar{typ: wire.Type{Kind: wire.TypeNamed, Name: name}}
	}
	return goVar{}
}

// baseName is the name a type reduces to (see goLinker), or empty for a
// type that names none (a slice, a map, a literal type).
func (x *goLink) baseName(t Node) string {
	k := x.k
	for {
		t = x.unparen(t)
		if t.IsNull() {
			return ""
		}
		switch t.KindId() {
		case k.pointerType:
			t = firstNamed(t)
		case k.genericType:
			t = t.ChildByFieldId(k.fType)
		case k.typeIdentifier, k.identifier:
			return x.text(t)
		case k.qualifiedType:
			var b strings.Builder
			x.tokens(&b, t)
			return b.String()
		default:
			return ""
		}
	}
}

// receiverOf is a method declaration's receiver base type.
func (x *goLink) receiverOf(m Node) string {
	r := m.ChildByFieldId(x.k.fReceiver)
	if r.IsNull() {
		return ""
	}
	p := firstNamed(r)
	if p.IsNull() {
		return ""
	}
	return x.baseName(p.ChildByFieldId(x.k.fType))
}

// signature spells a callable's parameter and result types by the rule of
// wire.Export.Signature: "(" every parameter's type, once per name it
// declares, a variadic one prefixed "...", joined by "," ")", then the
// results the same way, "()" when there are none. A type is its tokens
// without whitespace or comments, a function type in it spelled "func" and
// its own signature.
func (x *goLink) signature(params, result Node) string {
	var b strings.Builder
	x.paramTypes(&b, params)
	result = x.unparen(result)
	switch {
	case result.IsNull():
		b.WriteString("()")
	case result.KindId() == x.k.parameterList:
		x.paramTypes(&b, result)
	default:
		b.WriteByte('(')
		x.tokens(&b, result)
		b.WriteByte(')')
	}
	return b.String()
}

func (x *goLink) paramTypes(b *strings.Builder, list Node) {
	b.WriteByte('(')
	first := true
	x.params(list, func(typ Node, variadic bool) {
		if !first {
			b.WriteByte(',')
		}
		first = false
		if variadic {
			b.WriteString("...")
		}
		x.tokens(b, typ)
	})
	b.WriteByte(')')
}

// params calls visit with each parameter's type in a parameter list, once
// per name it declares. A declaration of names without a type declares its
// names as types (`func(a, b)` takes two parameters of types a and b).
func (x *goLink) params(list Node, visit func(typ Node, variadic bool)) {
	k := x.k
	if list.IsNull() {
		return
	}
	goNamed(list, func(p Node) {
		variadic := p.KindId() == k.variadicParameterDeclaration
		if !variadic && p.KindId() != k.parameterDeclaration {
			return
		}
		typ := p.ChildByFieldId(k.fType)
		names := 0
		x.eachName(p, func(id Node) {
			names++
			if typ.IsNull() {
				visit(id, variadic)
			}
		})
		if typ.IsNull() {
			return
		}
		for range max(names, 1) {
			visit(typ, variadic)
		}
	})
}

// tokens appends n's tokens without whitespace or comments, a function type
// spelled by the signature rule.
func (x *goLink) tokens(b *strings.Builder, n Node) {
	if n.IsExtra() {
		return
	}
	if n.KindId() == x.k.functionType {
		b.WriteString("func")
		b.WriteString(x.signature(n.ChildByFieldId(x.k.fParameters), n.ChildByFieldId(x.k.fResult)))
		return
	}
	c := n.first()
	if c.IsNull() {
		b.Write(textOf(x.src, n))
		return
	}
	for ; !c.IsNull(); c = c.next() {
		x.tokens(b, c)
	}
}

// export appends the export of a function, method or interface method.
func (x *goLink) export(n, name Node, receiver string, static, abstract bool) {
	k := x.k
	params, result := n.ChildByFieldId(k.fParameters), n.ChildByFieldId(k.fResult)
	e := wire.Export{Name: goSpan(name), Receiver: receiver, Signature: x.signature(params, result),
		Static: static, Abstract: abstract}
	x.params(params, func(_ Node, variadic bool) { e.Params++; e.Variadic = variadic })
	if r := x.unparen(result); !r.IsNull() {
		if r.KindId() == k.parameterList {
			r = firstNamed(r)
			if !r.IsNull() {
				r = r.ChildByFieldId(k.fType)
			}
		}
		if name := x.baseName(r); name != "" {
			e.Returns = wire.Type{Kind: wire.TypeNamed, Name: strings.Clone(name)}
		}
	}
	x.out.Exports = append(x.out.Exports, e)
}

// walk is the second walk: it keeps the block scopes and emits the tables.
func (x *goLink) walk(root Node) {
	c := root.Walk()
	defer c.Close()
	for {
		n := c.Node()
		x.enter(n)
		if c.GotoFirstChild() {
			continue
		}
		x.leave(n)
		for !c.GotoNextSibling() {
			if !c.GotoParent() {
				return
			}
			x.leave(c.Node())
		}
	}
}

// opensScope reports a node that opens a Go block ("Blocks"): a block, the
// implicit block of an if, for, switch or select and of each clause, and a
// function's block, which holds its receiver, parameters and results.
func (x *goLink) opensScope(n Node) bool {
	switch n.KindId() {
	case x.k.block, x.k.ifStatement, x.k.forStatement, x.k.expressionSwitchStatement, x.k.typeSwitchStatement,
		x.k.selectStatement, x.k.expressionCase, x.k.typeCase, x.k.defaultCase, x.k.communicationCase,
		x.k.functionDeclaration, x.k.methodDeclaration, x.k.funcLiteral:
		return true
	}
	return false
}

func (x *goLink) enter(n Node) {
	k := x.k
	if x.opensScope(n) {
		x.marks = append(x.marks, len(x.binds))
	}
	switch n.KindId() {
	case k.functionDeclaration, k.methodDeclaration, k.funcLiteral:
		for _, f := range []uint16{k.fReceiver, k.fParameters, k.fResult} {
			if l := n.ChildByFieldId(f); !l.IsNull() && l.KindId() == k.parameterList {
				goNamed(l, func(p Node) {
					// A variadic parameter is a slice of its declared type.
					var v goVar
					if p.KindId() == k.parameterDeclaration {
						v = x.typeVar(p.ChildByFieldId(k.fType))
					}
					x.eachName(p, func(id Node) { x.bind(id, v) })
				})
			}
		}
		if n.KindId() == k.funcLiteral {
			return
		}
		name := n.ChildByFieldId(k.fName)
		if name.IsNull() || x.blankDecl(n, name) {
			return
		}
		if n.KindId() == k.functionDeclaration {
			x.export(n, name, "", true, false)
		} else {
			x.export(n, name, x.copiedReceiver(n), false, false)
		}
	case k.methodElem:
		name := n.ChildByFieldId(k.fName)
		if name.IsNull() || x.blankDecl(n, name) {
			return
		}
		if t := n.Parent(); !t.IsNull() && t.KindId() == k.interfaceType {
			if spec := t.Parent(); !spec.IsNull() && spec.KindId() == k.typeSpec {
				if sn := spec.ChildByFieldId(k.fName); !sn.IsNull() {
					x.export(n, name, x.copied(sn), false, true)
				}
			}
		}
	case k.typeSpec:
		if name := n.ChildByFieldId(k.fName); !name.IsNull() && !x.blankDecl(n, name) {
			x.supertypes(n, name)
		}
	case k.varSpec, k.constSpec, k.fieldDeclaration:
		x.eachName(n, func(id Node) { x.blankDecl(n, id) })
		if n.KindId() == k.varSpec {
			x.values(n.ChildByFieldId(k.fValue), func(int) string { return "" })
		}
	case k.shortVarDeclaration:
		x.boundFunctions(n)
		x.values(n.ChildByFieldId(k.fRight), func(int) string { return "" })
	case k.assignmentStatement:
		left := x.list(n.ChildByFieldId(k.fLeft))
		x.values(n.ChildByFieldId(k.fRight), func(i int) string {
			if i < len(left) {
				if t := x.unparen(left[i]); x.is(t, k.selectorExpression) {
					if f := goChild(t, k.fField); !f.IsNull() {
						return x.copied(f)
					}
				}
			}
			return ""
		})
	case k.returnStatement:
		goNamed(n, func(l Node) { x.values(l, func(int) string { return "" }) })
	case k.sendStatement:
		x.values(n.ChildByFieldId(k.fValue), func(int) string { return "" })
	case k.literalValue:
		goNamed(n, func(e Node) {
			switch e.KindId() {
			case k.keyedElement:
				key, val := goChild(e, k.fKey), goChild(e, k.fValue)
				field := ""
				if !key.IsNull() {
					if id := firstNamed(key); x.is(id, k.identifier) || x.is(id, k.fieldIdentifier) {
						field = x.copied(id)
					}
				}
				if !val.IsNull() {
					x.value(firstNamed(val), field)
				}
			case k.literalElement:
				x.value(firstNamed(e), "")
			}
		})
	case k.callExpression:
		x.site(n)
		x.passed(n)
	}
}

func (x *goLink) leave(n Node) {
	k := x.k
	switch n.KindId() {
	case k.shortVarDeclaration:
		x.declare(n.ChildByFieldId(k.fLeft), n.ChildByFieldId(k.fRight), Node{})
	case k.varSpec:
		if len(x.marks) > 0 {
			x.declare(Node{}, n.ChildByFieldId(k.fValue), n)
		}
	case k.rangeClause, k.receiveStatement:
		if x.defines(n) {
			for _, id := range x.list(n.ChildByFieldId(k.fLeft)) {
				x.bind(id, goVar{})
			}
		}
	case k.expressionList:
		if p := n.Parent(); !p.IsNull() && p.KindId() == k.typeSwitchStatement && uint16(n.rec().field) == k.fAlias {
			for _, id := range x.list(n) {
				x.bind(id, goVar{})
			}
		}
	}
	if x.opensScope(n) {
		top := len(x.marks) - 1
		x.truncate(x.marks[top])
		x.marks = x.marks[:top]
	}
	if uint(n.EndByte()) == x.blankEnd && x.blankEnd != 0 {
		x.blankEnd = 0
	}
}

// blankDecl reports a declaration named `_`, the grammar's blank name, and
// makes it the innermost blank declaration; a declaration inside one is
// blank too, since the extraction extracts neither (extract.go, emit).
func (x *goLink) blankDecl(decl, name Node) bool {
	if x.blankEnd != 0 && uint(decl.StartByte()) < x.blankEnd {
		return true
	}
	if x.text(name) == "_" {
		x.blankEnd = uint(decl.EndByte())
		return true
	}
	return false
}

// defines reports a range clause or receive statement that declares its
// targets with `:=`.
func (x *goLink) defines(n Node) bool {
	for c := n.first(); !c.IsNull(); c = c.next() {
		if !c.IsNamed() && c.KindId() == x.k.define {
			return true
		}
	}
	return false
}

// declare binds the names a short variable declaration (names, values) or a
// local var spec (spec, values) declares, each with what its value says:
// the spec's declared type when it has one, else the paired value, or the
// first result of a single call for the first name.
func (x *goLink) declare(names, values, spec Node) {
	k := x.k
	var ids []Node
	var declared Node
	if spec.IsNull() {
		ids = x.list(names)
	} else {
		x.eachName(spec, func(id Node) { ids = append(ids, id) })
		declared = spec.ChildByFieldId(k.fType)
	}
	vals := x.list(values)
	for i, id := range ids {
		var v goVar
		switch {
		case !declared.IsNull():
			v = x.typeVar(declared)
		case len(vals) == len(ids):
			v = x.valueVar(vals[i])
			if spec.IsNull() && x.is(x.unparen(vals[i]), k.funcLiteral) {
				v.bound = true
			}
		case len(vals) == 1 && i == 0:
			if t := x.exprType(vals[0]); t.Kind == wire.TypeResult {
				v.typ = t
			}
		}
		if id.KindId() != k.identifier {
			continue
		}
		// A name `:=` redeclares in the same block is the same variable,
		// with the type it was declared with ("Short variable declarations").
		if spec.IsNull() && x.inInnermost(x.text(id)) {
			continue
		}
		x.bind(id, v)
	}
}

// valueVar is what a value expression says about the variable it
// initialises: its type, and its signature when it is a function.
func (x *goLink) valueVar(e Node) goVar {
	e = x.unparen(e)
	if e.IsNull() {
		return goVar{}
	}
	if e.KindId() == x.k.funcLiteral {
		return goVar{sig: x.signature(e.ChildByFieldId(x.k.fParameters), e.ChildByFieldId(x.k.fResult))}
	}
	if e.KindId() == x.k.identifier {
		if v, ok := x.lookup(x.text(e)); ok {
			// A copy of a bound function literal is a value, not its
			// declaration.
			v.bound = false
			return v
		}
		if f, ok := x.funcs[x.text(e)]; ok {
			return f
		}
	}
	if e.KindId() == x.k.selectorExpression {
		if fn := x.selected(e); fn.Name != "" {
			return goVar{typ: fn}
		}
	}
	return goVar{typ: x.exprType(e)}
}

// bind makes id's name resolve to v in the innermost block; `_` binds
// nothing.
func (x *goLink) bind(id Node, v goVar) {
	name := x.text(id)
	if name == "_" || len(x.marks) == 0 {
		return
	}
	prev, ok := x.index[name]
	if !ok {
		prev = -1
	}
	x.index[name] = int32(len(x.binds))
	x.binds = append(x.binds, goBind{name: name, v: v, prev: prev})
}

func (x *goLink) truncate(m int) {
	for i := len(x.binds) - 1; i >= m; i-- {
		b := x.binds[i]
		if b.prev < 0 {
			delete(x.index, b.name)
		} else {
			x.index[b.name] = b.prev
		}
	}
	x.binds = x.binds[:m]
}

// inInnermost reports a name bound in the innermost block.
func (x *goLink) inInnermost(name string) bool {
	if len(x.marks) == 0 {
		return false
	}
	i, ok := x.index[name]
	return ok && int(i) >= x.marks[len(x.marks)-1]
}

// lookup is the local variable or package-level variable name resolves to.
func (x *goLink) lookup(name string) (goVar, bool) {
	if i, ok := x.index[name]; ok {
		return x.binds[i].v, true
	}
	if t, ok := x.tops[name]; ok {
		return x.topVar(t), true
	}
	return goVar{}, false
}

// local reports a name that resolves to a local variable.
func (x *goLink) local(name string) bool {
	_, ok := x.index[name]
	return ok
}

// topVar computes a package-level variable's value once, before the second
// walk, so no local scope is open; a cycle through package-level
// initialisers yields nothing.
func (x *goLink) topVar(t *goTop) goVar {
	if t.state != 0 {
		return t.v
	}
	t.state = 1
	if declared := t.spec.ChildByFieldId(x.k.fType); !declared.IsNull() {
		t.v = x.typeVar(declared)
	} else {
		var names int
		x.eachName(t.spec, func(Node) { names++ })
		vals := x.list(t.spec.ChildByFieldId(x.k.fValue))
		switch {
		case len(vals) == names:
			t.v = x.valueVar(vals[t.i])
		case len(vals) == 1 && t.i == 0:
			if r := x.exprType(vals[0]); r.Kind == wire.TypeResult {
				t.v.typ = r
			}
		}
	}
	t.state = 2
	return t.v
}

// exprType is the type an expression's value has, as far as the file says.
func (x *goLink) exprType(e Node) wire.Type {
	k := x.k
	e = x.unparen(e)
	if e.IsNull() {
		return wire.Type{}
	}
	switch e.KindId() {
	case k.compositeLiteral:
		if name := x.baseName(e.ChildByFieldId(k.fType)); name != "" {
			return wire.Type{Kind: wire.TypeNamed, Name: name}
		}
	case k.unaryExpression:
		if op := e.ChildByFieldId(k.fOperator); !op.IsNull() && (op.KindId() == k.amp || op.KindId() == k.star) {
			return x.exprType(e.ChildByFieldId(k.fOperand))
		}
	case k.typeAssertionExpression:
		if name := x.baseName(e.ChildByFieldId(k.fType)); name != "" {
			return wire.Type{Kind: wire.TypeNamed, Name: name}
		}
	case k.callExpression:
		if f := x.unparen(e.ChildByFieldId(k.fFunction)); x.is(f, k.identifier) && x.text(f) == "new" && !x.local("new") {
			if args := goChild(e, k.fArguments); !args.IsNull() {
				if name := x.baseName(firstNamed(args)); name != "" {
					return wire.Type{Kind: wire.TypeNamed, Name: name}
				}
			}
			return wire.Type{}
		}
		if name := x.callee(e); !name.IsNull() {
			return wire.Type{Kind: wire.TypeResult, Index: x.sites[uint32(name.StartByte())]}
		}
	case k.identifier:
		name := x.text(e)
		if v, ok := x.lookup(name); ok {
			return v.typ
		}
		if f, ok := x.funcs[name]; ok {
			return f.typ
		}
		if x.imports[name] {
			return wire.Type{Kind: wire.TypeModule, Name: name}
		}
	case k.selectorExpression:
		o, f := x.unparen(e.ChildByFieldId(k.fOperand)), goChild(e, k.fField)
		if f.IsNull() || x.isImport(o) {
			return wire.Type{}
		}
		if t := x.exprType(o); t.Kind == wire.TypeNamed {
			if v, ok := x.types[t.Name][x.text(f)]; ok {
				return v.typ
			}
		}
		return wire.Type{Kind: wire.TypeField, Name: x.text(f)}
	}
	return wire.Type{}
}

// goCopiedType is t with its name copied out of the source.
func goCopiedType(t wire.Type) wire.Type {
	t.Name = strings.Clone(t.Name)
	return t
}

// isImport reports an operand that names an import no local shadows.
func (x *goLink) isImport(o Node) bool {
	return x.is(o, x.k.identifier) && x.imports[x.text(o)] && !x.local(x.text(o))
}

// isType reports an operand that names a type the file declares and no
// local shadows: the operand of a method expression `T.M`.
func (x *goLink) isType(o Node) bool {
	if !x.is(o, x.k.identifier) || x.local(x.text(o)) {
		return false
	}
	if _, ok := x.tops[x.text(o)]; ok {
		return false
	}
	_, ok := x.types[x.text(o)]
	return ok
}

// methodExprReceiver is the receiver a method expression's operand names,
// "T" for `T` and "*T" for `(*T)` where T is a type the file declares, or
// empty when the operand is not a type ("Method expressions").
func (x *goLink) methodExprReceiver(o Node) string {
	if x.isType(o) {
		return x.text(o)
	}
	if x.is(o, x.k.unaryExpression) {
		if op := o.ChildByFieldId(x.k.fOperator); !op.IsNull() && op.KindId() == x.k.star {
			if t := x.unparen(o.ChildByFieldId(x.k.fOperand)); x.isType(t) {
				return "*" + x.text(t)
			}
		}
	}
	return ""
}

// copiedReceiver is a method declaration's receiver base type, copied.
func (x *goLink) copiedReceiver(m Node) string { return strings.Clone(x.receiverOf(m)) }

// callee is a call expression's callee token, the one the structural query
// captures: the called identifier, or a called selector's field; null for
// any other callee, which is no call reference.
func (x *goLink) callee(call Node) Node {
	f := call.ChildByFieldId(x.k.fFunction)
	switch {
	case f.IsNull():
		return Node{}
	case f.KindId() == x.k.identifier:
		return f
	case f.KindId() == x.k.selectorExpression:
		if name := goChild(f, x.k.fField); !name.IsNull() && name.KindId() == x.k.fieldIdentifier {
			return name
		}
	}
	return Node{}
}

// site appends a call expression's site, when its callee is a call
// reference.
func (x *goLink) site(call Node) {
	k := x.k
	name := x.callee(call)
	if name.IsNull() {
		return
	}
	s := wire.Site{Name: goSpan(name), Shape: wire.ShapeBare}
	f := call.ChildByFieldId(k.fFunction)
	if f.KindId() == k.identifier {
		if v, ok := x.lookup(x.text(f)); ok && !v.bound {
			s.Shape, s.Signature, s.Receiver = wire.ShapeIndirect, v.sig, goCopiedType(v.typ)
		}
	} else {
		o := x.unparen(f.ChildByFieldId(k.fOperand))
		recv := x.methodExprReceiver(o)
		switch {
		case x.isImport(o):
			s.Shape, s.Qualifier = wire.ShapeStatic, x.copied(o)
		case recv != "":
			s.Shape, s.Qualifier = wire.ShapeStatic, strings.Clone(recv)
		default:
			s.Shape = wire.ShapeMember
			t := x.exprType(o)
			if t.Kind == wire.TypeNamed {
				if v, ok := x.types[t.Name][x.text(name)]; ok && v.sig != "" {
					s.Shape, s.Signature = wire.ShapeIndirect, v.sig
					t = wire.Type{Kind: wire.TypeField, Name: x.text(name)}
				}
			}
			s.Receiver = goCopiedType(t)
		}
	}
	if args := goChild(call, k.fArguments); !args.IsNull() {
		goNamed(args, func(a Node) { s.Args = append(s.Args, goCopiedType(x.exprType(a))) })
	}
	x.out.Sites = append(x.out.Sites, s)
}

// passed sends the values a call's arguments pass, whatever its callee; the
// arguments of new and make are types.
func (x *goLink) passed(call Node) {
	f := call.ChildByFieldId(x.k.fFunction)
	if !f.IsNull() && f.KindId() == x.k.identifier && (x.text(f) == "new" || x.text(f) == "make") && !x.local(x.text(f)) {
		return
	}
	if args := goChild(call, x.k.fArguments); !args.IsNull() {
		goNamed(args, func(a Node) { x.value(a, "") })
	}
}

// values sends a value for every function in a list of values, each written
// to the field fieldOf names for its position.
func (x *goLink) values(l Node, fieldOf func(i int) string) {
	for i, e := range x.list(l) {
		x.value(e, fieldOf(i))
	}
}

// value sends e when it is a function or method taken as a value: a
// package-level function of the file, a name of the package no file-level
// declaration of the file makes a variable, constant or type, a local
// holding a function, a package's `pkg.F`, a method expression `T.M`, or a
// method value `x.M` whose receiver's type is recovered and declares no
// field M.
func (x *goLink) value(e Node, field string) {
	k := x.k
	e = x.unparen(e)
	if e.IsNull() {
		return
	}
	var fn wire.Type
	switch e.KindId() {
	case k.identifier:
		name := x.text(e)
		if v, ok := x.lookup(name); ok {
			if v.typ.Kind == wire.TypeFunction {
				fn = v.typ
			}
			break
		}
		if _, ok := x.types[name]; ok || x.consts[name] || x.imports[name] || goPredeclared[name] {
			break
		}
		fn = wire.Type{Kind: wire.TypeFunction, Name: name}
	case k.selectorExpression:
		fn = x.selected(e)
	}
	if fn.Name == "" {
		return
	}
	x.out.Values = append(x.out.Values, wire.Value{At: goSpan(e), Field: field, Type: goCopiedType(fn)})
}

// selected is the function a selector takes as a value, a TypeFunction:
// "pkg.F" through an import; "T.M" for a method expression `T.M` on a file
// type and "*T.M" for `(*T).M`, each with Index 1; and "T.M" for a method
// value whose receiver's type T is recovered and declares no field M. It is
// TypeUnknown for any other selector.
func (x *goLink) selected(e Node) wire.Type {
	k := x.k
	o, f := x.unparen(e.ChildByFieldId(k.fOperand)), goChild(e, k.fField)
	fn := func(name string, receivers uint32) wire.Type {
		return wire.Type{Kind: wire.TypeFunction, Name: name + "." + x.text(f), Index: receivers}
	}
	switch {
	case f.IsNull():
		return wire.Type{}
	case x.isImport(o):
		return fn(x.text(o), 0)
	}
	if recv := x.methodExprReceiver(o); recv != "" {
		return fn(recv, 1)
	}
	if t := x.exprType(o); t.Kind == wire.TypeNamed {
		if _, isField := x.types[t.Name][x.text(f)]; !isField {
			return fn(t.Name, 0)
		}
	}
	return wire.Type{}
}

// goPredeclared are the predeclared identifiers that are values but never
// functions ("Predeclared identifiers").
var goPredeclared = map[string]bool{"nil": true, "true": true, "false": true, "iota": true}

// supertypes appends a declared type's embedded interfaces and embedded
// struct fields, each by its type's name.
func (x *goLink) supertypes(spec, name Node) {
	k := x.k
	t := x.unparen(spec.ChildByFieldId(k.fType))
	if t.IsNull() {
		return
	}
	add := func(typ Node) {
		if super := x.baseName(typ); super != "" {
			x.out.Supertypes = append(x.out.Supertypes, wire.Supertype{Type: x.copied(name), Super: strings.Clone(super)})
		}
	}
	switch t.KindId() {
	case k.interfaceType:
		goNamed(t, func(e Node) {
			if e.KindId() != k.typeElem {
				return
			}
			var only Node
			n := 0
			goNamed(e, func(c Node) { only = c; n++ })
			if n == 1 {
				add(only)
			}
		})
	case k.structType:
		x.structFields(t, func(field, typ Node) {
			if field.IsNull() {
				add(typ)
			}
		})
	}
}

// boundFunctions appends the export of every name a short variable
// declaration binds to a function literal, paired by position as the
// extraction pairs them (extract.go, addBoundFunctions).
func (x *goLink) boundFunctions(n Node) {
	k := x.k
	names, vals := x.list(n.ChildByFieldId(k.fLeft)), x.list(n.ChildByFieldId(k.fRight))
	for i := range min(len(names), len(vals)) {
		id, v := names[i], vals[i]
		if id.KindId() != k.identifier || v.KindId() != k.funcLiteral || v.ChildByFieldId(k.fBody).IsNull() {
			continue
		}
		if x.blankDecl(n, id) {
			continue
		}
		x.export(v, id, "", true, false)
	}
}

// goLinkSyntax holds the kind and field ids the Go linker matches, resolved
// once by name against the pinned grammar. Fields the registry does not
// list (field, path, key, arguments) are read from each child's own record
// (goChild), never through ChildByFieldId.
type goLinkSyntax struct {
	identifier, fieldIdentifier, typeIdentifier, packageIdentifier, sourceFile, block, expressionList,
	parenExpression, parenType, callExpression, selectorExpression, unaryExpression, compositeLiteral,
	typeAssertionExpression, literalValue, keyedElement, literalElement, funcLiteral, functionDeclaration,
	methodDeclaration, methodElem, parameterList, parameterDeclaration, variadicParameterDeclaration,
	functionType, pointerType, genericType, qualifiedType, structType, interfaceType, typeElem,
	fieldDeclarationList, fieldDeclaration, typeSpec, varSpec, constSpec, importSpec, shortVarDeclaration,
	assignmentStatement, returnStatement, sendStatement, rangeClause, receiveStatement, ifStatement,
	forStatement, expressionSwitchStatement, typeSwitchStatement, selectStatement, expressionCase, typeCase,
	defaultCase, communicationCase uint16

	amp, star, define uint16

	fAlias, fBody, fFunction, fLeft, fName, fOperand, fOperator, fParameters, fReceiver, fResult, fRight,
	fType, fValue uint16
	// Unregistered: read from the child's record only.
	fField, fPath, fKey, fArguments uint16
}

var (
	goLinkSyntaxOnce  sync.Once
	goLinkSyntaxTable *goLinkSyntax
)

// goLinkSyntaxOf resolves the table once. A name the grammar does not
// define panics, so a misspelt kind can never silently match nothing.
func goLinkSyntaxOf() *goLinkSyntax {
	goLinkSyntaxOnce.Do(func() {
		const language = "go"
		tl := mustGrammar(language)
		kind := func(name string) uint16 { return mustKind(tl, language, name, true) }
		tok := func(name string) uint16 { return mustKind(tl, language, name, false) }
		field := func(name string) uint16 { return mustField(tl, language, name) }
		raw := func(name string) uint16 {
			v := tl.FieldIdForName(name)
			if v == 0 {
				panic("worker: the " + language + " grammar has no field " + name)
			}
			return v
		}
		s := &goLinkSyntax{}
		s.identifier, s.fieldIdentifier, s.typeIdentifier = kind("identifier"), kind("field_identifier"), kind("type_identifier")
		s.packageIdentifier, s.sourceFile, s.block = kind("package_identifier"), kind("source_file"), kind("block")
		s.expressionList, s.parenExpression, s.parenType = kind("expression_list"), kind("parenthesized_expression"), kind("parenthesized_type")
		s.callExpression, s.selectorExpression, s.unaryExpression = kind("call_expression"), kind("selector_expression"), kind("unary_expression")
		s.compositeLiteral, s.typeAssertionExpression = kind("composite_literal"), kind("type_assertion_expression")
		s.literalValue, s.keyedElement, s.literalElement = kind("literal_value"), kind("keyed_element"), kind("literal_element")
		s.funcLiteral, s.functionDeclaration, s.methodDeclaration = kind("func_literal"), kind("function_declaration"), kind("method_declaration")
		s.methodElem, s.parameterList, s.parameterDeclaration = kind("method_elem"), kind("parameter_list"), kind("parameter_declaration")
		s.variadicParameterDeclaration, s.functionType = kind("variadic_parameter_declaration"), kind("function_type")
		s.pointerType, s.genericType, s.qualifiedType = kind("pointer_type"), kind("generic_type"), kind("qualified_type")
		s.structType, s.interfaceType, s.typeElem = kind("struct_type"), kind("interface_type"), kind("type_elem")
		s.fieldDeclarationList, s.fieldDeclaration = kind("field_declaration_list"), kind("field_declaration")
		s.typeSpec, s.varSpec, s.constSpec, s.importSpec = kind("type_spec"), kind("var_spec"), kind("const_spec"), kind("import_spec")
		s.shortVarDeclaration, s.assignmentStatement = kind("short_var_declaration"), kind("assignment_statement")
		s.returnStatement, s.sendStatement = kind("return_statement"), kind("send_statement")
		s.rangeClause, s.receiveStatement, s.ifStatement = kind("range_clause"), kind("receive_statement"), kind("if_statement")
		s.forStatement, s.expressionSwitchStatement = kind("for_statement"), kind("expression_switch_statement")
		s.typeSwitchStatement, s.selectStatement = kind("type_switch_statement"), kind("select_statement")
		s.expressionCase, s.typeCase, s.defaultCase = kind("expression_case"), kind("type_case"), kind("default_case")
		s.communicationCase = kind("communication_case")
		s.amp, s.star, s.define = tok("&"), tok("*"), tok(":=")
		s.fAlias, s.fBody, s.fFunction, s.fLeft = field("alias"), field("body"), field("function"), field("left")
		s.fName, s.fOperand, s.fOperator = field("name"), field("operand"), field("operator")
		s.fParameters, s.fReceiver, s.fResult = field("parameters"), field("receiver"), field("result")
		s.fRight, s.fType, s.fValue = field("right"), field("type"), field("value")
		s.fField, s.fPath, s.fKey, s.fArguments = raw("field"), raw("path"), raw("key"), raw("arguments")
		goLinkSyntaxTable = s
	})
	return goLinkSyntaxTable
}
