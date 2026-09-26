package sqlite

import (
	"cmp"
	"context"
	"database/sql"
	"slices"
	"strings"

	"github.com/Sawmonabo/codectx/internal/model"
)

// Read paths for the provider runtime (Section 11.1, 9.4). Each is bounded,
// read-only and runs in one short read transaction; none can see a unit that
// is not sealed, so a building or failed unit's aliases never influence
// resolution. UnitState and DependencyAliases read on the reader pool, so they
// answer from the last commit and never hold the ingestion group.

// UnitState reports the lifecycle state of the unit keyed by id and whether the
// row exists at all, as of the last commit: a unit the open ingestion group
// has begun, sealed or failed is answered as the last commit left it. The
// coordinator consults it before BeginUnit: a sealed unit with a matching key
// is reused through AttachUnit, never rebuilt. Every unit it can meet sealed
// was sealed by an earlier run or tick, and each of those ends in a committing
// Activate or Abort.
func (s *Store) UnitState(ctx context.Context, id model.UnitID) (state model.UnitState, exists bool, err error) {
	key, err := idBlob("unit_id", string(id))
	if err != nil {
		return "", false, err
	}
	err = s.read(ctx, func(tx *sql.Tx) error {
		err := tx.QueryRowContext(ctx, `SELECT state FROM units WHERE unit_key = ?`, key).Scan(&state)
		if isNoRows(err) {
			return nil
		}
		if err != nil {
			return wrap("units", err)
		}
		exists = true
		return nil
	})
	return state, exists, err
}

// StoredAlias is one canonical identity a scoped native key resolves to. The
// canonical key is what the resolver orders and tie-breaks on (Section 9.4:
// never by completion order), and what a provider must copy into the node
// facts it publishes under that identity.
type StoredAlias struct {
	NodeID       model.NodeID
	Kind         model.NodeKind
	CanonicalKey string
}

// MaxAliasLookup is the BATCH size of one alias-lookup read, not a cap on the
// answer. LookupAliases pages the matching identities with a keyset on the
// ordering columns and returns every one of them, so a key aliased to more
// identities than this costs more round trips and loses nothing.
const MaxAliasLookup = model.MaxAmbiguousCandidates + 2

// aliasUnitChunk bounds the `IN (...)` fan-out of ONE statement. A lookup over
// more dependency units than this is split across statements and the pages are
// merged, so a unit with many dependencies is slower, never refused: the SQL
// parameter budget is a query-shape bound (class B), not a scale bound.
const aliasUnitChunk = model.MaxDependenciesPerUnit

// aliasKey is one scoped native key an alias row is published under.
type aliasKey struct{ scope, native string }

// AliasTable is every alias row of a unit's declared dependencies, loaded in
// one read so resolving a unit's candidates costs no read per candidate. It
// answers LookupAliases exactly as Store.LookupAliases does over the same
// committed rows. It is immutable once loaded and safe for concurrent use.
//
// The heap holds the aliases of the declared dependencies and nothing else. A
// unit declares at most its own file's filesystem unit, whose aliases are that
// file's path chain, the file and its classified node, so the table is sized
// by one path's depth, never by the repository.
type AliasTable struct {
	units   map[model.UnitID]bool
	aliases map[aliasKey][]StoredAlias
}

// DependencyAliases loads every alias row of units in one read transaction on
// the reader pool, against the last commit. Each list is the distinct
// identities of one scoped native key, ordered by canonical key and then node
// id. No units is an empty table and no read.
//
// Every unit must be sealed in that snapshot. One that is not -- building,
// failed, absent, or sealed only in the uncommitted ingestion group -- is
// refused naming the unit, never answered as aliased to nothing: an empty
// answer would mint an identity other than the one the committed dependency
// names, and the run would still succeed.
func (s *Store) DependencyAliases(ctx context.Context, units []model.UnitID) (*AliasTable, error) {
	t := &AliasTable{units: make(map[model.UnitID]bool, len(units)), aliases: map[aliasKey][]StoredAlias{}}
	ids := make([]model.UnitID, 0, len(units))
	keys := make([][]byte, 0, len(units))
	for _, u := range units {
		if t.units[u] {
			continue
		}
		key, err := idBlob("unit_id", string(u))
		if err != nil {
			return nil, err
		}
		t.units[u] = true
		ids = append(ids, u)
		keys = append(keys, key)
	}
	if len(keys) == 0 {
		return t, nil
	}
	err := s.read(ctx, func(tx *sql.Tx) error {
		for lo := 0; lo < len(keys); lo += aliasUnitChunk {
			hi := min(lo+aliasUnitChunk, len(keys))
			if err := requireSealed(ctx, tx, ids[lo:hi], keys[lo:hi]); err != nil {
				return err
			}
			if err := t.load(ctx, tx, keys[lo:hi]); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	// Several dependencies may alias one key to the same identity, and several
	// chunks arrive unordered, so each list is put in canonical order once and
	// its repeats, now adjacent, are dropped.
	for k, list := range t.aliases {
		sortAliases(list)
		t.aliases[k] = slices.CompactFunc(list, func(a, b StoredAlias) bool { return a.NodeID == b.NodeID })
	}
	return t, nil
}

// requireSealed refuses the first of ids that is not sealed in tx's snapshot.
// keys are the ids' raw unit keys, in the same order.
func requireSealed(ctx context.Context, tx *sql.Tx, ids []model.UnitID, keys [][]byte) error {
	marks := strings.TrimSuffix(strings.Repeat("?,", len(keys)), ",")
	args := make([]any, 0, len(keys))
	for _, k := range keys {
		args = append(args, k)
	}
	rows, err := tx.QueryContext(ctx, `SELECT unit_key FROM units WHERE state = 'sealed' AND unit_key IN (`+marks+`)`, args...)
	if err != nil {
		return wrap("units", err)
	}
	defer rows.Close()
	sealed := make(map[string]bool, len(keys))
	for rows.Next() {
		var key []byte
		if err := rows.Scan(&key); err != nil {
			return wrap("units", err)
		}
		sealed[string(key)] = true
	}
	if err := rows.Err(); err != nil {
		return wrap("units", err)
	}
	for i, k := range keys {
		if !sealed[string(k)] {
			return invalid("dependency %s is not a sealed unit in the last commit; providers resolve only against completed dependencies", ids[i])
		}
	}
	return nil
}

// load appends every alias row of the sealed units keyed by keys to t,
// streamed: the statement is bounded by the chunk's units, and the rows by
// what those units published.
func (t *AliasTable) load(ctx context.Context, tx *sql.Tx, keys [][]byte) error {
	marks := strings.TrimSuffix(strings.Repeat("?,", len(keys)), ",")
	args := make([]any, 0, len(keys))
	for _, k := range keys {
		args = append(args, k)
	}
	rows, err := tx.QueryContext(ctx, `SELECT sk.key, nk.key, ni.canonical_key, ni.canonical, ni.kind FROM native_aliases na
		JOIN units u ON u.id = na.unit_id JOIN scope_keys sk ON sk.id = na.scope_key_id
		JOIN native_keys nk ON nk.id = na.native_key_id JOIN node_ids ni ON ni.id = na.node_id
		WHERE u.state = 'sealed' AND u.unit_key IN (`+marks+`)`, args...)
	if err != nil {
		return wrap("native_aliases", err)
	}
	defer rows.Close()
	for rows.Next() {
		var k aliasKey
		var a StoredAlias
		var canonicalKey, id []byte
		if err := rows.Scan(&k.scope, &k.native, &canonicalKey, &id, &a.Kind); err != nil {
			return wrap("native_aliases", err)
		}
		a.CanonicalKey = idHex(canonicalKey)
		a.NodeID = model.NodeID(idHex(id))
		t.aliases[k] = append(t.aliases[k], a)
	}
	if err := rows.Err(); err != nil {
		return wrap("native_aliases", err)
	}
	return nil
}

// LookupAliases answers what Store.LookupAliases answers for the same units
// and key, from the loaded table: the same argument validation, and a copy of
// the key's ordered identities. A unit the table was not loaded for is refused
// as an internal fault, because answering it would report aliases of rows this
// table never read. batch is accepted to satisfy the resolver's store
// interface and bounds nothing: the table already holds every row.
func (t *AliasTable) LookupAliases(_ context.Context, units []model.UnitID, scopeKey, nativeKey string, _ int) ([]StoredAlias, error) {
	if len(units) == 0 {
		return nil, nil
	}
	if scopeKey == "" || len(scopeKey) > model.MaxScopeKeyBytes || nativeKey == "" || len(nativeKey) > model.MaxNativeKeyBytes {
		return nil, invalid("alias lookup needs a bounded, non-empty scope key and native key")
	}
	for _, u := range units {
		if !t.units[u] {
			return nil, internal("alias lookup names unit " + string(u) + ", which is not a dependency this table was loaded for")
		}
	}
	return slices.Clone(t.aliases[aliasKey{scope: scopeKey, native: nativeKey}]), nil
}

// sortAliases puts identities in the resolver's order: canonical key, then
// node id, so a tie on the canonical key is broken by identity and never by
// the order rows were written.
func sortAliases(list []StoredAlias) {
	slices.SortFunc(list, func(a, b StoredAlias) int {
		return cmp.Or(cmp.Compare(a.CanonicalKey, b.CanonicalKey), cmp.Compare(a.NodeID, b.NodeID))
	})
}

// LookupAliases returns every distinct identity that (scopeKey, nativeKey) is
// aliased to by the given units, restricted to units that are sealed, ordered
// by canonical key and then node id. The result depends only on the alias rows
// of those sealed immutable units, never on the order they were written. The
// indexing path resolves through a DependencyAliases table instead; this
// per-key read sees the ingestion group's own writes.
//
// batch is the number of rows read per statement (<= 0 or over MaxAliasLookup
// means MaxAliasLookup); it bounds the read, not the answer. The heap holds one
// batch plus the distinct identities of the ONE key being resolved -- a
// per-symbol population, never a repository-sized one -- which is what lets the
// resolver page a truly ambiguous key instead of failing its analysis unit.
func (s *Store) LookupAliases(ctx context.Context, units []model.UnitID, scopeKey, nativeKey string, batch int) ([]StoredAlias, error) {
	if len(units) == 0 {
		return nil, nil
	}
	if scopeKey == "" || len(scopeKey) > model.MaxScopeKeyBytes || nativeKey == "" || len(nativeKey) > model.MaxNativeKeyBytes {
		return nil, invalid("alias lookup needs a bounded, non-empty scope key and native key")
	}
	if batch <= 0 || batch > MaxAliasLookup {
		batch = MaxAliasLookup
	}
	keys := make([][]byte, 0, len(units))
	for _, u := range units {
		key, err := idBlob("unit_id", string(u))
		if err != nil {
			return nil, err
		}
		keys = append(keys, key)
	}
	seen := make(map[model.NodeID]bool)
	var out []StoredAlias
	err := s.readOwn(ctx, func(tx *sql.Tx) error {
		// The dictionaries are RESOLVED here, never created: this is a read
		// transaction, and a key no unit has ever published is aliased to
		// nothing, which is the same empty answer the old TEXT predicate gave.
		// That is why this path does not consume the interner.
		scopeRef, nativeRef, err := internedPair(ctx, tx, scopeKey, nativeKey)
		if err != nil || scopeRef == noRef || nativeRef == noRef {
			return err
		}
		for lo := 0; lo < len(keys); lo += aliasUnitChunk {
			hi := lo + aliasUnitChunk
			if hi > len(keys) {
				hi = len(keys)
			}
			if err := s.aliasChunk(ctx, tx, keys[lo:hi], scopeRef, nativeRef, batch, seen, &out); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	// One chunk arrives ordered; several are merged, so the canonical order is
	// restored once over the distinct identities of this key.
	sortAliases(out)
	return out, nil
}

// aliasChunk pages one statement's worth of units by keyset on
// (canonical_key, canonical) -- the same pair the statement orders by, as a row
// value, because a keyset on canonical_key alone would skip every identity that
// ties on it, which is precisely the ambiguity this lookup exists to report.
//
// The cursor carries the CANONICAL node id, never node_ids.id: the surrogate is
// meaningful only inside one database file and only until the next rebuild
// (ids.go), so ordering or resuming on it would make the answer depend on write
// order -- the one thing Section 9.4 forbids this lookup to do. canonical_key
// is now a BLOB(32) rather than its lowercase hex TEXT, and hex is a
// monotone encoding, so memcmp over the raw bytes yields byte-for-byte the
// order the hex column yielded; the exported CanonicalKey stays hex TEXT.
//
// scopeKey/nativeKey are resolved to their dictionary rows by the caller and
// passed as surrogates, so this statement still probes idx_alias_lookup on its
// leading columns.
func (s *Store) aliasChunk(ctx context.Context, tx *sql.Tx, keys [][]byte, scopeRef, nativeRef int64,
	batch int, seen map[model.NodeID]bool, out *[]StoredAlias) error {
	marks := strings.TrimSuffix(strings.Repeat("?,", len(keys)), ",")
	query := `SELECT DISTINCT ni.canonical_key, ni.canonical, ni.kind FROM native_aliases na
		JOIN units u ON u.id = na.unit_id JOIN node_ids ni ON ni.id = na.node_id
		WHERE na.scope_key_id = ? AND na.native_key_id = ? AND u.state = 'sealed' AND u.unit_key IN (` + marks + `)
		AND (ni.canonical_key, ni.canonical) > (?, ?)
		ORDER BY ni.canonical_key, ni.canonical LIMIT ?`
	afterKey, afterID := []byte{}, []byte{}
	for {
		args := make([]any, 0, len(keys)+5)
		args = append(args, scopeRef, nativeRef)
		for _, k := range keys {
			args = append(args, k)
		}
		args = append(args, afterKey, afterID, batch)
		rows, err := tx.QueryContext(ctx, query, args...)
		if err != nil {
			return wrap("native_aliases", err)
		}
		n := 0
		for rows.Next() {
			var a StoredAlias
			var key, id []byte
			if err := rows.Scan(&key, &id, &a.Kind); err != nil {
				rows.Close()
				return wrap("native_aliases", err)
			}
			a.CanonicalKey = idHex(key)
			a.NodeID = model.NodeID(idHex(id))
			afterKey, afterID = key, id
			n++
			if seen[a.NodeID] {
				continue
			}
			seen[a.NodeID] = true
			*out = append(*out, a)
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return wrap("native_aliases", err)
		}
		rows.Close()
		if n < batch {
			return nil
		}
	}
}

// internedPair resolves a scope key and a native key to their dictionary
// surrogates inside a read transaction. A key with no dictionary row yields
// noRef, which every caller reads as "aliased to nothing" -- the dictionaries
// are written only by the unit writer, so an unseen key genuinely has no alias.
func internedPair(ctx context.Context, tx *sql.Tx, scopeKey, nativeKey string) (scope, native int64, err error) {
	err = tx.QueryRowContext(ctx, `SELECT id FROM scope_keys WHERE key = ?`, scopeKey).Scan(&scope)
	switch {
	case isNoRows(err):
		return noRef, noRef, nil
	case err != nil:
		return noRef, noRef, wrap("scope_keys", err)
	}
	// native_keys is hash-keyed (ADR-0003 SS2.3), so there is no index over
	// `key` to probe: resolving key->id walks the key's own hash chain by id,
	// comparing the stored key at each step. A free slot on the chain means
	// the key was never interned, which is the same noRef this function
	// returned before.
	native, found, err := lookupNativeKey(ctx, tx, nil, nativeKey)
	if err != nil || !found {
		return scope, noRef, err
	}
	return scope, native, nil
}
