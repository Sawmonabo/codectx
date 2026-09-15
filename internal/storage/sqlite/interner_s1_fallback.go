package sqlite

import (
	"context"
	"database/sql"

	"github.com/Sawmonabo/codectx/internal/model"
)

// LANE I-S1 FALLBACK -- DELETE THIS FILE WHEN LANE I-S4 LANDS.
//
// S4 owns the interner implementation. This file exists only so the writer
// path S1 owns builds, runs and is provable on its own; it defines exactly one
// exported-to-the-package symbol, newInterner, and the integration lane
// resolves the overlap by deleting the whole file. Nothing outside units.go
// refers to it.
//
// It implements ids.go's frozen contract literally: every method runs inside
// the caller's transaction, resolution is upsert-then-read so two writers
// converge on one row, and memory is the four bounded LRUs, reset by the
// writer at each batch boundary.
//
// ONE CONTRACT AMENDMENT, forced by the data and reported to the controller:
// nativeKey("") is VALID. model.Evidence.NativeKey is an optional field
// (facts.go:285 bounds it, it does not require it) and evidence.native_key_id
// is NOT NULL, so unlocated evidence interns the empty key like any other.
// scopeKey("") stays an error: model.NativeAlias.Validate already requires a
// non-empty scope key, so an empty one is a caller defect.
type sqlInterner struct {
	nodes     *refCache
	relations *refCache
	scopes    *refCache
	natives   *refCache
}

func newInterner(capacity int) interner {
	return &sqlInterner{
		nodes:     newRefCache(capacity),
		relations: newRefCache(capacity),
		scopes:    newRefCache(capacity),
		natives:   newRefCache(capacity),
	}
}

func (i *sqlInterner) node(ctx context.Context, tx *sql.Tx, id model.NodeID, kind string, canonicalKey []byte) (nodeRef, error) {
	raw, err := idBlob("node_id", string(id))
	if err != nil {
		return noRef, err
	}
	if len(canonicalKey) != model.IDHexLen/2 {
		return noRef, invalid("node canonical key is %d bytes, not %d", len(canonicalKey), model.IDHexLen/2)
	}
	if ref, ok := i.nodes.get(string(id)); ok {
		return nodeRef(ref), nil
	}
	ref, err := upsertRef(ctx, tx, "node_ids",
		`INSERT INTO node_ids(canonical, kind, canonical_key) VALUES(?, ?, ?) ON CONFLICT(canonical) DO NOTHING`,
		`SELECT id FROM node_ids WHERE canonical = ?`,
		[]any{raw, kind, canonicalKey}, []any{raw})
	if err != nil {
		return noRef, err
	}
	i.nodes.put(string(id), ref)
	return nodeRef(ref), nil
}

func (i *sqlInterner) relation(ctx context.Context, tx *sql.Tx, id model.RelationID, from nodeRef, kind string, to nodeRef) (relRef, error) {
	raw, err := idBlob("relation_id", string(id))
	if err != nil {
		return noRef, err
	}
	if !from.valid() || !to.valid() {
		return noRef, invalid("relation %s carries an unresolved endpoint", id)
	}
	if ref, ok := i.relations.get(string(id)); ok {
		return relRef(ref), nil
	}
	ref, err := upsertRef(ctx, tx, "relation_ids",
		`INSERT INTO relation_ids(canonical, from_node_id, kind, to_node_id) VALUES(?, ?, ?, ?) ON CONFLICT(canonical) DO NOTHING`,
		`SELECT id FROM relation_ids WHERE canonical = ?`,
		[]any{raw, int64(from), kind, int64(to)}, []any{raw})
	if err != nil {
		return noRef, err
	}
	i.relations.put(string(id), ref)
	return relRef(ref), nil
}

func (i *sqlInterner) scopeKey(ctx context.Context, tx *sql.Tx, key string) (scopeRef, error) {
	if key == "" {
		return noRef, invalid("scope key is empty")
	}
	ref, err := i.stringRef(ctx, tx, i.scopes, "scope_keys", key)
	return scopeRef(ref), err
}

func (i *sqlInterner) nativeKey(ctx context.Context, tx *sql.Tx, key string) (nativeRef, error) {
	ref, err := i.stringRef(ctx, tx, i.natives, "native_keys", key)
	return nativeRef(ref), err
}

func (i *sqlInterner) stringRef(ctx context.Context, tx *sql.Tx, cache *refCache, table, key string) (int64, error) {
	if ref, ok := cache.get(key); ok {
		return ref, nil
	}
	ref, err := upsertRef(ctx, tx, table,
		`INSERT INTO `+table+`(key) VALUES(?) ON CONFLICT(key) DO NOTHING`,
		`SELECT id FROM `+table+` WHERE key = ?`,
		[]any{key}, []any{key})
	if err != nil {
		return noRef, err
	}
	cache.put(key, ref)
	return ref, nil
}

func (i *sqlInterner) reset() {
	i.nodes.reset()
	i.relations.reset()
	i.scopes.reset()
	i.natives.reset()
}

// upsertRef is the contract's upsert-and-read. The INSERT yields to whatever
// row is already there and the SELECT reads the surviving id, so a row this
// call created and one another writer created are indistinguishable to the
// caller -- which is what makes the surrogate a function of the canonical
// identity and not of who wrote it first.
//
// Reading back unconditionally rather than through LastInsertId is deliberate:
// a conflicting INSERT reports the connection's previous insert rowid, which
// would silently bind a reference to the wrong dictionary row.
func upsertRef(ctx context.Context, tx *sql.Tx, table, insert, lookup string, insertArgs, lookupArgs []any) (int64, error) {
	if _, err := tx.ExecContext(ctx, insert, insertArgs...); err != nil {
		return noRef, wrap(table, err)
	}
	var ref int64
	if err := tx.QueryRowContext(ctx, lookup, lookupArgs...).Scan(&ref); err != nil {
		return noRef, wrap(table, err)
	}
	return ref, nil
}
