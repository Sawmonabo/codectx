package worker

import (
	"io"
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
}

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
