package link

import (
	"context"

	"github.com/Sawmonabo/codectx/internal/model"
)

// unit is one linking unit's member tables as the resolvers read them.
type unit struct {
	gen    model.GenerationID
	tables Tables
	// members yields the unit's member files in ascending FileID order.
	members func(yield func(model.FileVersion) error) error
	// dir is the unit's private scratch directory, where a resolver's
	// external sorts spill.
	dir string
}

// Files streams the unit's member tables in ascending FileID order, one
// decoded at a time; file is the member's ordinal in that order, the File of
// a Ref. A member with no table is skipped and keeps its ordinal. A table
// must not be retained past the yield that receives it.
func (u *unit) Files(ctx context.Context, yield func(file int32, fv model.FileVersion, t *Table) error) error {
	var file int32 = -1
	return u.members(func(fv model.FileVersion) error {
		file++
		t, ok, err := u.tables.Table(ctx, u.gen, fv)
		if err != nil || !ok {
			return err
		}
		return yield(file, fv, t)
	})
}

// Ref names one entry of a member's table: Index into its Decls, or into its
// Sites, by context.
type Ref struct {
	File, Index int32
}

// Binding is one declaration a resolver found a site may call.
type Binding struct {
	Site   Ref
	Target Ref
	// Detail is the technique that found it: one of the Detail constants.
	Detail string
}

// A resolver resolves every site of one unit and emits each binding it finds,
// in any order and with repeats; the core sorts them by site and target and
// publishes one `calls` occurrence per site and target. A site no resolver
// binds publishes nothing, and its structural call stands alone.
type resolver func(ctx context.Context, u *unit, emit func(Binding) error) error
