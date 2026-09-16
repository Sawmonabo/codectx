package model

import "time"

// Index, status and doctor contracts. The spec does not enumerate the fields of
// these records; each doc comment below states the minimal set the named
// sections imply and why nothing further is included.

// IndexRequest asks the coordinator to build or refresh a generation. The flags
// mirror Section 18.1's `index` options. Rebuild is explicit: Section 12.2
// forbids silently mutating or erasing an incompatible database.
type IndexRequest struct {
	Full    bool `json:"full"`
	Rebuild bool `json:"rebuild"`
	Watch   bool `json:"watch"`
}

// Validate rejects the one incoherent combination: a rebuild creates a new
// cache, so asking for it and a full reindex of the old one at once is
// ambiguous.
func (r IndexRequest) Validate() error {
	if r.Full && r.Rebuild {
		return invalid("index cannot be both --full and --rebuild; a rebuild already creates a new cache")
	}
	return nil
}

// IndexResult reports one completed indexing run. Section 13.1 requires a no-op
// refresh to emit reuse counts and perform no parse or FTS rewrite, so reuse and
// parse counts are first-class rather than log-only; Section 12.3 makes the
// published binding and health the outcome of activation.
type IndexResult struct {
	Binding      Binding           `json:"binding"`
	Health       GenerationHealth  `json:"health"`
	Status       GenerationStatus  `json:"status"`
	Completeness []CapabilityState `json:"completeness"`
	UnitsReused  int64             `json:"units_reused"`
	UnitsBuilt   int64             `json:"units_built"`
	// UnitsCarried counts sealed units of a refreshing semantic scope carried
	// into this generation as stale with provenance distance (Section 13.3);
	// UnitsInvalidated counts previously reusable units this run had to rebuild.
	UnitsCarried     int64            `json:"units_carried"`
	UnitsInvalidated int64            `json:"units_invalidated"`
	FilesParsed      int64            `json:"files_parsed"`
	FilesCaptured    int64            `json:"files_captured"`
	Runs             []ProviderResult `json:"runs"`
	// RunsOmitted is how many runs this generation produced beyond the
	// per-result ceiling Runs carries. Runs is a wire-sized page, not the
	// total: a generation with more runs than one response may carry says so
	// here instead of truncating in silence.
	RunsOmitted int64     `json:"runs_omitted"`
	StartedAt   time.Time `json:"started_at"`
	CompletedAt time.Time `json:"completed_at"`
	// Run is this indexing run as the run ledger recorded it, and Stages is
	// one page of its span tree in the order the stages were opened. Both are
	// absent where the run recorded nothing. They carry the same rows status
	// reports, so a surface renders the run's accounting and the result's own
	// counts above from one set of numbers rather than two.
	Run    *RunRecord    `json:"run,omitempty"`
	Stages []StageRecord `json:"stages,omitempty"`
	// StagesOmitted is how many of the run's stages this generation recorded
	// beyond the per-result ceiling Stages carries, exactly as RunsOmitted
	// reports it for runs. Stages is a wire-sized page, not the whole of the
	// run's accounting, and a short list that did not say how much it dropped
	// would read as the complete cost of the run -- the one thing a bounded
	// response must never do.
	StagesOmitted int64 `json:"stages_omitted"`
}

// Validate enforces the result shape.
func (r IndexResult) Validate() error {
	if err := r.Binding.Validate(); err != nil {
		return err
	}
	if !r.Health.Valid() {
		return invalid("index_result.health %q is not a known generation health", truncateForMessage(string(r.Health)))
	}
	if !r.Status.Valid() {
		return invalid("index_result.status %q is not a known generation status", truncateForMessage(string(r.Status)))
	}
	if err := validateCapabilityStates("index_result.completeness", r.Completeness); err != nil {
		return err
	}
	for _, count := range []struct {
		field string
		value int64
	}{
		{"index_result.units_reused", r.UnitsReused},
		{"index_result.units_built", r.UnitsBuilt},
		{"index_result.files_parsed", r.FilesParsed},
		{"index_result.files_captured", r.FilesCaptured},
		{"index_result.runs_omitted", r.RunsOmitted},
	} {
		if err := requireNonNegative(count.field, count.value); err != nil {
			return err
		}
	}
	if err := boundPage("index_result.runs", len(r.Runs)); err != nil {
		return err
	}
	for _, run := range r.Runs {
		if err := run.Validate(); err != nil {
			return err
		}
	}
	if err := validateRunLedger("index_result", r.Run, r.Stages, r.StagesOmitted); err != nil {
		return err
	}
	return nil
}

// Coherence distinguishes what an active generation is coherent with (Section
// 13.3). A valid historical query is not an assertion of current worktree
// freshness, so this is reported rather than inferred.
type Coherence string

const (
	CoherenceSnapshot       Coherence = "snapshot_coherent"
	CoherenceWorktreeChange Coherence = "worktree_changed"
	CoherenceRefreshPending Coherence = "refresh_pending"
	CoherenceSuperseded     Coherence = "superseded"
)

// Valid reports whether c is a known wire spelling.
func (c Coherence) Valid() bool {
	switch c {
	case CoherenceSnapshot, CoherenceWorktreeChange, CoherenceRefreshPending, CoherenceSuperseded:
		return true
	}
	return false
}

// IndexStatus is what `status` and `codectx_index_status` report. The fields are
// the minimal set Sections 13.2, 13.3 and 19.2 name explicitly: the active
// snapshot/generation binding, per-capability coverage, generation health,
// coherence with the worktree, and the watch coverage and last reconciliation
// that Section 13.2 requires be reported, plus the optional Section 23 resource
// block, which is nil unless the caller asked for it.
type IndexStatus struct {
	Binding            Binding            `json:"binding"`
	Health             GenerationHealth   `json:"health"`
	Coherence          Coherence          `json:"coherence"`
	CaptureConsistency CaptureConsistency `json:"capture_consistency"`
	Completeness       []CapabilityState  `json:"completeness"`
	FileCount          uint64             `json:"file_count"`
	SourceBytes        uint64             `json:"source_bytes"`
	WatchActive        bool               `json:"watch_active"`
	WatchComplete      bool               `json:"watch_complete"`
	PendingPaths       int64              `json:"pending_paths"`
	LastReconciledAt   *time.Time         `json:"last_reconciled_at,omitempty"`
	ActivatedAt        *time.Time         `json:"activated_at,omitempty"`
	Warnings           []string           `json:"warnings,omitempty"`
	// Resources is the Section 23 accounting block. It is nil on an ordinary
	// status so a cheap call stays cheap; Task 20 populates it.
	Resources *ResourceReport `json:"resources,omitempty"`
}

// ResourceReport is the resource accounting Section 23 requires status to
// expose. Every field is a pointer because Section 23 is explicit that an
// unavailable metric is recorded as unavailable, never as zero: a nil field
// means "not measured on this platform or not sampled", while a zero value is a
// real measurement of zero. Collapsing the two would let a missing RSS reading
// read as a process using no memory. Task 20 owns measurement and populates it.
type ResourceReport struct {
	ParentRSSBytes        *uint64 `json:"parent_rss_bytes,omitempty"`
	PeakParentRSSBytes    *uint64 `json:"peak_parent_rss_bytes,omitempty"`
	BaseWorkerRSSBytes    *uint64 `json:"base_worker_rss_bytes,omitempty"`
	GoManagedBytes        *uint64 `json:"go_managed_bytes,omitempty"`
	NativeWorkerBytes     *uint64 `json:"native_worker_bytes,omitempty"`
	QueryReservationBytes *uint64 `json:"query_reservation_bytes,omitempty"`
	CacheReservationBytes *uint64 `json:"cache_reservation_bytes,omitempty"`
	QueueReservationBytes *uint64 `json:"queue_reservation_bytes,omitempty"`
	LiveSubprocesses      *int64  `json:"live_subprocesses,omitempty"`
	PendingEvents         *int64  `json:"pending_events,omitempty"`
	DatabaseBytes         *uint64 `json:"database_bytes,omitempty"`
	WALBytes              *uint64 `json:"wal_bytes,omitempty"`
	TempBytes             *uint64 `json:"temp_bytes,omitempty"`
	CASBytes              *uint64 `json:"cas_bytes,omitempty"`
	// FreedBytes is the disk space this process has given back to the
	// filesystem since it started, one window at a time. A run reuses the
	// space it holds and frees only the leftovers of a dead run at its start,
	// because on a host that discards freed blocks into a sparse image a
	// multi-gigabyte free stalls every process on the machine, minutes later,
	// with nothing able to observe or wait for it. This is what makes that
	// claim checkable from outside. It is byte-exact and counted as each
	// truncation and each unlink releases the space: a file smaller than the
	// window frees its length without a windowed step and is counted here
	// like any other.
	FreedBytes *uint64 `json:"freed_bytes,omitempty"`
	// ScratchBytes is the disk the store's scratch pools hold: every working
	// file this process writes for its own later reading -- sort runs,
	// staging databases, blob staging surfaces, a search's state -- at its
	// current length, taken or free.
	//
	// It is the other half of FreedBytes and the reason that figure is small.
	// The product does not remove these files when it finishes with them; it
	// hands them back to the pool and writes over them next time, so what
	// would have been freed and re-created over and over is disclosed here
	// instead as space the store is holding. It shrinks only when an operator
	// asks for the pools to be emptied, which is what `codectx gc` does.
	ScratchBytes *uint64 `json:"scratch_bytes,omitempty"`
	// FreedByPurpose says what the freeing this process did was for, in bytes
	// released, keyed by internal/paced.Purpose. A well-behaved run frees the
	// outputs of foreign writers it cannot write over, the source trees it
	// built for them, and the leftovers of runs whose caller never came back
	// -- and nothing else. It is the labelled part of FreedBytes, counted by
	// the same additions, so it never exceeds it; the difference is the
	// removals no call site names, such as the engine shortening a file it
	// owns.
	FreedByPurpose map[string]uint64 `json:"freed_by_purpose,omitempty"`
	// PendingFreeBytes is disk this process has finished with and not yet
	// given back: every file and tree a removal renamed aside, waiting for the
	// reclaimer to release it a window at a time.
	//
	// It is not part of ScratchBytes, which is space the pools are holding to
	// write over again, and it is not yet part of FreedByPurpose, which counts
	// space as it is actually released. A run's removals return to their
	// callers at once and land here; a run that exits with this above zero
	// leaves the rest for the next run to release at the same pace.
	PendingFreeBytes *uint64 `json:"pending_free_bytes,omitempty"`
	// StuckFrees names the removals the reclaimer has tried and could not
	// make, each with the reason. It is the neighbour PendingFreeBytes needs:
	// that figure rising and never falling is either a run removing faster
	// than the pace gives back, which resolves itself, or a removal nothing
	// can make, which does not, and only this tells the two apart.
	StuckFrees []StuckFree `json:"stuck_frees,omitempty"`
	// AnalyzerUnits is what each heavy analysis unit this process ran was
	// given and what it used: the reservation it was admitted against, the
	// heap caps its two steps ran under, the machine-derived allocation those
	// caps were bounded by, and the peak resident memory its process tree
	// reached. It is the only place the three appear together, and the only
	// way an operator can see that a unit was handed far more than it needed
	// -- or serialized behind a reservation it never came close to using.
	//
	// It is process accounting and not a capability detail on purpose: an
	// observed peak differs on every run, and a capability row's details fold
	// into the analysis key, where two identical runs must key identically.
	AnalyzerUnits []AnalyzerUnit `json:"analyzer_units,omitempty"`
	UnitsReused   *int64         `json:"units_reused,omitempty"`
	UnitsParsed   *int64         `json:"units_parsed,omitempty"`
	// Run is the latest recorded run for this repository -- the live one if a
	// run is going, otherwise the one that produced the active generation --
	// and Stages is one page of its stages. Run is carried beside Stages
	// because a stage's share of the run is unreadable without the run it is
	// a share of.
	Run    *RunRecord    `json:"run,omitempty"`
	Stages []StageRecord `json:"stages,omitempty"`
	// StagesOmitted is how many of the run's stages this page does not carry,
	// with the same meaning it has on IndexResult.
	StagesOmitted int64 `json:"stages_omitted"`
}

// An AnalyzerUnit is one heavy unit's memory accounting. AllocationBytes and
// ObservedPeakBytes are absent rather than zero where the host does not expose
// available memory and where the platform cannot sample a process tree: a zero
// there would claim a measurement nobody made (Section 23).
type AnalyzerUnit struct {
	ScopeKey           string  `json:"scope_key"`
	Family             string  `json:"family"`
	ReservationBytes   uint64  `json:"reservation_bytes"`
	HeapCapBytes       uint64  `json:"heap_cap_bytes"`
	ExportHeapCapBytes uint64  `json:"export_heap_cap_bytes"`
	AllocationBytes    *uint64 `json:"allocation_bytes,omitempty"`
	ObservedPeakBytes  *uint64 `json:"observed_peak_bytes,omitempty"`
}

// Validate enforces the signed-64 storage bound on every measured byte count
// and rejects a negative count.
func (r ResourceReport) Validate() error {
	for _, f := range []struct {
		field string
		value *uint64
	}{
		{"resources.parent_rss_bytes", r.ParentRSSBytes},
		{"resources.peak_parent_rss_bytes", r.PeakParentRSSBytes},
		{"resources.base_worker_rss_bytes", r.BaseWorkerRSSBytes},
		{"resources.go_managed_bytes", r.GoManagedBytes},
		{"resources.native_worker_bytes", r.NativeWorkerBytes},
		{"resources.query_reservation_bytes", r.QueryReservationBytes},
		{"resources.cache_reservation_bytes", r.CacheReservationBytes},
		{"resources.queue_reservation_bytes", r.QueueReservationBytes},
		{"resources.database_bytes", r.DatabaseBytes},
		{"resources.wal_bytes", r.WALBytes},
		{"resources.temp_bytes", r.TempBytes},
		{"resources.cas_bytes", r.CASBytes},
		{"resources.freed_bytes", r.FreedBytes},
		{"resources.scratch_bytes", r.ScratchBytes},
		{"resources.pending_free_bytes", r.PendingFreeBytes},
	} {
		if f.value == nil {
			continue
		}
		if err := boundSigned64(f.field, *f.value); err != nil {
			return err
		}
	}
	for _, f := range []struct {
		field string
		value *int64
	}{
		{"resources.live_subprocesses", r.LiveSubprocesses},
		{"resources.pending_events", r.PendingEvents},
		{"resources.units_reused", r.UnitsReused},
		{"resources.units_parsed", r.UnitsParsed},
	} {
		if f.value == nil {
			continue
		}
		if err := requireNonNegative(f.field, *f.value); err != nil {
			return err
		}
	}
	if len(r.AnalyzerUnits) > MaxRecordsPerResult {
		return invalid("resources.analyzer_units holds %d rows, more than the %d a bounded response carries",
			len(r.AnalyzerUnits), MaxRecordsPerResult)
	}
	for _, u := range r.AnalyzerUnits {
		for _, f := range []struct {
			field string
			value uint64
		}{
			{"resources.analyzer_units.reservation_bytes", u.ReservationBytes},
			{"resources.analyzer_units.heap_cap_bytes", u.HeapCapBytes},
			{"resources.analyzer_units.export_heap_cap_bytes", u.ExportHeapCapBytes},
		} {
			if err := boundSigned64(f.field, f.value); err != nil {
				return err
			}
		}
		if u.AllocationBytes != nil {
			if err := boundSigned64("resources.analyzer_units.allocation_bytes", *u.AllocationBytes); err != nil {
				return err
			}
		}
		if u.ObservedPeakBytes != nil {
			if err := boundSigned64("resources.analyzer_units.observed_peak_bytes", *u.ObservedPeakBytes); err != nil {
				return err
			}
		}
	}
	if err := validateRunLedger("resources", r.Run, r.Stages, r.StagesOmitted); err != nil {
		return err
	}
	return nil
}

// Validate enforces the status shape.
func (s IndexStatus) Validate() error {
	if err := s.Binding.Validate(); err != nil {
		return err
	}
	if !s.Health.Valid() {
		return invalid("index_status.health %q is not a known generation health", truncateForMessage(string(s.Health)))
	}
	if !s.Coherence.Valid() {
		return invalid("index_status.coherence %q is not a known coherence state", truncateForMessage(string(s.Coherence)))
	}
	if !s.CaptureConsistency.Valid() {
		return invalid("index_status.capture_consistency %q is not a known capture consistency",
			truncateForMessage(string(s.CaptureConsistency)))
	}
	if err := validateCapabilityStates("index_status.completeness", s.Completeness); err != nil {
		return err
	}
	if err := boundSigned64("index_status.file_count", s.FileCount); err != nil {
		return err
	}
	if err := boundSigned64("index_status.source_bytes", s.SourceBytes); err != nil {
		return err
	}
	if err := requireNonNegative("index_status.pending_paths", s.PendingPaths); err != nil {
		return err
	}
	if err := boundStrings("index_status.warnings", s.Warnings, MaxReasonsPerEntry, MaxReasonBytes); err != nil {
		return err
	}
	if s.Resources != nil {
		if err := s.Resources.Validate(); err != nil {
			return err
		}
	}
	return nil
}

// StatusRequest selects what `status` and `codectx_index_status` report.
// Section 23 names status as the surface for resource accounting, and the
// report is optional because an ordinary status must stay cheap: Resources is
// false by default, and IndexStatus.Resources is nil unless it is set.
//
// Ruling Q1 makes this the request of
// IndexService.IndexStatus(ctx, model.StatusRequest); `codectx status
// --resources` and the MCP tool's input are its readers. INT re-points the
// facade, the CLI and the MCP schema onto it.
type StatusRequest struct {
	Resources bool `json:"resources"`
}

// Validate accepts any value; the single flag is independent.
func (r StatusRequest) Validate() error { return nil }

// DoctorRequest selects the diagnostic depth of Section 18.1. Deep is explicit
// because Section 22 reserves expensive integrity and parser smoke checks for
// it; no full database scan happens on an ordinary command.
type DoctorRequest struct {
	Offline bool `json:"offline"`
	Deep    bool `json:"deep"`
}

// Validate accepts any combination; both flags are independent.
func (r DoctorRequest) Validate() error { return nil }

// CheckState is the outcome of one doctor check. It reuses the capability
// vocabulary deliberately rather than introducing a competing one.
type CheckState string

const (
	CheckPass CheckState = "pass"
	CheckWarn CheckState = "warn"
	CheckFail CheckState = "fail"
	// CheckUnavailable records a metric or check that could not be measured.
	// Section 22 requires an unavailable metric be reported as unavailable,
	// never as zero.
	CheckUnavailable CheckState = "unavailable"
	// CheckUnverified records a check this run deliberately did not perform
	// because performing it walks the whole database. It is not `unavailable`:
	// the check is available on this host and on this build, it was skipped by
	// the mode the operator chose, and its detail names the flag that runs it.
	// Collapsing the two would tell an operator a verifiable fact is
	// unmeasurable. Like `unavailable` it does not degrade the report state --
	// a skipped check is not a defect.
	CheckUnverified CheckState = "unverified"
)

// Valid reports whether s is a known wire spelling.
func (s CheckState) Valid() bool {
	switch s {
	case CheckPass, CheckWarn, CheckFail, CheckUnavailable, CheckUnverified:
		return true
	}
	return false
}

// DoctorCheck is one diagnostic result. The machine reason code stays separate
// from user-readable remediation (Section 13.3).
type DoctorCheck struct {
	Name        string     `json:"name"`
	State       CheckState `json:"state"`
	Detail      string     `json:"detail,omitempty"`
	Code        string     `json:"code,omitempty"`
	Remediation string     `json:"remediation,omitempty"`
}

// Validate enforces the check shape.
func (c DoctorCheck) Validate() error {
	if err := requireField("doctor_check.name", c.Name, MaxIdentifierBytes); err != nil {
		return err
	}
	if !c.State.Valid() {
		return invalid("doctor_check.state %q is not a known check state", truncateForMessage(string(c.State)))
	}
	if err := boundField("doctor_check.detail", c.Detail, MaxDetailBytes); err != nil {
		return err
	}
	if err := boundField("doctor_check.code", c.Code, MaxIdentifierBytes); err != nil {
		return err
	}
	if err := boundField("doctor_check.remediation", c.Remediation, MaxDetailBytes); err != nil {
		return err
	}
	return nil
}

// DoctorReport is the bounded diagnostic answer. The spec does not enumerate
// its fields; Section 22 enumerates the checks `doctor` performs and requires
// each to carry a separate reason code and remediation, so the report is a
// bounded list of those checks plus the build identity and deep-mode flag
// needed to interpret them. Individual metric values live in each check's
// detail so an unmeasured metric can be reported unavailable rather than zero.
type DoctorReport struct {
	Build     BuildInfo     `json:"build"`
	Deep      bool          `json:"deep"`
	Offline   bool          `json:"offline"`
	State     CheckState    `json:"state"`
	Checks    []DoctorCheck `json:"checks"`
	CheckedAt time.Time     `json:"checked_at"`
}

// Validate enforces the report shape and its bounded check list.
func (r DoctorReport) Validate() error {
	if !r.State.Valid() {
		return invalid("doctor_report.state %q is not a known check state", truncateForMessage(string(r.State)))
	}
	// The check list is not length-bounded. Doctor is the one command that must
	// produce a report on a broken workspace, so a report that fails validation
	// for being too long is the one outcome it may never have; the checks are
	// enumerated by the code itself, not by repository size.
	for _, c := range r.Checks {
		if err := c.Validate(); err != nil {
			return err
		}
	}
	return nil
}

// ScratchCollection is what one operator request to give the pooled scratch
// space back did: what each pool held, by what its surfaces were taken for,
// and what was actually released.
//
// Held and freed are reported separately and are not the same number. Held is
// what the pools were carrying when the request arrived; freed is what the
// filesystem was actually given back, which excludes an instance another live
// process still owns.
type ScratchCollection struct {
	Pools      []ScratchPool `json:"pools"`
	HeldBytes  uint64        `json:"held_bytes"`
	FreedBytes uint64        `json:"freed_bytes"`
}

// ScratchPool is one pool of ScratchCollection: the directory it serves, what
// it held by purpose, and what went.
type ScratchPool struct {
	Directory     string            `json:"directory"`
	HeldBytes     uint64            `json:"held_bytes"`
	HeldByPurpose map[string]uint64 `json:"held_by_purpose,omitempty"`
	FreedBytes    uint64            `json:"freed_bytes"`
	// StuckFrees names the removals this collection could not make, each with
	// the reason the filesystem gave. They are why freed can fall short of
	// held without the request having failed, and an operator reading the two
	// figures needs them to tell "the space is gone" from "the space is stuck".
	StuckFrees []StuckFree `json:"stuck_frees,omitempty"`
	// LeftAlone names the instances of this pool the collection did not
	// touch, and why. An instance is a whole pool of surfaces, so held minus
	// freed is mostly these; without them the report reads as a collection
	// that quietly did less than it counted.
	LeftAlone []UntouchedInstance `json:"left_alone,omitempty"`
}

// A StuckFree is one removal the space reclaimer tried to make and could not.
type StuckFree struct {
	Entry  string `json:"entry"`
	Reason string `json:"reason"`
}

// An UntouchedInstance is one pool instance a collection left as it was.
type UntouchedInstance struct {
	Instance  string `json:"instance"`
	HeldBytes uint64 `json:"held_bytes"`
	Reason    string `json:"reason"`
}

// A RunRecord is one recorded run of the coordinator as the run ledger holds
// it: what the run was, which generation it produced, what it cost and what it
// got through. It is the row every surface reports first, because a stage's
// share of the run means nothing without it.
//
// The measured fields are pointers and the counters are not, and that split is
// deliberate: a counter the run keeps itself is always available and zero is a
// real answer, while ProcessPeakRSSBytes is a platform measurement that a host
// may not expose, where absent must not read as a process using no memory.
type RunRecord struct {
	// RunID is the run's 32-byte identifier, hex-encoded. It exists from the
	// coordinator's first stage, which is earlier than any generation, so it
	// and not the generation is what names a run.
	RunID string `json:"run_id"`
	// Kind is `index`, `deferred` or `overlay`: an ordinary indexing run, a
	// deferred publication, or the per-process run that carries work with no
	// generation of its own.
	Kind string `json:"kind"`
	// RepositoryID is the repository the run indexed, hex-encoded.
	RepositoryID string `json:"repository_id"`
	// GenerationID is the generation this run produced, absent until the run
	// reaches one and for ever on a run that failed before it. A run without
	// a generation is still a run, and this is how it says so.
	GenerationID *int64    `json:"generation_id,omitempty"`
	StartedAt    time.Time `json:"started_at"`
	// FinishedAt is absent while the run is still going.
	FinishedAt *time.Time `json:"finished_at,omitempty"`
	// WallMS is the run's elapsed time: its measured wall once FinishedAt is
	// set, and its elapsed time so far while it is live.
	WallMS int64 `json:"wall_ms"`
	// Outcome is one of `running`, `ok`, `failed`, `subdivided`, `reused`,
	// `skipped` or `interrupted`; `interrupted` is a run whose process ended
	// without finishing it.
	Outcome         string `json:"outcome"`
	FileCount       int64  `json:"file_count"`
	SourceBytes     int64  `json:"source_bytes"`
	UnitsPlanned    int64  `json:"units_planned"`
	UnitsSucceeded  int64  `json:"units_succeeded"`
	UnitsFailed     int64  `json:"units_failed"`
	UnitsSubdivided int64  `json:"units_subdivided"`
	// EventsDropped counts the accounting events the bounded bus refused
	// because the collector was behind. A run never waits on its own
	// accounting, so the loss is counted rather than prevented: above zero,
	// the stage rows are known to be incomplete and a reader must say so.
	EventsDropped int64 `json:"events_dropped"`
	// ProcessPeakRSSBytes is the peak resident size of this process, absent
	// where the platform does not expose it. The stage rows carry null here
	// because a resident-size delta across overlapping work measures the
	// process and not the stage.
	ProcessPeakRSSBytes *uint64 `json:"process_peak_rss_bytes,omitempty"`
}

// A StageRecord is one span of a run: a stage, a unit, a provider step or a
// worker, with the part of the run's cost that is attributable to it. Per-file
// work is never a stage; a worker's row aggregates its files.
//
// Every field the platform measures is a pointer, so an unavailable figure is
// absent and never zero (Section 23). The counters ItemsIn and ItemsOut are
// not, because they are the stage's own tally.
type StageRecord struct {
	// Seq is the run's own ordinal for this stage, in the order the stages
	// were opened, counting from zero; ParentSeq is the ordinal of the stage
	// this one nests under, absent at the run's top level. Absent and not
	// zero, because zero is the first stage's own ordinal. The tree is
	// carried as ordinals because that is what both the recording and the
	// reading side know.
	Seq       int64     `json:"seq"`
	ParentSeq *int64    `json:"parent_seq,omitempty"`
	Stage     string    `json:"stage"`
	ScopeKey  string    `json:"scope_key,omitempty"`
	Provider  string    `json:"provider,omitempty"`
	StartedAt time.Time `json:"started_at"`
	// FinishedAt is absent while the stage is running and on a stage that was
	// still open when its run ended.
	FinishedAt *time.Time `json:"finished_at,omitempty"`
	// WallMS is the stage's measured wall once it has finished, and its
	// elapsed time so far while Running is true. Running is carried beside it
	// precisely so that a live stage's partial time is never presented as a
	// final measurement.
	WallMS  int64 `json:"wall_ms"`
	Running bool  `json:"running,omitempty"`
	// CPUUserMS and CPUSysMS are absent where no processor time can be
	// attributed to this stage, and CPUUnattributed then names why:
	// `overlapped` for in-process work that ran beside other goroutines,
	// where the process-wide counters measure the process rather than the
	// stage, and `unsampled` for a platform that does not expose them.
	CPUUserMS       *int64  `json:"cpu_user_ms,omitempty"`
	CPUSysMS        *int64  `json:"cpu_sys_ms,omitempty"`
	CPUUnattributed string  `json:"cpu_unattributed,omitempty"`
	PeakRSSBytes    *uint64 `json:"peak_rss_bytes,omitempty"`
	ReadBytes       *uint64 `json:"read_bytes,omitempty"`
	WriteBytes      *uint64 `json:"write_bytes,omitempty"`
	ItemsIn         int64   `json:"items_in"`
	ItemsOut        int64   `json:"items_out"`
	// Outcome takes the same spellings as RunRecord.Outcome.
	Outcome        string `json:"outcome"`
	DiagnosticCode string `json:"diagnostic_code,omitempty"`
	// Failure is the retained detail of a failed stage: the typed error's
	// message and details as the ledger stored them.
	Failure string `json:"failure,omitempty"`
	// ShareOfWall is this stage's wall as a fraction of its run's, as the
	// ledger computes it when the row is read. It is zero on a row that
	// reached a surface before its run's own wall was known.
	ShareOfWall float64 `json:"share_of_wall,omitempty"`
}

// validateRunLedger enforces the shape of a run row and its page of stage
// rows. Both surfaces that carry them -- the completed run an index reports
// and the latest run status reports -- have the same bound and the same
// non-negativity rules, so they share one check rather than drifting apart.
func validateRunLedger(field string, run *RunRecord, stages []StageRecord, omitted int64) *Error {
	if err := requireNonNegative(field+".stages_omitted", omitted); err != nil {
		return err
	}
	// Rows can only have been dropped from a page that is actually full, of a
	// run whose rows it is a page of: a count reported over a short list names
	// stages nothing could have omitted.
	if omitted > 0 && (run == nil || len(stages) < MaxRecordsPerResult) {
		return invalid("%s.stages_omitted is %d on a page of %d stages that dropped none", field, omitted, len(stages))
	}
	if run != nil {
		for _, c := range []struct {
			name  string
			value int64
		}{
			{field + ".run.wall_ms", run.WallMS},
			{field + ".run.file_count", run.FileCount},
			{field + ".run.source_bytes", run.SourceBytes},
			{field + ".run.units_planned", run.UnitsPlanned},
			{field + ".run.units_succeeded", run.UnitsSucceeded},
			{field + ".run.units_failed", run.UnitsFailed},
			{field + ".run.units_subdivided", run.UnitsSubdivided},
			{field + ".run.events_dropped", run.EventsDropped},
		} {
			if err := requireNonNegative(c.name, c.value); err != nil {
				return err
			}
		}
		if run.ProcessPeakRSSBytes != nil {
			if err := boundSigned64(field+".run.process_peak_rss_bytes", *run.ProcessPeakRSSBytes); err != nil {
				return err
			}
		}
	}
	if err := boundPage(field+".stages", len(stages)); err != nil {
		return err
	}
	for i, s := range stages {
		at := indexed(field+".stages", i)
		for _, c := range []struct {
			name  string
			value int64
		}{
			{at + ".seq", s.Seq},
			{at + ".wall_ms", s.WallMS},
			{at + ".items_in", s.ItemsIn},
			{at + ".items_out", s.ItemsOut},
		} {
			if err := requireNonNegative(c.name, c.value); err != nil {
				return err
			}
		}
		for _, c := range []struct {
			name  string
			value *int64
		}{
			{at + ".parent_seq", s.ParentSeq},
			{at + ".cpu_user_ms", s.CPUUserMS},
			{at + ".cpu_sys_ms", s.CPUSysMS},
		} {
			if c.value == nil {
				continue
			}
			if err := requireNonNegative(c.name, *c.value); err != nil {
				return err
			}
		}
		for _, c := range []struct {
			name  string
			value *uint64
		}{
			{at + ".peak_rss_bytes", s.PeakRSSBytes},
			{at + ".read_bytes", s.ReadBytes},
			{at + ".write_bytes", s.WriteBytes},
		} {
			if c.value == nil {
				continue
			}
			if err := boundSigned64(c.name, *c.value); err != nil {
				return err
			}
		}
	}
	return nil
}
