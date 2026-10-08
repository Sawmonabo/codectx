package worker

import (
	"github.com/Sawmonabo/codectx/internal/provider/treesitter/flow"
	"github.com/Sawmonabo/codectx/internal/provider/treesitter/wire"
)

// goLinker builds the link tables of one Go file.
type goLinker struct {
}

func newGoLinker() *goLinker { return &goLinker{} }

func (k *goLinker) function(fn Node, g *flow.Graph, du flow.Edges, src []byte) {}

func (k *goLinker) file(root Node, src []byte, out *wire.Link) {}
