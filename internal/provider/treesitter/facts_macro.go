package treesitter

import "github.com/Sawmonabo/codectx/internal/model"

// macroOrigin marks, in a declaration's metadata, a declaration written in
// the body of the item-position Rust macro macro (wire.Decl.MacroInput):
// `origin` is `macro_input` and `origin_macro` the macro's path as written,
// bounded as a name is.
// The declaration is the macro's input as written, evidence of what the
// expansion declares rather than proof: a macro may transcribe its input
// once, several times, renamed or not at all; `origin_exact` marks one the
// worker proved the expansion holds as written (wire.Decl.MacroExact). An
// empty macro marks nothing.
func macroOrigin(meta map[string]any, macro string, exact bool) {
	if macro == "" {
		return
	}
	meta["origin"] = "macro_input"
	meta["origin_macro"] = bound(macro, model.MaxNameBytes)
	if exact {
		meta["origin_exact"] = true
	}
}
