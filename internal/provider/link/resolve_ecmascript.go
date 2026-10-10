package link

import "context"

// resolveECMAScript resolves the sites of a JavaScript, TypeScript or TSX
// unit: direct binding through imports and scopes, flow-based type inference
// (inferTypes), field-based resolution and class-hierarchy analysis where a
// subtype relation exists. The inference binds every site as part of its
// fixed point, since a site's targets decide what flows into their
// parameters; the resolver emits what the fixed point bound.
func resolveECMAScript(ctx context.Context, u *unit, emit func(Binding) error) error {
	h, err := buildHierarchy(ctx, u)
	if err != nil {
		return err
	}
	defer h.close()
	t, err := inferTypes(ctx, u, h)
	if err != nil {
		return err
	}
	defer t.close()
	return t.bindings(emit)
}
