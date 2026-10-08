package treesitter

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"github.com/Sawmonabo/codectx/internal/model"
	"github.com/Sawmonabo/codectx/internal/provider"
	"github.com/Sawmonabo/codectx/internal/provider/treesitter/lang"
	"github.com/Sawmonabo/codectx/internal/provider/treesitter/wire"
	"github.com/Sawmonabo/codectx/internal/source"
)

// TestOverlongDeclarationNamesAreTruncatedNotRefused pins that a storage
// ceiling on a field truncates and flags rather than failing the unit.
// Generated code clears MaxQualifiedNameBytes routinely, and a length guard
// that refused the declaration would turn one such declaration into
// CTX_PROVIDER_OUTPUT_INVALID, which publishes nothing for the whole file.
// Mutation: refuse a declaration whose name or qualified name is over its
// storage bound -> this fails.
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
	b.nodes = append(b.nodes, model.NodeFact{Node: model.Node{ID: id}, Evidence: []model.Evidence{b.evidence(structure, id, "", nil, "", "")}})
	b.nodeAt[id] = 0
	for i := 0; i < 4; i++ { // 1 stored + 4 offered = 5 occurrences, clip 2
		b.addEvidence(structure, id, nil, "")
	}
	if got := len(b.nodes[0].Evidence); got != 2 {
		t.Fatalf("fact kept %d occurrences, want the configured clip of 2", got)
	}
	if b.clipped[structure] != 3 {
		t.Fatalf("clipped = %d, want the 3 occurrences past the clip", b.clipped[structure])
	}
	if b.dropped[structure] != 0 {
		t.Fatalf("dropped = %d, want 0: a clip is not one of the other bounds", b.dropped[structure])
	}
	state, bounded := b.bounds(structure, model.CapabilityState{State: model.CapabilityFresh})
	if got := state.Details[model.DetailEvidenceClipped]; got != "3" {
		t.Fatalf("%s detail is %q, want %q", model.DetailEvidenceClipped, got, "3")
	}
	if !bounded {
		t.Fatal("a file whose only bound was the evidence clip must still report partial")
	}
}

// mintResolver mints every candidate's identity from its own fields, as the
// resolver does for a unit with no completed dependency.
type mintResolver struct{}

func (mintResolver) Resolve(_ context.Context, c model.NodeCandidate) (model.Resolution, error) {
	key := model.CanonicalNodeKey(c.ScopeKey, c.NativeKey, string(c.Kind), c.QualifiedName)
	node := model.Node{ID: model.NewNodeID("", c.Kind, key), Kind: c.Kind, Language: c.Language, Name: c.Name,
		QualifiedName: c.QualifiedName, FileID: c.FileID, ContentHash: c.ContentHash, Range: c.Range}
	return model.Resolution{Node: node, CanonicalKey: key}, nil
}

// TestAFailedFunctionDegradesOnlyItsFilesDependenceRows pins decision 9's
// failure scope: a function whose analysis the worker recovered from makes
// the file's four dependence rows partial and discloses it, while the
// structure row is untouched and every other function's facts are still
// published.
//
// Failure modes: a failed function that fails the unit loses the file's
// structural facts; one that reaches the structure row reports the file's
// structure incomplete when it is not; one that drops the whole file's
// dependence facts loses the functions that were analysed; one that is not
// disclosed claims coverage the file does not have.
//
// Mutation: return an error from builder.functions for a Failed message ->
// build fails. Skip the analysed functions once one has failed -> the
// data_flows_to relation is missing. Leave the four rows fresh -> the state
// check fails.
func TestAFailedFunctionDegradesOnlyItsFilesDependenceRows(t *testing.T) {
	src := []byte("func f(p int) {\n\tx := p\n\t_ = x\n}\nfunc g() {}\n")
	golang, ok := lang.Lookup("go")
	if !ok {
		t.Fatal("go is not pinned")
	}
	b := &builder{
		ctx:  context.Background(),
		req:  provider.UnitRequest{Unit: model.UnitSpec{ProviderID: lang.ProviderID}, Resolver: mintResolver{}},
		fv:   model.FileVersion{ID: "file", Path: "a.go"},
		lang: golang,
		src:  src,
		cur:  source.NewCursor(src),
		ex: &extraction{
			decls: []wire.Decl{
				{ID: 0, Parent: -1, Kind: "function", Name: "f", Qualified: "f", Start: 0, End: 33, SigEnd: 13},
				{ID: 1, Parent: -1, Kind: "function", Name: "g", Qualified: "g", Start: 33, End: 44, SigEnd: 41},
			},
			functions: []wire.Function{
				{
					Span:   wire.Span{Start: 0, End: 33},
					Vars:   []wire.Span{{Start: 7, End: 8}, {Start: 17, End: 18}}, // p, x
					Nodes:  []wire.Span{{Start: 17, End: 23}},                     // x := p
					Flows:  []wire.Pair{{From: 0, To: 1, Node: 0}},
					Reads:  []wire.Access{{Var: 0, Node: 0}},
					Writes: []wire.Access{{Var: 1, Node: 0}},
				},
				{Span: wire.Span{Start: 33, End: 44}, Failed: true, Cause: "index out of range"},
			},
		},
	}
	if err := b.build(); err != nil {
		t.Fatalf("a failed function failed the unit: %v", err)
	}
	structureRow := model.CapabilityState{ProviderID: lang.ProviderID, Capability: capabilityName, Scope: "file:a.go", State: model.CapabilityFresh}
	rows := b.capabilities(structureRow)
	if len(rows) != len(capabilities) || !reflect.DeepEqual(rows[0], structureRow) {
		t.Fatalf("rows = %+v, want the structure row unchanged and then the four dependence rows", rows)
	}
	for i, row := range rows[1:] {
		if row.Capability != capabilities[i+1] || row.State != model.CapabilityPartial || row.DiagnosticCode != model.CodeCoverageIncomplete {
			t.Fatalf("dependence row %+v, want %s partial with %s", row, capabilities[i+1], model.CodeCoverageIncomplete)
		}
		if got := row.Details[detailFailedFunctions]; got != "1 33-44" {
			t.Fatalf("%s = %q, want the count and the failed function's range", detailFailedFunctions, got)
		}
		if got := row.Details[detailFailureCause]; got != "index out of range" {
			t.Fatalf("%s = %q, want the recovered text", detailFailureCause, got)
		}
	}
	vars := map[string]model.NodeID{}
	var owner model.NodeID
	for _, n := range b.nodes {
		switch {
		case n.Node.Kind == model.NodeVariable && n.Evidence[0].Precision == model.PrecisionStaticAnalysis:
			vars[n.Node.Name] = n.Node.ID
		case n.Node.Kind == model.NodeFunction && n.Node.Name == "f":
			owner = n.Node.ID
		}
	}
	if vars["p"] == "" || vars["x"] == "" || owner == "" {
		t.Fatalf("the analysed function's variables or owner are missing: %v, owner %q", vars, owner)
	}
	want := map[model.RelationKind][2]model.NodeID{
		model.RelDataFlowsTo: {vars["p"], vars["x"]},
		model.RelReads:       {owner, vars["p"]},
		model.RelWrites:      {owner, vars["x"]},
	}
	for kind, ends := range want {
		f := b.rels[model.NewRelationID(b.req.Binding.RepositoryID, ends[0], kind, ends[1])]
		if f == nil || len(f.Evidence) == 0 || f.Evidence[0].Precision != model.PrecisionStaticAnalysis {
			t.Fatalf("the analysed function's %s fact is missing or not static_analysis: %+v", kind, f)
		}
	}
}
