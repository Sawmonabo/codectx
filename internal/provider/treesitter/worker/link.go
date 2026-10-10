package worker

import (
	"bytes"
	"cmp"
	"io"
	"slices"
	"sort"
	"strconv"

	"github.com/Sawmonabo/codectx/internal/provider/treesitter/flow"
	"github.com/Sawmonabo/codectx/internal/provider/treesitter/wire"
)

// linker builds one file's link tables (wire.Link) from the file's flat array
// beside the dependence pass, in the same walk. One linker serves one file:
// the dependence pass makes it for the file's language (newLinker) and drops
// it with the rest of the file's memory at the file boundary (state.release).
//
// The pass calls function once per callable, in the lowering's preorder,
// after that callable's analysis, and then file once. Neither may retain g,
// du or any node of the flat array past the call that receives it: the arena
// that holds g and du is reused by the next callable.
type linker interface {
	// function records what one analysed callable says about the file's
	// tables: the types its sites' receivers and arguments carry, traced
	// through the callable's assignments over its def-use chains du, and the
	// type it returns. fn is the callable, g its lowered graph, src the
	// file's bytes.
	function(fn Node, g *flow.Graph, du flow.Edges, src []byte)
	// file appends the file's exports, supertypes, sites and values to out,
	// read from the flat tree under root, after every callable.
	file(root Node, src []byte, out *wire.Link)
	// reads is the file's index of named-variable reads, which linkFunction
	// fills with every analysed callable before the linker reads it.
	reads() *readIndex
}

// linkReads gives a linker its file's read index; every linker embeds it.
type linkReads struct{ r readIndex }

func (l *linkReads) reads() *readIndex { return &l.r }

// newLinker returns the linker of language, for the languages that have a
// lowering. Each language family's link file declares its own.
func newLinker(language string) (linker, bool) {
	switch language {
	case "c", "cpp":
		return newCLinker(language), true
	case "go":
		return newGoLinker(), true
	case "java":
		return newJavaLinker(), true
	case "javascript", "typescript", "tsx":
		return newJavaScriptLinker(language), true
	case "python":
		return newPythonLinker(), true
	case "rust":
		return newRustLinker(), true
	}
	return nil, false
}

// linkFunction runs the file's linker over one analysed callable. A panic is
// recovered and abandons the file's link tables, never the callable's
// dependence facts or the file's structural facts: the failure is disclosed
// as Done.LinkFailure and no link message is sent.
func (w *state) linkFunction(fn Node, g *flow.Graph, du flow.Edges, src []byte) {
	if w.dep.linker == nil || w.dep.linkFailure != "" {
		return
	}
	defer func() {
		if r := recover(); r != nil {
			w.dep.linkFailure = "the link pass panicked in the function at bytes " + spanText(spanOf(fn)) + ": " + panicText(r)
		}
	}()
	w.dep.linker.reads().add(g, src)
	w.dep.linker.function(fn, g, du, src)
}

// link finishes the file's link tables and sends them as one KindLink
// message, unless the pass already failed; it returns the failure Done
// discloses. The error is a write failure, which ends the loop.
func (w *state) link(out io.Writer, root Node, src []byte) (failure string, err error) {
	if w.dep.linker == nil {
		return "", nil
	}
	if w.dep.linkFailure == "" {
		w.linkFile(root, src)
	}
	if w.dep.linkFailure != "" {
		return w.dep.linkFailure, nil
	}
	w.dep.msg = wire.AppendLink(w.dep.msg[:0], &w.dep.link)
	return "", wire.WriteMessage(out, wire.KindLink, w.dep.msg)
}

// linkFile runs the linker's file step, recovering a panic as the file's link
// failure.
func (w *state) linkFile(root Node, src []byte) {
	defer func() {
		if r := recover(); r != nil {
			w.dep.linkFailure = "the link pass panicked outside every function: " + panicText(r)
		}
	}()
	w.dep.linker.file(root, src, &w.dep.link)
}

// spanText is s as "start-end", the byte range a failure names.
func spanText(s flow.Span) string {
	return strconv.FormatUint(uint64(s.Start), 10) + "-" + strconv.FormatUint(uint64(s.End), 10)
}

// readIndex records, for one file, every read of a named variable by the
// flow graph node that reads it, across the file's analysed callables, so a
// linker can tell which variable an identifier inside an argument names: the
// one the innermost node spanning the identifier reads under that spelling.
// That is the lowering's own name resolution, block scoping, closures and
// shadowing included: a closure's graph names an enclosing callable's
// variable by the identifier that declares it, the span Function.Vars
// carries.
type readIndex struct {
	src  []byte
	uses []readUse
	// sorted reports uses ordered by the declared spelling, then by the
	// reading node's start ascending and its end descending; reach is, per
	// entry, the greatest node end of its spelling's entries up to it.
	sorted bool
	reach  []uint32
}

// readUse is one node's read of one named variable: the node's span and the
// variable's declaring identifier.
type readUse struct{ node, decl flow.Span }

// add records every named variable each node of g reads.
func (r *readIndex) add(g *flow.Graph, src []byte) {
	r.src, r.sorted = src, false
	for n := range int32(g.Len()) {
		for _, v := range g.Uses(n) {
			if d, named := g.Declared(v); named {
				r.uses = append(r.uses, readUse{node: g.Span(n), decl: d})
			}
		}
	}
}

func (r *readIndex) name(u readUse) []byte { return r.src[u.decl.Start:u.decl.End] }

func (r *readIndex) sort() {
	if r.sorted {
		return
	}
	r.sorted = true
	slices.SortFunc(r.uses, func(a, b readUse) int {
		return cmp.Or(bytes.Compare(r.name(a), r.name(b)), cmp.Compare(a.node.Start, b.node.Start),
			cmp.Compare(b.node.End, a.node.End), cmp.Compare(a.decl.Start, b.decl.Start))
	})
	r.uses = slices.Compact(r.uses)
	r.reach = slices.Grow(r.reach[:0], len(r.uses))[:len(r.uses)]
	for i, u := range r.uses {
		r.reach[i] = u.node.End
		if i > 0 && r.reach[i-1] > u.node.End && bytes.Equal(r.name(r.uses[i-1]), r.name(u)) {
			r.reach[i] = r.reach[i-1]
		}
	}
}

// variable is the declaring identifier of the named variable that the
// identifier id reads: the variable spelled as id that the innermost node
// spanning id reads. It reports false for an identifier no node reads as a
// variable (a type, a module, a field, a name the lowering binds nothing to).
func (r *readIndex) variable(id Node) (wire.Span, bool) {
	if r.src == nil {
		return wire.Span{}, false
	}
	r.sort()
	text := textOf(r.src, id)
	s := spanOf(id)
	lo, _ := slices.BinarySearchFunc(r.uses, text, func(u readUse, t []byte) int { return bytes.Compare(r.name(u), t) })
	hi := lo + sort.Search(len(r.uses)-lo, func(i int) bool { return bytes.Compare(r.name(r.uses[lo+i]), text) > 0 })
	i, _ := slices.BinarySearchFunc(r.uses[lo:hi], s.Start+1, func(u readUse, x uint32) int { return cmp.Compare(u.node.Start, x) })
	for i += lo - 1; i >= lo && r.reach[i] >= s.End; i-- {
		if r.uses[i].node.End >= s.End {
			return wireSpan(r.uses[i].decl), true
		}
	}
	return wire.Span{}, false
}

// argReads appends to dst the declaring identifiers of the named variables
// the argument expression arg reads, each once, in source order of the first
// read: every identifier under arg that name accepts, resolved by variable.
// skip names the subtrees whose identifiers the argument does not read: a
// call's result, a nested callable's body; arg itself may be one, which
// reads nothing.
func (r *readIndex) argReads(arg Node, name, skip func(Node) bool, dst []wire.Span) []wire.Span {
	start := len(dst)
	var walk func(n Node)
	walk = func(n Node) {
		switch {
		case n.IsNull() || skip(n):
			return
		case name(n):
			if d, ok := r.variable(n); ok && !slices.Contains(dst[start:], d) {
				dst = append(dst, d)
			}
			return
		}
		for c := n.first(); !c.IsNull(); c = c.next() {
			if c.IsNamed() {
				walk(c)
			}
		}
	}
	walk(arg)
	return dst
}
