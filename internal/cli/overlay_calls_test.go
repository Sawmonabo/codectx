package cli

import (
	"strings"
	"testing"

	"github.com/Sawmonabo/codectx/internal/model"
)

// TestCallAnswerRendersOverlayCalls pins that a human reader of a
// `--semantic-source lsp` callers answer sees its calls: the overlay's edges
// are in GraphResult.Calls, not Relations, and its nodes have no canonical id,
// so the canonical renderer would print "relations none" over an answer that
// holds a call.
// Mutation caught: rendering an overlay answer through the canonical body, or
// dropping the call sites from the overlay body.
func TestCallAnswerRendersOverlayCalls(t *testing.T) {
	file := model.FileID(strings.Repeat("1", 64))
	rng := func(line uint32) *model.SourceRange {
		return &model.SourceRange{Start: model.Position{Byte: 10, Line: line, Column: 2}, End: model.Position{Byte: 15, Line: line, Column: 7}}
	}
	res := model.GraphResult{
		Meta:      model.QueryMeta{Overlay: &model.OverlayBinding{ProviderID: "lsp:gopls", ProviderVersion: "v1", InputDigest: "d"}},
		Direction: model.DirectionIncoming,
		Nodes: []model.Node{
			{Kind: model.NodeFunction, Name: "run", FileID: file, Range: rng(3), SemanticSource: model.SemanticLSP},
			{Kind: model.NodeFunction, Name: "helper", FileID: file, Range: rng(9), SemanticSource: model.SemanticLSP},
		},
		Calls: []model.OverlayCall{{Seed: model.NodeID(strings.Repeat("a", 64)), From: 1, To: 0,
			SiteFile: file, SitePath: "main.go", Sites: []model.SourceRange{*rng(11)}}},
	}
	var b strings.Builder
	writeCallAnswer(&b, res)
	out := b.String()
	for _, want := range []string{"nodes       2 (language server overlay)", "#1  function", "helper",
		"calls       1", "#1 -> #0", "site main.go:11:2"} {
		if !strings.Contains(out, want) {
			t.Errorf("rendered answer lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "relations   none") {
		t.Errorf("an overlay answer with a call rendered as having none:\n%s", out)
	}
}
