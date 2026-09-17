package scip

// Which directories of a workspace are projects a precise indexer runs over.
//
// A repository does not keep its projects at its root. The measured case is a
// monorepo whose every `package.json` sits in a subdirectory: checking the
// root alone planned no unit at all, and `precise_definitions`,
// `precise_references` and `precise_implementations` were unavailable for the
// whole repository although six indexers were installed and every one of them
// had a project to run over.
//
// A project is what the language's own toolchain calls a project, because an
// indexer run at the wrong boundary resolves a different set of symbols. The
// dependence provider's unit planner answers the same question and does not
// answer it the same way: it still folds a non-nesting family's inner
// directories into the outermost one (internal/provider/dependence/units.go,
// nestedFamilies), so that planner's javascript, python and rust units are
// coarser than the projects below. The two are deliberately read as separate
// rulings here and not shared, so neither drifts by inheriting the other.
//
//   - Every triggering directory is a project, whatever the kind and whatever
//     encloses it. The workspace root is not privileged and it is not
//     excluded, and neither is a directory inside another project's tree: a
//     repository with a root `package.json` and an `app/package.json` has two
//     projects, a `go.mod` inside another module's tree is its own project,
//     and each is indexed by its own unit. A manifest is the toolchain's own
//     declaration of a boundary, so a nested one is a second program and not
//     a subdirectory of the first; indexing its files at the boundary above
//     runs the indexer over a program its toolchain never assembles and
//     resolves a different set of symbols for them.
//   - A directory that declares nothing is never a boundary, which is the
//     split the empirical round measured: cutting one `tsconfig` project at
//     subdirectories holding no `tsconfig` of their own loses more than half
//     of the calls that resolve to the project's own methods
//     (docs/research/10-round3-empirical.md Section 8). A Python package, a
//     Cargo workspace and a compilation database are whole for the same
//     reason -- whole up to the next directory that declares itself, and no
//     further.
//   - A trigger inside a dependency directory is a project only when the
//     workspace holds that directory at all. With `workspace.index_vendor`
//     false, which is the default, `node_modules`, `vendor`, `third_party`,
//     `bower_components` and `Godeps` are outside the snapshot
//     (internal/workspace/walk.go), so a `package.json` under `node_modules`
//     never reaches this rule and nothing indexes a dependency's own manifest
//     as a project of the repository. With `workspace.index_vendor` true the
//     operator has asked for those directories to be indexed, their manifests
//     are in the detection, and they are projects like any other -- bounded,
//     as the whole detection is, by provider.MaxDetectionInputs.

import (
	"slices"
	"strings"
)

// projectRoots turns the trigger paths detection recognized for one kind into
// its project directories, sorted, with the workspace root spelled as the
// empty string. A directory holding two triggers of one kind is one project.
func projectRoots(triggers []string) []string {
	dirs := make([]string, 0, len(triggers))
	for _, t := range triggers {
		dir := ""
		if i := strings.LastIndexByte(t, '/'); i >= 0 {
			dir = t[:i]
		}
		if !slices.Contains(dirs, dir) {
			dirs = append(dirs, dir)
		}
	}
	slices.Sort(dirs)
	return dirs
}

// triggerKind maps a manifest's base name to the kind it triggers. The six
// trigger sets are disjoint, so one name answers one kind; it is built from
// kindSpecs so it cannot drift from the triggers themselves.
var triggerKind = func() map[string]Kind {
	out := map[string]Kind{}
	for _, k := range Kinds {
		for _, t := range kindSpecs[k].triggers {
			out[t] = k
		}
	}
	return out
}()
