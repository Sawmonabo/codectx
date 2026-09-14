package dependence

// Unit planning (Section 11.6). One parse handles exactly one language, so a
// unit is one frontend-native project: a Go module, a Maven or Gradle module,
// a `package.json`/`tsconfig.json` project, a Python package, a Cargo
// workspace. C and C++ are one unit per repository because header resolution
// spans the whole tree.
//
// The rules are the parity results of docs/research/10-round3-empirical.md
// Sections 5 to 8, not preferences. A TypeScript project is never split: the
// four-way split of one project kept 99.7% of control-dependence edges but
// only 46% of the calls that resolved to internal methods, because the
// frontend needs the whole program to type a receiver. Python units are
// packages, which keep every method and every dependence edge and alias their
// cross-package calls by full name. Go units are modules found by walking
// `go.mod`, because the frontend reads a `go.work` root module only and a
// whole-repository parse of a multi-module workspace covered 36 files. Rust is
// the Cargo workspace root; a directory of `.rs` files without a `Cargo.toml`
// produces an empty graph and is therefore not a unit at all.
//
// A plan holds one descriptor per project, never a repository-sized file list
// (Section 6): membership is a predicate over the project root and the nested
// roots it does not own, and file counts and bytes are folded in a streaming
// pass.

import (
	"context"
	"log/slog"
	"path"
	"slices"
	"strings"

	"github.com/Sawmonabo/codectx/internal/lang"
	"github.com/Sawmonabo/codectx/internal/model"
)

// MaxUnitsPerFamily bounds how many projects of one family a plan may hold.
// A repository with more is refused with a typed resource limit rather than
// collapsed into a coarser unit, because collapsing would silently change
// which facts the analysis can resolve.
const MaxUnitsPerFamily = 512

// maxMarkersPerUnit bounds the manifest and lock paths one unit folds into its
// cache key.
const maxMarkersPerUnit = 64

// Unit is one planned frontend-native project.
type Unit struct {
	// ScopeKey is `pkg:<family>:<root>` for a project unit and `workspace` for
	// the C/C++ unit, which is the whole repository.
	ScopeKey string
	Family   Family
	// Root is the root-relative project directory; the empty string is the
	// repository root.
	Root string
	// Excluded are the root-relative directories of nested projects of the
	// same family that this unit does not own. They are never materialized for
	// this unit, so a nested module is analysed once, by its own unit.
	Excluded []string
	// Markers are the manifest and lock files whose content is part of the
	// unit's semantic closure (Section 11.6), sorted.
	Markers []string
	// Files and Bytes are the unit's own source files and their byte count:
	// the governor sizes the heap cap from Bytes, and a unit with no files is
	// never scheduled.
	Files int64
	Bytes int64
}

// markers names the manifest, lock and project files of a family. The first
// group defines a project root; the second only contributes to the cache key.
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

// FamilyOf maps a language tag to the family whose single parse covers it, or
// an empty family for a language this provider does not analyse.
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

// Plan is one snapshot's dependence units together with what could not be
// planned as a unit of its own. A refused project is not a silent omission:
// its files are analysed by the unit enclosing them, at a coarser boundary
// than they own, so the family that owns it cannot be published fresh, and
// Unplanned is how the provider knows that at publication time.
type Plan struct {
	// Units are the planned units, sorted by scope key.
	Units []Unit
	// Unplanned counts, per family, the projects planFamily refused because
	// their scope key does not fit model.MaxScopeKeyBytes. Their files are
	// analysed by the unit that encloses them, so nothing is lost, but at a
	// coarser boundary than the project owns — which is what the family
	// publishes partial for. It is a count and not the paths: the only paths
	// that can appear here are longer than two kilobytes each, and a plan never
	// holds a repository-sized list of them (Section 6). The refusal's locus is
	// on the warning this package logs.
	Unplanned map[Family]int
}

// PlanUnits derives every unit of the snapshot. It reads the manifest twice:
// once to find project roots and the files of the C/C++ unit, once to fold
// each unit's file count and byte total. Nothing repository-sized is retained.
func PlanUnits(ctx context.Context, view model.SnapshotView) (Plan, error) {
	roots := map[Family]map[string]bool{}
	markers := map[Family]map[string][]string{}
	present := map[Family]bool{}
	err := view.EachFile(ctx, model.FileSelection{}, func(fv model.FileVersion) error {
		if fv.Status == model.FileDeleted {
			return nil
		}
		dir, base := path.Split(fv.Path)
		dir = strings.TrimSuffix(dir, "/")
		if f := FamilyOf(lang.Of(fv.Path)); f != "" {
			present[f] = true
		}
		for _, f := range Families {
			if slices.Contains(projectMarkers[f], base) {
				if roots[f] == nil {
					roots[f] = map[string]bool{}
				}
				roots[f][dir] = true
			}
			if slices.Contains(projectMarkers[f], base) || slices.Contains(closureMarkers[f], base) {
				if markers[f] == nil {
					markers[f] = map[string][]string{}
				}
				markers[f][dir] = append(markers[f][dir], fv.Path)
			}
		}
		return nil
	})
	if err != nil {
		return Plan{}, err
	}

	plan := Plan{}
	for _, f := range Families {
		if !present[f] {
			continue
		}
		units, unplanned, err := planFamily(f, roots[f], markers[f])
		if err != nil {
			return Plan{}, err
		}
		plan.Units = append(plan.Units, units...)
		if unplanned > 0 {
			if plan.Unplanned == nil {
				plan.Unplanned = map[Family]int{}
			}
			plan.Unplanned[f] = unplanned
		}
	}
	if err := foldSizes(ctx, view, plan.Units); err != nil {
		return Plan{}, err
	}
	plan.Units = slices.DeleteFunc(plan.Units, func(u Unit) bool { return u.Files == 0 })
	slices.SortFunc(plan.Units, func(a, b Unit) int { return strings.Compare(a.ScopeKey, b.ScopeKey) })
	return plan, nil
}

// planFamily turns one family's marker directories into unit descriptors and
// reports how many projects it had to refuse.
func planFamily(f Family, roots map[string]bool, markers map[string][]string) ([]Unit, int, error) {
	if f == FamilyC {
		// Header resolution spans the tree, so C and C++ are the one family
		// whose unit is the repository itself.
		return []Unit{{ScopeKey: ScopeWorkspace, Family: f, Markers: flatten(markers)}}, 0, nil
	}
	dirs := make([]string, 0, len(roots))
	for d := range roots {
		dirs = append(dirs, d)
	}
	slices.Sort(dirs)
	if !nestedFamilies[f] {
		// Never split a project: a marker inside another project's directory
		// belongs to the outer project.
		dirs = slices.DeleteFunc(dirs, func(d string) bool {
			for _, outer := range dirs {
				if outer != d && within(d, outer) {
					return true
				}
			}
			return false
		})
	}
	if len(dirs) > MaxUnitsPerFamily {
		return nil, 0, resourceLimit("the repository holds more dependence projects of one family than the plan bounds").
			WithDetail("family", string(f)).WithDetail("projects", itoa(int64(len(dirs)))).
			WithDetail("limit", "MaxUnitsPerFamily").WithDetail("bound", itoa(MaxUnitsPerFamily))
	}
	// A project whose scope key does not fit is refused as a project, not as
	// source. Refusing here is what keeps the failure in the planner: the unit
	// would otherwise be admitted and fail at BeginUnit, where the scope key is
	// already the identity every published row carries.
	//
	// Only the projects that survive bound the other units, so a refused
	// directory is excluded from nothing and its files fall back to the unit
	// that encloses it — the enclosing project, or the family's
	// repository-root unit. No file of a family is ever orphaned, which is
	// also what guarantees the degradation below always has a unit to be
	// published on. It is still a degradation: that source is analysed at a
	// coarser project boundary than it owns, with the neighbouring projects'
	// files around it, so the caller publishes the family partial and the
	// refusal is counted rather than swallowed. The path is logged truncated
	// because it is what locates the project for an operator and a whole one
	// is over two kilobytes.
	planned := make([]string, 0, len(dirs))
	unplanned := 0
	for _, d := range dirs {
		if _, ok := scopeKey(f, d); !ok {
			unplanned++
			slog.Warn("a dependence project is not planned as its own unit: its scope key exceeds the identity bound",
				"component", component, "family", string(f), "path", truncate(d, model.MaxIdentifierBytes),
				"path_bytes", len(d), "bound", model.MaxScopeKeyBytes)
			continue
		}
		planned = append(planned, d)
	}
	units := make([]Unit, 0, len(planned)+1)
	for _, d := range planned {
		key, _ := scopeKey(f, d)
		units = append(units, Unit{ScopeKey: key, Family: f, Root: d,
			Excluded: nested(d, planned), Markers: markersUnder(markers, d, nested(d, planned))})
	}
	if f != FamilyRust && !slices.Contains(planned, "") {
		// Source of this family outside every project still has facts worth
		// having; the engine parses a bare directory happily. Rust does not:
		// without a Cargo.toml its helper produces an empty graph, which would
		// be indistinguishable from a crashed helper, so loose Rust files are
		// left unanalysed rather than published as an empty unit.
		//
		// A project rooted at the repository root already owns this scope key.
		// Emitting a second unit under the same key would leave unitFor
		// choosing between them by sort position, so it is not emitted at all.
		// The root key is "pkg:<family>:", always within the bound.
		//
		// Excluded is every project of the family that is planned as its own
		// unit, and only those: a refused directory is not excluded here, so
		// its files — and its manifests, which belong in this unit's semantic
		// closure now that it owns them — fall back to this unit rather than
		// being analysed by nobody.
		key, _ := scopeKey(f, "")
		units = append(units, Unit{ScopeKey: key, Family: f,
			Excluded: planned, Markers: markersUnder(markers, "", planned)})
	}
	return units, unplanned, nil
}

// foldSizes counts each unit's own files and bytes in one streaming pass.
func foldSizes(ctx context.Context, view model.SnapshotView, plan []Unit) error {
	return view.EachFile(ctx, model.FileSelection{}, func(fv model.FileVersion) error {
		f := FamilyOf(lang.Of(fv.Path))
		if f == "" || fv.Status == model.FileDeleted {
			return nil
		}
		for i := range plan {
			if plan[i].Family == f && plan[i].Contains(fv.Path) {
				plan[i].Files++
				plan[i].Bytes += fv.Size
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

// scopeKey is the unit's capability and alias scope. It names the family in
// the product's own vocabulary, never the engine's frontend name.
//
// A key that does not fit model.MaxScopeKeyBytes is refused, not truncated.
// Truncation is a colliding identity claim: two deep project directories with
// a long common prefix would cut to the same key, and every capability row,
// alias scope and cache entry of one would then be attributed to the other.
// The planner drops such a project instead, which loses one project's facts
// openly rather than mixing two projects' facts silently.
func scopeKey(f Family, root string) (string, bool) {
	key := "pkg:" + string(f) + ":" + root
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

// markersUnder collects the manifest paths a unit owns, bounded and sorted.
func markersUnder(markers map[string][]string, root string, excluded []string) []string {
	var out []string
	dirs := make([]string, 0, len(markers))
	for d := range markers {
		dirs = append(dirs, d)
	}
	slices.Sort(dirs)
	for _, d := range dirs {
		if !within(d, root) {
			continue
		}
		skip := false
		for _, ex := range excluded {
			if within(d, ex) {
				skip = true
				break
			}
		}
		if skip {
			continue
		}
		out = append(out, markers[d]...)
	}
	slices.Sort(out)
	if len(out) > maxMarkersPerUnit {
		out = out[:maxMarkersPerUnit]
	}
	return out
}

// flatten is markersUnder for the whole repository.
func flatten(markers map[string][]string) []string { return markersUnder(markers, "", nil) }
