package graph

import (
	"context"
	"strconv"
	"testing"
)

// TestAScratchPreparesEachStatementOnce guards the scratch's statement reuse:
// a search runs a fixed set of statement texts once per edge and per label, so
// the scratch prepares each text once for its transaction and reuses it, and
// every call still sees the writes before it.
//
// Mutation: preparing per call (or not caching the statement) prepares 301
// statements here instead of 3 and fails the count.
func TestAScratchPreparesEachStatementOnce(t *testing.T) {
	ctx := context.Background()
	sc, err := openPathScratch(ctx, "")
	if err != nil {
		t.Fatalf("openPathScratch: %v", err)
	}
	defer sc.close()
	const n = 100
	for i := range n {
		node := "n" + strconv.Itoa(i)
		if err := sc.exec(ctx, `INSERT INTO settled(node, dist, depth) VALUES(?, ?, ?)`, node, i, i%7); err != nil {
			t.Fatalf("exec %d: %v", i, err)
		}
		var dist int
		found, err := sc.row(ctx, `SELECT dist FROM settled WHERE node = ?`, []any{node}, &dist)
		if err != nil || !found || dist != i {
			t.Fatalf("row %d = %d, %v, %v; want the row just written", i, dist, found, err)
		}
		var count int
		if err := sc.each(ctx, `SELECT COUNT(*) FROM settled`, nil, func(scan func(...any) error) error {
			return scan(&count)
		}); err != nil || count != i+1 {
			t.Fatalf("each %d counted %d, %v; want %d", i, count, err, i+1)
		}
	}
	if sc.prepared != 3 {
		t.Fatalf("%d calls of 3 statement texts prepared %d statements, want 3", 3*n, sc.prepared)
	}
}
