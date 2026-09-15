package model

import "encoding/json"

// NodeKind is the exhaustive node vocabulary of Section 9.2.
type NodeKind string

const (
	NodeRepository     NodeKind = "repository"
	NodeDirectory      NodeKind = "directory"
	NodeFile           NodeKind = "file"
	NodePackage        NodeKind = "package"
	NodeModule         NodeKind = "module"
	NodeNamespace      NodeKind = "namespace"
	NodeFunction       NodeKind = "function"
	NodeMethod         NodeKind = "method"
	NodeClass          NodeKind = "class"
	NodeInterface      NodeKind = "interface"
	NodeStruct         NodeKind = "struct"
	NodeEnum           NodeKind = "enum"
	NodeField          NodeKind = "field"
	NodeVariable       NodeKind = "variable"
	NodeConstant       NodeKind = "constant"
	NodeTest           NodeKind = "test"
	NodeBuildTarget    NodeKind = "build_target"
	NodeDependency     NodeKind = "dependency"
	NodeConfiguration  NodeKind = "configuration"
	NodeDocument       NodeKind = "document"
	NodeEndpoint       NodeKind = "endpoint"
	NodeDatabaseEntity NodeKind = "database_entity"
)

// Valid reports whether k is a known wire spelling. An unknown value fails
// validation; there is no competing uppercase vocabulary (Section 22).
func (k NodeKind) Valid() bool {
	switch k {
	case NodeRepository, NodeDirectory, NodeFile, NodePackage, NodeModule, NodeNamespace,
		NodeFunction, NodeMethod, NodeClass, NodeInterface, NodeStruct, NodeEnum,
		NodeField, NodeVariable, NodeConstant, NodeTest, NodeBuildTarget, NodeDependency,
		NodeConfiguration, NodeDocument, NodeEndpoint, NodeDatabaseEntity:
		return true
	}
	return false
}

// RelationKind is the exhaustive relation vocabulary of Section 9.2. Reverse
// traversal uses the indexed target column; there is no duplicated called_by.
type RelationKind string

const (
	RelContains         RelationKind = "contains"
	RelDefines          RelationKind = "defines"
	RelReferences       RelationKind = "references"
	RelCalls            RelationKind = "calls"
	RelImports          RelationKind = "imports"
	RelExports          RelationKind = "exports"
	RelImplements       RelationKind = "implements"
	RelExtends          RelationKind = "extends"
	RelOverrides        RelationKind = "overrides"
	RelReads            RelationKind = "reads"
	RelWrites           RelationKind = "writes"
	RelDataFlowsTo      RelationKind = "data_flows_to"
	RelControlDependsOn RelationKind = "control_depends_on"
	RelTests            RelationKind = "tests"
	RelDependsOn        RelationKind = "depends_on"
	RelBuilds           RelationKind = "builds"
	RelGenerates        RelationKind = "generates"
	RelDocuments        RelationKind = "documents"
	RelConfigures       RelationKind = "configures"
	RelOwns             RelationKind = "owns"
	RelRenamedFrom      RelationKind = "renamed_from"
	RelMovedFrom        RelationKind = "moved_from"
	RelMayReferTo       RelationKind = "may_refer_to"
)

// Valid reports whether k is a known wire spelling.
func (k RelationKind) Valid() bool {
	switch k {
	case RelContains, RelDefines, RelReferences, RelCalls, RelImports, RelExports,
		RelImplements, RelExtends, RelOverrides, RelReads, RelWrites, RelDataFlowsTo,
		RelControlDependsOn, RelTests, RelDependsOn, RelBuilds, RelGenerates,
		RelDocuments, RelConfigures, RelOwns, RelRenamedFrom, RelMovedFrom, RelMayReferTo:
		return true
	}
	return false
}

// Precision describes the origin of a fact, not calibrated confidence and not
// guaranteed soundness (Section 9.3). Deterministic ranking weights derived
// from it are not probabilities.
type Precision string

const (
	PrecisionCompiler       Precision = "compiler"
	PrecisionLanguageServer Precision = "language_server"
	PrecisionStaticAnalysis Precision = "static_analysis"
	PrecisionSyntax         Precision = "syntax"
	PrecisionHeuristic      Precision = "heuristic"
)

// Valid reports whether p is a known wire spelling.
func (p Precision) Valid() bool {
	switch p {
	case PrecisionCompiler, PrecisionLanguageServer, PrecisionStaticAnalysis,
		PrecisionSyntax, PrecisionHeuristic:
		return true
	}
	return false
}

// Node is a published entity within one generation binding. It is not a
// globally authoritative record: a public NodeID is valid only inside the
// binding that returned it.
type Node struct {
	ID            NodeID          `json:"id"`
	Kind          NodeKind        `json:"kind"`
	Language      string          `json:"language,omitempty"`
	Name          string          `json:"name"`
	QualifiedName string          `json:"qualified_name,omitempty"`
	Signature     string          `json:"signature,omitempty"`
	FileID        FileID          `json:"file_id,omitempty"`
	ContentHash   string          `json:"content_hash,omitempty"`
	Range         *SourceRange    `json:"range,omitempty"`
	Metadata      json.RawMessage `json:"metadata,omitempty"`
	// SemanticSource labels where this node came from. The zero value and
	// SemanticCanonical both mean a sealed canonical fact; SemanticLSP marks an
	// ephemeral overlay answer, which has no canonical NodeID because nothing
	// was sealed, so its pinned file and range carry the whole result.
	SemanticSource SemanticSource `json:"semantic_source"`
}

// Validate enforces the node_facts constraints, including the mixed-range check
// and the bounded, strictly typed metadata payload.
func (n Node) Validate() error {
	if err := validateResultSource("node.semantic_source", n.SemanticSource); err != nil {
		return err
	}
	if n.SemanticSource == SemanticLSP {
		if err := optionalID("node.id", string(n.ID)); err != nil {
			return err
		}
		if err := requireID("node.file_id", string(n.FileID)); err != nil {
			return err
		}
		if n.Range == nil {
			return invalid("node.range is required for an lsp overlay node")
		}
	} else if err := requireID("node.id", string(n.ID)); err != nil {
		return err
	}
	if !n.Kind.Valid() {
		return invalid("node.kind %q is not a known node kind", truncateForMessage(string(n.Kind)))
	}
	if err := requireField("node.name", n.Name, MaxNameBytes); err != nil {
		return err
	}
	if err := boundField("node.language", n.Language, MaxLanguageBytes); err != nil {
		return err
	}
	if err := boundField("node.qualified_name", n.QualifiedName, MaxQualifiedNameBytes); err != nil {
		return err
	}
	if err := boundField("node.signature", n.Signature, MaxSignatureBytes); err != nil {
		return err
	}
	if err := optionalID("node.file_id", string(n.FileID)); err != nil {
		return err
	}
	if err := optionalID("node.content_hash", n.ContentHash); err != nil {
		return err
	}
	if err := validateLocatedRange("node.range", n.FileID, n.Range); err != nil {
		return err
	}
	if len(n.Metadata) > 0 {
		if err := boundField("node.metadata", string(n.Metadata), MaxMetadataBytes); err != nil {
			return err
		}
		if !json.Valid(n.Metadata) {
			return invalid("node.metadata is not valid JSON")
		}
	}
	return nil
}

// Relation is one canonical directed edge. Several occurrences may share it;
// APIs distinguish relation count from occurrence count.
type Relation struct {
	ID   RelationID   `json:"id"`
	From NodeID       `json:"from"`
	Kind RelationKind `json:"kind"`
	To   NodeID       `json:"to"`
}

// Validate enforces the relation_ids constraints.
func (r Relation) Validate() error {
	if err := requireID("relation.id", string(r.ID)); err != nil {
		return err
	}
	if err := requireID("relation.from", string(r.From)); err != nil {
		return err
	}
	if err := requireID("relation.to", string(r.To)); err != nil {
		return err
	}
	if !r.Kind.Valid() {
		return invalid("relation.kind %q is not a known relation kind", truncateForMessage(string(r.Kind)))
	}
	return nil
}

// Evidence records one occurrence backing a node attribute set or a relation.
// Exactly one of NodeID or RelationID is present.
type Evidence struct {
	ID              EvidenceID    `json:"id"`
	UnitID          UnitID        `json:"unit_id"`
	ProviderID      string        `json:"provider_id"`
	ProviderVersion string        `json:"provider_version"`
	OriginRunID     ProviderRunID `json:"origin_run_id"`
	NodeID          NodeID        `json:"node_id,omitempty"`
	RelationID      RelationID    `json:"relation_id,omitempty"`
	Precision       Precision     `json:"precision"`
	FileID          FileID        `json:"file_id,omitempty"`
	ContentHash     string        `json:"content_hash,omitempty"`
	Range           *SourceRange  `json:"range,omitempty"`
	// Bytes is the same interval as Range seen from the other side of
	// persistence. The evidence table stores start_byte/end_byte and no line or
	// column, so a QUERY row hydrated from storage carries Bytes and a nil
	// Range; only a fact on its way IN, straight from a provider, carries
	// Range. Identity is settled at ingest and is not re-derived here:
	// NewEvidenceID reads Range, so a hydrated row must never be re-hashed --
	// the one path that does re-identify stored evidence, the carry-over in
	// sqlite/delta.go, rebuilds Range from the same two offsets for exactly
	// that reason and does not use this field. Readers must not synthesise a
	// Range from Bytes either: a one-based line is not derivable from an offset
	// without the file, and inventing one would fabricate a location.
	Bytes     *ByteRange `json:"bytes,omitempty"`
	NativeKey string     `json:"native_key,omitempty"`
	Detail    string     `json:"detail,omitempty"`
}

// Validate enforces the evidence table constraints: the subject XOR, the
// precision vocabulary and the mixed-range check.
func (e Evidence) Validate() error {
	if err := requireID("evidence.id", string(e.ID)); err != nil {
		return err
	}
	if err := requireID("evidence.unit_id", string(e.UnitID)); err != nil {
		return err
	}
	if err := requireField("evidence.provider_id", e.ProviderID, MaxIdentifierBytes); err != nil {
		return err
	}
	if err := requireField("evidence.provider_version", e.ProviderVersion, MaxIdentifierBytes); err != nil {
		return err
	}
	if err := requireID("evidence.origin_run_id", string(e.OriginRunID)); err != nil {
		return err
	}
	hasNode, hasRelation := e.NodeID != "", e.RelationID != ""
	if hasNode == hasRelation {
		return invalid("evidence must name exactly one of node_id or relation_id")
	}
	if err := optionalID("evidence.node_id", string(e.NodeID)); err != nil {
		return err
	}
	if err := optionalID("evidence.relation_id", string(e.RelationID)); err != nil {
		return err
	}
	if !e.Precision.Valid() {
		return invalid("evidence.precision %q is not a known precision class", truncateForMessage(string(e.Precision)))
	}
	if err := optionalID("evidence.file_id", string(e.FileID)); err != nil {
		return err
	}
	if err := optionalID("evidence.content_hash", e.ContentHash); err != nil {
		return err
	}
	if err := validateLocatedRange("evidence.range", e.FileID, e.Range); err != nil {
		return err
	}
	if err := validateLocatedBytes("evidence.bytes", e.FileID, e.Bytes); err != nil {
		return err
	}
	if err := boundField("evidence.native_key", e.NativeKey, MaxNativeKeyBytes); err != nil {
		return err
	}
	if err := boundField("evidence.detail", e.Detail, MaxDetailBytes); err != nil {
		return err
	}
	// Identity is derived, not asserted. Every input to NewEvidenceID is a field
	// of this struct, so a mismatch means the producer invented an ID; accepting
	// it would let two rows describe the same occurrence under different keys and
	// defeat the Section 9.3 dedup property entirely.
	if want := NewEvidenceID(e); e.ID != want {
		return invalid("evidence.id %q does not match the identity derived from its fields (%q)",
			truncateForMessage(string(e.ID)), want)
	}
	return nil
}

// MatchBasis records how a candidate was reconciled to a canonical node. The
// values follow the Section 9.4 resolution order; Section 9.4 also names match
// basis as the second element of the public attribute precedence tuple, so it
// must be a stable vocabulary rather than free text.
type MatchBasis string

const (
	// MatchNativeKey is an unambiguous strong native identifier already
	// associated with this source scope.
	MatchNativeKey MatchBasis = "native_key"
	// MatchSourceLocation is an exact language/path/range/kind/qualified-name
	// match.
	MatchSourceLocation MatchBasis = "source_location"
	// MatchQualifiedSignature is an exact
	// package/qualified-name/signature/defining-file match.
	MatchQualifiedSignature MatchBasis = "qualified_signature"
	// MatchStructuralKey is a deterministic structural key.
	MatchStructuralKey MatchBasis = "structural_key"
	// MatchUnresolved is a provider-local unresolved entity: an honest
	// placeholder, never a fabricated precise definition.
	MatchUnresolved MatchBasis = "unresolved"
)

// Valid reports whether b is a known wire spelling.
func (b MatchBasis) Valid() bool {
	switch b {
	case MatchNativeKey, MatchSourceLocation, MatchQualifiedSignature,
		MatchStructuralKey, MatchUnresolved:
		return true
	}
	return false
}

// NodeCandidate is what a provider hands the resolver before canonical identity
// exists. Section 11.1 enumerates its contents as "provider/scope/native key,
// optional strong key, node kind/language/name/qualified name/signature, and
// source file/hash/range"; the fields below are exactly that list and nothing
// more, because a candidate must not carry an assumed NodeID.
type NodeCandidate struct {
	ProviderID    string       `json:"provider_id"`
	ScopeKey      string       `json:"scope_key"`
	NativeKey     string       `json:"native_key"`
	StrongKey     string       `json:"strong_key,omitempty"`
	Kind          NodeKind     `json:"kind"`
	Language      string       `json:"language,omitempty"`
	Name          string       `json:"name"`
	QualifiedName string       `json:"qualified_name,omitempty"`
	Signature     string       `json:"signature,omitempty"`
	FileID        FileID       `json:"file_id,omitempty"`
	ContentHash   string       `json:"content_hash,omitempty"`
	Range         *SourceRange `json:"range,omitempty"`
}

// Validate enforces candidate shape before resolution work begins.
func (c NodeCandidate) Validate() error {
	if err := requireField("node_candidate.provider_id", c.ProviderID, MaxIdentifierBytes); err != nil {
		return err
	}
	if err := requireField("node_candidate.scope_key", c.ScopeKey, MaxScopeKeyBytes); err != nil {
		return err
	}
	if err := requireField("node_candidate.native_key", c.NativeKey, MaxNativeKeyBytes); err != nil {
		return err
	}
	if err := boundField("node_candidate.strong_key", c.StrongKey, MaxNativeKeyBytes); err != nil {
		return err
	}
	if !c.Kind.Valid() {
		return invalid("node_candidate.kind %q is not a known node kind", truncateForMessage(string(c.Kind)))
	}
	if err := requireField("node_candidate.name", c.Name, MaxNameBytes); err != nil {
		return err
	}
	if err := boundField("node_candidate.language", c.Language, MaxLanguageBytes); err != nil {
		return err
	}
	if err := boundField("node_candidate.qualified_name", c.QualifiedName, MaxQualifiedNameBytes); err != nil {
		return err
	}
	if err := boundField("node_candidate.signature", c.Signature, MaxSignatureBytes); err != nil {
		return err
	}
	if err := optionalID("node_candidate.file_id", string(c.FileID)); err != nil {
		return err
	}
	if err := optionalID("node_candidate.content_hash", c.ContentHash); err != nil {
		return err
	}
	if err := validateLocatedRange("node_candidate.range", c.FileID, c.Range); err != nil {
		return err
	}
	return nil
}

// Resolution is the resolver's answer: "a canonical node, match basis, and
// bounded ambiguous candidate IDs" (Section 11.1). Ambiguous holds the equally
// supported alternatives that Section 9.4 requires be retained as may_refer_to
// edges instead of silently picking the first candidate.
type Resolution struct {
	Node  Node       `json:"node"`
	Basis MatchBasis `json:"basis"`
	// CanonicalKey is the canonical_entity_key of Section 9.1 that Node.ID
	// derives from. The resolver (internal/reconcile) is the single owner of
	// canonical-key derivation: for a minted identity it computes the key, for
	// an alias match it returns the key already registered for that node. A
	// provider copies it into NodeFact.CanonicalKey verbatim; storage rejects a
	// fact whose ID does not derive from the key it carries.
	CanonicalKey string   `json:"canonical_key"`
	Ambiguous    []NodeID `json:"ambiguous,omitempty"`
}

// Validate enforces the resolution shape, its canonical key and its ambiguity
// bound.
func (r Resolution) Validate() error {
	if err := r.Node.Validate(); err != nil {
		return err
	}
	if !r.Basis.Valid() {
		return invalid("resolution.basis %q is not a known match basis", truncateForMessage(string(r.Basis)))
	}
	if err := requireField("resolution.canonical_key", r.CanonicalKey, MaxNativeKeyBytes); err != nil {
		return err
	}
	// The ambiguous list carries no count bound. A native key aliased to more
	// equally supported identities than MaxAmbiguousCandidates is a property of
	// the repository, not an invalid resolution, and refusing it here made the
	// resolver refuse the provider's whole output instead. The list is bounded
	// in the only place a bound belongs -- the alias lookup's own page size in
	// storage -- so nothing unbounded reaches this validator.
	for i, id := range r.Ambiguous {
		if err := requireID(indexed("resolution.ambiguous", i), string(id)); err != nil {
			return err
		}
	}
	return nil
}

// NodeFact is one Node plus the bounded evidence that publishes it, as emitted
// through provider.Sink.PutNodes (Section 11.1). CanonicalKey is the
// canonical_entity_key of Section 9.1 that, with the repository and Node.Kind,
// derives Node.ID; storage records it in the node_ids dictionary and rejects a
// fact whose ID does not derive from it.
type NodeFact struct {
	Node         Node       `json:"node"`
	CanonicalKey string     `json:"canonical_key"`
	Evidence     []Evidence `json:"evidence"`
}

// Validate enforces that a published node fact has supporting evidence before
// seal, carries the canonical key its identity derives from, and that every
// evidence row names this node.
func (f NodeFact) Validate() error {
	if err := f.Node.Validate(); err != nil {
		return err
	}
	if err := requireField("node_fact.canonical_key", f.CanonicalKey, MaxNativeKeyBytes); err != nil {
		return err
	}
	if len(f.Evidence) == 0 {
		return invalid("node_fact %q has no evidence; every published node attribute set is evidence backed", f.Node.ID)
	}
	if err := boundCount("node_fact.evidence", len(f.Evidence), MaxEvidencePerFact); err != nil {
		return err
	}
	for i, e := range f.Evidence {
		if err := e.Validate(); err != nil {
			return err
		}
		if e.NodeID != f.Node.ID {
			return invalid("%s names node %q but the fact publishes %q", indexed("node_fact.evidence", i), e.NodeID, f.Node.ID)
		}
	}
	return nil
}

// RelationFact is one Relation plus the bounded evidence that publishes it.
// Each range-bearing evidence row is one occurrence of the same canonical edge.
type RelationFact struct {
	Relation Relation   `json:"relation"`
	Evidence []Evidence `json:"evidence"`
}

// Validate enforces that a published relation has supporting evidence before
// seal and that every evidence row names this relation.
func (f RelationFact) Validate() error {
	if err := f.Relation.Validate(); err != nil {
		return err
	}
	if len(f.Evidence) == 0 {
		return invalid("relation_fact %q has no evidence; every published relation is evidence backed", f.Relation.ID)
	}
	if err := boundCount("relation_fact.evidence", len(f.Evidence), MaxEvidencePerFact); err != nil {
		return err
	}
	for i, e := range f.Evidence {
		if err := e.Validate(); err != nil {
			return err
		}
		if e.RelationID != f.Relation.ID {
			return invalid("%s names relation %q but the fact publishes %q", indexed("relation_fact.evidence", i), e.RelationID, f.Relation.ID)
		}
	}
	return nil
}

// NativeAlias binds a provider-native key, within its scope, to a canonical
// node so a later unit can resolve the same native symbol without rescanning.
type NativeAlias struct {
	ScopeKey  string `json:"scope_key"`
	NativeKey string `json:"native_key"`
	NodeID    NodeID `json:"node_id"`
}

// Validate enforces that an alias stays scoped, as Section 9.4 requires for
// document-scoped SCIP locals.
func (a NativeAlias) Validate() error {
	if err := requireField("native_alias.scope_key", a.ScopeKey, MaxScopeKeyBytes); err != nil {
		return err
	}
	if err := requireField("native_alias.native_key", a.NativeKey, MaxNativeKeyBytes); err != nil {
		return err
	}
	if err := requireID("native_alias.node_id", string(a.NodeID)); err != nil {
		return err
	}
	return nil
}

// SearchUnit is one lexical document. Section 11.1 enumerates its contents as
// "a stable ID, optional NodeID, FileID, path, kind, name/qualified
// name/signature, source byte bounds, and a bounded body"; ID here is the
// search_key column. Bodies are stored once in their owning filesystem unit, so
// a symbol document carries names, signatures and bounded linked comments only.
type SearchUnit struct {
	ID            string    `json:"id"`
	NodeID        NodeID    `json:"node_id,omitempty"`
	FileID        FileID    `json:"file_id"`
	Path          string    `json:"path"`
	Kind          NodeKind  `json:"kind"`
	Name          string    `json:"name,omitempty"`
	QualifiedName string    `json:"qualified_name,omitempty"`
	Signature     string    `json:"signature,omitempty"`
	Bytes         ByteRange `json:"bytes"`
	Body          string    `json:"body"`
	TokenCount    int64     `json:"token_count"`
}

// Validate enforces the search_units constraints, including the 32-KiB chunk
// ceiling of Section 11.2 that keeps one oversized line from producing an
// oversized document.
func (u SearchUnit) Validate() error {
	if err := requireID("search_unit.id", u.ID); err != nil {
		return err
	}
	if err := optionalID("search_unit.node_id", string(u.NodeID)); err != nil {
		return err
	}
	if err := requireID("search_unit.file_id", string(u.FileID)); err != nil {
		return err
	}
	if err := requireField("search_unit.path", u.Path, MaxPathBytes); err != nil {
		return err
	}
	if !u.Kind.Valid() {
		return invalid("search_unit.kind %q is not a known node kind", truncateForMessage(string(u.Kind)))
	}
	if err := boundField("search_unit.name", u.Name, MaxNameBytes); err != nil {
		return err
	}
	if err := boundField("search_unit.qualified_name", u.QualifiedName, MaxQualifiedNameBytes); err != nil {
		return err
	}
	if err := boundField("search_unit.signature", u.Signature, MaxSignatureBytes); err != nil {
		return err
	}
	if err := u.Bytes.Validate("search_unit.bytes"); err != nil {
		return err
	}
	if err := boundField("search_unit.body", u.Body, MaxSearchBodyBytes); err != nil {
		return err
	}
	if err := requireNonNegative("search_unit.token_count", u.TokenCount); err != nil {
		return err
	}
	return nil
}
