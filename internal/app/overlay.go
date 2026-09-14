package app

import (
	"context"

	"github.com/Sawmonabo/codectx/internal/model"
)

// overlaySymbols and overlayReferences are the LSP route of Section 18.2: the
// facade dispatches semantic_source=lsp here, this resolves the profile, opens
// the managed server over the session's pinned snapshot view and maps the
// symbol and reference operation enums onto the overlay's own calls.
//
// The two signatures are the ones the skeleton lane froze in
// internal/workflow/workflow.go's comment block, so the facade's call sites
// compile against the shape lane L8 fills in. The bodies below are placeholders
// this lane created only so the facade could be wired; L8 replaces them. They
// are a typed CTX_INTERNAL rather than a panic so a command that reaches the
// route before L8 lands reports a defect instead of crashing.
//
// Whatever L8 writes here never persists a row and never grants coverage
// credit: an overlay answer is labelled model.SemanticLSP and carries the
// overlay's model.OverlayBinding, and a canonical fact is the only thing this
// tree stores.
func (s *stack) overlaySymbols(ctx context.Context, req model.SymbolRequest) (model.Page[model.Node], error) {
	return model.Page[model.Node]{}, overlayUnimplemented("symbol")
}

func (s *stack) overlayReferences(ctx context.Context, req model.ReferenceRequest) (model.Page[model.ReferenceOccurrence], error) {
	return model.Page[model.ReferenceOccurrence]{}, overlayUnimplemented("references")
}

// overlayUnimplemented is the placeholder answer of both routes above.
func overlayUnimplemented(op string) error {
	return &model.Error{Code: model.CodeInternal,
		Message: "app: the LSP overlay route for " + op + " is not implemented"}
}
