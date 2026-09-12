package model

import (
	"sort"
	"strconv"
	"time"
)

// WorkflowState is the read_sessions state vocabulary of Section 17.1. Every
// transition uses a compare-and-swap state version, so two clients cannot both
// advance from the same version.
type WorkflowState string

const (
	StateSweepOpen       WorkflowState = "sweep_open"
	StateVerifyOpen      WorkflowState = "verify_open"
	StateConsolidateOpen WorkflowState = "consolidate_open"
	StateComplete        WorkflowState = "complete"
	StateClosed          WorkflowState = "closed"
)

// Valid reports whether s is a known wire spelling.
func (s WorkflowState) Valid() bool {
	switch s {
	case StateSweepOpen, StateVerifyOpen, StateConsolidateOpen, StateComplete, StateClosed:
		return true
	}
	return false
}

// ObservationKind is the session_observations vocabulary of Section 17.2.
type ObservationKind string

const (
	ObservationAcceptFact    ObservationKind = "accept_fact"
	ObservationRejectFact    ObservationKind = "reject_fact"
	ObservationContradiction ObservationKind = "contradiction"
	ObservationUnresolved    ObservationKind = "unresolved"
	ObservationScopeReview   ObservationKind = "scope_review"
)

// Valid reports whether k is a known wire spelling.
func (k ObservationKind) Valid() bool {
	switch k {
	case ObservationAcceptFact, ObservationRejectFact, ObservationContradiction,
		ObservationUnresolved, ObservationScopeReview:
		return true
	}
	return false
}

// SourceCitation is a bounded reference to already-served source. Section 17.2
// requires cited intervals to be confirmed served by the same actor, so the
// pinned content hash travels with the interval.
type SourceCitation struct {
	FileID      FileID    `json:"file_id"`
	ContentHash string    `json:"content_hash"`
	Bytes       ByteRange `json:"bytes"`
}

// Validate enforces a nonempty cited interval: citing zero bytes cites nothing.
func (c SourceCitation) Validate() error {
	if err := requireID("source_citation.file_id", string(c.FileID)); err != nil {
		return err
	}
	if err := requireID("source_citation.content_hash", c.ContentHash); err != nil {
		return err
	}
	if err := c.Bytes.ValidateNonEmpty("source_citation.bytes"); err != nil {
		return err
	}
	return nil
}

// ClaimReference is one canonical claim an observation is about. Exactly one of
// NodeID, RelationID or Source is named, so a contradiction between two claims
// is unambiguous.
type ClaimReference struct {
	NodeID     NodeID          `json:"node_id,omitempty"`
	RelationID RelationID      `json:"relation_id,omitempty"`
	Source     *SourceCitation `json:"source,omitempty"`
}

// Validate enforces the one-subject rule.
func (r ClaimReference) Validate() error {
	named := 0
	if r.NodeID != "" {
		named++
	}
	if r.RelationID != "" {
		named++
	}
	if r.Source != nil {
		named++
	}
	if named != 1 {
		return invalid("claim_reference must name exactly one of node_id, relation_id or source, got %d", named)
	}
	if err := optionalID("claim_reference.node_id", string(r.NodeID)); err != nil {
		return err
	}
	if err := optionalID("claim_reference.relation_id", string(r.RelationID)); err != nil {
		return err
	}
	if r.Source != nil {
		if err := r.Source.Validate(); err != nil {
			return err
		}
	}
	return nil
}

// canonical folds one claim reference into a delimiter-safe digest. It is the
// deduplication key for the distinctness guards below and the sort key for
// observation identity, so two spellings of the same claim can never count as
// two distinct claims.
func (r ClaimReference) canonical() string {
	h := NewHasher(domainClaimRef)
	h.AddString(string(r.NodeID))
	h.AddString(string(r.RelationID))
	if r.Source == nil {
		h.AddString("")
		h.AddString("")
		h.AddString("")
		h.AddString("")
		return h.Sum()
	}
	h.AddString(string(r.Source.FileID))
	h.AddString(r.Source.ContentHash)
	h.AddString(strconv.FormatUint(r.Source.Bytes.Start, 10))
	h.AddString(strconv.FormatUint(r.Source.Bytes.End, 10))
	return h.Sum()
}

// ScopeReviewCategory is one of the eight required attestation categories of
// Section 17.2. A review must answer every one of them.
type ScopeReviewCategory string

const (
	ReviewCompleteFilesRead    ScopeReviewCategory = "complete_files_read"
	ReviewCallersConsumers     ScopeReviewCategory = "callers_consumers"
	ReviewContractsTypes       ScopeReviewCategory = "contracts_types"
	ReviewStateLifecycle       ScopeReviewCategory = "state_lifecycle"
	ReviewDependencies         ScopeReviewCategory = "dependencies"
	ReviewIntegrationPoints    ScopeReviewCategory = "integration_boundaries"
	ReviewSharedUtilities      ScopeReviewCategory = "shared_utilities_considered"
	ReviewRemainingUncertainty ScopeReviewCategory = "remaining_uncertainty"
)

// scopeReviewCategories is the exhaustive required set, in canonical order.
var scopeReviewCategories = [...]ScopeReviewCategory{
	ReviewCompleteFilesRead, ReviewCallersConsumers, ReviewContractsTypes,
	ReviewStateLifecycle, ReviewDependencies, ReviewIntegrationPoints,
	ReviewSharedUtilities, ReviewRemainingUncertainty,
}

// Valid reports whether c is a known wire spelling.
func (c ScopeReviewCategory) Valid() bool {
	for _, known := range scopeReviewCategories {
		if c == known {
			return true
		}
	}
	return false
}

// ScopeReviewEntry is one answered category. Section 17.2 requires each to
// contain relevant source references or an explicit source-backed explanation
// of non-applicability, which is why Note is mandatory even when References is
// empty. Blocking marks an uncertainty that prevents strict readiness.
type ScopeReviewEntry struct {
	Category   ScopeReviewCategory `json:"category"`
	Note       string              `json:"note"`
	References []ClaimReference    `json:"references,omitempty"`
	Blocking   bool                `json:"blocking"`
}

// Validate enforces the entry shape.
func (e ScopeReviewEntry) Validate() error {
	if !e.Category.Valid() {
		return invalid("scope_review_entry.category %q is not a required review category", truncateForMessage(string(e.Category)))
	}
	if err := requireTrimmed("scope_review_entry.note", e.Note, MaxNoteBytes); err != nil {
		return err
	}
	if err := boundCount("scope_review_entry.references", len(e.References), MaxObservationReferences); err != nil {
		return err
	}
	for _, r := range e.References {
		if err := r.Validate(); err != nil {
			return err
		}
	}
	return nil
}

// ScopeReview is the structured actor attestation bound to one manifest hash
// and scope version.
type ScopeReview struct {
	ManifestHash string             `json:"manifest_hash"`
	ScopeVersion int                `json:"scope_version"`
	Entries      []ScopeReviewEntry `json:"entries"`
}

// Validate enforces that every required category is answered exactly once.
func (r ScopeReview) Validate() error {
	if err := requireID("scope_review.manifest_hash", r.ManifestHash); err != nil {
		return err
	}
	if r.ScopeVersion < 1 {
		return invalid("scope_review.scope_version is %d; versions start at 1", r.ScopeVersion)
	}
	// The duplicate and required-category checks below already make more than
	// len(scopeReviewCategories) distinct entries impossible, but bounding the
	// count first means a hostile oversized slice is rejected before it is walked.
	if err := boundCount("scope_review.entries", len(r.Entries), len(scopeReviewCategories)); err != nil {
		return err
	}
	seen := make(map[ScopeReviewCategory]bool, len(scopeReviewCategories))
	for _, e := range r.Entries {
		if err := e.Validate(); err != nil {
			return err
		}
		if seen[e.Category] {
			return invalid("scope_review answers category %q more than once", e.Category)
		}
		seen[e.Category] = true
	}
	for _, required := range scopeReviewCategories {
		if !seen[required] {
			return invalid("scope_review is missing the required category %q", required)
		}
	}
	return nil
}

// ObservationRequest records an explicit actor observation. Section 17.2 fixes
// the per-kind guards enforced below; codectx never generates the note itself.
type ObservationRequest struct {
	SessionID     SessionID        `json:"session_id"`
	ActorID       string           `json:"actor_id"`
	ExpectedScope int              `json:"expected_scope_version"`
	Kind          ObservationKind  `json:"kind"`
	References    []ClaimReference `json:"references,omitempty"`
	Review        *ScopeReview     `json:"review,omitempty"`
	Note          string           `json:"note"`
}

// Validate enforces the per-kind reference guards: accept/reject requires at
// least one relation, contradiction requires at least two distinct claims,
// unresolved requires a specific node, relation or source, and scope_review
// carries the structured attestation instead of loose references.
func (r ObservationRequest) Validate() error {
	if err := requireID("observation.session_id", string(r.SessionID)); err != nil {
		return err
	}
	if err := requireTrimmed("observation.actor_id", r.ActorID, MaxIdentifierBytes); err != nil {
		return err
	}
	if r.ExpectedScope < 1 {
		return invalid("observation.expected_scope_version is %d; versions start at 1", r.ExpectedScope)
	}
	if !r.Kind.Valid() {
		return invalid("observation.kind %q is not a known observation kind", truncateForMessage(string(r.Kind)))
	}
	if err := requireTrimmed("observation.note", r.Note, MaxNoteBytes); err != nil {
		return err
	}
	if err := boundCount("observation.references", len(r.References), MaxObservationReferences); err != nil {
		return err
	}
	seen := make(map[string]bool, len(r.References))
	relations := 0
	for i, ref := range r.References {
		if err := ref.Validate(); err != nil {
			return err
		}
		key := ref.canonical()
		if seen[key] {
			return invalid("%s repeats an earlier reference; claim references must be distinct", indexed("observation.references", i))
		}
		seen[key] = true
		if ref.RelationID != "" {
			relations++
		}
	}

	switch r.Kind {
	case ObservationAcceptFact, ObservationRejectFact:
		if relations == 0 {
			return invalid("observation kind %q requires at least one relation reference", r.Kind)
		}
	case ObservationContradiction:
		if len(r.References) < 2 {
			return invalid("observation kind %q requires at least two distinct claim references, got %d", r.Kind, len(r.References))
		}
	case ObservationUnresolved:
		if len(r.References) == 0 {
			return invalid("observation kind %q requires a specific node, relation or source reference", r.Kind)
		}
	case ObservationScopeReview:
		if r.Review == nil {
			return invalid("observation kind %q requires the structured scope review", r.Kind)
		}
		if err := r.Review.Validate(); err != nil {
			return err
		}
	}
	if r.Kind != ObservationScopeReview && r.Review != nil {
		return invalid("observation kind %q must not carry a scope review", r.Kind)
	}
	return nil
}

// Observation is the stored, immutable record. Its ID hashes session, actor,
// scope, kind, canonically sorted semantic references and the note, so a
// duplicate submission is idempotent and an in-place mutation is impossible
// (Section 17.2).
type Observation struct {
	ID           ObservationID    `json:"id"`
	SessionID    SessionID        `json:"session_id"`
	ActorID      string           `json:"actor_id"`
	ScopeVersion int              `json:"scope_version"`
	Kind         ObservationKind  `json:"kind"`
	References   []ClaimReference `json:"references,omitempty"`
	Review       *ScopeReview     `json:"review,omitempty"`
	Note         string           `json:"note"`
	CreatedAt    time.Time        `json:"created_at"`
}

// Validate enforces the stored observation shape.
func (o Observation) Validate() error {
	if err := requireID("observation_record.id", string(o.ID)); err != nil {
		return err
	}
	return ObservationRequest{
		SessionID:     o.SessionID,
		ActorID:       o.ActorID,
		ExpectedScope: o.ScopeVersion,
		Kind:          o.Kind,
		References:    o.References,
		Review:        o.Review,
		Note:          o.Note,
	}.Validate()
}

// NewObservationID derives observation identity from session, actor, scope,
// kind, canonically sorted semantic references and the note (Section 17.2).
// Sorting is what makes a duplicate submission idempotent regardless of the
// order the client listed its references in; the creation timestamp is excluded
// so resubmission cannot forge a second record.
func NewObservationID(r ObservationRequest) ObservationID {
	h := NewHasher(domainObservation)
	h.AddString(string(r.SessionID))
	h.AddString(r.ActorID)
	h.AddString(strconv.Itoa(r.ExpectedScope))
	h.AddString(string(r.Kind))
	keys := make([]string, 0, len(r.References))
	for _, ref := range r.References {
		keys = append(keys, ref.canonical())
	}
	sort.Strings(keys)
	h.AddString(strconv.Itoa(len(keys)))
	for _, k := range keys {
		h.AddString(k)
	}
	if r.Review == nil {
		h.AddString("")
	} else {
		h.AddString(r.Review.canonical())
	}
	h.AddString(r.Note)
	return ObservationID(h.Sum())
}

// canonical folds a scope review into a digest in the fixed category order, so
// the same attestation always produces the same observation identity.
func (r ScopeReview) canonical() string {
	h := NewHasher(domainScopeReview)
	h.AddString(r.ManifestHash)
	h.AddString(strconv.Itoa(r.ScopeVersion))
	byCategory := make(map[ScopeReviewCategory]ScopeReviewEntry, len(r.Entries))
	for _, e := range r.Entries {
		byCategory[e.Category] = e
	}
	for _, category := range scopeReviewCategories {
		e, ok := byCategory[category]
		h.AddString(string(category))
		if !ok {
			h.AddString("")
			h.AddString("")
			h.AddString("0")
			continue
		}
		h.AddString(e.Note)
		keys := make([]string, 0, len(e.References))
		for _, ref := range e.References {
			keys = append(keys, ref.canonical())
		}
		sort.Strings(keys)
		inner := NewHasher(domainClaimRef)
		for _, k := range keys {
			inner.AddString(k)
		}
		h.AddString(inner.Sum())
		if e.Blocking {
			h.AddString("1")
		} else {
			h.AddString("0")
		}
	}
	return h.Sum()
}

// SessionRequest names one session operation by the actor that owns it. Section
// 16.1 requires the actor on status, close and every other session operation:
// one subagent's session never satisfies another's requirement.
type SessionRequest struct {
	SessionID SessionID `json:"session_id"`
	ActorID   string    `json:"actor_id"`
}

// Validate enforces the request shape.
func (r SessionRequest) Validate() error {
	if err := requireID("session.session_id", string(r.SessionID)); err != nil {
		return err
	}
	if err := requireTrimmed("session.actor_id", r.ActorID, MaxIdentifierBytes); err != nil {
		return err
	}
	return nil
}

// SessionStatus is the honest readiness answer of Sections 16.3 and 17.1. The
// two readiness booleans are deliberately separate: a session stays fully read
// for its historical snapshot after a newer generation activates, while strict
// implementation readiness additionally requires the verify phase, complete
// resolved scope, a current-scope review, no blocking uncertainty and no
// required-file waiver.
type SessionStatus struct {
	SessionID               SessionID         `json:"session_id"`
	ActorID                 string            `json:"actor_id"`
	Binding                 Binding           `json:"binding"`
	ManifestID              ManifestID        `json:"manifest_id"`
	Phase                   Phase             `json:"phase"`
	State                   WorkflowState     `json:"state"`
	StateVersion            int               `json:"state_version"`
	ScopeVersion            int               `json:"scope_version"`
	Completeness            []CapabilityState `json:"completeness"`
	ScopeComplete           bool              `json:"scope_complete"`
	ReadCompleteForSnapshot bool              `json:"read_complete_for_snapshot"`
	ReadyForImplementation  bool              `json:"ready_for_implementation"`
	StrictGateSatisfied     bool              `json:"strict_gate_satisfied"`
	Superseded              bool              `json:"superseded"`
	RequiredFiles           int64             `json:"required_files"`
	FullyServedFiles        int64             `json:"fully_served_files"`
	WaivedFiles             int64             `json:"waived_files"`
	CreatedAt               time.Time         `json:"created_at"`
	ExpiresAt               time.Time         `json:"expires_at"`
}

// Validate enforces the status shape and the invariant that strict readiness is
// never reported alongside a waived required file (Section 17.3).
func (s SessionStatus) Validate() error {
	if err := requireID("session_status.session_id", string(s.SessionID)); err != nil {
		return err
	}
	if err := requireTrimmed("session_status.actor_id", s.ActorID, MaxIdentifierBytes); err != nil {
		return err
	}
	if err := s.Binding.Validate(); err != nil {
		return err
	}
	if err := requireID("session_status.manifest_id", string(s.ManifestID)); err != nil {
		return err
	}
	if !s.Phase.Valid() {
		return invalid("session_status.phase %q is not a known phase", truncateForMessage(string(s.Phase)))
	}
	if !s.State.Valid() {
		return invalid("session_status.state %q is not a known workflow state", truncateForMessage(string(s.State)))
	}
	if s.StateVersion < 1 || s.ScopeVersion < 1 {
		return invalid("session_status versions start at 1, got state_version=%d scope_version=%d", s.StateVersion, s.ScopeVersion)
	}
	if err := validateCapabilityStates("session_status.completeness", s.Completeness); err != nil {
		return err
	}
	for _, count := range []struct {
		field string
		value int64
	}{
		{"session_status.required_files", s.RequiredFiles},
		{"session_status.fully_served_files", s.FullyServedFiles},
		{"session_status.waived_files", s.WaivedFiles},
	} {
		if err := requireNonNegative(count.field, count.value); err != nil {
			return err
		}
	}
	if s.ReadyForImplementation && s.WaivedFiles > 0 {
		return invalid("session_status reports ready_for_implementation with %d waived files; a waiver never grants strict readiness", s.WaivedFiles)
	}
	if s.ReadyForImplementation && !s.StrictGateSatisfied {
		return invalid("session_status reports ready_for_implementation without a satisfied strict gate")
	}
	return nil
}

// AdvanceRequest is a version-checked guarded transition.
type AdvanceRequest struct {
	SessionID       SessionID     `json:"session_id"`
	ActorID         string        `json:"actor_id"`
	Target          WorkflowState `json:"target"`
	ExpectedVersion int           `json:"expected_version"`
}

// Validate enforces the request shape and rejects a target that is not a
// reachable forward state.
func (r AdvanceRequest) Validate() error {
	if err := requireID("advance.session_id", string(r.SessionID)); err != nil {
		return err
	}
	if err := requireTrimmed("advance.actor_id", r.ActorID, MaxIdentifierBytes); err != nil {
		return err
	}
	switch r.Target {
	case StateVerifyOpen, StateConsolidateOpen, StateComplete, StateClosed:
	default:
		return invalid("advance.target %q is not a reachable transition target", truncateForMessage(string(r.Target)))
	}
	if r.ExpectedVersion < 1 {
		return invalid("advance.expected_version is %d; versions start at 1", r.ExpectedVersion)
	}
	return nil
}

// WorkflowStatus is the answer to a transition: the new state and the version
// the next compare-and-swap must present. Section 17.1 makes the manifest
// pointer versioned, so a transition that recompiled one reports its ID.
type WorkflowStatus struct {
	SessionID    SessionID     `json:"session_id"`
	State        WorkflowState `json:"state"`
	StateVersion int           `json:"state_version"`
	ScopeVersion int           `json:"scope_version"`
	ManifestID   ManifestID    `json:"manifest_id"`
	Phase        Phase         `json:"phase"`
}

// Validate enforces the status shape.
func (s WorkflowStatus) Validate() error {
	if err := requireID("workflow_status.session_id", string(s.SessionID)); err != nil {
		return err
	}
	if !s.State.Valid() {
		return invalid("workflow_status.state %q is not a known workflow state", truncateForMessage(string(s.State)))
	}
	if s.StateVersion < 1 || s.ScopeVersion < 1 {
		return invalid("workflow_status versions start at 1, got state_version=%d scope_version=%d", s.StateVersion, s.ScopeVersion)
	}
	if err := requireID("workflow_status.manifest_id", string(s.ManifestID)); err != nil {
		return err
	}
	if !s.Phase.Valid() {
		return invalid("workflow_status.phase %q is not a known phase", truncateForMessage(string(s.Phase)))
	}
	return nil
}

// FactReference carries "RelationID, sorted EvidenceIDs and ObservationID"
// (Section 17.3): the canonical claim, the evidence that supports it and the
// observation in which the actor accepted or rejected it.
type FactReference struct {
	RelationID    RelationID    `json:"relation_id"`
	EvidenceIDs   []EvidenceID  `json:"evidence_ids"`
	ObservationID ObservationID `json:"observation_id"`
}

// Validate enforces the reference shape.
func (r FactReference) Validate() error {
	if err := requireID("fact_reference.relation_id", string(r.RelationID)); err != nil {
		return err
	}
	if err := boundCount("fact_reference.evidence_ids", len(r.EvidenceIDs), MaxEvidencePerFact); err != nil {
		return err
	}
	for i, id := range r.EvidenceIDs {
		if err := requireID(indexed("fact_reference.evidence_ids", i), string(id)); err != nil {
			return err
		}
	}
	if err := requireID("fact_reference.observation_id", string(r.ObservationID)); err != nil {
		return err
	}
	return nil
}

// ObservationReference carries "ObservationID, canonical node/relation/source
// references and note" (Section 17.3): the capsule's view of a contradiction or
// unresolved item without re-embedding the whole observation record.
type ObservationReference struct {
	ObservationID ObservationID    `json:"observation_id"`
	Kind          ObservationKind  `json:"kind"`
	References    []ClaimReference `json:"references"`
	Note          string           `json:"note"`
}

// Validate enforces the reference shape.
func (r ObservationReference) Validate() error {
	if err := requireID("observation_reference.observation_id", string(r.ObservationID)); err != nil {
		return err
	}
	if !r.Kind.Valid() {
		return invalid("observation_reference.kind %q is not a known observation kind", truncateForMessage(string(r.Kind)))
	}
	if err := boundCount("observation_reference.references", len(r.References), MaxObservationReferences); err != nil {
		return err
	}
	for _, ref := range r.References {
		if err := ref.Validate(); err != nil {
			return err
		}
	}
	if err := requireTrimmed("observation_reference.note", r.Note, MaxNoteBytes); err != nil {
		return err
	}
	return nil
}

// Capsule is the deterministic completion record of Section 17.3. It is
// assembled only from stored facts and explicit observations; codectx generates
// no summary of its own.
type Capsule struct {
	SessionID           SessionID              `json:"session_id"`
	ActorID             string                 `json:"actor_id"`
	Binding             Binding                `json:"binding"`
	ManifestHash        string                 `json:"manifest_hash"`
	ScopeVersion        int                    `json:"scope_version"`
	Scope               []NodeID               `json:"scope"`
	AcceptedFacts       []FactReference        `json:"accepted_facts"`
	RejectedFacts       []FactReference        `json:"rejected_facts"`
	Contradictions      []ObservationReference `json:"contradictions"`
	Unresolved          []ObservationReference `json:"unresolved"`
	ScopeReviewIDs      []string               `json:"scope_review_ids"`
	Coverage            []FileCoverage         `json:"coverage"`
	Waivers             []WaiverRecord         `json:"waivers"`
	Completeness        []CapabilityState      `json:"completeness"`
	StrictGateSatisfied bool                   `json:"strict_gate_satisfied"`
	CanonicalHash       string                 `json:"canonical_hash"`
	CreatedAt           time.Time              `json:"created_at"`
}

// Validate enforces the capsule shape and the invariant that a capsule with
// waivers can never claim a satisfied strict gate.
func (c Capsule) Validate() error {
	if err := requireID("capsule.session_id", string(c.SessionID)); err != nil {
		return err
	}
	if err := requireTrimmed("capsule.actor_id", c.ActorID, MaxIdentifierBytes); err != nil {
		return err
	}
	if err := c.Binding.Validate(); err != nil {
		return err
	}
	if err := requireID("capsule.manifest_hash", c.ManifestHash); err != nil {
		return err
	}
	if err := requireID("capsule.canonical_hash", c.CanonicalHash); err != nil {
		return err
	}
	if c.ScopeVersion < 1 {
		return invalid("capsule.scope_version is %d; versions start at 1", c.ScopeVersion)
	}
	// Section 17.3 requires every capsule list to be a bounded record set: the
	// capsule is a durable artifact that a later session replays, so an
	// unbounded list here becomes an unbounded read forever after.
	if err := boundCount("capsule.scope", len(c.Scope), MaxRecordsPerResult); err != nil {
		return err
	}
	for i, id := range c.Scope {
		if err := requireID(indexed("capsule.scope", i), string(id)); err != nil {
			return err
		}
	}
	for _, group := range []struct {
		field string
		refs  []FactReference
	}{
		{"capsule.accepted_facts", c.AcceptedFacts},
		{"capsule.rejected_facts", c.RejectedFacts},
	} {
		if err := boundCount(group.field, len(group.refs), MaxRecordsPerResult); err != nil {
			return err
		}
		for _, f := range group.refs {
			if err := f.Validate(); err != nil {
				return err
			}
		}
	}
	for _, group := range []struct {
		field string
		refs  []ObservationReference
	}{
		{"capsule.contradictions", c.Contradictions},
		{"capsule.unresolved", c.Unresolved},
	} {
		if err := boundCount(group.field, len(group.refs), MaxRecordsPerResult); err != nil {
			return err
		}
		for _, o := range group.refs {
			if err := o.Validate(); err != nil {
				return err
			}
		}
	}
	if err := boundCount("capsule.scope_review_ids", len(c.ScopeReviewIDs), MaxRecordsPerResult); err != nil {
		return err
	}
	for i, id := range c.ScopeReviewIDs {
		if err := requireID(indexed("capsule.scope_review_ids", i), id); err != nil {
			return err
		}
	}
	if err := boundCount("capsule.coverage", len(c.Coverage), MaxRecordsPerResult); err != nil {
		return err
	}
	for _, f := range c.Coverage {
		if err := f.Validate(); err != nil {
			return err
		}
	}
	if err := boundCount("capsule.waivers", len(c.Waivers), MaxRecordsPerResult); err != nil {
		return err
	}
	for _, w := range c.Waivers {
		if err := w.Validate(); err != nil {
			return err
		}
	}
	if err := validateCapabilityStates("capsule.completeness", c.Completeness); err != nil {
		return err
	}
	if c.StrictGateSatisfied && len(c.Waivers) > 0 {
		return invalid("capsule claims a satisfied strict gate with %d waivers recorded", len(c.Waivers))
	}
	return nil
}

// CapsulePage is one bounded page of a capsule. Section 17.3 requires paginated
// or streamed export when a capsule exceeds the generic response budget, so the
// header travels with each page and the heavy lists are paged separately.
// CapsuleView selects which of the capsule's bounded record lists a page
// projects. The spellings are the Capsule field's own JSON tags, so the view
// name and the list it returns cannot drift apart. It is closed for the same
// reason ContextView is: an unrecognized view would otherwise page an empty
// projection and read as "this capsule recorded nothing".
type CapsuleView string

const (
	CapsuleViewAcceptedFacts  CapsuleView = "accepted_facts"
	CapsuleViewRejectedFacts  CapsuleView = "rejected_facts"
	CapsuleViewContradictions CapsuleView = "contradictions"
	CapsuleViewUnresolved     CapsuleView = "unresolved"
	CapsuleViewCoverage       CapsuleView = "coverage"
	CapsuleViewWaivers        CapsuleView = "waivers"
)

// Valid reports whether v is a known wire spelling.
func (v CapsuleView) Valid() bool {
	switch v {
	case CapsuleViewAcceptedFacts, CapsuleViewRejectedFacts, CapsuleViewContradictions,
		CapsuleViewUnresolved, CapsuleViewCoverage, CapsuleViewWaivers:
		return true
	}
	return false
}

type CapsulePage struct {
	Meta           QueryMeta              `json:"meta"`
	SessionID      SessionID              `json:"session_id"`
	ManifestHash   string                 `json:"manifest_hash"`
	CanonicalHash  string                 `json:"canonical_hash"`
	View           CapsuleView            `json:"view"`
	AcceptedFacts  []FactReference        `json:"accepted_facts,omitempty"`
	RejectedFacts  []FactReference        `json:"rejected_facts,omitempty"`
	Contradictions []ObservationReference `json:"contradictions,omitempty"`
	Unresolved     []ObservationReference `json:"unresolved,omitempty"`
	Coverage       []FileCoverage         `json:"coverage,omitempty"`
	Waivers        []WaiverRecord         `json:"waivers,omitempty"`
}

// Validate enforces the page shape and its per-page item bound.
func (p CapsulePage) Validate() error {
	if err := p.Meta.Validate(); err != nil {
		return err
	}
	if err := requireID("capsule_page.session_id", string(p.SessionID)); err != nil {
		return err
	}
	if err := requireID("capsule_page.manifest_hash", p.ManifestHash); err != nil {
		return err
	}
	if err := requireID("capsule_page.canonical_hash", p.CanonicalHash); err != nil {
		return err
	}
	if !p.View.Valid() {
		return invalid("capsule_page.view %q is not a known capsule view", truncateForMessage(string(p.View)))
	}
	items := len(p.AcceptedFacts) + len(p.RejectedFacts) + len(p.Contradictions) +
		len(p.Unresolved) + len(p.Coverage) + len(p.Waivers)
	if err := boundCount("capsule_page items", items, MaxCapsuleItemsPerPage); err != nil {
		return err
	}
	return nil
}
