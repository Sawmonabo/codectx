package app

import (
	"context"
	"errors"
	"path"
	"sort"

	"github.com/Sawmonabo/codectx/internal/config"
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

// SelectedTools is the lock entries one repository's own manifests and sources
// select. It reads the mappings that already decide them -- the SCIP indexers'
// triggers, the language servers' root markers, the dependence families'
// project markers, and lang.Of/dependence.FamilyOf for the source check below
// -- from the packages that own them. There is deliberately no table here: a
// second copy of the mapping that decides what gets downloaded is exactly the
// drift policy.md forbids.
//
// The answer is the whole repository's answer, not its root directory's. A
// repository does not keep its projects at its root: every `package.json` of a
// measured monorepo sits in a subdirectory, so a root-only marker check
// resolved no indexer payload at all while each of that repository's projects
// had an indexer and a language server to run. The traversal is the
// workspace's own, under the configuration's traversal policy, so a manifest
// inside a dependency directory or any other excluded tree is never seen --
// the same tree the precise and dependence planners walk, which is what makes
// what this command installs and what an index needs one question.
//
// The Git ignore predicate snapshot.TraversalPolicy adds is not installed
// here: building it needs a Git process runner this command has none of, and
// without it the walk sees a few paths a capture would exclude. That direction
// only ever selects a payload the repository may not need; the direction this
// command must never take is missing one.
func SelectedTools(dir string) ([]string, error) {
	root, err := workspace.Discover(dir)
	if err != nil {
		return nil, err
	}
	defer root.Close()

	cfg, err := config.Load(root.Path)
	if err != nil {
		return nil, err
	}
	lock := toolchain.Embedded()

	// The marker names every mapping below asks about, collected before the
	// walk so that one traversal answers all of them.
	markers := map[string]bool{}
	for _, kind := range scip.Kinds {
		for _, t := range scip.Triggers(kind) {
			markers[t] = true
		}
	}
	for _, def := range lsp.Definitions() {
		for _, m := range def.RootMarkers {
			markers[m] = true
		}
	}
	// The dependence provider is the one provider a marker pass alone
	// under-serves. Its C/C++ family declares no project marker at all, and the
	// other families' units are found by walking for sources, so a directory
	// carrying nothing but sources still runs the graph engine at index time.
	// Marker-only selection therefore left a C or C++ repository without the
	// engine or its runtime, and the user discovered that mid-index on the
	// offline runner prefetching exists to serve. Either signal selects the
	// engine: a project marker of any family, or a source file of any family.
	for _, family := range dependence.Families {
		for _, m := range dependence.ProjectMarkers(family) {
			markers[m] = true
		}
	}

	present, hasDependenceSource, err := repositorySignals(root, cfg.TraversalPolicy(), markers)
	if err != nil {
		return nil, err
	}

	selected := make(map[string]bool, len(lock.Tools))
	// A tool is selected by the first of its markers the repository holds;
	// which one it was, and which directory holds it, does not change the
	// payload.
	add := func(name string, names []string) {
		for _, marker := range names {
			if present[marker] {
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
	if cpg := cpgEntries(lock); len(cpg) > 0 {
		wanted := hasDependenceSource
		for _, family := range dependence.Families {
			if wanted {
				break
			}
			for _, marker := range dependence.ProjectMarkers(family) {
				if present[marker] {
					wanted = true
					break
				}
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
	runtimes := make([]string, 0, 2)
	for name := range selected {
		entry, ok := lock.Tools[name]
		if !ok {
			// Every name above is a lock entry by construction, so a miss is a
			// build that shipped a profile the lock does not carry.
			return nil, &model.Error{Code: model.CodeInternal,
				Message: "a profile names a tool the embedded lock does not carry: " + name}
		}
		if entry.Runtime != "" {
			runtimes = append(runtimes, entry.Runtime)
		}
	}
	for _, name := range runtimes {
		selected[name] = true
	}
	// An empty selection is an ANSWER, not a failure. A repository that selects
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

// repositorySignals walks the workspace once and answers the two questions the
// selection asks of it: which of the marker file names the repository holds
// anywhere the traversal admits, and whether it holds a source file of a
// language family the dependence provider analyses.
//
// Nothing repository-sized is retained: the set of marker names is fixed by
// the packages that own them before the walk begins, and the source question
// is one bit. The walk ends as soon as every marker has been seen and a source
// has been found, because at that moment the answer is complete -- that is the
// question being over, not a bound on how much of the repository is read.
func repositorySignals(root workspace.Root, policy workspace.Policy, markers map[string]bool) (map[string]bool, bool, error) {
	present := make(map[string]bool, len(markers))
	source := false
	err := workspace.Walk(context.Background(), root, policy, func(f workspace.File) error {
		base := path.Base(f.Path)
		if markers[base] {
			present[base] = true
		}
		if !source && dependence.FamilyOf(lang.Of(base)) != "" {
			source = true
		}
		if source && len(present) == len(markers) {
			return errSignalsComplete
		}
		return nil
	})
	if err != nil && !errors.Is(err, errSignalsComplete) {
		return nil, false, err
	}
	return present, source, nil
}

// errSignalsComplete ends the selection walk once nothing later in the
// repository could change its answer.
var errSignalsComplete = errors.New("every tool-selection signal is decided")

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
