package worker

import (
	"github.com/Sawmonabo/codectx/internal/provider/treesitter/flow"
	"github.com/Sawmonabo/codectx/internal/provider/treesitter/wire"
)

// javaLinker builds the link tables of one Java file.
type javaLinker struct {
}

func newJavaLinker() *javaLinker { return &javaLinker{} }

func (k *javaLinker) function(fn Node, g *flow.Graph, du flow.Edges, src []byte) {}

func (k *javaLinker) file(root Node, src []byte, out *wire.Link) {}
