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
// The rule is the one the dependence provider's unit planner uses, for the
// same reason: a project is what the language's own toolchain calls a
// project, and an indexer run at the wrong boundary resolves a different set
// of symbols.
//
//   - Every triggering directory is a project, including the workspace root
//     when the root itself triggers. The root is not privileged and it is not
//     excluded: a repository with a root `package.json` and an `app/`
//     `package.json` has two projects, and each is indexed by its own unit.
//   - Nesting: Go modules and Maven or Gradle modules nest -- a `go.mod`
//     inside another module's tree is its own project, and the nearest
//     trigger above a file is the project that owns it. Every other kind
//     takes the outermost trigger and never splits below it, because the
//     whole program is what makes its symbols resolvable: a `tsconfig`
//     project split by subdirectory loses more than half of the calls that
//     resolve to its own methods (docs/research/10-round3-empirical.md
//     Section 8), and a Python package, a Cargo workspace and a compilation
//     database are whole for the same reason.
//   - A trigger inside a dependency directory is not a project. Those
//     directories -- `node_modules`, `vendor`, a virtual environment, a build
//     tree -- are not part of the snapshot at all (internal/workspace/walk.go
//     and internal/snapshot/policy.go exclude them), so a `package.json`
//     under `node_modules` never reaches this rule: nothing indexes a
//     dependency's own manifest as a project of the repository.

import (
	"slices"
	"strings"
)

// nestedKinds are the kinds whose projects nest inside one another. It mirrors
// the dependence planner's nestedFamilies, which is the same ruling about the
// same toolchains.
var nestedKinds = map[Kind]bool{KindGo: true, KindJava: true}

// projectRoots turns the trigger paths detection recognized for one kind into
// its project directories, sorted, with the workspace root spelled as the
// empty string. A directory holding two triggers of one kind is one project.
func projectRoots(k Kind, triggers []string) []string {
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
	if !nestedKinds[k] {
		dirs = slices.DeleteFunc(dirs, func(d string) bool {
			for _, outer := range dirs {
				if outer != d && within(d, outer) {
					return true
				}
			}
			return false
		})
	}
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

// within reports whether p lies at or under the root-relative directory dir;
// the empty dir is the workspace root and contains everything.
func within(p, dir string) bool {
	if dir == "" {
		return true
	}
	return p == dir || strings.HasPrefix(p, dir+"/")
}

// ownerOf is the project of kind k that owns a root-relative path: the nearest
// root at or above it, or no root at all when none encloses it. It is what
// makes a nested Go or Maven module's files members of the nested unit rather
// than of the one above it.
func ownerOf(roots []string, p string) (string, bool) {
	best, found := "", false
	for _, r := range roots {
		if within(p, r) && (!found || len(r) > len(best)) {
			best, found = r, true
		}
	}
	return best, found
}
