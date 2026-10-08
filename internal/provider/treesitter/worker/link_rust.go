package worker

import (
	"github.com/Sawmonabo/codectx/internal/provider/treesitter/flow"
	"github.com/Sawmonabo/codectx/internal/provider/treesitter/wire"
)

// rustLinker builds the link tables of one Rust file.
type rustLinker struct {
}

func newRustLinker() *rustLinker { return &rustLinker{} }

func (k *rustLinker) function(fn Node, g *flow.Graph, du flow.Edges, src []byte) {}

func (k *rustLinker) file(root Node, src []byte, out *wire.Link) {}
