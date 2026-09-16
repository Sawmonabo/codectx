package sqlite

import (
	"context"

	"github.com/Sawmonabo/codectx/internal/pagination"
)

// The page-clamp collector lives in internal/pagination, beside the cursors its
// notice tells the caller to continue with, so a package that drains one into
// its answer -- the graph engine, the search service, the coverage session --
// does not have to import the store for a type the store only writes into.
//
// PageClamps and WithPageClamps are re-exported because this package's callers
// install a collector on the context they hand to the store and drain it into
// their answer's notices, so they name the collector type without importing the
// pagination package for it alone.
type PageClamps = pagination.PageClamps

// WithPageClamps returns a context that collects page-bound clamps and the
// collector to read them from once the reads are done.
func WithPageClamps(ctx context.Context) (context.Context, *PageClamps) {
	return pagination.WithPageClamps(ctx)
}

// pageLimit resolves a requested page size against the wire ceiling and reports
// the resolution to the context's collector when the two differ.
func pageLimit(ctx context.Context, limit int) int { return pagination.PageLimit(ctx, limit) }
