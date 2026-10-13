package sqlite

import (
	"context"
	"database/sql"

	"github.com/Sawmonabo/codectx/internal/model"
)

// routeIDBytes is the width of one relation identity in a stored route.
const routeIDBytes = model.IDHexLen / 2

// putEntryRoutes writes every evidence route of one manifest entry as its own
// row, in route order, inside PutManifest's transaction. A route is stored as
// written and in full, its relation identities packed as raw bytes one after
// another; the entry itself keeps only the count. Packed, a route of up to
// five relations stays under one twentieth of a 4 KiB page, the row size a
// WITHOUT ROWID table is built for.
func putEntryRoutes(ctx context.Context, stmt *sql.Stmt, manifest []byte, e model.ContextEntry) error {
	for i, path := range e.EvidencePaths {
		rels := make([]byte, 0, len(path)*routeIDBytes)
		for _, id := range path {
			raw, err := idBlob("manifest route relation", string(id))
			if err != nil {
				return err
			}
			rels = append(rels, raw...)
		}
		if _, err := stmt.ExecContext(ctx, manifest, e.Ordinal, i, rels); err != nil {
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
		rows, err := tx.QueryContext(ctx, `SELECT ordinal, route_index, relations FROM context_entry_routes
			WHERE manifest_id = ? AND (ordinal, route_index) > (?, ?) ORDER BY ordinal, route_index LIMIT ?`,
			raw, afterEntry, afterIndex, limit)
		if err != nil {
			return wrap("context_entry_routes", err)
		}
		defer rows.Close()
		for rows.Next() {
			var r model.ContextRoute
			var rels []byte
			if err := rows.Scan(&r.Entry, &r.Index, &rels); err != nil {
				return wrap("context_entry_routes", err)
			}
			if len(rels) == 0 || len(rels)%routeIDBytes != 0 {
				return corrupt("manifest route payload is not a whole number of relation identities")
			}
			r.Relations = make([]model.RelationID, 0, len(rels)/routeIDBytes)
			for at := 0; at < len(rels); at += routeIDBytes {
				r.Relations = append(r.Relations, model.RelationID(idHex(rels[at:at+routeIDBytes])))
			}
			out = append(out, r)
		}
		return wrap("context_entry_routes", rows.Err())
	})
	return out, err
}
