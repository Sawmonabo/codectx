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
	Binding       Binding           `json:"binding"`
	Health        GenerationHealth  `json:"health"`
	Status        GenerationStatus  `json:"status"`
	Completeness  []CapabilityState `json:"completeness"`
	UnitsReused   int64             `json:"units_reused"`
	UnitsBuilt    int64             `json:"units_built"`
	FilesParsed   int64             `json:"files_parsed"`
	FilesCaptured int64             `json:"files_captured"`
	Runs          []ProviderResult  `json:"runs"`
	StartedAt     time.Time         `json:"started_at"`
	CompletedAt   time.Time         `json:"completed_at"`
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
	} {
		if err := requireNonNegative(count.field, count.value); err != nil {
			return err
		}
	}
	if err := boundCount("index_result.runs", len(r.Runs), MaxRecordsPerResult); err != nil {
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
// that Section 13.2 requires be reported. Resource accounting is reported
// separately by DoctorReport so an ordinary status stays cheap.
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
	return nil
}

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
)

// Valid reports whether s is a known wire spelling.
func (s CheckState) Valid() bool {
	switch s {
	case CheckPass, CheckWarn, CheckFail, CheckUnavailable:
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
	if err := boundCount("doctor_report.checks", len(r.Checks), MaxCapabilityStates); err != nil {
		return err
	}
	for _, c := range r.Checks {
		if err := c.Validate(); err != nil {
			return err
		}
	}
	return nil
}
