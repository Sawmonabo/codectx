package worker

import (
	"sync"

	ts "github.com/tree-sitter/go-tree-sitter"
)

// A macro's input is evidence, not proof, of what its expansion declares
// (macro_rust.go). One shape proves it: a `macro_rules!` every rule of which
// binds its input as item fragments (`$x:item`, or token trees `$t:tt` under
// a repetition) and transcribes each such binding exactly once, under the
// same repetitions and inside no other group, so the expansion holds every
// input item as written, with at most attributes added around it. tokio's
// `macro_rules! cfg_rt { ($($item:item)*) => { $( #[cfg(feature = "rt")]
// $item )* } }` is one. The check reads the definition alone; it is no
// expander, and it applies to a definition the file itself holds in textual
// scope at the invocation (the Rust Reference's "Macros By Example",
// textual scope: after the definition, inside the module holding it).

// rsMacroRulesSyntax is the Rust grammar's ids the shape check reads.
type rsMacroRulesSyntax struct {
	macroDefinition, macroRule, tokenTree, tokenRepetition         uint16
	tokenRepetitionPattern, tokenBindingPattern, fragmentSpecifier uint16
	metavariable                                                   uint16
	fName, fType, fLeft, fRight                                    uint16
}

var (
	rsMacroRulesOnce   sync.Once
	rsMacroRulesTables *rsMacroRulesSyntax
)

func rsMacroRulesSyntaxOf() *rsMacroRulesSyntax {
	rsMacroRulesOnce.Do(func() {
		const language = "rust"
		tl := mustGrammar(language)
		kind := func(name string) uint16 { return mustKind(tl, language, name, true) }
		field := func(name string) uint16 {
			v := tl.FieldIdForName(name)
			if v == 0 {
				panic("worker: the rust grammar has no field " + name)
			}
			return v
		}
		rsMacroRulesTables = &rsMacroRulesSyntax{
			macroDefinition: kind("macro_definition"), macroRule: kind("macro_rule"), tokenTree: kind("token_tree"),
			tokenRepetition: kind("token_repetition"), tokenRepetitionPattern: kind("token_repetition_pattern"),
			tokenBindingPattern: kind("token_binding_pattern"), fragmentSpecifier: kind("fragment_specifier"),
			metavariable: kind("metavariable"),
			fName:        field("name"), fType: field("type"), fLeft: field("left"), fRight: field("right"),
		}
	})
	return rsMacroRulesTables
}

// macroExact reports that the declaration starting at start, the input of
// the macro named path, is exactly an item of the expansion: path is one
// identifier naming a `macro_rules!` of the file in textual scope at start
// whose every rule transcribes its item input unchanged. The file's
// definitions are indexed by name once, and each definition is checked
// once.
func (e *extraction) macroExact(path string, start uint) bool {
	m := e.macros
	if path == "" || m == nil {
		return false
	}
	s := rsMacroRulesSyntaxOf()
	if m.rules == nil {
		m.rules = map[string][]ts.Node{}
		for _, byName := range e.decls {
			for _, d := range byName {
				if d.node.KindId() == s.macroDefinition && d.name != "" {
					m.rules[d.name] = append(m.rules[d.name], d.node)
				}
			}
		}
		m.exact = map[uintptr]bool{}
	}
	def := inScope(m.rules[path], start)
	if def == nil {
		return false
	}
	exact, seen := m.exact[def.Id()]
	if !seen {
		exact = transcribesItems(def, s, e.src)
		m.exact[def.Id()] = exact
	}
	return exact
}

// inScope is the one of defs in textual scope at offset at: the last one
// ending before at whose parent (its module's item list) holds at, or nil.
func inScope(defs []ts.Node, at uint) *ts.Node {
	var best *ts.Node
	for i := range defs {
		n := &defs[i]
		if n.EndByte() > at {
			continue
		}
		if p := n.Parent(); p == nil || p.StartByte() > at || at >= p.EndByte() {
			continue
		}
		if best == nil || n.StartByte() > best.StartByte() {
			best = n
		}
	}
	return best
}

// transcribesItems reports that every rule of the macro_rules definition def
// binds item input and transcribes each binding of it exactly once, at the
// depth of repetitions it was bound at, inside no group but repetitions.
func transcribesItems(def *ts.Node, s *rsMacroRulesSyntax, src []byte) bool {
	rules := 0
	for i := range def.NamedChildCount() {
		r := def.NamedChild(i)
		if r == nil || r.KindId() != s.macroRule {
			continue
		}
		rules++
		left, right := r.ChildByFieldId(s.fLeft), r.ChildByFieldId(s.fRight)
		if left == nil || right == nil {
			return false
		}
		bound := map[string]int{}
		itemBindings(left, s, src, 0, bound)
		if len(bound) == 0 {
			return false
		}
		uses := map[string]int{}
		ok := true
		transcribed(right, s, src, 0, true, bound, uses, &ok)
		if !ok {
			return false
		}
		for name := range bound {
			if uses[name] != 1 {
				return false
			}
		}
	}
	return rules > 0
}

// itemBindings records in bound, by name, the repetition depth of each
// binding under the pattern n that takes item input: an `item` fragment
// anywhere, a `tt` fragment under at least one repetition.
func itemBindings(n *ts.Node, s *rsMacroRulesSyntax, src []byte, depth int, bound map[string]int) {
	for i := range n.NamedChildCount() {
		c := n.NamedChild(i)
		switch c.KindId() {
		case s.tokenRepetitionPattern:
			itemBindings(c, s, src, depth+1, bound)
		case s.tokenBindingPattern:
			name, typ := c.ChildByFieldId(s.fName), c.ChildByFieldId(s.fType)
			if name == nil || typ == nil {
				continue
			}
			switch frag := typ.Utf8Text(src); {
			case frag == "item", frag == "tt" && depth > 0:
				bound[name.Utf8Text(src)] = depth
			}
		default:
			itemBindings(c, s, src, depth, bound)
		}
	}
}

// transcribed counts in uses each use of a binding of bound under the
// transcriber n, which is at depth repetitions and, when top, inside no
// group but repetitions; a use at another depth, or inside a group, clears
// ok.
func transcribed(n *ts.Node, s *rsMacroRulesSyntax, src []byte, depth int, top bool, bound, uses map[string]int, ok *bool) {
	for i := range n.NamedChildCount() {
		c := n.NamedChild(i)
		switch c.KindId() {
		case s.metavariable:
			name := c.Utf8Text(src)
			d, item := bound[name]
			if !item {
				continue
			}
			uses[name]++
			if d != depth || !top {
				*ok = false
			}
		case s.tokenRepetition:
			transcribed(c, s, src, depth+1, top, bound, uses, ok)
		case s.tokenTree:
			transcribed(c, s, src, depth, false, bound, uses, ok)
		default:
			transcribed(c, s, src, depth, top, bound, uses, ok)
		}
	}
}
