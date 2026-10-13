package sqlite

import (
	"strings"
	"testing"
)

// TestCollectionFindsSessionsByIndex proves the two questions collection asks
// of read_sessions are index lookups rather than scans of every session: the
// snapshot delete's NOT EXISTS per candidate snapshot, and DeleteGeneration's
// count of the sessions naming the generation. The statements are the ones
// gc.go runs.
//
// mutation: dropping idx_read_sessions_snapshot or
// idx_read_sessions_generation turns its statement's read_sessions step into
// a SCAN, which fails here.
func TestCollectionFindsSessionsByIndex(t *testing.T) {
	_, _, tx := internFixture(t)
	for _, q := range []struct {
		name, query, index string
	}{
		{"snapshot delete", `DELETE FROM snapshots WHERE id = x'00' AND NOT EXISTS (SELECT 1 FROM generations g WHERE g.snapshot_id = snapshots.id)
			AND NOT EXISTS (SELECT 1 FROM retention_leases l WHERE l.snapshot_id = snapshots.id)
			AND NOT EXISTS (SELECT 1 FROM read_sessions rs WHERE rs.snapshot_id = snapshots.id)`, "idx_read_sessions_snapshot"},
		{"snapshot sweep", `DELETE FROM snapshots WHERE NOT EXISTS (SELECT 1 FROM generations g WHERE g.snapshot_id = snapshots.id)
			AND NOT EXISTS (SELECT 1 FROM retention_leases l WHERE l.snapshot_id = snapshots.id)
			AND NOT EXISTS (SELECT 1 FROM read_sessions rs WHERE rs.snapshot_id = snapshots.id)`, "idx_read_sessions_snapshot"},
		{"generation sessions", `SELECT count(*) FROM read_sessions WHERE generation_id = 1`, "idx_read_sessions_generation"},
	} {
		rows, err := tx.Query("EXPLAIN QUERY PLAN " + q.query)
		if err != nil {
			t.Fatalf("%s: EXPLAIN QUERY PLAN: %v", q.name, err)
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
		var found bool
		for _, step := range steps {
			if !strings.Contains(step, "read_sessions") && !strings.Contains(step, " rs ") && !strings.HasSuffix(step, " rs") {
				continue
			}
			if !strings.HasPrefix(step, "SEARCH") || !strings.Contains(step, q.index) {
				t.Errorf("%s: read_sessions step %q is not a search of %s; plan:\n%s", q.name, step, q.index, strings.Join(steps, "\n"))
			}
			found = true
		}
		if !found {
			t.Fatalf("%s: the plan names no read_sessions step:\n%s", q.name, strings.Join(steps, "\n"))
		}
	}
}
