package sqlite

import (
	"strings"
	"testing"
)

// TestSealVisibilityChecksSeekEachEndpointByNode guards the cost of sealing a
// unit with a wide dependency closure. Every relation endpoint and alias
// target the seal checks must be one seek of idx_node_facts_id on node_id,
// with the closure applied as a membership test. A plan that seeks (node_id,
// unit_id) instead walks the closure list once per endpoint, so a unit that
// depends on every file unit of a repository seals in time quadratic in the
// repository. The statements are SealUnit's own (sealChecks).
//
// mutation: dropping the unary plus from `+nf.unit_id IN (SELECT id FROM
// closure)` in either visibility check turns its node_facts step into a seek on
// (node_id=? AND unit_id=?), which fails here.
func TestSealVisibilityChecksSeekEachEndpointByNode(t *testing.T) {
	_, _, tx := internFixture(t)
	checked := 0
	for _, c := range sealChecks {
		if !strings.Contains(c.query, "FROM closure") || !strings.Contains(c.query, "node_facts nf") {
			continue
		}
		checked++
		rows, err := tx.Query("EXPLAIN QUERY PLAN "+c.query, int64(1))
		if err != nil {
			t.Fatalf("%s: EXPLAIN QUERY PLAN: %v", c.what, err)
		}
		var steps []string
		for rows.Next() {
			var id, parent, notused int
			var detail string
			if err := rows.Scan(&id, &parent, &notused, &detail); err != nil {
				t.Fatal(err)
			}
			steps = append(steps, detail)
		}
		rows.Close()
		found := false
		for _, step := range steps {
			if !strings.Contains(step, " nf ") {
				continue
			}
			found = true
			if !strings.HasSuffix(step, "idx_node_facts_id (node_id=?)") {
				t.Errorf("%s: node_facts step %q is not a seek on node_id alone; plan:\n%s",
					c.what, step, strings.Join(steps, "\n"))
			}
		}
		if !found {
			t.Fatalf("%s: the plan names no node_facts step:\n%s", c.what, strings.Join(steps, "\n"))
		}
	}
	if checked != 2 {
		t.Fatalf("expected the two visibility checks (relation endpoints, alias targets), found %d", checked)
	}
}
