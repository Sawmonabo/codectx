package worker

import (
	ts "github.com/tree-sitter/go-tree-sitter"

	"github.com/Sawmonabo/codectx/internal/provider/treesitter/flow"
)

// javascriptLowering lowers JavaScript functions, methods, arrows,
// generators and async functions (async is a modifier of these kinds).
var javascriptLowering = Lowering{
	callable: set("function_declaration", "function_expression", "arrow_function", "method_definition",
		"generator_function", "generator_function_declaration"),
	lower: lowerJavaScript,
}

// lowerJavaScript lowers one JavaScript callable's parameters and body into b.
func lowerJavaScript(l *Lowering, b *flow.Builder, fn *ts.Node, src []byte) {}
