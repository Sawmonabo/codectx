package worker

import (
	"slices"
	"strings"

	ts "github.com/tree-sitter/go-tree-sitter"

	"github.com/Sawmonabo/codectx/internal/provider/treesitter/lang"
	"github.com/Sawmonabo/codectx/internal/provider/treesitter/wire"
)

// parseChunkBytes bounds each copy the binding makes of the source while the
// parser reads it (readUTF8 copies whatever the callback returns into a C
// string kept until the parse ends).
const parseChunkBytes = 64 << 10

// atRecordBound reports whether a per-file record set has reached the bound the
// request carries. A zero bound -- the default -- is unlimited, so the file
// yields what it yields; only a value the operator set can stop it, and a file
// it stops is reported truncated.
func atRecordBound(max uint64, have int) bool {
	return max != 0 && uint64(have) >= uint64(max)
}

type span struct{ start, end uint }

// decl is one captured declaration before it is numbered and emitted.
type decl struct {
	node       ts.Node
	body       *ts.Node
	declarator *ts.Node
	kind       string
	name       string
	nameStart  uint
	nameEnd    uint
	exported   bool
	test       bool
	prototype  bool
	macro      bool
	impl       string
	parentIdx  int
	ex         *extraction
}

// scopeNode is a non-declaration node that contributes to qualified names and
// turns the functions inside it into methods (a Rust impl block).
type scopeNode struct {
	span
	name string
}

type importRec struct {
	span
	path  string
	names []string
}

type refRec struct {
	span
	// nameSpan is the callee/type identifier token alone, inside span. The
	// parent builds the Section 11.3 callsite alias from it.
	nameSpan  span
	kind      string
	name      string
	qualifier *ts.Node
}

// extraction is the state of one parse.
type extraction struct {
	g            *grammar
	l            lang.Language
	path         string
	src          []byte
	decls        map[span]map[uint]*decl // by declaration range, then name start
	scopes       []scopeNode
	imports      map[span]*importRec
	refs         []refRec
	pkg          string
	exportRanges map[span]bool
	exportNames  map[string]bool
	truncated    bool
	// maxRecords is the request's MaxRecordsPerFile, the one per-file record
	// bound; 0 is unlimited.
	maxRecords uint64
	// declCount is how many declaration RECORDS this extraction will emit --
	// one per (range, name start) pair, which is what emit flattens e.decls
	// into and therefore what the parent counts as it reads the frames back.
	// len(e.decls) is the number of RANGES and is smaller whenever one range
	// declares several names (`var a, b = ...`), so bounding on it would let
	// the worker send more frames than the parent was told to accept and the
	// parent would fail a healthy unit.
	declCount int
}

// kindRank orders the kinds two patterns may assign to the same declaration
// (a `const f = () => {}` is both a constant and a function); the more
// specific wins.
var kindRank = map[string]int{"test": 7, "function": 6, "method": 6, "class": 5, "struct": 5, "interface": 5, "enum": 5,
	"namespace": 5, "module": 5, "constant": 3, "field": 2, "variable": 1}

var typeLike = set("class", "struct", "interface", "enum")

// run executes the language query over root and emits the facts.
func (e *extraction) run(q *ts.Query, root *ts.Node, emit *emitter) error {
	e.decls = map[span]map[uint]*decl{}
	e.imports = map[span]*importRec{}
	e.exportRanges = map[span]bool{}
	e.exportNames = map[string]bool{}
	names := q.CaptureNames()

	cursor := ts.NewQueryCursor()
	defer cursor.Close()
	matches := cursor.Matches(q, root, e.src)
	caps := map[string][]ts.Node{}
	for m := matches.Next(); m != nil; m = matches.Next() {
		clear(caps)
		for _, c := range m.Captures {
			n := names[c.Index]
			caps[n] = append(caps[n], c.Node)
		}
		e.dispatch(caps)
	}
	if cursor.DidExceedMatchLimit() {
		e.truncated = true
	}
	return e.emit(emit)
}

func (e *extraction) dispatch(caps map[string][]ts.Node) {
	for name, nodes := range caps {
		switch {
		case strings.HasPrefix(name, "def."):
			e.addDecl(name[len("def."):], nodes[0], caps)
			return
		case name == "import":
			e.addImport(nodes[0], caps)
			return
		case name == "call":
			e.addRef("call", nodes[0], caps["call.name"], caps["call.qualifier"])
			return
		case name == "ref.type":
			for _, n := range nodes {
				e.addRef("type", n, []ts.Node{n}, nil)
			}
			return
		case name == "scope":
			if len(caps["scope.name"]) > 0 {
				e.scopes = append(e.scopes, scopeNode{span{nodes[0].StartByte(), nodes[0].EndByte()}, caps["scope.name"][0].Utf8Text(e.src)})
			}
			return
		case name == "package":
			e.pkg = nodes[0].Utf8Text(e.src)
			return
		case name == "export":
			for _, n := range nodes {
				e.exportRanges[span{n.StartByte(), n.EndByte()}] = true
			}
			return
		case name == "export.name":
			for _, n := range nodes {
				e.exportNames[n.Utf8Text(e.src)] = true
			}
			return
		}
	}
}

func (e *extraction) addDecl(kind string, node ts.Node, caps map[string][]ts.Node) {
	d := &decl{node: node, kind: kind, parentIdx: -1, ex: e}
	if b := caps["body"]; len(b) > 0 {
		d.body = &b[0]
	}
	switch {
	case len(caps["name"]) > 0:
		n := caps["name"][0]
		d.name, d.nameStart, d.nameEnd = n.Utf8Text(e.src), n.StartByte(), n.EndByte()
	case len(caps["test.name"]) > 0:
		n := caps["test.name"][0]
		d.name, d.nameStart, d.nameEnd = strings.Trim(n.Utf8Text(e.src), "\"'`"), n.StartByte(), n.EndByte()
	case len(caps["declarator"]) > 0:
		d.declarator = &caps["declarator"][0]
		d.nameStart = d.declarator.StartByte()
	}
	if d.kind == "test" {
		d.test = true
	}
	key := span{node.StartByte(), node.EndByte()}
	byName := e.decls[key]
	if byName == nil {
		byName = map[uint]*decl{}
		e.decls[key] = byName
	}
	if prev, ok := byName[d.nameStart]; ok {
		if kindRank[d.kind] > kindRank[prev.kind] {
			prev.kind, prev.body, prev.test = d.kind, d.body, d.test || prev.test
		}
		return
	}
	if atRecordBound(e.maxRecords, e.declCount) {
		e.truncated = true
		return
	}
	byName[d.nameStart] = d
	e.declCount++
}

func (e *extraction) addImport(node ts.Node, caps map[string][]ts.Node) {
	key := span{node.StartByte(), node.EndByte()}
	rec := e.imports[key]
	if rec == nil {
		if atRecordBound(e.maxRecords, len(e.imports)) {
			e.truncated = true
			return
		}
		rec = &importRec{span: key}
		e.imports[key] = rec
	}
	if p := caps["import.path"]; len(p) > 0 && rec.path == "" {
		rec.path = strings.Trim(p[0].Utf8Text(e.src), "\"'`")
	}
	for _, n := range caps["import.name"] {
		if t := n.Utf8Text(e.src); t != "" && !slices.Contains(rec.names, t) {
			rec.names = append(rec.names, t)
		}
	}
}

func (e *extraction) addRef(kind string, node ts.Node, name []ts.Node, qualifier []ts.Node) {
	if len(name) == 0 {
		return
	}
	if atRecordBound(e.maxRecords, len(e.refs)) {
		e.truncated = true
		return
	}
	r := refRec{span: span{node.StartByte(), node.EndByte()}, kind: kind, name: name[0].Utf8Text(e.src),
		nameSpan: span{name[0].StartByte(), name[0].EndByte()}}
	if len(qualifier) > 0 {
		r.qualifier = &qualifier[0]
	}
	e.refs = append(e.refs, r)
}

// emit numbers declarations in source order, derives containment, qualified
// names, kinds, docs and flags, and writes every record.
func (e *extraction) emit(out *emitter) error {
	var decls []*decl
	for _, byName := range e.decls {
		for _, d := range byName {
			decls = append(decls, d)
		}
	}
	// Source order: outer before inner, then by name position, so numbering is
	// a function of the bytes alone.
	slices.SortFunc(decls, func(a, b *decl) int {
		if c := cmpUint(a.node.StartByte(), b.node.StartByte()); c != 0 {
			return c
		}
		if c := cmpUint(b.node.EndByte(), a.node.EndByte()); c != 0 {
			return c
		}
		return cmpUint(a.nameStart, b.nameStart)
	})
	slices.SortFunc(e.scopes, func(a, b scopeNode) int { return cmpUint(a.start, b.start) })

	// Containment via a stack of open declarations; scope nodes take part in
	// the chain but are never emitted.
	var stack []frame
	nameSpans := make(map[span]bool, len(decls))
	wireDecls := make([]wire.Decl, 0, len(decls))
	scopeIdx := 0
	for _, d := range decls {
		if e.g.refine != nil {
			e.g.refine(d, e.src)
		}
		if d.name == "" {
			continue
		}
		start, end := d.node.StartByte(), d.node.EndByte()
		for scopeIdx < len(e.scopes) && e.scopes[scopeIdx].start <= start {
			s := e.scopes[scopeIdx]
			stack = pop(stack, s.start)
			stack = append(stack, frame{span: s.span, idx: -1, name: s.name})
			scopeIdx++
		}
		stack = pop(stack, start)
		// A declaration sharing its exact range with the previous one (a
		// multi-name field) is a sibling, not a child.
		if n := len(stack); n > 0 && stack[n-1].span == (span{start, end}) {
			stack = stack[:n-1]
		}
		var parts []string
		if e.pkg != "" && (e.l.Name == "go" || e.l.Name == "java") {
			parts = append(parts, e.pkg)
		}
		for _, f := range stack {
			parts = append(parts, f.name)
			if f.idx >= 0 {
				d.parentIdx = f.idx
			}
		}
		if n := len(stack); n > 0 {
			top := stack[n-1]
			if d.kind == "function" && (top.idx < 0 || typeLike[top.kind]) {
				d.kind = "method"
			}
			if top.idx < 0 && d.impl == "" {
				d.impl = top.name
			}
		}
		if d.impl != "" && !slices.Contains(parts, d.impl) {
			parts = append(parts, d.impl)
		}
		if d.parentIdx < 0 && e.exportNames[d.name] {
			d.exported = true
		}
		if e.g.isTest != nil && e.g.isTest(d, e.path, e.src) {
			d.kind, d.test = "test", true
		}
		parts = append(parts, d.name)
		w := wire.Decl{ID: len(wireDecls), Parent: -1, Kind: d.kind, Name: d.name, Qualified: strings.Join(parts, e.l.Separator),
			Start: uint32(start), End: uint32(end), SigEnd: uint32(end),
			Exported: d.exported, Test: d.test, Prototype: d.prototype, Macro: d.macro, Impl: d.impl}
		if d.parentIdx >= 0 {
			w.Parent = d.parentIdx
		}
		if d.body != nil && d.body.StartByte() > start {
			w.SigEnd = uint32(d.body.StartByte())
		}
		if ds, de, ok := e.doc(d); ok {
			w.DocStart, w.DocEnd = uint32(ds), uint32(de)
		}
		nameSpans[span{d.nameStart, d.nameEnd}] = true
		stack = append(stack, frame{span: span{start, end}, idx: w.ID, name: d.name, kind: d.kind})
		wireDecls = append(wireDecls, w)
	}
	for _, w := range wireDecls {
		if err := out.decl(w); err != nil {
			return err
		}
	}

	imports := make([]*importRec, 0, len(e.imports))
	for _, rec := range e.imports {
		imports = append(imports, rec)
	}
	slices.SortFunc(imports, func(a, b *importRec) int { return cmpUint(a.start, b.start) })
	localNames := map[string]bool{}
	for _, rec := range imports {
		if rec.path == "" {
			continue
		}
		if len(rec.names) == 0 && e.g.importNames != nil {
			rec.names = e.g.importNames(rec.path)
		}
		for _, n := range rec.names {
			localNames[n] = true
		}
		if err := out.imp(wire.Import{Start: uint32(rec.start), End: uint32(rec.end), Path: rec.path, Names: rec.names}); err != nil {
			return err
		}
	}

	slices.SortFunc(e.refs, func(a, b refRec) int {
		if c := cmpUint(a.start, b.start); c != 0 {
			return c
		}
		return cmpUint(a.end, b.end)
	})
	for _, r := range e.refs {
		if r.kind == "type" && nameSpans[r.span] {
			continue // the declaration's own name
		}
		if r.kind == "call" && isTestDecl(wireDecls, r.span) {
			continue // a test declaration is not a call to its framework
		}
		w := wire.Ref{Kind: r.kind, Start: uint32(r.start), End: uint32(r.end),
			NameStart: uint32(r.nameSpan.start), NameEnd: uint32(r.nameSpan.end),
			Name: r.name, Scope: enclosing(wireDecls, r.span)}
		if r.qualifier != nil {
			q := r.qualifier.Utf8Text(e.src)
			// The import decision is made on the receiver as written; only
			// the reported copy is bounded, so a call through an import is
			// still recognized however long the receiver expression is.
			w.Qualified, w.QualifierIsImport = true, localNames[q]
			if len(q) <= wire.MaxQualifierBytes {
				w.Qualifier = q
			}
		}
		if err := out.ref(w); err != nil {
			return err
		}
	}
	return nil
}

// doc returns the documentation attached to d: the language's in-body
// docstring, else the comments immediately preceding the declaration, else
// those preceding the wrapper it sits in (an export statement, a decorated
// definition, a grouped type declaration).
func (e *extraction) doc(d *decl) (uint, uint, bool) {
	if e.g.docstring != nil {
		if s, en, ok := e.g.docstring(d); ok {
			return s, en, true
		}
	}
	anchor := &d.node
	for {
		if s, en, ok := e.precedingComments(anchor); ok {
			return s, en, true
		}
		p := anchor.Parent()
		if p == nil || !e.g.wrappers[p.Kind()] {
			return 0, 0, false
		}
		anchor = p
	}
}

// precedingComments collects the contiguous run of comment siblings ending
// on the line before n (or on n's own line).
func (e *extraction) precedingComments(n *ts.Node) (uint, uint, bool) {
	var start, end uint
	found := false
	next := n
	for p := n.PrevNamedSibling(); p != nil && e.g.comments[p.Kind()]; p = p.PrevNamedSibling() {
		if p.EndPosition().Row+1 < next.StartPosition().Row {
			break
		}
		if !found {
			end = p.EndByte()
		}
		start, found, next = p.StartByte(), true, p
	}
	return start, end, found
}

// frame is one open declaration or scope node on the containment stack; idx
// is the declaration ordinal, or -1 for a scope node.
type frame struct {
	span
	idx  int
	name string
	kind string
}

// pop closes every open frame that ends at or before offset.
func pop(stack []frame, offset uint) []frame {
	for len(stack) > 0 && stack[len(stack)-1].end <= offset {
		stack = stack[:len(stack)-1]
	}
	return stack
}

// enclosing returns the innermost declaration containing r, or -1.
func enclosing(decls []wire.Decl, r span) int {
	i, _ := slices.BinarySearchFunc(decls, r.start, func(d wire.Decl, start uint) int { return cmpUint(uint(d.Start), start) })
	// i is the first declaration starting after r.start (or at it, for a
	// declaration whose own range is r); walk back to the last starting at or
	// before r.start and then up its parent chain to the one that contains r.
	for i > 0 && (i >= len(decls) || uint(decls[i].Start) > r.start) {
		i--
	}
	if i >= len(decls) || uint(decls[i].Start) > r.start {
		return -1
	}
	for j := i; j >= 0; j = decls[j].Parent {
		if uint(decls[j].Start) <= r.start && r.end <= uint(decls[j].End) && !(uint(decls[j].Start) == r.start && uint(decls[j].End) == r.end) {
			return j
		}
	}
	return -1
}

func isTestDecl(decls []wire.Decl, r span) bool {
	i, ok := slices.BinarySearchFunc(decls, r.start, func(d wire.Decl, start uint) int { return cmpUint(uint(d.Start), start) })
	for ; ok && i < len(decls) && uint(decls[i].Start) == r.start; i++ {
		if uint(decls[i].End) == r.end && decls[i].Kind == "test" {
			return true
		}
	}
	return false
}

func cmpUint(a, b uint) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}
