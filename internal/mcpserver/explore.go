package mcpserver

// L2 EXPLORE owns this file: the index and discovery handlers (digest §4 rows
// 1-5 and 7). Handlers are thin — Validate() then ONE facade call then ok().
// Ranking, paging, cursor codecs and truncation belong to internal/search;
// re-deriving any of them here would be a duplicate implementation.
//
// indexStatus below is L0's worked example and is already complete; the other
// five are L2's to fill in, replacing the notImplemented body and this note.

import (
	"context"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/Sawmonabo/codectx/internal/model"
)

// indexStatus answers codectx_index_status. It is the one tool L0 carries end
// to end, so that jsonschema reflection over a model type, the shared envelope
// and the in-memory transport are all proven before a fill-in lane starts.
//
// IndexStatus takes no request, so there is nothing to Validate().
func (h *handlers) indexStatus(ctx context.Context, _ *mcp.CallToolRequest, _ emptyInput) (*mcp.CallToolResult, result[model.IndexStatus], error) {
	var zero result[model.IndexStatus]
	st, err := h.index.IndexStatus(ctx)
	if err != nil {
		return nil, zero, toolFailure(h.log, err)
	}
	return nil, ok(h, st), nil
}

// refreshIndex answers codectx_refresh_index. L2: map refreshInput onto
// model.IndexRequest with Watch FORCED FALSE — a tool never starts a watcher —
// and route to IndexService.Refresh (or Index when full/rebuild is asked for).
func (h *handlers) refreshIndex(_ context.Context, _ *mcp.CallToolRequest, _ refreshInput) (*mcp.CallToolResult, result[model.IndexResult], error) {
	var zero result[model.IndexResult]
	return nil, zero, h.notImplemented("codectx_refresh_index")
}

// repoOverview answers codectx_repo_overview. L2: Overview is a TYPED REFUSAL
// this wave (its producer is Task 20's). Surface that *model.Error honestly
// through toolFailure as a tool error; returning an empty page or an empty item
// list instead is a silent capability reduction and a Critical defect.
func (h *handlers) repoOverview(_ context.Context, _ *mcp.CallToolRequest, _ model.OverviewRequest) (*mcp.CallToolResult, result[model.Page[model.OverviewItem]], error) {
	var zero result[model.Page[model.OverviewItem]]
	return nil, zero, h.notImplemented("codectx_repo_overview")
}

// search answers codectx_search.
func (h *handlers) search(_ context.Context, _ *mcp.CallToolRequest, _ model.SearchRequest) (*mcp.CallToolResult, result[model.Page[model.SearchHit]], error) {
	var zero result[model.Page[model.SearchHit]]
	return nil, zero, h.notImplemented("codectx_search")
}

// findSymbol answers codectx_find_symbol. L2: pass SemanticSource and Profile
// through untouched so the LSP overlay route stays reachable, and label the
// answer with the overlay binding the facade puts in QueryMeta. No source bytes.
func (h *handlers) findSymbol(_ context.Context, _ *mcp.CallToolRequest, _ model.SymbolRequest) (*mcp.CallToolResult, result[model.Page[model.Node]], error) {
	var zero result[model.Page[model.Node]]
	return nil, zero, h.notImplemented("codectx_find_symbol")
}

// references answers codectx_references.
func (h *handlers) references(_ context.Context, _ *mcp.CallToolRequest, _ model.ReferenceRequest) (*mcp.CallToolResult, result[model.Page[model.ReferenceOccurrence]], error) {
	var zero result[model.Page[model.ReferenceOccurrence]]
	return nil, zero, h.notImplemented("codectx_references")
}
