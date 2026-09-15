// Package plan derives, for one captured snapshot, exactly the units the
// active providers will run, which sealed units of the previous generation may
// be reused byte for byte, which refreshing semantic units may be carried
// stale with their provenance distance, and which predecessor each rebuilt
// unit imports its delta from (Sections 13.1, 13.3).
//
// The planner runs no provider and writes nothing. It reads the snapshot
// manifest once per pass and the previous generation's unit selection through
// the store's read-only calls, and it answers with a value the coordinator
// executes. Reuse is demonstrated by an equal UnitID over the same inputs and
// dependencies, never assumed because a path is unchanged (Section 13.1).
//
// It also owns the heavy-analyzer admission gate (Scheduler), because
// admission is a scheduling decision over the same unit descriptors this
// package produces.
package plan

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"slices"
	"strconv"

	"github.com/Sawmonabo/codectx/internal/config"
	"github.com/Sawmonabo/codectx/internal/model"
	"github.com/Sawmonabo/codectx/internal/pagination"
	"github.com/Sawmonabo/codectx/internal/provider"
	"github.com/Sawmonabo/codectx/internal/provider/dependence"
	"github.com/Sawmonabo/codectx/internal/provider/filesystem"
	"github.com/Sawmonabo/codectx/internal/provider/manifest"
	"github.com/Sawmonabo/codectx/internal/storage/sqlite"
)

// Unplanned keys. Each names one thing the plan could not do, or did at a
// coarser boundary than the product owns, so a reader cannot confuse two
// different degradations counted in the same map.
const (
	// unplannedScopeKey counts the files whose unit scope key does not fit
	// model.MaxScopeKeyBytes, per provider.
	unplannedScopeKey = "scope_key_over_bound:"
	// unplannedNoInputs counts the semantic scopes refused because nothing in
	// the pinned snapshot is a member of them, per provider.
	unplannedNoInputs = "semantic_scope_without_inputs:"
	// unplannedProjects counts the dependence projects the provider's own
	// planner refused as units of their own, per family.
	unplannedProjects = "dependence_projects_unplanned:"
	// unplannedDuplicateFileNode counts the manifest units whose file node
	// duplicates the filesystem unit's node for the same file identity
	// (ruling Q8 as re-ruled, Task 7 residual). See the comment at duplicate().
	unplannedDuplicateFileNode = "manifest_duplicate_file_node"
)

// scopeSeparator joins a provider ID and a scope key into the Reuse/Previous
// map key. It is a byte no identifier may contain, so two different pairs can
// never collide on one key.
const scopeSeparator = "\x00"

// Key is the Reuse and Previous map key for one provider scope.
func Key(providerID, scopeKey string) string { return providerID + scopeSeparator + scopeKey }

// Unit is one unit the coordinator will run. It carries everything a
// model.UnitBuild needs except the analysis config hash, which is the
// coordinator's and reaches identity through Spec.
type Unit struct {
	ProviderID, ProviderVersion, ScopeKey string
	// Inputs yields the unit's declared inputs in ascending FileID order,
	// which is the order model.UnitInputHasher requires and what makes the
	// input digest independent of discovery order. It is a sequence and not a
	// slice because a workspace-scoped unit's membership is the whole snapshot:
	// on a monorepo that list is repository-sized, so it is streamed from an
	// external sort rather than held in heap (Section 6). It must be
	// re-iterable -- the planner folds the identity digest over it and the
	// coordinator streams it again into storage -- and it must not be called
	// after the Plan it came from is closed. InputCount is its length.
	InputCount int64
	Inputs     func(yield func(model.UnitInput) error) error
	// DependsOn are the units this one is reconciled against. It is set only
	// where the relation is one to one and bounded -- a manifest or structural
	// unit against the filesystem unit of the same file -- and left empty for
	// a workspace or package unit, whose dependency would be the repository's
	// whole file-unit list and would exceed model.MaxDependenciesPerUnit.
	DependsOn []model.UnitID
	// Heavy marks a unit that must pass the Scheduler before it runs, and
	// Reservation is what it is admitted against.
	Heavy       bool
	Reservation dependence.Reservation
	// Deferred marks a unit that does not block the base generation: under
	// `providers.dependence.enabled = "auto"` the coordinator runs it as
	// low-priority background work after base activation and publishes it
	// through a later generation (Section 11.6, ruling Q9).
	Deferred bool
}

// Spec folds the unit's identity. analysisConfigHash is the coordinator's
// config.Config.AnalysisConfigHash, which the units table stores no column for:
// analyzer configuration reaches storage only through this key, which is what
// makes a config change invalidate the unit (Section 20.2).
func (u Unit) Spec(analysisConfigHash string) (model.UnitSpec, error) {
	h := model.NewUnitInputHasher()
	if u.Inputs != nil {
		if err := u.Inputs(h.Add); err != nil {
			return model.UnitSpec{}, err
		}
	}
	spec := model.UnitSpec{ProviderID: u.ProviderID, ProviderVersion: u.ProviderVersion,
		ScopeKey: u.ScopeKey, InputHash: h.Sum(), DependencyHash: model.DependencyHash(u.DependsOn)}
	spec.ID = model.NewUnitID(spec, analysisConfigHash)
	return spec, nil
}

// Carried is one previous sealed unit of a refreshing semantic scope, with the
// provenance distance Section 13.3 requires it to answer `stale` with. The
// coordinator attaches it through sqlite.AttachCarried; it is never reported
// fresh. Its scope's deferred unit is also in Plan.Units -- see Plan.Carry.
//
// It is also the row shape Inputs.CarriedPage reads back: a scope the previous
// generation already carried is the same unit with the distance it had then,
// which this generation's carry accumulates onto rather than restarting.
type Carried struct {
	Unit                 model.UnitID
	ProviderID, ScopeKey string
	// DistanceGenerations is how many activations separate the carried unit's
	// snapshot from this one, and DistanceFiles how many member-file changes
	// do, summed over those generations: a file edited in two of them counts
	// once per generation, because each of those generations is one the unit
	// did not see.
	DistanceGenerations, DistanceFiles int
}

// Plan is one snapshot's complete unit plan.
type Plan struct {
	// Units yields the units to run, in provider dependency order. It is a
	// sequence and not a slice because a file-invalidated provider plans one
	// unit per snapshot file: on a monorepo that list is repository-sized, so
	// the file units are streamed from an external sort rather than held in
	// heap (Section 6), exactly as a workspace unit's membership already is.
	// A hand-built Plan may leave it nil, which is a plan that runs nothing.
	//
	// It is re-iterable, it must not be called after Plan.Close, and the
	// order it answers is byte for byte the order the in-heap list answered:
	// the sort key is (the provider's position in Selection.Active, arrival
	// sequence), so every unit identity and every digest is unchanged.
	Units func(yield func(Unit) error) error
	// Reuse maps Key(providerID, scopeKey) to the sealed unit of the previous
	// generation whose identity the fresh plan reproduces exactly. Those
	// scopes have no entry in Units: there is nothing to run.
	Reuse map[string]model.UnitID
	// Carry are the stale predecessors of deferred semantic scopes, to attach
	// through sqlite.AttachCarried. A carried scope's *deferred* unit is also
	// in Units, because it runs later, in its own work generation (Section
	// 11.6, ruling Q1). The coordinator attaches the carried predecessor into
	// this generation with sqlite.AttachCarried and must not attach the
	// deferred unit here: generation_units holds one row per
	// (generation, provider, scope).
	Carry []Carried
	// Previous maps Key(providerID, scopeKey) to the sealed predecessor a
	// rebuilt semantic unit imports its delta from. A file-invalidated unit has
	// no entry: it is rebuilt whole from one file, and there is no delta
	// applier for it.
	Previous map[string]model.UnitID
	// Unplanned counts, per keyed reason, what the plan could not do. Counts
	// and not paths: a plan never holds a repository-sized list (Section 6).
	Unplanned map[string]int
	// States is Selection.States plus every degradation the planner itself
	// found, which is the complete capability picture before any unit runs.
	States []model.CapabilityState

	// shared is the spilled, sorted membership of every whole-snapshot unit,
	// which their Unit.Inputs sequences stream from.
	shared *pagination.SortedRun[model.UnitInput]
	// files is the spilled, sorted run of every file-invalidated provider's
	// unit, which Units streams from. Semantic units are not in it: a package-
	// or workspace-scoped unit list is bounded by the scope count, never by
	// the repository, so it stays in heap (H-L1b).
	files *pagination.SortedRun[fileUnitRecord]
}

// Close releases the plan's two spilled runs: the shared input membership every
// whole-snapshot unit's Unit.Inputs reads from, and the file units Plan.Units
// streams. A caller closes the plan after executing it, and neither sequence may
// be read afterwards. A plan small enough to fit its run buffers holds no file
// and Close is then free; a zero Plan may be closed.
func (p *Plan) Close() error {
	if p == nil {
		return nil
	}
	err := p.shared.Close()
	p.shared = nil
	if ferr := p.files.Close(); err == nil {
		err = ferr
	}
	p.files = nil
	return err
}

// Inputs are the planner's read-only dependencies.
type Inputs struct {
	View model.SnapshotView
	// Selection is what Registry.Select answered. Its Detections carry the two
	// things the planner needs and a descriptor does not hold: ObservedVersion,
	// which folds into UnitSpec.ProviderVersion so a unit is keyed by the tool
	// that actually produced it, and InputPaths, which is what
	// scip.Provider.Scopes reads. The planner is handed no workspace root and
	// must not re-run detection anyway: detection touches the live checkout,
	// and the plan is about the pinned snapshot.
	Selection provider.Selection
	Store     *sqlite.Store
	// PrevGen is the generation reuse, carry and delta predecessors are read
	// from; zero means there is none and every unit is built.
	PrevGen model.GenerationID
	// CarriedPage is one keyset page of PrevGen's stale members, exactly
	// sqlite.Store.CarriedUnits' shape: page in (provider, scope) order from
	// the given cursor (two empty strings start from the beginning), at most
	// limit rows, a short page being the last. Build pages until a short page
	// arrives and folds the result by Key(providerID, scopeKey), so a unit
	// carried across several generations accumulates its distance instead of
	// reporting 1 forever.
	//
	// It is the page fetcher and not the folded lookup because completeness is
	// then structural: a coordinator that called the listing once and folded
	// one page would satisfy a lookup-shaped field with a partial map, and
	// every scope past the first page would read as "never carried" -- a unit
	// stale for ten generations reported stale for one. Build never chooses
	// the response bound it pages with beyond asking for model.MaxPageItems,
	// which is the cap the store applies anyway; a nil fetcher is refused
	// whenever PrevGen is set.
	CarriedPage func(ctx context.Context, afterProviderID, afterScopeKey string, limit int) ([]Carried, error)
	Config      config.Config
	// TempDir is where the planner spills the sorted input run of whole-snapshot
	// units. Empty takes the process temporary directory. The spill lives only
	// as long as the returned Plan and is removed by Plan.Close.
	TempDir string
}

// Build derives the plan. It reads the snapshot manifest and the previous
// generation; it runs nothing and writes nothing.
func Build(ctx context.Context, in Inputs) (Plan, error) {
	if in.View == nil || in.Store == nil {
		return Plan{}, invalid("the planner needs a snapshot view and the store")
	}
	if in.PrevGen != 0 && in.CarriedPage == nil {
		return Plan{}, invalid("the planner needs the previous generation's carry distances; wire Inputs.CarriedPage to the paged sqlite.Store.CarriedUnits listing")
	}
	b := &builder{in: in, cfgHash: in.Config.AnalysisConfigHash(),
		plan: Plan{Reuse: map[string]model.UnitID{}, Previous: map[string]model.UnitID{},
			Unplanned: map[string]int{}, States: slices.Clone(in.Selection.States)},
		byProvider: map[string][]Unit{}, overBound: map[string]string{}, noInputs: map[string]string{},
		providerOrder: make(map[string]int, len(in.Selection.Active))}
	for i, p := range in.Selection.Active {
		if _, dup := b.providerOrder[p.Descriptor().ID]; !dup {
			b.providerOrder[p.Descriptor().ID] = i
		}
	}
	dir := in.TempDir
	if dir == "" {
		dir = os.TempDir()
	}
	sorter, err := pagination.NewExternalSort(dir, "plan-inputs-", 0, encodeInput, decodeInput, compareInput)
	if err != nil {
		return Plan{}, err
	}
	// The run buffer is charged in BYTES as well as records. A model.UnitInput
	// is two bounded hex ids, so the record count alone would be a ~13 MiB
	// constant -- but "bounded" is the field ceiling, not a fixed width, and a
	// byte budget is what makes the planner's peak the same operator-set
	// number the query path's sorts already respect instead of a second,
	// implicit one. resources.query_memory_bytes is that number, and
	// pagination.SortRunBytes takes one sort's quarter share of it.
	sorter = sorter.WithRunBytes(pagination.SortRunBytes(in.Config.Resources.QueryMemoryBytes), sizeOfInput)
	b.allInputs = sorter
	// Sorted() removes the runs on the success path; this covers every early
	// return, which would otherwise leave spill files behind.
	defer func() { _ = sorter.Close() }()
	units, err := pagination.NewExternalSort(dir, "plan-units-", 0, encodeUnit, decodeUnit, compareUnit)
	if err != nil {
		return Plan{}, err
	}
	// Charged in BYTES as well as records for the reason F37 settled for the
	// input sort: a unit record carries a scope key, which is bounded by
	// model.MaxScopeKeyBytes rather than fixed, so a record count alone would
	// be a budget that means something different on every repository.
	units = units.WithRunBytes(pagination.SortRunBytes(in.Config.Resources.QueryMemoryBytes), sizeOfUnit)
	b.allUnits = units
	defer func() { _ = units.Close() }()
	if err := b.classifyProviders(ctx); err != nil {
		return Plan{}, err
	}
	if err := b.foldCarry(ctx); err != nil {
		return Plan{}, err
	}
	if err := b.walk(ctx); err != nil {
		return Plan{}, err
	}
	if err := b.emit(ctx); err != nil {
		_ = b.plan.Close()
		return Plan{}, err
	}
	// Every output map of one value is normalized empty-to-nil here, in one
	// place: a caller ranging over the plan cannot then tell an empty map from
	// a missing one by accident.
	if len(b.plan.Reuse) == 0 {
		b.plan.Reuse = nil
	}
	if len(b.plan.Previous) == 0 {
		b.plan.Previous = nil
	}
	if len(b.plan.Unplanned) == 0 {
		b.plan.Unplanned = nil
	}
	return b.plan, nil
}

// builder holds one Build. Everything repository-sized in it is either the
// plan's own output or one input list per planned unit, which the frozen Unit
// shape requires; nothing else accumulates with the repository.
type builder struct {
	in      Inputs
	cfgHash string
	plan    Plan

	// fileProviders are the file-invalidated providers in dependency order,
	// so the filesystem unit of a path is derived before the units that depend
	// on it.
	fileProviders []fileProvider
	// semantics are the package- and workspace-scoped units, grouped by
	// provider ID; emit walks the selection to order them.
	semantics map[string][]*semantic
	// allInputs accumulates the inputs of every unit whose membership is the
	// whole snapshot. It is an external sort and not a slice: on a monorepo the
	// snapshot IS the list, so holding it would make peak RSS a function of
	// repository size. Records spill to sorted runs as the walk fills the run
	// buffer and are merged once in emit, so peak is the run buffer plus one
	// read block per run. The merged run is shared by every such unit -- one
	// sequence, re-iterated -- rather than copied per unit.
	allInputs *pagination.ExternalSort[model.UnitInput]
	// allUnits accumulates every file-invalidated provider's unit. It is an
	// external sort for the same reason allInputs is: a file provider plans
	// one unit per snapshot file, so holding the list would make peak RSS a
	// function of repository size. The records spill as the walk fills the run
	// buffer and are merged once in emit; Plan.Units then streams the merged
	// run, so the executor's peak is one batch of units and not the plan's.
	allUnits *pagination.ExternalSort[fileUnitRecord]
	// unitSeq is the arrival counter that, with the provider's position in
	// Selection.Active, keys allUnits. Sorting by (position, arrival) is what
	// reproduces the in-heap concatenation exactly.
	unitSeq int64
	// providerOrder is each active provider's position in Selection.Active,
	// which is the dependency order Plan.Units answers in.
	providerOrder map[string]int
	// prior is the complete fold of Inputs.CarriedPage by Key, empty when
	// there is no previous generation. It holds one entry per stale scope of
	// that generation -- unit-scoped like Plan.Reuse and Plan.Previous, never
	// file-scoped.
	prior map[string]Carried

	byProvider map[string][]Unit
	// overBound records one exemplar path per provider whose scope key does
	// not fit, for the degradation row; the count lives in Unplanned.
	overBound map[string]string
	// noInputs records one exemplar scope key per provider whose semantic
	// scope no snapshot file is a member of, for the degradation row.
	noInputs map[string]string
	// fsActive records whether the filesystem provider is active. Change
	// attribution is derived from its per-file unit identity, which is the
	// only content-exact comparison the planner has.
	fsActive bool
}

// fileProvider is one active file-invalidated provider.
type fileProvider struct {
	id      string
	version string
	gate    fileGate
	// dependsOnFile marks a provider whose unit for a path is reconciled
	// against the filesystem unit of that same path.
	dependsOnFile bool
}

// classifyProviders splits the active selection into file-invalidated and
// larger-scoped providers and derives every scope the larger ones build.
func (b *builder) classifyProviders(ctx context.Context) error {
	b.semantics = map[string][]*semantic{}
	for _, p := range b.in.Selection.Active {
		d := p.Descriptor()
		det, ok := b.in.Selection.Detections[d.ID]
		if !ok {
			// Argument validation on an exported API, not a reachable
			// degradation: Registry.Select fills Detections for exactly the
			// providers it returns in Active. A caller that hands Build a
			// hand-built Selection instead must not silently plan an active
			// provider's units from a zero Detection, which would change every
			// UnitSpec.ProviderVersion.
			return invalid("the selection carries no detection for active provider " + d.ID +
				"; Selection must be the value Registry.Select returned")
		}
		version, err := foldVersion(d.Version, det.ObservedVersion)
		if err != nil {
			return err
		}
		if d.InvalidationScope == model.InvalidationFile {
			gate, err := fileGateFor(p)
			if err != nil {
				return err
			}
			if d.ID == filesystem.ID {
				b.fsActive = true
			}
			b.fileProviders = append(b.fileProviders, fileProvider{id: d.ID, version: version,
				gate: gate, dependsOnFile: d.ID != filesystem.ID && slices.Contains(d.DependsOn, filesystem.ID)})
			continue
		}
		scopes, unplanned, err := semanticScopes(ctx, p, det, b.in.View)
		if err != nil {
			return err
		}
		for reason, n := range unplanned {
			b.plan.Unplanned[reason] += n
		}
		for i := range scopes {
			scopes[i].version = version
			b.semantics[d.ID] = append(b.semantics[d.ID], &scopes[i])
		}
	}
	return nil
}

// foldCarry reads the previous generation's carry distances once, before the
// manifest walk. It pages Inputs.CarriedPage to the end -- a page shorter than
// the limit is the last, the same keyset loop the snapshot view uses over the
// manifest -- because a partial fold is indistinguishable from "this scope was
// never carried": the scopes past the first page would each restart their
// provenance distance at 1 and report a unit stale for ten generations as
// stale for one.
func (b *builder) foldCarry(ctx context.Context) error {
	if b.in.PrevGen == 0 {
		return nil
	}
	b.prior = map[string]Carried{}
	afterProviderID, afterScopeKey := "", ""
	for {
		page, err := b.in.CarriedPage(ctx, afterProviderID, afterScopeKey, model.MaxPageItems)
		if err != nil {
			return err
		}
		for _, c := range page {
			b.prior[Key(c.ProviderID, c.ScopeKey)] = c
		}
		if len(page) < model.MaxPageItems {
			return nil
		}
		last := page[len(page)-1]
		afterProviderID, afterScopeKey = last.ProviderID, last.ScopeKey
	}
}

// walk is the one manifest pass that plans every file unit, folds every
// semantic unit's inputs, and attributes each changed or removed file to the
// semantic units that own it.
func (b *builder) walk(ctx context.Context) error {
	shared := false
	for _, list := range b.semantics {
		for _, s := range list {
			shared = shared || s.allFiles
		}
	}
	return b.in.View.EachFile(ctx, model.FileSelection{}, func(fv model.FileVersion) error {
		if err := ctx.Err(); err != nil {
			return model.Canceled(err)
		}
		deleted := fv.Status == model.FileDeleted
		changed, removed, err := b.fileUnits(ctx, fv, deleted)
		if err != nil {
			return err
		}
		in := model.UnitInput{FileID: fv.ID, ContentHash: fv.ContentHash, Executable: fv.Executable}
		if shared && !deleted {
			if err := b.allInputs.Add(in); err != nil {
				return err
			}
		}
		// One membership test per file per semantic unit decides both the
		// unit's declared inputs and the provenance distance of a carry, so
		// the predicate is evaluated once.
		for _, list := range b.semantics {
			for _, s := range list {
				if !s.contains(fv) {
					continue
				}
				if !deleted && !s.allFiles {
					s.inputs = append(s.inputs, in)
				}
				if changed || removed {
					s.changed++
					s.removed = s.removed || removed
				}
			}
		}
		return nil
	})
}

// fileUnits plans every file-invalidated provider's unit for one snapshot file
// and reports whether the file's bytes changed since the previous generation
// and whether it is a tombstone the previous generation indexed.
//
// A deleted file gets no unit: a tombstone has no bytes. It is still looked up
// in the previous generation, because a member file that existed then and is
// gone now is exactly what Section 13.3 forbids carrying over.
func (b *builder) fileUnits(ctx context.Context, fv model.FileVersion, deleted bool) (changed, removed bool, err error) {
	scopeKey := filesystem.ScopeKey(fv.Path)
	if len(scopeKey) > model.MaxScopeKeyBytes {
		if deleted {
			// A tombstone was never going to be planned, so counting it as a
			// refused scope would over-report the degradation. It is still a
			// change for every scope that owns it, and never a removal: a key
			// this long had no unit in the previous generation either.
			return true, false, nil
		}
		// Refused, never truncated and never silently dropped: a truncated key
		// is a colliding identity claim, and two deep paths sharing a long
		// prefix would then have one another's facts. The refusal is counted
		// and published as a degradation row in emit.
		for _, fp := range b.fileProviders {
			if !fp.gate(fv) {
				continue
			}
			b.plan.Unplanned[unplannedScopeKey+fp.id]++
			if _, seen := b.overBound[fp.id]; !seen {
				b.overBound[fp.id] = fv.Path
			}
		}
		// Whether such a file's bytes changed cannot be established without a
		// unit to compare, and a unit nothing indexes cannot be claimed
		// unchanged: it counts as changed for every scope that owns it.
		return true, false, nil
	}
	// Without the filesystem provider there is no content-exact comparison, so
	// no file is claimed unchanged and no semantic scope is carried.
	changed = !b.fsActive
	var fsUnit model.UnitID
	for _, fp := range b.fileProviders {
		if deleted || !fp.gate(fv) {
			continue
		}
		u := Unit{ProviderID: fp.id, ProviderVersion: fp.version, ScopeKey: scopeKey, InputCount: 1,
			Inputs: staticInputs([]model.UnitInput{{FileID: fv.ID, ContentHash: fv.ContentHash,
				Executable: fv.Executable}})}
		if fp.dependsOnFile {
			// The registry hands providers back in dependency order, so the
			// filesystem unit of this path is already derived. If it is not,
			// the dependency would silently be dropped from this unit's
			// identity, which would let it be reused against a file whose
			// identity unit changed.
			if fsUnit == "" {
				return false, false, internalErr("provider " + fp.id +
					" is planned before the filesystem unit it depends on")
			}
			u.DependsOn = []model.UnitID{fsUnit}
		}
		spec, err := u.Spec(b.cfgHash)
		if err != nil {
			return false, false, err
		}
		// A file unit is rebuilt whole from one file; no delta applier exists
		// for one, so its predecessor is not recorded.
		reused, _, err := b.previous(ctx, fp.id, scopeKey, spec.ID)
		if err != nil {
			return false, false, err
		}
		if fp.id == filesystem.ID {
			fsUnit = spec.ID
			// The filesystem unit folds this file's content hash and nothing
			// else, so reusing it is the content-exact answer to "did this
			// file change", and a path the previous generation never held is a
			// change too.
			changed = !reused
			b.duplicate(fv)
		}
		if reused {
			b.plan.Reuse[Key(fp.id, scopeKey)] = spec.ID
			continue
		}
		rec := fileUnitRecord{Order: b.providerOrder[fp.id], Seq: b.unitSeq,
			ProviderID: fp.id, ProviderVersion: fp.version, ScopeKey: scopeKey,
			FileID: fv.ID, ContentHash: fv.ContentHash, Executable: fv.Executable}
		if len(u.DependsOn) == 1 {
			rec.DependsOn = u.DependsOn[0]
		}
		b.unitSeq++
		if err := b.allUnits.Add(rec); err != nil {
			return false, false, err
		}
	}
	if deleted {
		// A tombstone the previous generation selected a filesystem unit for is
		// a member that existed and no longer does.
		prev, err := b.selected(ctx, filesystem.ID, scopeKey)
		if err != nil {
			return false, false, err
		}
		return true, prev != "", nil
	}
	return changed, false, nil
}

// duplicate counts the file identity that two units publish a node fact for:
// the filesystem unit's, which carries the file's metadata, and the manifest
// unit's, which is nil-valued (manifest.unit.fileNode).
//
// Ruling Q8 first put the suppression at plan time. It cannot be there:
// node_facts' conflict clause is ON CONFLICT(unit_id, node_id) DO NOTHING
// (internal/storage/sqlite/units.go:459), which is per unit, so both rows are
// written whatever order the units run in, and no plan-time ordering or
// dependency suppresses either. Re-ruled: precedence is applied at read time
// in the query layer (Task 13/14) -- for one node id across the units of the
// active generation the filesystem provider's row wins over a nil-valued
// manifest row. The plan counts the duplicate only, which is what this does.
func (b *builder) duplicate(fv model.FileVersion) {
	for _, fp := range b.fileProviders {
		if fp.id == manifest.ID && fp.gate(fv) {
			b.plan.Unplanned[unplannedDuplicateFileNode]++
			return
		}
	}
}

// emit turns the semantic scopes into units, decides reuse, carry and delta
// predecessor for each, and orders the plan by the selection's dependency
// order.
func (b *builder) emit(ctx context.Context) error {
	shared, err := b.allInputs.Sorted()
	if err != nil {
		return err
	}
	b.plan.shared = shared
	gov := dependence.NewGovernor(b.in.Config.Providers.Dependence.UnitMemoryFloorBytes,
		b.in.Config.Providers.Dependence.UnitMemoryCeilingBytes)
	machine := dependence.ObserveMachine()
	deferDependence := b.in.Config.Providers.Dependence.Enabled == config.Auto

	for _, p := range b.in.Selection.Active {
		d := p.Descriptor()
		for _, s := range b.semantics[d.ID] {
			// A package- or project-scoped member list is bounded by that
			// scope's size, not by the repository's, so it stays in heap; only
			// the whole-snapshot membership is spilled.
			count := int64(len(s.inputs))
			inputs := staticInputs(s.inputs)
			if s.allFiles {
				count, inputs = shared.Len(), shared.Each
			} else {
				slices.SortFunc(s.inputs, func(a, c model.UnitInput) int { return compareID(a.FileID, c.FileID) })
			}
			if count == 0 {
				// A unit that declares nothing folds the empty input digest,
				// which is the same digest whatever the snapshot holds -- an
				// identity that reuses forever no matter what changed. It is
				// reachable: a scip `import:` scope whose index file detection
				// saw in the live checkout is not in the pinned snapshot (the
				// workspace policy excluded it, or it is over a size bound) is
				// a member of nothing. Refused and published as a degradation,
				// never planned with a constant identity.
				b.plan.Unplanned[unplannedNoInputs+s.providerID]++
				if _, seen := b.noInputs[s.providerID]; !seen {
					b.noInputs[s.providerID] = s.scopeKey
				}
				continue
			}
			u := Unit{ProviderID: s.providerID, ProviderVersion: s.version, ScopeKey: s.scopeKey,
				InputCount: count, Inputs: inputs}
			if s.heavy {
				u.Heavy = true
				// The heap estimate is sized from the family's own source
				// bytes, which is the figure research measured the per-family
				// ratio against; the unit's declared inputs are a larger set
				// (its manifests and lock files) and would inflate it.
				u.Reservation = gov.Reserve(s.family, s.bytes, machine)
				u.Deferred = deferDependence
			}
			spec, err := u.Spec(b.cfgHash)
			if err != nil {
				return err
			}
			reused, prev, err := b.previous(ctx, s.providerID, s.scopeKey, spec.ID)
			if err != nil {
				return err
			}
			if reused {
				b.plan.Reuse[Key(s.providerID, s.scopeKey)] = spec.ID
				continue
			}
			if prev != "" {
				b.plan.Previous[Key(s.providerID, s.scopeKey)] = prev
				b.carry(s, prev, u.Deferred)
			}
			b.byProvider[d.ID] = append(b.byProvider[d.ID], u)
		}
	}
	files, err := b.allUnits.Sorted()
	if err != nil {
		return err
	}
	b.plan.files = files
	// One slot per position in Selection.Active, holding that provider's
	// semantic units; a file provider's slot stays empty because the merged
	// run serves it. A duplicate entry in Active keeps its units at the first
	// position, which is the one providerOrder keyed its records to, so it is
	// emitted once rather than twice.
	slots := make([][]Unit, len(b.in.Selection.Active))
	for i, p := range b.in.Selection.Active {
		if id := p.Descriptor().ID; b.providerOrder[id] == i {
			slots[i] = b.byProvider[id]
		}
	}
	b.plan.Units = unitSequence(files, slots)
	b.degradations()
	return nil
}

// unitSequence is Plan.Units: one pass that interleaves the merged file-unit
// run with the semantic units held in heap, in Selection.Active order.
//
// It reproduces the concatenation it replaced byte for byte. A provider is
// either file-invalidated or larger-scoped and never both (classifyProviders
// branches on InvalidationScope), so each position in the order is served by
// exactly one of the two sources: the run's records for that position, in
// arrival order, or that provider's bounded semantic list. Peak is one unit,
// plus the run's own read block.
func unitSequence(run *pagination.SortedRun[fileUnitRecord], slots [][]Unit) func(yield func(Unit) error) error {
	return func(yield func(Unit) error) error {
		next := 0
		// flush emits every semantic provider strictly before position n. A
		// file provider's position is skipped here: the run serves it.
		flush := func(n int) error {
			for ; next < n; next++ {
				for _, u := range slots[next] {
					if err := yield(u); err != nil {
						return err
					}
				}
			}
			return nil
		}
		if err := run.Each(func(rec fileUnitRecord) error {
			if rec.Order >= len(slots) {
				// Unreachable: every record is keyed by providerOrder, which
				// is a position in the very slice slots was sized from. It is
				// a typed refusal and not a skipped flush because skipping one
				// would move that provider's semantic units silently to the
				// tail -- a reordering, which is the one thing this sequence
				// exists to preserve.
				return internalErr("the plan's unit run names provider position " +
					strconv.Itoa(rec.Order) + ", past the " + strconv.Itoa(len(slots)) +
					" active providers it was planned against")
			}
			if err := flush(rec.Order); err != nil {
				return err
			}
			return yield(rec.unit())
		}); err != nil {
			return err
		}
		return flush(len(slots))
	}
}

// carry records the stale predecessor of a refreshing semantic scope with its
// provenance distance (Section 13.3). A scope whose member file the previous
// generation indexed and the snapshot no longer holds is not carried: the
// carried unit's facts would be about source that is gone.
//
// Only scopes the current snapshot produced reach here, so a unit whose whole
// scope disappeared -- a deleted module -- is never carried either.
//
// Only a deferred scope is carried, and that is a hard constraint rather than
// a preference: generation_units is keyed (generation_id, provider_id,
// scope_key), so one generation holds exactly one row per scope. A deferred
// unit runs in a later work generation and publishes through a later
// activation (Section 11.6), so this generation's row is free for its
// predecessor. A unit that seals inside this generation occupies that row
// itself, and carrying its predecessor beside it would be a primary-key
// conflict, not a stale answer. Carrying such a scope after its unit *fails*
// is a runtime decision this planner cannot make; Previous already names the
// sealed predecessor the coordinator would attach.
func (b *builder) carry(s *semantic, prev model.UnitID, deferred bool) {
	if s.removed || !deferred {
		return
	}
	c := Carried{Unit: prev, ProviderID: s.providerID, ScopeKey: s.scopeKey,
		DistanceGenerations: 1, DistanceFiles: s.changed}
	if prior, ok := b.prior[Key(s.providerID, s.scopeKey)]; ok {
		// The predecessor was already stale in the previous generation, so its
		// distance is measured from its own snapshot, not from that one.
		c.DistanceGenerations += prior.DistanceGenerations
		c.DistanceFiles += prior.DistanceFiles
	}
	b.plan.Carry = append(b.plan.Carry, c)
}

// degradations publishes one capability row per affected provider capability
// for everything the planner refused. One row per (provider, capability) and
// not one per path: the row count must stay inside model.MaxCapabilityStates
// whatever the repository holds, and the offending path cannot be the row's
// scope -- it is by definition longer than model.MaxScopeKeyBytes, which
// CapabilityState.Validate bounds Scope by.
func (b *builder) degradations() {
	for _, p := range b.in.Selection.Active {
		d := p.Descriptor()
		if n := b.plan.Unplanned[unplannedScopeKey+d.ID]; n > 0 {
			for _, c := range d.Capabilities {
				state := b.partial(d, c).WithDetail("files_over_scope_bound", strconv.Itoa(n))
				state = state.WithDetail("bound", strconv.Itoa(model.MaxScopeKeyBytes))
				b.plan.States = append(b.plan.States, state.WithDetail("path", model.TruncateDetail(b.overBound[d.ID])))
			}
		}
		if n := b.plan.Unplanned[unplannedNoInputs+d.ID]; n > 0 {
			for _, c := range d.Capabilities {
				state := b.partial(d, c).WithDetail("scopes_without_inputs", strconv.Itoa(n))
				b.plan.States = append(b.plan.States, state.WithDetail("scope_key", model.TruncateDetail(b.noInputs[d.ID])))
			}
		}
	}
}

// partial is the empty degradation row every refusal above fills in.
func (b *builder) partial(d model.ProviderDescriptor, capability string) model.CapabilityState {
	return model.CapabilityState{ProviderID: d.ID, Capability: capability, Scope: provider.ScopeWorkspace,
		State: model.CapabilityPartial, DiagnosticCode: model.CodeProviderOutputInvalid}
}

// previous answers, for one scope, whether the previous generation's sealed
// unit reproduces this identity exactly (reuse) and what its unit was (the
// delta predecessor). A previous unit that is not sealed is neither: failed
// and building output is invisible and can never be a member or a predecessor.
func (b *builder) previous(ctx context.Context, providerID, scopeKey string, id model.UnitID) (bool, model.UnitID, error) {
	prev, err := b.selected(ctx, providerID, scopeKey)
	if err != nil || prev == "" {
		return false, "", err
	}
	state, exists, err := b.in.Store.UnitState(ctx, prev)
	if err != nil {
		return false, "", err
	}
	if !exists || state != model.UnitSealed {
		return false, "", nil
	}
	return prev == id, prev, nil
}

// selected reads the previous generation's unit for a scope, answering the
// empty unit when there is none.
func (b *builder) selected(ctx context.Context, providerID, scopeKey string) (model.UnitID, error) {
	if b.in.PrevGen == 0 {
		return "", nil
	}
	prev, err := b.in.Store.SelectedUnit(ctx, b.in.PrevGen, providerID, scopeKey)
	if err != nil {
		// Storage marks "the generation selects no such unit" with this detail
		// (sqlite.ReasonNotFound), which is what distinguishes it from a
		// malformed argument under the same code.
		var typed *model.Error
		if errors.As(err, &typed) && typed.Details["reason"] == sqlite.ReasonNotFound {
			return "", nil
		}
		return "", err
	}
	return prev, nil
}

// foldVersion is UnitSpec.ProviderVersion: the descriptor's static version
// folded with the version detection actually observed, so a unit is keyed by
// the tool that produced it while Descriptor() stays static.
//
// The two are joined while the join fits model.MaxIdentifierBytes, because
// both halves are provenance every evidence row carries, and digested only
// when it does not. Truncating the join is the bug scip.go:363 names by hand:
// it silently drops whatever sorts last, so replacing that payload would
// change no unit key at all.
func foldVersion(descriptor, observed string) (string, error) {
	if descriptor == "" {
		return "", invalid("a provider descriptor carries no version")
	}
	if observed == "" || observed == descriptor {
		return descriptor, nil
	}
	if joined := descriptor + "+" + observed; len(joined) <= model.MaxIdentifierBytes {
		return joined, nil
	}
	return model.H(domainProviderVersion, descriptor, observed), nil
}

// domainProviderVersion separates this digest from every other digest in the
// product, so one can never be presented as another.
const domainProviderVersion = "unit-provider-version-v1"

// compareID orders file identities, which are fixed-width lowercase hex, so
// byte order is identity order.
func compareID(a, b model.FileID) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

func invalid(msg string) error { return &model.Error{Code: model.CodeArgumentInvalid, Message: msg} }

// staticInputs is the Unit.Inputs sequence of a list already in heap: a file
// unit's single input, or a package-scoped unit's member list.
func staticInputs(inputs []model.UnitInput) func(yield func(model.UnitInput) error) error {
	return func(yield func(model.UnitInput) error) error {
		for _, in := range inputs {
			if err := yield(in); err != nil {
				return err
			}
		}
		return nil
	}
}

// encodeInput / decodeInput are the external sort's codec. JSON and not a
// packed encoding because model.UnitInput already carries the field tags, the
// record never leaves this process, and a codec that cannot drift from the
// struct is worth more here than the bytes a packed one would save.
func encodeInput(in model.UnitInput) ([]byte, error) {
	b, err := json.Marshal(in)
	if err != nil {
		return nil, internalErr("encoding a unit input for the plan's external sort: " + err.Error())
	}
	return b, nil
}

func decodeInput(b []byte) (model.UnitInput, error) {
	var in model.UnitInput
	if err := json.Unmarshal(b, &in); err != nil {
		return model.UnitInput{}, internalErr("decoding a unit input from the plan's external sort: " + err.Error())
	}
	return in, nil
}

// sizeOfInput charges one buffered input against the run's byte budget: the
// two id strings plus the slice header and the two string headers the buffer
// holds them in. It is the retained heap of one record, not its encoded
// length; the encoded form is written straight to the run file and never
// accumulates.
func sizeOfInput(in model.UnitInput) int64 {
	const overhead = 64
	return int64(len(in.FileID)+len(in.ContentHash)) + overhead
}

// compareInput orders the sort by the same key model.UnitInputHasher requires,
// so the merged run is exactly the order the in-heap sort produced and the
// folded input digest -- which is UnitSpec identity -- stays byte for byte the
// same. File identities are unique within a snapshot (one manifest row per
// path) and the hasher refuses a non-ascending pair, so no tie is reachable.
func compareInput(a, b model.UnitInput) int { return compareID(a.FileID, b.FileID) }

// fileUnitRecord is one file-invalidated provider's unit as the plan's unit
// sort spills it. It is scalars only -- one file's identity, not an input list
// -- which is what makes the record a fixed, small size: a unit carrying its
// own membership would be variable-length, would charge the run buffer by the
// scope's size, and a large enough one would exceed the sort's record ceiling
// and fail the plan outright. Semantic units are never spilled for exactly
// that reason; their lists are bounded by the scope count and stay in heap.
type fileUnitRecord struct {
	// Order is the provider's position in Selection.Active and Seq its arrival
	// in the manifest walk. Together they are the sort key, and sorting on them
	// reproduces the order the in-heap per-provider lists were concatenated in.
	Order int   `json:"o"`
	Seq   int64 `json:"q"`

	ProviderID      string `json:"p"`
	ProviderVersion string `json:"v"`
	ScopeKey        string `json:"s"`

	// FileID, ContentHash and Executable are the unit's single declared input.
	FileID      model.FileID `json:"f"`
	ContentHash string       `json:"c"`
	Executable  bool         `json:"x,omitempty"`
	// DependsOn is the filesystem unit of the same file, empty for a provider
	// that does not depend on it. A file unit never has more than one
	// dependency (fileUnits sets exactly the filesystem unit or none).
	DependsOn model.UnitID `json:"d,omitempty"`
}

// unit rehydrates the record into the Unit the executor runs. Its identity is
// unchanged: Spec folds the same single input and the same dependency list.
func (r fileUnitRecord) unit() Unit {
	u := Unit{ProviderID: r.ProviderID, ProviderVersion: r.ProviderVersion, ScopeKey: r.ScopeKey,
		InputCount: 1, Inputs: staticInputs([]model.UnitInput{{FileID: r.FileID,
			ContentHash: r.ContentHash, Executable: r.Executable}})}
	if r.DependsOn != "" {
		u.DependsOn = []model.UnitID{r.DependsOn}
	}
	return u
}

// encodeUnit / decodeUnit are the unit sort's codec, JSON for the same reason
// the input codec is: the record never leaves this process, and a codec that
// cannot drift from the struct is worth more than the bytes a packed one saves.
func encodeUnit(r fileUnitRecord) ([]byte, error) {
	b, err := json.Marshal(r)
	if err != nil {
		return nil, internalErr("encoding a planned unit for the plan's external sort: " + err.Error())
	}
	return b, nil
}

func decodeUnit(b []byte) (fileUnitRecord, error) {
	var r fileUnitRecord
	if err := json.Unmarshal(b, &r); err != nil {
		return fileUnitRecord{}, internalErr("decoding a planned unit from the plan's external sort: " + err.Error())
	}
	return r, nil
}

// sizeOfUnit charges one buffered unit record against the run's byte budget:
// its five variable strings plus the struct's fixed fields and their headers.
// It is the retained heap of one record, not its encoded length.
func sizeOfUnit(r fileUnitRecord) int64 {
	const overhead = 128
	return int64(len(r.ProviderID)+len(r.ProviderVersion)+len(r.ScopeKey)+
		len(r.FileID)+len(r.ContentHash)+len(r.DependsOn)) + overhead
}

// compareUnit orders the unit sort by (provider position, arrival), which is
// the concatenation order Plan.Units answers in. Every pair differs in Seq --
// it is a per-plan counter incremented once per record -- so no tie is
// reachable and the merge needs no stable-sort guarantee to be deterministic.
func compareUnit(a, b fileUnitRecord) int {
	switch {
	case a.Order != b.Order:
		if a.Order < b.Order {
			return -1
		}
		return 1
	case a.Seq < b.Seq:
		return -1
	case a.Seq > b.Seq:
		return 1
	}
	return 0
}
