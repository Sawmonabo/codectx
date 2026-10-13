package link

import (
	"context"
	"strings"
)

// rustTraitOverrides emits what the Rust method d at ref implements, d being
// an item of an `impl Trait for Type` block that names its trait
// (Decl.Trait). A trait impl defines only its trait's items (the Rust
// Reference's "Trait implementations"), so d implements that one trait's
// item of its name and no other trait's of its self type: the trait's
// method of the unit when the trait is the unit's, else the external
// trait's method, named as the external trait node of the block's supertype
// is (externalName). The self type is never consulted, so a blanket impl
// over a type parameter (`impl<T: Debug> Describe for T`), which has no
// self type of the unit, implements its trait's item too. Any other Rust
// function implements nothing: the worker names the trait of every item of
// a block the grammar parses as a trait impl, since the grammar's trait
// field is a type identifier, a scoped one or a generic type over either,
// all of which it spells (wire.Export.Trait).
func (h *hierarchy) rustTraitOverrides(ctx context.Context, fc fileCtx, ref Ref, d *Decl, emit func(Binding) error) error {
	if !d.Override || d.Trait == "" {
		return nil
	}
	inUnit := false
	err := h.typesIn(fc, d.Trait, func(trait entry) error {
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

// rustGlobalExternal is what the Rust global path `::path` names outside
// the unit: path itself, read from the crate it starts with, unless that
// crate is the unit's (keyRustCrate), whose types typesIn reads. A path
// starting with `::` names a crate of the extern prelude and is read from
// it whatever the file's `use` declarations bind (the Rust Reference's
// "Paths", path qualifiers, 2018 edition and later; rustPath reads it the
// same way inside the unit). `::name` alone names a crate, not a type.
func (h *hierarchy) rustGlobalExternal(path string) (qualified string, ok bool, err error) {
	crate, _, qualifiedPath := strings.Cut(path, "::")
	if !qualifiedPath || crate == "" {
		return "", false, nil
	}
	unit := false
	if err := h.decls.lookup(keyRustCrate+crate, func(entry) error {
		unit = true
		return nil
	}); err != nil || unit {
		return "", false, err
	}
	return path, true, nil
}
