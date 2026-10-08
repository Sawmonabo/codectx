package worker

import (
	"github.com/Sawmonabo/codectx/internal/provider/treesitter/flow"
	"github.com/Sawmonabo/codectx/internal/provider/treesitter/wire"
)

// javaScriptLinker builds the link tables of one the ECMAScript family (language is "javascript", "typescript" or "tsx") file.
type javaScriptLinker struct {
	language string
}

func newJavaScriptLinker(language string) *javaScriptLinker {
	return &javaScriptLinker{language: language}
}

func (k *javaScriptLinker) function(fn Node, g *flow.Graph, du flow.Edges, src []byte) {}

func (k *javaScriptLinker) file(root Node, src []byte, out *wire.Link) {}
