package sqlite

import (
	"context"
	"database/sql"

	"github.com/Sawmonabo/codectx/internal/model"
)

// ParsedLanguage is the language of the module node that provider's unit at
// scope published for the file at path in the pinned generation: the language
// that unit's parse of the file kept. ok is false when the generation holds no
// such unit or the unit published no module node for the path, which a unit
// that failed or that the provider did not run leaves behind.
//
// It reads what the unit already stored and writes nothing. The answer is
// four primary-key or index seeks (generation_units by its key, idx_nodes_qname
// on the path and the unit, the node_facts row it names for its language,
// node_ids by its key), so a caller asking about one path per hit pays a
// constant per hit and holds no set.
func (r *PinnedReader) ParsedLanguage(ctx context.Context, provider, scope, path string) (language string, ok bool, err error) {
	if provider == "" || scope == "" || path == "" {
		return "", false, invalid("a parsed-language lookup needs a provider, a scope and a path")
	}
	err = r.s.read(ctx, func(tx *sql.Tx) error {
		err := tx.QueryRowContext(ctx, `SELECT nf.language FROM generation_units gu
			JOIN node_facts nf ON nf.unit_id = gu.unit_id AND nf.qualified_name = ?4
			JOIN node_ids n ON n.id = nf.node_id AND n.kind = ?5
			WHERE gu.generation_id = ?1 AND gu.provider_id = ?2 AND gu.scope_key = ?3 AND nf.language <> ''
			LIMIT 1`, r.gen, provider, scope, path, string(model.NodeModule)).Scan(&language)
		if isNoRows(err) {
			return nil
		}
		if err != nil {
			return wrap("node_facts", err)
		}
		ok = true
		return nil
	})
	if err != nil {
		return "", false, err
	}
	return language, ok, nil
}
