package link

import (
	"cmp"
	"context"
	"encoding/json"
	"strings"

	srclang "github.com/Sawmonabo/codectx/internal/lang"
	"github.com/Sawmonabo/codectx/internal/model"
	"github.com/Sawmonabo/codectx/internal/pagination"
	"github.com/Sawmonabo/codectx/internal/provider"
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

// compareRef orders refs by file, then index.
func compareRef(a, b Ref) int {
	return cmp.Or(cmp.Compare(a.File, b.File), cmp.Compare(a.Index, b.Index))
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

// planned is the plan's unit the request names. Planning from the
// pinned snapshot is what makes the members the ones the unit's inputs were
// keyed on; a scope the plan does not hold is a disagreement with the
// planner and fails rather than being guessed at.
func planned(ctx context.Context, req provider.UnitRequest) (Unit, error) {
	plan, err := PlanUnits(ctx, req.Content)
	if err != nil {
		return Unit{}, err
	}
	for _, pu := range plan.Units {
		if pu.ScopeKey == req.Unit.ScopeKey {
			return pu, nil
		}
	}
	return Unit{}, &model.Error{Code: model.CodeArgumentInvalid,
		Message: "the linking provider has no unit for this scope in this snapshot",
		Details: map[string]string{"scope_key": req.Unit.ScopeKey}}
}

// resolverFor is the resolution of decision 2 for a unit's language family.
func resolverFor(f Family) resolver {
	switch f {
	case FamilyJavaScript:
		return resolveECMAScript
	case FamilyPython:
		return resolvePython
	}
	return resolveStatic
}

// sortMembers sorts the unit's member files into ascending FileID order: a
// live source file of the unit's own family that the unit owns. Manifests
// and lock files are inputs of the unit's closure but carry no table, so they
// are not members. The manifest is served in path order and a FileID is a
// digest, so the order is an external sort that spills rather than a list
// held in the heap.
func sortMembers(ctx context.Context, view model.SnapshotView, pu Unit, dir string) (*pagination.SortedRun[model.FileVersion], error) {
	s, err := pagination.NewExternalSort(dir, 0,
		func(fv model.FileVersion) ([]byte, error) { return json.Marshal(fv) },
		func(b []byte) (model.FileVersion, error) {
			var fv model.FileVersion
			err := json.Unmarshal(b, &fv)
			return fv, err
		},
		func(a, b model.FileVersion) int { return strings.Compare(string(a.ID), string(b.ID)) })
	if err != nil {
		return nil, err
	}
	defer s.Close()
	err = view.EachFile(ctx, model.FileSelection{}, func(fv model.FileVersion) error {
		if fv.Status == model.FileDeleted || !pu.OwnsInput(fv.Path) || FamilyOf(srclang.Of(fv.Path)) != pu.Family {
			return nil
		}
		return s.Add(fv)
	})
	if err != nil {
		return nil, err
	}
	return s.Sorted()
}

// coverage is what the unit's tables leave out: members with no table, whose
// sites the linker cannot see, and the worker entries their parent dropped.
type coverage struct {
	untabled int64
	dropped  int64
}
