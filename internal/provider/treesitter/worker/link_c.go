package worker

import (
	"github.com/Sawmonabo/codectx/internal/provider/treesitter/flow"
	"github.com/Sawmonabo/codectx/internal/provider/treesitter/wire"
)

// cLinker builds the link tables of one C and C++ (language is "c" or "cpp") file.
type cLinker struct {
	language string
}

func newCLinker(language string) *cLinker { return &cLinker{language: language} }

func (k *cLinker) function(fn Node, g *flow.Graph, du flow.Edges, src []byte) {}

func (k *cLinker) file(root Node, src []byte, out *wire.Link) {}
