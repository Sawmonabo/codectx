package plan

// Per-provider scope derivation. The registry is closed -- the composition
// root builds it from this product's own providers -- so the planner dispatches
// on descriptor identity rather than inventing a plugin interface no provider
// implements. A provider the planner does not know is a typed error and never
// a silent omission: a registered provider that plans no unit is a capability
// that answers nothing while status reports the generation fresh.

import (
	"context"

	"github.com/Sawmonabo/codectx/internal/model"
	"github.com/Sawmonabo/codectx/internal/provider"
	"github.com/Sawmonabo/codectx/internal/provider/dependence"
	"github.com/Sawmonabo/codectx/internal/provider/filesystem"
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
			if !isProfileScope(key) {
				// A supplied index reads exactly one file: the index itself.
				// It is recognized by rebuilding the scope key from each
				// candidate path rather than by parsing the key, because the
				// prefix that spells it is the scip package's own.
				out = append(out, semantic{providerID: d.ID, scopeKey: key,
					contains: func(fv model.FileVersion) bool { return scip.ImportScope(fv.Path) == key }})
				continue
			}
			// A profile runs an indexer over the whole workspace, which is what
			// its workspace invalidation scope declares; every retained file is
			// an input.
			out = append(out, semantic{providerID: d.ID, scopeKey: key, allFiles: true,
				contains: func(model.FileVersion) bool { return true }})
		}
		return out, nil, nil
	case dependence.ProviderID:
		dp, err := dependence.PlanUnits(ctx, view)
		if err != nil {
			return nil, nil, err
		}
		out := make([]semantic, 0, len(dp.Units))
		for _, u := range dp.Units {
			// Residual 109: a dependence unit declares its semantic closure and
			// nothing else -- the files of its own family that it owns, plus
			// the manifest and lock files it owns (Section 11.6: "the unit's
			// source file hashes, its manifest and lock files"). Every file
			// under the root would fold a README, an image and another
			// language's sources into the key of the heaviest unit in the
			// product, so a documentation edit would reparse it; Section 13.1
			// forbids exactly that.
			//
			// dependence.Unit.OwnsInput is that closure, and it is the same
			// predicate the provider's own cache key folds, so a unit's
			// planned identity and its graph cache key cannot drift apart. It
			// answers for a tombstone as well as a live row, which is what
			// lets a deleted member set semantic.removed and keep the scope
			// out of Plan.Carry (Section 13.3). FamilyOf maps both "c" and
			// "cpp" to one family inside it, so the whole-repository C/C++
			// unit keeps both languages' sources.
			out = append(out, semantic{providerID: d.ID, scopeKey: u.ScopeKey, heavy: true,
				family: u.Family, bytes: u.Bytes,
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

// isProfileScope reports whether a scip scope key names an indexer profile
// rather than a supplied index, without parsing the key: the scip package owns
// both spellings, and rebuilding the key from each known kind and the project
// root the key claims is the only comparison that cannot drift from it.
func isProfileScope(key string) bool {
	for _, k := range scip.Kinds {
		if root, ok := scip.ProfileRoot(key, k); ok && scip.ProfileScope(string(k), root) == key {
			return true
		}
	}
	return false
}

// semantic is one planned unit larger than a file, before its inputs are
// folded. contains is the membership predicate over the snapshot manifest.
type semantic struct {
	providerID string
	scopeKey   string
	// allFiles marks a unit whose membership is the whole snapshot, so the
	// planner can share one sorted input slice rather than copying it per unit.
	allFiles bool
	heavy    bool
	family   dependence.Family
	// bytes is the source byte count the provider's own planner folded for
	// this unit, which is the figure the family heap estimate was measured
	// against; the unit's declared inputs are a larger set.
	bytes    int64
	version  string
	contains func(model.FileVersion) bool

	inputs []model.UnitInput
	// changed counts the member files whose content differs from the previous
	// generation, and removed records that a member the previous generation
	// indexed is now a tombstone -- which Section 13.3 makes ineligible for
	// carry, because a carried unit must not answer about source that is gone.
	changed int
	removed bool
}

func internalErr(msg string) error {
	return &model.Error{Code: model.CodeInternal, Message: msg}
}
