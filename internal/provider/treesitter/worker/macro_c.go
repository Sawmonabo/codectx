package worker

import (
	"bytes"
	"slices"
	"strings"

	ts "github.com/tree-sitter/go-tree-sitter"
)

// C and C++ declarations written with a macro among their specifiers
// (`SQLITE_API int f(void);`, `XMLPUBFUN xmlNodePtr XMLCALL xmlNewNode(...)`,
// `void f(void) G_GNUC_CONST;`). The grammar has no preprocessor, so such a
// declaration misparses: the macro is taken for the type, the real type
// lands in an ERROR, or the declaration is split or lost. A misparse is
// proven by the tree, never guessed from a name: an ERROR or MISSING among a
// file-scope declaration's specifiers or declarator, a file-scope ERROR or
// expression statement (illegal at file scope), or a typedef name combined
// with a `signed`/`unsigned`/`short`/`long` specifier (C17 6.7.2p2). The
// identifiers before the declarator's name are then classified by the
// language's own rule: a typedef name never combines with another type
// specifier, so when a keyword type or a struct, union or enum specifier is
// present every other bare identifier is a macro; otherwise exactly one is
// the type, and an identifier the file defines as an object-like macro
// before the declaration is not. Function-like macro calls before the
// specifiers and identifiers after the parameter list are attribute macros.
// The file is parsed again with those tokens excluded (included ranges keep
// every offset the file's), and the second parse is kept when it has no
// more error bytes than the first. When the rule leaves more than one
// candidate type, every candidate but the last is excluded so the name
// parses exactly, and the declaration says its return type is that set.

// cTok is one token of a C declaration as written: an identifier or
// keyword (word), a single punctuator, or a literal.
type cTok struct {
	start, end int
	word       bool
}

// cLex splits src[start:end] into tokens, skipping whitespace, comments and
// line continuations.
func cLex(src []byte, start, end int) []cTok {
	var out []cTok
	for i := start; i < end; {
		c := src[i]
		switch {
		case c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == '\f' || c == '\v' || c == '\\':
			i++
		case c == '/' && i+1 < end && src[i+1] == '/':
			for i < end && src[i] != '\n' {
				i++
			}
		case c == '/' && i+1 < end && src[i+1] == '*':
			j := bytes.Index(src[i+2:end], []byte("*/"))
			if j < 0 {
				return out
			}
			i += j + 4
		case c == '"' || c == '\'':
			j := i + 1
			for j < end && src[j] != c {
				if src[j] == '\\' {
					j++
				}
				j++
			}
			out = append(out, cTok{start: i, end: min(j+1, end)})
			i = j + 1
		case cIdentStart(c):
			j := i + 1
			for j < end && (cIdentStart(src[j]) || src[j] >= '0' && src[j] <= '9') {
				j++
			}
			out = append(out, cTok{start: i, end: j, word: true})
			i = j
		case c >= '0' && c <= '9':
			j := i + 1
			for j < end && (cIdentStart(src[j]) || src[j] >= '0' && src[j] <= '9' || src[j] == '.') {
				j++
			}
			out = append(out, cTok{start: i, end: j})
			i = j
		case c == ':' && i+1 < end && src[i+1] == ':':
			out = append(out, cTok{start: i, end: i + 2})
			i += 2
		default:
			out = append(out, cTok{start: i, end: i + 1})
			i++
		}
	}
	return out
}

func cIdentStart(c byte) bool {
	return c == '_' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= 0x80
}

// cPrimitive is the grammar's primitive_type token set (tree-sitter-c
// grammar.js, primitive_type), which a type_identifier never is.
var cPrimitive = map[string]bool{"bool": true, "char": true, "int": true, "float": true, "double": true, "void": true,
	"size_t": true, "ssize_t": true, "ptrdiff_t": true, "intptr_t": true, "uintptr_t": true, "charptr_t": true,
	"nullptr_t": true, "max_align_t": true,
	"int8_t": true, "int16_t": true, "int32_t": true, "int64_t": true, "uint8_t": true, "uint16_t": true, "uint32_t": true,
	"uint64_t": true, "char8_t": true, "char16_t": true, "char32_t": true}

// cTypeWords are the keywords that are, or begin, a type specifier other
// than a typedef name: the sized specifiers and the tag keywords.
var cTypeWords = map[string]bool{"signed": true, "unsigned": true, "short": true, "long": true, "_Complex": true,
	"_Imaginary": true, "struct": true, "union": true, "enum": true, "class": true, "typename": true}

// cTagWords are followed by their tag name, which is no macro.
var cTagWords = map[string]bool{"struct": true, "union": true, "enum": true, "class": true}

// cAttributeWords take a parenthesized argument the grammar reads itself.
var cAttributeWords = map[string]bool{"__attribute__": true, "__attribute": true, "__declspec": true, "alignas": true,
	"_Alignas": true, "__asm__": true, "__asm": true, "asm": true, "noexcept": true, "throw": true, "decltype": true}

// cDecision is what cDecide reads from the words written before a
// declarator's name: the macro tokens (an identifier, or a function-like
// call from its name through its closing parenthesis) and, when the rule
// leaves several identifiers that may be the type, those identifiers.
type cDecision struct {
	macros     []cTok // spans to exclude; a call's span covers its arguments
	names      []string
	candidates []string
}

// cDecide classifies toks, the tokens before the declarator's name, under
// the rule above. keyword reports a word the grammar spells as a keyword;
// defined reports an identifier the file defines as an object-like macro
// before the declaration. ok is false when the words hold a construct the
// rule does not read (a C++ template or qualified name), which then changes
// nothing.
func cDecide(src []byte, toks []cTok, keyword, defined func(string) bool) (d cDecision, ok bool) {
	var bare, calls []cTok
	hasType := false
	for i := 0; i < len(toks); i++ {
		t := toks[i]
		text := string(src[t.start:t.end])
		if !t.word {
			switch text {
			case "*", "&", "&&", "(", ")", ",":
				continue
			}
			return d, false
		}
		next := i+1 < len(toks) && string(src[toks[i+1].start:toks[i+1].end]) == "("
		switch {
		case cAttributeWords[text]:
			if next {
				i = cClose(src, toks, i+1)
			}
		case cPrimitive[text] || cTypeWords[text]:
			hasType = true
			if cTagWords[text] && i+1 < len(toks) && toks[i+1].word {
				i++
			}
		case keyword(text):
		case next:
			end := cClose(src, toks, i+1)
			calls = append(calls, cTok{start: t.start, end: toks[min(end, len(toks)-1)].end, word: true})
			i = end
		default:
			bare = append(bare, t)
		}
	}
	add := func(ts ...cTok) {
		for _, t := range ts {
			d.macros = append(d.macros, t)
			name := src[t.start:t.end]
			if i := bytes.IndexByte(name, '('); i >= 0 {
				name = bytes.TrimSpace(name[:i])
			}
			d.names = append(d.names, string(name))
		}
	}
	switch {
	case hasType:
		add(bare...)
		add(calls...)
	case len(bare) == 0:
		// A function-like macro is the only specifier: the grammar reads
		// the last as a macro type specifier (`PyAPI_FUNC(int)`).
		if len(calls) > 1 {
			add(calls[:len(calls)-1]...)
		}
	case len(bare) == 1:
		add(calls...)
	default:
		var rest []cTok
		for _, t := range bare {
			if defined(string(src[t.start:t.end])) {
				add(t)
			} else {
				rest = append(rest, t)
			}
		}
		add(calls...)
		if len(rest) > 1 {
			for _, t := range rest {
				d.candidates = append(d.candidates, string(src[t.start:t.end]))
			}
			add(rest[:len(rest)-1]...)
		}
	}
	return d, true
}

// cClose returns the index of the parenthesis closing the one at toks[open],
// or the last index when it does not close.
func cClose(src []byte, toks []cTok, open int) int {
	depth := 0
	for i := open; i < len(toks); i++ {
		switch string(src[toks[i].start:toks[i].end]) {
		case "(":
			depth++
		case ")":
			if depth--; depth == 0 {
				return i
			}
		}
	}
	return len(toks) - 1
}

// cTrailing reads the tokens after a parameter list's closing parenthesis up
// to the declaration's end: each identifier the grammar does not spell as a
// keyword, with its arguments when it is called, is an attribute macro
// (`G_GNUC_CONST`, `G_GNUC_PRINTF(1, 2)`).
func cTrailing(src []byte, toks []cTok, keyword func(string) bool) []cTok {
	var out []cTok
	for i := 0; i < len(toks); i++ {
		t := toks[i]
		text := string(src[t.start:t.end])
		if !t.word {
			if text == "=" {
				return out
			}
			continue
		}
		next := i+1 < len(toks) && string(src[toks[i+1].start:toks[i+1].end]) == "("
		switch {
		case cAttributeWords[text] || keyword(text):
			if next {
				i = cClose(src, toks, i+1)
			}
		case next:
			end := cClose(src, toks, i+1)
			out = append(out, cTok{start: t.start, end: toks[end].end, word: true})
			i = end
		default:
			out = append(out, t)
		}
	}
	return out
}

// cMacroPass finds the macro tokens of every misparsed file-scope
// declaration of the tree under root and returns them in source order; none
// when the tree has no error.
func cMacroPass(root *ts.Node, src []byte, tl *ts.Language) []cTok {
	if !root.HasError() {
		return nil
	}
	p := cMacroScan{src: src, tl: tl, defines: cMacroDefs{}}
	p.container(root)
	slices.SortFunc(p.out, func(a, b cTok) int { return a.start - b.start })
	return slices.CompactFunc(p.out, func(a, b cTok) bool { return a.start == b.start })
}

type cMacroScan struct {
	src []byte
	tl  *ts.Language
	// defines holds the object-like macro directives the walk has passed.
	defines cMacroDefs
	out     []cTok
}

func (p *cMacroScan) keyword(s string) bool { return p.tl.IdForNodeKind(s, false) != 0 }

// container walks the children of a node file-scope declarations sit in.
func (p *cMacroScan) container(n *ts.Node) {
	count := n.ChildCount()
	for i := uint(0); i < count; i++ {
		c := n.Child(i)
		switch c.Kind() {
		case "preproc_def", "preproc_call":
			p.define(c)
			continue
		case "preproc_if", "preproc_ifdef", "preproc_else", "preproc_elif", "preproc_elifdef", "declaration_list",
			"template_declaration":
			p.container(c)
			continue
		case "linkage_specification", "namespace_definition":
			if body := c.ChildByFieldName("body"); body != nil {
				if body.Kind() == "declaration_list" {
					p.container(body)
				} else {
					p.group(n, i, i)
				}
			}
			continue
		}
		if !p.suspect(c) {
			continue
		}
		j := i
		for j+1 < count && !p.ends(n.Child(j)) && !cDirective(n.Child(j+1).Kind()) {
			j++
		}
		p.group(n, i, j)
		i = j
	}
}

func cDirective(kind string) bool {
	return len(kind) > 7 && kind[:7] == "preproc"
}

// suspect reports a child of a file-scope container whose shape proves a
// misparse among a declaration's specifiers or declarator.
func (p *cMacroScan) suspect(c *ts.Node) bool {
	switch c.Kind() {
	case "ERROR", "expression_statement":
		return true
	case "declaration", "function_definition":
	default:
		return false
	}
	count := c.ChildCount()
	for i := uint(0); i < count; i++ {
		k := c.Child(i)
		if k.IsError() || k.IsMissing() {
			return true
		}
		if k.Kind() == "sized_type_specifier" && cHasKind(k, "type_identifier") {
			return true
		}
		if f := c.FieldNameForChild(uint32(i)); f == "declarator" && cDeclaratorError(k) {
			return true
		}
	}
	return false
}

// cDeclaratorError reports an ERROR or MISSING directly under a declarator
// on the way to its name (`gboolean g_foo (void)` read as a declarator named
// gboolean with g_foo in an ERROR; in C++ `M T f(void)` read as `T::f` with
// the `::` missing).
func cDeclaratorError(d *ts.Node) bool {
	for d != nil {
		count := d.ChildCount()
		var inner *ts.Node
		for i := uint(0); i < count; i++ {
			k := d.Child(i)
			if k.IsError() || k.IsMissing() {
				return true
			}
			if d.FieldNameForChild(uint32(i)) == "declarator" {
				inner = k
			}
		}
		d = inner
	}
	return false
}

func cHasKind(n *ts.Node, kind string) bool {
	count := n.ChildCount()
	for i := uint(0); i < count; i++ {
		if n.Child(i).Kind() == kind {
			return true
		}
	}
	return false
}

// ends reports a node that ends a declaration: a function definition, or a
// declaration or statement whose last token is a `;` actually written.
func (p *cMacroScan) ends(c *ts.Node) bool {
	if c.Kind() == "function_definition" {
		return true
	}
	if count := c.ChildCount(); count > 0 {
		last := c.Child(count - 1)
		return last.Kind() == ";" && !last.IsMissing()
	}
	return false
}

// group reads the declaration the children i..j of n spell together.
func (p *cMacroScan) group(n *ts.Node, i, j uint) {
	first, last := n.Child(i), n.Child(j)
	start, end := int(first.StartByte()), int(last.EndByte())
	body := -1
	params := -1
	for k := i; k <= j; k++ {
		c := n.Child(k)
		if c.Kind() == "function_definition" {
			if b := c.ChildByFieldName("body"); b != nil {
				body = int(b.StartByte())
			}
		}
		if params < 0 {
			params = cParams(c)
		}
	}
	if body >= 0 {
		end = body
	}
	toks := cLex(p.src, start, end)
	// The parameter list: the tree's, else the first `(` after an
	// identifier that is not the first word and whose `)` ends the
	// declaration (`f(void);`, `f(void) {`).
	open := -1
	for k, t := range toks {
		if params >= 0 && t.start == params || params < 0 && k > 1 && p.text(t) == "(" && toks[k-1].word &&
			p.closesDeclaration(toks, k) {
			open = k
			break
		}
	}
	if open < 1 || !toks[open-1].word {
		return
	}
	defined := func(s string) bool { return p.defines.defined(s, uint(start)) }
	d, ok := cDecide(p.src, toks[:open-1], p.keyword, defined)
	if !ok {
		return
	}
	p.out = append(p.out, d.macros...)
	closeAt := cClose(p.src, toks, open)
	if closeAt+1 < len(toks) {
		p.out = append(p.out, cTrailing(p.src, toks[closeAt+1:], p.keyword)...)
	}
}

func (p *cMacroScan) text(t cTok) string { return string(p.src[t.start:t.end]) }

// closesDeclaration reports a `(` at toks[k] whose `)` is followed by `;`,
// `{` or the end.
func (p *cMacroScan) closesDeclaration(toks []cTok, k int) bool {
	c := cClose(p.src, toks, k)
	return c+1 >= len(toks) || p.text(toks[c+1]) == ";" || p.text(toks[c+1]) == "{"
}

// cParams is the start of the first parameter list of a function declarator
// in c outside every body, or -1.
func cParams(c *ts.Node) int {
	switch c.Kind() {
	case "compound_statement", "field_declaration_list", "enumerator_list":
		return -1
	case "function_declarator":
		if ps := c.ChildByFieldName("parameters"); ps != nil {
			return int(ps.StartByte())
		}
	}
	count := c.ChildCount()
	for i := uint(0); i < count; i++ {
		if at := cParams(c.Child(i)); at >= 0 {
			return at
		}
	}
	return -1
}

// cIncluded is the complement of the sorted, disjoint excluded spans over
// src, as parser ranges.
func cIncluded(src []byte, excluded []cTok) []ts.Range {
	var out []ts.Range
	line, lineStart := uint(0), 0
	scanned := 0
	point := func(off int) ts.Point {
		for ; scanned < off; scanned++ {
			if src[scanned] == '\n' {
				line++
				lineStart = scanned + 1
			}
		}
		return ts.Point{Row: line, Column: uint(off - lineStart)}
	}
	at := 0
	for _, x := range excluded {
		if x.start > at {
			out = append(out, ts.Range{StartByte: uint(at), EndByte: uint(x.start), StartPoint: point(at), EndPoint: point(x.start)})
		}
		at = max(at, x.end)
	}
	out = append(out, ts.Range{StartByte: uint(at), EndByte: uint(len(src)), StartPoint: point(at), EndPoint: point(len(src))})
	return out
}

// cUnmacro is kept parsed again with the macro tokens of its misparsed
// declarations excluded, when it is a C or C++ parse with such a
// declaration and the second parse has no more error bytes; otherwise kept
// itself. The tree not kept is closed.
func (w *state) cUnmacro(kept parsed, src []byte) parsed {
	if kept.l.Name != "c" && kept.l.Name != "cpp" {
		return kept
	}
	tl := kept.g.tsLanguage()
	root := kept.tree.RootNode()
	excluded := cMacroPass(root, src, tl)
	if len(excluded) == 0 {
		return kept
	}
	parser, _, err := w.tools(kept.l, kept.g)
	if err != nil {
		return kept
	}
	if err := parser.SetIncludedRanges(cIncluded(src, excluded)); err != nil {
		return kept
	}
	tree := Parse(parser, src, nil)
	_ = parser.SetIncludedRanges(nil)
	if tree == nil {
		parser.Reset()
		return kept
	}
	again := parsed{tree: tree, l: kept.l, g: kept.g, query: kept.query}
	before, fail := kept.parseErrors()
	after, fail2 := again.parseErrors()
	if fail != nil || fail2 != nil || after.Bytes > before.Bytes {
		tree.Close()
		return kept
	}
	kept.tree.Close()
	return again
}

// cSpecifierMacros reads, from a declaration of the kept tree, the macros
// its specifiers were written with: the identifiers in the source gaps
// before it (after prev, the end of what precedes it) and between its
// children (spans), which the kept parse excluded, less the candidates.
// candidates are the identifiers that may be its type when the rule leaves
// several: then the return type is that set, not the one the parse kept. typeKind is the kind
// of the declaration's `type` child and typeName its text.
func cSpecifierMacros(src []byte, prev, start uint, spans [][2]uint, typeKind, typeName string,
	keyword, defined func(string) bool) (macros, candidates []string) {
	var gaps []cTok
	at := prev
	for _, s := range append([][2]uint{{start, start}}, spans...) {
		if s[0] > at {
			gaps = append(gaps, cLex(src, int(at), int(s[0]))...)
		}
		at = max(at, s[1])
	}
	var words []cTok
	for i := 0; i < len(gaps); i++ {
		t := gaps[i]
		if !t.word {
			continue
		}
		text := string(src[t.start:t.end])
		if keyword(text) || cAttributeWords[text] {
			continue
		}
		macros = append(macros, text)
		if i+1 < len(gaps) && string(src[gaps[i+1].start:gaps[i+1].end]) == "(" {
			i = cClose(src, gaps, i+1)
			continue
		}
		words = append(words, t)
	}
	if typeKind != "type_identifier" || len(words) == 0 {
		return macros, nil
	}
	rest := []string{}
	for _, t := range words {
		if s := string(src[t.start:t.end]); !defined(s) {
			rest = append(rest, s)
		}
	}
	if !defined(typeName) {
		rest = append(rest, typeName)
	}
	if len(rest) < 2 {
		return macros, nil
	}
	// The candidates are the type's set, not macros.
	proven := macros[:0:0]
	for _, m := range macros {
		if !slices.Contains(rest, m) {
			proven = append(proven, m)
		}
	}
	return proven, rest
}

// cDeclMacros is cSpecifierMacros for a declaration the extraction
// captured: the specifier macros of a C or C++ file-scope declaration or
// function definition, and its candidate return types when the rule leaves
// several. Every other declaration has none.
func cDeclMacros(e *extraction, d *decl) (macros, candidates []string) {
	if e.l.Name != "c" && e.l.Name != "cpp" || d.declarator == nil {
		return nil, nil
	}
	n := d.node
	if k := n.Kind(); k != "declaration" && k != "function_definition" {
		return nil, nil
	}
	prev := n.StartByte()
	if cMayFollowMacro(e.src, prev) {
		switch p, parent := n.PrevSibling(), n.Parent(); {
		case p != nil:
			prev = p.EndByte()
		case parent != nil && parent.Parent() != nil:
			prev = parent.StartByte()
		default:
			// The root begins at the first byte the parse included, so a
			// macro the parse excluded before it lies from the file's start.
			prev = 0
		}
	}
	var spans [][2]uint
	typeKind, typeName := "", ""
	count := n.ChildCount()
	for i := uint(0); i < count; i++ {
		c := n.Child(i)
		if n.FieldNameForChild(uint32(i)) == "body" {
			break
		}
		spans = append(spans, [2]uint{c.StartByte(), c.EndByte()})
		if n.FieldNameForChild(uint32(i)) == "type" {
			typeKind, typeName = c.Kind(), c.Utf8Text(e.src)
		}
	}
	if e.cDefines == nil {
		e.cDefines = cMacroDefs{}
		p := cMacroScan{src: e.src, defines: e.cDefines}
		root := &n
		for root.Parent() != nil {
			root = root.Parent()
		}
		p.collectDefines(root)
	}
	tl := e.g.tsLanguage()
	keyword := func(s string) bool { return tl.IdForNodeKind(s, false) != 0 }
	defined := func(s string) bool { return e.cDefines.defined(s, n.StartByte()) }
	return cSpecifierMacros(e.src, prev, n.StartByte(), spans, typeKind, typeName, keyword, defined)
}

// collectDefines records every object-like macro directive under n.
func (p *cMacroScan) collectDefines(n *ts.Node) {
	count := n.ChildCount()
	for i := uint(0); i < count; i++ {
		c := n.Child(i)
		switch c.Kind() {
		case "preproc_def", "preproc_call":
			p.define(c)
		case "preproc_if", "preproc_ifdef", "preproc_else", "preproc_elif", "preproc_elifdef", "declaration_list",
			"linkage_specification", "namespace_definition", "template_declaration":
			p.collectDefines(c)
		}
	}
}

// cMayFollowMacro reports whether the source before start, past
// whitespace, ends in an identifier or a `)`: only then can an excluded
// macro token precede a declaration starting there, so only then is its
// previous sibling, a walk over the parent's children, looked up.
func cMayFollowMacro(src []byte, start uint) bool {
	i := int(start) - 1
	for i >= 0 && (src[i] == ' ' || src[i] == '\t' || src[i] == '\n' || src[i] == '\r') {
		i--
	}
	return i >= 0 && (cIdentStart(src[i]) || src[i] >= '0' && src[i] <= '9' || src[i] == ')')
}

// cMacroDef is one directive on an object-like macro name: its `#define`
// or, when undef, its `#undef`, at the directive's start.
type cMacroDef struct {
	at    uint
	undef bool
}

// cMacroDefs are a file's object-like macro directives by name, in source
// order.
type cMacroDefs map[string][]cMacroDef

// defined reports whether name is an object-like macro at offset before: the
// last directive on it before that offset is a `#define`.
func (m cMacroDefs) defined(name string, before uint) bool {
	on := false
	for _, d := range m[name] {
		if d.at >= before {
			break
		}
		on = !d.undef
	}
	return on
}

// define records the directive c: an object-like `#define`, or an `#undef`
// (a preproc_call whose directive, spelled with any spaces after its `#`
// (ISO C 6.10p2), is undef).
func (p *cMacroScan) define(c *ts.Node) {
	if c.Kind() == "preproc_def" {
		if name := c.ChildByFieldName("name"); name != nil {
			n := name.Utf8Text(p.src)
			p.defines[n] = append(p.defines[n], cMacroDef{at: c.StartByte()})
		}
		return
	}
	d, a := c.ChildByFieldName("directive"), c.ChildByFieldName("argument")
	if d == nil || a == nil || strings.Join(strings.Fields(strings.TrimPrefix(d.Utf8Text(p.src), "#")), "") != "undef" {
		return
	}
	n := strings.TrimSpace(cUncommented(a.Utf8Text(p.src)))
	p.defines[n] = append(p.defines[n], cMacroDef{at: c.StartByte(), undef: true})
}
