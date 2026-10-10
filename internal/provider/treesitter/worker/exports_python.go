package worker

import (
	"slices"
	"strings"

	ts "github.com/tree-sitter/go-tree-sitter"
)

// pyAll is what a Python module's `__all__` says it exports, read from the
// static forms the type checkers read (PEP 8, the typed-library rules): an
// assignment of a list or tuple of string literals, `+=` of one, and
// `.extend` of one, `.append` and `.remove` of one string literal, each a
// statement of the module itself. Any other statement that binds or changes
// `__all__` -- a computed value, one nested in a conditional or a loop, a
// method these forms do not name -- makes it computed, and the module falls
// back to the underscore rule.
type pyAll struct {
	// ops are the statements naming `__all__`, as the query captured them.
	ops []pyAllOp
	// present reports that the module binds or changes `__all__`;
	// computed that some statement doing so is not a static form.
	present, computed bool
	names             map[string]bool
}

// pyAllOp is one captured statement naming `__all__`: an assignment, an
// augmented assignment, or a call of one of its methods (method).
type pyAllOp struct {
	node   ts.Node
	method string
}

// pyAllSyntax holds the Python kind and field ids the `__all__` forms are
// read by, resolved once per extraction.
type pyAllSyntax struct {
	module, expressionStatement, assignment, augmentedAssignment, call uint16
	list, tuple, str, stringStart, stringContent, interpolation        uint16
	fRight, fOperator, fArguments                                      uint16
}

func (e *extraction) pyAllSyntax() pyAllSyntax {
	tl := e.g.tsLanguage()
	id := func(name string) uint16 { return tl.IdForNodeKind(name, true) }
	return pyAllSyntax{
		module: id("module"), expressionStatement: id("expression_statement"), assignment: id("assignment"),
		augmentedAssignment: id("augmented_assignment"), call: id("call"), list: id("list"), tuple: id("tuple"),
		str: id("string"), stringStart: id("string_start"), stringContent: id("string_content"),
		interpolation: id("interpolation"),
		fRight:        tl.FieldIdForName("right"), fOperator: tl.FieldIdForName("operator"),
		fArguments: tl.FieldIdForName("arguments"),
	}
}

// settleAll evaluates the captured statements in source order.
func (e *extraction) settleAll() {
	a := &e.all
	if len(a.ops) == 0 {
		return
	}
	a.present = true
	a.names = map[string]bool{}
	slices.SortFunc(a.ops, func(x, y pyAllOp) int { return cmpUint(x.node.StartByte(), y.node.StartByte()) })
	x := e.pyAllSyntax()
	for _, op := range a.ops {
		if !e.pyAllStatic(&x, op) {
			a.computed = true
			return
		}
	}
}

// pyAllStatic applies one statement to the names and reports whether it is
// one of the static forms.
func (e *extraction) pyAllStatic(x *pyAllSyntax, op pyAllOp) bool {
	a := &e.all
	n := op.node
	stmt := n.Parent()
	if stmt == nil || stmt.KindId() != x.expressionStatement {
		return false
	}
	if mod := stmt.Parent(); mod == nil || mod.KindId() != x.module {
		return false
	}
	switch n.KindId() {
	case x.assignment:
		right := n.ChildByFieldId(x.fRight)
		if right == nil {
			// `__all__: list[str]` annotates and binds nothing.
			return true
		}
		names, ok := e.pyStrings(x, right)
		if !ok {
			return false
		}
		clear(a.names)
		for _, s := range names {
			a.names[s] = true
		}
		return true
	case x.augmentedAssignment:
		operator := n.ChildByFieldId(x.fOperator)
		right := n.ChildByFieldId(x.fRight)
		if operator == nil || operator.Utf8Text(e.src) != "+=" || right == nil {
			return false
		}
		names, ok := e.pyStrings(x, right)
		if !ok {
			return false
		}
		for _, s := range names {
			a.names[s] = true
		}
		return true
	case x.call:
		args := n.ChildByFieldId(x.fArguments)
		if args == nil || args.NamedChildCount() != 1 {
			return false
		}
		arg := args.NamedChild(0)
		switch op.method {
		case "extend":
			names, ok := e.pyStrings(x, arg)
			if !ok {
				return false
			}
			for _, s := range names {
				a.names[s] = true
			}
			return true
		case "append", "remove":
			s, ok := e.pyString(x, arg)
			if !ok {
				return false
			}
			if op.method == "append" {
				a.names[s] = true
			} else {
				delete(a.names, s)
			}
			return true
		}
	}
	return false
}

// pyStrings is the strings of a list or tuple of string literals, and false
// for any other expression.
func (e *extraction) pyStrings(x *pyAllSyntax, n *ts.Node) ([]string, bool) {
	if n.KindId() != x.list && n.KindId() != x.tuple {
		return nil, false
	}
	out := make([]string, 0, n.NamedChildCount())
	for i := range n.NamedChildCount() {
		c := n.NamedChild(i)
		if c == nil || c.IsExtra() {
			continue
		}
		s, ok := e.pyString(x, c)
		if !ok {
			return nil, false
		}
		out = append(out, s)
	}
	return out, true
}

// pyString is a string literal's text, and false for anything else: a
// formatted string with an interpolation, a byte string, a concatenation.
func (e *extraction) pyString(x *pyAllSyntax, n *ts.Node) (string, bool) {
	if n == nil || n.KindId() != x.str {
		return "", false
	}
	var text string
	for i := range n.NamedChildCount() {
		c := n.NamedChild(i)
		switch c.KindId() {
		case x.interpolation:
			return "", false
		case x.stringStart:
			if strings.ContainsAny(c.Utf8Text(e.src), "bB") {
				return "", false
			}
		case x.stringContent:
			text += c.Utf8Text(e.src)
		}
	}
	return text, true
}

// pyExported is Python's export rule for a top-level declaration: the names
// of a static `__all__` when the module has one; otherwise, and when
// `__all__` is computed, every name without a leading underscore.
func pyExported(e *extraction, d *decl) bool {
	if e.all.present && !e.all.computed {
		return e.all.names[d.name]
	}
	return !strings.HasPrefix(d.name, "_")
}
