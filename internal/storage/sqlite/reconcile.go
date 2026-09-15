package sqlite

import (
	"context"
	"database/sql"
	"sort"
	"strings"

	"github.com/Sawmonabo/codectx/internal/model"
)

// Read paths for the provider runtime (Section 11.1, 9.4). Both are bounded,
// read-only and run in one short read transaction; neither can see a unit that
// is not sealed, so a building or failed unit's aliases never influence
// resolution.

// UnitState reports the lifecycle state of the unit keyed by id and whether the
// row exists at all. The coordinator consults it before BeginUnit: a sealed
// unit with a matching key is reused through AttachUnit, never rebuilt.
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
// identities than this costs more round trips and loses nothing. It keeps its
// former value so a caller that passes it asks for the same batch it used to
// ask for as a ceiling.
const MaxAliasLookup = model.MaxAmbiguousCandidates + 2

// aliasUnitChunk bounds the `IN (...)` fan-out of ONE statement. A lookup over
// more dependency units than this is split across statements and the pages are
// merged, so a unit with many dependencies is slower, never refused: the SQL
// parameter budget is a query-shape bound (class B), not a scale bound.
const aliasUnitChunk = model.MaxDependenciesPerUnit

// LookupAliases returns every distinct identity that (scopeKey, nativeKey) is
// aliased to by the given units, restricted to units that are sealed, ordered
// by canonical key and then node id. The result depends only on the alias rows
// of those sealed immutable units, never on the order they were written.
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
	err := s.read(ctx, func(tx *sql.Tx) error {
		for lo := 0; lo < len(keys); lo += aliasUnitChunk {
			hi := lo + aliasUnitChunk
			if hi > len(keys) {
				hi = len(keys)
			}
			if err := s.aliasChunk(ctx, tx, keys[lo:hi], scopeKey, nativeKey, batch, seen, &out); err != nil {
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
	sort.Slice(out, func(i, j int) bool {
		if out[i].CanonicalKey != out[j].CanonicalKey {
			return out[i].CanonicalKey < out[j].CanonicalKey
		}
		return out[i].NodeID < out[j].NodeID
	})
	return out, nil
}

// aliasChunk pages one statement's worth of units by keyset on
// (canonical_key, id) -- the same pair the statement orders by, as a row value,
// because a keyset on canonical_key alone would skip every identity that ties
// on it, which is precisely the ambiguity this lookup exists to report.
func (s *Store) aliasChunk(ctx context.Context, tx *sql.Tx, keys [][]byte, scopeKey, nativeKey string,
	batch int, seen map[model.NodeID]bool, out *[]StoredAlias) error {
	marks := strings.TrimSuffix(strings.Repeat("?,", len(keys)), ",")
	query := `SELECT DISTINCT ni.canonical_key, ni.id, ni.kind FROM native_aliases na
		JOIN units u ON u.id = na.unit_id JOIN node_ids ni ON ni.id = na.node_id
		WHERE na.scope_key = ? AND na.native_key = ? AND u.state = 'sealed' AND u.unit_key IN (` + marks + `)
		AND (ni.canonical_key, ni.id) > (?, ?)
		ORDER BY ni.canonical_key, ni.id LIMIT ?`
	afterKey, afterID := "", []byte{}
	for {
		args := make([]any, 0, len(keys)+5)
		args = append(args, scopeKey, nativeKey)
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
			var id []byte
			if err := rows.Scan(&a.CanonicalKey, &id, &a.Kind); err != nil {
				rows.Close()
				return wrap("native_aliases", err)
			}
			a.NodeID = model.NodeID(idHex(id))
			afterKey, afterID = a.CanonicalKey, id
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
