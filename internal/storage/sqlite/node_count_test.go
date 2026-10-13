package sqlite_test

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Sawmonabo/codectx/internal/model"
	store "github.com/Sawmonabo/codectx/internal/storage/sqlite"
)

// TestCountNodesCountsWhatEveryPageOfNodesServes holds the one-statement count
// to the walk it replaces: for each filter shape, CountNodes equals the number
// of distinct nodes a full keyset walk of Nodes serves. The fixture has a node
// two units both publish, a return node carrying the same name, and a unit of
// another generation publishing the name, and the count must see none of them
// twice or at all. The name lookup must also be served by the name index.
//
// Mutations it catches: counting fact rows rather than distinct node ids (the
// node two units publish counts twice); dropping the non-symbol kind exclusion
// (the return node counts); dropping the generation's visibility join (the
// other generation's node counts); a predicate the name index cannot serve (a
// scan of every fact).
func TestCountNodesCountsWhatEveryPageOfNodesServes(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "codectx.db")
	f := newFixture(t, dbPath)
	a := f.file("pkg/a.go", "package pkg\n")
	snap := f.snapshot("count", a)
	gen, err := f.s.BeginGeneration(f.ctx, f.repo, snap.ID, model.H("semantic"), "main")
	if err != nil {
		t.Fatal(err)
	}
	run := f.run(gen)
	publish := func(w *store.UnitWriter, facts []model.NodeFact) {
		t.Helper()
		if err := w.PutNodes(f.ctx, facts); err != nil {
			t.Fatalf("PutNodes: %v", err)
		}
		if err := f.s.SealUnit(f.ctx, w); err != nil {
			t.Fatalf("SealUnit: %v", err)
		}
	}
	first := f.beginScopeKey(gen, run, "cfg", "file:"+a.path, a)
	var facts []model.NodeFact
	for _, scope := range []string{"s0", "s1", "s2", "s3", "s4"} {
		facts = append(facts, f.nodeFact(first, run, scope, "F", &a))
	}
	ret := f.nodeFact(first, run, "s9", "F", &a)
	ret.Node.Kind = model.NodeReturn
	ret.Node.ID = model.NewNodeID(f.repo, model.NodeReturn, ret.CanonicalKey)
	ret.Evidence[0].NodeID = ret.Node.ID
	ret.Evidence[0].ID = model.NewEvidenceID(ret.Evidence[0])
	publish(first, append(facts, ret))
	second := f.beginScopeKey(gen, run, "cfg", "second", a)
	publish(second, []model.NodeFact{f.nodeFact(second, run, "s0", "F", &a), f.nodeFact(second, run, "s5", "F", &a)})
	f.activate(gen, 0)

	other, err := f.s.BeginGeneration(f.ctx, f.repo, snap.ID, model.H("semantic-other"), "other")
	if err != nil {
		t.Fatal(err)
	}
	otherRun := f.run(other)
	outsider := f.beginScopeKey(other, otherRun, "cfg-other", "outsider", a)
	publish(outsider, []model.NodeFact{f.nodeFact(outsider, otherRun, "s7", "F", &a)})

	r, err := f.s.PinGeneration(f.ctx, f.repo, gen, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	for _, filter := range []store.NodeFilter{{Name: "F"}, {QualifiedName: "s0.F"}, {QualifiedPrefix: "s"},
		{Name: "F", Kinds: []model.NodeKind{model.NodeReturn}}} {
		walked := map[model.NodeID]bool{}
		after := model.NodeID("")
		for {
			page, err := r.Nodes(f.ctx, filter, after, 2)
			if err != nil {
				t.Fatalf("Nodes(%+v): %v", filter, err)
			}
			if len(page) == 0 {
				break
			}
			for _, n := range page {
				walked[n.Node.ID] = true
			}
			after = page[len(page)-1].Node.ID
		}
		got, err := r.CountNodes(f.ctx, filter)
		if err != nil {
			t.Fatalf("CountNodes(%+v): %v", filter, err)
		}
		if got != int64(len(walked)) {
			t.Errorf("CountNodes(%+v) = %d, want the %d nodes the walk serves", filter, got, len(walked))
		}
	}
	if n, _ := r.CountNodes(f.ctx, store.NodeFilter{Name: "F"}); n != 6 {
		t.Errorf("CountNodes(name F) = %d, want the 6 visible F symbols (s0 to s5)", n)
	}

	db := openRawDB(t, dbPath)
	if _, err := db.Exec(`ANALYZE`); err != nil {
		t.Fatalf("ANALYZE: %v", err)
	}
	query, args, err := store.CountNodesStatement(r, store.NodeFilter{Name: "F"})
	if err != nil {
		t.Fatal(err)
	}
	if plan := explain(t, db, query, args...); !strings.Contains(plan, "idx_nodes_name") {
		t.Errorf("the count of a name does not read the name index:\n%s", plan)
	}
}
