package model

import (
	"encoding/json"
	"slices"
)

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
	NodeSection        NodeKind = "section"
	NodeEndpoint       NodeKind = "endpoint"
	NodeDatabaseEntity NodeKind = "database_entity"
	// NodeType is a type declared outside the indexed project, which the
	// project's code names (a supertype, a receiver's type) without
	// declaring: whether it is a class, an interface, a trait or a struct
	// is not known from that code, so one kind keeps it one node wherever
	// it is named.
	NodeType NodeKind = "type"
	// NodeReturn is a callable's formal return: the one node every value the
	// callable returns flows into and every call's result flows out of. It
	// is unnamed in the source, so it is named "return", located at the
	// whole callable rather than at a token, and carries no name range; it
	// stands where a code property graph's method return does. It is not a
	// symbol: a by-name lookup lists it only when asked for its kind.
	NodeReturn NodeKind = "return"
)

// Valid reports whether k is a known wire spelling. An unknown value fails
// validation; there is no competing uppercase vocabulary (Section 22).
func (k NodeKind) Valid() bool {
	switch k {
	case NodeRepository, NodeDirectory, NodeFile, NodePackage, NodeModule, NodeNamespace,
		NodeFunction, NodeMethod, NodeClass, NodeInterface, NodeStruct, NodeEnum,
		NodeField, NodeVariable, NodeConstant, NodeTest, NodeBuildTarget, NodeDependency,
		NodeConfiguration, NodeDocument, NodeSection, NodeEndpoint, NodeDatabaseEntity, NodeType, NodeReturn:
		return true
	}
	return false
}

// NonSymbolKinds are the kinds a lookup by name or by file leaves out when
// it names no kind: a return node, whose name is the same in every callable,
// would otherwise answer every query for "return" and every qualified
// prefix of a callable. A lookup that names the kind still finds it.
func NonSymbolKinds() []NodeKind { return []NodeKind{NodeReturn} }

// Symbol reports whether a node of kind k answers a lookup by name or by
// file that names no kind (NonSymbolKinds).
func (k NodeKind) Symbol() bool { return !slices.Contains(NonSymbolKinds(), k) }

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
	// RelMayCall runs from a caller to a declaration whose name, call shape
	// and language family fit one of the caller's open call sites (ADR-0015).
	// Its precision is heuristic and its resolution name_only, or unique_name
	// when exactly one compatible declaration exists. It is computed per query
	// and never published as a fact.
	RelMayCall RelationKind = "may_call"
	// RelMayImport runs from a file to a file whose path matches the literal
	// fragments of one of its open import sites. It is computed per query and
	// never published as a fact.
	RelMayImport RelationKind = "may_import"
	// RelRoutesTo runs from an endpoint (a program entry point or a framework
	// route) to the function that handles it.
	RelRoutesTo RelationKind = "routes_to"
)

// Valid reports whether k is a known wire spelling.
func (k RelationKind) Valid() bool {
	switch k {
	case RelContains, RelDefines, RelReferences, RelCalls, RelImports, RelExports,
		RelImplements, RelExtends, RelOverrides, RelReads, RelWrites, RelDataFlowsTo,
		RelControlDependsOn, RelTests, RelDependsOn, RelBuilds, RelGenerates,
		RelDocuments, RelConfigures, RelOwns, RelRenamedFrom, RelMovedFrom, RelMayReferTo,
		RelMayCall, RelMayImport, RelRoutesTo:
		return true
	}
	return false
}

// QueryTime reports a relation kind that answers compute per query from the
// stored facts and that no provider ever publishes.
func (k RelationKind) QueryTime() bool { return k == RelMayCall || k == RelMayImport }

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
	// NameRange is the declaration's name token, inside Range: the `Foo` of
	// `func Foo()`. It is absent for a node the source names with no token of
	// its own (a module, an anonymous function, an endpoint, a reference).
	NameRange *SourceRange `json:"name_range,omitempty"`
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
	if n.NameRange != nil {
		if n.Range == nil {
			return invalid("node.name_range is present without node.range; a name lies inside its node")
		}
		if err := n.NameRange.Validate("node.name_range"); err != nil {
			return err
		}
		if n.NameRange.Start.Byte < n.Range.Start.Byte || n.NameRange.End.Byte > n.Range.End.Byte {
			return invalid("node.name_range lies outside node.range")
		}
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
	// Dispatch is, for one occurrence of a call, whether the call can run an
	// override of its target (DispatchVirtual) or cannot (DispatchExact: a
	// `super` call, a qualified call, a call on a constructed object, a final,
	// static or non-virtual method). It is computed per occurrence, so one
	// relation can hold a virtual and an exact occurrence. It is empty for
	// every evidence row that is not a call occurrence.
	Dispatch Dispatch `json:"dispatch,omitempty"`
	// SetSize is the number of targets the site this occurrence belongs to was
	// bound to when the analysis bound it to several (one class's method per
	// class a value flow reaches, one function per address-taken function of
	// a signature, one file per file an include matches); each of those
	// relations carries the same SetSize, read as "1 of n". It is 0 for a
	// site bound to one target, and never 1.
	SetSize int64 `json:"set_size,omitempty"`
}

// Dispatch is the per-occurrence dispatch field of call evidence.
type Dispatch string

const (
	// DispatchVirtual is a call that can run an override of its target.
	DispatchVirtual Dispatch = "virtual"
	// DispatchExact is a call that runs exactly its target.
	DispatchExact Dispatch = "exact"
)

// Valid reports whether d is a known wire spelling. The empty value, which
// every non-call evidence row carries, is valid.
func (d Dispatch) Valid() bool {
	switch d {
	case "", DispatchVirtual, DispatchExact:
		return true
	}
	return false
}

// Validate enforces the evidence table constraints: the subject XOR, the
// precision vocabulary and the mixed-range check.
func (e Evidence) Validate() error {
	if err := requireEvidenceID("evidence.id", e.ID); err != nil {
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
	if !e.Dispatch.Valid() {
		return invalid("evidence.dispatch %q is not a known dispatch", truncateForMessage(string(e.Dispatch)))
	}
	if e.Dispatch != "" && !hasRelation {
		return invalid("evidence.dispatch is set on node evidence; only a call occurrence carries it")
	}
	if err := requireNonNegative("evidence.set_size", e.SetSize); err != nil {
		return err
	}
	if e.SetSize == 1 {
		return invalid("evidence.set_size is 1; a site bound to one target carries 0")
	}
	if e.SetSize != 0 && !hasRelation {
		return invalid("evidence.set_size is set on node evidence; only a relation occurrence carries it")
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
	// Descriptor is a located declaration's descriptor path: its qualified
	// name along its ancestry, each enclosing module, type and function
	// (ADR-0014). With the file, kind and language it keys the declaration's
	// identity; the byte range never does. An external supertype's node
	// carries the supertype's qualified name, an external method's node that
	// name and the method's name. It is empty for a candidate whose identity
	// is file-scoped (a local, an anonymous function, a call site).
	Descriptor string `json:"descriptor,omitempty"`
	// Disambiguator separates declarations that share a descriptor path: the
	// parameter types as written where the language overloads, the enclosing
	// branch where a name is defined conditionally, or the content match the
	// resolver settles for an exact duplicate (ADR-0014).
	Disambiguator string `json:"disambiguator,omitempty"`
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
	if err := boundField("node_candidate.descriptor", c.Descriptor, MaxQualifiedNameBytes); err != nil {
		return err
	}
	if err := boundField("node_candidate.disambiguator", c.Disambiguator, MaxSignatureBytes); err != nil {
		return err
	}
	if c.Disambiguator != "" && c.Descriptor == "" {
		return invalid("node_candidate.disambiguator is set without a descriptor; it separates declarations that share one")
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
	// The ambiguous list carries no count bound. How many equally supported
	// identities a native key is aliased to is a property of the repository,
	// not an invalid resolution. The list is bounded by what the unit's sealed
	// dependencies published: the resolver reads it from their alias table,
	// which holds those rows and nothing else.
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
	// The canonical key is a 32-byte digest rendered as lowercase hex, not free
	// text: storage keeps it as canonical_key BLOB(32) and the node identity
	// derives from the decoded bytes, so a key that is not a digest has no
	// storable form. Refusing it here names the producer's field; the storage
	// writer keeps its own assertion because a fact can reach it from a carry
	// path that never re-ran this method.
	if err := requireID("node_fact.canonical_key", f.CanonicalKey); err != nil {
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
	if f.Relation.Kind.QueryTime() {
		return invalid("relation_fact %q is a %s relation, which answers compute per query and no provider publishes", f.Relation.ID, f.Relation.Kind)
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
	return nil
}

// Node metadata keys (Section 9.3). A node's Metadata is a JSON object; these
// are the keys whose values this package defines.
const (
	// MetaResolution holds a NameResolution: how a name-resolved node, or a
	// relation answers derive by name, was resolved.
	MetaResolution = "resolution"
	// MetaCandidates holds the number of candidates the resolution considered.
	MetaCandidates = "candidates"
	// MetaCallee holds the callee name of a call placeholder, as written.
	MetaCallee = "callee"
	// MetaReason, MetaShape and MetaArguments describe an open call site
	// (OpenSiteMetadata); MetaReason and MetaFragments an open import site
	// (OpenImportSiteMetadata).
	MetaReason    = "reason"
	MetaShape     = "shape"
	MetaArguments = "arguments"
	MetaFragments = "fragments"
	// MetaFramework, MetaMethod, MetaRoute, MetaPath and MetaPrefix describe an
	// endpoint (EndpointMetadata).
	MetaFramework = "framework"
	MetaMethod    = "method"
	MetaRoute     = "route"
	MetaPath      = "path"
	MetaPrefix    = "prefix"
	// MetaDecorators holds a declaration's decorators and annotations, in
	// source order ([]Decorator).
	MetaDecorators = "decorators"
	// MetaTypeAnnotations holds the annotations written on the types of a
	// declaration's signature, in source order ([]Decorator): Java type-use
	// annotations, which annotate a type rather than the declaration. A
	// parameter's own decorators are on its variable node, under
	// MetaDecorators.
	MetaTypeAnnotations = "type_annotations"
	// MetaExtensible marks a method that code outside the indexed project
	// can override or implement under its language's rules (true), so the
	// overrides the index lists for it may not be all of them.
	MetaExtensible = "extensible"
	// MetaUnresolvedLines names the rows of a requirements file's
	// configuration node that record the lines no requirement interprets, in
	// source order; each row's value is {"line","reason","via"} and the row is
	// located at its line. They are always rows, never inline, so each keeps
	// its range.
	MetaUnresolvedLines = "unresolved_lines"
)

// NameResolution is the `resolution` attribute of Section 9.3: the factual
// basis a name was resolved on. No numeric confidence is published anywhere.
type NameResolution string

const (
	ResolutionExact      NameResolution = "exact"
	ResolutionImport     NameResolution = "import"
	ResolutionSameModule NameResolution = "same_module"
	// ResolutionUniqueName is a name-only candidate that is the one compatible
	// declaration of its name in the generation.
	ResolutionUniqueName NameResolution = "unique_name"
	// ResolutionNameOnly is a name-only candidate among several compatible
	// declarations of its name.
	ResolutionNameOnly   NameResolution = "name_only"
	ResolutionAmbiguous  NameResolution = "ambiguous"
	ResolutionUnresolved NameResolution = "unresolved"
	// ResolutionExternal is a structural node for a supertype outside the
	// indexed project, or a method of one, so a hierarchy relation or a call
	// on a receiver of that type has an endpoint. The node is keyed by its
	// NodeCandidate.Descriptor (the supertype's qualified name; for a method,
	// that name and the method's) and Disambiguator, so a call target and an
	// `overrides` target of the same external method are one node.
	ResolutionExternal NameResolution = "external"
)

// Valid reports whether r is a known wire spelling.
func (r NameResolution) Valid() bool {
	switch r {
	case ResolutionExact, ResolutionImport, ResolutionSameModule, ResolutionUniqueName,
		ResolutionNameOnly, ResolutionAmbiguous, ResolutionUnresolved, ResolutionExternal:
		return true
	}
	return false
}

// OpenSiteReason is why the analysis bound no target to a call site.
type OpenSiteReason string

const (
	// ReasonReceiverUntyped is a member call whose receiver has no recovered
	// type.
	ReasonReceiverUntyped OpenSiteReason = "receiver_untyped"
	// ReasonValueUntracked is a call through a value whose flow the analysis
	// does not follow.
	ReasonValueUntracked OpenSiteReason = "value_untracked"
	// ReasonGlobalUnbound is a bare name no visible scope declares.
	ReasonGlobalUnbound OpenSiteReason = "global_unbound"
	// ReasonComputed is a callee, or an import path, that does not fold to a
	// name: `obj[k]()`, `require(expr)`. An open call site with this reason
	// has no candidates.
	ReasonComputed OpenSiteReason = "computed"
	// ReasonReflective is a call of a reflection API that runs a method or
	// function chosen at run time (`m.invoke(obj)`, `v.Call(args)`): the
	// site also calls that API's external method, and what it reaches is
	// not known.
	ReasonReflective OpenSiteReason = "reflective"
)

// Valid reports whether r is a known wire spelling.
func (r OpenSiteReason) Valid() bool {
	switch r {
	case ReasonReceiverUntyped, ReasonValueUntracked, ReasonGlobalUnbound, ReasonComputed, ReasonReflective:
		return true
	}
	return false
}

// Named reports whether an open site of reason r is named by its callee:
// whether a declaration of that name and shape may be what it calls. A
// computed callee names nothing, and a reflective one names the reflection
// API it calls, not what that API runs.
func (r OpenSiteReason) Named() bool { return r != ReasonComputed && r != ReasonReflective }

// CallShape is the form of a call site's callee expression, as candidates
// for it are matched: methods and function-valued fields for a member site,
// functions for a bare or value site, classes for a construction.
type CallShape string

const (
	CallShapeBare      CallShape = "bare"
	CallShapeMember    CallShape = "member"
	CallShapeValue     CallShape = "value"
	CallShapeConstruct CallShape = "construct"
)

// Valid reports whether c is a known wire spelling.
func (c CallShape) Valid() bool {
	switch c {
	case CallShapeBare, CallShapeMember, CallShapeValue, CallShapeConstruct:
		return true
	}
	return false
}

// OpenSiteMetadata is the metadata of an open-site node: one node per call
// site no analysis bound, located at the site and named by the callee, with
// the caller's `calls` edge to it (ADR-0015). Its candidates are computed per
// query as `may_call`, never stored.
type OpenSiteMetadata struct {
	// Resolution is always ResolutionUnresolved.
	Resolution NameResolution `json:"resolution"`
	Reason     OpenSiteReason `json:"reason"`
	Shape      CallShape      `json:"shape"`
	// Arguments is the number of arguments the site passes.
	Arguments int64 `json:"arguments"`
}

// Validate enforces the open-site metadata shape.
func (m OpenSiteMetadata) Validate() error {
	if m.Resolution != ResolutionUnresolved {
		return invalid("open_site.resolution is %q; an open site is %q", truncateForMessage(string(m.Resolution)), ResolutionUnresolved)
	}
	if !m.Reason.Valid() {
		return invalid("open_site.reason %q is not a known open-site reason", truncateForMessage(string(m.Reason)))
	}
	if !m.Shape.Valid() {
		return invalid("open_site.shape %q is not a known call shape", truncateForMessage(string(m.Shape)))
	}
	if err := requireNonNegative("open_site.arguments", m.Arguments); err != nil {
		return err
	}
	return nil
}

// OpenImportSiteMetadata is the metadata of an open import site: an import
// whose path does not fold to a constant (`require("./x/" + name)`,
// `import(expr)`, `importlib.import_module(expr)`, `#include MACRO` with the
// macro undefined in the file). The files whose paths match its fragments are
// computed per query as `may_import` (ADR-0015).
type OpenImportSiteMetadata struct {
	// Resolution is always ResolutionUnresolved.
	Resolution NameResolution `json:"resolution"`
	// Reason is always ReasonComputed.
	Reason OpenSiteReason `json:"reason"`
	// Fragments are the path's literal parts, in source order; empty when the
	// path has none. It is absent when the list is stored as rows.
	Fragments []string `json:"fragments,omitempty"`
	// FragmentsRows is the number of rows holding the fragments when the list
	// did not fit the node's metadata (MetadataRowsKey(MetaFragments)); zero
	// when they are inline.
	FragmentsRows int64 `json:"fragments_rows,omitempty"`
}

// Validate enforces the open-import-site metadata shape. The fragment list
// has no count bound: a list too long for the node row is stored as rows.
func (m OpenImportSiteMetadata) Validate() error {
	if m.Resolution != ResolutionUnresolved {
		return invalid("open_import_site.resolution is %q; an open import site is %q", truncateForMessage(string(m.Resolution)), ResolutionUnresolved)
	}
	if m.Reason != ReasonComputed {
		return invalid("open_import_site.reason is %q; an open import site is %q", truncateForMessage(string(m.Reason)), ReasonComputed)
	}
	for i, f := range m.Fragments {
		if err := requireField(indexed("open_import_site.fragments", i), f, MaxPathBytes); err != nil {
			return err
		}
	}
	return nil
}

// ExternalMetadata is the metadata of an external supertype or external
// method node (ResolutionExternal).
type ExternalMetadata struct {
	// Resolution is always ResolutionExternal.
	Resolution NameResolution `json:"resolution"`
}

// Validate enforces the external node's metadata shape.
func (m ExternalMetadata) Validate() error {
	if m.Resolution != ResolutionExternal {
		return invalid("external.resolution is %q; an external node is %q", truncateForMessage(string(m.Resolution)), ResolutionExternal)
	}
	return nil
}

// PrefixState says whether every prefix an endpoint's route is mounted under
// (a blueprint, an included router, a router mount, a class-level mapping)
// resolved statically.
type PrefixState string

const (
	PrefixResolved   PrefixState = "resolved"
	PrefixUnresolved PrefixState = "unresolved"
)

// Valid reports whether p is a known wire spelling.
func (p PrefixState) Valid() bool { return p == PrefixResolved || p == PrefixUnresolved }

// EndpointMetadata is the metadata of an endpoint node, which `routes_to` its
// handler. A framework route carries Framework, Method, Route and Prefix; a
// program entry point (a `__main__` guard, a program `main`) carries none of
// them. The full Path is published only when every prefix resolved; a URL is
// never guessed.
type EndpointMetadata struct {
	Framework string `json:"framework,omitempty"`
	// Method is the HTTP method as the route declares it, empty when the
	// route accepts any.
	Method string `json:"method,omitempty"`
	// Route is the route as written at its declaration.
	Route  string      `json:"route,omitempty"`
	Path   string      `json:"path,omitempty"`
	Prefix PrefixState `json:"prefix,omitempty"`
}

// Validate enforces the endpoint metadata shape: a route states its prefix,
// and a path appears exactly when every prefix resolved.
func (m EndpointMetadata) Validate() error {
	if err := boundField("endpoint.framework", m.Framework, MaxIdentifierBytes); err != nil {
		return err
	}
	if err := boundField("endpoint.method", m.Method, MaxIdentifierBytes); err != nil {
		return err
	}
	if err := boundField("endpoint.route", m.Route, MaxPathBytes); err != nil {
		return err
	}
	if err := boundField("endpoint.path", m.Path, MaxPathBytes); err != nil {
		return err
	}
	if m.Prefix != "" && !m.Prefix.Valid() {
		return invalid("endpoint.prefix %q is not a known prefix state", truncateForMessage(string(m.Prefix)))
	}
	if (m.Framework != "") != (m.Route != "") {
		return invalid("endpoint names a framework without a route or a route without a framework")
	}
	if m.Route != "" && m.Prefix == "" {
		return invalid("endpoint.route is set without a prefix state")
	}
	if m.Route == "" && (m.Method != "" || m.Prefix != "" || m.Path != "") {
		return invalid("endpoint without a route carries a method, prefix or path")
	}
	if (m.Path != "") != (m.Prefix == PrefixResolved) {
		return invalid("endpoint.path is published exactly when every prefix resolved")
	}
	return nil
}

// Decorator is one decorator or annotation on a declaration, as written: its
// name (`app.route`, `Override`, `GetMapping`), its byte range and the byte
// range of each argument, in order. Framework route tables read these; the
// declaration's MetaDecorators value lists them in source order.
type Decorator struct {
	Name      string      `json:"name"`
	Bytes     ByteRange   `json:"bytes"`
	Arguments []ByteRange `json:"arguments,omitempty"`
}

// Validate enforces the decorator shape: every argument lies inside the
// decorator.
func (d Decorator) Validate() error {
	if err := requireField("decorator.name", d.Name, MaxQualifiedNameBytes); err != nil {
		return err
	}
	if err := d.Bytes.Validate("decorator.bytes"); err != nil {
		return err
	}
	for i, a := range d.Arguments {
		field := indexed("decorator.arguments", i)
		if err := a.Validate(field); err != nil {
			return err
		}
		if a.Start < d.Bytes.Start || a.End > d.Bytes.End {
			return invalid("%s [%d,%d) lies outside the decorator [%d,%d)", field, a.Start, a.End, d.Bytes.Start, d.Bytes.End)
		}
	}
	return nil
}
