package sqlite

import (
	"context"
	"database/sql"

	"github.com/Sawmonabo/codectx/internal/model"
)

// FilesByID hydrates snapshot file metadata for a bounded batch of ids in one
// query (len(ids) <= model.MaxPageItems). Ids absent from the pinned snapshot
// are omitted rather than erroring, exactly like NodesByID (adjacency.go:230).
// It exists because the budget pass needs Size/Path/Status for a selected set,
// and Files (keyset paging, query.go:452) and File (one row, query.go:429)
// would make that an N+1.
//
// Rows come back ordered by file_id, which is the snapshot's primary key order
// and NOT the order of ids: the statement is one indexed probe, so a caller
// that needs its own order indexes the result by model.FileVersion.ID.
func (r *PinnedReader) FilesByID(ctx context.Context, ids []model.FileID) ([]model.FileVersion, error) {
	query, args, err := r.filesByIDQuery(ids)
	if err != nil {
		return nil, err
	}
	var out []model.FileVersion
	err = r.s.read(ctx, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, query, args...)
		if err != nil {
			return wrap("snapshot_files", err)
		}
		defer rows.Close()
		for rows.Next() {
			fv, err := scanFile(rows)
			if err != nil {
				return err
			}
			out = append(out, fv)
		}
		return wrap("snapshot_files", rows.Err())
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// filesByIDQuery builds the statement FilesByID runs, separately so the query
// plan can be asserted against exactly the SQL that ships. It reuses fileQuery
// and scanFile (query.go:480,483) rather than introducing a second projection
// of snapshot_files, so File, Files and FilesByID cannot drift apart.
//
// The batch is bounded at model.MaxPageItems rather than batchBlobs' own
// record ceiling: this is a page-shaped read, and resources.max_page_items is
// the bound the rest of the reader publishes for one.
func (r *PinnedReader) filesByIDQuery(ids []model.FileID) (string, []any, error) {
	if len(ids) > model.MaxPageItems {
		return "", nil, invalid("file_id batch has %d ids, limit %d", len(ids), model.MaxPageItems)
	}
	raw := make([]string, len(ids))
	for i, id := range ids {
		raw[i] = string(id)
	}
	values, err := batchBlobs("file_id", raw)
	if err != nil {
		return "", nil, err
	}
	// ?1 is the pinned generation, which is the marker fileQuery's WHERE
	// clause already reads; the generation join is the whole visibility
	// filter here, because snapshot_files is snapshot-scoped rather than
	// unit-scoped and memberOf does not apply to it.
	b := newBinder(r.gen)
	list := b.markList(values)
	limitMark := b.mark(len(values))
	return fileQuery + ` AND sf.file_id IN ` + list + ` ORDER BY sf.file_id LIMIT ` + limitMark, b.args, nil
}
