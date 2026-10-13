package app

import (
	"testing"

	"github.com/Sawmonabo/codectx/internal/model"
	"github.com/Sawmonabo/codectx/internal/provider/lsp"
)

// TestOverlayNodesCarryTheServerSelectionAsNameRange pins that an overlay
// symbol node carries the server's selection range (the name token) as its
// NameRange, and that a selection outside the symbol's range is left out so
// the node still validates.
// Mutation caught: dropping Selection (the overlay node has no name range,
// unlike the sealed node for the same symbol), or copying an out-of-range
// selection (Node.Validate fails the whole answer).
func TestOverlayNodesCarryTheServerSelectionAsNameRange(t *testing.T) {
	pos := func(b uint64) model.Position { return model.Position{Byte: b, Line: 1, Column: uint32(b)} }
	rng := model.SourceRange{Start: pos(10), End: pos(40)}
	inside := model.SourceRange{Start: pos(15), End: pos(18)}
	outside := model.SourceRange{Start: pos(41), End: pos(44)}
	const file = model.FileID("0000000000000000000000000000000000000000000000000000000000000002")
	symbol := func(sel *model.SourceRange) lsp.Symbol {
		return lsp.Symbol{Name: "Run", Kind: model.NodeFunction,
			Location: lsp.Location{File: file, Path: "a.go", ContentHash: string(file), Range: rng, Selection: sel}}
	}
	nodes := overlaySymbolNodes([]lsp.Symbol{symbol(&inside), symbol(&outside), symbol(nil)})
	if got := nodes[0].NameRange; got == nil || *got != inside {
		t.Errorf("name range = %v, want the selection %v", got, inside)
	}
	for i, n := range nodes {
		if i > 0 && n.NameRange != nil {
			t.Errorf("node %d has name range %v, want none (selection outside the range, or absent)", i, n.NameRange)
		}
		if err := n.Validate(); err != nil {
			t.Errorf("node %d does not validate: %v", i, err)
		}
	}
}
