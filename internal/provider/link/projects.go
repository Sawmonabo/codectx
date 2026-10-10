package link

// Project planning (Section 11.6). A linking unit is one project of the
// repository: a Go module, a Maven or Gradle module, a `package.json`/
// `tsconfig.json` project, a Python package, a Cargo workspace. C and C++ are
// one unit per repository, because header resolution spans the whole tree and
// a call into a header can land on any file of it.
//
// The boundaries are the ones linking needs, not preferences. A call resolves
// only to a declaration the unit holds, so a unit is the set of files whose
// calls resolve across each other. A TypeScript project is never split below
// its own manifest: a receiver's type flows from anywhere in the program, and a
// unit cut at a directory with no manifest of its own leaves the call
// unresolved for want of the declaration on the other side of the cut. Python
// units are packages, which hold every method a package's calls name and alias
// their cross-package calls by full name. Go units are modules found by walking
// `go.mod`, because a module is the unit of import resolution and a workspace
// root names only some of its modules. Rust is the Cargo workspace root; a
// directory of `.rs` files without a `Cargo.toml` is no crate and so no
// project at all.
//
// A plan holds one descriptor per project and no repository-sized list at all
// (Section 6): membership is a predicate over the project root, the nested
// roots the unit does not own, and its family's source languages, manifest
// names and lock names (Unit.OwnsInput), and file counts are folded in a
// streaming pass. The manifest half of the closure is that predicate and
// not a stored list of marker paths, so nothing here grows with the number of
// directories holding a go.sum.

import (
	"context"
	"log/slog"
	"path"
	"slices"
	"strings"

	srclang "github.com/Sawmonabo/codectx/internal/lang"
	"github.com/Sawmonabo/codectx/internal/model"
	"github.com/Sawmonabo/codectx/internal/provider"
)

// component is the slog component every entry of this package carries.
const component = "provider.link"

// Family is the set of languages one project links together. The nine
// supported languages collapse onto six families: the languages of one family
// share a project and its manifests.
type Family string

const (
	// FamilyC covers C and C++. Header resolution spans the whole tree, so it
	// is the one family whose unit is the repository (Section 11.6).
	FamilyC Family = "c"
	// FamilyGo covers Go.
	FamilyGo Family = "go"
	// FamilyJava covers Java.
	FamilyJava Family = "java"
	// FamilyJavaScript covers JavaScript, TypeScript and TSX, which share one
	// project.
	FamilyJavaScript Family = "javascript"
	// FamilyPython covers Python.
	FamilyPython Family = "python"
	// FamilyRust covers Rust.
	FamilyRust Family = "rust"
)

// families is every family in a fixed order, so a plan is a function of the
// repository alone.
var families = []Family{FamilyC, FamilyGo, FamilyJava, FamilyJavaScript, FamilyPython, FamilyRust}

// FamilyOf maps a language tag to the family whose project covers it, or an
// empty family for a language the linking provider does not link.
func FamilyOf(language string) Family {
	switch language {
	case "go":
		return FamilyGo
	case "javascript", "typescript", "tsx":
		return FamilyJavaScript
	case "python":
		return FamilyPython
	case "java":
		return FamilyJava
	case "c", "cpp":
		return FamilyC
	case "rust":
		return FamilyRust
	}
	return ""
}

// Unit is one planned project.
type Unit struct {
	// ScopeKey is `pkg:<family>:<root>` for a project unit and `workspace` for
	// the C/C++ unit, which is the whole repository.
	ScopeKey string
	Family   Family
	// Root is the root-relative project directory; the empty string is the
	// repository root.
	Root string
	// Excluded are the root-relative directories of nested projects of the
	// same family that this unit does not own. They are never members of this
	// unit, so a nested module is linked once, by its own unit.
	Excluded []string
	// Files counts the unit's own source files; a unit with no files is never
	// planned.
	Files int64
}

// markers names the manifest, lock and project files of a family. The first
// group defines a project root; the second only joins the unit's closure.
var (
	projectMarkers = map[Family][]string{
		FamilyGo:         {"go.mod"},
		FamilyJava:       {"pom.xml", "build.gradle", "build.gradle.kts"},
		FamilyJavaScript: {"tsconfig.json", "jsconfig.json", "package.json"},
		FamilyPython:     {"pyproject.toml", "setup.py", "setup.cfg"},
		FamilyRust:       {"Cargo.toml"},
	}
	closureMarkers = map[Family][]string{
		FamilyC:          {"compile_commands.json", "CMakeLists.txt", "Makefile"},
		FamilyGo:         {"go.sum"},
		FamilyJava:       {"settings.gradle", "settings.gradle.kts", "gradle.lockfile"},
		FamilyJavaScript: {"package-lock.json", "yarn.lock", "pnpm-lock.yaml"},
		FamilyPython:     {"requirements.txt", "requirements-dev.txt", "poetry.lock", "Pipfile.lock"},
		FamilyRust:       {"Cargo.lock"},
	}
	// nestedFamilies own a project per nested marker: a Go module and a Maven
	// or Gradle module inside another one is its own unit. Every other family
	// takes the outermost marker and never splits below it.
	nestedFamilies = map[Family]bool{FamilyGo: true, FamilyJava: true}
)

// Plan is one snapshot's project units together with what could not be
// planned as a unit of its own. A refused project is not a silent omission:
// its files are members of the unit enclosing them, at a coarser boundary
// than they own, and Unplanned is how a consumer knows that.
type Plan struct {
	// Units are the planned units, sorted by scope key.
	Units []Unit
	// Unplanned counts, per family, the projects planFamily refused because
	// their scope key does not fit model.MaxScopeKeyBytes. Their files are
	// members of the unit that encloses them, so nothing is lost, but at a
	// coarser boundary than the project owns. It is a count and not the
	// paths: the only paths that can appear here are longer than two
	// kilobytes each, and a plan never holds a repository-sized list of them
	// (Section 6). The refusal's locus is on the warning this package logs.
	Unplanned map[Family]int
}

// PlanUnits derives every unit of the snapshot. It reads the manifest twice:
// once to find project roots and the files of the C/C++ unit, once to fold
// each unit's file count. Nothing repository-sized is retained.
func PlanUnits(ctx context.Context, view model.SnapshotView) (Plan, error) {
	roots := map[Family]map[string]bool{}
	present := map[Family]bool{}
	err := view.EachFile(ctx, model.FileSelection{}, func(fv model.FileVersion) error {
		if fv.Status == model.FileDeleted {
			return nil
		}
		dir, base := path.Split(fv.Path)
		dir = strings.TrimSuffix(dir, "/")
		if f := FamilyOf(srclang.Of(fv.Path)); f != "" {
			present[f] = true
		}
		for _, f := range families {
			if slices.Contains(projectMarkers[f], base) {
				if roots[f] == nil {
					roots[f] = map[string]bool{}
				}
				roots[f][dir] = true
			}
		}
		return nil
	})
	if err != nil {
		return Plan{}, err
	}

	plan := Plan{}
	for _, f := range families {
		if !present[f] {
			continue
		}
		units, unplanned := planFamily(f, roots[f])
		plan.Units = append(plan.Units, units...)
		if unplanned > 0 {
			if plan.Unplanned == nil {
				plan.Unplanned = map[Family]int{}
			}
			plan.Unplanned[f] = unplanned
		}
	}
	if err := foldFiles(ctx, view, plan.Units); err != nil {
		return Plan{}, err
	}
	plan.Units = slices.DeleteFunc(plan.Units, func(u Unit) bool { return u.Files == 0 })
	slices.SortFunc(plan.Units, func(a, b Unit) int { return strings.Compare(a.ScopeKey, b.ScopeKey) })
	return plan, nil
}

// planFamily turns one family's marker directories into unit descriptors and
// reports how many projects it had to refuse.
func planFamily(f Family, roots map[string]bool) ([]Unit, int) {
	if f == FamilyC {
		// Header resolution spans the tree, so C and C++ are the one family
		// whose unit is the repository itself.
		return []Unit{{ScopeKey: provider.ScopeWorkspace, Family: f}}, 0
	}
	dirs := make([]string, 0, len(roots))
	for d := range roots {
		dirs = append(dirs, d)
	}
	slices.Sort(dirs)
	if !nestedFamilies[f] {
		// Never split a project: a marker inside another project's directory
		// belongs to the outer project.
		//
		// The precise provider answers the same question the other way round:
		// there a nested manifest is its own project, whatever encloses it
		// (internal/provider/scip/projects.go). The divergence is deliberate,
		// and it is the difference between what the two lose. The precise
		// collapse cost located facts for a project nobody had planned a unit
		// for; this collapse keeps the outer project whole, and the outer
		// project is the program whose calls are linked across each other.
		// Every file of it is one unit, so a call from the nested directory
		// into the rest of the project, or back, resolves to the declaration
		// it names, which a separate unit for the nested directory could not
		// hold. Neither rule inherits the other.
		dirs = slices.DeleteFunc(dirs, func(d string) bool {
			for _, outer := range dirs {
				if outer != d && within(d, outer) {
					return true
				}
			}
			return false
		})
	}
	// How many projects one family holds is a property of the repository. A
	// monorepo is planned whole: every project gets its own unit, because
	// collapsing them into a coarser one would silently change which calls
	// resolve, and refusing the plan would refuse the repository.
	//
	// A project whose scope key does not fit is refused as a project, not as
	// source. Refusing here is what keeps the failure in the planner: the unit
	// would otherwise be admitted and fail at BeginUnit, where the scope key is
	// already the identity every published row carries.
	//
	// Only the projects that survive bound the other units, so a refused
	// directory is excluded from nothing and its files fall back to the unit
	// that encloses it — the enclosing project, or the family's
	// repository-root unit. No file of a family is ever orphaned — except
	// loose Rust source outside every crate, which belongs to no project (see
	// the FamilyRust guard below). It is still a degradation: that source is
	// linked at a coarser project boundary than it owns, with the
	// neighbouring projects' files around it, and the refusal is counted
	// rather than swallowed. The path is logged truncated because it is what
	// locates the project for an operator and a whole one is over two
	// kilobytes.
	kept := make([]string, 0, len(dirs))
	unplanned := 0
	for _, d := range dirs {
		if _, ok := scopeKey(f, d); !ok {
			unplanned++
			bounded, _ := model.TruncateField(d, model.MaxIdentifierBytes)
			slog.Warn("a project is not planned as its own unit: its scope key exceeds the identity bound",
				"component", component, "family", string(f), "path", bounded,
				"path_bytes", len(d), "bound", model.MaxScopeKeyBytes)
			continue
		}
		kept = append(kept, d)
	}
	units := make([]Unit, 0, len(kept)+1)
	for _, d := range kept {
		key, _ := scopeKey(f, d)
		units = append(units, Unit{ScopeKey: key, Family: f, Root: d,
			Excluded: nested(d, kept)})
	}
	if f != FamilyRust && !slices.Contains(kept, "") {
		// Source of this family outside every project still has calls worth
		// linking, so the family's repository root is a unit of its own. Rust
		// is not: a crate is declared by its Cargo.toml, and loose Rust files
		// outside every crate belong to no project, so they are left unlinked
		// rather than published as a unit no crate declares.
		//
		// A project rooted at the repository root already owns this scope key.
		// Emitting a second unit under the same key would leave a lookup by
		// scope key choosing between them by sort position, so it is not
		// emitted at all. The root key is "pkg:<family>:", always within the
		// bound.
		//
		// Excluded is every project of the family that is planned as its own
		// unit, and only those: a refused directory is not excluded here, so
		// its files — and its manifests, which belong in this unit's semantic
		// closure now that it owns them — fall back to this unit rather than
		// being linked by nobody.
		key, _ := scopeKey(f, "")
		units = append(units, Unit{ScopeKey: key, Family: f, Excluded: kept})
	}
	return units, unplanned
}

// foldFiles counts each unit's own files in one streaming pass.
func foldFiles(ctx context.Context, view model.SnapshotView, plan []Unit) error {
	return view.EachFile(ctx, model.FileSelection{}, func(fv model.FileVersion) error {
		f := FamilyOf(srclang.Of(fv.Path))
		if f == "" || fv.Status == model.FileDeleted {
			return nil
		}
		for i := range plan {
			if plan[i].Family == f && plan[i].Contains(fv.Path) {
				plan[i].Files++
				return nil
			}
		}
		return nil
	})
}

// Contains reports whether path belongs to this unit: under its root and not
// under a nested project it does not own.
func (u Unit) Contains(p string) bool {
	if !within(p, u.Root) {
		return false
	}
	for _, ex := range u.Excluded {
		if within(p, ex) {
			return false
		}
	}
	return true
}

// OwnsInput reports whether path is part of this unit's semantic closure
// (Section 11.6): a source file of its own family that it owns, or a manifest
// or lock file it owns. It is the manifest half of the unit's declared
// inputs, which is exactly "any of these changing invalidates the unit".
//
// It is a predicate over the path and not a lookup in a stored list of marker
// paths, so it holds nothing that grows with the repository (Section 6): the
// C/C++ unit is the whole tree, and a list would hold every Makefile in it.
func (u Unit) OwnsInput(p string) bool {
	if !u.Contains(p) {
		return false
	}
	if FamilyOf(srclang.Of(p)) == u.Family {
		return true
	}
	base := path.Base(p)
	return slices.Contains(projectMarkers[u.Family], base) || slices.Contains(closureMarkers[u.Family], base)
}

// ScopeKeyPrefix is what every scope key of a family begins with, and the one
// place the family's position in that key is spelled. A caller holding a scope
// key and a family -- the planner matching this repository's recorded history
// to the language it was recorded for -- tests it against this rather than
// rebuilding the key format for itself, so the format cannot drift between the
// package that writes it and the package that reads it.
//
// It ends in the separator, so a prefix test can never match a family whose
// name merely starts with another's. The whole-family unit's own key is
// exactly this prefix, with an empty root.
func ScopeKeyPrefix(f Family) string { return "pkg:" + string(f) + ":" }

// scopeKey is the unit's capability and alias scope, named by its family.
//
// A key that does not fit model.MaxScopeKeyBytes is refused, not truncated.
// Truncation is a colliding identity claim: two deep project directories with
// a long common prefix would cut to the same key, and every capability row,
// alias scope and stored resolution of one would then be attributed to the
// other. The planner drops such a project instead, which loses one project's
// boundary openly rather than mixing two projects' facts silently.
func scopeKey(f Family, root string) (string, bool) {
	key := ScopeKeyPrefix(f) + root
	if len(key) > model.MaxScopeKeyBytes {
		return "", false
	}
	return key, true
}

// within reports whether p lies at or under the root-relative directory dir;
// the empty dir is the repository root and contains everything.
func within(p, dir string) bool {
	if dir == "" {
		return true
	}
	return p == dir || strings.HasPrefix(p, dir+"/")
}

// nested returns the directories of dirs that lie strictly under d.
func nested(d string, dirs []string) []string {
	var out []string
	for _, o := range dirs {
		if o != d && within(o, d) {
			out = append(out, o)
		}
	}
	return out
}

// PlanUnit plans the one unit scopeKey names, exactly as PlanUnits plans it,
// from a single read of the manifest: the family's project roots are
// collected, and every live source file of the family under the unit's root
// is handed to candidate as it is read, before the unit's excluded nested
// projects are known, so the caller keeps the ones Unit.Contains admits.
// Files is left zero. It reports false when the scope names no
// project the snapshot plans: a key of no family, a root with no marker, or a
// project refused or folded into an enclosing one.
//
// It is how one unit finds its members without a second planning of the
// whole snapshot: PlanUnits reads the manifest twice for every family, and a
// unit that only needs its own membership pays for none of that.
func PlanUnit(ctx context.Context, view model.SnapshotView, scopeKey string, candidate func(model.FileVersion) error) (Unit, bool, error) {
	f, root, ok := ParseScopeKey(scopeKey)
	if !ok {
		return Unit{}, false, nil
	}
	roots := map[string]bool{}
	err := view.EachFile(ctx, model.FileSelection{}, func(fv model.FileVersion) error {
		if fv.Status == model.FileDeleted {
			return nil
		}
		dir, base := path.Split(fv.Path)
		if slices.Contains(projectMarkers[f], base) {
			roots[strings.TrimSuffix(dir, "/")] = true
		}
		if FamilyOf(srclang.Of(fv.Path)) != f || !within(fv.Path, root) {
			return nil
		}
		return candidate(fv)
	})
	if err != nil {
		return Unit{}, false, err
	}
	units, _ := planFamily(f, roots)
	for _, u := range units {
		if u.ScopeKey == scopeKey {
			return u, true, nil
		}
	}
	return Unit{}, false, nil
}

// ParseScopeKey is the family and root a unit's scope key names (scopeKey),
// and false for a key no unit carries.
func ParseScopeKey(key string) (Family, string, bool) {
	if key == provider.ScopeWorkspace {
		return FamilyC, "", true
	}
	rest, ok := strings.CutPrefix(key, "pkg:")
	if !ok {
		return "", "", false
	}
	family, root, ok := strings.Cut(rest, ":")
	if !ok || !slices.Contains(families, Family(family)) || Family(family) == FamilyC {
		return "", "", false
	}
	return Family(family), root, true
}
