package sqlite

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
)

// After an out-of-memory, disk-full, I/O or interrupt error SQLite may roll
// the whole transaction back on its own, taking the ingest savepoint with it.
// The ingestion job then reports the job's own error and that SQLite rolled
// the group back, never a failed rollback ("no such savepoint"), abandons the
// group, and the next job opens a new one. fn's own ROLLBACK stands in for
// SQLite's: it leaves the connection in the same state.
//
// Mutation caught: the savepoint failure reported as such (the message names
// a missing savepoint and hides what happened to the group).
func TestATransactionSQLiteRolledBackIsReportedAsSuch(t *testing.T) {
	ctx := context.Background()
	s, err := Open(ctx, filepath.Join(t.TempDir(), "codectx.db"), WithDerivedReaders(Options{}))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer s.Close()
	err = s.ingest(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `ROLLBACK`); err != nil {
			t.Fatalf("roll the transaction back: %v", err)
		}
		return internal("tmp_graph_rel: out of memory (7)")
	})
	if err == nil {
		t.Fatal("the job's error was lost")
	}
	msg := err.Error()
	if !strings.Contains(msg, "tmp_graph_rel: out of memory (7)") ||
		!strings.Contains(msg, "SQLite rolled back the ingestion group's transaction") ||
		strings.Contains(msg, "no such savepoint") {
		t.Fatalf("error %q, want the job's error and that SQLite rolled the group back", msg)
	}
	if err := s.ingest(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `SELECT 1`)
		return err
	}); err != nil {
		t.Fatalf("the next job after an abandoned group: %v", err)
	}
}
