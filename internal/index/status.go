package index

import (
	"context"
	"errors"
	"log/slog"
	"slices"
	"strconv"
	"sync"
	"time"

	"github.com/Sawmonabo/codectx/internal/model"
	"github.com/Sawmonabo/codectx/internal/provider"
	"github.com/Sawmonabo/codectx/internal/vcs/git"
)

// statusLeaseTTL is how long the status read pins the generation it projects.
// It is short because nothing is served from the pin: it exists so the
// generation cannot be collected between the pointer read and the capability
// read (Section 12.3).
const statusLeaseTTL = 30 * time.Second

// maxStatusChanges bounds the worktree comparison. Status answers one bit --
// did anything move since the capture -- so it stops at the first change and
// never enumerates the worktree.
const maxStatusChanges = 1

// errWorktreeChanged stops the bounded worktree comparison at its first hit.
var errWorktreeChanged = errors.New("index: worktree changed")

// Status projects the active generation (Sections 13.2, 13.3). It runs no
// provider, captures nothing and never publishes: a status call on a busy
// workspace is a read.
func (c *Coordinator) Status(ctx context.Context) (model.IndexStatus, error) {
	gen, err := c.opts.Store.ActiveGeneration(ctx, c.repo)
	if err != nil {
		return model.IndexStatus{}, err
	}
	pinned, err := c.opts.Store.PinGeneration(ctx, c.repo, gen, statusLeaseTTL)
	if err != nil {
		return model.IndexStatus{}, err
	}
	defer pinned.Close()
	binding := pinned.Binding()
	states, err := pinned.Capabilities(ctx)
	if err != nil {
		return model.IndexStatus{}, err
	}
	snap, err := c.opts.Store.Snapshot(ctx, binding.SnapshotID)
	if err != nil {
		return model.IndexStatus{}, err
	}
	coherence, warnings, err := c.coherence(ctx, snap)
	if err != nil {
		return model.IndexStatus{}, err
	}
	st := model.IndexStatus{Binding: binding, Health: healthOf(states), Coherence: coherence,
		CaptureConsistency: snap.CaptureConsistency, Completeness: states,
		FileCount: snap.FileCount, SourceBytes: snap.SourceBytes, Warnings: warnings}
	c.watch.project(&st)
	return st, nil
}

// coherence answers what the active generation is coherent with (Section
// 13.3). A moved HEAD or any worktree change makes it `worktree_changed`; the
// snapshot itself is always coherent with its own bytes.
//
// A non-Git workspace is reported coherent with an explicit warning rather
// than compared: the comparison would be a full walk of the repository, and
// Section 13.3 forbids inferring freshness. The warning says the check was not
// performed, which is the honest answer.
func (c *Coordinator) coherence(ctx context.Context, snap model.Snapshot) (model.Coherence, []string, error) {
	if !c.opts.Root.HasGit || c.opts.Git == nil {
		return model.CoherenceSnapshot, []string{"worktree coherence is not checked for a workspace that is not a Git repository"}, nil
	}
	head, err := c.opts.Git.Head(ctx, c.opts.Root.Path)
	if err != nil {
		return "", nil, err
	}
	if head != snap.HeadObjectID {
		return model.CoherenceWorktreeChange, nil, nil
	}
	err = c.opts.Git.Status(ctx, c.opts.Root.Path, c.opts.Config.Workspace.IncludeUntracked, maxStatusChanges,
		func(git.Change) error { return errWorktreeChanged })
	if errors.Is(err, errWorktreeChanged) {
		return model.CoherenceWorktreeChange, nil, nil
	}
	if err != nil {
		var typed *model.Error
		if errors.As(err, &typed) && typed.Code == model.CodeResourceLimit {
			// The bound is one change: reaching it is the answer, not a
			// failure.
			return model.CoherenceWorktreeChange, nil, nil
		}
		return "", nil, err
	}
	return model.CoherenceSnapshot, nil, nil
}

// watchState is what the coordinator's own reconciliation loop knows about
// watch coverage. The filesystem-notification watcher is composed above this
// package (internal/index/watch, driven by internal/app), so what is reported
// here is exactly what this loop provides: periodic reconciliation, which is
// never complete notification coverage, and the time it last succeeded.
type watchState struct {
	mu         sync.Mutex
	active     int
	reconciled time.Time
}

func (w *watchState) enter() {
	w.mu.Lock()
	w.active++
	w.mu.Unlock()
}

func (w *watchState) leave() {
	w.mu.Lock()
	w.active--
	w.mu.Unlock()
}

func (w *watchState) reconciledAt(t time.Time) {
	w.mu.Lock()
	w.reconciled = t
	w.mu.Unlock()
}

// project fills the Section 13.2 watch fields. WatchComplete is false while
// the only coverage is this loop: reporting complete coverage for periodic
// reconciliation would claim notification coverage the product does not have.
func (w *watchState) project(st *model.IndexStatus) {
	w.mu.Lock()
	defer w.mu.Unlock()
	st.WatchActive = w.active > 0
	if !w.reconciled.IsZero() {
		at := w.reconciled
		st.LastReconciledAt = &at
	}
	if st.WatchActive {
		st.Warnings = append(st.Warnings,
			"watch coverage is periodic reconciliation only; filesystem notification coverage is reported by the watcher that drives refresh")
	}
}

// capabilityReport accumulates one generation's capability rows and publishes
// a bounded, deterministic list. Rows are bounded by model.MaxCapabilityStates
// whatever the repository holds: a per-unit failure or carry is aggregated
// rather than published one row per unit, because the list must fit the same
// bound on a repository with one unit and one with ten thousand.
type capabilityReport struct {
	order    []string
	rows     map[string]model.CapabilityState
	failures map[string]*failureRow
	carried  []model.CapabilityState
}

// failureRow aggregates every failed unit of one provider capability: how many
// failed, one exemplar scope, and the diagnostic they carried.
type failureRow struct {
	providerID, capability, scope, code string
	units                               int
}

func newCapabilityReport() *capabilityReport {
	return &capabilityReport{rows: map[string]model.CapabilityState{}, failures: map[string]*failureRow{}}
}

func stateKey(providerID, capability, scope string, state model.CapabilityStateValue) string {
	return providerID + "\x00" + capability + "\x00" + scope + "\x00" + string(state)
}

// add records one capability row.
//
// A `fresh` row is folded into one row per provider capability at workspace
// scope, counting the scopes it covers. Every provider publishes one such row
// per unit it sealed, so keeping them per scope would fill the whole report
// with "this file worked" on any repository past a few hundred files and push
// the degradations -- the only rows a reader acts on -- past the bound. A row
// that is not fresh keeps its scope: which file or package is stale, partial
// or failed is exactly what the reader needs.
func (r *capabilityReport) add(s model.CapabilityState) {
	if s.State == model.CapabilityFresh {
		s.Scope = provider.ScopeWorkspace
		s.DiagnosticCode = ""
		s.Details = nil
	}
	key := stateKey(s.ProviderID, s.Capability, s.Scope, s.State)
	if existing, ok := r.rows[key]; ok {
		r.rows[key] = existing.WithDetail("scopes", strconv.Itoa(countDetail(existing)+1))
		return
	}
	r.order = append(r.order, key)
	r.rows[key] = s
}

// countDetail reads the scope count a folded row carries; a row folded for the
// first time carries none and counts as one.
func countDetail(s model.CapabilityState) int {
	if n, err := strconv.Atoi(s.Details["scopes"]); err == nil {
		return n
	}
	return 1
}

// addFresh publishes a healthy row for a capability nothing else reported on.
// A provider that is partial or failed somewhere keeps that row instead: a
// fresh row beside it would let a reader pick whichever it saw first.
func (r *capabilityReport) addFresh(providerID, capability string) {
	if r.reported(providerID, capability) {
		return
	}
	r.add(model.CapabilityState{ProviderID: providerID, Capability: capability,
		Scope: provider.ScopeWorkspace, State: model.CapabilityFresh})
}

// addUnavailable publishes the degradation of residual 113: a provider that
// detection found available and that produced no unit at all. "Available, zero
// units" is not fresh coverage, and without this row the capability would be
// missing from the report entirely, which reads as nothing to worry about.
func (r *capabilityReport) addUnavailable(providerID, capability string) {
	if r.reported(providerID, capability) {
		return
	}
	r.add(model.CapabilityState{ProviderID: providerID, Capability: capability,
		Scope: provider.ScopeWorkspace, State: model.CapabilityUnavailable,
		DiagnosticCode: model.CodeProviderUnavailable,
		Details:        map[string]string{"reason": "no_units_planned"}})
}

// addDeferred publishes the row of a capability whose only work is running in
// the background (Section 11.6, ruling Q9): it has no member in this
// generation and no previous unit to carry, so there is nothing to answer from
// yet. A carried predecessor reports `stale` through addCarried instead, and
// that row wins because it is recorded first.
func (r *capabilityReport) addDeferred(providerID, capability string) {
	if r.reported(providerID, capability) {
		return
	}
	r.add(model.CapabilityState{ProviderID: providerID, Capability: capability,
		Scope: provider.ScopeWorkspace, State: model.CapabilityUnavailable,
		DiagnosticCode: model.CodeProviderUnavailable,
		Details:        map[string]string{"reason": "units_deferred"}})
}

// addFailure records one failed unit of an optional provider.
func (r *capabilityReport) addFailure(providerID, capability, scope, code string) {
	key := providerID + "\x00" + capability
	row, ok := r.failures[key]
	if !ok {
		row = &failureRow{providerID: providerID, capability: capability, scope: scope, code: code}
		r.failures[key] = row
	}
	row.units++
}

// addCarried records the provenance distance of one carried stale scope
// (Section 13.3, ruling Q4).
func (r *capabilityReport) addCarried(providerID, capability, scope string, generations, files int) {
	r.carried = append(r.carried, model.CapabilityState{ProviderID: providerID, Capability: capability,
		Scope: scope, State: model.CapabilityStale, DiagnosticCode: model.CodeSnapshotChanged,
		Details: map[string]string{
			"stale_generations":   strconv.Itoa(generations),
			"stale_changed_files": strconv.Itoa(files),
		}})
}

// reported answers whether anything was already published for this provider
// capability, in any state and at any scope.
func (r *capabilityReport) reported(providerID, capability string) bool {
	prefix := providerID + "\x00" + capability + "\x00"
	for _, key := range r.order {
		if len(key) > len(prefix) && key[:len(prefix)] == prefix {
			return true
		}
	}
	if _, ok := r.failures[providerID+"\x00"+capability]; ok {
		return true
	}
	for _, c := range r.carried {
		if c.ProviderID == providerID && c.Capability == capability {
			return true
		}
	}
	return false
}

// finish publishes the bounded list. The per-scope carry rows are collapsed
// per provider capability before anything is dropped, because a stale
// capability with an aggregate distance is still an honest stale answer while
// a truncated list silently loses one.
func (r *capabilityReport) finish(log *slog.Logger) []model.CapabilityState {
	out := make([]model.CapabilityState, 0, len(r.order)+len(r.failures)+len(r.carried))
	for _, key := range r.order {
		out = append(out, r.rows[key])
	}
	failures := make([]*failureRow, 0, len(r.failures))
	for _, f := range r.failures {
		failures = append(failures, f)
	}
	slices.SortFunc(failures, func(a, b *failureRow) int {
		if a.providerID != b.providerID {
			return compareString(a.providerID, b.providerID)
		}
		return compareString(a.capability, b.capability)
	})
	for _, f := range failures {
		out = append(out, model.CapabilityState{ProviderID: f.providerID, Capability: f.capability,
			Scope: provider.ScopeWorkspace, State: model.CapabilityFailed, DiagnosticCode: f.code,
			Details: map[string]string{"units_failed": strconv.Itoa(f.units), "scope_key": model.TruncateDetail(f.scope)}})
	}
	carried := slices.Clone(r.carried)
	slices.SortFunc(carried, func(a, b model.CapabilityState) int {
		if a.ProviderID != b.ProviderID {
			return compareString(a.ProviderID, b.ProviderID)
		}
		if a.Capability != b.Capability {
			return compareString(a.Capability, b.Capability)
		}
		return compareString(a.Scope, b.Scope)
	})
	if len(out)+len(carried) > model.MaxCapabilityStates {
		carried = collapseCarried(carried)
	}
	out = append(out, carried...)
	if len(out) > model.MaxCapabilityStates {
		// The per-scope rows are folded to one row per provider capability and
		// state before anything is dropped: an aggregate degradation with a
		// count is still an honest degradation, a truncated list is not.
		out = collapseScopes(out)
	}
	if len(out) > model.MaxCapabilityStates {
		log.Warn("the capability report exceeded its bound and was truncated", "component", component,
			"rows", len(out), "limit", model.MaxCapabilityStates)
		out = out[:model.MaxCapabilityStates]
	}
	return out
}

// collapseScopes folds every per-scope row of one provider capability and
// state into one workspace-scoped row carrying the scope count and one
// exemplar scope.
func collapseScopes(rows []model.CapabilityState) []model.CapabilityState {
	var order []string
	folds := map[string]model.CapabilityState{}
	counts := map[string]int{}
	for _, c := range rows {
		key := c.ProviderID + "\x00" + c.Capability + "\x00" + string(c.State)
		if _, ok := folds[key]; !ok {
			folded := c
			if c.Scope != provider.ScopeWorkspace {
				folded = c.WithDetail("scope_key", model.TruncateDetail(c.Scope))
				folded.Scope = provider.ScopeWorkspace
			}
			folds[key] = folded
			order = append(order, key)
		}
		counts[key] += countDetail(c)
	}
	out := make([]model.CapabilityState, 0, len(order))
	for _, key := range order {
		out = append(out, folds[key].WithDetail("scopes", strconv.Itoa(counts[key])))
	}
	return out
}

// collapseCarried folds every carried scope of one provider capability into
// one row carrying the scope count and the greatest distance, which is the
// distance a reader must assume for that capability.
func collapseCarried(carried []model.CapabilityState) []model.CapabilityState {
	type fold struct {
		row                 model.CapabilityState
		scopes, gens, files int
	}
	var order []string
	folds := map[string]*fold{}
	for _, c := range carried {
		key := c.ProviderID + "\x00" + c.Capability
		f, ok := folds[key]
		if !ok {
			f = &fold{row: c}
			folds[key] = f
			order = append(order, key)
		}
		f.scopes++
		f.gens = max(f.gens, atoiDetail(c, "stale_generations"))
		f.files = max(f.files, atoiDetail(c, "stale_changed_files"))
	}
	out := make([]model.CapabilityState, 0, len(order))
	for _, key := range order {
		f := folds[key]
		out = append(out, model.CapabilityState{ProviderID: f.row.ProviderID, Capability: f.row.Capability,
			Scope: provider.ScopeWorkspace, State: model.CapabilityStale, DiagnosticCode: f.row.DiagnosticCode,
			Details: map[string]string{"stale_scopes": strconv.Itoa(f.scopes),
				"stale_generations": strconv.Itoa(f.gens), "stale_changed_files": strconv.Itoa(f.files)}})
	}
	return out
}

func atoiDetail(s model.CapabilityState, key string) int {
	n, err := strconv.Atoi(s.Details[key])
	if err != nil {
		return 0
	}
	return n
}

func compareString(a, b string) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}
