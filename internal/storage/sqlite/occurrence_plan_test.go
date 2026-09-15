package sqlite

import (
	"context"
	"database/sql"
	"strings"
	"testing"
)

// TestOccurrenceQueryPlanIsPinned holds the two properties occurrenceQuery is
// written for, which nothing else in the suite can observe: search_vocab is the
// OUTER loop, so a term's instances arrive in the doclist order OccurrenceStream
// refills against, and the plan materialises nothing, so peak memory is one
// refill and not one term's whole instance list.
//
// Both are properties of the PLAN, not of any answer, so a schema change that
// silently costs the pin -- losing idx_search_doc, or joining search_units on a
// column the planner would rather drive from -- passes every other test in this
// package while turning a corpus-frequent term back into an unbounded sort.
func TestOccurrenceQueryPlanIsPinned(t *testing.T) {
	db, err := sql.Open("sqlite", "file:"+t.TempDir()+"/plan.db")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()
	ctx := context.Background()
	if _, err := db.ExecContext(ctx, schemaSQL); err != nil {
		t.Fatalf("schema: %v", err)
	}
	r := &PinnedReader{}
	rows, err := db.QueryContext(ctx, "EXPLAIN QUERY PLAN "+occurrenceQuery+r.visibleDocument("su"), 1, "term")
	if err != nil {
		t.Fatalf("explain: %v", err)
	}
	defer rows.Close()
	var steps []string
	for rows.Next() {
		var id, parent, notused int
		var detail string
		if err := rows.Scan(&id, &parent, &notused, &detail); err != nil {
			t.Fatalf("scan: %v", err)
		}
		steps = append(steps, detail)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	plan := strings.Join(steps, "\n")
	if len(steps) == 0 || !strings.HasPrefix(steps[0], "SCAN v ") {
		t.Errorf("search_vocab is not the outer loop; the emission order is no longer the scan order:\n%s", plan)
	}
	if !strings.Contains(plan, "SEARCH su USING INDEX idx_search_doc") {
		t.Errorf("search_units is not resolved by idx_search_doc:\n%s", plan)
	}
	if strings.Contains(strings.ToUpper(plan), "TEMP B-TREE") {
		t.Errorf("the plan materialises a temp b-tree; peak memory is no longer one refill:\n%s", plan)
	}
}
