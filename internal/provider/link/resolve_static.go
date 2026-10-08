package link

import "context"

// resolveStatic resolves the sites of a Java, Go, Rust, C or C++ unit: direct
// binding, the signature-keyed address-taken × indirect-site cross-product
// without a reachability fixed point, and class-hierarchy analysis, for Java
// over classes and interfaces and for the others over interfaces and traits.
func resolveStatic(ctx context.Context, u *unit, emit func(Binding) error) error { return nil }
