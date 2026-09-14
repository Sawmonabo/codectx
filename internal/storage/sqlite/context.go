package sqlite

import (
	"context"

	"github.com/Sawmonabo/codectx/internal/model"
)

// FilesByID hydrates snapshot file metadata for a bounded batch of ids in one
// query (len(ids) <= model.MaxPageItems). Ids absent from the pinned snapshot
// are omitted rather than erroring, exactly like NodesByID (adjacency.go:230).
// It exists because the budget pass needs Size/Path/Status for a selected set,
// and Files (keyset paging, query.go:452) and File (one row, query.go:429)
// would make that an N+1.
func (r *PinnedReader) FilesByID(ctx context.Context, ids []model.FileID) ([]model.FileVersion, error) {
	return nil, &model.Error{Code: model.CodeInternal, Message: "PinnedReader.FilesByID is not implemented"}
}
