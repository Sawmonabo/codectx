package model

import (
	"strings"
	"testing"
)

func pinnedEvidence() Evidence {
	return Evidence{
		UnitID:          UnitID(strings.Repeat("a", IDHexLen)),
		ProviderID:      "provider",
		ProviderVersion: "1",
		NodeID:          NodeID(strings.Repeat("b", IDHexLen)),
		Precision:       PrecisionSyntax,
		NativeKey:       "pkg.F",
		FileID:          FileID(strings.Repeat("c", IDHexLen)),
		ContentHash:     strings.Repeat("d", IDHexLen),
		Range:           &SourceRange{Start: Position{Byte: 10, Line: 1, Column: 10}, End: Position{Byte: 20, Line: 1, Column: 20}},
	}
}

// TestEvidenceIdentityIsPinned holds the evidence identity to one value across
// builds: it is the primary key every stored row carries, the keyset cursor of
// every evidence page and an input to every capsule digest, so a change to its
// derivation silently re-keys every store and moves every pinned capsule. The
// literal was computed outside the package, from the Section 9.1 framing (each
// component prefixed by its big-endian 64-bit length) over the fields below,
// as the first 16 bytes of the SHA-256. Provider identity and line
// positions are not identity inputs, so they are set only to make the row
// valid.
//
// mutation: keeping the whole digest, keeping its last 16 bytes instead of its
// first, or dropping or reordering any framed field changes the value.
func TestEvidenceIdentityIsPinned(t *testing.T) {
	const pinned = "39e7e6d2b490d96c1d31bee044abd550"
	if got := NewEvidenceID(pinnedEvidence()); got != pinned {
		t.Fatalf("NewEvidenceID = %s, pinned %s", got, pinned)
	}
}

// TestEvidenceValidationTakesTheEvidenceWidth proves every validator that
// accepts an evidence identity accepts exactly the derived width and refuses
// the width of every other public identity, so a derived identity is never
// refused and a full digest passed where evidence is expected is never
// accepted.
//
// mutation: any of these validators left on the generic 64-character check
// refuses the derived identity (first case) and accepts the full digest
// (second case).
func TestEvidenceValidationTakesTheEvidenceWidth(t *testing.T) {
	derived := NewEvidenceID(pinnedEvidence())
	full := EvidenceID(strings.Repeat("e", IDHexLen))
	validators := map[string]func(EvidenceID) error{
		"fact_reference": func(id EvidenceID) error {
			return FactReference{RelationID: RelationID(strings.Repeat("f", IDHexLen)), EvidenceIDs: []EvidenceID{id},
				ObservationID: ObservationID(strings.Repeat("1", IDHexLen))}.Validate()
		},
		"decode": func(id EvidenceID) error {
			_, err := DecodeEvidenceID(id)
			return err
		},
	}
	for name, validate := range validators {
		if err := validate(derived); err != nil {
			t.Errorf("%s refused a derived evidence identity %s: %v", name, derived, err)
		}
		if err := validate(full); err == nil {
			t.Errorf("%s accepted a %d-character identity as evidence", name, IDHexLen)
		}
	}
	e := pinnedEvidence()
	e.OriginRunID = ProviderRunID(strings.Repeat("2", IDHexLen))
	e.ID = NewEvidenceID(e)
	if err := e.Validate(); err != nil {
		t.Errorf("Evidence.Validate refused its own derived identity: %v", err)
	}
	e.ID = full
	if err := e.Validate(); err == nil {
		t.Errorf("Evidence.Validate accepted a %d-character identity", IDHexLen)
	}
}
