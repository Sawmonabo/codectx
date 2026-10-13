package graph

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/Sawmonabo/codectx/internal/model"
)

// rowsGraph is the fixture's reader with the metadata-row seam over a fixed
// set of rows, keyed by node.
type rowsGraph struct {
	*locatedGraph
	rows map[model.NodeID][]model.MetadataRow
}

var _ MetadataRowReader = (*rowsGraph)(nil)

func (g *rowsGraph) MetadataRows(ctx context.Context, node model.NodeID, field string) ([]model.MetadataRow, error) {
	var out []model.MetadataRow
	for _, r := range g.rows[node] {
		if r.Field == field {
			out = append(out, r)
		}
	}
	return out, ctx.Err()
}

// TestOpenImportSiteReadsFragmentRows pins that an open import site whose
// fragments are stored as metadata rows (its metadata names fragments_rows)
// is still an open import site and matches files by the whole list, read from
// its rows in ordinal order, whatever order the reader returns them in.
// Mutation caught: treating a site with no inline fragments as no site (no
// possible import at all), reading only the first row (b.js would match), or
// joining the rows out of ordinal order (nothing would match).
func TestOpenImportSiteReadsFragmentRows(t *testing.T) {
	open, _ := json.Marshal(map[string]any{"resolution": model.ResolutionUnresolved,
		"reason": model.ReasonComputed, model.MetadataRowsKey(model.MetaFragments): 2})
	file := func(p string) fixtureDecl {
		return fixtureDecl{Key: p, Name: p[strings.LastIndexByte(p, '/')+1:], Kind: model.NodeFile, File: p,
			Bytes: [2]uint64{0, 100}, Language: "javascript"}
	}
	f := newCallFixture("open_import_rows", fixtureScenario{
		Nodes: []fixtureDecl{
			file("src/app.js"), file("src/plugins/a.js"), file("src/plugins/b.js"),
			{Key: "import_site", Name: "require", Kind: model.NodeModule, File: "src/app.js", Bytes: [2]uint64{10, 30},
				Language: "javascript", Metadata: open},
		},
		Relations: []fixtureFact{
			{From: "src/app.js", Kind: model.RelImports, To: "import_site", Occurrences: site(10)},
		},
	})
	reader := &rowsGraph{locatedGraph: f.reader(), rows: map[model.NodeID][]model.MetadataRow{
		f.id(t, "import_site"): {
			{Field: model.MetaFragments, Ordinal: 1, Shape: model.RowElement, Value: json.RawMessage(`"a"`)},
			{Field: model.MetaFragments, Ordinal: 0, Shape: model.RowElement, Value: json.RawMessage(`"./plugins/"`)},
		},
	}}
	e, err := New(Options{Adjacency: f, Reader: reader, Limits: fixtureLimits()})
	if err != nil {
		t.Fatal(err)
	}
	res, err := e.Neighbors(context.Background(), model.GraphRequest{
		Start: []model.NodeID{f.id(t, "src/app.js")}, Direction: model.DirectionOutgoing,
		Relations: []model.RelationKind{model.RelImports}, MaxDepth: 1})
	if err != nil {
		t.Fatalf("imports of app.js: %v", err)
	}
	var possible []string
	for _, r := range res.Relations {
		if r.Kind == model.RelMayImport {
			possible = append(possible, f.name(r.To))
		}
	}
	if !slices.Equal(possible, []string{"a.js"}) {
		t.Errorf("possible imports = %v, want a.js alone: the fragments are ./plugins/ then a", possible)
	}
}
