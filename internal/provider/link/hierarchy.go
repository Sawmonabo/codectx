package link

import "context"

// hierarchy is one unit's class hierarchy: every type declaration, the
// subtype relation its tables declare (wire.Supertype) and, for Go, the
// interfaces each type's method set satisfies. It answers class-hierarchy
// analysis for every resolver.
type hierarchy struct{}

// buildHierarchy reads the unit's tables into its hierarchy.
func buildHierarchy(ctx context.Context, u *unit) (*hierarchy, error) { return &hierarchy{}, nil }

// types yields the type declarations a type name as written in a table of
// the member file may denote, each once.
func (h *hierarchy) types(ctx context.Context, file int32, name string, yield func(Ref) error) error {
	return nil
}

// dispatch yields every method declaration a call of method on a receiver
// whose static type is the declaration typ may reach: the method typ
// declares or inherits, and every override in a subtype of typ, each once.
func (h *hierarchy) dispatch(ctx context.Context, typ Ref, method string, yield func(Ref) error) error {
	return nil
}

// close releases what the hierarchy spilled.
func (h *hierarchy) close() error { return nil }
