package model

import (
	"strings"
	"testing"
)

// TestObservationReferencesCarryNoModelCeiling protects the rule that the model
// refuses no attestation for the SIZE of what it cites.
//
// ObservationRequest, ScopeReviewEntry and ObservationReference each used to
// reject a list longer than a hard-coded 64. An actor that read 200 files and
// cited every one of them had its observation refused with no key to raise, and
// a stored observation over that count could not be read back at all. The only
// ceiling now is the caller's workflow.max_observation_references, unlimited by
// default and reported with its own name when an operator does set it.
func TestObservationReferencesCarryNoModelCeiling(t *testing.T) {
	const count = 512 // comfortably past the 64 that used to refuse
	id := strings.Repeat("a", IDHexLen)
	refs := make([]ClaimReference, count)
	for i := range refs {
		// Distinct relation ids: the distinctness guard is a real rule and must
		// not be what admits or refuses this list.
		refs[i] = ClaimReference{RelationID: RelationID(hexCount(i))}
	}
	req := ObservationRequest{
		SessionID: SessionID(id), ActorID: "actor", ExpectedScope: 1,
		Kind: ObservationAcceptFact, References: refs, Note: "read the whole package",
	}
	if err := req.Validate(); err != nil {
		t.Fatalf("an observation citing %d claims was refused by the model: %v", count, err)
	}
	entry := ScopeReviewEntry{Category: scopeReviewCategories[0], Note: "n", References: refs}
	if err := entry.Validate(); err != nil {
		t.Fatalf("a review category citing %d claims was refused by the model: %v", count, err)
	}
	read := ObservationReference{
		ObservationID: ObservationID(id), Kind: ObservationAcceptFact,
		References: refs, Note: "n",
	}
	if err := read.Validate(); err != nil {
		t.Fatalf("a stored observation citing %d claims could not be read back: %v", count, err)
	}
}

// hexCount renders i as a full-width lowercase hex id.
func hexCount(i int) string {
	const hex = "0123456789abcdef"
	out := make([]byte, IDHexLen)
	for p := range out {
		out[p] = '0'
	}
	for p := IDHexLen - 1; p >= 0 && i > 0; p-- {
		out[p] = hex[i%16]
		i /= 16
	}
	return string(out)
}
