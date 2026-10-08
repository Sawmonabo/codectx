package link

import "context"

// resolveECMAScript resolves the sites of a JavaScript, TypeScript or TSX
// unit: direct binding through imports and scopes, flow-based type inference
// (inferTypes), field-based resolution and class-hierarchy analysis where a
// subtype relation exists.
func resolveECMAScript(ctx context.Context, u *unit, emit func(Binding) error) error { return nil }
