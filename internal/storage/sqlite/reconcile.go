package sqlite

import (
	"context"
	"database/sql"
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

// MaxAliasLookup bounds one alias lookup: the primary identity, the bounded
// ambiguous list a Resolution may carry, and one more row so the resolver can
// tell "exactly at the bound" from "more than the bound" and refuse rather
// than silently drop an equally supported identity. A lookup can never grow
// with the number of aliases stored.
const MaxAliasLookup = model.MaxAmbiguousCandidates + 2

// LookupAliases returns the distinct identities that (scopeKey, nativeKey) is
// aliased to by the given units, restricted to units that are sealed, ordered
// by canonical key and then node id, and truncated at limit (at most
// MaxAliasLookup). The result depends only on the alias rows of those sealed
// immutable units, never on the order they were written.
func (s *Store) LookupAliases(ctx context.Context, units []model.UnitID, scopeKey, nativeKey string, limit int) ([]StoredAlias, error) {
	if len(units) == 0 {
		return nil, nil
	}
	if len(units) > model.MaxDependenciesPerUnit {
		return nil, invalid("alias lookup names %d units, limit %d", len(units), model.MaxDependenciesPerUnit)
	}
	if scopeKey == "" || len(scopeKey) > model.MaxScopeKeyBytes || nativeKey == "" || len(nativeKey) > model.MaxNativeKeyBytes {
		return nil, invalid("alias lookup needs a bounded, non-empty scope key and native key")
	}
	if limit <= 0 || limit > MaxAliasLookup {
		limit = MaxAliasLookup
	}
	args := make([]any, 0, len(units)+3)
	args = append(args, scopeKey, nativeKey)
	marks := make([]string, 0, len(units))
	for _, u := range units {
		key, err := idBlob("unit_id", string(u))
		if err != nil {
			return nil, err
		}
		marks = append(marks, "?")
		args = append(args, key)
	}
	args = append(args, limit)
	query := `SELECT DISTINCT ni.id, ni.kind, ni.canonical_key FROM native_aliases na
		JOIN units u ON u.id = na.unit_id JOIN node_ids ni ON ni.id = na.node_id
		WHERE na.scope_key = ? AND na.native_key = ? AND u.state = 'sealed' AND u.unit_key IN (` + strings.Join(marks, ",") + `)
		ORDER BY ni.canonical_key, ni.id LIMIT ?`
	var out []StoredAlias
	err := s.read(ctx, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, query, args...)
		if err != nil {
			return wrap("native_aliases", err)
		}
		defer rows.Close()
		for rows.Next() {
			var a StoredAlias
			var id []byte
			if err := rows.Scan(&id, &a.Kind, &a.CanonicalKey); err != nil {
				return wrap("native_aliases", err)
			}
			a.NodeID = model.NodeID(idHex(id))
			out = append(out, a)
		}
		return wrap("native_aliases", rows.Err())
	})
	return out, err
}
