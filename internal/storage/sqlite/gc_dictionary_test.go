package sqlite_test

import (
	"database/sql"
	"path/filepath"
	"testing"
	"time"
)

// TestNativeKeySweepClearsTheWholeBacklog pins the property the two child-column
// indexes exist for: one collection run drains the ENTIRE unreferenced native-key
// dictionary, not a prefix of it. Before those indexes the sweep carried a
// per-run delete budget derived from the child-table sizes, because each deletion
// cost a full scan of native_aliases and of evidence to enforce the foreign keys;
// at any real corpus size that budget cleared a few hundred keys per run against a
// backlog of hundreds of thousands, so the dictionary grew faster than it drained.
//
// The backlog here is deliberately larger than one keyset batch, so the assertion
// fails both if the pass stops after its first batch and if a budget were ever
// reintroduced at a scale that binds. The referenced key is the other half of the
// invariant: a sweep that cleared everything would also be "converging".
func TestNativeKeySweepClearsTheWholeBacklog(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "codectx.db")
	f := newFixture(t, dbPath)

	raw, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()

	// 1 000 dictionary rows no alias and no evidence row names: five keyset
	// batches at the collector's batch size.
	const backlog = 1000
	if _, err := raw.Exec(`WITH RECURSIVE n(i) AS (SELECT 1 UNION ALL SELECT i + 1 FROM n WHERE i < ?)
		INSERT INTO native_keys(key) SELECT 'unreferenced:' || i FROM n`, backlog); err != nil {
		t.Fatalf("seed the backlog: %v", err)
	}
	// One live key, referenced the way evidence and aliases reference it: by
	// surrogate. The alias row is seeded with foreign keys off, so the live key
	// is kept by the collector's probes alone and by nothing else in the store.
	var live int64
	if err := raw.QueryRow(`INSERT INTO native_keys(key) VALUES('referenced') RETURNING id`).Scan(&live); err != nil {
		t.Fatalf("seed the live key: %v", err)
	}
	if _, err := raw.Exec(`PRAGMA foreign_keys = off`); err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(`INSERT INTO native_aliases(unit_id, scope_key_id, native_key_id, node_id)
		VALUES(-1, -1, ?, -1)`, live); err != nil {
		t.Fatalf("seed the alias: %v", err)
	}

	if err := f.s.Recover(f.ctx, time.Now()); err != nil {
		t.Fatalf("Recover: %v", err)
	}

	var remaining int64
	if err := raw.QueryRow(`SELECT count(*) FROM native_keys WHERE key LIKE 'unreferenced:%'`).Scan(&remaining); err != nil {
		t.Fatal(err)
	}
	if remaining != 0 {
		t.Fatalf("one collection run left %d of %d unreferenced native keys; the sweep must drain the whole backlog, not a prefix", remaining, backlog)
	}
	var kept int64
	if err := raw.QueryRow(`SELECT count(*) FROM native_keys WHERE id = ?`, live).Scan(&kept); err != nil {
		t.Fatal(err)
	}
	if kept != 1 {
		t.Fatal("the sweep deleted a native key an alias row still names")
	}
}
