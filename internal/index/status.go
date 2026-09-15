package index

import (
	"context"
	"errors"
	"log/slog"
	"slices"
	"strconv"
	"sync"
	"time"

	"github.com/Sawmonabo/codectx/internal/index/watch"
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
// workspace is a read, which is why it is the one entry point legal without
// the workspace indexing lock.
//
// The composition-time rows of Options.States are folded into the published
// completeness and the whole list is brought back inside its bound here, so a
// capability whose provider could not be constructed is reported even by a
// generation published before it failed, and the answer still validates.
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
	states = composedStates(states, c.opts.States)
	states, omitted := boundStates(states, c.log)
	coherence, warnings, err := c.coherence(ctx, snap)
	if err != nil {
		return model.IndexStatus{}, err
	}
	if omitted > 0 {
		// Section 18.2: a truncated report shows what it omitted. A renderer
		// prints what it is handed, so the count has to be in the result.
		warnings = append(warnings, "the capability report holds more than its bound of "+
			strconv.Itoa(model.MaxCapabilityStates)+" rows; "+strconv.Itoa(omitted)+
			" of the least severe were omitted")
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

// watchState is what the running watch loops know about watch coverage. When a
// notification watcher drives the loop, coverage is the watcher's own answer;
// without one the only coverage is periodic reconciliation, which is never
// complete notification coverage, and what is reported is exactly that.
type watchState struct {
	mu         sync.Mutex
	active     int
	source     *watch.Watcher
	reconciled time.Time
}

// enter records one running watch and the notification source driving it, if
// any. Concurrent watches over one coordinator are not a supported
// composition, but the counter makes a second one visible rather than letting
// the first one's exit report "watch off" while it still runs.
func (w *watchState) enter(source *watch.Watcher) {
	w.mu.Lock()
	w.active++
	if source != nil {
		w.source = source
	}
	w.mu.Unlock()
}

func (w *watchState) leave() {
	w.mu.Lock()
	w.active--
	if w.active == 0 {
		w.source = nil
	}
	w.mu.Unlock()
}

func (w *watchState) reconciledAt(t time.Time) {
	w.mu.Lock()
	w.reconciled = t
	w.mu.Unlock()
}

// project fills the Section 13.2 watch fields. With a notification watcher
// running they are the watcher's own Coverage(); without one WatchComplete is
// false and the warning says so, because reporting complete coverage for
// periodic reconciliation would claim notification coverage the product does
// not have.
func (w *watchState) project(st *model.IndexStatus) {
	w.mu.Lock()
	defer w.mu.Unlock()
	st.WatchActive = w.active > 0
	at := w.reconciled
	if w.source != nil {
		complete, pending, lastReconciled := w.source.Coverage()
		st.WatchComplete = complete
		st.PendingPaths = int64(pending)
		if lastReconciled.After(at) {
			at = lastReconciled
		}
	}
	if !at.IsZero() {
		st.LastReconciledAt = &at
	}
	if st.WatchActive && !st.WatchComplete {
		st.Warnings = append(st.Warnings,
			"watch coverage is incomplete: not every directory the traversal admits carries a filesystem notification watch, so changes under the rest are seen only at the next periodic reconciliation")
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

// newCapabilityReport seeds a report with the composition-time rows of
// Options.States. A provider that could not be constructed never reaches the
// registry, so nothing on the indexing path would otherwise report its
// capabilities at all, and the operator would have to run a second, different
// command to learn that a capability is unavailable. Seeding here rather than
// merging above this package is what puts those rows inside the
// MaxCapabilityStates bound instead of past it.
func (c *Coordinator) newCapabilityReport() *capabilityReport {
	r := newCapabilityReport()
	for _, st := range c.opts.States {
		r.add(st)
	}
	return r
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

// addDeferred publishes the row of a capability whose work is still running in
// the background (Section 11.6, ruling Q9): the deferred scope has no member in
// this generation, so the capability cannot be reported as fresh coverage.
//
// A `fresh` row already recorded for one of the provider's other scopes is
// demoted rather than left to win: it was added while the run was in flight,
// and keeping it would publish "this capability is fresh" for a capability one
// of whose scopes has nothing behind it at all -- the over-claim Section 13.3
// forbids. Any other row (stale, partial, failed) already under-claims and
// stays.
func (r *capabilityReport) addDeferred(providerID, capability string) {
	key := stateKey(providerID, capability, provider.ScopeWorkspace, model.CapabilityFresh)
	if _, ok := r.rows[key]; ok {
		delete(r.rows, key)
		r.order = slices.DeleteFunc(r.order, func(k string) bool { return k == key })
	}
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
	switch {
	case !ok:
		row = &failureRow{providerID: providerID, capability: capability, scope: scope, code: code}
		r.failures[key] = row
	case scope < row.scope:
		// The exemplar is the lexicographically first scope, never the first
		// to arrive: the units of one provider are built concurrently, and
		// both details_json and diagnostic_code fold into the AnalysisKey, so
		// an arrival-ordered exemplar would key two identical runs
		// differently. The code travels with the scope it belongs to -- a
		// published row naming one scope's key and another scope's failure
		// reason is arrival-ordered again, in a shape that also misreports.
		row.scope, row.code = scope, code
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

// finish publishes the bounded list and how many rows the bound omitted. The
// per-scope carry rows are collapsed per provider capability before anything is
// dropped, because a stale capability with an aggregate distance is still an
// honest stale answer while a truncated list silently loses one.
func (r *capabilityReport) finish(log *slog.Logger) ([]model.CapabilityState, int) {
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
	return boundStates(out, log)
}

// foldToPrimaryKey brings the assembled list inside the one row per
// (provider_id, capability, scope_key) that generation_capabilities is keyed
// by -- its primary key does not carry the state. The three assembly buckets
// above key their own rows by state as well, and the bound's own collapse
// keys by state too, so one provider capability reaches here more than once at
// one scope in four shapes, each of which failed the insert and took `codectx
// index` down on a constraint error instead of publishing the degraded
// generation it had built:
//
//   - a sibling unit that succeeded folds a `fresh` row to the workspace scope
//     (add) while the unit that failed publishes the `failed` fold here;
//   - the planner's and the registry's `partial` degradations are workspace
//     scoped too (plan.builder.partial, provider detection), so they collide
//     with that same `failed` fold;
//   - a carried scope whose key is itself the workspace scope -- a dependence
//     family planned as one whole-workspace unit -- publishes `stale` beside
//     either of those;
//   - above the bound, collapseScopes folds by provider capability AND state
//     and rewrites every fold's scope to the workspace scope, so two states of
//     one provider capability that fold separately come back out as two
//     workspace-scoped rows (collapseCarried can add a third). That shape is
//     regenerated after the first fold, which is why boundStates folds again
//     on the far side of the collapse rather than trusting its input.
//
// The most severe row survives, ranked by the same severityRank boundStates
// truncates by: Section 13.3 lets a report under-claim and never over-claim.
// The fold keeps the first occurrence of a key and never reorders, so the
// published row is a function of the set and not of the order the concurrent
// unit workers reported in -- the determinism the exemplar choices above exist
// for, because these rows fold into the AnalysisKey.
//
// It does not make addDeferred's demotion redundant. That one deletes the
// fresh row so that reported() stops finding it and the deferred row is added
// at all; without the delete the capability publishes one `fresh` row, alone,
// and this fold has nothing to out-rank it with.
func foldToPrimaryKey(rows []model.CapabilityState) []model.CapabilityState {
	at := make(map[string]int, len(rows))
	out := make([]model.CapabilityState, 0, len(rows))
	for _, c := range rows {
		key := c.ProviderID + "\x00" + c.Capability + "\x00" + c.Scope
		if i, ok := at[key]; ok {
			if severityRank(c.State) < severityRank(out[i].State) {
				out[i] = c
			}
			continue
		}
		at[key] = len(out)
		out = append(out, c)
	}
	return out
}

// boundStates brings an assembled capability list inside
// model.MaxCapabilityStates and reports how many rows it had to omit. It is
// the one place the bound is applied: the indexing path calls it through
// finish, and Status calls it after folding in the composition-time rows, so a
// published list can never exceed the bound its own contract validates against.
//
// It is also the one place the primary key of generation_capabilities is
// closed. Every caller's list is folded to one row per (provider, capability,
// scope) on the way in, and folded again on the far side of collapseScopes,
// which keys by state and rewrites each fold to the workspace scope and so
// regenerates the very duplicates the first fold removed.
//
// When rows must go, the degradations stay. The list is first folded to one
// row per provider capability and state, and only if that is still too long is
// it ordered by severity -- failed, stale, partial, unavailable, then fresh --
// and cut from the tail. Truncating the assembly order instead would drop the
// failures and carries, which are appended last, and keep the fresh rows:
// Section 13.3 allows a report to under-claim and never to over-claim.
// Truncation only removes rows, so the folded key stays closed.
func boundStates(out []model.CapabilityState, log *slog.Logger) ([]model.CapabilityState, int) {
	out = foldToPrimaryKey(out)
	if len(out) > model.MaxCapabilityStates {
		out = foldToPrimaryKey(collapseScopes(out))
	}
	if len(out) <= model.MaxCapabilityStates {
		return out, 0
	}
	// The order is total, so which rows survive is a function of the set and
	// not of the order the unit workers happened to report in.
	slices.SortStableFunc(out, func(a, b model.CapabilityState) int {
		if r := severityRank(a.State) - severityRank(b.State); r != 0 {
			return r
		}
		if a.ProviderID != b.ProviderID {
			return compareString(a.ProviderID, b.ProviderID)
		}
		if a.Capability != b.Capability {
			return compareString(a.Capability, b.Capability)
		}
		return compareString(string(a.State), string(b.State))
	})
	omitted := len(out) - model.MaxCapabilityStates
	log.Warn("the capability report exceeded its bound and was truncated", "component", component,
		"rows", len(out), "limit", model.MaxCapabilityStates, "omitted", omitted)
	return out[:model.MaxCapabilityStates], omitted
}

// severityRank orders capability states by how much a reader must act on them.
// A fresh row is the one a truncated report can afford to lose.
func severityRank(s model.CapabilityStateValue) int {
	switch s {
	case model.CapabilityFailed:
		return 0
	case model.CapabilityStale:
		return 1
	case model.CapabilityPartial:
		return 2
	case model.CapabilityUnavailable:
		return 3
	case model.CapabilityFresh:
		return 5
	}
	return 4
}

// composedStates folds the composition-time rows of Options.States into a list
// read back from a published generation. A generation this build published
// already holds them, so the fold is keyed by provider capability and the
// published row wins: one published under an older build, or by a process
// whose provider did construct, gains the row it is missing rather than
// growing a duplicate.
func composedStates(published, composed []model.CapabilityState) []model.CapabilityState {
	if len(composed) == 0 {
		return published
	}
	have := make(map[string]bool, len(published))
	for _, s := range published {
		have[s.ProviderID+"\x00"+s.Capability] = true
	}
	out := slices.Clone(published)
	for _, s := range composed {
		if have[s.ProviderID+"\x00"+s.Capability] {
			continue
		}
		out = append(out, s)
	}
	return out
}

// collapseScopes folds every per-scope row of one provider capability and
// state into one workspace-scoped row carrying the scope count and one
// exemplar scope.
//
// The exemplar is the lexicographically first scope of the fold, never the
// first row to arrive: the rows come from unit workers that run concurrently,
// and the exemplar is published as a `scope_key` detail, which folds into
// details_json and therefore into the AnalysisKey. Two identical runs must key
// identically.
//
// The whole exemplar row is what is published, not just its scope. Its
// diagnostic code and its other details are the chosen scope's, so the fold
// describes one scope truthfully instead of pairing one scope's key with
// whichever row happened to arrive first; diagnostic_code folds into the
// AnalysisKey as well, so an arrival-ordered one is the same determinism
// defect the scope exemplar exists to prevent. Rows that are all
// workspace-scoped have no scope to choose between and are ordered by their
// code, which is likewise arrival-independent.
func collapseScopes(rows []model.CapabilityState) []model.CapabilityState {
	var order []string
	folds := map[string]model.CapabilityState{}
	counts := map[string]int{}
	scoped := map[string]bool{}
	for _, c := range rows {
		key := c.ProviderID + "\x00" + c.Capability + "\x00" + string(c.State)
		best, ok := folds[key]
		if !ok {
			order = append(order, key)
		}
		if !ok || betterExemplar(c, best, scoped[key]) {
			folds[key] = c
			scoped[key] = c.Scope != provider.ScopeWorkspace
		}
		counts[key] += countDetail(c)
	}
	out := make([]model.CapabilityState, 0, len(order))
	for _, key := range order {
		row := folds[key]
		exemplar := row.Scope
		row.Scope = provider.ScopeWorkspace
		if scoped[key] {
			row = row.WithDetail("scope_key", model.TruncateDetail(exemplar))
		}
		out = append(out, row.WithDetail("scopes", strconv.Itoa(counts[key])))
	}
	return out
}

// betterExemplar answers whether candidate should replace the exemplar chosen
// so far, whose scope is a real one when haveScoped is set. A real scope always
// beats the workspace scope, which names no scope at all; between two real ones
// and between two workspace ones the order is lexicographic, on the scope first
// and on the diagnostic code when the scopes are equal.
func betterExemplar(candidate, best model.CapabilityState, haveScoped bool) bool {
	scoped := candidate.Scope != provider.ScopeWorkspace
	if scoped != haveScoped {
		return scoped
	}
	if candidate.Scope != best.Scope {
		return candidate.Scope < best.Scope
	}
	return candidate.DiagnosticCode < best.DiagnosticCode
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
