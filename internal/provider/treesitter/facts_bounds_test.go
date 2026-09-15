package treesitter

import (
	"strings"
	"testing"

	"github.com/Sawmonabo/codectx/internal/model"
	"github.com/Sawmonabo/codectx/internal/provider"
	"github.com/Sawmonabo/codectx/internal/provider/treesitter/lang"
	"github.com/Sawmonabo/codectx/internal/provider/treesitter/wire"
	"github.com/Sawmonabo/codectx/internal/source"
)

// TestOverlongDeclarationNamesAreTruncatedNotRefused pins the ruling that a
// storage ceiling on a field truncates and flags rather than failing the unit.
// Generated code clears MaxQualifiedNameBytes routinely, and the old guard
// turned one such declaration into CTX_PROVIDER_OUTPUT_INVALID, which publishes
// nothing for the whole file. Restoring the length arms of that guard makes
// this fail, which is the mutation that proves it.
func TestOverlongDeclarationNamesAreTruncatedNotRefused(t *testing.T) {
	src := []byte("func f() {}\n")
	qualified := strings.Repeat("a", 3000)
	b := &builder{
		fv:     model.FileVersion{Path: "gen/api.go"},
		src:    src,
		cur:    source.NewCursor(src),
		byName: map[string][]int{},
		ex: &extraction{decls: []wire.Decl{{
			ID: 0, Parent: -1, Kind: "function", Name: "f", Qualified: qualified,
			Start: 0, End: uint32(len(src)), SigEnd: 9,
		}}},
	}
	if err := b.validateDecls(); err != nil {
		t.Fatalf("an over-long qualified name must not fail the unit: %v", err)
	}
	d := b.decls[0]
	if len(d.Qualified) > model.MaxQualifiedNameBytes {
		t.Fatalf("qualified name stored at %d bytes, ceiling %d", len(d.Qualified), model.MaxQualifiedNameBytes)
	}
	if got := d.truncated["qualified_name"]; got != len(qualified) {
		t.Fatalf("truncated_fields records original length %d, want %d", got, len(qualified))
	}
	// The cut value is a prefix, not an identity: the native key falls back to
	// the file-local declaration key so two generated symbols sharing a
	// 2048-byte prefix do not collide into one node.
	if key := b.identityKey(&d); key == d.Qualified || !strings.HasPrefix(key, "decl:f@gen/api.go:") {
		t.Fatalf("identity key %q is the truncated qualified name", key)
	}
}

// TestEvidenceClipIsAttributedNotFoldedIntoDropped pins the disclosure of the
// per-fact evidence clip. Failure mode it protects: an operator who set
// index.max_evidence_per_fact sees only a generic partial count and cannot
// tell how much of it their own clip caused -- or, if the clip were simply
// taken out of that count, sees no partial state at all and believes the file
// carries every occurrence. Counting a clipped occurrence in dropped again
// makes this fail.
func TestEvidenceClipIsAttributedNotFoldedIntoDropped(t *testing.T) {
	b := &builder{
		req:    provider.UnitRequest{Unit: model.UnitSpec{ProviderID: lang.ProviderID}},
		fv:     model.FileVersion{Path: "a.go"},
		nodeAt: map[model.NodeID]int{},
		rels:   map[model.RelationID]*model.RelationFact{},
		// A user clip of 2: a fact keeps two occurrences, the rest are cut.
		evidenceClip: 2,
	}
	const id model.NodeID = "n1"
	b.nodes = append(b.nodes, model.NodeFact{Node: model.Node{ID: id}, Evidence: []model.Evidence{b.evidence(id, "", nil, "", "")}})
	b.nodeAt[id] = 0
	for i := 0; i < 4; i++ { // 1 stored + 4 offered = 5 occurrences, clip 2
		b.addEvidence(id, nil, "")
	}
	if got := len(b.nodes[0].Evidence); got != 2 {
		t.Fatalf("fact kept %d occurrences, want the configured clip of 2", got)
	}
	if b.clipped != 3 {
		t.Fatalf("clipped = %d, want the 3 occurrences past the clip", b.clipped)
	}
	if b.dropped != 0 {
		t.Fatalf("dropped = %d, want 0: a clip is not one of the other bounds", b.dropped)
	}
	state, bounded := b.bounds(model.CapabilityState{State: model.CapabilityFresh})
	if got := state.Details[detailEvidenceClipped]; got != "3" {
		t.Fatalf("%s detail is %q, want %q", detailEvidenceClipped, got, "3")
	}
	if !bounded {
		t.Fatal("a file whose only bound was the evidence clip must still report partial")
	}
}
