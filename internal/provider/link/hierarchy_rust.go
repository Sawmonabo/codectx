package link

import "context"

// rustTraitOverrides emits what the Rust method d at ref implements, d being
// an item of an `impl Trait for Type` block that names its trait
// (Decl.Trait). A trait impl defines only its trait's items (the Rust
// Reference's "Trait implementations"), so d implements that one trait's
// item of its name and no other trait's of its self type: the trait's
// method of the unit when the trait is the unit's, else the external
// trait's method, named as the external trait node of the block's supertype
// is (externalName). The self type is never consulted, so a blanket impl
// over a type parameter (`impl<T: Debug> Describe for T`), which has no
// self type of the unit, implements its trait's item too.
func (h *hierarchy) rustTraitOverrides(ctx context.Context, fc fileCtx, ref Ref, d *Decl, emit func(Binding) error) error {
	inUnit := false
	err := h.typesIn(ctx, fc, d.Trait, func(trait entry) error {
		inUnit = true
		return h.methods(trait.Ref, d.Name, func(e entry) error {
			if e.Ref == ref {
				return nil
			}
			return emit(Binding{Kind: FactOverrides, Site: ref, Target: e.Ref, Detail: DetailSupertype})
		})
	})
	if err != nil || inUnit {
		return err
	}
	external, ok, err := h.externalName(ctx, fc, d.Trait)
	if err != nil || !ok {
		return err
	}
	return emit(Binding{Kind: FactOverrides, Site: ref, Detail: DetailSupertype, External: externalMethod(fc.family, external, d.Name)})
}
