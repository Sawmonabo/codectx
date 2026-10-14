package sqlite_test

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Sawmonabo/codectx/internal/model"
	store "github.com/Sawmonabo/codectx/internal/storage/sqlite"
	sqlitedrv "modernc.org/sqlite"
)

// TestWalkTierReadsEachPageFromItsOwnPosition pins the exact tiers' walk (and
// the tier index as the driver of Nodes' statement too): it
// serves every node of a tier once, as the row Nodes serves for it (the
// precedence winner among the node's rows in the tier), and each window is a
// seek into the tier's index, read from the index alone, that reads its own
// entries: a window deep in the tier touches about as much of the database as
// the first does. The fixture is 4,000 nodes in 40 files, published by three
// units each (12,000 rows in each tier), beside as many rows of other names,
// with the statistics an activation writes. An equality tier is also read in
// content order from a span (TierSpan), the seek a resumed exact page starts
// with.
//
// Mutations it catches:
//   - keep every visible entry instead of the winner of its node (every row
//     of a node is served: 12,000, not 4,000);
//   - flip unitRank.precedes (a node is served with a losing unit's row, not
//     the row Nodes serves);
//   - order the window by node id (ORDER BY nf.node_id): the keyset then
//     skips and repeats nodes, and the plan sorts the tier in a temporary
//     b-tree;
//   - a keyset the planner only filters with, such as a row value that
//     repeats the equality column beside the equality: the deep window reads
//     the tier from its start, several times the first window's b-tree pages;
//   - start a walk from a span with a filter on the span instead of a seek,
//     or drop the span (the walk from pkg/f20.go starts at pkg/f00.go);
//   - a window that reads a column the tier index does not hold (or sort
//     columns that are generated, which SQLite counts as reading every
//     column): the plan is no longer a COVERING INDEX read;
//   - a probe that reaches the node's rows through the tier index (drop the
//     unary +): the plan no longer seeks idx_node_facts_id by node;
//   - a page that ends after one window that held fewer served nodes than
//     its bound (losing units' rows, or the kind filter dropping winners):
//     the walk ends after its first, short page;
//   - plain JOINs instead of CROSS JOIN in Nodes': with statistics the
//     planner starts from the generation's units and reads every fact they
//     hold, not from the tier index.
func TestWalkTierReadsEachPageFromItsOwnPosition(t *testing.T) {
	const nodes, units, page = 4000, 3, 200
	dbPath := filepath.Join(t.TempDir(), "codectx.db")
	f := newFixture(t, dbPath)
	var files []fileFixture
	for i := range 40 {
		files = append(files, f.file(fmt.Sprintf("pkg/f%02d.go", i), fmt.Sprintf("package pkg // %d\n", i)))
	}
	snap := f.snapshot("walk", files...)
	gen, err := f.s.BeginGeneration(f.ctx, f.repo, snap.ID, model.H("semantic"), "main")
	if err != nil {
		t.Fatal(err)
	}
	run := f.run(gen)
	for u := range units {
		w := f.beginScopeKey(gen, run, "cfg", "unit"+strconv.Itoa(u), files...)
		var facts []model.NodeFact
		for i := range nodes {
			fact := f.nodeFact(w, run, fmt.Sprintf("pkg/p%05d", i), "String", &files[i%len(files)])
			if i%3 == 0 {
				fact = asKind(f, fact, model.NodeMethod)
			}
			facts = append(facts, fact,
				f.nodeFact(w, run, fmt.Sprintf("other/o%05d", i), "N"+strconv.Itoa(i), nil))
			if len(facts) == 500 {
				if err := w.PutNodes(f.ctx, facts); err != nil {
					t.Fatalf("PutNodes: %v", err)
				}
				facts = facts[:0]
			}
		}
		if err := w.PutNodes(f.ctx, facts); err != nil {
			t.Fatalf("PutNodes: %v", err)
		}
		if err := f.s.SealUnit(f.ctx, w); err != nil {
			t.Fatalf("SealUnit: %v", err)
		}
	}
	f.activate(gen, 0)
	r, err := f.s.PinGeneration(f.ctx, f.repo, gen, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	db := openRawDB(t, dbPath)
	var stats int
	if err := db.QueryRow(`SELECT count(*) FROM sqlite_stat1`).Scan(&stats); err != nil || stats == 0 {
		t.Fatalf("the activation left %d sqlite_stat1 rows (%v): the plans below need statistics", stats, err)
	}

	tiers := []struct {
		filter store.NodeFilter
		index  string
		column string
	}{
		{store.NodeFilter{QualifiedPrefix: "pkg/"}, "idx_nodes_qname", "qualified_name"},
		{store.NodeFilter{Name: "String"}, "idx_nodes_name", "name"},
	}
	for _, tc := range tiers {
		want := map[model.NodeID]model.UnitID{}
		var after model.NodeID
		for {
			rows, err := r.Nodes(f.ctx, tc.filter, after, page)
			if err != nil {
				t.Fatalf("Nodes(%+v): %v", tc.filter, err)
			}
			if len(rows) == 0 {
				break
			}
			for _, n := range rows {
				want[n.Node.ID] = n.UnitID
			}
			after = rows[len(rows)-1].Node.ID
		}
		got := map[model.NodeID]model.UnitID{}
		served := 0
		if err := r.WalkTier(f.ctx, tc.filter, nil, page, func(rows []store.StoredNode) (int, error) {
			for _, n := range rows {
				served++
				got[n.Node.ID] = n.UnitID
			}
			return 0, nil
		}); err != nil {
			t.Fatalf("WalkTier(%+v): %v", tc.filter, err)
		}
		if served != nodes || len(got) != nodes || len(want) != nodes {
			t.Fatalf("%+v: the walk served %d rows for %d nodes; Nodes lists %d; want each of %d nodes once",
				tc.filter, served, len(got), len(want), nodes)
		}
		for id, unit := range want {
			if got[id] != unit {
				t.Fatalf("%+v: node %s is served with unit %s's row, Nodes serves unit %s's", tc.filter, id, got[id], unit)
			}
		}

		// The entry 300 rows before the tier's end, in the index's order.
		var key, path string
		var start, end, unit, node int64
		if err := db.QueryRow(`SELECT `+tc.column+`, path, sort_start, sort_end, unit_id, node_id FROM node_facts
			WHERE `+tc.column+` >= ? AND `+tc.column+` < ? ORDER BY 1, 2, 3, 4, 5, 6 LIMIT 1 OFFSET ?`,
			tierLow(tc.filter), tierHigh(tc.filter), nodes*units-300).Scan(&key, &path, &start, &end, &unit,
			&node); err != nil {
			t.Fatalf("deep entry: %v", err)
		}
		firstQ, firstArgs, err := store.TierWindowStatement(r, tc.filter, nil, nil, page)
		if err != nil {
			t.Fatal(err)
		}
		deepQ, deepArgs, err := store.TierWindowStatement(r, tc.filter, nil, []any{key, path, start, end, unit, node}, page)
		if err != nil {
			t.Fatal(err)
		}
		queries := []struct {
			name  string
			query string
			args  []any
		}{{"first", firstQ, firstArgs}, {"deep", deepQ, deepArgs}}
		var spanQ string
		var spanArgs []any
		if tc.filter.QualifiedPrefix == "" {
			// The last file's span: a seek reads only that file's rows.
			if spanQ, spanArgs, err = store.TierWindowStatement(r, tc.filter, &store.TierSpan{Path: "pkg/f39.go"}, nil,
				page); err != nil {
				t.Fatal(err)
			}
			queries = append(queries, struct {
				name  string
				query string
				args  []any
			}{"span", spanQ, spanArgs})
		}
		for _, q := range queries {
			plan := explain(t, db, q.query, q.args...)
			if strings.Contains(plan, "TEMP B-TREE") || !strings.HasPrefix(plan, "SEARCH nf USING COVERING INDEX "+tc.index) {
				t.Errorf("%+v %s window does not read %s alone, in its order:\n%s", tc.filter, q.name, tc.index, plan)
			}
		}
		probeQ, probeArgs, err := store.TierProbeStatement(r, tc.filter, "[1,2,3]")
		if err != nil {
			t.Fatal(err)
		}
		if plan := explain(t, db, probeQ, probeArgs...); !strings.Contains(plan, "SEARCH nf USING INDEX idx_node_facts_id (node_id=?)") {
			t.Errorf("%+v: the probe of a window's nodes does not seek idx_node_facts_id by node:\n%s", tc.filter, plan)
		}
		// Nodes, which pages by node id, must sort the tier, but it too reads
		// the tier through its index rather than the generation's units.
		nodesQ, nodesArgs, err := store.NodesStatement(r, tc.filter, string(after), page)
		if err != nil {
			t.Fatal(err)
		}
		if plan := explain(t, db, nodesQ, nodesArgs...); !strings.HasPrefix(plan, "SEARCH nf USING INDEX "+tc.index) {
			t.Errorf("%+v: Nodes does not start from %s:\n%s", tc.filter, tc.index, plan)
		}
		first := pagesTouched(t, f.ctx, db, firstQ, firstArgs...)
		deep := pagesTouched(t, f.ctx, db, deepQ, deepArgs...)
		t.Logf("%+v: b-tree pages touched by the first window %d, by a window 300 rows from the end %d", tc.filter, first, deep)
		if spanQ != "" {
			if at := pagesTouched(t, f.ctx, db, spanQ, spanArgs...); at > first+30 {
				t.Errorf("%+v: a window from the last file's span touched %d b-tree pages, the first window %d: the "+
					"span is filtered, not sought", tc.filter, at, first)
			}
		}
		// The first and the deep window read the same number of entries; what
		// may differ is only where the index read starts.
		if deep > first+30 {
			t.Errorf("%+v: a window deep in the tier touched %d b-tree pages, the first window %d: the window reads "+
				"the tier before its position", tc.filter, deep, first)
		}
	}

	// An equality tier is in content order, and a walk from a span starts at
	// the first row whose span is not before it: from pkg/f20.go, the 2,000
	// nodes of files f20 to f39, each once, in path order.
	from := &store.TierSpan{Path: "pkg/f20.go"}
	var paths []string
	served := 0
	if err := r.WalkTier(f.ctx, store.NodeFilter{Name: "String"}, from, page, func(rows []store.StoredNode) (int, error) {
		for _, n := range rows {
			served++
			paths = append(paths, string(n.Node.FileID))
		}
		// Pages after the first ask for 37 rows: the walk still serves the
		// tier whole, at whatever bound the caller asks for next.
		return 37, nil
	}); err != nil {
		t.Fatalf("WalkTier(from %+v): %v", from, err)
	}
	pathOf := map[string]string{}
	for _, ff := range files {
		pathOf[string(ff.id)] = ff.path
	}
	for i, id := range paths {
		paths[i] = pathOf[id]
	}
	if served != nodes/2 || paths[0] != "pkg/f20.go" || !slices.IsSorted(paths) {
		t.Errorf("the walk from %s served %d nodes from %s to %s (sorted %v); want %d from pkg/f20.go, in path order",
			from.Path, served, paths[0], paths[len(paths)-1], slices.IsSorted(paths), nodes/2)
	}

	// The kind filter is applied to a window's winners, so a window can
	// serve fewer nodes than it holds winners; the page reads further windows
	// rather than ending the tier short. Every third node is a method.
	methods := 0
	pages := 0
	if err := r.WalkTier(f.ctx, store.NodeFilter{Name: "String", Kinds: []model.NodeKind{model.NodeMethod}}, nil, page,
		func(rows []store.StoredNode) (int, error) {
			pages++
			for _, n := range rows {
				if n.Node.Kind != model.NodeMethod {
					return 0, fmt.Errorf("the method walk served a %s", n.Node.Kind)
				}
				methods++
			}
			return 0, nil
		}); err != nil {
		t.Fatalf("WalkTier(methods): %v", err)
	}
	if want := (nodes + 2) / 3; methods != want || pages != (want+page-1)/page {
		t.Errorf("the method walk served %d nodes in %d pages; want %d in %d full pages", methods, pages, want,
			(want+page-1)/page)
	}
}

// asKind is fact as a node of kind: the node id folds the kind.
func asKind(f *fixture, fact model.NodeFact, kind model.NodeKind) model.NodeFact {
	fact.Node.Kind = kind
	fact.Node.ID = model.NewNodeID(f.repo, kind, fact.CanonicalKey)
	ev := fact.Evidence[0]
	ev.NodeID = fact.Node.ID
	ev.ID = model.NewEvidenceID(ev)
	fact.Evidence = []model.Evidence{ev}
	return fact
}

// tierLow and tierHigh bound the tier's index range for the fixture's deep
// entry lookup.
func tierLow(f store.NodeFilter) string {
	if f.Name != "" {
		return f.Name
	}
	return f.QualifiedPrefix
}

func tierHigh(f store.NodeFilter) string {
	if f.Name != "" {
		return f.Name + "\x00"
	}
	return strings.TrimSuffix(f.QualifiedPrefix, "/") + "0"
}

// pagesTouched runs query to its end on one connection and answers how many
// b-tree pages it read through the page cache (hits plus misses), the
// connection's own counters, zeroed first.
func pagesTouched(t *testing.T, ctx context.Context, db *sql.DB, query string, args ...any) int {
	t.Helper()
	conn, err := db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	counters := func() int {
		var total int
		if err := conn.Raw(func(dc any) error {
			s := dc.(sqlitedrv.DBStatus)
			for _, op := range []sqlitedrv.DBStatusOp{sqlitedrv.DBStatusCacheHit, sqlitedrv.DBStatusCacheMiss} {
				n, _, err := s.Status(op, true)
				if err != nil {
					return err
				}
				total += n
			}
			return nil
		}); err != nil {
			t.Fatalf("db status: %v", err)
		}
		return total
	}
	counters()
	rows, err := conn.QueryContext(ctx, query, args...)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	for rows.Next() {
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	rows.Close()
	return counters()
}
