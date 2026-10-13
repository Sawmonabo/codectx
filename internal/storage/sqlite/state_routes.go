package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"

	"github.com/Sawmonabo/codectx/internal/model"
)

// putEntryRoutes writes every evidence route of one manifest entry as its own
// row, in route order, inside PutManifest's transaction. A route is stored as
// written and in full; the entry itself keeps only the count.
func putEntryRoutes(ctx context.Context, stmt *sql.Stmt, manifest []byte, e model.ContextEntry) error {
	for i, path := range e.EvidencePaths {
		rels, err := json.Marshal(path)
		if err != nil {
			return internal("manifest route: " + err.Error())
		}
		if _, err := stmt.ExecContext(ctx, manifest, e.Ordinal, i, string(rels)); err != nil {
			return wrap("context_entry_routes", err)
		}
	}
	return nil
}

// ManifestRoutes pages a manifest's evidence routes in (entry ordinal, route
// index) order, after the route (afterEntry, afterIndex); afterEntry -1 starts
// before the first. The keyset is the routes table's primary key, so a page
// reads only its own rows however many routes one entry has.
func (s *Store) ManifestRoutes(ctx context.Context, id model.ManifestID, afterEntry, afterIndex, limit int) ([]model.ContextRoute, error) {
	raw, err := idBlob("manifest_id", string(id))
	if err != nil {
		return nil, err
	}
	limit = pageLimit(ctx, limit)
	var out []model.ContextRoute
	err = s.read(ctx, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, `SELECT ordinal, route_index, relations_json FROM context_entry_routes
			WHERE manifest_id = ? AND (ordinal, route_index) > (?, ?) ORDER BY ordinal, route_index LIMIT ?`,
			raw, afterEntry, afterIndex, limit)
		if err != nil {
			return wrap("context_entry_routes", err)
		}
		defer rows.Close()
		for rows.Next() {
			var r model.ContextRoute
			var rels string
			if err := rows.Scan(&r.Entry, &r.Index, &rels); err != nil {
				return wrap("context_entry_routes", err)
			}
			if json.Unmarshal([]byte(rels), &r.Relations) != nil {
				return corrupt("manifest route payload is not readable")
			}
			out = append(out, r)
		}
		return wrap("context_entry_routes", rows.Err())
	})
	return out, err
}
