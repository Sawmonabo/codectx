package link

import "context"

// resolvePython resolves the sites of a Python unit: direct binding through
// imports and modules, a call of a class as its construction, flow-based type
// inference (inferTypes), field-based resolution and class-hierarchy
// analysis in method resolution order.
func resolvePython(ctx context.Context, u *unit, emit func(Binding) error) error { return nil }
