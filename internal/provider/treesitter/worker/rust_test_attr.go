package worker

import (
	"strings"
	"sync"

	ts "github.com/tree-sitter/go-tree-sitter"
)

// rsTestSyntax is the Rust grammar's ids the test attribute check reads.
type rsTestSyntax struct {
	attribute, ident, tokenTree, comma uint16
	fArguments                         uint16
}

var (
	rsTestOnce   sync.Once
	rsTestTables *rsTestSyntax
)

// rsTestSyntaxOf resolves the table once from tl, the Rust grammar (passed
// in, since the grammar table's own test rule calls it).
func rsTestSyntaxOf(tl *ts.Language) *rsTestSyntax {
	rsTestOnce.Do(func() {
		const language = "rust"
		kind := func(name string) uint16 { return mustKind(tl, language, name, true) }
		f := tl.FieldIdForName("arguments")
		if f == 0 {
			panic("worker: the rust grammar has no field arguments")
		}
		rsTestTables = &rsTestSyntax{attribute: kind("attribute"), ident: kind("identifier"), tokenTree: kind("token_tree"),
			comma: mustKind(tl, language, ",", false), fArguments: f}
	})
	return rsTestTables
}

// rustTestAttribute reports that the attribute item a, parsed with the Rust
// grammar tl, makes the function it
// is on a test: a test attribute (`#[test]`, or a path ending in `::test`, a
// test framework's), or a `#[cfg(p)]` whose predicate p holds only when the
// `test` option is set, so the function exists only in a test build
// (`cfg(test)`, `cfg(all(test, unix))`; never `cfg(not(test))` or
// `cfg(any(test, feature = "x"))`). The predicate is evaluated as the Rust
// Reference's "Conditional compilation" defines all, any and not, with
// `test` unset and every other option unknown: a predicate that is then
// false whatever the others are needs `test`.
func rustTestAttribute(a *ts.Node, tl *ts.Language, src []byte) bool {
	s := rsTestSyntaxOf(tl)
	attr := childOfKind(*a, s.attribute)
	if attr == nil {
		return false
	}
	path := attr.NamedChild(0)
	if path == nil {
		return false
	}
	name := string(src[path.StartByte():path.EndByte()])
	if name == "test" || strings.HasSuffix(name, "::test") {
		return true
	}
	args := attr.ChildByFieldId(s.fArguments)
	if name != "cfg" || args == nil {
		return false
	}
	parts := cfgParts(args, s)
	return len(parts) == 1 && cfgValue(parts[0], s, src) == cfgFalse
}

// A configuration predicate's value with `test` unset: false, unknown (it
// depends on another option) or true.
const (
	cfgFalse = iota
	cfgUnknown
	cfgTrue
)

// cfgParts is the children of the token tree t between its delimiters,
// split at its own commas.
func cfgParts(t *ts.Node, s *rsTestSyntax) [][]*ts.Node {
	var parts [][]*ts.Node
	var cur []*ts.Node
	n := t.ChildCount()
	for i := uint(1); i+1 < n; i++ {
		c := t.Child(i)
		switch {
		case c == nil || c.IsExtra():
		case !c.IsNamed() && c.KindId() == s.comma:
			parts, cur = append(parts, cur), nil
		default:
			cur = append(cur, c)
		}
	}
	if len(cur) > 0 {
		parts = append(parts, cur)
	}
	return parts
}

// cfgValue is the value of the predicate written as the tokens p.
func cfgValue(p []*ts.Node, s *rsTestSyntax, src []byte) int {
	word := func(n *ts.Node) string { return string(src[n.StartByte():n.EndByte()]) }
	switch {
	case len(p) == 1:
		switch word(p[0]) {
		case "test":
			return cfgFalse
		case "true":
			return cfgTrue
		case "false":
			return cfgFalse
		}
		return cfgUnknown
	case len(p) == 2 && p[0].KindId() == s.ident && p[1].KindId() == s.tokenTree:
		args := cfgParts(p[1], s)
		switch word(p[0]) {
		case "all":
			v := cfgTrue
			for _, a := range args {
				v = min(v, cfgValue(a, s, src))
			}
			return v
		case "any":
			v := cfgFalse
			for _, a := range args {
				v = max(v, cfgValue(a, s, src))
			}
			return v
		case "not":
			if len(args) == 1 {
				return cfgTrue - cfgValue(args[0], s, src)
			}
		}
	}
	return cfgUnknown
}
