package model

import "strconv"

// Public content-derived identifiers (Section 9.1). Every one of these is a
// lowercase SHA-256 hex string except GenerationID, which is an
// installation-local row number used for lookup and never a reproducible
// analysis fingerprint. Numeric SQLite row IDs never escape as entity identity.
type (
	// RepositoryID is a persisted random identity for one repository within
	// this installation. It is not derived from the repository's contents.
	RepositoryID string
	SnapshotID   string
	AnalysisKey  string
	UnitID       string
	FileID       string
	NodeID       string
	RelationID   string
	EvidenceID   string
	ManifestID   string
	// SessionID and ProviderRunID use cryptographic randomness: they are
	// operational identifiers and are excluded from every semantic hash.
	SessionID     string
	ProviderRunID string
	// ObservationID identifies one immutable actor observation. Section 17.2
	// defines its derivation; NewObservationID owns it.
	ObservationID string
	GenerationID  int64
)

// Hash domains of Section 9.1. The version suffix is part of the domain, so a
// change to a key's composition yields a disjoint identity space rather than
// silently reinterpreting stored IDs.
const (
	domainFile     = "file-v1"
	domainNode     = "node-v1"
	domainRelation = "relation-v1"
	domainSnapshot = "snapshot-v1"
	domainUnit     = "unit-v1"
	domainAnalysis = "analysis-v1"
	domainEvidence = "evidence-v1"
	domainNodeKey  = "node-key-v1"

	domainObservation = "observation-v1"
	domainClaimRef    = "claim-reference-v1"
	domainScopeReview = "scope-review-v1"
)

// NewFileID derives file identity from the normalized root-relative path.
// Path identity is file identity: a rename produces a new FileID and lineage is
// carried by evidence-backed renamed_from/moved_from relations instead.
func NewFileID(repo RepositoryID, normalizedPath string) FileID {
	return FileID(H(domainFile, string(repo), normalizedPath))
}

// NewNodeID derives node identity from the canonical entity key. The key
// identifies a declaration or a provider-local unresolved entity; it is not an
// assertion of timeless semantic continuity, and an edit may change it.
func NewNodeID(repo RepositoryID, kind NodeKind, canonicalKey string) NodeID {
	return NodeID(H(domainNode, string(repo), string(kind), canonicalKey))
}

// CanonicalNodeKey folds a resolution scope and the parts that distinguish one
// declaration from another into one delimiter-safe key. Section 9.4 requires
// that overloads, shadowed variables, anonymous declarations and separate
// namespaces never merge by short name, and that SCIP local identifiers stay
// scoped to their document, so scope is always the first component.
//
// This helper owns only the framing. Which string is a scope, and which extra
// parts disambiguate a declaration, is resolution policy owned by
// internal/reconcile.
func CanonicalNodeKey(scope, name string, extra ...string) string {
	h := NewHasher(domainNodeKey)
	h.AddString(scope)
	h.AddString(name)
	for _, part := range extra {
		h.AddString(part)
	}
	return h.Sum()
}

// NewRelationID derives the identity of one canonical directed edge. Several
// occurrences may share it; each occurrence carries its own range-bearing
// evidence.
func NewRelationID(repo RepositoryID, from NodeID, kind RelationKind, to NodeID) RelationID {
	return RelationID(H(domainRelation, string(repo), string(from), string(kind), string(to)))
}

// NewEvidenceID derives evidence identity from semantic unit identity, subject,
// precision, native key, source hash/range and bounded detail (Section 9.3).
// It excludes OriginRunID and every timestamp, so a reused unit keeps its
// original evidence rows and a rerun over unchanged input is a no-op.
//
// Both subject slots are always framed, one of them empty, so a node subject
// and a relation subject can never alias. Byte offsets are framed in decimal
// text and an absent range frames two empty components, which keeps "no range"
// distinct from the range [0,0).
func NewEvidenceID(e Evidence) EvidenceID {
	h := NewHasher(domainEvidence)
	h.AddString(string(e.UnitID))
	h.AddString(string(e.NodeID))
	h.AddString(string(e.RelationID))
	h.AddString(string(e.Precision))
	h.AddString(e.NativeKey)
	h.AddString(string(e.FileID))
	h.AddString(e.ContentHash)
	start, end := "", ""
	if e.Range != nil {
		start = strconv.FormatUint(e.Range.Start.Byte, 10)
		end = strconv.FormatUint(e.Range.End.Byte, 10)
	}
	h.AddString(start)
	h.AddString(end)
	h.AddString(e.Detail)
	return EvidenceID(h.Sum())
}

// NewSnapshotID derives snapshot identity. manifestHash is the aggregate digest
// of the canonical sorted source manifest, folded entry by entry with Hasher so
// the builder never concatenates a repository-sized string (Section 10.1).
func NewSnapshotID(repo RepositoryID, headProvenance, sourcePolicyHash, manifestHash string) SnapshotID {
	return SnapshotID(H(domainSnapshot, string(repo), headProvenance, sourcePolicyHash, manifestHash))
}

// NewUnitID derives the reusable unit key. spec supplies provider identity,
// scope and the pre-folded canonical-input and dependency-unit-key digests;
// analysisConfigHash is passed separately because the units table stores no
// config column of its own — the analyzer configuration reaches storage only
// through this key, which is exactly what makes a config change invalidate the
// unit (Sections 9.1, 12.2, 20.2).
func NewUnitID(spec UnitSpec, analysisConfigHash string) UnitID {
	return UnitID(H(domainUnit, spec.ProviderID, spec.ProviderVersion, spec.ScopeKey,
		analysisConfigHash, spec.InputHash, spec.DependencyHash))
}

// NewAnalysisKey derives the reproducible analysis fingerprint of a generation.
// unitMembershipHash and capabilityCompletenessHash are aggregate digests over
// canonically sorted membership and capability rows, folded with Hasher.
func NewAnalysisKey(snapshot SnapshotID, schemaFingerprint, unitMembershipHash,
	capabilityCompletenessHash, normalizationVersion, semanticConfigHash string) AnalysisKey {
	return AnalysisKey(H(domainAnalysis, string(snapshot), schemaFingerprint, unitMembershipHash,
		capabilityCompletenessHash, normalizationVersion, semanticConfigHash))
}

// Binding qualifies every public result with the generation it was read from.
// A public NodeID is valid only within its binding; queries validate membership
// in that generation before returning a fact.
type Binding struct {
	RepositoryID RepositoryID `json:"repository_id"`
	SnapshotID   SnapshotID   `json:"snapshot_id"`
	GenerationID GenerationID `json:"generation_id"`
	AnalysisKey  AnalysisKey  `json:"analysis_key"`
}

// Validate checks that a binding names a real generation. AnalysisKey may be
// absent while a generation is still staging (the column is nullable).
func (b Binding) Validate() error {
	if err := requireID("binding.repository_id", string(b.RepositoryID)); err != nil {
		return err
	}
	if err := requireID("binding.snapshot_id", string(b.SnapshotID)); err != nil {
		return err
	}
	if b.GenerationID <= 0 {
		return invalid("binding.generation_id must be a positive generation row, got %d", b.GenerationID)
	}
	if err := optionalID("binding.analysis_key", string(b.AnalysisKey)); err != nil {
		return err
	}
	return nil
}
