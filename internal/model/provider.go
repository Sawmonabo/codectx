package model

// Neutral provider descriptors and run metadata live here so that storage can
// record provenance and the coordinator can schedule work without either
// importing the other, and without any package importing a provider's native
// handles (Sections 7.1, 11.1).

import (
	"maps"
	"slices"
	"strconv"
)

// InvalidationScope is the unit granularity a provider reprocesses. A file-local
// provider reprocesses only changed files; a package or workspace provider
// reruns its declared larger unit.
type InvalidationScope string

const (
	InvalidationFile      InvalidationScope = "file"
	InvalidationPackage   InvalidationScope = "package"
	InvalidationWorkspace InvalidationScope = "workspace"
)

// Valid reports whether s is a known wire spelling.
func (s InvalidationScope) Valid() bool {
	switch s {
	case InvalidationFile, InvalidationPackage, InvalidationWorkspace:
		return true
	}
	return false
}

// RunState is the lowercase provider-run vocabulary of Section 22.
type RunState string

const (
	RunRunning   RunState = "running"
	RunSucceeded RunState = "succeeded"
	RunPartial   RunState = "partial"
	RunSkipped   RunState = "skipped"
	RunTimedOut  RunState = "timed_out"
	RunFailed    RunState = "failed"
	RunCanceled  RunState = "canceled"
)

// Valid reports whether s is a known wire spelling.
func (s RunState) Valid() bool {
	switch s {
	case RunRunning, RunSucceeded, RunPartial, RunSkipped, RunTimedOut, RunFailed, RunCanceled:
		return true
	}
	return false
}

// CapabilityStateValue is the per-capability, per-scope freshness vocabulary of
// Sections 13.3 and 22. A disabled optional tool is unavailable while the base
// generation stays fresh; an enabled provider that failed is failed.
type CapabilityStateValue string

const (
	CapabilityFresh       CapabilityStateValue = "fresh"
	CapabilityPartial     CapabilityStateValue = "partial"
	CapabilityStale       CapabilityStateValue = "stale"
	CapabilityUnavailable CapabilityStateValue = "unavailable"
	CapabilityFailed      CapabilityStateValue = "failed"
)

// Valid reports whether s is a known wire spelling.
func (s CapabilityStateValue) Valid() bool {
	switch s {
	case CapabilityFresh, CapabilityPartial, CapabilityStale, CapabilityUnavailable, CapabilityFailed:
		return true
	}
	return false
}

// GenerationHealth is the whole-generation health vocabulary of Sections 13.3
// and 22.
type GenerationHealth string

const (
	HealthFresh    GenerationHealth = "fresh"
	HealthDegraded GenerationHealth = "degraded"
	HealthFailed   GenerationHealth = "failed"
)

// Valid reports whether h is a known wire spelling.
func (h GenerationHealth) Valid() bool {
	switch h {
	case HealthFresh, HealthDegraded, HealthFailed:
		return true
	}
	return false
}

// GenerationStatus is the generations.status lifecycle vocabulary. Only active
// is served to ordinary queries.
type GenerationStatus string

const (
	GenerationStaging    GenerationStatus = "staging"
	GenerationActive     GenerationStatus = "active"
	GenerationSuperseded GenerationStatus = "superseded"
	GenerationFailed     GenerationStatus = "failed"
)

// Valid reports whether s is a known wire spelling.
func (s GenerationStatus) Valid() bool {
	switch s {
	case GenerationStaging, GenerationActive, GenerationSuperseded, GenerationFailed:
		return true
	}
	return false
}

// UnitState is the units.state lifecycle vocabulary. Only a sealed unit is
// eligible for generation membership; building, failed and quarantined output
// is invisible to queries.
type UnitState string

const (
	UnitBuilding    UnitState = "building"
	UnitSealed      UnitState = "sealed"
	UnitFailed      UnitState = "failed"
	UnitQuarantined UnitState = "quarantined"
)

// Valid reports whether s is a known wire spelling.
func (s UnitState) Valid() bool {
	switch s {
	case UnitBuilding, UnitSealed, UnitFailed, UnitQuarantined:
		return true
	}
	return false
}

// SourceBinding records whether a unit's facts are provably about the captured
// bytes. Section 11.4 forbids silently upgrading externally supplied data:
// without a verified input-hash association the unit stays unverified and is
// isolated from strict canonical compiler facts.
type SourceBinding string

const (
	SourceBindingVerified   SourceBinding = "verified"
	SourceBindingUnverified SourceBinding = "unverified"
)

// Valid reports whether b is a known wire spelling.
func (b SourceBinding) Valid() bool {
	return b == SourceBindingVerified || b == SourceBindingUnverified
}

// LeaseOwnerKind is the retention_leases owner vocabulary. A lease pins the
// generation or snapshot its owner still needs, so GC cannot collect it.
type LeaseOwnerKind string

const (
	LeaseQuery   LeaseOwnerKind = "query"
	LeaseCursor  LeaseOwnerKind = "cursor"
	LeaseSession LeaseOwnerKind = "session"
	LeaseStaging LeaseOwnerKind = "staging"
)

// Valid reports whether k is a known wire spelling.
func (k LeaseOwnerKind) Valid() bool {
	switch k {
	case LeaseQuery, LeaseCursor, LeaseSession, LeaseStaging:
		return true
	}
	return false
}

// ProviderDescriptor is a provider's static identity and scheduling contract.
// The Section 11.1 sketch carries no JSON tags; the explicit lowercase tags
// below are required because this record reaches the status and doctor wire
// surfaces.
type ProviderDescriptor struct {
	ID                string            `json:"id"`
	Version           string            `json:"version"`
	Capabilities      []string          `json:"capabilities"`
	DependsOn         []string          `json:"depends_on"`
	InvalidationScope InvalidationScope `json:"invalidation_scope"`
	Required          bool              `json:"required"`
}

// Validate enforces the descriptor's identity and bounded declaration lists.
func (d ProviderDescriptor) Validate() error {
	if err := requireField("provider_descriptor.id", d.ID, MaxIdentifierBytes); err != nil {
		return err
	}
	if err := requireField("provider_descriptor.version", d.Version, MaxIdentifierBytes); err != nil {
		return err
	}
	if err := boundCount("provider_descriptor.capabilities", len(d.Capabilities), MaxCapabilityStates); err != nil {
		return err
	}
	for i, c := range d.Capabilities {
		if err := requireField(indexed("provider_descriptor.capabilities", i), c, MaxIdentifierBytes); err != nil {
			return err
		}
	}
	if err := boundCount("provider_descriptor.depends_on", len(d.DependsOn), MaxFilterValues); err != nil {
		return err
	}
	for i, dep := range d.DependsOn {
		if err := requireField(indexed("provider_descriptor.depends_on", i), dep, MaxIdentifierBytes); err != nil {
			return err
		}
		if dep == d.ID {
			return invalid("provider_descriptor %q declares itself as a dependency", truncateForMessage(d.ID))
		}
	}
	if !d.InvalidationScope.Valid() {
		return invalid("provider_descriptor.invalidation_scope %q is not a known scope",
			truncateForMessage(string(d.InvalidationScope)))
	}
	return nil
}

// UnitSpec names one immutable unit of provider work. InputHash and
// DependencyHash are aggregate digests folded with Hasher over the canonical
// inputs the unit read and the keys of its declared dependencies; a unit may be
// reused only when both still match (Section 9.4).
type UnitSpec struct {
	ID              UnitID `json:"id"`
	ProviderID      string `json:"provider_id"`
	ProviderVersion string `json:"provider_version"`
	ScopeKey        string `json:"scope_key"`
	InputHash       string `json:"input_hash"`
	DependencyHash  string `json:"dependency_hash"`
}

// Validate enforces the units table constraints. Unlike Evidence, it cannot
// verify that ID matches NewUnitID: that derivation also consumes the analysis
// config hash, which Section 11.1 keeps off this record. Unit identity is
// therefore producer-owned here, and the store must recompute it at seal time.
func (s UnitSpec) Validate() error {
	if err := requireID("unit_spec.id", string(s.ID)); err != nil {
		return err
	}
	if err := requireField("unit_spec.provider_id", s.ProviderID, MaxIdentifierBytes); err != nil {
		return err
	}
	if err := requireField("unit_spec.provider_version", s.ProviderVersion, MaxIdentifierBytes); err != nil {
		return err
	}
	if err := requireField("unit_spec.scope_key", s.ScopeKey, MaxScopeKeyBytes); err != nil {
		return err
	}
	if err := requireID("unit_spec.input_hash", s.InputHash); err != nil {
		return err
	}
	if err := requireID("unit_spec.dependency_hash", s.DependencyHash); err != nil {
		return err
	}
	return nil
}

// MaxCapabilityDetails bounds the structured detail map a capability state may
// carry. It is the same bound Error.Details carries, for the same reason: a
// diagnostic surface a provider fills from its own findings must not grow with
// the size of the repository.
const MaxCapabilityDetails = MaxErrorDetails

// The reserved capability detail keys are the report fold's own bookkeeping:
// they are assertions about the FOLD (how many scopes a row stands for, how
// many units failed behind it, which merged values were cut, how many provider
// details did not fit), not particulars a provider published. They are never
// evicted to make room for a provider detail, because dropping one silently
// falsifies the report -- a lost `scopes` under-counts the scopes a row speaks
// for, and a lost `details_truncated` or `details_omitted` publishes a clipped
// or incomplete detail map as if it were whole.
const (
	// DetailScopes counts the scopes one folded row stands for.
	DetailScopes = "scopes"
	// DetailUnitsFailed counts the units that failed behind one row.
	DetailUnitsFailed = "units_failed"
	// DetailDetailsTruncated names the detail values that were cut to fit
	// MaxDetailBytes.
	DetailDetailsTruncated = "details_truncated"
	// DetailDetailsOmitted counts the provider details WithDetail dropped
	// because the map was full.
	DetailDetailsOmitted = "details_omitted"
)

// reservedCapabilityDetails is the set the constants above name. It is an
// array so its length is a compile-time constant: the provider budget below is
// derived from it, and a fifth reserved key must move that budget in the same
// edit that adds the key.
var reservedCapabilityDetails = [...]string{
	DetailScopes,
	DetailUnitsFailed,
	DetailDetailsTruncated,
	DetailDetailsOmitted,
}

// ReservedCapabilityDetail reports whether key is one the report fold owns.
// A provider that writes one of these keys is writing the fold's bookkeeping,
// not its own particulars; the fold's own writes go through the same door and
// are the reason the key is guaranteed room.
func ReservedCapabilityDetail(key string) bool {
	return slices.Contains(reservedCapabilityDetails[:], key)
}

// MaxProviderCapabilityDetails is how many details a PROVIDER may contribute.
// The reserved keys hold back the remainder of MaxCapabilityDetails so the
// fold can always write them, whatever a provider filled the map with.
const MaxProviderCapabilityDetails = MaxCapabilityDetails - len(reservedCapabilityDetails)

// CapabilityState reports one provider capability at one scope. The machine
// reason code is kept separate from user-readable remediation (Section 13.3).
// Details carries the machine-readable particulars of a state that is not
// fresh — which methods a partial capability skipped, which unit failed, which
// backend pass raised — as bounded key/value pairs rather than as extra
// capability rows invented to smuggle text through the scope column.
type CapabilityState struct {
	ProviderID     string               `json:"provider_id"`
	Capability     string               `json:"capability"`
	Scope          string               `json:"scope"`
	State          CapabilityStateValue `json:"state"`
	DiagnosticCode string               `json:"diagnostic_code,omitempty"`
	Details        map[string]string    `json:"details,omitempty"`
}

// WithDetail returns the state with one bounded diagnostic pair added, so a
// publisher can build a row in one expression. Values are truncated to
// MaxDetailBytes and the map is capped at MaxCapabilityDetails entries; an
// empty key is dropped, so a detail map never grows without bound and never
// carries a keyless value.
//
// A provider detail that does not fit the MaxProviderCapabilityDetails budget
// is dropped AND counted in DetailDetailsOmitted, so a reader is never handed a
// silently short detail map. A reserved key (ReservedCapabilityDetail) is
// always admitted: the budget holds room back for exactly those keys, so the
// fold's own bookkeeping -- including this omission count and the truncation
// flag -- can never be the thing that is evicted.
//
// The map is copied rather than written through. CapabilityState is a value
// type that lives in slices and is copied freely, and Details is a reference:
// writing into the receiver's own map would reach every copy that shares it,
// so one row's detail would silently appear on another row, or on a caller's
// state that was never passed to this method at all. The copy is bounded by
// MaxCapabilityDetails, so it costs nothing worth trading that away for.
func (c CapabilityState) WithDetail(key, value string) CapabilityState {
	if key == "" {
		return c
	}
	if _, replacing := c.Details[key]; !replacing && !ReservedCapabilityDetail(key) &&
		c.providerDetails() >= MaxProviderCapabilityDetails {
		// The dropped key is counted, not swallowed. This recurses exactly
		// once: DetailDetailsOmitted is reserved, so the branch above cannot
		// be taken for it.
		omitted, _ := strconv.Atoi(c.Details[DetailDetailsOmitted])
		return c.WithDetail(DetailDetailsOmitted, strconv.Itoa(omitted+1))
	}
	details := make(map[string]string, len(c.Details)+1)
	maps.Copy(details, c.Details)
	details[key] = TruncateDetail(value)
	c.Details = details
	return c
}

// providerDetails counts the details a provider contributed, which is what the
// MaxProviderCapabilityDetails budget bounds; the reserved keys are held back
// from that budget rather than charged to it.
func (c CapabilityState) providerDetails() int {
	n := 0
	for k := range c.Details {
		if !ReservedCapabilityDetail(k) {
			n++
		}
	}
	return n
}

// Validate enforces the generation_capabilities constraints.
func (c CapabilityState) Validate() error {
	if err := requireField("capability_state.provider_id", c.ProviderID, MaxIdentifierBytes); err != nil {
		return err
	}
	if err := requireField("capability_state.capability", c.Capability, MaxIdentifierBytes); err != nil {
		return err
	}
	if err := requireField("capability_state.scope", c.Scope, MaxScopeKeyBytes); err != nil {
		return err
	}
	if !c.State.Valid() {
		return invalid("capability_state.state %q is not a known capability state", truncateForMessage(string(c.State)))
	}
	if err := boundField("capability_state.diagnostic_code", c.DiagnosticCode, MaxIdentifierBytes); err != nil {
		return err
	}
	if err := boundCount("capability_state.details", len(c.Details), MaxCapabilityDetails); err != nil {
		return err
	}
	for k, v := range c.Details {
		if err := requireField("capability_state.details key", k, MaxIdentifierBytes); err != nil {
			return err
		}
		if err := boundField("capability_state.details["+k+"]", v, MaxDetailBytes); err != nil {
			return err
		}
	}
	return nil
}

// validateCapabilityStates bounds and checks a completeness list; every public
// result carries one.
// The list itself is NOT length-bounded: a completeness report names every
// capability the answer actually rests on, and failing the answer because the
// repository has more capabilities than a constant anticipated withholds the
// very report the caller needs. Each row is still bounded in every field.
func validateCapabilityStates(field string, states []CapabilityState) error {
	for _, s := range states {
		if err := s.Validate(); err != nil {
			return err
		}
	}
	return nil
}

// ProviderResult is what one provider run reports back to the coordinator. The
// Section 11.1 sketch carries no JSON tags; the explicit tags below are
// required because run counters surface in status and doctor output.
type ProviderResult struct {
	RunID          ProviderRunID     `json:"run_id"`
	State          RunState          `json:"state"`
	Capabilities   []CapabilityState `json:"capabilities"`
	RecordsEmitted uint64            `json:"records_emitted"`
	BytesProcessed uint64            `json:"bytes_processed"`
}

// Validate enforces the provider_runs constraints.
func (r ProviderResult) Validate() error {
	if err := requireID("provider_result.run_id", string(r.RunID)); err != nil {
		return err
	}
	if !r.State.Valid() {
		return invalid("provider_result.state %q is not a known run state", truncateForMessage(string(r.State)))
	}
	if err := validateCapabilityStates("provider_result.capabilities", r.Capabilities); err != nil {
		return err
	}
	if err := boundSigned64("provider_result.records_emitted", r.RecordsEmitted); err != nil {
		return err
	}
	if err := boundSigned64("provider_result.bytes_processed", r.BytesProcessed); err != nil {
		return err
	}
	return nil
}
