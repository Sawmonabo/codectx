package plan

// Per-provider scope derivation. The registry is closed -- the composition
// root builds it from this product's own providers -- so the planner dispatches
// on descriptor identity rather than inventing a plugin interface no provider
// implements. A provider the planner does not know is a typed error and never
// a silent omission: a registered provider that plans no unit is a capability
// that answers nothing while status reports the generation fresh.

import (
	"context"

	"github.com/Sawmonabo/codectx/internal/admission"
	"github.com/Sawmonabo/codectx/internal/model"
	"github.com/Sawmonabo/codectx/internal/provider"
	"github.com/Sawmonabo/codectx/internal/provider/filesystem"
	"github.com/Sawmonabo/codectx/internal/provider/link"
	"github.com/Sawmonabo/codectx/internal/provider/manifest"
	"github.com/Sawmonabo/codectx/internal/provider/scip"
	tslang "github.com/Sawmonabo/codectx/internal/provider/treesitter/lang"
)

// languageProvider is the tree-sitter provider's own eligibility answer: the
// pinned language for a manifest row, or none. It is taken as an interface so
// the planner depends on the question, not on the concrete type.
type languageProvider interface {
	LanguageOf(model.FileVersion) (tslang.Language, bool)
}

// scoper is the SCIP provider's own answer to which unit scope keys it builds
// for a detection. Only it has one: its scopes are the supplied index and the
// approved profiles whose triggers detection recognized, neither of which can
// be derived from the snapshot manifest (the import path and the resolvable
// payloads are the provider's private state).
type scoper interface {
	Scopes(provider.Detection) []string
}

// fileGate reports whether a file-invalidated provider builds a unit for one
// snapshot file. A deleted file is never offered: a tombstone has no bytes to
// index, and the planner refuses it before any gate sees it.
type fileGate func(model.FileVersion) bool

// fileGateFor returns the per-file predicate of a file-invalidated provider.
func fileGateFor(p provider.Provider) (fileGate, error) {
	d := p.Descriptor()
	switch d.ID {
	case filesystem.ID:
		// Every retained file has a filesystem unit: the provider classifies
		// and records whatever it is handed.
		return func(model.FileVersion) bool { return true }, nil
	case manifest.ID:
		return func(fv model.FileVersion) bool { return manifest.Recognize(fv.Path) }, nil
	case tslang.ProviderID:
		lp, ok := p.(languageProvider)
		if !ok {
			return nil, internalErr("the structural provider does not answer which files it can parse")
		}
		return func(fv model.FileVersion) bool {
			_, ok := lp.LanguageOf(fv)
			return ok
		}, nil
	}
	return nil, internalErr("the planner does not know which files provider " + d.ID + " builds units for")
}

// semanticScopes returns the scope keys of a provider whose unit is larger
// than a file, together with the membership predicate that decides which
// snapshot files each one declares as input.
func semanticScopes(ctx context.Context, p provider.Provider, det provider.Detection,
	view model.SnapshotView) ([]semantic, map[string]int, error) {
	d := p.Descriptor()
	switch d.ID {
	case scip.ID:
		sc, ok := p.(scoper)
		if !ok {
			return nil, nil, internalErr("the scip provider does not answer which unit scopes it builds")
		}
		var out []semantic
		for _, key := range sc.Scopes(det) {
			// Which of the two scope spellings a key is, and what a profile
			// reserves, are both the scip package's answers: the planner
			// parses no key. A key with no profile reservation is a supplied
			// index.
			mem, disk, isProfile := scip.ProfileReservation(key)
			if !isProfile {
				// A supplied index reads exactly one file: the index itself.
				// It is recognized by rebuilding the scope key from each
				// candidate path, for the same reason.
				out = append(out, semantic{providerID: d.ID, scopeKey: key,
					contains: func(fv model.FileVersion) bool { return scip.ImportScope(fv.Path) == key }})
				continue
			}
			// A profile runs an indexer over the whole workspace, which is what
			// its workspace invalidation scope declares; every retained file is
			// an input. Its indexer is a heavy child, so the unit carries the
			// memory and temporary disk the run reserves, for admission on the
			// process's one ledger.
			out = append(out, semantic{providerID: d.ID, scopeKey: key, allFiles: true,
				admit:    admission.Reservation{MemoryBytes: mem, DiskBytes: disk},
				contains: func(model.FileVersion) bool { return true }})
		}
		return out, nil, nil
	case link.ProviderID:
		// The linking provider's units are its own planner's projects
		// (link.PlanUnits), with their membership: a project's member files
		// are what its calls resolve across. A unit declares its project's
		// closure and nothing else -- the files of its own family it owns,
		// plus the manifest and lock files it owns -- so a documentation edit
		// or another language's source never rebuilds it (Section 13.1).
		// link.Unit.OwnsInput is that closure; FamilyOf maps both "c" and
		// "cpp" to one family inside it, so the C/C++ unit keeps both
		// languages' sources. It runs no heavy child.
		//
		// A project the planner refused as a unit of its own is linked with
		// the unit that encloses it, which loses no call, but it is still a
		// boundary the plan drew coarser than the repository's own; it is
		// counted per family so the degradation is never silent.
		dp, err := link.PlanUnits(ctx, view)
		if err != nil {
			return nil, nil, err
		}
		out := make([]semantic, 0, len(dp.Units))
		for _, u := range dp.Units {
			out = append(out, semantic{providerID: d.ID, scopeKey: u.ScopeKey, family: u.Family,
				contains: func(fv model.FileVersion) bool { return u.OwnsInput(fv.Path) }})
		}
		var unplanned map[string]int
		for f, n := range dp.Unplanned {
			if unplanned == nil {
				unplanned = map[string]int{}
			}
			unplanned[unplannedProjects+string(f)] = n
		}
		return out, unplanned, nil
	}
	return nil, nil, internalErr("the planner does not know which unit scopes provider " + d.ID + " builds")
}

// semantic is one planned unit larger than a file, before its inputs are
// folded. contains is the membership predicate over the snapshot manifest.
type semantic struct {
	providerID string
	scopeKey   string
	// allFiles marks a unit whose membership is the whole snapshot, so the
	// planner can share one sorted input slice rather than copying it per unit.
	allFiles bool
	// family is the linking unit's language family, which decides its
	// synthetic dependency keys (builder.linkKeys); empty for every other unit.
	family link.Family
	// admit is what an external-indexer profile unit reserves in both
	// dimensions while it runs; it is set only for a profile scope, and zero
	// for every other unit.
	admit    admission.Reservation
	version  string
	contains func(model.FileVersion) bool
	// dependencyKeys are the unit's synthetic dependency keys
	// (Unit.DependencyKeys).
	dependencyKeys []string

	inputs []model.UnitInput
}

func internalErr(msg string) error {
	return &model.Error{Code: model.CodeInternal, Message: msg}
}
