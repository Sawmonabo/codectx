package app

import (
	"context"
	"errors"
	"path"
	"sort"

	"github.com/Sawmonabo/codectx/internal/config"
	"github.com/Sawmonabo/codectx/internal/lang"
	"github.com/Sawmonabo/codectx/internal/model"
	"github.com/Sawmonabo/codectx/internal/process"
	"github.com/Sawmonabo/codectx/internal/provider/dependence"
	"github.com/Sawmonabo/codectx/internal/provider/lsp"
	"github.com/Sawmonabo/codectx/internal/provider/scip"
	"github.com/Sawmonabo/codectx/internal/toolchain"
	"github.com/Sawmonabo/codectx/internal/vcs/git"
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
// workspace's own, under the configuration's traversal policy, so an untracked
// manifest inside a dependency directory or any other excluded tree is never
// seen -- the same tree the precise and dependence planners walk, which is what
// makes what this command installs and what an index needs one question.
//
// A tracked path wins over every exclusion, here as in a capture. Section 10.2
// forces Git's own membership past ignore rules and past the vendor and
// generated exclusions, which is why a capture installs ForceInclude and
// ForceIncludeDir over its staging database: a tracked `third_party/mycrate/
// Cargo.toml` IS in the manifest, roots a project and plans a unit, so a
// selection that pruned it installed nothing for a project the index then
// needs -- the offline runner's mid-index failure again, from the other side.
//
// The second pass below is that force-include, reached without hooks and
// without a staging database. A walk carrying the capture's two hooks emits
// the paths the policy admits plus the tracked paths under excluded
// directories; this walk plus every tracked path emits the paths the policy
// admits plus ALL tracked paths. The two sets differ only by tracked paths the
// policy already admits, which the walk emitted either way, so the answer is
// the same one -- and it is reached by streaming Git's index rather than by
// holding a tracked-path set the hooks could be asked about, which is the
// repository-sized retention this command is not allowed.
//
// The Git ignore predicate snapshot.TraversalPolicy adds is still not
// installed, so the walk sees a few untracked ignored paths a capture would
// exclude. That direction only ever selects a payload the repository may not
// need; the direction this command must never take is missing one.
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
	// passes so that one reading of the repository answers all of them.
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

	sig := newSignals(markers)
	policy := cfg.TraversalPolicy()
	if err := sig.walk(root, policy); err != nil {
		return nil, err
	}
	if err := sig.tracked(root, policy.MaxFiles); err != nil {
		return nil, err
	}
	present, hasDependenceSource := sig.present, sig.source

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

// signals is the answer the two passes below build together: which of the
// marker file names the repository holds anywhere its source reaches, and
// whether it holds a source file of a language family the dependence provider
// analyses. Both passes ask the same two questions of a path, through observe,
// so neither can record a signal the other would have read differently.
//
// Nothing repository-sized is retained: the set of marker names is fixed by
// the packages that own them before the first pass begins, present is bounded
// by that set, and the source question is one bit. Each pass ends as soon as
// complete reports the answer decided -- that is the question being over, not
// a bound on how much of the repository is read.
type signals struct {
	markers map[string]bool
	present map[string]bool
	source  bool
}

func newSignals(markers map[string]bool) *signals {
	return &signals{markers: markers, present: make(map[string]bool, len(markers))}
}

// observe records what one root-relative path contributes. Only its last
// element is read: a marker is a file name, and a path's language is its
// extension's.
func (s *signals) observe(rel string) {
	base := path.Base(rel)
	if s.markers[base] {
		s.present[base] = true
	}
	if !s.source && dependence.FamilyOf(lang.Of(base)) != "" {
		s.source = true
	}
}

// complete reports that every signal is decided, so nothing a pass could still
// see would change the answer.
func (s *signals) complete() bool { return s.source && len(s.present) == len(s.markers) }

// walk is the first pass: every file the traversal policy admits.
func (s *signals) walk(root workspace.Root, policy workspace.Policy) error {
	err := workspace.Walk(context.Background(), root, policy, func(f workspace.File) error {
		s.observe(f.Path)
		if s.complete() {
			return errSignalsComplete
		}
		return nil
	})
	if err != nil && !errors.Is(err, errSignalsComplete) {
		return err
	}
	return nil
}

// tracked is the second pass: every path Git tracks, whatever the policy says
// about the directory holding it. It is the capture's force-include, from the
// capture's own source -- the index listing, bounded by the same
// workspace.max_files the walk is bounded by, with skip-worktree entries
// passed over for the reason the capture passes over them: a sparse checkout
// never materialized that path, so it is not on disk to root anything.
//
// The capture's forced predicate is `tracked = 1 OR change IN ('added',
// 'modified', 'deleted')`, and the index alone answers it for every path that
// exists: a staged add is an index entry, a modification is of a tracked path,
// and a deleted path is not in the worktree to carry a marker or a source. So
// no second Git call is needed here, and there is no second definition of what
// this repository tracks for this one to drift from.
//
// A workspace with a .git entry and no usable Git is an error rather than a
// quieter answer, exactly as it is for a capture: the quieter answer is an
// under-selection, and an under-selection is what the offline runner discovers
// mid-index.
func (s *signals) tracked(root workspace.Root, maxFiles int64) error {
	if !root.HasGit || s.complete() {
		return nil
	}
	exe, err := git.Locate()
	if err != nil {
		return err
	}
	// One listing runs at a time and reserves nothing, so the runner is sized
	// at the package's smallest child reservation: this command starts no
	// other child and has no allocation to divide between them.
	runner, err := process.NewRunner(process.Limits{MaxConcurrent: 1, MemoryBudgetBytes: smallestChildReservationBytes})
	if err != nil {
		return err
	}
	ctx := context.Background()
	g, err := git.New(ctx, runner, exe, 0)
	if err != nil {
		return err
	}
	err = g.ListIndex(ctx, root.Path, maxFiles, func(e git.IndexEntry) error {
		if e.SkipWorktree {
			return nil
		}
		s.observe(e.Path)
		if s.complete() {
			return errSignalsComplete
		}
		return nil
	})
	if err != nil && !errors.Is(err, errSignalsComplete) {
		return err
	}
	return nil
}

// errSignalsComplete ends a selection pass once nothing it could still see
// could change its answer.
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
