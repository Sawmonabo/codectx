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

// MaxAliasLookup is the limit the resolver passes with each alias lookup. It
// caps nothing: an AliasTable already holds every alias row of the unit's
// dependencies and answers a key with every identity it is aliased to.
const MaxAliasLookup = model.MaxAmbiguousCandidates + 2

// aliasUnitChunk bounds the `IN (...)` fan-out of ONE statement. A lookup over
// more dependency units than this is split across statements and the pages are
// merged, so a unit with many dependencies is slower, never refused: the SQL
// parameter budget is a query-shape bound (class B), not a scale bound.
const aliasUnitChunk = model.MaxDependenciesPerUnit

// aliasKey is one scoped native key an alias row is published under.
type aliasKey struct{ scope, native string }

// AliasTable is every alias row of a unit's declared dependencies, loaded in
// one read so resolving a unit's candidates costs no read per candidate. It is
// the one alias path: the index build and the provider test harness both
// resolve through it. It is immutable once loaded and safe for concurrent use.
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

// LookupAliases returns every distinct identity that (scopeKey, nativeKey) is
// aliased to by the given units, from the loaded table: a copy of the key's
// identities ordered by canonical key and then node id, which depends only on
// the sealed units' alias rows and never on the order they were written. A
// unit the table was not loaded for is refused as an internal fault, because
// answering it would report aliases of rows this table never read. The limit
// is accepted to satisfy the resolver's store interface and bounds nothing:
// the table already holds every row.
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
