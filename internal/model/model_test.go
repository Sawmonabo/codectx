package model_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/Sawmonabo/codectx/internal/model"
)

const testRepo = model.RepositoryID("a1b2c3")

// TestCanonicalHashFraming protects canonical identity itself (Section 9.1).
//
// Failure mode: without a length prefix on every component, including the
// domain, moving bytes across a component boundary produces the same digest.
// Two different files, nodes or snapshots would then collide on one ID, and the
// store would silently serve one entity's facts for another. The second
// assertion protects determinism between the two hashing entry points: the
// streaming hasher folds repository-sized manifests, so if it ever disagrees
// with fixed-arity H the same input yields two different IDs depending on which
// call site produced it. The third protects the storage contract that a public
// ID decodes to exactly the 32 bytes SQLite's BLOB columns check for.
func TestCanonicalHashFraming(t *testing.T) {
	if got, want := model.H("a", "bc"), model.H("ab", "c"); got == want {
		t.Errorf("H(\"a\",\"bc\") == H(\"ab\",\"c\") = %q; component framing is not delimiter safe", got)
	}
	if got, want := model.H("d", "a", "bc"), model.H("d", "ab", "c"); got == want {
		t.Errorf("H(d,\"a\",\"bc\") == H(d,\"ab\",\"c\") = %q; component framing is not delimiter safe", got)
	}

	h := model.NewHasher("d")
	h.AddString("a")
	h.AddString("bc")
	if got, want := h.Sum(), model.H("d", "a", "bc"); got != want {
		t.Errorf("streaming hasher = %q, fixed-arity H = %q; they must agree", got, want)
	}

	raw, err := model.DecodeID(model.H("d", "x"))
	if err != nil {
		t.Fatalf("DecodeID: %v", err)
	}
	if len(raw) != 32 {
		t.Errorf("DecodeID returned %d bytes, want the 32 bytes SQLite stores", len(raw))
	}
}

// TestRelationIDIsDirectional protects graph correctness (Section 9.2).
//
// Failure mode: if direction or kind were not part of relation identity, a
// caller edge and a callee edge would share one row. Reverse queries would
// report fabricated callers, and impact analysis would expand the wrong way.
func TestRelationIDIsDirectional(t *testing.T) {
	from := model.NewNodeID(testRepo, model.NodeFunction, "pkg.Caller")
	to := model.NewNodeID(testRepo, model.NodeFunction, "pkg.Callee")

	forward := model.NewRelationID(testRepo, from, model.RelCalls, to)
	if reverse := model.NewRelationID(testRepo, to, model.RelCalls, from); forward == reverse {
		t.Errorf("reversed relation has the same ID %q; direction is not part of identity", forward)
	}
	if other := model.NewRelationID(testRepo, from, model.RelReferences, to); forward == other {
		t.Errorf("relation kind is not part of identity: %q", forward)
	}
}

// TestEvidenceIdentitySeparatesOccurrences protects occurrence counting and
// unit reuse (Sections 9.2, 9.3).
//
// Failure mode: if evidence identity ignored the source range, the several call
// occurrences of one canonical relation would collapse into one row and the API
// could not distinguish relation count from occurrence count. If it included
// the random run ID, re-running a provider over unchanged input would duplicate
// every evidence row instead of reusing the sealed unit, so the same snapshot
// would produce a different AnalysisKey on every index.
func TestEvidenceIdentitySeparatesOccurrences(t *testing.T) {
	rel := model.NewRelationID(testRepo, model.NewNodeID(testRepo, model.NodeFunction, "pkg.A"),
		model.RelCalls, model.NewNodeID(testRepo, model.NodeFunction, "pkg.B"))

	base := model.Evidence{
		UnitID:          model.UnitID(model.H("unit-v1", "treesitter", "1.0", "pkg")),
		ProviderID:      "treesitter",
		ProviderVersion: "1.0",
		OriginRunID:     model.ProviderRunID(model.H("run", "one")),
		RelationID:      rel,
		Precision:       model.PrecisionSyntax,
		FileID:          model.NewFileID(testRepo, "pkg/a.go"),
		ContentHash:     model.H("blob", "body"),
		Range:           &model.SourceRange{Start: model.Position{Byte: 10, Line: 3, Column: 2}, End: model.Position{Byte: 20, Line: 3, Column: 12}},
	}
	second := base
	second.Range = &model.SourceRange{Start: model.Position{Byte: 40, Line: 7}, End: model.Position{Byte: 50, Line: 7, Column: 10}}
	if model.NewEvidenceID(base) == model.NewEvidenceID(second) {
		t.Error("two occurrences of the same edge at different ranges share one evidence ID")
	}

	rerun := base
	rerun.OriginRunID = model.ProviderRunID(model.H("run", "two"))
	if model.NewEvidenceID(base) != model.NewEvidenceID(rerun) {
		t.Error("evidence identity changed with the run ID; it must exclude run randomness")
	}
}

// TestNodeIDIsScopeBound protects symbol resolution (Section 9.4).
//
// Failure mode: SCIP local identifiers are document scoped and shadowed or
// overloaded declarations share a short name. If the canonical key were not
// scope bound and delimiter safe, two distinct locals would merge into one
// node, so references to one variable would be attributed to another and
// context plans would pull in the wrong file.
func TestNodeIDIsScopeBound(t *testing.T) {
	outer := model.NewNodeID(testRepo, model.NodeVariable, model.CanonicalNodeKey("pkg/a.go#Outer", "x"))
	inner := model.NewNodeID(testRepo, model.NodeVariable, model.CanonicalNodeKey("pkg/a.go#Inner", "x"))
	if outer == inner {
		t.Errorf("two scopes of local %q share NodeID %q", "x", outer)
	}
	if model.CanonicalNodeKey("ab", "c") == model.CanonicalNodeKey("a", "bc") {
		t.Error("CanonicalNodeKey is not delimiter safe across scope and name")
	}
	if kindShift := model.NewNodeID(testRepo, model.NodeConstant, model.CanonicalNodeKey("pkg/a.go#Outer", "x")); kindShift == outer {
		t.Error("node kind is not part of NodeID identity")
	}
}

// TestValidationRejectsBadRangesAndEnums protects storage integrity at the only
// boundary that can still refuse bad data cheaply (Sections 9.3, 12.2, 22).
//
// Each case names a CHECK constraint or enum vocabulary that the SQLite schema
// also enforces. Reaching the store with such a value either aborts a write
// transaction mid-seal or, for the enum cases, persists a wire spelling that no
// reader's validation accepts, leaving facts that can never be queried.
func TestValidationRejectsBadRangesAndEnums(t *testing.T) {
	file := model.NewFileID(testRepo, "pkg/a.go")
	nodeID := model.NewNodeID(testRepo, model.NodeFunction, "pkg.F")
	relID := model.NewRelationID(testRepo, nodeID, model.RelCalls, nodeID)
	okRange := &model.SourceRange{Start: model.Position{Byte: 4, Line: 1}, End: model.Position{Byte: 8, Line: 1, Column: 4}}

	validNode := model.Node{ID: nodeID, Kind: model.NodeFunction, Name: "F", FileID: file, Range: okRange}
	if err := validNode.Validate(); err != nil {
		t.Fatalf("valid node rejected: %v", err)
	}
	rangeWithoutFile := validNode
	rangeWithoutFile.FileID = ""
	inverted := validNode
	inverted.Range = &model.SourceRange{Start: model.Position{Byte: 8, Line: 1}, End: model.Position{Byte: 4, Line: 1}}

	validEvidence := model.Evidence{
		UnitID:          model.UnitID(model.H("unit-v1", "x")),
		ProviderID:      "treesitter",
		ProviderVersion: "1.0",
		OriginRunID:     model.ProviderRunID(model.H("run", "one")),
		NodeID:          nodeID,
		Precision:       model.PrecisionSyntax,
		FileID:          file,
		ContentHash:     model.H("blob", "body"),
		Range:           okRange,
	}
	validEvidence.ID = model.NewEvidenceID(validEvidence)
	if err := validEvidence.Validate(); err != nil {
		t.Fatalf("valid evidence rejected: %v", err)
	}
	// A producer that invents an identity instead of deriving it would let the
	// same occurrence be stored twice under different keys, defeating the
	// Section 9.3 dedup property the derivation exists to provide.
	forgedID := validEvidence
	forgedID.ID = model.EvidenceID(model.H("evidence-v1", "invented"))
	bothSubjects := validEvidence
	bothSubjects.RelationID = relID
	noSubject := validEvidence
	noSubject.NodeID = ""
	unknownPrecision := validEvidence
	unknownPrecision.Precision = model.Precision("COMPILER")

	// Observation identity is derived from canonically sorted references so a
	// resubmission is idempotent. An invented ID stores the same observation
	// twice and makes the capsule's contradiction set wrong.
	obsReq := model.ObservationRequest{
		SessionID: model.SessionID(model.H("session", "1")), ActorID: "a",
		ExpectedScope: 1, Kind: model.ObservationUnresolved,
		References: []model.ClaimReference{{NodeID: nodeID}}, Note: "n",
	}
	validObservation := model.Observation{
		ID: model.NewObservationID(obsReq), SessionID: obsReq.SessionID, ActorID: obsReq.ActorID,
		ScopeVersion: obsReq.ExpectedScope, Kind: obsReq.Kind,
		References: obsReq.References, Note: obsReq.Note,
	}
	if err := validObservation.Validate(); err != nil {
		t.Fatalf("valid observation rejected: %v", err)
	}
	forgedObservationID := validObservation
	forgedObservationID.ID = model.ObservationID(model.H("observation-v1", "invented"))

	// node_ids.canonical_key is BLOB(32): a key that is not a 32-byte digest
	// rendered as lowercase hex has no storable form, and the node identity is
	// derived from the decoded bytes.
	validFact := model.NodeFact{Node: validNode, CanonicalKey: model.H("canonical-entity-key-v1", "pkg.F"),
		Evidence: []model.Evidence{validEvidence}}
	if err := validFact.Validate(); err != nil {
		t.Fatalf("valid node fact rejected: %v", err)
	}
	freeTextKey := validFact
	freeTextKey.CanonicalKey = "pkg.F"
	uppercaseKey := validFact
	uppercaseKey.CanonicalKey = strings.ToUpper(validFact.CanonicalKey)
	shortKey := validFact
	shortKey.CanonicalKey = validFact.CanonicalKey[:63]

	tests := []struct {
		protects string
		value    interface{ Validate() error }
	}{
		{"a free-text canonical key, which canonical_key BLOB(32) cannot store", freeTextKey},
		{"an uppercase canonical key, which decodes to the same bytes under two spellings", uppercaseKey},
		{"a short canonical key, which is not a 32-byte digest", shortKey},
		{"node_facts range without its file_id, which the schema's mixed-range CHECK rejects", rangeWithoutFile},
		{"an inverted half-open range, which would serve bytes outside the entity", inverted},
		{"evidence naming both a node and a relation, breaking the subject XOR", bothSubjects},
		{"evidence naming no subject at all, an unattributable fact", noSubject},
		{"an uppercase precision spelling competing with the lowercase vocabulary", unknownPrecision},
		{"evidence carrying an invented ID, which would store one occurrence under two keys", forgedID},
		{"an observation carrying an invented ID, which would break resubmission idempotence", forgedObservationID},
	}
	for _, tc := range tests {
		t.Run(tc.protects, func(t *testing.T) {
			err := tc.value.Validate()
			if err == nil {
				t.Fatalf("Validate accepted %#v, want a typed rejection", tc.value)
			}
			var typed *model.Error
			if !errors.As(err, &typed) {
				t.Fatalf("Validate returned %T, want *model.Error", err)
			}
			if typed.Code == "" || typed.Message == "" {
				t.Errorf("typed error = %+v, want a code and a safe message", typed)
			}
		})
	}
}

// TestEstimateTokensUTF8Bytes protects budget determinism (Section 15.4).
//
// Failure mode: the estimate is stored in every context manifest and slice, so
// an off-by-one at a division boundary changes the canonical manifest hash and
// breaks manifest reuse. A negative byte count must fail rather than silently
// estimate zero, which would pack a file into a slice with no budget for it.
// The label is persisted alongside the estimate; changing it invalidates every
// stored manifest.
func TestEstimateTokensUTF8Bytes(t *testing.T) {
	for _, tc := range []struct {
		in   int64
		want int64
	}{{0, 0}, {1, 1}, {3, 1}, {4, 2}} {
		got, err := model.EstimateTokensUTF8Bytes(tc.in)
		if err != nil {
			t.Fatalf("EstimateTokensUTF8Bytes(%d): %v", tc.in, err)
		}
		if got != tc.want {
			t.Errorf("EstimateTokensUTF8Bytes(%d) = %d, want %d", tc.in, got, tc.want)
		}
	}
	got, err := model.EstimateTokensUTF8Bytes(-1)
	if err == nil {
		t.Fatalf("EstimateTokensUTF8Bytes(-1) = %d, want an error", got)
	}
	var typed *model.Error
	if !errors.As(err, &typed) || typed.Code != model.CodeArgumentInvalid {
		t.Errorf("EstimateTokensUTF8Bytes(-1) error = %v, want *model.Error with %s", err, model.CodeArgumentInvalid)
	}
}
