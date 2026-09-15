package sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"math"
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

// TestNativeKeyHashCollisionIsDetected is the safety proof of the hash-keyed
// dictionary (ADR-0003 SS2.3): the id is a digest, so two distinct keys CAN
// land on one id, and the dictionary must separate them rather than merge
// them. Safety is detection, not improbability.
//
// A real SHA-256 collision is not constructible, so the test constructs the
// state one would produce: a row squatting on the id the key's digest names,
// holding a different key. The interner must probe past it, give the key its
// own id, and leave the squatter's key intact -- and the reader-side resolution
// (internedPair) must agree with the writer.
//
// Mutation: delete the `stored == key` comparison in lookupNativeKey so any
// occupied slot counts as a hit. Both keys then resolve to one id and the
// first assertion fails -- two distinct native keys silently merged.
func TestNativeKeyHashCollisionIsDetected(t *testing.T) {
	ctx, s, tx := internFixture(t)
	in := newInterner(s.opts)

	const key = "pkg/mod.Symbol#method()."
	const squatter = "a different native key entirely"
	collided := nativeKeyID(key)
	if _, err := tx.ExecContext(ctx, `INSERT INTO native_keys(id, key) VALUES(?, ?)`, collided, squatter); err != nil {
		t.Fatalf("seeding the colliding row: %v", err)
	}

	ref, err := in.nativeKey(ctx, tx, key)
	if err != nil {
		t.Fatalf("nativeKey(%q): %v", key, err)
	}
	if int64(ref) == collided {
		t.Fatalf("native key %q was merged onto id %d, which already holds %q", key, collided, squatter)
	}
	if want := nextNativeKeyID(collided); int64(ref) != want {
		t.Fatalf("native key %q resolved to %d, want the next id on its chain %d", key, ref, want)
	}

	var stored, squatted string
	if err := tx.QueryRowContext(ctx,
		`SELECT (SELECT key FROM native_keys WHERE id = ?), (SELECT key FROM native_keys WHERE id = ?)`,
		int64(ref), collided).Scan(&stored, &squatted); err != nil {
		t.Fatalf("reading both rows back: %v", err)
	}
	if stored != key || squatted != squatter {
		t.Fatalf("the chain lost a key: id %d = %q (want %q), id %d = %q (want %q)",
			ref, stored, key, collided, squatted, squatter)
	}

	// The reader resolves by walking the same chain, so it must land on the
	// same id -- a reader that stopped at the first occupied slot would read
	// the squatter's aliases for this key.
	if _, err := tx.ExecContext(ctx, `INSERT INTO scope_keys(key) VALUES(?)`, "scope"); err != nil {
		t.Fatalf("seeding the scope key: %v", err)
	}
	_, native, err := internedPair(ctx, tx, "scope", key)
	if err != nil {
		t.Fatalf("internedPair: %v", err)
	}
	if native != int64(ref) {
		t.Fatalf("the reader resolved %q to %d, the writer to %d", key, native, ref)
	}

	// Re-resolving through a flushed cache must converge on the same id.
	in.reset()
	again, err := in.nativeKey(ctx, tx, key)
	if err != nil {
		t.Fatalf("nativeKey(%q) after reset: %v", key, err)
	}
	if again != ref {
		t.Fatalf("native key %q resolved to %d after a cache flush, was %d", key, again, ref)
	}
}

// TestNativeKeyIDNeverCollidesWithTheAbsentSentinel pins the two edges of the
// derivation that the CHECK(id > 0) constraint and the noRef sentinel depend
// on: a digest whose first 63 bits are zero must clamp to 1 rather than be
// stored as 0 and read back as "absent", and the probe must wrap rather than
// overflow into the negative ids the CHECK forbids.
func TestNativeKeyIDNeverCollidesWithTheAbsentSentinel(t *testing.T) {
	for _, key := range []string{"", "a", "pkg/mod.Symbol#method()."} {
		if id := nativeKeyID(key); id <= 0 {
			t.Fatalf("nativeKeyID(%q) = %d, which CHECK(id > 0) rejects and noRef reads as absent", key, id)
		}
	}
	if got := nextNativeKeyID(math.MaxInt64); got != 1 {
		t.Fatalf("nextNativeKeyID(MaxInt64) = %d, want a wrap to 1", got)
	}
}
