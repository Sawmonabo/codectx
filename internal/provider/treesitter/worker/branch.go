package worker

import (
	"slices"
	"strconv"
	"strings"
	"sync"

	ts "github.com/tree-sitter/go-tree-sitter"
)

// A declaration's branch (wire.Decl.Branch) names the conditional branches
// between it and its parent declaration, or the file, outermost first. Each
// branch is one element, and the elements are joined by " / ":
//
//	if "<condition>" then | else | elif "<condition>"
//	try body | else | finally | catch | "<handler header>"   (__try alike)
//	switch "<subject>" "<case label>"       (match, select: the same shape;
//	                                         a label outside any switch is
//	                                         case "<case label>")
//	#if "<condition>" then | else           (#ifdef, #ifndef, #elif, #elifdef, #elifndef)
//	cfg "<predicate>"                       (a Rust #[cfg(...)] attribute)
//	cfg_attr "<predicate>" … cfg "<predicate>"
//	                                        (a Rust #[cfg_attr(p, cfg(q))],
//	                                         one element per cfg it applies;
//	                                         nested cfg_attr predicates in turn)
//	macro "<path>" | macro "<path>" "<header>"
//	                                        (a Rust item-position macro's body
//	                                         or brace group, macro_rust.go)
//
// A Rust `#[cfg]` or `#[cfg_attr]` applies to the item, variant, field or
// match arm it is an outer attribute of (the Rust Reference's "Conditional
// compilation": both "may be used anywhere attributes are allowed"); an
// arm's own elements follow its match element.
//
// A construct is spelled as its keyword, a condition, subject, label or
// header as written with its whitespace collapsed and quoted, so no text of
// the source can make two branches spell alike. Nothing in it is a position
// or a count: the two definitions of a name in the arms of one `if` differ
// by their arm, and an edit anywhere outside the conditions and labels on
// the path leaves a declaration's branch as it was.

// branchRole is what a node kind is to a branch.
type branchRole uint8

const (
	// branchIf is an if: its consequence is the "then" arm and its
	// alternative the "else" arm, or, for a Python elif clause, an "elif"
	// arm spelled with the clause's condition.
	branchIf branchRole = iota + 1
	// branchTry is a try: its body is the "body" arm and each clause the
	// arm its kind names (branchSyntax.arms).
	branchTry
	// branchSwitch is a switch, match or select, whose arms are its case
	// clauses.
	branchSwitch
	// branchCase is one case clause, the arm of the switch that is its
	// parent or grandparent.
	branchCase
	// branchPreproc is a preprocessor conditional: its lines are the "then"
	// arm and its alternative (the #elif or #else that follows) the "else"
	// arm. An #else is that arm and no construct of its own.
	branchPreproc
)

// branchLabel is how a case clause or a try clause spells itself.
type branchLabel uint8

const (
	// labelColon is the clause's text before its own `:` token: `case 1`,
	// `default`, `except ValueError as e`.
	labelColon branchLabel = iota + 1
	// labelBody is the clause's text before its body field:
	// `catch (IOException e)`.
	labelBody
	// labelSwitchLabels is a Java switch group's or rule's labels, joined
	// by ", ".
	labelSwitchLabels
	// labelPattern is a Rust match arm's pattern, guard included.
	labelPattern
	// labelWord is the clause's fixed word (branchArm.word).
	labelWord
)

// branchRule is one node kind's part in a branch.
type branchRule struct {
	role branchRole
	// construct is the keyword the element starts with; empty for a
	// directive whose keyword is its first token as the grammar names it
	// (`#ifdef` or `#ifndef`).
	construct string
	// cond are the fields whose children, from the first one's start to the
	// last one's end, are the condition or subject.
	cond []uint16
	// label is a case clause's label.
	label branchLabel
}

// branchArm is how a try's clause of one kind names its arm.
type branchArm struct {
	label branchLabel
	word  string
}

// branchSyntax is one grammar's branch table, by kind id.
type branchSyntax struct {
	rules []branchRule
	arms  []branchArm
	// elif is the Python elif clause kind, 0 elsewhere.
	elif uint16
	// colon is the `:` token, 0 in a grammar no label reads it in.
	colon uint16
	// switchLabel is the Java switch label kind, 0 elsewhere.
	switchLabel uint16
	// cfg reports a grammar whose `#[cfg(...)]` attributes are branches:
	// attributeItem and attribute are its attribute kinds, identifier its
	// identifier kind.
	cfg                             bool
	attributeItem, attribute, ident uint16
	// matchArm is the Rust match arm kind, whose outer attributes are its
	// own children; comma is the `,` token a cfg_attr's arguments are
	// split at.
	matchArm, comma                uint16
	fCondition, fConsequence, fAlt uint16
	fBody, fPattern, fArguments    uint16
}

func (x *branchSyntax) rule(id uint16) branchRule {
	if int(id) < len(x.rules) {
		return x.rules[id]
	}
	return branchRule{}
}

func (x *branchSyntax) arm(id uint16) branchArm {
	if int(id) < len(x.arms) {
		return x.arms[id]
	}
	return branchArm{}
}

var (
	branchMu     sync.Mutex
	branchTables = map[string]*branchSyntax{}
)

// branchSyntaxOf is the branch table of language, the grammar tl, resolved
// once. A kind or field the grammar does not define panics, so a misspelt
// name never silently drops a branch.
func branchSyntaxOf(language string, tl *ts.Language) *branchSyntax {
	branchMu.Lock()
	defer branchMu.Unlock()
	if x, ok := branchTables[language]; ok {
		return x
	}
	x := resolveBranchSyntax(language, tl)
	branchTables[language] = x
	return x
}

func resolveBranchSyntax(language string, tl *ts.Language) *branchSyntax {
	kind := func(name string) uint16 { return mustKind(tl, language, name, true) }
	field := func(name string) uint16 {
		v := tl.FieldIdForName(name)
		if v == 0 {
			panic("worker: the " + language + " grammar has no field " + name)
		}
		return v
	}
	x := &branchSyntax{
		rules:      make([]branchRule, tl.NodeKindCount()),
		arms:       make([]branchArm, tl.NodeKindCount()),
		fCondition: field("condition"), fConsequence: field("consequence"), fAlt: field("alternative"),
		fBody: field("body"),
	}
	rule := func(name string, r branchRule) { x.rules[kind(name)] = r }
	arm := func(name string, a branchArm) { x.arms[kind(name)] = a }
	fields := func(names ...string) []uint16 {
		out := make([]uint16, len(names))
		for i, n := range names {
			out[i] = field(n)
		}
		return out
	}
	finally := branchArm{label: labelWord, word: "finally"}
	switch language {
	case "python":
		x.colon = mustKind(tl, language, ":", false)
		x.elif = kind("elif_clause")
		rule("if_statement", branchRule{role: branchIf, construct: "if", cond: fields("condition")})
		rule("try_statement", branchRule{role: branchTry, construct: "try"})
		rule("match_statement", branchRule{role: branchSwitch, construct: "match", cond: fields("subject")})
		rule("case_clause", branchRule{role: branchCase, label: labelColon})
		arm("except_clause", branchArm{label: labelColon})
		arm("else_clause", branchArm{label: labelWord, word: "else"})
		arm("finally_clause", finally)
	case "javascript", "typescript", "tsx":
		x.colon = mustKind(tl, language, ":", false)
		rule("if_statement", branchRule{role: branchIf, construct: "if", cond: fields("condition")})
		rule("try_statement", branchRule{role: branchTry, construct: "try"})
		rule("switch_statement", branchRule{role: branchSwitch, construct: "switch", cond: fields("value")})
		rule("switch_case", branchRule{role: branchCase, label: labelColon})
		rule("switch_default", branchRule{role: branchCase, label: labelColon})
		// A try has at most one catch clause, so its word names it.
		arm("catch_clause", branchArm{label: labelWord, word: "catch"})
		arm("finally_clause", finally)
	case "go":
		x.colon = mustKind(tl, language, ":", false)
		rule("if_statement", branchRule{role: branchIf, construct: "if", cond: fields("initializer", "condition")})
		rule("expression_switch_statement", branchRule{role: branchSwitch, construct: "switch", cond: fields("initializer", "value")})
		rule("type_switch_statement", branchRule{role: branchSwitch, construct: "switch", cond: fields("initializer", "alias", "value")})
		rule("select_statement", branchRule{role: branchSwitch, construct: "select"})
		for _, c := range []string{"expression_case", "type_case", "communication_case", "default_case"} {
			rule(c, branchRule{role: branchCase, label: labelColon})
		}
	case "java":
		x.switchLabel = kind("switch_label")
		rule("if_statement", branchRule{role: branchIf, construct: "if", cond: fields("condition")})
		rule("try_statement", branchRule{role: branchTry, construct: "try"})
		rule("try_with_resources_statement", branchRule{role: branchTry, construct: "try"})
		rule("switch_expression", branchRule{role: branchSwitch, construct: "switch", cond: fields("condition")})
		rule("switch_block_statement_group", branchRule{role: branchCase, label: labelSwitchLabels})
		rule("switch_rule", branchRule{role: branchCase, label: labelSwitchLabels})
		arm("catch_clause", branchArm{label: labelBody})
		arm("finally_clause", finally)
	case "c", "cpp":
		x.colon = mustKind(tl, language, ":", false)
		rule("preproc_if", branchRule{role: branchPreproc, construct: "#if", cond: fields("condition")})
		rule("preproc_ifdef", branchRule{role: branchPreproc, cond: fields("name")})
		rule("preproc_elif", branchRule{role: branchPreproc, construct: "#elif", cond: fields("condition")})
		rule("preproc_elifdef", branchRule{role: branchPreproc, cond: fields("name")})
		rule("if_statement", branchRule{role: branchIf, construct: "if", cond: fields("condition")})
		rule("switch_statement", branchRule{role: branchSwitch, construct: "switch", cond: fields("condition")})
		rule("case_statement", branchRule{role: branchCase, label: labelColon})
		rule("seh_try_statement", branchRule{role: branchTry, construct: "__try"})
		arm("seh_except_clause", branchArm{label: labelBody})
		arm("seh_finally_clause", finally)
		if language == "cpp" {
			rule("try_statement", branchRule{role: branchTry, construct: "try"})
			arm("catch_clause", branchArm{label: labelBody})
		}
	case "rust":
		x.cfg = true
		x.attributeItem, x.attribute, x.ident = kind("attribute_item"), kind("attribute"), kind("identifier")
		x.matchArm, x.comma = kind("match_arm"), mustKind(tl, language, ",", false)
		x.fPattern, x.fArguments = field("pattern"), field("arguments")
		rule("if_expression", branchRule{role: branchIf, construct: "if", cond: fields("condition")})
		rule("match_expression", branchRule{role: branchSwitch, construct: "match", cond: fields("value")})
		rule("match_arm", branchRule{role: branchCase, label: labelPattern})
	}
	return x
}

// branchLink is one branch element on the path from the file to a node,
// linked to the one enclosing it; depth counts the elements from the file.
type branchLink struct {
	text  string
	up    *branchLink
	depth int
	// macro is, on a macro element, the macro path whose input the
	// declarations under it are written in (macroInput).
	macro string
}

func (l *branchLink) push(text string) *branchLink {
	d := 1
	if l != nil {
		d = l.depth + 1
	}
	return &branchLink{text: text, up: l, depth: d}
}

// branchText is the branch of a declaration whose path is at, below its
// parent declaration's path parent (nil for the file): the elements at
// deeper than parent's, outermost first.
func branchText(at, parent *branchLink) string {
	floor := 0
	if parent != nil {
		floor = parent.depth
	}
	var parts []string
	for l := at; l != nil && l.depth > floor; l = l.up {
		parts = append(parts, l.text)
	}
	slices.Reverse(parts)
	return strings.Join(parts, " / ")
}

// branchWalk records the branch path of every declaration node.
type branchWalk struct {
	x      *branchSyntax
	e      *extraction
	starts []uint
}

// branches sets e.branchAt: the branch path of every declaration node that
// a conditional encloses, by its range. The walk enters only the nodes that
// hold a declaration's start, so its cost is the declarations' ancestors
// and their siblings, never a per-declaration climb from the root.
func (e *extraction) branches(root *ts.Node) {
	e.branchAt = nil
	if len(e.decls) == 0 {
		return
	}
	w := &branchWalk{x: branchSyntaxOf(e.l.Name, e.g.tsLanguage()), e: e, starts: make([]uint, 0, len(e.decls))}
	for s := range e.decls {
		w.starts = append(w.starts, s.start)
	}
	slices.Sort(w.starts)
	cur := root.Walk()
	defer cur.Close()
	w.children(cur, *root, nil, nil)
}

// holds reports whether a declaration node starts inside n.
func (w *branchWalk) holds(n *ts.Node) bool {
	start, end := n.StartByte(), n.EndByte()
	i, _ := slices.BinarySearch(w.starts, start)
	return i < len(w.starts) && (w.starts[i] < end || w.starts[i] == start)
}

// children walks the children of n, which cur is on, with n's path at; up is
// n's parent. cur is back on n when it returns.
func (w *branchWalk) children(cur *ts.TreeCursor, n ts.Node, up *ts.Node, at *branchLink) {
	if !cur.GotoFirstChild() {
		return
	}
	x := w.x
	// cfg holds the elements of the `#[cfg(...)]` and `#[cfg_attr(...)]`
	// attributes before the next item, which they apply to.
	var cfg []string
	for {
		c := cur.Node()
		f := cur.FieldId()
		switch {
		case c.IsExtra():
		case x.cfg && c.KindId() == x.attributeItem:
			cfg = w.cfgElements(c, cfg)
		default:
			if w.holds(c) {
				link := at
				for _, p := range cfg {
					link = link.push(p)
				}
				if t, ok := w.arm(&n, f, c); ok {
					link = link.push(t)
				}
				if t, ok := w.caseArm(c, &n, up); ok {
					link = link.push(t)
				}
				if x.cfg && c.KindId() == x.matchArm {
					link = w.armCfg(c, link)
				}
				if link != nil {
					if s := (span{c.StartByte(), c.EndByte()}); w.e.decls[s] != nil {
						if w.e.branchAt == nil {
							w.e.branchAt = map[span]*branchLink{}
						}
						w.e.branchAt[s] = link
					}
				}
				if bodies := w.e.macroBodiesOf(c); bodies != nil {
					for _, b := range bodies {
						w.macroBody(b, link)
					}
				} else {
					w.children(cur, *c, &n, link)
				}
			}
			cfg = cfg[:0]
		}
		if !cur.GotoNextSibling() {
			break
		}
	}
	cur.GotoParent()
}

// macroBody walks the kept re-parse b of a macro invocation's body, whose
// path is at, under b's macro element: the invocation's own tokens hold no
// declaration, its re-parse does.
func (w *branchWalk) macroBody(b macroBody, at *branchLink) {
	link := at.push(b.element)
	link.macro = b.path
	cur := b.root.Walk()
	defer cur.Close()
	w.children(cur, b.root, nil, link)
}

// arm is the element conditional n contributes for its child c in field f,
// and false when n is no conditional or c is no arm of it (a condition).
func (w *branchWalk) arm(n *ts.Node, f uint16, c *ts.Node) (string, bool) {
	x := w.x
	r := x.rule(n.KindId())
	switch r.role {
	case branchIf:
		head := r.construct + " " + strconv.Quote(w.fieldsText(n, r.cond))
		switch {
		case f == x.fConsequence:
			return head + " then", true
		case f == x.fAlt && x.elif != 0 && c.KindId() == x.elif:
			return head + " elif " + strconv.Quote(w.fieldsText(c, []uint16{x.fCondition})), true
		case f == x.fAlt:
			return head + " else", true
		}
	case branchTry:
		if f == x.fBody {
			return r.construct + " body", true
		}
		switch a := x.arm(c.KindId()); a.label {
		case 0:
		case labelWord:
			return r.construct + " " + a.word, true
		default:
			return r.construct + " " + strconv.Quote(w.label(c, a.label)), true
		}
	case branchPreproc:
		if !c.IsNamed() || slices.Contains(r.cond, f) {
			return "", false
		}
		construct := r.construct
		if construct == "" {
			if k := n.Child(0); k != nil {
				construct = k.Kind()
			}
		}
		head := construct + " " + strconv.Quote(w.fieldsText(n, r.cond))
		if f == x.fAlt {
			return head + " else", true
		}
		return head + " then", true
	}
	return "", false
}

// caseArm is the element of case clause c, child of n and grandchild of up:
// its switch, the one of n and up that is one, with the clause's label.
func (w *branchWalk) caseArm(c, n, up *ts.Node) (string, bool) {
	x := w.x
	r := x.rule(c.KindId())
	if r.role != branchCase {
		return "", false
	}
	label := strconv.Quote(w.label(c, r.label))
	sw := n
	if x.rule(sw.KindId()).role != branchSwitch {
		sw = up
	}
	if sw == nil || x.rule(sw.KindId()).role != branchSwitch {
		return "case " + label, true
	}
	s := x.rule(sw.KindId())
	return s.construct + " " + strconv.Quote(w.fieldsText(sw, s.cond)) + " " + label, true
}

// label spells clause q by how.
func (w *branchWalk) label(q *ts.Node, how branchLabel) string {
	x := w.x
	switch how {
	case labelColon:
		for i := range q.ChildCount() {
			if c := q.Child(i); c != nil && !c.IsNamed() && c.KindId() == x.colon {
				return w.text(q.StartByte(), c.StartByte())
			}
		}
	case labelBody:
		if b := q.ChildByFieldId(x.fBody); b != nil {
			return w.text(q.StartByte(), b.StartByte())
		}
	case labelSwitchLabels:
		var parts []string
		for i := range q.NamedChildCount() {
			if c := q.NamedChild(i); c != nil && c.KindId() == x.switchLabel {
				parts = append(parts, w.text(c.StartByte(), c.EndByte()))
			}
		}
		return strings.Join(parts, ", ")
	case labelPattern:
		if p := q.ChildByFieldId(x.fPattern); p != nil {
			return w.text(p.StartByte(), p.EndByte())
		}
	}
	return ""
}

// fieldsText is the text of n's children in fields, from the first one's
// start to the last one's end, or empty when n has none.
func (w *branchWalk) fieldsText(n *ts.Node, fields []uint16) string {
	if len(fields) == 0 {
		return ""
	}
	cur := n.Walk()
	defer cur.Close()
	var start, end uint
	found := false
	for ok := cur.GotoFirstChild(); ok; ok = cur.GotoNextSibling() {
		if !slices.Contains(fields, cur.FieldId()) {
			continue
		}
		c := cur.Node()
		if !found {
			start, found = c.StartByte(), true
		}
		end = c.EndByte()
	}
	if !found {
		return ""
	}
	return w.text(start, end)
}

// cfgElements appends to dst the branch elements of the attribute item a:
// `cfg "<predicate>"` for a `#[cfg(...)]`, the predicate its arguments as
// written; for a `#[cfg_attr(p, a1, a2, ...)]` one element for each ai that
// is a `cfg(q)`, `cfg_attr "p" cfg "(q)"`, and for each that is a nested
// `cfg_attr(p2, ...)` its own elements prefixed the same way, since a nested
// cfg_attr applies when both predicates hold (the Rust Reference's
// "Conditional compilation"). Every other attribute, and every other
// attribute a cfg_attr applies (a derive, a doc, a path), adds nothing.
func (w *branchWalk) cfgElements(a *ts.Node, dst []string) []string {
	x := w.x
	attr := childOfKind(*a, x.attribute)
	if attr == nil {
		return dst
	}
	path := attr.NamedChild(0)
	args := attr.ChildByFieldId(x.fArguments)
	if path == nil || args == nil || path.KindId() != x.ident {
		return dst
	}
	return w.cfgAttribute(string(w.e.src[path.StartByte():path.EndByte()]), args, "", dst)
}

// cfgAttribute appends to dst the elements of the attribute named name with
// the argument token tree args, each prefixed by prefix, the elements of
// the cfg_attrs it is applied under.
func (w *branchWalk) cfgAttribute(name string, args *ts.Node, prefix string, dst []string) []string {
	switch name {
	case "cfg":
		return append(dst, prefix+"cfg "+strconv.Quote(w.text(args.StartByte(), args.EndByte())))
	case "cfg_attr":
	default:
		return dst
	}
	x := w.x
	// The arguments' children are `(`, the parts separated by `,` tokens,
	// and `)`: the first part is the predicate, each later one an attribute
	// it applies, a path followed by its own token tree.
	n := args.ChildCount()
	if n < 2 {
		return dst
	}
	part, first := 0, uint(1)
	for i := uint(1); i < n; i++ {
		c := args.Child(i)
		if c == nil || (i < n-1 && (c.IsNamed() || c.KindId() != x.comma)) {
			continue
		}
		// c ends the part of children [first, i).
		if i > first {
			if part == 0 {
				p := args.Child(first)
				prefix += "cfg_attr " + strconv.Quote(w.text(p.StartByte(), args.Child(i-1).EndByte())) + " "
			} else if id, tree := args.Child(first), args.Child(first+1); i-first == 2 && id.KindId() == x.ident && tree.IsNamed() {
				dst = w.cfgAttribute(string(w.e.src[id.StartByte():id.EndByte()]), tree, prefix, dst)
			}
		}
		part++
		first = i + 1
	}
	return dst
}

// armCfg is link with the elements of the outer `#[cfg]` and `#[cfg_attr]`
// attributes of the Rust match arm m pushed, in source order: an arm's
// attributes are its own children, not siblings before it.
func (w *branchWalk) armCfg(m *ts.Node, link *branchLink) *branchLink {
	var elems []string
	for i := range m.NamedChildCount() {
		if c := m.NamedChild(i); c != nil && c.KindId() == w.x.attributeItem {
			elems = w.cfgElements(c, elems)
		}
	}
	for _, e := range elems {
		link = link.push(e)
	}
	return link
}

// text is the source between start and end with its whitespace collapsed.
func (w *branchWalk) text(start, end uint) string {
	if end < start {
		return ""
	}
	return strings.Join(strings.Fields(string(w.e.src[start:end])), " ")
}
