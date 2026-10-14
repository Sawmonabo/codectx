package model

import (
	"strings"
	"testing"
)

// TestOverlayCallSitesLieInTheCaller pins the attribution rule of a language
// server call edge (LSP 3.17: an incoming call's fromRanges are relative to
// the caller `from`, an outgoing call's to the queried caller): an edge whose
// sites name a file other than its caller's, or whose ends do not name overlay
// nodes of its own answer, is refused rather than published.
// Mutation caught: dropping the site-file check (an outgoing call's sites,
// which lie in the queried caller, published against the callee's file), or
// dropping the bounds or overlay check on From/To (an edge pointing past the
// answer's nodes or at a canonical node).
func TestOverlayCallSitesLieInTheCaller(t *testing.T) {
	id := func(c string) string { return strings.Repeat(c, 64) }
	rng := SourceRange{Start: Position{Byte: 4, Line: 1, Column: 5}, End: Position{Byte: 9, Line: 1, Column: 10}}
	node := func(file string, source SemanticSource) Node {
		n := Node{Kind: NodeFunction, Name: "f", FileID: FileID(file), Range: &rng, SemanticSource: source}
		if source != SemanticLSP {
			n.ID = NodeID(id("9"))
		}
		return n
	}
	caller, callee := id("1"), id("2")
	nodes := []Node{node(caller, SemanticLSP), node(callee, SemanticLSP), node(callee, SemanticCanonical)}
	result := func(c OverlayCall) GraphResult {
		return GraphResult{
			Meta:      QueryMeta{Binding: Binding{RepositoryID: RepositoryID(id("a")), GenerationID: 1, SnapshotID: SnapshotID(id("b"))}},
			Direction: DirectionOutgoing, Nodes: nodes, Calls: []OverlayCall{c},
		}
	}
	good := OverlayCall{Seed: NodeID(id("3")), From: 0, To: 1, SiteFile: FileID(caller), Sites: []SourceRange{rng}}
	if err := result(good).Validate(); err != nil {
		t.Fatalf("a call whose sites lie in its caller does not validate: %v", err)
	}
	for _, tc := range []struct {
		name string
		edit func(*OverlayCall)
	}{
		{"sites in the callee's file", func(c *OverlayCall) { c.SiteFile = FileID(callee) }},
		{"from past the answer's nodes", func(c *OverlayCall) { c.From = len(nodes) }},
		{"to a canonical node", func(c *OverlayCall) { c.To = 2 }},
		{"no seed", func(c *OverlayCall) { c.Seed = "" }},
	} {
		c := good
		tc.edit(&c)
		if err := result(c).Validate(); err == nil {
			t.Errorf("%s: validated, want a refusal", tc.name)
		}
	}
}

// TestGraphRequestSemanticSource pins that an empty semantic source is the
// canonical default and an unknown one is refused, so a misspelled `lsp`
// never reaches the canonical engine as if canonical had been asked for.
// Mutation caught: Validate accepting any spelling, or Source() answering the
// raw value for an empty one.
func TestGraphRequestSemanticSource(t *testing.T) {
	req := GraphRequest{Start: []NodeID{NodeID(strings.Repeat("1", 64))}, Direction: DirectionIncoming}
	if err := req.Validate(); err != nil || req.Source() != SemanticCanonical {
		t.Fatalf("empty source: Validate = %v, Source = %q; want valid and canonical", err, req.Source())
	}
	req.SemanticSource = "lps"
	if err := req.Validate(); err == nil {
		t.Fatalf("semantic_source %q validated, want a refusal", req.SemanticSource)
	}
}
