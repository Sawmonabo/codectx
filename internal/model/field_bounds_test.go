package model

import (
	"strings"
	"testing"
)

// TestOversizeStorageFieldsDoNotFailTheUnit protects the rule that a field
// wider than its storage ceiling is shortened and flagged by its producer, not
// a reason to refuse the fact.
//
// boundField used to reject every ceiling alike, so one symbol with a generated
// signature past 4 KiB failed ProviderResult.Validate and took the whole unit's
// facts with it -- the repository lost a file's worth of answers over a field
// nobody could resubmit. The identity fields in the second half must still
// reject: a shortened path or native key names a different thing.
func TestOversizeStorageFieldsDoNotFailTheUnit(t *testing.T) {
	id := strings.Repeat("a", IDHexLen)
	wide := strings.Repeat("x", MaxSignatureBytes+1)

	node := Node{
		ID: NodeID(id), FileID: FileID(id), Kind: NodeFunction,
		Name: strings.Repeat("n", MaxNameBytes+1), QualifiedName: strings.Repeat("q", MaxQualifiedNameBytes+1),
		Signature: wide,
	}
	if err := node.Validate(); err != nil {
		t.Fatalf("a node with an oversize name, qualified name and signature failed: %v", err)
	}
	hit := SearchHit{NodeID: NodeID(id), FileID: FileID(id), Path: "a.go", Kind: NodeFunction,
		Name: node.Name, QualifiedName: node.QualifiedName, Signature: wide,
		Tier: TierExactName, Reasons: []string{strings.Repeat("r", MaxReasonBytes+1)}}
	if err := hit.Validate(); err != nil {
		t.Fatalf("a search hit with oversize descriptive fields failed: %v", err)
	}

	// Identity keeps rejecting, and says which field and how wide.
	cand := NodeCandidate{ProviderID: "p", ScopeKey: strings.Repeat("s", MaxScopeKeyBytes+1),
		NativeKey: "k", Name: "n", Kind: NodeFunction}
	err := cand.Validate()
	if err == nil {
		t.Fatal("an oversize scope key was admitted; a shortened join key names a different unit")
	}
	if !strings.Contains(err.Error(), "node_candidate.scope_key") {
		t.Fatalf("the rejection does not name the field: %v", err)
	}
	if !truncatingField("node.signature") || truncatingField("node_candidate.native_key") {
		t.Fatal("the field classification does not match the rule it documents")
	}
}
