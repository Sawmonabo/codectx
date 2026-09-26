package worker

import (
	ts "github.com/tree-sitter/go-tree-sitter"

	"github.com/Sawmonabo/codectx/internal/provider/treesitter/flow"
)

// goLowering lowers Go functions, methods and function literals.
var goLowering = Lowering{
	callable: set("function_declaration", "method_declaration", "func_literal"),
	lower:    lowerGo,
}

// lowerGo lowers one Go callable's parameters and body into b.
func lowerGo(l *Lowering, b *flow.Builder, fn *ts.Node, src []byte) {}
