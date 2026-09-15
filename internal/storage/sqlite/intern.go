package sqlite

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"errors"
	"fmt"
	"math"

	"github.com/Sawmonabo/codectx/internal/model"
)

// dbInterner is the one implementation of the frozen `interner` contract in
// ids.go: it resolves a canonical identity or an interned string to its
// storage-internal surrogate inside the caller's write transaction, creating
// the dictionary row on first sight.
//
// It holds no *sql.DB and opens nothing; every method is handed the caller's
// *sql.Tx. It is NOT safe for concurrent use and does not need to be: the store
// opens its writer pool with exactly one connection (open.go:128), so at most
// one write transaction -- and therefore at most one caller of this interner --
// exists at a time.
//
// Memory is a function of cache capacity, never of repository size. Three
// bounded LRUs (node ids, relation ids, and one shared dictionary for the S-3
// string tables) each hold `capacity` entries; with the measured average key
// widths of the S-3 sizing -- 32-byte canonical ids rendered as 64-char hex,
// scope keys averaging 34 B and native keys 52 B -- one entry costs roughly
// 64-128 B of key plus ~80 B of map bucket and list element, so the default
// capacity of 1000 (Options.BatchRecords) puts the whole interner well under
// 1 MiB regardless of how many distinct strings the repository contains. The
// writer calls reset() at each batch boundary, so the footprint never
// accumulates across a repository-sized index run.
type dbInterner struct {
	// nodes maps model.NodeID -> node_ids.id.
	nodes *refCache
	// relations maps model.RelationID -> relation_ids.id.
	relations *refCache
	// strings is the shared dictionary cache for scope_keys and native_keys.
	// Its keys are the interned string prefixed with a one-byte table tag,
	// which is injective for any key content and so needs no separator. The
	// two tables draw from one vocabulary (the S-3 sizing measured
	// evidence.native_key's 52 711 distinct values as a subset of
	// native_aliases.native_key's 216 385), so one cache of a given capacity
	// serves both better than two of half the size.
	strings *refCache

	hits   uint64
	misses uint64
}

// Table tags for the shared string cache. They are cache-local and never
// reach SQL or disk.
const (
	scopeKeyTag  = 's'
	nativeKeyTag = 'n'
)

// internCacheFloor keeps the LRUs useful when a caller configures a very small
// batch: below a few hundred entries a cache thrashes and every resolution
// degenerates to an indexed lookup. It is a memory floor, not a work limit --
// exceeding it evicts, it never rejects or truncates anything.
const internCacheFloor = 256

// canonicalKeyBytes is the width node_ids.canonical_key is pinned to by
// CHECK(length(canonical_key) = 32); S-4 made it a BLOB of raw bytes, not hex.
const canonicalKeyBytes = 32

var _ interner = (*dbInterner)(nil)

// newInterner builds the interner for one writer. Capacity is taken from the
// store's batch sizing (Options.BatchRecords, default 1000) so that peak
// interner memory is a function of the batch, exactly as the rest of the
// writer's memory contract is.
func newInterner(opts Options) *dbInterner {
	capacity := opts.BatchRecords
	if capacity < internCacheFloor {
		capacity = internCacheFloor
	}
	return &dbInterner{
		nodes:     newRefCache(capacity),
		relations: newRefCache(capacity),
		strings:   newRefCache(capacity),
	}
}

// internStats is the writer's diagnostic view of interner effectiveness: a low
// hit rate means the batch is scattered across the identity space and each
// fact costs an indexed lookup. Counts are cumulative over the interner's whole
// life and are deliberately NOT cleared by reset(), so a run-level hit rate
// survives the per-batch cache flush.
type internStats struct {
	Hits   uint64
	Misses uint64
}

// stats reports the cumulative cache hit and miss counts. Consumed by the S1
// writer's diagnostics.
func (in *dbInterner) stats() internStats {
	return internStats{Hits: in.hits, Misses: in.misses}
}

func (in *dbInterner) node(ctx context.Context, tx *sql.Tx, id model.NodeID, kind string, canonicalKey []byte) (nodeRef, error) {
	if kind == "" {
		return noRef, invalid("node kind: must not be empty")
	}
	if len(canonicalKey) != canonicalKeyBytes {
		return noRef, invalid("node canonical key: %d bytes, want %d raw bytes", len(canonicalKey), canonicalKeyBytes)
	}
	canonical, err := idBlob("node id", string(id))
	if err != nil {
		return noRef, err
	}
	if ref, ok := in.nodes.get(string(id)); ok {
		in.hits++
		return nodeRef(ref), nil
	}
	in.misses++
	ref, found, err := in.resolve(ctx, tx, "node_ids",
		`INSERT INTO node_ids(canonical, kind, canonical_key) VALUES(?, ?, ?) ON CONFLICT DO NOTHING`,
		[]any{canonical, kind, canonicalKey},
		`SELECT id FROM node_ids WHERE canonical = ?`,
		[]any{canonical})
	if err != nil {
		return noRef, err
	}
	if !found {
		// The insert was suppressed but no row carries this canonical id, so
		// the conflict was on UNIQUE(kind, canonical_key): a different node
		// already owns this (kind, key). model.NewNodeID derives the canonical
		// id from exactly those two values plus the repository, and one store
		// is one repository (S-5), so the caller passed an inconsistent
		// triple. Report it; never retry, and never loop.
		return noRef, internal(fmt.Sprintf("node id %s: kind %q and canonical key %s are already bound to a different node id",
			id, kind, idHex(canonicalKey)))
	}
	in.nodes.put(string(id), ref)
	return nodeRef(ref), nil
}

func (in *dbInterner) relation(ctx context.Context, tx *sql.Tx, id model.RelationID, from nodeRef, kind string, to nodeRef) (relRef, error) {
	if kind == "" {
		return noRef, invalid("relation kind: must not be empty")
	}
	// An unresolved endpoint would otherwise surface as an opaque foreign-key
	// or CHECK failure from SQLite. The interner does not recurse: the caller
	// resolves both endpoints first.
	if !from.valid() || !to.valid() {
		return noRef, invalid("relation %s: endpoints must be resolved before interning (from=%d, to=%d)", id, from, to)
	}
	canonical, err := idBlob("relation id", string(id))
	if err != nil {
		return noRef, err
	}
	if ref, ok := in.relations.get(string(id)); ok {
		in.hits++
		return relRef(ref), nil
	}
	in.misses++
	ref, found, err := in.resolve(ctx, tx, "relation_ids",
		`INSERT INTO relation_ids(canonical, from_node_id, kind, to_node_id) VALUES(?, ?, ?, ?) ON CONFLICT DO NOTHING`,
		[]any{canonical, int64(from), kind, int64(to)},
		`SELECT id FROM relation_ids WHERE canonical = ?`,
		[]any{canonical})
	if err != nil {
		return noRef, err
	}
	if !found {
		// As in node(): the conflict was on UNIQUE(from_node_id, kind,
		// to_node_id) rather than on `canonical`, which means the endpoints
		// and kind the caller passed do not derive the canonical id it passed.
		return noRef, internal(fmt.Sprintf("relation id %s: endpoints (%d, %q, %d) are already bound to a different relation id",
			id, from, kind, to))
	}
	in.relations.put(string(id), ref)
	return relRef(ref), nil
}

// scopeKey resolves into scope_keys, which keeps `key TEXT UNIQUE` and so
// keeps the upsert-and-read shape: 403 distinct scope keys on the control
// fixture is not an amplifier, and reconcile resolves a scope key BY key.
func (in *dbInterner) scopeKey(ctx context.Context, tx *sql.Tx, key string) (scopeRef, error) {
	if key == "" {
		return noRef, invalid("scope key: must not be empty")
	}
	cacheKey := string(scopeKeyTag) + key
	if ref, ok := in.strings.get(cacheKey); ok {
		in.hits++
		return scopeRef(ref), nil
	}
	in.misses++
	ref, found, err := in.resolve(ctx, tx, "scope_keys",
		`INSERT INTO scope_keys(key) VALUES(?) ON CONFLICT DO NOTHING`,
		[]any{key},
		`SELECT id FROM scope_keys WHERE key = ?`,
		[]any{key})
	if err != nil {
		return noRef, err
	}
	if !found {
		// `key TEXT UNIQUE` is the table's only uniqueness constraint, so a
		// suppressed insert whose key then reads back as absent cannot happen
		// inside one transaction. Reaching here means the row vanished under
		// us, which is a corrupt store, not a caller error.
		return noRef, corrupt("scope key %q was neither inserted into scope_keys nor found there", key)
	}
	in.strings.put(cacheKey, ref)
	return scopeRef(ref), nil
}

// nativeKey resolves into the hash-keyed native dictionary (ADR-0003 SS2.3).
// It accepts the empty string: model.Evidence.NativeKey is optional and
// evidence.native_key_id is NOT NULL, so unlocated evidence interns the empty
// key like any other. A scope key stays non-empty (model.NativeAlias.Validate
// requires it), which is why that check lives in scopeKey only.
//
// The id is computed, not allocated, so the resolution is a probe of the hash
// chain rather than an insert-then-select over a UNIQUE index: the chain is
// walked until the stored key equals the incoming key (a hit) or a free id is
// reached (a miss, which this method then claims). Ids are therefore stable for
// the life of the database and across a cache flush, exactly as the contract in
// ids.go requires -- more strongly than before, since they no longer depend on
// insertion order at all.
func (in *dbInterner) nativeKey(ctx context.Context, tx *sql.Tx, key string) (nativeRef, error) {
	cacheKey := string(nativeKeyTag) + key
	if ref, ok := in.strings.get(cacheKey); ok {
		in.hits++
		return nativeRef(ref), nil
	}
	in.misses++
	for {
		id, found, err := lookupNativeKey(ctx, tx, key)
		if err != nil {
			return noRef, err
		}
		if found {
			in.strings.put(cacheKey, id)
			return nativeRef(id), nil
		}
		// id is a free slot on this key's chain. Claim it. ON CONFLICT(id) DO
		// NOTHING keeps the upsert-and-read convergence the interner contract
		// mandates: if another writer claimed the slot first, this insert is
		// suppressed and the next probe walks past it. Progress is guaranteed
		// because a suppressed insert means the slot is now occupied, so the
		// next lookup cannot return the same free id.
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO native_keys(id, key) VALUES(?, ?) ON CONFLICT(id) DO NOTHING`, id, key); err != nil {
			return noRef, wrap("native_keys", err)
		}
	}
}

// nativeKeyID is the dictionary's id derivation: the first 63 bits of the
// key's SHA-256, big-endian. The top bit is cleared so the value is always a
// positive int64 (SQLite has no unsigned integer type), and zero is clamped to
// 1 so that CHECK(id > 0) holds and noRef keeps meaning "absent" -- the empty
// native key is a real, frequently interned value and must not land on the
// sentinel.
func nativeKeyID(key string) int64 {
	sum := sha256.Sum256([]byte(key))
	id := int64(binary.BigEndian.Uint64(sum[:8]) & math.MaxInt64)
	if id == 0 {
		id = 1
	}
	return id
}

// nextNativeKeyID is the collision probe: the next id on a key's chain. It
// wraps past the largest positive int64 back to 1 rather than overflowing into
// the negative range the CHECK forbids.
func nextNativeKeyID(id int64) int64 {
	if id == math.MaxInt64 {
		return 1
	}
	return id + 1
}

// lookupNativeKey walks one key's hash chain. It returns (id, true) for the id
// whose stored key equals key, and (id, false) for the first free id on the
// chain -- which is where a writer must place the key and which a reader reads
// as "this key has never been interned".
//
// This is the collision handling ADR-0003 SS2.3 requires: two keys whose
// digests agree are DETECTED by the string comparison and separated onto
// different ids, never silently merged. Deleting that comparison merges them,
// which is what TestNativeKeyHashCollisionIsDetected proves.
func lookupNativeKey(ctx context.Context, tx *sql.Tx, key string) (int64, bool, error) {
	start := nativeKeyID(key)
	for id := start; ; {
		var stored string
		err := tx.QueryRowContext(ctx, `SELECT key FROM native_keys WHERE id = ?`, id).Scan(&stored)
		switch {
		case isNoRows(err):
			return id, false, nil
		case err != nil:
			return noRef, false, wrap("native_keys", err)
		case stored == key:
			return id, true, nil
		}
		// Not a free slot and not this key: a digest collision (or a chain
		// displaced by one). Probe on. The wrap back to the starting id can
		// only be reached by a dictionary holding 2^63-1 distinct keys, so it
		// is a corruption report, not a bound on the user's work.
		if id = nextNativeKeyID(id); id == start {
			return noRef, false, corrupt("native key dictionary is full: %q has no free id", key)
		}
	}
}

// resolve is the upsert-and-read the interner contract mandates: an
// unconditional INSERT whose catch-all ON CONFLICT DO NOTHING fires for any
// uniqueness constraint, followed by a SELECT of the surviving row. Two writers
// racing on the same key therefore converge on one id -- whichever insert
// landed first wins and the other reads it back.
//
// RETURNING cannot replace the SELECT even though the embedded engine supports
// it (open.go:238-256 refuses anything older than 3.51.3; RETURNING landed in
// 3.35.0): "The RETURNING clause only returns rows that are directly modified
// by the DELETE, INSERT, or UPDATE statement", and the DO NOTHING branch
// modifies nothing, so exactly the conflict case -- the one that needs the id
// most -- would come back empty.
//
// found is false when the insert was suppressed and the SELECT matched nothing;
// each caller knows which of its table's constraints that implicates.
func (in *dbInterner) resolve(ctx context.Context, tx *sql.Tx, table, insertSQL string, insertArgs []any, selectSQL string, selectArgs []any) (ref int64, found bool, err error) {
	if _, err := tx.ExecContext(ctx, insertSQL, insertArgs...); err != nil {
		return noRef, false, wrap(table, err)
	}
	switch err := tx.QueryRowContext(ctx, selectSQL, selectArgs...).Scan(&ref); {
	case errors.Is(err, sql.ErrNoRows):
		return noRef, false, nil
	case err != nil:
		return noRef, false, wrap(table, err)
	}
	return ref, true, nil
}

func (in *dbInterner) reset() {
	in.nodes.reset()
	in.relations.reset()
	in.strings.reset()
}
