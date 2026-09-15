package app

import (
	"errors"
	"io"
	"maps"
	"os"
	"sort"

	"github.com/Sawmonabo/codectx/internal/lang"
	"github.com/Sawmonabo/codectx/internal/model"
	"github.com/Sawmonabo/codectx/internal/provider/dependence"
	"github.com/Sawmonabo/codectx/internal/provider/lsp"
	"github.com/Sawmonabo/codectx/internal/provider/scip"
	"github.com/Sawmonabo/codectx/internal/toolchain"
	"github.com/Sawmonabo/codectx/internal/workspace"
)

// cpgKind is the lock's own word for the tool the dependence provider runs.
// The selection below asks the inventory which entry that is rather than
// naming the engine: the engine's name appears in the lock and in the one
// package that runs it, never on a command surface.
const cpgKind = "cpg"

// SelectedTools is the lock entries the manifests and sources at one repository's
// root select. It reads the mappings that already decide them -- the SCIP
// indexers' triggers, the language servers' root markers, the dependence
// families' project markers, and lang.Of/dependence.FamilyOf for the source
// check below -- from the packages that own them. There is deliberately no
// table here: a second copy of the mapping that decides what gets downloaded is
// exactly the drift policy.md forbids.
//
// The answer is the root's answer, not the planner's. Markers are read at the
// repository root, through the confined handle and by metadata alone, which is
// the same rule lsp.Definition.Detect applies: a present marker selects a
// payload, it never starts anything, and nothing below the root is walked. That
// matches the SCIP and LSP providers, which are root-only too, but it does not
// match the dependence provider, which walks the tree for sources; sources
// under a root that declares nothing are therefore outside what this command
// can see, and docs/toolchain.md says so.
func SelectedTools(path string) ([]string, error) {
	root, err := workspace.Discover(path)
	if err != nil {
		return nil, err
	}
	defer root.Close()

	lock := toolchain.Embedded()
	selected := make(map[string]bool, len(lock.Tools))
	present := func(marker string) bool {
		info, err := root.Lstat(marker)
		return err == nil && info.Mode().IsRegular()
	}
	// A tool is selected by the first of its markers that exists; which one it
	// was does not change the payload.
	add := func(name string, markers []string) {
		for _, marker := range markers {
			if present(marker) {
				selected[name] = true
				return
			}
		}
	}
	for _, kind := range scip.Kinds {
		add(string(kind), scip.Triggers(kind))
	}
	for _, def := range lsp.Definitions() {
		add(def.Name, def.RootMarkers)
	}
	// The dependence provider is the one provider a marker pass alone
	// under-serves. Its C/C++ family declares no project marker at all -- its
	// unit is the repository itself, planned unconditionally -- and the other
	// families' units are found by walking for sources, so a root carrying
	// nothing but sources still runs the graph engine at index time. Marker-only
	// selection therefore left a C or C++ repository without the engine or its
	// runtime, and the user discovered that mid-index on the offline runner
	// prefetching exists to serve. Either signal at the root selects the engine:
	// a project marker of any family, or a source file of any family. This is
	// still no walk -- one listing of the root directory, bounded below.
	//
	// Two root shapes remain under-served and docs/toolchain.md names both: a
	// root that declares nothing and holds no source of its own, and a root
	// declaring only a C or C++ build (CMakeLists.txt, compile_commands.json,
	// Makefile) with its sources under src/ -- those three are the C/C++
	// family's closure markers, which dependence does not export, not project
	// markers. Closing the second needs a ClosureMarkers accessor beside
	// ProjectMarkers; hardcoding the three names here is the drift this
	// function exists to avoid.
	if cpg := cpgEntries(lock); len(cpg) > 0 {
		wanted := rootDeclaresDependenceProject(present)
		if !wanted {
			if wanted, err = rootHasDependenceSource(root); err != nil {
				return nil, err
			}
		}
		if wanted {
			for _, name := range cpg {
				selected[name] = true
			}
		}
	}
	// The runtimes come from the lock's own `runtime` field rather than from a
	// second mapping: a Node-hosted indexer cannot run without node, and the
	// point of prefetching is that nothing is fetched later.
	for name := range maps.Clone(selected) {
		entry, ok := lock.Tools[name]
		if !ok {
			// Every name above is a lock entry by construction, so a miss is a
			// build that shipped a profile the lock does not carry.
			return nil, &model.Error{Code: model.CodeInternal,
				Message: "a profile names a tool the embedded lock does not carry: " + name}
		}
		if entry.Runtime != "" {
			selected[entry.Runtime] = true
		}
	}
	// An empty selection is an ANSWER, not a failure. A root that selects
	// nothing gives the doctor no toolchain row to report; only
	// `tools prefetch --for-repo` treats it as a user error, because that
	// command was asked to install something and has nothing to install.
	names := make([]string, 0, len(selected))
	for name := range selected {
		names = append(names, name)
	}
	sort.Strings(names)
	return names, nil
}

// rootDeclaresDependenceProject reports whether the repository root carries a
// project marker of any family the dependence provider analyses, read through
// the same metadata-only predicate every other marker uses.
func rootDeclaresDependenceProject(present func(string) bool) bool {
	for _, family := range dependence.Families {
		for _, marker := range dependence.ProjectMarkers(family) {
			if present(marker) {
				return true
			}
		}
	}
	// The C/C++ family declares no project marker at all; its build files are
	// closure markers, and a root carrying a C/C++ build declaration (the
	// out-of-source CMake layout keeps every source under src/) declares the
	// family. Only the two that declare a C/C++ build are admitted: a Makefile
	// is ubiquitous in Go, Python and Rust roots and would fetch the engine for
	// a repository that never resolves it; go.sum or yarn.lock alone must not
	// select the engine when the project marker beside them already decides it.
	// A CMakeLists.txt at a root with no C/C++ source anywhere still selects
	// the engine -- the planner gates on sources, this command on the root --
	// and docs/toolchain.md says so.
	for _, marker := range dependence.ClosureMarkers(dependence.FamilyC) {
		if marker == "Makefile" {
			continue
		}
		if present(marker) {
			return true
		}
	}
	return false
}

const (
	// rootListingBatch is how many directory entries the source check holds at
	// once. Nothing is retained beyond one batch: the answer is a single bit and
	// the first source file decides it.
	rootListingBatch = 512
	// There is no bound on the number of entries the loop below reads. The
	// listing is batched at rootListingBatch and keeps no entry, so its peak
	// cost is ONE batch regardless of how many names the root holds; refusing a
	// large root made the toolchain probe -- and with it the whole prefetch --
	// fail on a repository that is merely big.
)

// rootHasDependenceSource reports whether the repository root itself holds a
// source file of a family the dependence provider analyses. It answers the half
// of the selection markers cannot: the C/C++ family declares no project marker,
// so a C or C++ repository is invisible to the marker pass even though the
// planner always gives it a unit.
//
// The classification is not a table here either: lang.Of names the language of
// a path and dependence.FamilyOf names the family that analyses that language,
// both read from the packages that own them.
//
// The root is opened by its own absolute path rather than through the confined
// handle because there is nothing to confine: Root.Path is the directory
// os.OpenRoot itself opened, no user-controlled component is joined to it, and
// workspace.Root exposes no listing of its own (its checkPath refuses "." by
// design). Only entry names are used, and a non-regular entry -- a directory,
// or a symlink, which is never followed -- selects nothing.
func rootHasDependenceSource(root workspace.Root) (bool, error) {
	dir, err := os.Open(root.Path)
	if err != nil {
		return false, listingError(err.Error())
	}
	defer dir.Close()
	for {
		entries, err := dir.ReadDir(rootListingBatch)
		if errors.Is(err, io.EOF) {
			return false, nil
		}
		if err != nil {
			return false, listingError(err.Error())
		}
		for _, e := range entries {
			if !e.Type().IsRegular() {
				continue
			}
			if dependence.FamilyOf(lang.Of(e.Name())) != "" {
				return true, nil
			}
		}
	}
}

func listingError(cause string) *model.Error {
	return (&model.Error{Code: model.CodeArgumentInvalid,
		Message: "the repository root cannot be listed: " + cause}).
		WithRemediation("pass --for-repo a readable repository root, or name the tools to install")
}

// cpgEntries are the lock entries of the kind the dependence provider runs.
// Asking the inventory which entry that is keeps the engine's name out of this
// package: `--for-repo` needs to know that a Go or Java project needs the graph
// engine, not which engine it is.
func cpgEntries(lock toolchain.Lock) []string {
	var out []string
	for _, name := range lock.Names() {
		if lock.Tools[name].Kind == cpgKind {
			out = append(out, name)
		}
	}
	return out
}
