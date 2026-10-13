package worker

import "github.com/Sawmonabo/codectx/internal/provider/treesitter/wire"

// CommonJS exports, each a module export record (wire.Reexport with no
// From for a local name, with From for another module's), read as the
// TypeScript binder and Node's own export detection read them, by syntax at
// any depth of the file:
//
//   - `module.exports = x`, the right-most value of an assignment chain
//     (`module.exports = exports = f`): the module's whole value
//     (wire.ExportAssignment), which a call of the module itself reaches,
//     and, unless the file marks itself `__esModule`, its default export.
//     An identifier aliases the declaration or import it names, a function
//     expression the declaration the structural query makes of it
//     (`exports`). An object literal exports each key whose value is an
//     identifier (a shorthand key included) as that name, and a spread
//     `...require("./x")` re-exports x. `module.exports = require("./x")`
//     re-exports x whole.
//   - `exports.x = v`, `module.exports.x = v`, `exports["x"] = v` with v an
//     identifier: the named export x aliasing v (a function value is the
//     Exported declaration itself). `default` is an ordinary name here.
//   - `Object.defineProperty(exports, "x", { value: v })` or
//     `{ get() { return v } }`: the named export x aliasing v; `__esModule`
//     marks the file.
//   - `exports = …` or `module.exports = …` outside a chain assigning both
//     unbinds `exports` from the module's value (Node): no `exports.x` of
//     the file is then an export.
//
// A file written with ES module syntax (an import or export statement) is
// no CommonJS module, and none of these is read in it.
type cjsExports struct {
	esm, marked, unbound bool
	// whole is the module's whole value: a local name, or with wholeFrom
	// another module's whole value.
	whole, wholeFrom string
	named            []cjsNamed
}

// cjsNamed is one named export: name aliasing original, of the module from
// when it is set, and written through `exports` when viaExports.
type cjsNamed struct {
	name, original, from string
	viaExports           bool
}

// cjsHolder is what the expression e denotes: 1 `module.exports`, 2
// `exports`, 0 anything else.
func (k *javaScriptLinker) cjsHolder(e Node) int {
	kk := k.k
	e = k.l.unparen(e)
	switch jsKindOf(e) {
	case kk.identifier:
		if string(textOf(k.src, e)) == "exports" {
			return 2
		}
	case kk.memberExpression:
		obj, p := e.ChildByFieldId(kk.fObject), e.ChildByFieldId(kk.fProperty)
		if jsKindOf(obj) == kk.identifier && string(textOf(k.src, obj)) == "module" &&
			jsKindOf(p) == k.x.propertyIdentifier && string(textOf(k.src, p)) == "exports" {
			return 1
		}
	}
	return 0
}

// cjsAssignment reads the assignment n as a CommonJS export.
func (k *javaScriptLinker) cjsAssignment(n Node) {
	kk := k.k
	left := k.l.unparen(n.ChildByFieldId(kk.fLeft))
	right := n.ChildByFieldId(kk.fRight)
	switch k.cjsHolder(left) {
	case 1:
		if !k.inChain(n, 2) {
			// `exports` keeps the object `module.exports` held before.
			k.cjs.unbound = true
		}
		k.cjsWhole(k.rightMost(right))
		return
	case 2:
		if !k.inChain(n, 1) {
			k.cjs.unbound = true
		}
		return
	}
	var holder int
	var name string
	switch jsKindOf(left) {
	case kk.memberExpression:
		if p := left.ChildByFieldId(kk.fProperty); jsKindOf(p) == k.x.propertyIdentifier {
			holder, name = k.cjsHolder(left.ChildByFieldId(kk.fObject)), string(textOf(k.src, p))
		}
	case kk.subscriptExpression:
		if i := left.ChildByFieldId(kk.fIndex); jsKindOf(i) == k.x.str {
			holder, name = k.cjsHolder(left.ChildByFieldId(kk.fObject)), k.unquote(i)
		}
	}
	if holder == 0 || name == "" {
		return
	}
	if name == "__esModule" {
		k.cjs.marked = true
		return
	}
	if v := k.l.unparen(k.rightMost(right)); jsKindOf(v) == kk.identifier {
		k.cjs.named = append(k.cjs.named, cjsNamed{name: name, original: string(textOf(k.src, v)), viaExports: holder == 2})
	}
}

// rightMost is the value an assignment chain starting at e assigns.
func (k *javaScriptLinker) rightMost(e Node) Node {
	for e = k.l.unparen(e); jsKindOf(e) == k.k.assignmentExpression; e = k.l.unparen(e.ChildByFieldId(k.k.fRight)) {
	}
	return e
}

// inChain reports an assignment chained with one to the holder other (1
// `module.exports`, 2 `exports`), which keeps the two bound to one object
// (`module.exports = exports = f`).
func (k *javaScriptLinker) inChain(n Node, other int) bool {
	kk := k.k
	for p := n; jsKindOf(p) == kk.assignmentExpression; {
		if k.cjsHolder(p.ChildByFieldId(kk.fLeft)) == other {
			return true
		}
		up := p.Parent()
		if jsKindOf(up) != kk.assignmentExpression || up.ChildByFieldId(kk.fRight) != p {
			break
		}
		p = up
	}
	for r := n.ChildByFieldId(kk.fRight); jsKindOf(r) == kk.assignmentExpression; r = r.ChildByFieldId(kk.fRight) {
		if k.cjsHolder(r.ChildByFieldId(kk.fLeft)) == other {
			return true
		}
	}
	return false
}

// cjsWhole reads the value v assigned to `module.exports`.
func (k *javaScriptLinker) cjsWhole(v Node) {
	kk, x := k.k, k.x
	switch jsKindOf(v) {
	case kk.identifier:
		k.cjs.whole, k.cjs.wholeFrom = string(textOf(k.src, v)), ""
	case x.arrowFunction, kk.functionExpression, kk.class:
		// The structural query declares the function or class by the name
		// or property it is stored under, `exports`.
		k.cjs.whole, k.cjs.wholeFrom = "exports", ""
	case kk.callExpression:
		if m, ok := k.required(v); ok {
			k.cjs.whole, k.cjs.wholeFrom = wire.ExportAssignment, m
			k.cjs.named = append(k.cjs.named, cjsNamed{name: "*", original: "*", from: m})
		}
	case x.object:
		for c := v.first(); !c.IsNull(); c = c.next() {
			switch {
			case !c.IsNamed() || c.IsExtra():
			case c.KindId() == x.shorthandPropertyIdentifier:
				name := string(textOf(k.src, c))
				k.cjs.named = append(k.cjs.named, cjsNamed{name: name, original: name})
			case c.KindId() == x.pair:
				key := k.propertyName(c.ChildByFieldId(kk.fKey))
				if val := k.l.unparen(c.ChildByFieldId(kk.fValue)); key != "" && jsKindOf(val) == kk.identifier {
					k.cjs.named = append(k.cjs.named, cjsNamed{name: key, original: string(textOf(k.src, val))})
				}
			case c.KindId() == kk.spreadElement:
				if call := firstNamed(c); jsKindOf(call) == kk.callExpression {
					if m, ok := k.required(call); ok {
						k.cjs.named = append(k.cjs.named, cjsNamed{name: "*", original: "*", from: m})
					}
				}
			}
		}
	}
}

// cjsDefineProperty reads `Object.defineProperty(exports, "x", desc)`, the
// call n, as a CommonJS export.
func (k *javaScriptLinker) cjsDefineProperty(n Node) {
	kk, x := k.k, k.x
	f := n.ChildByFieldId(kk.fFunction)
	if jsKindOf(f) != kk.memberExpression {
		return
	}
	obj, p := f.ChildByFieldId(kk.fObject), f.ChildByFieldId(kk.fProperty)
	if jsKindOf(obj) != kk.identifier || string(textOf(k.src, obj)) != "Object" || string(textOf(k.src, p)) != "defineProperty" {
		return
	}
	args := n.ChildByFieldId(kk.fArguments)
	var a []Node
	for c := args.first(); !c.IsNull(); c = c.next() {
		if c.IsNamed() && !c.IsExtra() {
			a = append(a, c)
		}
	}
	if len(a) < 3 || jsKindOf(a[1]) != x.str {
		return
	}
	holder := k.cjsHolder(a[0])
	name := k.unquote(a[1])
	if holder == 0 || name == "" {
		return
	}
	if name == "__esModule" {
		k.cjs.marked = true
		return
	}
	if jsKindOf(a[2]) != x.object {
		return
	}
	for c := a[2].first(); !c.IsNull(); c = c.next() {
		var key string
		var value Node
		switch {
		case !c.IsNamed() || c.IsExtra():
			continue
		case c.KindId() == x.pair:
			key, value = k.propertyName(c.ChildByFieldId(kk.fKey)), k.l.unparen(c.ChildByFieldId(kk.fValue))
		case c.KindId() == kk.methodDefinition:
			key, value = k.propertyName(c.ChildByFieldId(kk.fName)), c
		default:
			continue
		}
		var v Node
		switch key {
		case "value":
			v = value
		case "get":
			v = k.returned(value)
		default:
			continue
		}
		if jsKindOf(v) == kk.identifier {
			k.cjs.named = append(k.cjs.named, cjsNamed{name: name, original: string(textOf(k.src, v)), viaExports: holder == 2})
		}
	}
}

// returned is the expression a getter fn returns: an arrow function's
// expression body, or the value of the only return statement of its block.
func (k *javaScriptLinker) returned(fn Node) Node {
	kk := k.k
	switch jsKindOf(fn) {
	case k.x.arrowFunction, kk.functionExpression, kk.methodDefinition:
	default:
		return Node{}
	}
	body := fn.ChildByFieldId(kk.fBody)
	if jsKindOf(body) != kk.statementBlock {
		return k.l.unparen(body)
	}
	var ret Node
	for c := body.first(); !c.IsNull(); c = c.next() {
		if !c.IsNamed() || c.IsExtra() {
			continue
		}
		if c.KindId() != k.x.returnStatement || !ret.IsNull() {
			return Node{}
		}
		ret = c
	}
	return k.l.unparen(firstNamed(ret))
}

// cjsFinish appends the file's CommonJS export records to out.
func (k *javaScriptLinker) cjsFinish(out *wire.Link) {
	c := &k.cjs
	if c.esm {
		return
	}
	hasDefault := false
	for _, e := range c.named {
		if e.viaExports && c.unbound {
			continue
		}
		hasDefault = hasDefault || e.name == "default"
		out.Reexports = append(out.Reexports, wire.Reexport{Name: e.name, Original: e.original, From: e.from})
	}
	if c.whole == "" {
		return
	}
	out.Reexports = append(out.Reexports, wire.Reexport{Name: wire.ExportAssignment, Original: c.whole, From: c.wholeFrom})
	if !c.marked && !hasDefault {
		// An ES module's default import of a module that does not mark
		// itself `__esModule` is its whole value.
		out.Reexports = append(out.Reexports, wire.Reexport{Name: "default", Original: c.whole, From: c.wholeFrom})
	}
}
