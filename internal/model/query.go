package model

import (
	"context"
	"time"
)

// PageRequest is the shared pagination input. A supplied cursor already selects
// its pinned generation, so a request that also names a generation or changes a
// filter is rejected rather than silently repinned (Sections 14.1, 14.4).
//
// Zero-value convention for every request bound in this package — Limit here,
// ReadChunkRequest.MaxBytes, GraphRequest.MaxDepth/MaxVisited/MaxEdges and the
// Budget fields: zero means "use the configured or endpoint default", a negative
// value is invalid, and a positive value is taken as given. Section 20.2 is
// explicit that no zero or negative setting ever means unlimited; the default a
// zero resolves to is itself a finite configured bound.
type PageRequest struct {
	Limit  int    `json:"limit"`
	Cursor string `json:"cursor,omitempty"`
}

// Validate bounds the page before any work begins.
func (p PageRequest) Validate() error {
	if p.Limit < 0 || p.Limit > MaxPageItems {
		return invalid("page.limit is %d; it must be between 0 and %d, where 0 means the endpoint default", p.Limit, MaxPageItems)
	}
	if err := boundField("page.cursor", p.Cursor, MaxTokenBytes); err != nil {
		return err
	}
	return nil
}

// ValidatePinned bounds the page and enforces the Section 14.1 rule that a
// request may not name both a cursor and a generation: the cursor already pins
// one, so honouring the second would silently repin the query onto a different
// generation and return results the caller's earlier pages cannot be compared
// against. Every request embedding both a PageRequest and a GenerationID calls
// this instead of Validate.
func (p PageRequest) ValidatePinned(field string, generation GenerationID) error {
	if err := p.Validate(); err != nil {
		return err
	}
	if err := requireNonNegative(field+".generation_id", int64(generation)); err != nil {
		return err
	}
	if p.Cursor != "" && generation != 0 {
		return invalid("%s names both a cursor and a generation_id; a cursor already pins its generation", field)
	}
	return nil
}

// OverlayBinding labels a result that came from the LSP working-tree overlay
// rather than from sealed canonical facts. Section 11.6 requires query metadata
// to carry the overlay provider, version and input digest whenever LSP is used,
// and Section 19.3 requires overlay results to be distinctly labeled: without
// this a caller cannot tell an ephemeral dirty-worktree answer from a fact the
// index will still agree with tomorrow.
type OverlayBinding struct {
	ProviderID      string `json:"provider_id"`
	ProviderVersion string `json:"provider_version"`
	InputDigest     string `json:"input_digest"`
}

// Validate enforces the overlay label's shape.
func (b OverlayBinding) Validate() error {
	if err := requireField("overlay.provider_id", b.ProviderID, MaxIdentifierBytes); err != nil {
		return err
	}
	if err := requireField("overlay.provider_version", b.ProviderVersion, MaxIdentifierBytes); err != nil {
		return err
	}
	if err := requireID("overlay.input_digest", b.InputDigest); err != nil {
		return err
	}
	return nil
}

// validateResultSource checks the semantic source carried on a result record,
// where the zero value means canonical. It exists so Node and
// ReferenceOccurrence cannot drift apart on what an overlay record may omit.
func validateResultSource(field string, s SemanticSource) *Error {
	if s != "" && !s.Valid() {
		return invalid("%s %q is not a known semantic source", field, truncateForMessage(string(s)))
	}
	return nil
}

// QueryMeta accompanies every public result: the generation it was read from,
// per-capability completeness, whether the answer was truncated and why, and a
// continuation cursor when safe continuation exists.
type QueryMeta struct {
	Binding          Binding           `json:"binding"`
	Completeness     []CapabilityState `json:"completeness"`
	Truncated        bool              `json:"truncated"`
	TruncationReason string            `json:"truncation_reason,omitempty"`
	NextCursor       string            `json:"next_cursor,omitempty"`
	// Overlay is set only when the result came from the LSP overlay; a nil
	// overlay means every record in the result is a canonical fact.
	Overlay *OverlayBinding `json:"overlay,omitempty"`
	// Notices are the answer's non-fatal disclosures: a request that asked for
	// a bound larger than the configuration allows reports "requested N,
	// effective M" here rather than being silently clamped, and a surface that
	// skipped or cut something for a user-set reason names it here.
	//
	// A notice is not a truncation: Truncated says the ANSWER is short, a
	// notice says the answer was produced under a bound the caller did not
	// choose. Both can hold at once and neither implies the other.
	Notices []string `json:"notices,omitempty"`
}

// Validate enforces the result metadata contract, including the rule that a
// truncated result states why it was truncated.
func (m QueryMeta) Validate() error {
	if err := m.Binding.Validate(); err != nil {
		return err
	}
	if err := validateCapabilityStates("meta.completeness", m.Completeness); err != nil {
		return err
	}
	if m.Truncated && m.TruncationReason == "" {
		return invalid("meta.truncated is set without a truncation_reason")
	}
	if err := boundField("meta.truncation_reason", m.TruncationReason, MaxReasonBytes); err != nil {
		return err
	}
	if err := boundField("meta.next_cursor", m.NextCursor, MaxTokenBytes); err != nil {
		return err
	}
	// Notices are bounded per ROW, not in total: the count is a function of how
	// many bounds the caller asked to raise, which is small and bounded by the
	// request shape, while an aggregate cap would be exactly the report-row
	// drop this contract exists to prevent.
	for i, note := range m.Notices {
		if note == "" {
			return invalid("meta.notices[%d] is empty", i)
		}
		if err := boundField("meta.notices", note, MaxReasonBytes); err != nil {
			return err
		}
	}
	if m.Overlay != nil {
		if err := m.Overlay.Validate(); err != nil {
			return err
		}
	}
	return nil
}

// Page is the one paginated result shape. Every endpoint that returns a
// homogeneous list returns this rather than a per-endpoint {meta, items} twin.
type Page[T any] struct {
	Meta  QueryMeta `json:"meta"`
	Items []T       `json:"items"`
}

// Validate checks the page envelope. It bounds Items at MaxPageItems so a
// response can never exceed the page size its request was capped to; the item
// type is opaque here, so each endpoint still validates its own elements.
func (p Page[T]) Validate() error {
	if err := p.Meta.Validate(); err != nil {
		return err
	}
	if err := boundCount("page.items", len(p.Items), MaxPageItems); err != nil {
		return err
	}
	return nil
}

// Direction selects graph traversal orientation. Section 14.3 requires
// direction to be reported with every traversal, because callers and callees
// are not interchangeable.
type Direction string

const (
	DirectionOutgoing Direction = "outgoing"
	DirectionIncoming Direction = "incoming"
	DirectionBoth     Direction = "both"
)

// Valid reports whether d is a known wire spelling.
func (d Direction) Valid() bool {
	switch d {
	case DirectionOutgoing, DirectionIncoming, DirectionBoth:
		return true
	}
	return false
}

// SemanticSource routes a symbol, reference or call-hierarchy request. Canonical
// is the default; lsp returns distinctly labeled overlay results that are never
// persisted as canonical facts (Sections 11.5, 11.6).
type SemanticSource string

const (
	SemanticCanonical SemanticSource = "canonical"
	SemanticLSP       SemanticSource = "lsp"
)

// Valid reports whether s is a known wire spelling. The empty value is not
// valid; callers default explicitly to SemanticCanonical.
func (s SemanticSource) Valid() bool {
	return s == SemanticCanonical || s == SemanticLSP
}

// SymbolOperation selects the typed symbol operation, using the exact CLI
// spellings of Section 18.1.
type SymbolOperation string

const (
	SymbolResolve          SymbolOperation = "resolve"
	SymbolDocumentSymbols  SymbolOperation = "document-symbols"
	SymbolWorkspaceSymbols SymbolOperation = "workspace-symbols"
	SymbolDefinition       SymbolOperation = "definition"
)

// Valid reports whether o is a known wire spelling.
func (o SymbolOperation) Valid() bool {
	switch o {
	case SymbolResolve, SymbolDocumentSymbols, SymbolWorkspaceSymbols, SymbolDefinition:
		return true
	}
	return false
}

// ReferenceOperation selects the reference variant, using the exact CLI
// spellings of Section 18.1.
type ReferenceOperation string

const (
	ReferenceReferences     ReferenceOperation = "references"
	ReferenceImplements     ReferenceOperation = "implements"
	ReferenceTypeDefinition ReferenceOperation = "type-definition"
)

// Valid reports whether o is a known wire spelling.
func (o ReferenceOperation) Valid() bool {
	switch o {
	case ReferenceReferences, ReferenceImplements, ReferenceTypeDefinition:
		return true
	}
	return false
}

// SearchRequest is the paginated lexical, path and symbol discovery input.
// GenerationID zero selects the active generation once at request start.
type SearchRequest struct {
	GenerationID GenerationID `json:"generation_id,omitempty"`
	Query        string       `json:"query"`
	Kinds        []NodeKind   `json:"kinds,omitempty"`
	Languages    []string     `json:"languages,omitempty"`
	Paths        []string     `json:"paths,omitempty"`
	Page         PageRequest  `json:"page"`
}

// Validate enforces the Section 14.1 rule that query text, filters and limits
// are all checked before any retrieval work begins.
func (r SearchRequest) Validate() error {
	if err := requireTrimmed("search.query", r.Query, MaxQueryTextBytes); err != nil {
		return err
	}
	if err := boundCount("search.kinds", len(r.Kinds), MaxFilterValues); err != nil {
		return err
	}
	for i, k := range r.Kinds {
		if !k.Valid() {
			return invalid("%s %q is not a known node kind", indexed("search.kinds", i), truncateForMessage(string(k)))
		}
	}
	if err := boundStrings("search.languages", r.Languages, MaxFilterValues, MaxLanguageBytes); err != nil {
		return err
	}
	if err := boundStrings("search.paths", r.Paths, MaxFilterValues, MaxPathBytes); err != nil {
		return err
	}
	if err := r.Page.ValidatePinned("search", r.GenerationID); err != nil {
		return err
	}
	return nil
}

// SearchTier is the retrieval tier that produced a hit. Section 14.2 fixes
// exactly these five tiers and makes the tier the FIRST tie-breaker in the
// deterministic ranking, ahead of score: an unrecognized spelling would not fail
// loudly, it would silently reorder results, so the vocabulary is closed.
//
// Declaration order is the Section 14.2 ranking order, most to least specific,
// which is what Rank exposes.
type SearchTier string

const (
	TierExactPath           SearchTier = "exact_path"
	TierExactQualifiedName  SearchTier = "exact_qualified_name"
	TierQualifiedNamePrefix SearchTier = "qualified_name_prefix"
	TierExactName           SearchTier = "exact_name"
	TierLexicalFTS          SearchTier = "lexical_fts"
)

// searchTiers is the ranking order of Section 14.2.
var searchTiers = [...]SearchTier{
	TierExactPath, TierExactQualifiedName, TierQualifiedNamePrefix,
	TierExactName, TierLexicalFTS,
}

// Rank returns the tie-break ordinal, lower sorting first. An unknown tier ranks
// after every known one rather than aliasing the most specific tier.
func (t SearchTier) Rank() int {
	for i, known := range searchTiers {
		if t == known {
			return i
		}
	}
	return len(searchTiers)
}

// Valid reports whether t is a known wire spelling.
func (t SearchTier) Valid() bool { return t.Rank() < len(searchTiers) }

// SearchHit is one deduplicated search result. Section 14.2 fixes its contents:
// the canonical entity or file chunk that was matched, the integer ranking
// component with its retrieval tier, bounded reasons and the occurrence count
// that deduplication folded away. It carries no source body — Section 19.3
// reserves those for codectx_read_source.
type SearchHit struct {
	NodeID          NodeID       `json:"node_id,omitempty"`
	FileID          FileID       `json:"file_id"`
	Path            string       `json:"path"`
	Kind            NodeKind     `json:"kind"`
	Name            string       `json:"name,omitempty"`
	QualifiedName   string       `json:"qualified_name,omitempty"`
	Signature       string       `json:"signature,omitempty"`
	Tier            SearchTier   `json:"tier"`
	ScoreMicros     int64        `json:"score_micros"`
	Range           *SourceRange `json:"range,omitempty"`
	OccurrenceCount int64        `json:"occurrence_count"`
	Reasons         []string     `json:"reasons,omitempty"`
	// UnresolvedFields names the fields of THIS hit that could not be
	// resolved, against the typed reason each failed with. It is the search
	// counterpart of the truncated-fields map a stored record carries: a hit
	// whose `range` is absent because the content store no longer holds the
	// blob says so, instead of presenting a missing range as "this hit has no
	// position" or costing the caller every other hit in the answer. The key
	// set is fixed by this package (SearchHitFieldRange today), so the map is
	// bounded by the code rather than by the repository.
	UnresolvedFields map[string]string `json:"unresolved_fields,omitempty"`
}

// SearchHitFieldRange is the reserved UnresolvedFields key for SearchHit.Range.
// A hit carrying it has a nil Range and the reason it stayed nil.
const SearchHitFieldRange = "range"

// MarkUnresolved records that one field of the hit could not be resolved and
// why. It is the only writer of UnresolvedFields: a caller that assigned the
// map directly would be one unbounded reason away from writing a provider's
// error text onto the wire.
func (h *SearchHit) MarkUnresolved(field, reason string) {
	if h.UnresolvedFields == nil {
		h.UnresolvedFields = make(map[string]string, 1)
	}
	h.UnresolvedFields[field] = truncateUTF8(reason, MaxDetailBytes)
}

// Validate enforces the hit's bounds; reasons are bounded because Section 14.3
// caps total explanation bytes per result.
func (h SearchHit) Validate() error {
	if err := optionalID("search_hit.node_id", string(h.NodeID)); err != nil {
		return err
	}
	if err := requireID("search_hit.file_id", string(h.FileID)); err != nil {
		return err
	}
	if err := requireField("search_hit.path", h.Path, MaxPathBytes); err != nil {
		return err
	}
	if !h.Kind.Valid() {
		return invalid("search_hit.kind %q is not a known node kind", truncateForMessage(string(h.Kind)))
	}
	if !h.Tier.Valid() {
		return invalid("search_hit.tier %q is not a known retrieval tier", truncateForMessage(string(h.Tier)))
	}
	if err := boundField("search_hit.name", h.Name, MaxNameBytes); err != nil {
		return err
	}
	if err := boundField("search_hit.qualified_name", h.QualifiedName, MaxQualifiedNameBytes); err != nil {
		return err
	}
	if err := boundField("search_hit.signature", h.Signature, MaxSignatureBytes); err != nil {
		return err
	}
	if h.Range != nil {
		if err := h.Range.Validate("search_hit.range"); err != nil {
			return err
		}
	}
	if err := requireNonNegative("search_hit.occurrence_count", h.OccurrenceCount); err != nil {
		return err
	}
	if err := boundStrings("search_hit.reasons", h.Reasons, MaxReasonsPerEntry, MaxReasonBytes); err != nil {
		return err
	}
	// The same wire bound Error.Details and CapabilityState.Details carry: the
	// map names fields of one hit, so it is small by construction and this
	// rejects a producer defect rather than limiting any work.
	if err := boundCount("search_hit.unresolved_fields", len(h.UnresolvedFields), MaxErrorDetails); err != nil {
		return err
	}
	for k, v := range h.UnresolvedFields {
		if err := requireField("search_hit.unresolved_fields key", k, MaxIdentifierBytes); err != nil {
			return err
		}
		if err := boundField("search_hit.unresolved_fields["+k+"]", v, MaxDetailBytes); err != nil {
			return err
		}
	}
	return nil
}

// SymbolRequest resolves a name or canonical ID to nodes, with explicit
// ambiguity rather than a silent first-candidate choice. Operation and
// SemanticSource keep every supported LSP operation reachable through the same
// typed facade as the canonical path (Sections 11.6, 18.2).
type SymbolRequest struct {
	GenerationID   GenerationID    `json:"generation_id,omitempty"`
	Query          string          `json:"query"`
	Operation      SymbolOperation `json:"operation"`
	SemanticSource SemanticSource  `json:"semantic_source"`
	Profile        string          `json:"profile,omitempty"`
	FileID         FileID          `json:"file_id,omitempty"`
	Range          *SourceRange    `json:"range,omitempty"`
	Page           PageRequest     `json:"page"`
}

// Validate enforces the request shape, including the Section 11.6 rule that a
// live-query route names the pinned file/range or symbol input it needs.
func (r SymbolRequest) Validate() error {
	if !r.Operation.Valid() {
		return invalid("symbol.operation %q is not a known symbol operation", truncateForMessage(string(r.Operation)))
	}
	if !r.SemanticSource.Valid() {
		return invalid("symbol.semantic_source %q is not a known semantic source", truncateForMessage(string(r.SemanticSource)))
	}
	if err := boundField("symbol.profile", r.Profile, MaxIdentifierBytes); err != nil {
		return err
	}
	if err := optionalID("symbol.file_id", string(r.FileID)); err != nil {
		return err
	}
	if err := validateLocatedRange("symbol.range", r.FileID, r.Range); err != nil {
		return err
	}
	if r.Operation == SymbolDocumentSymbols {
		if err := requireID("symbol.file_id", string(r.FileID)); err != nil {
			return err
		}
	} else if err := requireTrimmed("symbol.query", r.Query, MaxQueryTextBytes); err != nil {
		return err
	}
	if err := r.Page.ValidatePinned("symbol", r.GenerationID); err != nil {
		return err
	}
	return nil
}

// ReferenceRequest asks for reference occurrences, implementations or type
// definitions of one resolved node.
type ReferenceRequest struct {
	GenerationID   GenerationID       `json:"generation_id,omitempty"`
	NodeID         NodeID             `json:"node_id"`
	Operation      ReferenceOperation `json:"operation"`
	SemanticSource SemanticSource     `json:"semantic_source"`
	Profile        string             `json:"profile,omitempty"`
	Page           PageRequest        `json:"page"`
}

// Validate enforces the request shape.
func (r ReferenceRequest) Validate() error {
	if err := requireID("reference.node_id", string(r.NodeID)); err != nil {
		return err
	}
	if !r.Operation.Valid() {
		return invalid("reference.operation %q is not a known reference operation", truncateForMessage(string(r.Operation)))
	}
	if !r.SemanticSource.Valid() {
		return invalid("reference.semantic_source %q is not a known semantic source", truncateForMessage(string(r.SemanticSource)))
	}
	if err := boundField("reference.profile", r.Profile, MaxIdentifierBytes); err != nil {
		return err
	}
	if err := r.Page.ValidatePinned("reference", r.GenerationID); err != nil {
		return err
	}
	return nil
}

// ReferenceOccurrence is one occurrence of a canonical relation, not the
// relation itself: Section 9.2 requires APIs to distinguish relation count from
// occurrence count, so the relation and the occurrence's own evidence are both
// named.
type ReferenceOccurrence struct {
	RelationID RelationID   `json:"relation_id"`
	EvidenceID EvidenceID   `json:"evidence_id"`
	Kind       RelationKind `json:"kind"`
	FromNodeID NodeID       `json:"from_node_id"`
	ToNodeID   NodeID       `json:"to_node_id"`
	Precision  Precision    `json:"precision"`
	FileID     FileID       `json:"file_id,omitempty"`
	Path       string       `json:"path,omitempty"`
	Range      *SourceRange `json:"range,omitempty"`
	// Bytes locates a canonical occurrence when Range cannot: evidence is
	// persisted as a byte interval with no line or column context, so a sealed
	// occurrence carries Bytes and a nil Range while an overlay answer, which
	// comes from a language server that speaks positions, carries Range.
	// Presenting the byte interval AS a line number is the one thing neither
	// this type nor its renderers may do.
	Bytes *ByteRange `json:"bytes,omitempty"`
	// FromName is the qualified name (or the plain name, where the provider
	// sealed no qualified one) of FromNodeID, hydrated once per page so a
	// reader can tell two occurrences apart without resolving every id itself.
	// It is empty when the node is not visible in this generation and for an
	// overlay row that sealed no canonical edge; consumers then fall back to
	// the id and never render a blank where a symbol belongs.
	FromName string `json:"from_name,omitempty"`
	// SemanticSource labels where this occurrence came from. The zero value and
	// SemanticCanonical both mean a sealed canonical fact; SemanticLSP marks an
	// ephemeral overlay answer that has no canonical relation or evidence row,
	// so its IDs are empty and its file/range carry the whole result.
	SemanticSource SemanticSource `json:"semantic_source"`
}

// Validate enforces the occurrence shape.
func (o ReferenceOccurrence) Validate() error {
	if err := validateResultSource("reference_occurrence.semantic_source", o.SemanticSource); err != nil {
		return err
	}
	overlay := o.SemanticSource == SemanticLSP
	for _, f := range []struct {
		field string
		value string
	}{
		{"reference_occurrence.relation_id", string(o.RelationID)},
		{"reference_occurrence.evidence_id", string(o.EvidenceID)},
		{"reference_occurrence.from_node_id", string(o.FromNodeID)},
		{"reference_occurrence.to_node_id", string(o.ToNodeID)},
	} {
		// An overlay occurrence is not a canonical fact: no relation was sealed
		// and no evidence row exists, so these IDs may legitimately be absent.
		// Whatever is present must still be a well-formed ID.
		check := requireID
		if overlay {
			check = optionalID
		}
		if err := check(f.field, f.value); err != nil {
			return err
		}
	}
	if overlay {
		// The pinned file and range are then the only thing that makes the
		// occurrence verifiable, so they become mandatory in their place.
		if err := requireID("reference_occurrence.file_id", string(o.FileID)); err != nil {
			return err
		}
		if o.Range == nil {
			return invalid("reference_occurrence.range is required for an lsp overlay occurrence")
		}
	}
	if !o.Kind.Valid() {
		return invalid("reference_occurrence.kind %q is not a known relation kind", truncateForMessage(string(o.Kind)))
	}
	if !o.Precision.Valid() {
		return invalid("reference_occurrence.precision %q is not a known precision class", truncateForMessage(string(o.Precision)))
	}
	if err := optionalID("reference_occurrence.file_id", string(o.FileID)); err != nil {
		return err
	}
	if err := boundField("reference_occurrence.path", o.Path, MaxPathBytes); err != nil {
		return err
	}
	if err := validateLocatedRange("reference_occurrence.range", o.FileID, o.Range); err != nil {
		return err
	}
	if err := validateLocatedBytes("reference_occurrence.bytes", o.FileID, o.Bytes); err != nil {
		return err
	}
	if err := boundField("reference_occurrence.from_name", o.FromName, MaxQualifiedNameBytes); err != nil {
		return err
	}
	return nil
}

// GraphRequest is a bounded neighborhood traversal. Every bound is explicit:
// Section 14.3 forbids an unbounded expansion and requires visited and edge
// counts to be reported back.
type GraphRequest struct {
	GenerationID GenerationID   `json:"generation_id,omitempty"`
	Start        []NodeID       `json:"start"`
	Relations    []RelationKind `json:"relations,omitempty"`
	Direction    Direction      `json:"direction"`
	MaxDepth     int            `json:"max_depth"`
	MaxVisited   int            `json:"max_visited"`
	MaxEdges     int            `json:"max_edges"`
	Page         PageRequest    `json:"page"`
}

// Validate enforces bounded seeds, depth and work budgets. A zero bound means
// "use the configured default", never "unlimited" (Section 20.1).
func (r GraphRequest) Validate() error {
	if len(r.Start) == 0 {
		return invalid("graph.start is required")
	}
	if err := boundCount("graph.start", len(r.Start), MaxStartNodes); err != nil {
		return err
	}
	for i, id := range r.Start {
		if err := requireID(indexed("graph.start", i), string(id)); err != nil {
			return err
		}
	}
	if err := boundCount("graph.relations", len(r.Relations), MaxFilterValues); err != nil {
		return err
	}
	for i, k := range r.Relations {
		if !k.Valid() {
			return invalid("%s %q is not a known relation kind", indexed("graph.relations", i), truncateForMessage(string(k)))
		}
	}
	if !r.Direction.Valid() {
		return invalid("graph.direction %q is not a known direction", truncateForMessage(string(r.Direction)))
	}
	// Zero defers to the configured traversal caps; see PageRequest.
	for _, b := range []struct {
		field string
		value int
	}{
		{"graph.max_depth", r.MaxDepth},
		{"graph.max_visited", r.MaxVisited},
		{"graph.max_edges", r.MaxEdges},
	} {
		if err := requireNonNegative(b.field, int64(b.value)); err != nil {
			return err
		}
	}
	if err := r.Page.ValidatePinned("graph", r.GenerationID); err != nil {
		return err
	}
	return nil
}

// GraphResult is a heterogeneous traversal answer, so it is not a Page: nodes
// and edges are returned together with the traversal accounting Section 14.3
// requires. Exhausting a hard budget sets Meta.Truncated with its reason rather
// than presenting the result as exhaustive.
type GraphResult struct {
	Meta         QueryMeta  `json:"meta"`
	Direction    Direction  `json:"direction"`
	Nodes        []Node     `json:"nodes"`
	Relations    []Relation `json:"relations"`
	VisitedCount int64      `json:"visited_count"`
	EdgeCount    int64      `json:"edge_count"`
	MaxDepth     int        `json:"max_depth"`
}

// Validate enforces the traversal accounting contract.
func (r GraphResult) Validate() error {
	if err := r.Meta.Validate(); err != nil {
		return err
	}
	if !r.Direction.Valid() {
		return invalid("graph_result.direction %q is not a known direction", truncateForMessage(string(r.Direction)))
	}
	if err := boundPage("graph_result.nodes", len(r.Nodes)); err != nil {
		return err
	}
	for _, n := range r.Nodes {
		if err := n.Validate(); err != nil {
			return err
		}
	}
	if err := boundPage("graph_result.relations", len(r.Relations)); err != nil {
		return err
	}
	for _, rel := range r.Relations {
		if err := rel.Validate(); err != nil {
			return err
		}
	}
	if err := requireNonNegative("graph_result.visited_count", r.VisitedCount); err != nil {
		return err
	}
	if err := requireNonNegative("graph_result.edge_count", r.EdgeCount); err != nil {
		return err
	}
	if err := requireNonNegative("graph_result.max_depth", int64(r.MaxDepth)); err != nil {
		return err
	}
	return nil
}

// PathRequest asks for a bounded shortest dependency path between two resolved
// nodes.
type PathRequest struct {
	GenerationID GenerationID   `json:"generation_id,omitempty"`
	From         NodeID         `json:"from"`
	To           NodeID         `json:"to"`
	Relations    []RelationKind `json:"relations,omitempty"`
	// Direction is the orientation the search follows edges in. The empty
	// value means DirectionOutgoing, which is what every caller got before
	// this field existed: "what does From depend on, on the way to To".
	//
	// It exists because an outgoing-only search answers "no path" for a pair
	// that IS connected against the edge direction -- a hub reached only by
	// its callers is the ordinary case -- and "no path exists" is the one
	// answer a path search may not get wrong. DirectionIncoming follows edges
	// backwards and DirectionBoth follows them either way, which makes the
	// result the undirected cheapest route.
	Direction  Direction `json:"direction,omitempty"`
	MaxDepth   int       `json:"max_depth"`
	MaxVisited int       `json:"max_visited"`
	// Page carries the continuation of a path search whose earlier page spent
	// its work budget or its deadline before the walk reached the target. The
	// external-memory walk persists its own state, so a resumed page carries
	// on settling cost buckets rather than restarting; Limit is unused here
	// (a path answer is one route list, not a keyset page) and is validated
	// only so a caller that sets it is told so rather than ignored.
	Page PageRequest `json:"page"`
}

// Validate enforces the request shape.
func (r PathRequest) Validate() error {
	if err := requireID("path.from", string(r.From)); err != nil {
		return err
	}
	if err := requireID("path.to", string(r.To)); err != nil {
		return err
	}
	if err := boundCount("path.relations", len(r.Relations), MaxFilterValues); err != nil {
		return err
	}
	for i, k := range r.Relations {
		if !k.Valid() {
			return invalid("%s %q is not a known relation kind", indexed("path.relations", i), truncateForMessage(string(k)))
		}
	}
	// The empty direction is the outgoing default, so it is accepted here and
	// normalized by the engine; any other unknown spelling is refused rather
	// than silently walked in a direction the caller did not ask for.
	if r.Direction != "" && !r.Direction.Valid() {
		return invalid("path.direction %q is not a known direction", truncateForMessage(string(r.Direction)))
	}
	if err := requireNonNegative("path.max_depth", int64(r.MaxDepth)); err != nil {
		return err
	}
	if err := requireNonNegative("path.max_visited", int64(r.MaxVisited)); err != nil {
		return err
	}
	// The same rule GraphRequest keeps: a cursor already pins its generation,
	// so a request that names both is rejected rather than silently repinned.
	if err := r.Page.ValidatePinned("path", r.GenerationID); err != nil {
		return err
	}
	return nil
}

// RelationPath is one evidence-backed route between two nodes. Edges are
// bounded because Section 14.3 caps reason paths per result.
type RelationPath struct {
	Relations []RelationID `json:"relations"`
	Evidence  []EvidenceID `json:"evidence,omitempty"`
	CostUnits int64        `json:"cost_units"`
}

// Validate enforces the path bound and its nonnegative deterministic integer
// cost.
func (p RelationPath) Validate() error {
	if len(p.Relations) == 0 {
		return invalid("relation_path.relations is required")
	}
	if err := boundCount("relation_path.relations", len(p.Relations), MaxRelationsPerPath); err != nil {
		return err
	}
	for i, id := range p.Relations {
		if err := requireID(indexed("relation_path.relations", i), string(id)); err != nil {
			return err
		}
	}
	if err := boundCount("relation_path.evidence", len(p.Evidence), MaxRelationsPerPath); err != nil {
		return err
	}
	for i, id := range p.Evidence {
		if err := requireID(indexed("relation_path.evidence", i), string(id)); err != nil {
			return err
		}
	}
	if err := requireNonNegative("relation_path.cost_units", p.CostUnits); err != nil {
		return err
	}
	return nil
}

// PathResult is heterogeneous: the nodes on the route are returned alongside
// the bounded routes themselves, and an exhausted traversal budget is reported
// as truncation rather than "no path exists".
type PathResult struct {
	Meta         QueryMeta      `json:"meta"`
	Paths        []RelationPath `json:"paths"`
	Nodes        []Node         `json:"nodes"`
	VisitedCount int64          `json:"visited_count"`
}

// Validate enforces the result shape.
func (r PathResult) Validate() error {
	if err := r.Meta.Validate(); err != nil {
		return err
	}
	if err := boundCount("path_result.paths", len(r.Paths), MaxReasonPathsPerEntry); err != nil {
		return err
	}
	for _, p := range r.Paths {
		if err := p.Validate(); err != nil {
			return err
		}
	}
	if err := boundPage("path_result.nodes", len(r.Nodes)); err != nil {
		return err
	}
	for _, n := range r.Nodes {
		if err := n.Validate(); err != nil {
			return err
		}
	}
	if err := requireNonNegative("path_result.visited_count", r.VisitedCount); err != nil {
		return err
	}
	return nil
}

// ImpactEntry is one affected entity. Section 14.3 requires every affected
// entry to carry an evidence-backed reason and the direction that made it
// affected, so a caller impacted by a callee change is distinguishable from a
// downstream dependency that merely needs reading.
type ImpactEntry struct {
	NodeID      NodeID         `json:"node_id"`
	FileID      FileID         `json:"file_id,omitempty"`
	Name        string         `json:"name,omitempty"`
	Kind        NodeKind       `json:"kind"`
	Direction   Direction      `json:"direction"`
	Depth       int            `json:"depth"`
	ScoreMicros int64          `json:"score_micros"`
	Reasons     []string       `json:"reasons"`
	Paths       []RelationPath `json:"paths,omitempty"`
}

// Validate enforces the entry shape and its bounded explanation.
func (e ImpactEntry) Validate() error {
	if err := requireID("impact_entry.node_id", string(e.NodeID)); err != nil {
		return err
	}
	if err := optionalID("impact_entry.file_id", string(e.FileID)); err != nil {
		return err
	}
	if err := boundField("impact_entry.name", e.Name, MaxQualifiedNameBytes); err != nil {
		return err
	}
	if !e.Kind.Valid() {
		return invalid("impact_entry.kind %q is not a known node kind", truncateForMessage(string(e.Kind)))
	}
	if !e.Direction.Valid() {
		return invalid("impact_entry.direction %q is not a known direction", truncateForMessage(string(e.Direction)))
	}
	if err := requireNonNegative("impact_entry.depth", int64(e.Depth)); err != nil {
		return err
	}
	if len(e.Reasons) == 0 {
		return invalid("impact_entry %q has no reason; every affected entry is evidence backed", e.NodeID)
	}
	if err := boundStrings("impact_entry.reasons", e.Reasons, MaxReasonsPerEntry, MaxReasonBytes); err != nil {
		return err
	}
	if err := boundCount("impact_entry.paths", len(e.Paths), MaxReasonPathsPerEntry); err != nil {
		return err
	}
	for _, p := range e.Paths {
		if err := p.Validate(); err != nil {
			return err
		}
	}
	return nil
}

// OverviewRequest asks for the bounded repository, package, module and language
// map backing `repo-map` and `codectx_repo_overview`.
type OverviewRequest struct {
	GenerationID GenerationID `json:"generation_id,omitempty"`
	Depth        int          `json:"depth"`
	Page         PageRequest  `json:"page"`
}

// Validate enforces the request shape.
func (r OverviewRequest) Validate() error {
	if err := requireNonNegative("overview.depth", int64(r.Depth)); err != nil {
		return err
	}
	if err := r.Page.ValidatePinned("overview", r.GenerationID); err != nil {
		return err
	}
	return nil
}

// OverviewItem is one entry of the repository map. The spec does not enumerate
// its fields; this is the minimal set Section 19.2's "bounded
// repository/package/module/language map" implies, aggregated per canonical
// container node so no whole-repository file list is materialized.
type OverviewItem struct {
	NodeID       NodeID   `json:"node_id"`
	Kind         NodeKind `json:"kind"`
	Path         string   `json:"path"`
	Name         string   `json:"name"`
	Language     string   `json:"language,omitempty"`
	Depth        int      `json:"depth"`
	FileCount    int64    `json:"file_count"`
	SymbolCount  int64    `json:"symbol_count"`
	SourceBytes  int64    `json:"source_bytes"`
	ParentNodeID NodeID   `json:"parent_node_id,omitempty"`
}

// Validate enforces the item shape.
func (i OverviewItem) Validate() error {
	if err := requireID("overview_item.node_id", string(i.NodeID)); err != nil {
		return err
	}
	if !i.Kind.Valid() {
		return invalid("overview_item.kind %q is not a known node kind", truncateForMessage(string(i.Kind)))
	}
	if err := requireField("overview_item.path", i.Path, MaxPathBytes); err != nil {
		return err
	}
	if err := requireField("overview_item.name", i.Name, MaxNameBytes); err != nil {
		return err
	}
	if err := boundField("overview_item.language", i.Language, MaxLanguageBytes); err != nil {
		return err
	}
	if err := requireNonNegative("overview_item.depth", int64(i.Depth)); err != nil {
		return err
	}
	if err := requireNonNegative("overview_item.file_count", i.FileCount); err != nil {
		return err
	}
	if err := requireNonNegative("overview_item.symbol_count", i.SymbolCount); err != nil {
		return err
	}
	if err := requireNonNegative("overview_item.source_bytes", i.SourceBytes); err != nil {
		return err
	}
	if err := optionalID("overview_item.parent_node_id", string(i.ParentNodeID)); err != nil {
		return err
	}
	return nil
}

// QueryDeadline installs the configured per-request deadline on ctx and returns
// the context to run the request under, plus the cancel that releases it.
//
// resources.query_timeout is a DEFAULT and never a ceiling, and its default is
// zero, which means NO deadline at all. Two rules follow, and this helper is
// the single place the services that answer queries spell them:
//
//   - A ctx that ALREADY carries a deadline -- the operator's `--timeout`, an
//     MCP client's own budget -- keeps it untouched, whether it is shorter or
//     longer than the configured value. context.WithTimeout would silently take
//     the smaller of the two, so a raised budget would expire at the configured
//     default and report the call as out of time at a fraction of the time the
//     caller granted it.
//   - A non-positive timeout installs nothing. `now + 0` is an instant that has
//     already passed, so applying it would refuse every request rather than run
//     it unbounded, and an unbounded call is what returns the COMPLETE answer.
//     Cancellation remains the caller's stop in that case.
//
// The returned cancel is always non-nil and is always safe to defer.
func QueryDeadline(ctx context.Context, timeout time.Duration) (context.Context, context.CancelFunc) {
	if timeout <= 0 {
		return ctx, func() {}
	}
	if _, ok := ctx.Deadline(); ok {
		return ctx, func() {}
	}
	return context.WithTimeout(ctx, timeout)
}
