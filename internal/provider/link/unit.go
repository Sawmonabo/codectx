package link

import (
	"cmp"
	"context"
	"encoding/json"
	"strings"

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
	// root is the project's root-relative directory (Unit.Root),
	// the empty string the repository root, and content the pinned snapshot
	// its manifests are read from (a Go module's go.mod, which decides which
	// import paths are the project's own).
	root    string
	content model.SnapshotView
	// family is the unit's language family; a Rust unit reads its crates'
	// names from their Cargo.toml (rustCrates).
	family Family
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

// Fact is what a Binding states. Every fact is about one entry of the
// member Site.File names, and is published from that member's file: the
// site's, the import's, or the declaration's.
type Fact uint8

const (
	// FactCall: call site Site calls the declaration Target, or, when
	// External is set, the external method it names (externalMethod).
	// Dispatch is the occurrence's own: DispatchVirtual when the call can run
	// an override of what it names, DispatchExact when it cannot (`super`, a
	// qualified call, a constructed object, a final, static, private or
	// non-virtual method, a function). A site with several call facts
	// publishes one `calls` per distinct target, each with set_size the
	// number of distinct targets.
	FactCall Fact = 0
	// FactExternalSite: call site Site is a proven external call that no
	// edge names (an import, a module or a receiver type outside the
	// project, without a method node to target), counted as external.
	FactExternalSite Fact = 1
	// FactOpenSite: call site Site is bound by nothing, for Reason. A site
	// with no fact at all is open too, for the reason its shape gives
	// (openReason); a resolver emits this fact only to say otherwise.
	FactOpenSite Fact = 2
	// FactImport: the import Site (an index into Table.Imports) resolves to
	// the member file Target.File, whose module it imports (Target.Index is
	// -1). An import that resolves to several files has one fact per file.
	FactImport Fact = 3
	// FactFlow: argument Arg of call site Site flows into parameter Param
	// (an index into Target's Decl.Parameters) of the declaration Target the
	// site is bound to (argumentFlows). Only a bound site has one.
	FactFlow Fact = 4
	// FactExtends: the type declaration Site has the declared supertype
	// Target, or the external type External names (externalType).
	FactExtends Fact = 5
	// FactImplements: the type declaration Site implements the interface or
	// trait Target or External: declared, or, in Go, satisfied by its method
	// set.
	FactImplements Fact = 6
	// FactOverrides: the method declaration Site directly overrides or
	// implements the method Target, or the external method External names.
	FactOverrides Fact = 7
)

// Binding is one fact a resolver found. Site names the entry of a member's
// table the fact is about (a call site, an import, a declaration, by Kind),
// Target a declaration or a member's module of the unit, and External,
// instead of Target, a type or method outside the project.
type Binding struct {
	Kind   Fact
	Site   Ref
	Target Ref
	// Detail is the technique that found it: one of the Detail constants.
	Detail string
	// Dispatch is a FactCall's per-occurrence dispatch, empty for every
	// other kind.
	Dispatch model.Dispatch
	// Arg and Param are a FactFlow's argument index at the site and
	// parameter index in the target's Decl.Parameters.
	Arg, Param int32
	// External is the external type or method a FactCall, FactExtends,
	// FactImplements or FactOverrides targets (externalType,
	// externalMethod); Target is then unused.
	External string
	// Reason is a FactOpenSite's reason.
	Reason model.OpenSiteReason
}

// A resolver resolves every site of one unit and emits each fact it finds,
// in any order and with repeats; the core sorts them by site and target and
// publishes one occurrence per fact. A call site with no call fact
// publishes an open-site node and the caller's `calls` edge to it, unless a
// FactExternalSite says it is a proven external call.
type resolver func(ctx context.Context, u *unit, emit func(Binding) error) error

// resolverFor is the flow family's resolution of decision 2 (resolve has
// the static family's).
func resolverFor(f Family) resolver {
	if f == FamilyPython {
		return resolvePython
	}
	return resolveECMAScript
}

// members is a unit's member files in ascending FileID order: each live
// source file of the unit's own family that the unit owns. Manifests and lock
// files are inputs of the unit's closure but carry no table, so they are not
// members.
type members struct {
	unit Unit
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

// Close releases the sorted run.
func (m *members) Close() error { return m.run.Close() }

// planMembers is the unit the request names, planned from the pinned snapshot
// as the planner planned it, with its members. Planning from the pinned
// snapshot is what makes the members the ones the unit's inputs were keyed
// on; a scope the plan does not hold, or one that owns no member, is a
// disagreement with the planner and fails rather than being guessed at.
//
// The manifest is read once (PlanUnit). It is served in path order
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
	pu, ok, err := PlanUnit(ctx, req.Content, req.Unit.ScopeKey, s.Add)
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

// coverage is what the unit's facts leave out: members with no table, whose
// sites the linker cannot see, the worker entries their parent dropped, and
// the ambiguous sites whose call-site key does not fit a native key.
type coverage struct {
	untabled int64
	dropped  int64
	unkeyed  int64
}
