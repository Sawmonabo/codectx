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
//     (docs/research/10-engine-empirical.md Section 8). A Python package, a
//     Cargo workspace and a compilation database are whole for the same
//     reason -- whole up to the next directory that declares itself, and no
//     further.
//   - A nested project is excluded from the project that encloses it. The
//     outer unit's indexer still runs over the whole outer directory, because
//     no profile's argument array can exclude a subtree, so the importer drops
//     every document under a nested project root instead (nestedProject,
//     importer.seeDocument): one path is published by exactly one unit. A
//     nested directory whose scope key does not fit is not a project of its
//     own (Scopes refuses it), so its files stay with the unit that encloses
//     it; a project a truncated detection never planned is dropped from the
//     enclosing unit too, and the detection's `unplanned_projects` detail
//     already says those projects are not indexed precisely.
//   - A trigger inside a dependency directory is a project only when the
//     workspace holds that directory at all. With `workspace.index_vendor`
//     false, which is the default, `node_modules`, `vendor`, `third_party`,
//     `bower_components` and `Godeps` are outside the snapshot
//     (internal/workspace/walk.go), so a `package.json` under `node_modules`
//     never reaches this rule and nothing indexes a dependency's own manifest
//     as a project of the repository. Two things qualify that. The Git ignore
//     hook excludes an ignored directory whatever `index_vendor` says, so with
//     it true a gitignored `node_modules` is still not a project; and a
//     tracked path wins over both exclusions (Section 10.2), so a tracked
//     `third_party/lib/Cargo.toml` is a project with `index_vendor` false --
//     but detection sees it only when the policy it is handed carries the
//     capture's ForceInclude and ForceIncludeDir hooks, since the walk
//     descends into nothing else the policy excludes. With `index_vendor`
//     true and no ignore match, those directories' manifests are in the
//     detection and are projects like any other -- bounded, as the whole
//     detection is, by provider.MaxDetectionInputs.

import (
	"slices"
	"strings"

	"github.com/Sawmonabo/codectx/internal/model"
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

// plannable reports whether the project of kind k at dir gets a unit of its
// own: its scope key fits the identity bound. Scopes plans exactly these, and
// the importer excludes exactly these from an enclosing unit, so the two can
// never disagree about which unit publishes a path.
func plannable(k Kind, dir string) bool {
	return len(ProfileScope(string(k), dir)) <= model.MaxScopeKeyBytes
}

// nestedProject reports the project directory the file at rel roots when that
// directory is a plannable project of kind k strictly inside the project at
// root. Only a trigger of the same kind nests: a project of another kind is
// indexed by another indexer, whose documents never reach this unit.
func nestedProject(k Kind, root, rel string) (string, bool) {
	dir, base := "", rel
	if i := strings.LastIndexByte(rel, '/'); i >= 0 {
		dir, base = rel[:i], rel[i+1:]
	}
	if kind, ok := triggerKind[base]; !ok || kind != k || !strictlyInside(dir, root) {
		return "", false
	}
	return dir, plannable(k, dir)
}

// strictlyInside reports whether the root-relative directory dir lies below
// root and is not root itself; the empty root is the workspace root.
func strictlyInside(dir, root string) bool {
	if root == "" {
		return dir != ""
	}
	return strings.HasPrefix(dir, root+"/")
}
