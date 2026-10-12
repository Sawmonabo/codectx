package index

import (
	"context"
	"errors"
	"log/slog"
	"maps"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Sawmonabo/codectx/internal/config"
	"github.com/Sawmonabo/codectx/internal/index/plan"
	"github.com/Sawmonabo/codectx/internal/index/watch"
	"github.com/Sawmonabo/codectx/internal/model"
	"github.com/Sawmonabo/codectx/internal/provider"
	"github.com/Sawmonabo/codectx/internal/storage/sqlite"
	"github.com/Sawmonabo/codectx/internal/vcs/git"
	"github.com/Sawmonabo/codectx/internal/workspace"
)

// statusLeaseTTL is how long the status read asks to pin the generation it
// projects. It is short because nothing is served from the pin: on a handle
// that can write it exists so the generation cannot be collected between the
// pointer read and the capability read (Section 12.3). A read-only handle
// records no retention lease at all, so there the pin is the read snapshot
// alone and a generation collected mid-report ends it typed rather than
// projecting bytes from two instants.
const statusLeaseTTL = 30 * time.Second

// maxStatusChanges bounds the worktree comparison. Status answers one bit --
// did anything move since the capture -- so it stops at the first change and
// never enumerates the worktree.
const maxStatusChanges = 1

// errWorktreeChanged stops the bounded worktree comparison at its first hit.
var errWorktreeChanged = errors.New("index: worktree changed")

// StatusOptions are what a status report reads. Store is the handle the report
// is answered from and Repo the identity it is scoped by; Git and Root are the
// worktree comparison.
//
// Repo is supplied rather than derived so the derivation keeps exactly one
// spelling in the process -- Coordinator.New's -- and a reader built beside a
// coordinator answers about the same repository by construction.
type StatusOptions struct {
	Root   workspace.Root
	Config config.Config
	Store  *sqlite.Store
	Git    *git.Git
	Repo   model.RepositoryID
	Logger *slog.Logger
}

// StatusReader answers the Sections 13.2/13.3 status report from ONE store
// handle. It is the one producer of that report: `codectx status` and the
// server's codectx_index_status both reach this body, so the two can never
// drift into different answers about one generation.
//
// It is separate from Coordinator because a status report is a read and needs
// none of what a coordinator owns -- no capture, no provider runtime, no delta
// appliers, no work directory. That is what lets the one
// process that indexes and answers at the same time serve this report from its
// read-only handle: that handle has no writer connection, so a status call
// cannot commit the ingestion group the same process's own refresh has open,
// and cannot queue behind it.
type StatusReader struct {
	opts StatusOptions
	log  *slog.Logger
	// watch and retention are the in-process state of the coordinator that
	// built this reader: a running watch's coverage and the last retention
	// sweep that did not finish. Both are nil for a reader built without one,
	// and neither performs a store call, so which handle the report is read
	// from does not change what they project.
	watch     *watchState
	retention *retentionState
}

// NewStatusReader validates the dependencies and builds the reader.
func NewStatusReader(o StatusOptions) (*StatusReader, error) {
	switch {
	case o.Store == nil:
		return nil, invalid("a status reader needs a store handle")
	case o.Repo == "":
		return nil, invalid("a status reader needs the repository identity")
	case o.Root.HasGit && o.Git == nil:
		return nil, invalid("the workspace is a Git repository but no git executable is available")
	}
	r := &StatusReader{opts: o, log: o.Logger}
	if r.log == nil {
		r.log = slog.Default()
	}
	return r, nil
}

// StatusReader is this coordinator's status path over the given store handle,
// carrying everything the coordinator knows: the repository identity it
// derived and its own watch and retention state.
//
// The handle is a parameter because the serving composition answers this
// report from its read-only handle while the coordinator itself writes through
// the other one. Passing the coordinator's own store returns the path
// `codectx status` takes.
func (c *Coordinator) StatusReader(store *sqlite.Store) (*StatusReader, error) {
	r, err := NewStatusReader(StatusOptions{Root: c.opts.Root, Config: c.opts.Config, Store: store,
		Git: c.opts.Git, Repo: c.repo, Logger: c.log})
	if err != nil {
		return nil, err
	}
	r.watch, r.retention = &c.watch, &c.retention
	return r, nil
}

// Status projects the active generation (Sections 13.2, 13.3). It runs no
// provider, captures nothing and never publishes: a status call on a busy
// workspace is a read, which is why it is legal without the workspace indexing
// lock.
//
// The stored completeness is brought back inside its bound here, as the
// indexing path does before publishing, so the answer validates.
func (r *StatusReader) Status(ctx context.Context) (model.IndexStatus, error) {
	gen, err := r.opts.Store.ActiveGeneration(ctx, r.opts.Repo)
	if err != nil {
		return model.IndexStatus{}, err
	}
	pinned, err := r.opts.Store.PinGeneration(ctx, r.opts.Repo, gen, statusLeaseTTL)
	if err != nil {
		return model.IndexStatus{}, err
	}
	defer pinned.Close()
	binding := pinned.Binding()
	states, err := pinned.Capabilities(ctx)
	if err != nil {
		return model.IndexStatus{}, err
	}
	snap, err := r.opts.Store.Snapshot(ctx, binding.SnapshotID)
	if err != nil {
		return model.IndexStatus{}, err
	}
	states, aggregated := boundStates(states, r.log)
	coherence, warnings, err := r.coherence(ctx, snap)
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
		FileCount: snap.FileCount, SourceBytes: snap.SourceBytes, Warnings: warnings,
		ProvidersDisabled: disabledProviders(r.opts.Config)}
	watchers, err := r.watchers(ctx)
	if err != nil {
		return model.IndexStatus{}, err
	}
	st.Watchers = watchers
	// The reasons come from the run rows of the generation just pinned, so a
	// `codectx status` in another terminal and the index-status tool read the
	// same failures the run that published them recorded. Nothing in this
	// process is consulted: an in-process set would answer only for the
	// process that did the indexing.
	failed, omitted, err := r.opts.Store.FailedRuns(ctx, binding.GenerationID)
	if err != nil {
		return model.IndexStatus{}, err
	}
	// The run row keeps the analyzer's standard-error tail; this answer is
	// handed to any client of `status --json` and the index-status tool, so it
	// never carries it.
	for i := range failed {
		failed[i].Details = withoutRawOutput(failed[i].Details)
	}
	st.FailedUnits, st.FailedUnitsOmitted = failed, omitted
	if r.watch != nil {
		r.watch.project(&st)
	}
	if r.retention != nil {
		r.retention.project(&st)
	}
	return st, nil
}

// watchers lists the watching processes whose heartbeat is live, freshest
// deadline first (Section 13.2).
//
// It is read from the store and not from watchState, so it answers in a process
// that is watching nothing: a `codectx status` in another terminal is exactly
// the reader this list exists for, and it has no in-process watch to project.
// A watcher that has completed no pass is listed with no pass time rather than
// omitted -- a watch waiting for whichever process holds the workspace is
// running, and dropping it would report the workspace as one no watch has ever
// run in.
//
// A row past its writer's own deadline is left out: its process stopped
// refreshing, and listing it would report a watch that is no longer running.
// That the watch stopped is what the doctor's `watch_heartbeat` check reports;
// this list states who is watching now.
func (r *StatusReader) watchers(ctx context.Context) ([]model.WatchProcess, error) {
	rows, err := r.opts.Store.WatchHeartbeats(ctx, r.opts.Repo)
	if err != nil {
		return nil, err
	}
	now := time.Now()
	var out []model.WatchProcess
	for _, hb := range rows {
		if !hb.Live(now) {
			continue
		}
		out = append(out, model.WatchProcess{SessionID: hb.SessionID, PID: hb.WriterPID,
			LastBeatAt: hb.BeatAt, LastPassAt: hb.LastPassAt, PendingEvents: hb.PendingEvents})
	}
	return out, nil
}

// coherence answers what the active generation is coherent with (Section
// 13.3). A moved HEAD or any worktree change makes it `worktree_changed`; the
// snapshot itself is always coherent with its own bytes.
//
// A non-Git workspace is reported coherent with an explicit warning rather
// than compared: the comparison would be a full walk of the repository, and
// Section 13.3 forbids inferring freshness. The warning says the check was not
// performed, which is the honest answer.
func (r *StatusReader) coherence(ctx context.Context, snap model.Snapshot) (model.Coherence, []string, error) {
	if !r.opts.Root.HasGit || r.opts.Git == nil {
		return model.CoherenceSnapshot, []string{"worktree coherence is not checked for a workspace that is not a Git repository"}, nil
	}
	head, err := r.opts.Git.Head(ctx, r.opts.Root.Path)
	if err != nil {
		return "", nil, err
	}
	if head != snap.HeadObjectID {
		return model.CoherenceWorktreeChange, nil, nil
	}
	err = r.opts.Git.Status(ctx, r.opts.Root.Path, r.opts.Config.Workspace.IncludeUntracked, maxStatusChanges,
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
	mu     sync.Mutex
	active int
	source *watch.Watcher
	// skipping is true while this watch is inside a held-lock episode: a run
	// of beats that could not take the workspace because another process has
	// it. It makes the episode reportable once instead of once per beat, and a
	// beat that takes the workspace clears it, so the next episode is reported
	// again.
	skipping   bool
	reconciled time.Time
	// watchSession is the identity of the watch this coordinator is running,
	// minted by Watch and carried here because the beat is published from both
	// the heartbeat ticker and the end of a reconciliation pass. It keys this
	// watch's own heartbeat row, so a second watching process on this
	// workspace keeps its own row rather than overwriting this one's.
	watchSession string
}

// enter records one running watch, its session identity and the notification
// source driving it, if any. Concurrent watches over one coordinator are not a
// supported composition, but the counter makes a second one visible rather than
// letting the first one's exit report "watch off" while it still runs.
func (w *watchState) enter(source *watch.Watcher, session string) {
	w.mu.Lock()
	w.active++
	w.skipping = false
	w.watchSession = session
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
		w.watchSession = ""
	}
	w.mu.Unlock()
}

// session is the identity of the watch running over this coordinator, which is
// what its heartbeat row is keyed by.
func (w *watchState) session() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.watchSession
}

// beginSkipping opens a held-lock episode and reports whether this beat is the
// one that opened it; every later beat of the same episode reports false.
func (w *watchState) beginSkipping() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.skipping {
		return false
	}
	w.skipping = true
	return true
}

// tookWorkspace ends whatever episode was open.
func (w *watchState) tookWorkspace() {
	w.mu.Lock()
	w.skipping = false
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

// heartbeat is what this watch publishes for another process to read: which
// watch it is, when its last pass completed and how many events are pending,
// the latter two absent when nothing measured them.
//
// A watch that has completed no pass publishes neither figure. Until it owns
// the workspace it has reconciled nothing, and the events its notification
// queue has already collected are a backlog it has not touched, not coverage of
// them: published, `0 pending` would tell another process's `status` that this
// workspace is caught up while an index it is waiting behind is still running.
// The row itself is still published, which is what keeps a watch that is
// waiting distinguishable from a workspace where no watch has ever run.
func (w *watchState) heartbeat() (session string, lastPass *time.Time, pending *int64) {
	w.mu.Lock()
	defer w.mu.Unlock()
	session = w.watchSession
	// w.reconciled and not observe's merged time: the watcher marks itself
	// reconciled on its own rescans and polling ticks, which happen whether or
	// not this watch ever had the workspace. Only w.reconciled is a pass this
	// coordinator completed while holding it.
	if w.reconciled.IsZero() {
		return session, nil, nil
	}
	_, _, pending, at := w.observe()
	return session, &at, pending
}

// capabilityReport accumulates one generation's capability rows and publishes
// a bounded, deterministic list. Rows are bounded by model.MaxCapabilityStates
// whatever the repository holds: a per-unit failure is aggregated
// rather than published one row per unit, because the list must fit the same
// bound on a repository with one unit and one with ten thousand.
type capabilityReport struct {
	order    []string
	rows     map[string]model.CapabilityState
	failures map[string]*failureRow
}

// failureRow aggregates every failed unit of one provider capability: how many
// failed, how many were planned, one exemplar scope with the reason it carried,
// and a bounded sample of the scope keys that failed behind the row.
type failureRow struct {
	providerID, capability, scope, code string
	// message and details are the exemplar scope's typed reason. The code
	// alone cannot tell an analyzer that could not be started from one that
	// exited nonzero, and the particulars a provider attached -- which
	// profile, which tool, which project, what exit status -- are what an
	// operator acts on. The standard-error tail is excluded: it is raw
	// analyzer output and lives on the run row alone.
	message string
	// remediation is that same exemplar scope's remediation, published as a
	// field of the row rather than as a detail: it belongs to the exemplar's
	// code and message, and the detail map is the provider's own budget.
	remediation string
	details     map[string]string
	// scopes is the bounded, sorted sample of the failed scope keys, and
	// planned is how many units the plan gave this provider. "Two failed" is
	// a different report depending on whether two or two hundred were tried.
	scopes  []string
	planned int
	units   int
	// covered records that another scope of this provider capability DID
	// publish into this generation. A capability with facts in it is partial,
	// never failed: Section 13.3 lets a report under-claim, and reporting a
	// capability that answers queries as failed is the opposite.
	covered bool
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
	scopeKeyDetail = model.DetailScopeKey
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
	// unitsPlannedDetail counts the units the plan gave the provider behind
	// one row.
	unitsPlannedDetail = model.DetailUnitsPlanned
	// failedScopesDetail names the scopes that failed behind one row. It is
	// deliberately absent from scopeNamingDetails: it is the SET of failed
	// scopes rather than the exemplar the row's diagnostic code belongs to,
	// so two rows folding onto one primary key must union their sets. Keeping
	// the receiving row's value, as a scope-naming detail does, would publish
	// one row's failed scopes as though they were all of them.
	failedScopesDetail = model.DetailFailedScopes
	// failureMessageDetail carries the exemplar scope's safe message.
	failureMessageDetail = model.DetailFailureMessage
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
// ceiling -- and clearing the whole map would make state the only channel that
// survives the fold, so a provider with nothing worse than an admitted-oversize
// count would have to publish `partial` to be heard at all. The
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
// the rows come from unit workers that run concurrently. Two integers sum,
// equal ones included -- two units that each saw ten call sites saw twenty --
// because a numeric detail is a count of units, records, sites or bytes and
// the merged row stands for both sets. The exceptions are the figures that
// are not counts (extremeDetail): a bound every unit reports alike and the
// largest size one unit saw keep the larger value, never the sum. Any other
// pair is deduped and joined in sorted order, so two reasons are both
// reported and a reason repeated by ten units is reported once. A
// scope-naming detail keeps the receiving row's value: that row's diagnostic
// code is the one being published, so its exemplar scope must be too. The
// scope count is not merged here at all: every caller writes it from
// countDetail afterwards, by the rule its own fold needs.
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
		if k != scopesDetail {
			keys = append(keys, k)
		}
	}
	slices.Sort(keys)
	var cut []string
	for _, k := range keys {
		have, ok := row.Details[k]
		if !ok {
			row = row.WithDetail(k, other.Details[k])
			continue
		}
		value, truncated := mergeDetailValue(k, have, other.Details[k], extremeDetail(k, row, other))
		if truncated {
			cut = append(cut, k)
		}
		row = row.WithDetail(k, value)
	}
	if len(cut) > 0 {
		flagged, _ := mergeDetailValue(truncatedDetail, row.Details[truncatedDetail], strings.Join(cut, detailSeparator), false)
		row = row.WithDetail(truncatedDetail, flagged)
	}
	return row
}

// mergeDetailValue combines the two values one detail key carries and reports
// whether the result had to be cut to fit model.MaxDetailBytes. Two integers
// sum, or, for an extreme figure, keep the larger; equal values of any other
// kind stay as they are.
func mergeDetailValue(key, a, b string, extreme bool) (value string, truncated bool) {
	if scopeNamingDetails[key] {
		return a, false
	}
	if x, err := strconv.ParseInt(a, 10, 64); err == nil {
		if y, err := strconv.ParseInt(b, 10, 64); err == nil {
			if extreme {
				return strconv.FormatInt(max(x, y), 10), false
			}
			return strconv.FormatInt(x+y, 10), false
		}
	}
	if a == b {
		return a, false
	}
	return joinDetailValues(a, b)
}

// extremeDetail reports whether key is a figure the fold keeps the larger of
// instead of summing: it is not a count, so a sum is a number nothing
// measured. These are the largest record a unit saw, the planner's scope-key
// bound, and a user-set limit a unit reports beside the count of what crossed
// it -- a provider.Degradation publishes the limit under its configuration
// key and the count under "over_" plus that key, which is what names a key a
// limit wherever it comes from. Every unit of one run reports the same limit,
// so the larger of two is that limit.
func extremeDetail(key string, a, b model.CapabilityState) bool {
	if key == provider.DetailLargestRecordBytes || key == plan.DetailScopeKeyBound {
		return true
	}
	over := "over_" + key
	return a.Details[over] != "" || b.Details[over] != ""
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

// addUnavailable publishes the degradation of a provider that
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

// addFailures records one provider capability's failed units. The aggregate
// arrives whole rather than one unit at a time, because the row publishes
// figures -- how many failed of how many planned, which scopes -- that only
// the coverage pass, which sees every unit of one provider at once, can count.
//
// The exemplar is the aggregate's lexicographically first scope, never the
// first to arrive: the units of one provider are built concurrently, and both
// details_json and diagnostic_code fold into the AnalysisKey, so an
// arrival-ordered exemplar would key two identical runs differently. The code,
// message and details travel with the scope they belong to -- a published row
// naming one scope's key and another scope's reason is arrival-ordered again,
// in a shape that also misreports.
func (r *capabilityReport) addFailures(providerID, capability string, f *providerFailures) {
	if f == nil || len(f.named) == 0 {
		return
	}
	exemplar := f.named[0]
	scopes := make([]string, 0, len(f.named))
	for _, s := range f.named {
		scopes = append(scopes, s.scopeKey)
	}
	r.failures[providerID+"\x00"+capability] = &failureRow{providerID: providerID, capability: capability,
		scope: exemplar.scopeKey, code: exemplar.failure.code, message: exemplar.failure.message,
		remediation: exemplar.failure.remediation, details: exemplar.failure.details,
		scopes: scopes, planned: f.planned, units: f.units}
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

// reported answers whether anything was already published for this provider
// capability, in any state and at any scope.
func (r *capabilityReport) reported(providerID, capability string) bool {
	prefix := providerID + "\x00" + capability + "\x00"
	for _, key := range r.order {
		if len(key) > len(prefix) && key[:len(prefix)] == prefix {
			return true
		}
	}
	_, ok := r.failures[providerID+"\x00"+capability]
	return ok
}

// publish builds the one capability row that stands for every failed unit of
// one provider capability.
//
// A capability with facts in this generation is `partial`; one with none is
// `failed` and never `unavailable`. That choice is load-bearing: healthOf
// makes partial, stale and failed degrade the generation and deliberately
// leaves unavailable fresh, because `unavailable` is the state of a capability
// nobody attempted -- no units planned. A provider
// whose every planned unit failed WAS attempted and is broken, so reporting it
// unavailable would publish a fresh generation over a provider that answers
// nothing.
//
// The details are written in sorted key order. A detail map past the provider
// budget drops its overflow, and which entry overflows must not depend on map
// iteration order: these details fold into the AnalysisKey, where two
// identical runs must key identically.
func (f *failureRow) publish() model.CapabilityState {
	state := model.CapabilityFailed
	if f.covered {
		state = model.CapabilityPartial
	}
	row := model.CapabilityState{ProviderID: f.providerID, Capability: f.capability,
		Scope: provider.ScopeWorkspace, State: state, DiagnosticCode: f.code,
		Remediation: f.remediation,
		Details: map[string]string{unitsFailedDetail: strconv.Itoa(f.units),
			scopeKeyDetail: model.TruncateDetail(f.scope)}}
	if f.planned > 0 {
		row = row.WithDetail(unitsPlannedDetail, strconv.Itoa(f.planned))
	}
	if len(f.scopes) > 0 {
		row = row.WithDetail(failedScopesDetail, strings.Join(f.scopes, detailSeparator))
		if f.units > len(f.scopes) {
			// The list is a sample, not the set: units_failed carries the
			// count and this flag says the names are not all of them, which
			// is the same disclosure a merge that had to cut a value makes.
			row = row.WithDetail(truncatedDetail, failedScopesDetail)
		}
	}
	if f.message != "" {
		row = row.WithDetail(failureMessageDetail, f.message)
	}
	details := withoutRawOutput(f.details)
	for _, k := range slices.Sorted(maps.Keys(details)) {
		row = row.WithDetail(k, details[k])
	}
	return row
}

// withoutRawOutput is a provider's failure details less the standard-error
// tail. The tail is raw analyzer output: it is kept on the run row and leaves
// through no capability row, status answer or log line. The input is never
// modified; a map that holds no tail is answered as it is.
func withoutRawOutput(details map[string]string) map[string]string {
	if _, ok := details[model.DetailStderrTail]; !ok {
		return details
	}
	out := make(map[string]string, len(details)-1)
	for k, v := range details {
		if k != model.DetailStderrTail {
			out[k] = v
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// finish publishes the bounded list.
func (r *capabilityReport) finish(log *slog.Logger) []model.CapabilityState {
	out := make([]model.CapabilityState, 0, len(r.order)+len(r.failures))
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
		out = append(out, f.publish())
	}
	rows, _ := boundStates(out, log)
	return rows
}

// foldToPrimaryKey brings the assembled list inside the one row per
// (provider_id, capability, scope_key) that generation_capabilities is keyed
// by -- its primary key does not carry the state. The three assembly buckets
// above key their own rows by state as well, and the bound's own collapse
// keys by state too, so one provider capability reaches here more than once at
// one scope in three shapes, each of which failed the insert and took `codectx
// index` down on a constraint error instead of publishing the degraded
// generation it had built:
//
//   - a sibling unit that succeeded folds a `fresh` row to the workspace scope
//     (add) while the unit that failed publishes the `failed` fold here;
//   - the planner's and the registry's `partial` degradations are workspace
//     scoped too (plan.builder.partial, provider detection), so they collide
//     with that same `failed` fold;
//   - above the bound, collapseScopes folds by provider capability AND state
//     and rewrites every fold's scope to the workspace scope, so two states of
//     one provider capability that fold separately come back out as two
//     workspace-scoped rows. That shape is
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
// on one primary key are two assertions about the same scope -- a planner
// degradation meeting a failed unit's row, say -- and summing their `scopes`
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
// finish and Status over the stored list, so a reported list can never exceed
// the bound its own contract validates against.
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
// failures, which are appended last, and keep the fresh rows:
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

func compareString(a, b string) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}
