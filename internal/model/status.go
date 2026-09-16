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
	// claim checkable from outside.
	FreedBytes  *uint64 `json:"freed_bytes,omitempty"`
	UnitsReused *int64  `json:"units_reused,omitempty"`
	UnitsParsed *int64  `json:"units_parsed,omitempty"`
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
