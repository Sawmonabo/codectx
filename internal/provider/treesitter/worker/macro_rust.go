package worker

import (
	"strconv"
	"sync"

	ts "github.com/tree-sitter/go-tree-sitter"
)

// Rust items inside an item-position macro invocation.
//
// The grammar parses a macro's body as a flat token tree, so the items a
// `cfg_rt! { pub fn spawn() {} }` or a `cfg_if! { if #[cfg(unix)] { ... }
// else { ... } }` writes are no declarations of the file. A macro invocation
// in item position (a child of the file or of a declaration list) with a
// brace body has that body re-parsed by the same grammar, restricted to the
// body's bytes with the parser's included ranges, so every node of the
// re-parse keeps the file's own byte offsets and positions and no offset is
// remapped. The re-parse is kept only when it is clean: no error or missing
// node, and every top-level node an item, an attribute or a comment
// (rsItemKinds). When the whole body is not, each brace group directly
// inside it whose header (its tokens since the previous group) holds no item
// keyword is tried the same way, one level deep, which recovers the arms of
// a `cfg_if!`-style body and never a nested body of an item (the `{ const A
// = 1; }` of `bitflags! { struct F: u32 { const A = 1; } }`). A
// `macro_rules!` definition is never re-parsed: it is no invocation.
//
// A kept re-parse is one more tree of the file: the structural query runs
// over it as over the file's own, so the items it holds are declarations,
// its calls and type references references, and an item-position invocation
// inside it is re-parsed in turn. Its nodes then replace the body's tokens
// in the file's flat array (spliceMacros), so its functions are lowered and
// linked as the enclosing scope's own: a method written in a macro inside an
// impl is the impl's. Each of its declarations sits in a branch
// element `macro "<path>"`, or `macro "<path>" "<header>"` for a group,
// spelled as written, so two definitions of one name in two invocations
// (`cfg_rt!` and `cfg_not_rt!`) or two arms are two entities; and it is
// marked as the macro's input (wire.Decl.MacroInput), since a macro may
// transcribe its input any number of times, renamed or not at all (the Rust
// Reference's "Macros By Example").

// rsMacroSyntax is the Rust grammar's ids a re-parse is decided by,
// resolved once.
type rsMacroSyntax struct {
	sourceFile, declarationList, tokenTree, macroInvocation uint16
	open, close                                             uint16
	fMacro                                                  uint16
	// items reports, by kind id, a node a clean re-parse may hold at its
	// top level.
	items []bool
	// keywords reports, by kind id, a token that makes a brace group's
	// header an item's head rather than an arm's.
	keywords []bool
}

var (
	rsMacroOnce   sync.Once
	rsMacroTables *rsMacroSyntax
)

// rsItemKinds are the kinds a clean re-parse holds at its top level: the
// Rust Reference's items (modules, extern crates, use declarations,
// functions, type aliases, structs, enumerations, unions, constants,
// statics, traits, implementations, extern blocks, macro invocations and
// macro_rules definitions) and associated items (a function signature, an
// associated type), the attributes on them and comments.
var rsItemKinds = []string{"mod_item", "extern_crate_declaration", "use_declaration", "function_item",
	"function_signature_item", "type_item", "associated_type", "struct_item", "enum_item", "union_item",
	"const_item", "static_item", "trait_item", "impl_item", "foreign_mod_item", "macro_invocation",
	"macro_definition", "attribute_item", "inner_attribute_item", "line_comment", "block_comment",
	"empty_statement"}

// rsItemKeywords are the tokens that start an item or a statement: a brace
// group after one of them is that item's body, never an arm of the macro.
var rsItemKeywords = []string{"fn", "struct", "enum", "union", "trait", "impl", "mod", "type", "const", "static",
	"let"}

func rsMacroSyntaxOf() *rsMacroSyntax {
	rsMacroOnce.Do(func() {
		const language = "rust"
		tl := mustGrammar(language)
		kind := func(name string) uint16 { return mustKind(tl, language, name, true) }
		s := &rsMacroSyntax{
			sourceFile: kind("source_file"), declarationList: kind("declaration_list"), tokenTree: kind("token_tree"),
			macroInvocation: kind("macro_invocation"),
			open:            mustKind(tl, language, "{", false), close: mustKind(tl, language, "}", false),
			fMacro: tl.FieldIdForName("macro"),
			items:  make([]bool, tl.NodeKindCount()), keywords: make([]bool, tl.NodeKindCount()),
		}
		if s.fMacro == 0 {
			panic("worker: the rust grammar has no field macro")
		}
		for _, k := range rsItemKinds {
			s.items[kind(k)] = true
		}
		for _, k := range rsItemKeywords {
			s.keywords[mustKind(tl, language, k, false)] = true
		}
		rsMacroTables = s
	})
	return rsMacroTables
}

// macroBody is one kept re-parse: its tree and root, the branch element
// its declarations sit in, and the token tree it re-parses.
type macroBody struct {
	tree    *ts.Tree
	root    ts.Node
	element string
	// tt is the range of the token tree re-parsed, the invocation's body or
	// one of its groups, whose tokens the re-parse replaces in the file's
	// flat array (spliceMacros).
	tt span
	// path is the invocation's macro path as written.
	path string
}

// macroBodies is a file's re-parsed macro bodies. pending are the
// item-position invocations the query met, in every tree, that are not yet
// re-parsed; at holds each re-parsed invocation's kept bodies, by its range.
type macroBodies struct {
	parser  *ts.Parser
	pending []ts.Node
	at      map[span][]macroBody
	trees   []*ts.Tree
}

// newMacroBodies is the macro body state of a file of language parsed by
// parser, or nil for a language whose macros are not re-parsed.
func newMacroBodies(language string, parser *ts.Parser) *macroBodies {
	if language != "rust" || parser == nil {
		return nil
	}
	return &macroBodies{parser: parser}
}

// close frees every kept tree. It may be called on nil.
func (m *macroBodies) close() {
	if m == nil {
		return
	}
	for _, t := range m.trees {
		t.Close()
	}
	m.trees, m.at, m.pending = nil, nil, nil
}

// macroCandidate records the macro invocation n, which the query matched,
// when it is in item position with a brace body.
func (e *extraction) macroCandidate(n ts.Node) {
	m := e.macros
	if m == nil {
		return
	}
	s := rsMacroSyntaxOf()
	p := n.Parent()
	if p == nil || (p.KindId() != s.sourceFile && p.KindId() != s.declarationList) {
		return
	}
	if bodyOf(n, s) == nil {
		return
	}
	m.pending = append(m.pending, n)
}

// bodyOf is the brace token tree of the macro invocation n, or nil.
func bodyOf(n ts.Node, s *rsMacroSyntax) *ts.Node {
	k := n.ChildCount()
	if k == 0 {
		return nil
	}
	t := n.Child(k - 1)
	if t == nil || t.KindId() != s.tokenTree || !braced(t, s) {
		return nil
	}
	return t
}

// braced reports a token tree written `{ ... }`.
func braced(t *ts.Node, s *rsMacroSyntax) bool {
	k := t.ChildCount()
	if k < 2 {
		return false
	}
	o, c := t.Child(0), t.Child(k-1)
	return o != nil && c != nil && o.KindId() == s.open && c.KindId() == s.close
}

// expandMacros re-parses every pending invocation and runs the query q over
// each kept body with cursor, until no invocation is pending: an
// item-position invocation inside a kept body is matched by that run and
// re-parsed in turn.
func (e *extraction) expandMacros(q *ts.Query, cursor *ts.QueryCursor, names []string, caps map[string][]ts.Node) {
	m := e.macros
	if m == nil {
		return
	}
	s := rsMacroSyntaxOf()
	for len(m.pending) > 0 {
		n := m.pending[0]
		m.pending = m.pending[1:]
		bodies := e.reparse(n, s)
		if len(bodies) == 0 {
			continue
		}
		if m.at == nil {
			m.at = map[span][]macroBody{}
		}
		m.at[span{n.StartByte(), n.EndByte()}] = bodies
		for i := range bodies {
			matches := cursor.Matches(q, &bodies[i].root, e.src)
			for match := matches.Next(); match != nil; match = matches.Next() {
				clear(caps)
				for _, c := range match.Captures {
					caps[names[c.Index]] = append(caps[names[c.Index]], c.Node)
				}
				e.dispatch(caps)
			}
			if cursor.DidExceedMatchLimit() {
				e.truncated = true
			}
		}
	}
}

// reparse is the kept bodies of the item-position invocation n: its whole
// brace body when that re-parses clean, else each clean brace group
// directly inside it whose header holds no item keyword.
func (e *extraction) reparse(n ts.Node, s *rsMacroSyntax) []macroBody {
	body := bodyOf(n, s)
	path := ""
	if p := n.ChildByFieldId(s.fMacro); p != nil {
		path = collapseSpace(p.Utf8Text(e.src))
	}
	head := "macro " + strconv.Quote(path)
	if b, ok := e.parseBody(body, s, head); ok {
		b.path = path
		return []macroBody{b}
	}
	var out []macroBody
	k := body.ChildCount()
	from := body.Child(0).EndByte()
	item := false
	for i := uint(1); i+1 < k; i++ {
		c := body.Child(i)
		if c == nil || c.IsExtra() {
			continue
		}
		if c.KindId() != s.tokenTree || !braced(c, s) {
			if !c.IsNamed() && s.keywords[c.KindId()] {
				item = true
			}
			continue
		}
		if !item {
			header := collapseSpace(string(e.src[from:c.StartByte()]))
			if b, ok := e.parseBody(c, s, head+" "+strconv.Quote(header)); ok {
				b.path = path
				out = append(out, b)
			}
		}
		from, item = c.EndByte(), false
	}
	return out
}

// parseBody re-parses the interior of the brace token tree t, between its
// braces, and answers it with the branch element element when it is clean.
// A tree that is not kept is closed here.
func (e *extraction) parseBody(t *ts.Node, s *rsMacroSyntax, element string) (macroBody, bool) {
	m := e.macros
	o, c := t.Child(0), t.Child(t.ChildCount()-1)
	r := ts.Range{StartByte: o.EndByte(), EndByte: c.StartByte(), StartPoint: o.EndPosition(), EndPoint: c.StartPosition()}
	if r.EndByte <= r.StartByte {
		return macroBody{}, false
	}
	if err := m.parser.SetIncludedRanges([]ts.Range{r}); err != nil {
		return macroBody{}, false
	}
	tree := Parse(m.parser, e.src, nil)
	// The parser is the file's for the language: it parses whole sources
	// again for the next file.
	if err := m.parser.SetIncludedRanges(nil); err != nil {
		panic("worker: the parser refused the whole-source range: " + err.Error())
	}
	if tree == nil {
		m.parser.Reset()
		return macroBody{}, false
	}
	root := tree.RootNode()
	if !cleanItems(root, s) {
		tree.Close()
		return macroBody{}, false
	}
	m.trees = append(m.trees, tree)
	return macroBody{tree: tree, root: *root, element: element, tt: span{t.StartByte(), t.EndByte()}}, true
}

// cleanItems reports a re-parse with no error or missing node whose every
// top-level node is an item, an attribute or a comment, and at least one.
func cleanItems(root *ts.Node, s *rsMacroSyntax) bool {
	if root.HasError() || root.NamedChildCount() == 0 {
		return false
	}
	for i := range root.NamedChildCount() {
		c := root.NamedChild(i)
		if c == nil || int(c.KindId()) >= len(s.items) || !s.items[c.KindId()] {
			return false
		}
	}
	return true
}

// macroBodiesOf is the kept bodies of the macro invocation n, or nil.
func (e *extraction) macroBodiesOf(n *ts.Node) []macroBody {
	if e.macros == nil || e.macros.at == nil {
		return nil
	}
	return e.macros.at[span{n.StartByte(), n.EndByte()}]
}

// macroInput is the path of the innermost macro whose input the
// declaration at path is written in: the macro of the nearest macro
// element on path, or empty.
func macroInput(path *branchLink) string {
	for l := path; l != nil; l = l.up {
		if l.macro != "" {
			return l.macro
		}
	}
	return ""
}
