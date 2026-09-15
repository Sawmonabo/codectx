package model

import (
	"strings"
	"time"
)

// Phase is the context-compilation phase. Only sweep and verify can open a new
// session; consolidate is compiled by the workflow service for an existing
// session using its current observation and scope digest (Section 15.1).
type Phase string

const (
	PhaseSweep       Phase = "sweep"
	PhaseVerify      Phase = "verify"
	PhaseConsolidate Phase = "consolidate"
)

// Valid reports whether p is a known wire spelling.
func (p Phase) Valid() bool {
	switch p {
	case PhaseSweep, PhaseVerify, PhaseConsolidate:
		return true
	}
	return false
}

// CanOpenSession reports whether a phase may open a new read session.
func (p Phase) CanOpenSession() bool {
	return p == PhaseSweep || p == PhaseVerify
}

// Requirement is how much of an entry the actor must read. A required_full
// entry cannot be demoted to satisfy a budget (Section 15.4).
type Requirement string

const (
	RequirementFull        Requirement = "required_full"
	RequirementSymbol      Requirement = "required_symbol"
	RequirementRecommended Requirement = "recommended"
	RequirementOptional    Requirement = "optional"
)

// Valid reports whether r is a known wire spelling.
func (r Requirement) Valid() bool {
	switch r {
	case RequirementFull, RequirementSymbol, RequirementRecommended, RequirementOptional:
		return true
	}
	return false
}

// EstimateMethodUTF8Bytes is the exported label of the default token estimate.
// It is persisted in every context manifest, so changing it invalidates stored
// manifests rather than silently reinterpreting them.
const EstimateMethodUTF8Bytes = "utf8_bytes_div_3_heuristic"

// EstimateTokensUTF8Bytes is the default token estimate of Section 15.4. It is
// a heuristic, not a conservative guarantee for every tokenizer; exact byte
// limits are enforced independently.
func EstimateTokensUTF8Bytes(n int64) (int64, error) {
	if err := requireNonNegative("source byte count", n); err != nil {
		return 0, err
	}
	// Equivalent to ceil(n/3), without n+2 overflow or loading file bytes.
	tokens := n / 3
	if n%3 != 0 {
		tokens++
	}
	return tokens, nil
}

// Budget bounds one compiled plan. MaxBytes and MaxEstimatedTokens apply per
// slice, MaxFiles applies to distinct selected files across the whole plan and
// MaxSlices caps total slices (Section 15.4).
type Budget struct {
	MaxEstimatedTokens int64 `json:"max_estimated_tokens"`
	MaxBytes           int64 `json:"max_bytes"`
	MaxFiles           int   `json:"max_files"`
	MaxSlices          int   `json:"max_slices"`
	// MaxManifestBytes is the stored-manifest byte budget for THIS request. It
	// is a caller budget, not a deployment ceiling: a request that knows it can
	// hold a larger manifest may raise the configured value, and zero takes the
	// configured one. Refusing to compile because a deployment default was
	// smaller than the caller's real window is a scale refusal.
	//
	// BudgetUnlimited (-1) is the caller's explicit "no manifest ceiling". It
	// is spelled apart from zero because zero already means "take the
	// configured value", so a caller under a finite deployment default has no
	// other way to raise the budget all the way -- and a caller budget a
	// request cannot raise to unlimited is still a scale refusal.
	MaxManifestBytes int64 `json:"max_manifest_bytes,omitempty"`
}

// Validate rejects a negative budget field. Zero is accepted and means "use the
// configured budget", per the zero-value convention documented on PageRequest;
// Section 20.2 is explicit that zero never means unlimited, and the configured
// default a zero resolves to is itself finite.
func (b Budget) Validate() error {
	for _, f := range []struct {
		field string
		value int64
	}{
		{"budget.max_estimated_tokens", b.MaxEstimatedTokens},
		{"budget.max_bytes", b.MaxBytes},
		{"budget.max_files", int64(b.MaxFiles)},
		{"budget.max_slices", int64(b.MaxSlices)},
	} {
		if err := requireNonNegative(f.field, f.value); err != nil {
			return err
		}
	}
	// max_manifest_bytes is the one budget field that admits BudgetUnlimited;
	// anything below it is still a negative bound.
	if b.MaxManifestBytes < BudgetUnlimited {
		return requireNonNegative("budget.max_manifest_bytes", b.MaxManifestBytes)
	}
	return nil
}

// BudgetUnlimited is the request-side spelling of "no ceiling" for a budget
// field whose zero already means "take the configured value".
const BudgetUnlimited int64 = -1

// ContextRequest is the semantic input to compilation. Its normalized form is
// part of manifest identity, so it carries no operational field.
type ContextRequest struct {
	Task         string       `json:"task"`
	Seeds        []string     `json:"seeds,omitempty"`
	Phase        Phase        `json:"phase"`
	Budget       Budget       `json:"budget"`
	GenerationID GenerationID `json:"generation_id,omitempty"`
}

// Validate enforces the request shape and bounded seeds.
func (r ContextRequest) Validate() error {
	if err := requireTrimmed("context.task", r.Task, MaxTaskBytes); err != nil {
		return err
	}
	if err := boundStrings("context.seeds", r.Seeds, MaxSeeds, MaxPathBytes); err != nil {
		return err
	}
	if !r.Phase.Valid() {
		return invalid("context.phase %q is not a known phase", truncateForMessage(string(r.Phase)))
	}
	if err := r.Budget.Validate(); err != nil {
		return err
	}
	if err := requireNonNegative("context.generation_id", int64(r.GenerationID)); err != nil {
		return err
	}
	return nil
}

// PlanRequest compiles a manifest and opens an actor-specific session. A
// session is reused only for an explicit idempotency key bound to the same
// actor and request; identical tasks from different actors always get separate
// sessions (Section 15.1).
type PlanRequest struct {
	Context        ContextRequest `json:"context"`
	ActorID        string         `json:"actor_id"`
	IdempotencyKey string         `json:"idempotency_key,omitempty"`

	// Cursor continues a compile that ended on its query deadline at a pass
	// boundary (ruling C9). It is the token the previous PlanResult printed as
	// NextCursor, and it is presented beside the SAME request: the token is
	// bound to the compile's request identity, so a cursor offered with any
	// other request is refused rather than answered.
	Cursor string `json:"cursor,omitempty"`
}

// Validate enforces the plan shape, including the Section 17.1 rule that direct
// consolidation planning is not allowed.
func (r PlanRequest) Validate() error {
	if err := r.Context.Validate(); err != nil {
		return err
	}
	if !r.Context.Phase.CanOpenSession() {
		return invalid("context.phase %q cannot open a session; consolidate is compiled by the workflow service for an existing session",
			r.Context.Phase)
	}
	if err := requireTrimmed("plan.actor_id", r.ActorID, MaxIdentifierBytes); err != nil {
		return err
	}
	if r.IdempotencyKey != "" {
		if err := requireTrimmed("plan.idempotency_key", r.IdempotencyKey, MaxIdentifierBytes); err != nil {
			return err
		}
	}
	// A wire-class bound, not a limit: the token is signed and self-describing,
	// so anything past MaxTokenBytes is a forged or corrupted argument rather
	// than a large legitimate request. It is the same bound page.cursor carries.
	if err := boundField("plan.cursor", r.Cursor, MaxTokenBytes); err != nil {
		return err
	}
	return nil
}

// ContextEntry is one ranked selection. At least one of NodeID and FileID is
// present — the context_entries CHECK is an OR, not an XOR, because a whole
// file is selected without a node and a symbol is selected within its file.
//
// EstimatedBytes is a SHARE of the plan's transport cost, not a self-contained
// size for this entry: it always counts the entry's own measured metadata, but
// the wire-encoded source of the file is counted on the file's first entry
// only, because the file is transported once however many entries select it.
// Summing the entries of a file (or of a slice) therefore gives that file's or
// slice's real cost; reading one sibling entry's value as "the bytes needed to
// serve this entry" does not.
type ContextEntry struct {
	Ordinal         int            `json:"ordinal"`
	NodeID          NodeID         `json:"node_id,omitempty"`
	FileID          FileID         `json:"file_id,omitempty"`
	Requirement     Requirement    `json:"requirement"`
	ScoreMicros     int64          `json:"score_micros"`
	EstimatedBytes  int64          `json:"estimated_bytes"`
	EstimatedTokens int64          `json:"estimated_tokens"`
	Reasons         []string       `json:"reasons"`
	EvidencePaths   [][]RelationID `json:"evidence_paths,omitempty"`
}

// Validate enforces the context_entries constraints and the bounded explanation
// caps of Section 15.3.
func (e ContextEntry) Validate() error {
	if err := requireNonNegative("context_entry.ordinal", int64(e.Ordinal)); err != nil {
		return err
	}
	if e.NodeID == "" && e.FileID == "" {
		return invalid("context_entry %d names neither a node_id nor a file_id", e.Ordinal)
	}
	if err := optionalID("context_entry.node_id", string(e.NodeID)); err != nil {
		return err
	}
	if err := optionalID("context_entry.file_id", string(e.FileID)); err != nil {
		return err
	}
	if !e.Requirement.Valid() {
		return invalid("context_entry.requirement %q is not a known requirement", truncateForMessage(string(e.Requirement)))
	}
	if err := requireNonNegative("context_entry.estimated_bytes", e.EstimatedBytes); err != nil {
		return err
	}
	if err := requireNonNegative("context_entry.estimated_tokens", e.EstimatedTokens); err != nil {
		return err
	}
	if err := boundStrings("context_entry.reasons", e.Reasons, MaxReasonsPerEntry, MaxReasonBytes); err != nil {
		return err
	}
	// No count bound on evidence_paths. MaxReasonPathsPerEntry is superseded by
	// the configured context.max_reason_paths_per_entry limit, which the ranking
	// lane honours as written -- including unlimited. Refusing an entry here for
	// carrying more routes than the old model constant would fail a persist for
	// obeying the operator's own setting, and what an entry cannot enumerate is
	// already disclosed by its MorePaths reason rather than dropped.
	for i, path := range e.EvidencePaths {
		field := indexed("context_entry.evidence_paths", i)
		if err := boundCount(field, len(path), MaxRelationsPerPath); err != nil {
			return err
		}
		for j, id := range path {
			if err := requireID(indexed(field, j), string(id)); err != nil {
				return err
			}
		}
	}
	return nil
}

// ContextSlice is one deliverable unit of a plan. It references entry ordinals
// rather than duplicating entry structs, so a large manifest is never held in
// memory twice (Section 15.1).
type ContextSlice struct {
	Index           int   `json:"index"`
	EntryOrdinals   []int `json:"entry_ordinals"`
	EstimatedBytes  int64 `json:"estimated_bytes"`
	EstimatedTokens int64 `json:"estimated_tokens"`
}

// Validate enforces the context_slices constraints.
func (s ContextSlice) Validate() error {
	if err := requireNonNegative("context_slice.index", int64(s.Index)); err != nil {
		return err
	}
	if len(s.EntryOrdinals) == 0 {
		return invalid("context_slice %d has no entries", s.Index)
	}
	// A page width, not a ceiling on a slice: a slice wider than one page is
	// served as a page plus a cursor, so this reports a producer that returned
	// an oversized page rather than refusing a legitimate plan.
	if err := boundPage("context_slice.entry_ordinals", len(s.EntryOrdinals)); err != nil {
		return err
	}
	for i, o := range s.EntryOrdinals {
		if err := requireNonNegative(indexed("context_slice.entry_ordinals", i), int64(o)); err != nil {
			return err
		}
	}
	if err := requireNonNegative("context_slice.estimated_bytes", s.EstimatedBytes); err != nil {
		return err
	}
	if err := requireNonNegative("context_slice.estimated_tokens", s.EstimatedTokens); err != nil {
		return err
	}
	return nil
}

// ContextReference names an entity that was considered but excluded. It is the
// typed shape of the excluded_context_entries reference_json column; Section
// 19.3 forbids an untyped payload there.
type ContextReference struct {
	NodeID NodeID `json:"node_id,omitempty"`
	FileID FileID `json:"file_id,omitempty"`
	Path   string `json:"path,omitempty"`
}

// Validate enforces that a reference names something. A path alone is enough:
// an unresolved task token recorded as an exclusion (Section 15.2) has no
// node or file behind it, and the exclusion exists precisely to keep that
// omission visible.
func (r ContextReference) Validate() error {
	if r.NodeID == "" && r.FileID == "" && r.Path == "" {
		return invalid("context_reference names neither a node_id, a file_id nor a path")
	}
	if err := optionalID("context_reference.node_id", string(r.NodeID)); err != nil {
		return err
	}
	if err := optionalID("context_reference.file_id", string(r.FileID)); err != nil {
		return err
	}
	if err := boundField("context_reference.path", r.Path, MaxPathBytes); err != nil {
		return err
	}
	return nil
}

// ExcludedContextEntry records one candidate the compiler did not select, with
// the reason. Exclusions are retained so an omission is visible rather than
// silent (Section 15.4).
type ExcludedContextEntry struct {
	Ordinal   int              `json:"ordinal"`
	Reference ContextReference `json:"reference"`
	Reason    string           `json:"reason"`
}

// Validate enforces the excluded_context_entries constraints.
func (e ExcludedContextEntry) Validate() error {
	if err := requireNonNegative("excluded_context_entry.ordinal", int64(e.Ordinal)); err != nil {
		return err
	}
	if err := e.Reference.Validate(); err != nil {
		return err
	}
	if err := requireTrimmed("excluded_context_entry.reason", e.Reason, MaxReasonBytes); err != nil {
		return err
	}
	return nil
}

// ContextManifest is the immutable header of a compiled plan. Entries, slices
// and exclusions are read by bounded pages, never embedded.
type ContextManifest struct {
	ID             ManifestID        `json:"id"`
	Binding        Binding           `json:"binding"`
	Phase          Phase             `json:"phase"`
	RequestHash    string            `json:"request_hash"`
	PolicyVersion  string            `json:"policy_version"`
	CanonicalHash  string            `json:"canonical_hash"`
	Budget         Budget            `json:"budget"`
	EntryCount     int               `json:"entry_count"`
	SliceCount     int               `json:"slice_count"`
	Completeness   []CapabilityState `json:"completeness"`
	ScopeComplete  bool              `json:"scope_complete"`
	EstimateMethod string            `json:"estimate_method"`
	CreatedAt      time.Time         `json:"created_at"`
	// Notices are the compile's non-fatal disclosures, the same contract
	// QueryMeta.Notices carries on the query surfaces: a request that asked for
	// a bound the configuration resolves differently reports "requested N,
	// effective M" here rather than being silently clamped, and a field the
	// compile had to truncate names the count it truncated here rather than
	// shortening an explanation without saying so.
	//
	// They are NOT part of CanonicalHash and are not persisted: the hash
	// identifies the PLAN, and two compiles of one generation that differ only
	// in the bounds the operator configured are the same plan. A manifest
	// re-read from storage therefore carries no notices, and Compile re-emits
	// the configuration-derived ones on the reuse path so the caller that
	// asked is still told.
	Notices []string `json:"notices,omitempty"`
}

// Validate enforces the context_manifests constraints.
func (m ContextManifest) Validate() error {
	if err := requireID("manifest.id", string(m.ID)); err != nil {
		return err
	}
	if err := m.Binding.Validate(); err != nil {
		return err
	}
	if !m.Phase.Valid() {
		return invalid("manifest.phase %q is not a known phase", truncateForMessage(string(m.Phase)))
	}
	if err := requireID("manifest.request_hash", m.RequestHash); err != nil {
		return err
	}
	if err := requireID("manifest.canonical_hash", m.CanonicalHash); err != nil {
		return err
	}
	if err := requireField("manifest.policy_version", m.PolicyVersion, MaxIdentifierBytes); err != nil {
		return err
	}
	if err := m.Budget.Validate(); err != nil {
		return err
	}
	if err := requireNonNegative("manifest.entry_count", int64(m.EntryCount)); err != nil {
		return err
	}
	if err := requireNonNegative("manifest.slice_count", int64(m.SliceCount)); err != nil {
		return err
	}
	if err := validateCapabilityStates("manifest.completeness", m.Completeness); err != nil {
		return err
	}
	if err := requireField("manifest.estimate_method", m.EstimateMethod, MaxIdentifierBytes); err != nil {
		return err
	}
	// Bounded per notice, not in total -- the QueryMeta.Notices rule: the count
	// is a function of how many bounds the request met, and an aggregate cap
	// would be the silent disclosure drop these notices exist to prevent. The
	// producers pass every notice through TruncateField, so this bound reports
	// a producer defect rather than refusing a legitimate compile.
	for i, note := range m.Notices {
		if strings.TrimSpace(note) == "" {
			return invalid("manifest.notices[%d] is empty", i)
		}
		if err := boundField("manifest.notices", note, MaxReasonBytes); err != nil {
			return err
		}
	}
	return nil
}

// PlanResult is the answer to a plan request. It is ONE type with two shapes
// (ruling C9): either the finished plan -- the immutable manifest plus the
// operational session binding the actor uses from here on -- or the report that
// this call ended on its query deadline at a pass boundary, carrying the token
// the next call resumes from and nothing else.
//
// It is widened rather than split because a continuation is the same answer to
// the same question, not a different endpoint: a client decodes one type and
// branches on Truncated. A truncated result carries the ZERO manifest and no
// session id, because a partial plan is never persisted and no session may bind
// to one -- which is exactly what the exactly-one-of rule in Validate enforces.
type PlanResult struct {
	Manifest  ContextManifest `json:"manifest"`
	SessionID SessionID       `json:"session_id"`
	ActorID   string          `json:"actor_id"`

	// Truncated says this call stopped at a pass boundary and compiled no
	// manifest. TruncationReason names why ("deadline" is the only reason a
	// compile ends this way) and NextCursor is the token that continues it.
	Truncated        bool   `json:"truncated,omitempty"`
	TruncationReason string `json:"truncation_reason,omitempty"`
	NextCursor       string `json:"next_cursor,omitempty"`
}

// Validate enforces the result shape: exactly one of (Manifest + SessionID) or
// (NextCursor + Truncated). A result carrying both would let a reader treat a
// continuation as an answer; a result carrying neither is an empty answer no
// caller can act on. ActorID is required on both shapes -- it is who asked, and
// a continuation is resumed by the same actor.
func (r PlanResult) Validate() error {
	if err := requireTrimmed("plan_result.actor_id", r.ActorID, MaxIdentifierBytes); err != nil {
		return err
	}
	continuation := r.Truncated || r.NextCursor != "" || r.TruncationReason != ""
	if continuation {
		if r.SessionID != "" || r.Manifest.ID != "" {
			return invalid("plan_result names both a compiled plan and a continuation cursor; a truncated compile persists no manifest and opens no session")
		}
		if !r.Truncated {
			return invalid("plan_result carries a continuation cursor without truncated")
		}
		if err := requireField("plan_result.truncation_reason", r.TruncationReason, MaxIdentifierBytes); err != nil {
			return err
		}
		if err := requireField("plan_result.next_cursor", r.NextCursor, MaxTokenBytes); err != nil {
			return err
		}
		return nil
	}
	if r.Manifest.ID == "" && r.SessionID == "" {
		return invalid("plan_result names neither a compiled plan nor a continuation cursor")
	}
	if err := r.Manifest.Validate(); err != nil {
		return err
	}
	if err := requireID("plan_result.session_id", string(r.SessionID)); err != nil {
		return err
	}
	return nil
}

// ContextView selects which projection of a manifest to page through, using the
// exact `--view` spellings of Section 18.1.
type ContextView string

const (
	ViewEntries  ContextView = "entries"
	ViewSlices   ContextView = "slices"
	ViewExcluded ContextView = "excluded"
)

// Valid reports whether v is a known wire spelling.
func (v ContextView) Valid() bool {
	switch v {
	case ViewEntries, ViewSlices, ViewExcluded:
		return true
	}
	return false
}

// ContextPageRequest pages one projection of a session's current manifest.
// Every session operation requires the actor, which Section 16.1 makes a
// mandatory part of the request rather than ambient state.
type ContextPageRequest struct {
	SessionID SessionID   `json:"session_id"`
	ActorID   string      `json:"actor_id"`
	View      ContextView `json:"view"`
	Page      PageRequest `json:"page"`
}

// Validate enforces the request shape. There is deliberately no cursor/generation
// conflict check here as there is on every generation-pinned query request: a
// context page names no generation at all, because the session already pins one
// and the caller cannot repin it.
func (r ContextPageRequest) Validate() error {
	if err := requireID("context_page.session_id", string(r.SessionID)); err != nil {
		return err
	}
	if err := requireTrimmed("context_page.actor_id", r.ActorID, MaxIdentifierBytes); err != nil {
		return err
	}
	if !r.View.Valid() {
		return invalid("context_page.view %q is not a known view", truncateForMessage(string(r.View)))
	}
	if err := r.Page.Validate(); err != nil {
		return err
	}
	return nil
}

// IncludeRequest adds discovered scope to a pinned session. ExpectedVersion is
// the compare-and-swap guard of Section 17.1: two clients cannot both extend
// scope from the same version.
type IncludeRequest struct {
	SessionID       SessionID `json:"session_id"`
	ActorID         string    `json:"actor_id"`
	Seeds           []string  `json:"seeds"`
	ExpectedVersion int       `json:"expected_version"`
}

// Validate enforces the request shape.
func (r IncludeRequest) Validate() error {
	if err := requireID("include.session_id", string(r.SessionID)); err != nil {
		return err
	}
	if err := requireTrimmed("include.actor_id", r.ActorID, MaxIdentifierBytes); err != nil {
		return err
	}
	if len(r.Seeds) == 0 {
		return invalid("include.seeds is required")
	}
	if err := boundStrings("include.seeds", r.Seeds, MaxSeeds, MaxPathBytes); err != nil {
		return err
	}
	if r.ExpectedVersion < 1 {
		return invalid("include.expected_version is %d; versions start at 1", r.ExpectedVersion)
	}
	return nil
}

// NextContextItem is the metadata-only answer to `context next`. Section 19.2
// marks the tool **metadata only**, so this record names the next required file
// and byte offset to read but never carries source bytes. Action distinguishes
// "read this range" from "no read is outstanding; take this manifest action".
//
// Action is deliberately a bounded free-form string, not a closed enum: Section
// 19.2 names the two categories but fixes no manifest-action vocabulary, and
// inventing one here would bind Tasks 16 and 17 to spellings the spec never
// chose. Close it in this file once those tasks fix the set.
type NextContextItem struct {
	Binding     Binding     `json:"binding"`
	Action      string      `json:"action"`
	FileID      FileID      `json:"file_id,omitempty"`
	Path        string      `json:"path,omitempty"`
	ContentHash string      `json:"content_hash,omitempty"`
	Requirement Requirement `json:"requirement,omitempty"`
	Offset      uint64      `json:"offset"`
	Size        int64       `json:"size"`
	Remaining   int64       `json:"remaining_files"`
}

// Validate enforces the item shape.
func (i NextContextItem) Validate() error {
	if err := i.Binding.Validate(); err != nil {
		return err
	}
	if err := requireField("next_context_item.action", i.Action, MaxIdentifierBytes); err != nil {
		return err
	}
	if err := optionalID("next_context_item.file_id", string(i.FileID)); err != nil {
		return err
	}
	if err := boundField("next_context_item.path", i.Path, MaxPathBytes); err != nil {
		return err
	}
	if err := optionalID("next_context_item.content_hash", i.ContentHash); err != nil {
		return err
	}
	if i.Requirement != "" && !i.Requirement.Valid() {
		return invalid("next_context_item.requirement %q is not a known requirement", truncateForMessage(string(i.Requirement)))
	}
	if err := boundSigned64("next_context_item.offset", i.Offset); err != nil {
		return err
	}
	if err := requireNonNegative("next_context_item.size", i.Size); err != nil {
		return err
	}
	if err := requireNonNegative("next_context_item.remaining_files", i.Remaining); err != nil {
		return err
	}
	return nil
}

// ContextPage is one bounded page of a session's current manifest, the result
// type ContextPageRequest has lacked. It mirrors CapsulePage: the header
// travels with every page and the three projections are paged separately, so a
// cursor is never ambiguous about which list it advances through.
type ContextPage struct {
	Meta       QueryMeta              `json:"meta"`
	SessionID  SessionID              `json:"session_id"`
	ManifestID ManifestID             `json:"manifest_id"`
	View       ContextView            `json:"view"`
	Entries    []ContextEntry         `json:"entries,omitempty"`
	Slices     []ContextSlice         `json:"slices,omitempty"`
	Excluded   []ExcludedContextEntry `json:"excluded,omitempty"`
}

// Validate enforces the page shape and its per-page item bound. A page carries
// records for exactly the view it declares: a second populated list would let a
// reader page past records it never saw.
func (p ContextPage) Validate() error {
	if err := p.Meta.Validate(); err != nil {
		return err
	}
	if err := requireID("context_page.session_id", string(p.SessionID)); err != nil {
		return err
	}
	if err := requireID("context_page.manifest_id", string(p.ManifestID)); err != nil {
		return err
	}
	if !p.View.Valid() {
		return invalid("context_page.view %q is not a known view", truncateForMessage(string(p.View)))
	}
	lists := []struct {
		view  ContextView
		count int
	}{
		{ViewEntries, len(p.Entries)},
		{ViewSlices, len(p.Slices)},
		{ViewExcluded, len(p.Excluded)},
	}
	items := 0
	for _, l := range lists {
		if l.count > 0 && l.view != p.View {
			return invalid("context_page declares view %q but also carries %d %s records",
				p.View, l.count, l.view)
		}
		items += l.count
	}
	if err := boundCount("context_page items", items, MaxPageItems); err != nil {
		return err
	}
	for _, e := range p.Entries {
		if err := e.Validate(); err != nil {
			return err
		}
	}
	for _, s := range p.Slices {
		if err := s.Validate(); err != nil {
			return err
		}
	}
	for _, e := range p.Excluded {
		if err := e.Validate(); err != nil {
			return err
		}
	}
	return nil
}
