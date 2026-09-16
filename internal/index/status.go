package index

import (
	"context"
	"errors"
	"log/slog"
	"slices"
	"strconv"
	"strings"
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
	states, aggregated := boundStates(states, c.log)
	coherence, warnings, err := c.coherence(ctx, snap)
	if err != nil {
		return model.IndexStatus{}, err
	}
	if aggregated {
		// Section 18.2: a report that aggregated says so. Nothing was omitted
		// -- every row still names its capability and the number of scopes it
		// speaks for -- but a reader must know the scope keys are exemplars.
		warnings = append(warnings, "the capability report holds more than "+
			strconv.Itoa(model.MaxCapabilityStates)+" rows and was aggregated per provider "+
			"capability; every row carries the number of scopes it stands for and none was omitted")
	}
	st := model.IndexStatus{Binding: binding, Health: healthOf(states), Coherence: coherence,
		CaptureConsistency: snap.CaptureConsistency, Completeness: states,
		FileCount: snap.FileCount, SourceBytes: snap.SourceBytes, Warnings: warnings}
	c.watch.project(&st)
	c.retention.project(&st)
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

// observe reads what the running watch loops know, under w.mu: whether a watch
// is active, whether its coverage is complete notification coverage, how many
// notification events are pending, and when a pass last completed.
//
// pending and at are absent rather than zero when nothing measured them. A
// periodic-only watch has no notification queue to count and a watch that has
// completed no pass has no pass time; `0` and the epoch would state that the
// watch is caught up. The two readers below -- the in-process status projection
// and the cross-process heartbeat -- must answer from one rule, because a
// second copy of it would let `codectx status` in this process and `codectx
// status` in another disagree about the same watch.
func (w *watchState) observe() (active bool, complete bool, pending *int64, at time.Time) {
	at = w.reconciled
	if w.source != nil {
		coverageComplete, pendingPaths, lastReconciled := w.source.Coverage()
		complete = coverageComplete
		n := int64(pendingPaths)
		pending = &n
		if lastReconciled.After(at) {
			at = lastReconciled
		}
	}
	return w.active > 0, complete, pending, at
}

// project fills the Section 13.2 watch fields. With a notification watcher
// running they are the watcher's own Coverage(); without one WatchComplete is
// false and the warning says so, because reporting complete coverage for
// periodic reconciliation would claim notification coverage the product does
// not have.
func (w *watchState) project(st *model.IndexStatus) {
	w.mu.Lock()
	defer w.mu.Unlock()
	active, complete, pending, at := w.observe()
	st.WatchActive = active
	st.WatchComplete = complete
	if pending != nil {
		st.PendingPaths = *pending
	}
	if !at.IsZero() {
		st.LastReconciledAt = &at
	}
	if st.WatchActive && !st.WatchComplete {
		st.Warnings = append(st.Warnings,
			"watch coverage is incomplete: not every directory the traversal admits carries a filesystem notification watch, so changes under the rest are seen only at the next periodic reconciliation")
	}
}

// heartbeat is what this watch publishes for another process to read: when its
// last pass completed and how many events are pending, both absent when nothing
// measured them.
func (w *watchState) heartbeat() (lastPass *time.Time, pending *int64) {
	w.mu.Lock()
	defer w.mu.Unlock()
	_, _, pending, at := w.observe()
	if !at.IsZero() {
		lastPass = &at
	}
	return lastPass, pending
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
	// covered records that another scope of this provider capability DID
	// publish into this generation. A capability with facts in it is partial,
	// never failed: Section 13.3 lets a report under-claim, and reporting a
	// capability that answers queries as failed is the opposite.
	covered bool
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
		s = withoutScopeNamingDetails(s)
	}
	key := stateKey(s.ProviderID, s.Capability, s.Scope, s.State)
	if existing, ok := r.rows[key]; ok {
		merged := mergeDetails(existing, s)
		r.rows[key] = merged.WithDetail(scopesDetail, strconv.Itoa(countDetail(existing)+countDetail(s)))
		return
	}
	r.order = append(r.order, key)
	// No scopesDetail here: a row folded for the first time stands for one
	// scope, which countDetail already reads from its absence. Writing
	// `scopes=1` on every row would put a word that says nothing on every line
	// of the report. What makes the count survive is that the key is RESERVED,
	// not that it is written early.
	r.rows[key] = s
}

// scopeNamingDetails are the details whose value names the one scope the row
// was reported at: the fold rewrites a fresh row to the workspace scope, so
// they would name a scope the published row no longer stands for.
//
// They are also the details a merge must not join. Every other detail is a
// count or a reason, and two of them are both true of the merged row; a scope
// name is an exemplar -- joining one row's scope name onto another row's
// diagnostic code publishes a row that pairs one scope's key with another
// scope's reason, the misreport collapseScopes chooses its exemplar row whole
// to avoid.
var scopeNamingDetails = map[string]bool{scopeKeyDetail: true, "unit_id": true, "file_id": true, "path": true}

const (
	// scopeKeyDetail carries the exemplar scope of a fold.
	scopeKeyDetail = "scope_key"
	// scopesDetail counts the scopes a folded row stands for.
	//
	// It, unitsFailedDetail and truncatedDetail are the model's RESERVED
	// capability-detail keys: model.CapabilityState.WithDetail holds room for
	// them back from the provider budget, so this fold's own bookkeeping can
	// never be the entry evicted to make room for a provider detail. Naming
	// them from model rather than respelling them here is what makes that
	// guarantee apply to these writes.
	scopesDetail = model.DetailScopes
	// unitsFailedDetail counts the units that failed behind one row.
	unitsFailedDetail = model.DetailUnitsFailed
	// truncatedDetail names the merged details that did not fit
	// model.MaxDetailBytes, so a clipped value is never published as if it
	// were whole.
	truncatedDetail = model.DetailDetailsTruncated
	// detailSeparator joins the values of a merged multi-valued detail.
	detailSeparator = ","
)

// withoutScopeNamingDetails drops the scope-naming details of a row whose
// scope the fold has just rewritten, and keeps every other detail.
//
// Only those keys go. A fresh row carries its degradations here -- how many
// oversize records were admitted, which fields were truncated to fit a stored
// ceiling -- and clearing the whole map made state the only channel that
// survived the fold, which is why a provider with nothing worse than an
// admitted-oversize count had to publish `partial` to be heard at all. The
// per-unit noise the fold exists to keep out of the report is the scope name
// itself: one row per file saying which file it was.
func withoutScopeNamingDetails(s model.CapabilityState) model.CapabilityState {
	kept := 0
	for k := range s.Details {
		if !scopeNamingDetails[k] {
			kept++
		}
	}
	if kept == len(s.Details) {
		return s
	}
	if kept == 0 {
		s.Details = nil
		return s
	}
	details := make(map[string]string, kept)
	for k, v := range s.Details {
		if !scopeNamingDetails[k] {
			details[k] = v
		}
	}
	s.Details = details
	return s
}

// mergeDetails folds other's details into row's, so a fold publishes what both
// rows knew instead of only the first or the most severe one's.
//
// The merge is a function of the two maps and not of the order they arrived
// in, which these rows require: details_json folds into the AnalysisKey, and
// the rows come from unit workers that run concurrently. Equal values stay as
// they are; two integers sum, because every numeric detail here is a count of
// units, scopes, records or bytes and the merged row stands for both sets; any
// other pair is deduped and joined in sorted order, so two reasons are both
// reported and a reason repeated by ten units is reported once. A
// scope-naming detail keeps the receiving row's value: that row's diagnostic
// code is the one being published, so its exemplar scope must be too.
//
// A joined value is bounded by model.TruncateField and a cut one is named in
// the `details_truncated` detail. WithDetail bounds a value silently; a reader
// that acts on a list of reasons must know the list is not the whole list.
func mergeDetails(row, other model.CapabilityState) model.CapabilityState {
	if len(other.Details) == 0 {
		return row
	}
	keys := make([]string, 0, len(other.Details))
	for k := range other.Details {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	var cut []string
	for _, k := range keys {
		have, ok := row.Details[k]
		if !ok {
			row = row.WithDetail(k, other.Details[k])
			continue
		}
		value, truncated := mergeDetailValue(k, have, other.Details[k])
		if truncated {
			cut = append(cut, k)
		}
		row = row.WithDetail(k, value)
	}
	if len(cut) > 0 {
		flagged, _ := mergeDetailValue(truncatedDetail, row.Details[truncatedDetail], strings.Join(cut, detailSeparator))
		row = row.WithDetail(truncatedDetail, flagged)
	}
	return row
}

// mergeDetailValue combines the two values one detail key carries and reports
// whether the result had to be cut to fit model.MaxDetailBytes.
func mergeDetailValue(key, a, b string) (value string, truncated bool) {
	if a == b {
		return a, false
	}
	if scopeNamingDetails[key] {
		return a, false
	}
	if x, err := strconv.Atoi(a); err == nil {
		if y, err := strconv.Atoi(b); err == nil {
			return strconv.Itoa(x + y), false
		}
	}
	return joinDetailValues(a, b)
}

// joinDetailValues unions two multi-valued details: the values are deduped and
// sorted, so the result is the same whichever row the fold merged into.
//
// A union that does not fit model.MaxDetailBytes is cut between values, never
// inside one, and always at the same place: what is kept is the longest sorted
// prefix of the whole set that fits. Cutting at a byte offset would publish
// half a reason code that the next fold would then split out and carry as a
// reason of its own, and the surviving set would depend on the order the
// concurrent unit workers folded in -- and these details fold into the
// AnalysisKey, where two identical runs must key identically. Dropping whole
// values from the tail of the sorted set is order-independent, because adding
// a value can only move the cut earlier and anything already dropped would
// have been dropped again.
//
// A single value wider than the bound has no whole-value prefix to keep; it is
// cut to fit so the detail still says something. Every value reaching here
// came through WithDetail, which bounds it, so this is a floor rather than a
// path a caller can reach.
func joinDetailValues(a, b string) (value string, truncated bool) {
	parts := make([]string, 0, 8)
	for _, v := range []string{a, b} {
		for _, part := range strings.Split(v, detailSeparator) {
			if part != "" {
				parts = append(parts, part)
			}
		}
	}
	slices.Sort(parts)
	parts = slices.Compact(parts)
	var joined strings.Builder
	for _, part := range parts {
		width := len(part)
		if joined.Len() > 0 {
			width += len(detailSeparator)
		}
		if joined.Len()+width > model.MaxDetailBytes {
			truncated = true
			break
		}
		if joined.Len() > 0 {
			joined.WriteString(detailSeparator)
		}
		joined.WriteString(part)
	}
	if joined.Len() == 0 && len(parts) > 0 {
		bounded, _ := model.TruncateField(parts[0], model.MaxDetailBytes)
		return bounded, true
	}
	return joined.String(), truncated
}

// countDetail reads the scope count a folded row carries; a row folded for the
// first time carries none and counts as one.
func countDetail(s model.CapabilityState) int {
	if n, err := strconv.Atoi(s.Details[scopesDetail]); err == nil {
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

// coveredElsewhere records that this provider capability has a scope that did
// publish into this generation, which is what turns its failure row from
// failed into partial. It is a no-op for a capability with no failure row: a
// capability nothing failed for has nothing to soften.
func (r *capabilityReport) coveredElsewhere(providerID, capability string) {
	if row, ok := r.failures[providerID+"\x00"+capability]; ok {
		row.covered = true
	}
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
		state := model.CapabilityFailed
		if f.covered {
			state = model.CapabilityPartial
		}
		out = append(out, model.CapabilityState{ProviderID: f.providerID, Capability: f.capability,
			Scope: provider.ScopeWorkspace, State: state, DiagnosticCode: f.code,
			Details: map[string]string{unitsFailedDetail: strconv.Itoa(f.units), scopeKeyDetail: model.TruncateDetail(f.scope)}})
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
	rows, _ := boundStates(out, log)
	return rows
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
// The most severe row survives, ranked by outranks: Section 13.3 lets a report
// under-claim and never over-claim. The fold keeps the first occurrence of a
// key and never reorders its output, but which of two rows on one key supplies
// the surviving state and diagnostic code is decided by outranks alone, so the
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
			survivor, other := out[i], c
			if outranks(c, out[i]) {
				survivor, other = c, out[i]
			}
			merged := mergeDetails(survivor, other)
			// The counts do not add here: the two rows are two assertions
			// about the SAME scope, so the greater of them is the number of
			// scopes the merged row stands for and summing would count one
			// scope twice. After the collapse they stand for disjoint sets and
			// foldCollapsed adds them.
			if n := max(countDetail(out[i]), countDetail(c)); n > 1 {
				merged = merged.WithDetail(scopesDetail, strconv.Itoa(n))
			}
			out[i] = merged
			continue
		}
		at[key] = len(out)
		out = append(out, c)
	}
	return out
}

// foldCollapsed is foldToPrimaryKey for the far side of collapseScopes, where
// it must also SUM the counts of the rows it merges.
//
// The distinction matters and is not cosmetic. Before the collapse, two rows
// on one primary key are two assertions about the same scope -- a
// composition-time row meeting a stored one, say -- and summing their `scopes`
// would count one scope twice. After it, collapseScopes has keyed by provider
// capability AND state, written a `scopes` detail on every row it emits and
// rewritten each to the workspace scope; the rows that now collide therefore
// stand for DISJOINT scope sets, and keeping only the survivor's count reports
// a smaller set than the row represents while the report claims nothing was
// omitted. An under-count with no flag is exactly what the count exists to
// prevent, so here, and only here, the counts add.
func foldCollapsed(rows []model.CapabilityState) []model.CapabilityState {
	at := make(map[string]int, len(rows))
	out := make([]model.CapabilityState, 0, len(rows))
	for _, c := range rows {
		key := c.ProviderID + "\x00" + c.Capability + "\x00" + c.Scope
		i, ok := at[key]
		if !ok {
			at[key] = len(out)
			out = append(out, c)
			continue
		}
		scopes := countDetail(out[i]) + countDetail(c)
		survivor, other := out[i], c
		if outranks(c, out[i]) {
			survivor, other = c, out[i]
		}
		// mergeDetails already sums the numeric details the two rows share --
		// units_failed among them -- and carries over the ones only the less
		// severe row holds. Only `scopes` is written here, because its sum
		// must count a row that carries no count at all as the one scope it
		// stands for.
		out[i] = mergeDetails(survivor, other).WithDetail(scopesDetail, strconv.Itoa(scopes))
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
func boundStates(out []model.CapabilityState, log *slog.Logger) ([]model.CapabilityState, bool) {
	before := len(out)
	out = foldToPrimaryKey(out)
	if len(out) > model.MaxCapabilityStates {
		// Past the reporting threshold the list is aggregated per provider
		// capability -- a lossless view, because every fold carries the count
		// of the scopes it stands for -- and never cut. A row dropped off the
		// tail was unrecoverable; an aggregated one still names its capability,
		// its worst state and how many scopes it speaks for.
		out = foldCollapsed(collapseScopes(out))
		log.Info("the capability report was aggregated per provider capability", "component", component,
			"rows_in", before, "rows_out", len(out), "threshold", model.MaxCapabilityStates)
		return out, true
	}
	return out, false
}

// outranks reports whether a must supply the surviving state and diagnostic
// code when a and b fold onto one primary key. It is a strict total order on
// the rows that can collide there, which is what makes a fold's result a
// function of the set rather than of the order the rows arrived in.
//
// Severity decides it first. It ties in one shape that actually occurs: a
// provider with one failed scope and another that published is `partial`, and
// the planner's and the registry's own degradations are `partial` and
// workspace-scoped too, so the two meet on one key with equal rank. The row
// that NAMES A FAILED SCOPE carries the code there. It is the more specific
// assertion -- it points at a scope and says what happened to it, while a
// planner or registry degradation speaks about the provider as a whole -- and
// the details union either way, so the scope key and the failed-unit count
// survive whichever row wins.
//
// The last comparison is the diagnostic code itself, so two rows that are
// alike in both respects still fold the same way in either order.
func outranks(a, b model.CapabilityState) bool {
	if ra, rb := severityRank(a.State), severityRank(b.State); ra != rb {
		return ra < rb
	}
	if fa, fb := namesFailedScope(a), namesFailedScope(b); fa != fb {
		return fa
	}
	return a.DiagnosticCode < b.DiagnosticCode
}

// namesFailedScope reports whether a row is one of the failure folds: those
// are the only rows that carry a count of the units that failed behind them,
// beside the scope key of the exemplar they name.
func namesFailedScope(s model.CapabilityState) bool {
	return s.Details[unitsFailedDetail] != ""
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
			row = row.WithDetail(scopeKeyDetail, model.TruncateDetail(exemplar))
		}
		out = append(out, row.WithDetail(scopesDetail, strconv.Itoa(counts[key])))
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
