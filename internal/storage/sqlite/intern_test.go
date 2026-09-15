package sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/Sawmonabo/codectx/internal/model"
)

// decodeForTest turns a public hex identifier into the raw 32 bytes the
// canonical_key column stores.
func decodeForTest(t *testing.T, id string) []byte {
	t.Helper()
	raw, err := model.DecodeID(id)
	if err != nil {
		t.Fatalf("decode %q: %v", id, err)
	}
	return raw
}

// internFixture opens a real store and hands the test a write transaction, the
// only context in which the interner contract (ids.go) permits a call.
func internFixture(t *testing.T) (context.Context, *Store, *sql.Tx) {
	t.Helper()
	ctx := context.Background()
	s, err := Open(ctx, filepath.Join(t.TempDir(), "intern.db"), Options{})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	tx, err := s.writer.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	t.Cleanup(func() { _ = tx.Rollback() })
	return ctx, s, tx
}

// TestInternerCacheStaysBoundedAndIDsSurviveReset is the scale-posture proof:
// the interner's heap footprint is a function of its cache capacity and never
// of how many distinct strings the repository holds, and flushing the cache at
// a batch boundary costs nothing but a lookup -- every id is resolved back
// identically from the database. Mutating refCache.put to skip eviction (an
// unbounded, whole-repository dictionary) fails the first assertion.
func TestInternerCacheStaysBoundedAndIDsSurviveReset(t *testing.T) {
	ctx, s, tx := internFixture(t)
	in := newInterner(s.opts)
	capacity := in.strings.capacity
	const over = 4 // intern well past capacity
	total := capacity * over

	first := make(map[string]int64, total)
	for i := range total {
		key := fmt.Sprintf("native-key-%06d", i)
		ref, err := in.nativeKey(ctx, tx, key)
		if err != nil {
			t.Fatalf("nativeKey(%q): %v", key, err)
		}
		if got := in.strings.len(); got > capacity {
			t.Fatalf("after interning %d distinct keys the cache holds %d entries, capacity is %d: "+
				"the interner is a whole-repository dictionary, not a bounded LRU", i+1, got, capacity)
		}
		first[key] = int64(ref)
	}
	if in.strings.len() != capacity {
		t.Fatalf("cache holds %d entries after %d distinct keys, want it saturated at %d", in.strings.len(), total, capacity)
	}

	// A batch boundary drops every cached entry; the ids must not move.
	in.reset()
	if in.strings.len() != 0 {
		t.Fatalf("reset left %d cached entries", in.strings.len())
	}
	for key, want := range first {
		ref, err := in.nativeKey(ctx, tx, key)
		if err != nil {
			t.Fatalf("nativeKey(%q) after reset: %v", key, err)
		}
		if int64(ref) != want {
			t.Fatalf("nativeKey(%q) resolved to %d after reset, was %d: ids are not stable across a cache flush", key, ref, want)
		}
	}

}

// TestInternerUpsertThenReadConverges proves the contract's concurrency clause
// without racing: a second interner (a second writer) that has never seen the
// key must read back the id the first one created rather than minting its own,
// and the two dictionaries must not collide despite sharing one cache.
func TestInternerUpsertThenReadConverges(t *testing.T) {
	ctx, s, tx := internFixture(t)
	a, b := newInterner(s.opts), newInterner(s.opts)

	scope, err := a.scopeKey(ctx, tx, "shared")
	if err != nil {
		t.Fatalf("scopeKey: %v", err)
	}
	native, err := a.nativeKey(ctx, tx, "shared")
	if err != nil {
		t.Fatalf("nativeKey: %v", err)
	}
	again, err := b.scopeKey(ctx, tx, "shared")
	if err != nil {
		t.Fatalf("scopeKey (second writer): %v", err)
	}
	if again != scope {
		t.Fatalf("second writer minted scope id %d for a key the first resolved to %d", again, scope)
	}
	// Both tables were handed the identical string through one shared cache.
	// A cache that ignored the table tag would return the scope id here.
	var scopeRows, nativeRows string
	if err := tx.QueryRowContext(ctx, `SELECT (SELECT key FROM scope_keys WHERE id = ?), (SELECT key FROM native_keys WHERE id = ?)`,
		int64(scope), int64(native)).Scan(&scopeRows, &nativeRows); err != nil {
		t.Fatalf("read back: %v", err)
	}
	if scopeRows != "shared" || nativeRows != "shared" {
		t.Fatalf("the shared cache crossed dictionaries: scope_keys[%d]=%q native_keys[%d]=%q", scope, scopeRows, native, nativeRows)
	}

	// The same for identities: a node resolved by one writer is read, not
	// re-inserted, by another, and a relation's endpoints are the surrogates.
	key := decodeForTest(t, model.H("node-key", "alpha"))
	id := model.NodeID(model.H("node-v1", "alpha"))
	from, err := a.node(ctx, tx, id, "function", key)
	if err != nil {
		t.Fatalf("node: %v", err)
	}
	if got, err := b.node(ctx, tx, id, "function", key); err != nil || got != from {
		t.Fatalf("node re-resolved to (%d, %v), want %d", got, err, from)
	}
	toKey := decodeForTest(t, model.H("node-key", "beta"))
	to, err := a.node(ctx, tx, model.NodeID(model.H("node-v1", "beta")), "function", toKey)
	if err != nil {
		t.Fatalf("node: %v", err)
	}
	relID := model.RelationID(model.H("relation-v1", "alpha", "calls", "beta"))
	rel, err := a.relation(ctx, tx, relID, from, "calls", to)
	if err != nil {
		t.Fatalf("relation: %v", err)
	}
	if got, err := b.relation(ctx, tx, relID, from, "calls", to); err != nil || got != rel {
		t.Fatalf("relation re-resolved to (%d, %v), want %d", got, err, rel)
	}
	var storedFrom, storedTo int64
	if err := tx.QueryRowContext(ctx, `SELECT from_node_id, to_node_id FROM relation_ids WHERE id = ?`, int64(rel)).Scan(&storedFrom, &storedTo); err != nil {
		t.Fatalf("read relation: %v", err)
	}
	if storedFrom != int64(from) || storedTo != int64(to) {
		t.Fatalf("relation endpoints stored as (%d, %d), want (%d, %d)", storedFrom, storedTo, from, to)
	}
}

// TestInternerRejectsMalformedInput holds the contract's last clause: a bad
// canonical id, an empty interned string or an unresolved endpoint is reported
// as an error, never a panic and never an opaque SQLite constraint failure.
func TestInternerRejectsMalformedInput(t *testing.T) {
	ctx, s, tx := internFixture(t)
	in := newInterner(s.opts)
	good := decodeForTest(t, model.H("node-key", "alpha"))
	id := model.NodeID(model.H("node-v1", "alpha"))

	if _, err := in.node(ctx, tx, id, "", good); err == nil {
		t.Fatal("node accepted an empty kind")
	}
	if _, err := in.node(ctx, tx, id, "function", good[:31]); err == nil {
		t.Fatal("node accepted a 31-byte canonical key")
	}
	if _, err := in.node(ctx, tx, "not-a-hex-id", "function", good); err == nil {
		t.Fatal("node accepted a canonical id that is not 64 hex characters")
	}
	if _, err := in.scopeKey(ctx, tx, ""); err == nil {
		t.Fatal("scopeKey accepted an empty key")
	}
	// An empty native key is valid: unlocated evidence carries no native key
	// and evidence.native_key_id is NOT NULL, so "" must intern to one id.
	if _, err := in.nativeKey(ctx, tx, ""); err != nil {
		t.Fatalf("nativeKey refused the empty key that unlocated evidence interns: %v", err)
	}
	ref, err := in.node(ctx, tx, id, "function", good)
	if err != nil {
		t.Fatalf("node: %v", err)
	}
	relID := model.RelationID(model.H("relation-v1", "alpha", "calls", "alpha"))
	if _, err := in.relation(ctx, tx, relID, ref, "calls", noRef); err == nil {
		t.Fatal("relation accepted an unresolved endpoint")
	}
	// The same (kind, canonical_key) under a second canonical id is a caller
	// bug, not a retry: report it rather than loop.
	if _, err := in.node(ctx, tx, model.NodeID(model.H("node-v1", "other")), "function", good); err == nil {
		t.Fatal("node accepted a second canonical id for one (kind, canonical_key)")
	}
}
