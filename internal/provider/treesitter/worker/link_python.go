package worker

import (
	"github.com/Sawmonabo/codectx/internal/provider/treesitter/flow"
	"github.com/Sawmonabo/codectx/internal/provider/treesitter/wire"
)

// pythonLinker builds the link tables of one Python file.
type pythonLinker struct {
}

func newPythonLinker() *pythonLinker { return &pythonLinker{} }

func (k *pythonLinker) function(fn Node, g *flow.Graph, du flow.Edges, src []byte) {}

func (k *pythonLinker) file(root Node, src []byte, out *wire.Link) {}
