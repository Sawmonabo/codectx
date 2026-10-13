package link

import (
	"encoding/json"
	"fmt"
	"slices"
	"testing"

	"github.com/Sawmonabo/codectx/internal/model"
)

// TestOpenImportSiteKeepsEveryFragment pins that an open import site whose
// fragment list does not fit the node row publishes every fragment, in
// source order, as metadata rows on the site with a fragments_rows marker,
// and that a list that fits stays inline with no rows.
// Mutation caught: cutting the list at the metadata bound (a prefix of the
// fragments matches files the import cannot name), losing the order, or
// dropping the marker the graph reads the rows by.
func TestOpenImportSiteKeepsEveryFragment(t *testing.T) {
	var many []string
	for i := range 900 {
		many = append(many, fmt.Sprintf("part%04d/", i))
	}
	a := table("a.js", "javascript", nil, nil)
	a.DynamicImports = []DynamicImport{
		{Caller: -1, Range: span(0, 10), Fragments: many},
		{Caller: -1, Range: span(20, 30), Fragments: []string{"./x/", "", ".js"}},
	}
	got := publishFacts(t, testUnit(t, a), "u", nil, nil, nil)
	var sites []model.NodeFact
	for _, f := range got.nodes {
		if f.Node.Name == "import" {
			sites = append(sites, f)
		}
	}
	slices.SortFunc(sites, func(x, y model.NodeFact) int { return int(x.Node.Range.Start.Byte) - int(y.Node.Range.Start.Byte) })
	if len(sites) != 2 {
		t.Fatalf("published %d open import sites, want 2", len(sites))
	}
	var big model.OpenImportSiteMetadata
	if err := json.Unmarshal(sites[0].Node.Metadata, &big); err != nil || big.Validate() != nil {
		t.Fatalf("the long site's metadata %s is not a valid open import site", sites[0].Node.Metadata)
	}
	if big.Fragments != nil || big.FragmentsRows != int64(len(many)) {
		t.Fatalf("the long site's metadata is %s, want fragments_rows %d and no inline list", sites[0].Node.Metadata, len(many))
	}
	var rows []model.MetadataRow
	for _, ev := range sites[0].Evidence {
		if r, ok := model.ParseMetadataRow(ev.Detail); ok && r.Field == model.MetaFragments {
			rows = append(rows, r)
		}
	}
	raw, err := model.JoinMetadataRows(rows)
	if err != nil {
		t.Fatalf("join %d fragment rows: %v", len(rows), err)
	}
	var back []string
	if err := json.Unmarshal(raw, &back); err != nil || !slices.Equal(back, many) {
		t.Fatalf("the rows rebuild %d fragments, want all %d in source order", len(back), len(many))
	}
	if string(sites[1].Node.Metadata) != `{"fragments":["./x/",".js"],"reason":"computed","resolution":"unresolved"}` {
		t.Errorf("the short site's metadata = %s, want its non-empty fragments inline", sites[1].Node.Metadata)
	}
	if len(sites[1].Evidence) != 1 {
		t.Errorf("the short site carries %d evidence rows, want only its occurrence", len(sites[1].Evidence))
	}
}
