package sqlite_test

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
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
// precedence winner among the node's rows in the tier), and each page is a
// seek into the tier's index that reads its own rows: a page deep in the tier
// touches about as much of the database as the first page does. The fixture is
// 4,000 nodes published by three units each (12,000 rows in each tier), beside
// as many rows of other names, with the statistics an activation writes.
//
// Mutations it catches:
//   - drop the NOT EXISTS (every row of a node is served: 6,000, not 2,000);
//   - flip its precedence comparison (a node is served with a losing unit's
//     row, not the row Nodes serves);
//   - page by node id (ORDER BY ni.canonical): the index keyset then skips
//     and repeats nodes (600 served of 4,000 here), and the plan sorts the
//     whole tier in a temporary b-tree on every page;
//   - a keyset the planner only filters with, such as the three-column row
//     value beside the equality or beside the prefix's lower bound: the deep
//     page reads the tier's index from its start, about a hundred more
//     b-tree pages than the first page reads;
//   - plain JOINs instead of CROSS JOIN, in WalkTier's statement or in
//     Nodes': with statistics the planner starts from the generation's units
//     and reads every fact they hold, not from the tier index.
func TestWalkTierReadsEachPageFromItsOwnPosition(t *testing.T) {
	const nodes, units, page = 4000, 3, 200
	dbPath := filepath.Join(t.TempDir(), "codectx.db")
	f := newFixture(t, dbPath)
	a := f.file("pkg/a.go", "package pkg\n")
	snap := f.snapshot("walk", a)
	gen, err := f.s.BeginGeneration(f.ctx, f.repo, snap.ID, model.H("semantic"), "main")
	if err != nil {
		t.Fatal(err)
	}
	run := f.run(gen)
	for u := range units {
		w := f.beginScopeKey(gen, run, "cfg", "unit"+strconv.Itoa(u), a)
		var facts []model.NodeFact
		for i := range nodes {
			facts = append(facts, f.nodeFact(w, run, fmt.Sprintf("pkg/p%05d", i), "String", nil),
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
		if err := r.WalkTier(f.ctx, tc.filter, page, func(rows []store.StoredNode) error {
			for _, n := range rows {
				served++
				got[n.Node.ID] = n.UnitID
			}
			return nil
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
		var key string
		var unit, node int64
		if err := db.QueryRow(`SELECT `+tc.column+`, unit_id, node_id FROM node_facts WHERE `+tc.column+` >= ? AND `+
			tc.column+` < ? ORDER BY 1, 2, 3 LIMIT 1 OFFSET ?`, tierLow(tc.filter), tierHigh(tc.filter),
			nodes*units-300).Scan(&key, &unit, &node); err != nil {
			t.Fatalf("deep entry: %v", err)
		}
		firstQ, firstArgs, err := store.TierPageStatement(r, tc.filter, "", 0, 0, page)
		if err != nil {
			t.Fatal(err)
		}
		deepQ, deepArgs, err := store.TierPageStatement(r, tc.filter, key, unit, node, page)
		if err != nil {
			t.Fatal(err)
		}
		for _, q := range []struct {
			name  string
			query string
			args  []any
		}{{"first", firstQ, firstArgs}, {"deep", deepQ, deepArgs}} {
			plan := explain(t, db, q.query, q.args...)
			if strings.Contains(plan, "TEMP B-TREE") || !strings.HasPrefix(plan, "SEARCH nf USING INDEX "+tc.index) {
				t.Errorf("%+v %s page does not read %s in its order:\n%s", tc.filter, q.name, tc.index, plan)
			}
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
		t.Logf("%+v: b-tree pages touched by the first page %d, by a page 300 rows from the end %d", tc.filter, first, deep)
		// The first and the deep page probe the same number of rows for their
		// winners; what may differ is only where the index read starts.
		if deep > first+30 {
			t.Errorf("%+v: a page deep in the tier touched %d b-tree pages, the first page %d: the page reads the tier "+
				"before its position", tc.filter, deep, first)
		}
	}
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
