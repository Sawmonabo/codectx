package sqlite

import (
	"context"
	"database/sql"

	"github.com/Sawmonabo/codectx/internal/model"
)

// SuppliedIndex is one `--scip-index` path recorded against a generation, with
// whether the generation ended up holding a unit built from it.
//
// Path is recorded even when nothing resolved: that is the whole point of the
// record. A path that matched no file plans no unit, so a reader looking only
// at units cannot tell a typo from a run that supplied no index at all.
type SuppliedIndex struct {
	Path     string
	Resolved bool
}

// RecordSuppliedIndex records one supplied index against a generation. It is
// called while the generation is still staging, so the row is removed with the
// generation by the foreign key if the run never activates.
//
// The write is idempotent on (generation_id, path): a caller that supplies the
// same path twice records one row, and the later resolution wins rather than
// failing the publish over a duplicate.
func (s *Store) RecordSuppliedIndex(ctx context.Context, gen model.GenerationID, path string, resolved bool) error {
	if path == "" {
		return invalid("a supplied index path cannot be empty")
	}
	return s.write(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `INSERT INTO generation_supplied_indexes(generation_id, path, resolved)
			VALUES(?, ?, ?) ON CONFLICT(generation_id, path) DO UPDATE SET resolved = excluded.resolved`,
			int64(gen), path, boolInt(resolved))
		return wrap("generation_supplied_indexes", err)
	})
}

// SuppliedIndexes reports the supplied indexes recorded against a generation,
// ordered by path so two calls produce the same report. A generation with no
// recorded path returns an empty list, which is the answer "this generation was
// built with no supplied index" -- a measurement now that the recording half
// exists, where before the same emptiness was merely the absence of a feature.
func (s *Store) SuppliedIndexes(ctx context.Context, gen model.GenerationID) ([]SuppliedIndex, error) {
	var out []SuppliedIndex
	err := s.read(ctx, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, `SELECT path, resolved FROM generation_supplied_indexes
			WHERE generation_id = ? ORDER BY path`, int64(gen))
		if err != nil {
			return wrap("generation_supplied_indexes", err)
		}
		defer rows.Close()
		out = nil
		for rows.Next() {
			var si SuppliedIndex
			var resolved int
			if err := rows.Scan(&si.Path, &resolved); err != nil {
				return wrap("generation_supplied_indexes", err)
			}
			si.Resolved = resolved != 0
			out = append(out, si)
		}
		return wrap("generation_supplied_indexes", rows.Err())
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}
