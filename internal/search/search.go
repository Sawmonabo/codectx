// Package search answers the Section 14 discovery endpoints over one pinned
// generation: exact/prefix symbol and path lookup, and generation-local BM25
// lexical retrieval. It holds no graph, context-compiler, MCP or LSP concern.
package search

import (
	"context"
	"log/slog"
	"time"

	"github.com/Sawmonabo/codectx/internal/config"
	"github.com/Sawmonabo/codectx/internal/model"
	"github.com/Sawmonabo/codectx/internal/pagination"
	"github.com/Sawmonabo/codectx/internal/storage/sqlite"
)

// Options composes the service: Store+Repo pin generations, Signer signs
// cursors, Spools hold ranked pages, Resources is Section 20.1.
type Options struct {
	Store     *sqlite.Store
	Repo      model.RepositoryID
	Signer    *pagination.Signer
	Spools    *pagination.Spools
	Resources config.Resources
	CursorTTL time.Duration
	Now       func() time.Time
	Logger    *slog.Logger
}

// Service answers the Section 14 discovery endpoints. Safe for concurrent use.
type Service struct { /* unexported */
}

// New validates the options and builds the service.
func New(o Options) (*Service, error) {
	return nil, &model.Error{Code: model.CodeInternal, Message: "search.New is not implemented"}
}

// Search answers the lexical + exact discovery endpoint, paging through a
// bounded spool because ranking is global over the candidate set.
func (s *Service) Search(ctx context.Context, req model.SearchRequest) (model.Page[model.SearchHit], error) {
	return model.Page[model.SearchHit]{}, &model.Error{Code: model.CodeInternal, Message: "search.Service.Search is not implemented"}
}

// Resolve answers the symbol endpoint, paging by keyset over the tier-ordered
// exact and prefix lookups.
func (s *Service) Resolve(ctx context.Context, req model.SymbolRequest) (model.Page[model.Node], error) {
	return model.Page[model.Node]{}, &model.Error{Code: model.CodeInternal, Message: "search.Service.Resolve is not implemented"}
}

// Close releases everything the service holds open.
func (s *Service) Close() error {
	return &model.Error{Code: model.CodeInternal, Message: "search.Service.Close is not implemented"}
}
