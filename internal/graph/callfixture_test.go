package graph

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"testing"

	"github.com/Sawmonabo/codectx/internal/model"
)

// The call-answer fixtures (testdata/dispatch.json): small programs whose
// facts are written the way the linker publishes them -- one `calls` per site
// to the method the receiver's declared type declares, `overrides` to the
// directly overridden method, each occurrence carrying its own dispatch and
// set size -- with every node and call-site range computed from the source
// text the scenario carries.

type fixtureOccurrence struct {
	Bytes    [2]uint64      `json:"bytes"`
	Dispatch model.Dispatch `json:"dispatch"`
	SetSize  int64          `json:"set_size"`
}

type fixtureFact struct {
	From        string              `json:"from"`
	Kind        model.RelationKind  `json:"kind"`
	To          string              `json:"to"`
	Occurrences []fixtureOccurrence `json:"occurrences"`
}

type fixtureDecl struct {
	Name     string          `json:"name"`
	Kind     model.NodeKind  `json:"kind"`
	File     string          `json:"file"`
	Bytes    [2]uint64       `json:"bytes"`
	Metadata json.RawMessage `json:"metadata"`
}

type fixtureScenario struct {
	Source    map[string]string `json:"source"`
	Nodes     []fixtureDecl     `json:"nodes"`
	Relations []fixtureFact     `json:"relations"`
}

// callFixture is one scenario as both engine ports: the Adjacency (with the
// evidence-row seam) and, through reader(), a packed reader that also locates.
type callFixture struct {
	scenario  string
	binding   model.Binding
	nodes     map[model.NodeID]model.Node
	located   map[model.NodeID]model.NodeLocation
	relations []model.Relation
	evidence  map[model.RelationID][]model.Evidence
	byName    map[string]model.NodeID
	relByEnds map[string]model.RelationID
}

// loadCallFixture reads one scenario of testdata/dispatch.json.
func loadCallFixture(t *testing.T, scenario string) *callFixture {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "dispatch.json"))
	if err != nil {
		t.Fatalf("read dispatch fixture: %v", err)
	}
	var doc struct {
		Scenarios map[string]fixtureScenario `json:"scenarios"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("decode dispatch fixture: %v", err)
	}
	sc, ok := doc.Scenarios[scenario]
	if !ok {
		t.Fatalf("dispatch fixture has no scenario %q", scenario)
	}
	f := &callFixture{
		scenario: scenario,
		binding: model.Binding{
			RepositoryID: model.RepositoryID(fixtureID("repo-dispatch")),
			SnapshotID:   model.SnapshotID(fixtureID("snap-" + scenario)),
			GenerationID: 1,
			AnalysisKey:  model.AnalysisKey(fixtureID("akey-" + scenario)),
		},
		nodes:     map[model.NodeID]model.Node{},
		located:   map[model.NodeID]model.NodeLocation{},
		evidence:  map[model.RelationID][]model.Evidence{},
		byName:    map[string]model.NodeID{},
		relByEnds: map[string]model.RelationID{},
	}
	for _, d := range sc.Nodes {
		id := f.nodeID(d.Name)
		file := model.FileID(fixtureID(scenario + "/" + d.File))
		f.nodes[id] = model.Node{ID: id, Kind: d.Kind, Name: d.Name, QualifiedName: d.Name,
			Language: "python", FileID: file, Metadata: d.Metadata, SemanticSource: model.SemanticCanonical}
		f.byName[d.Name] = id
		if d.Bytes[1] > d.Bytes[0] {
			f.located[id] = model.NodeLocation{NodeID: id, FileID: file, Path: d.File,
				Bytes: model.ByteRange{Start: d.Bytes[0], End: d.Bytes[1]}}
		}
	}
	file := func(from string) model.FileID {
		if loc, ok := f.located[f.byName[from]]; ok {
			return loc.FileID
		}
		return ""
	}
	for _, r := range sc.Relations {
		id := model.RelationID(fixtureID(scenario + "/" + r.From + "/" + string(r.Kind) + "/" + r.To))
		f.relations = append(f.relations, model.Relation{ID: id, From: f.nodeID(r.From), Kind: r.Kind, To: f.nodeID(r.To)})
		f.relByEnds[r.From+" "+string(r.Kind)+" "+r.To] = id
		for i, o := range r.Occurrences {
			ev := model.Evidence{
				ID:         model.EvidenceID(fixtureID(string(id) + "/" + string(rune('a'+i)))),
				RelationID: id, Precision: model.PrecisionStaticAnalysis,
				FileID: file(r.From), Bytes: &model.ByteRange{Start: o.Bytes[0], End: o.Bytes[1]},
				Dispatch: o.Dispatch, SetSize: o.SetSize,
			}
			f.evidence[id] = append(f.evidence[id], ev)
		}
	}
	for id := range f.evidence {
		sort.Slice(f.evidence[id], func(i, j int) bool { return f.evidence[id][i].ID < f.evidence[id][j].ID })
	}
	sort.Slice(f.relations, func(i, j int) bool { return f.relations[i].ID < f.relations[j].ID })
	return f
}

func (f *callFixture) nodeID(name string) model.NodeID {
	return model.NodeID(fixtureID(f.scenario + "/" + name))
}

// id names a declared node, failing the test on a name the scenario lacks.
func (f *callFixture) id(t *testing.T, name string) model.NodeID {
	t.Helper()
	id, ok := f.byName[name]
	if !ok {
		t.Fatalf("scenario %s declares no node %q", f.scenario, name)
	}
	return id
}

// rel names a declared relation by its ends.
func (f *callFixture) rel(t *testing.T, from string, kind model.RelationKind, to string) model.RelationID {
	t.Helper()
	id, ok := f.relByEnds[from+" "+string(kind)+" "+to]
	if !ok {
		t.Fatalf("scenario %s declares no relation %s %s %s", f.scenario, from, kind, to)
	}
	return id
}

// name is the readable name of a node id, for failure messages.
func (f *callFixture) name(id model.NodeID) string {
	if n, ok := f.nodes[id]; ok {
		return n.Name
	}
	return string(id)
}

// engine builds an engine over the scenario with both ports.
func (f *callFixture) engine(t *testing.T) *Engine {
	t.Helper()
	e, err := New(Options{Adjacency: f, Reader: f.reader(), Limits: fixtureLimits()})
	if err != nil {
		t.Fatalf("new engine: %v", err)
	}
	return e
}

// reader is the packed reader over the scenario, with the locating seam.
func (f *callFixture) reader() *locatedGraph {
	nodes := make([]model.Node, 0, len(f.nodes))
	for _, n := range f.nodes {
		nodes = append(nodes, n)
	}
	sort.Slice(nodes, func(i, j int) bool { return nodes[i].ID < nodes[j].ID })
	counts := make(map[model.RelationID]int64, len(f.evidence))
	for id, rows := range f.evidence {
		counts[id] = int64(len(rows))
	}
	g := NewMemoryGraph(f.binding, nodes, append([]model.Relation(nil), f.relations...)).WithEvidenceCounts(counts)
	return &locatedGraph{MemoryGraph: g, f: f}
}

func (f *callFixture) NodesByID(ctx context.Context, ids []model.NodeID) ([]model.Node, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	var out []model.Node
	for _, id := range ids {
		if n, ok := f.nodes[id]; ok {
			out = append(out, n)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

func (f *callFixture) EvidenceFor(ctx context.Context, relations []model.RelationID, limit int) (map[model.RelationID][]model.EvidenceID, error) {
	rows, err := f.EvidenceRows(ctx, relations, limit)
	if err != nil {
		return nil, err
	}
	out := make(map[model.RelationID][]model.EvidenceID, len(rows))
	for rel, list := range rows {
		for _, ev := range list {
			out[rel] = append(out[rel], ev.ID)
		}
	}
	return out, nil
}

func (f *callFixture) EvidenceRows(ctx context.Context, relations []model.RelationID, limit int) (map[model.RelationID][]model.Evidence, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	out := make(map[model.RelationID][]model.Evidence, len(relations))
	for _, id := range relations {
		ev := f.evidence[id]
		if l := fixtureEdgePageLimit(limit); len(ev) > l {
			ev = ev[:l]
		}
		if len(ev) > 0 {
			out[id] = append([]model.Evidence(nil), ev...)
		}
	}
	return out, nil
}

func (f *callFixture) Capabilities(ctx context.Context) ([]model.CapabilityState, error) {
	return nil, ctx.Err()
}

func (f *callFixture) Binding() model.Binding { return f.binding }

// locatedGraph is the in-heap reader with the store's locating seam over the
// fixture's stored ranges.
type locatedGraph struct {
	*MemoryGraph
	f *callFixture
}

var _ SourceLocator = (*locatedGraph)(nil)

func (g *locatedGraph) NodeLocations(ctx context.Context, ids []model.NodeID) ([]model.NodeLocation, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	var out []model.NodeLocation
	for _, id := range ids {
		if loc, ok := g.f.located[id]; ok {
			out = append(out, loc)
		}
	}
	return out, nil
}

func (g *locatedGraph) RelationOccurrences(ctx context.Context, rel model.RelationID, after model.EvidenceID,
	limit int) ([]model.RelationOccurrence, model.EvidenceID, error) {
	if err := ctx.Err(); err != nil {
		return nil, "", err
	}
	limit = fixtureEdgePageLimit(limit)
	var page []model.Evidence
	for _, ev := range g.f.evidence[rel] {
		if ev.ID > after {
			page = append(page, ev)
		}
		if len(page) == limit {
			break
		}
	}
	out := make([]model.RelationOccurrence, 0, len(page))
	for _, ev := range page {
		if ev.Bytes == nil || ev.FileID == "" {
			continue
		}
		out = append(out, model.RelationOccurrence{RelationID: rel, EvidenceID: ev.ID, FileID: ev.FileID,
			Bytes: *ev.Bytes, Dispatch: ev.Dispatch, SetSize: ev.SetSize})
	}
	var next model.EvidenceID
	if len(page) == limit {
		next = page[len(page)-1].ID
	}
	return out, next, nil
}
