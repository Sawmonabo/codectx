package sqlite_test

import (
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Sawmonabo/codectx/internal/model"
	store "github.com/Sawmonabo/codectx/internal/storage/sqlite"
)

// TestNodesAmongAnswersTheTierRowOfEachAskedNode pins the batched tier lookup
// the search tiers decide ownership with: for the ids asked, it answers the row
// a walk of the tier's keyset serves for each node the tier lists, and nothing
// for an id the tier does not list; and its statement is driven by the ids
// (one probe of node_ids' canonical index each), never by a scan of the tier.
//
// Mutations it catches: dropping the id restriction (every node of the tier
// comes back, not the asked ones); writing the joins as plain JOINs (the
// planner starts from the generation's units and reads every fact they hold,
// observed on this fixture after ANALYZE); dropping the unary + on the tier
// predicate (the facts may be reached through a name index, a scan of the tier).
func TestNodesAmongAnswersTheTierRowOfEachAskedNode(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "codectx.db")
	f := newFixture(t, dbPath)
	a := f.file("pkg/a.go", "package pkg\n")
	snap := f.snapshot("among", a)
	gen, err := f.s.BeginGeneration(f.ctx, f.repo, snap.ID, model.H("semantic"), "main")
	if err != nil {
		t.Fatal(err)
	}
	run := f.run(gen)
	w := f.beginScopeKey(gen, run, "cfg", "file:"+a.path, a)
	var facts []model.NodeFact
	for _, scope := range []string{"s0", "s1", "s2", "s3", "s4", "s5"} {
		facts = append(facts, f.nodeFact(w, run, scope, "F", &a))
	}
	facts = append(facts, f.nodeFact(w, run, "s6", "G", &a))
	if err := w.PutNodes(f.ctx, facts); err != nil {
		t.Fatalf("PutNodes: %v", err)
	}
	if err := f.s.SealUnit(f.ctx, w); err != nil {
		t.Fatalf("SealUnit: %v", err)
	}
	f.activate(gen, 0)
	r, err := f.s.PinGeneration(f.ctx, f.repo, gen, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()

	tier := store.NodeFilter{Name: "F"}
	walked, err := r.Nodes(f.ctx, tier, "", model.MaxPageItems)
	if err != nil || len(walked) != 6 {
		t.Fatalf("Nodes(name F) = %d rows, %v; want the 6 F nodes", len(walked), err)
	}
	// Ask for every other F node, the G node (not in the tier) and an id no
	// node has, out of order.
	other := facts[6].Node.ID
	absent := model.NodeID(model.H("no such node"))
	ask := []model.NodeID{walked[4].Node.ID, other, absent, walked[0].Node.ID, walked[2].Node.ID}
	got, err := r.NodesAmong(f.ctx, tier, ask)
	if err != nil {
		t.Fatalf("NodesAmong: %v", err)
	}
	want := []store.StoredNode{walked[0], walked[2], walked[4]}
	if len(got) != len(want) {
		t.Fatalf("NodesAmong answered %d rows, want %d (the asked nodes the tier lists)", len(got), len(want))
	}
	for i := range want {
		if got[i].Node.ID != want[i].Node.ID || got[i].Node.QualifiedName != want[i].Node.QualifiedName ||
			got[i].UnitID != want[i].UnitID {
			t.Errorf("row %d = %s %q, want the walk's row %s %q", i, got[i].Node.ID, got[i].Node.QualifiedName,
				want[i].Node.ID, want[i].Node.QualifiedName)
		}
	}
	if !slices.IsSortedFunc(got, func(x, y store.StoredNode) int { return strings.Compare(string(x.Node.ID), string(y.Node.ID)) }) {
		t.Errorf("NodesAmong is not in ascending node-id order")
	}

	db := openRawDB(t, dbPath)
	if _, err := db.Exec(`ANALYZE`); err != nil {
		t.Fatalf("ANALYZE: %v", err)
	}
	for _, filter := range []store.NodeFilter{{Name: "F"}, {QualifiedName: "s1.F"}, {QualifiedPrefix: "s"}} {
		query, args, err := store.NodesAmongStatement(r, filter, ask)
		if err != nil {
			t.Fatal(err)
		}
		plan := explain(t, db, query, args...)
		t.Logf("%+v plan:\n%s", filter, plan)
		if strings.Contains(plan, "idx_nodes_name") || strings.Contains(plan, "idx_nodes_qname") {
			t.Errorf("the lookup of %+v reads a name index, a scan of the whole tier:\n%s", filter, plan)
		}
		lines := strings.Split(plan, "\n")
		if !strings.HasPrefix(lines[0], "SEARCH ni USING") || !strings.Contains(lines[0], "canonical=?") {
			t.Errorf("the lookup of %+v is not driven by the asked ids:\n%s", filter, plan)
		}
		if len(lines) < 2 || !strings.Contains(lines[1], "idx_node_facts_id") {
			t.Errorf("the lookup of %+v does not reach the facts by node id:\n%s", filter, plan)
		}
	}
}
