package link

import (
	"cmp"
	"context"
	"encoding/json"
	"strings"

	"github.com/Sawmonabo/codectx/internal/model"
	"github.com/Sawmonabo/codectx/internal/pagination"
	"github.com/Sawmonabo/codectx/internal/provider"
	"github.com/Sawmonabo/codectx/internal/provider/dependence"
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
	// root is the project's root-relative directory (dependence.Unit.Root),
	// the empty string the repository root, and content the pinned snapshot
	// its manifests are read from (a Go module's go.mod, which decides which
	// import paths are the project's own).
	root    string
	content model.SnapshotView
	// family is the unit's language family; a Rust unit reads its crates'
	// names from their Cargo.toml (rustCrates).
	family dependence.Family
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

// resolverFor is the resolution of decision 2 for a unit's language family.
func resolverFor(f dependence.Family) resolver {
	switch f {
	case dependence.FamilyJavaScript:
		return resolveECMAScript
	case dependence.FamilyPython:
		return resolvePython
	}
	return resolveStatic
}

// members is a unit's member files in ascending FileID order: each live
// source file of the unit's own family that the unit owns. Manifests and lock
// files are inputs of the unit's closure but carry no table, so they are not
// members.
type members struct {
	unit dependence.Unit
	// run holds every live file of the family under the unit's root, sorted,
	// and Each keeps those the unit owns; n is how many it keeps.
	run *pagination.SortedRun[model.FileVersion]
	n   int64
}

// Each yields the members in ascending FileID order.
func (m *members) Each(yield func(model.FileVersion) error) error {
	return m.run.Each(func(fv model.FileVersion) error {
		if !m.unit.Contains(fv.Path) {
			return nil
		}
		return yield(fv)
	})
}

// Len is the member count.
func (m *members) Len() int64 { return m.n }

// Close releases the sorted run.
func (m *members) Close() error { return m.run.Close() }

// planMembers is the unit the request names, planned from the pinned snapshot
// as the planner planned it, with its members. Planning from the pinned
// snapshot is what makes the members the ones the unit's inputs were keyed
// on; a scope the plan does not hold, or one that owns no member, is a
// disagreement with the planner and fails rather than being guessed at.
//
// The manifest is read once (dependence.PlanUnit). It is served in path order
// and a FileID is a digest, so the order is an external sort that spills into
// dir rather than a list held in the heap.
func planMembers(ctx context.Context, req provider.UnitRequest, dir string) (*members, error) {
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
	pu, ok, err := dependence.PlanUnit(ctx, req.Content, req.Unit.ScopeKey, s.Add)
	if err != nil {
		return nil, err
	}
	run, err := s.Sorted()
	if err != nil {
		return nil, err
	}
	m := &members{unit: pu, run: run}
	if ok {
		err = m.run.Each(func(fv model.FileVersion) error {
			if pu.Contains(fv.Path) {
				m.n++
			}
			return nil
		})
	}
	if err != nil || m.n == 0 {
		m.Close()
		if err == nil {
			err = &model.Error{Code: model.CodeArgumentInvalid,
				Message: "the linking provider has no unit for this scope in this snapshot",
				Details: map[string]string{"scope_key": req.Unit.ScopeKey}}
		}
		return nil, err
	}
	return m, nil
}

// coverage is what the unit's tables leave out: members with no table, whose
// sites the linker cannot see, and the worker entries their parent dropped.
type coverage struct {
	untabled int64
	dropped  int64
}
