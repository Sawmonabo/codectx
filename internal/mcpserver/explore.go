package mcpserver

// L2 EXPLORE owns this file: the index and discovery handlers (digest §4 rows
// 1-5 and 7). Handlers are thin — Validate() then ONE facade call then ok().
// Ranking, paging, cursor codecs and truncation belong to internal/search;
// re-deriving any of them here would be a duplicate implementation. Nothing in
// this package touches a store or an analysis engine.

import (
	"context"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/Sawmonabo/codectx/internal/model"
)

// indexStatus answers codectx_index_status. It is the one tool L0 carries end
// to end, so that jsonschema reflection over a model type, the shared envelope
// and the in-memory transport are all proven before a fill-in lane starts.
//
// The input is model.StatusRequest (ruling Q1), the same request `codectx
// status --resources` builds: the Section 23 accounting block is a field on the
// answer, and a model that could not ask for it would have to guess at this
// installation's resource state or go without. It defaults to false, so a
// client that sends no arguments still gets the cheap status it always got.
func (h *handlers) indexStatus(ctx context.Context, _ *mcp.CallToolRequest, in model.StatusRequest) (*mcp.CallToolResult, result[model.IndexStatus], error) {
	var zero result[model.IndexStatus]
	if err := in.Validate(); err != nil {
		return nil, zero, toolFailure(h.log, err)
	}
	st, err := h.index.IndexStatus(ctx, in)
	if err != nil {
		return nil, zero, toolFailure(h.log, err)
	}
	return nil, ok(h, st), nil
}

// refreshIndex answers codectx_refresh_index.
//
// Watch is forced false: a tool never starts a watcher. The watcher is the
// serve process's single decision, made once from mcp.watch, and the tool takes
// emptyInput precisely so a client cannot ask for one here.
//
// The route is IndexService.Refresh and only Refresh (digest §4 row 2), which
// takes no arguments at all: Refresh refuses full and rebuild with a remediated
// CTX_ARGUMENT_INVALID naming `index --full`/`index --rebuild`, so a `full` or
// `rebuild` argument here could only ever yield that refusal — a dead input
// surface. Section 19.2 lists no codectx_index tool, so rerouting either to
// IndexService.Index is not the alternative: that would make a build-a-new-
// generation operation — one that, for rebuild, creates a new cache — reachable
// through a tool whose name does not say so.
func (h *handlers) refreshIndex(ctx context.Context, _ *mcp.CallToolRequest, _ emptyInput) (*mcp.CallToolResult, result[model.IndexResult], error) {
	var zero result[model.IndexResult]
	// The request is not Validate()d here: IndexRequest.Validate rejects only
	// the full+rebuild combination, and all three fields are fixed false, so the
	// branch could never fire. Refresh validates the request it is given.
	res, err := h.index.Refresh(ctx, model.IndexRequest{})
	if err != nil {
		return nil, zero, toolFailure(h.log, err)
	}
	return nil, ok(h, res), nil
}

// repoOverview answers codectx_repo_overview.
//
// Overview is a TYPED REFUSAL this wave — its producer is Task 20's — so the
// only correct thing this handler does with that *model.Error is hand it to
// toolFailure, which is what every handler here already does with a facade
// error. There is deliberately NO special case: returning an empty page, an
// empty item list or a success envelope with no data would be the silent
// capability reduction Section 30.1 forbids, because a model told "no results"
// cannot tell that apart from "this repository has no packages".
func (h *handlers) repoOverview(ctx context.Context, _ *mcp.CallToolRequest, in model.OverviewRequest) (*mcp.CallToolResult, result[model.Page[model.OverviewItem]], error) {
	var zero result[model.Page[model.OverviewItem]]
	if err := in.Validate(); err != nil {
		return nil, zero, toolFailure(h.log, err)
	}
	page, err := h.explore.Overview(ctx, in)
	if err != nil {
		return nil, zero, toolFailure(h.log, err)
	}
	return nil, ok(h, page), nil
}

// search answers codectx_search. Validate() is not replaced by the SDK's schema
// validation: required-ness in the inferred schema comes only from the absence
// of omitempty, so the query's byte bound and the cursor/generation
// exactly-one-of rule are still checked here.
func (h *handlers) search(ctx context.Context, _ *mcp.CallToolRequest, in model.SearchRequest) (*mcp.CallToolResult, result[model.Page[model.SearchHit]], error) {
	var zero result[model.Page[model.SearchHit]]
	if err := in.Validate(); err != nil {
		return nil, zero, toolFailure(h.log, err)
	}
	page, err := h.explore.Search(ctx, in)
	if err != nil {
		return nil, zero, toolFailure(h.log, err)
	}
	return nil, ok(h, page), nil
}

// findSymbol answers codectx_find_symbol. SemanticSource and Profile are passed
// through UNTOUCHED, which is what keeps the LSP overlay route of Section 11.6
// reachable through the same typed facade as the canonical path; rewriting
// either here would silently answer a live-source question with index facts.
// The overlay label the facade puts in QueryMeta.Overlay is likewise returned
// verbatim, so an ephemeral dirty-worktree answer stays distinguishable from a
// sealed fact. model.Node carries no file content: only codectx_read_source
// returns source bytes.
func (h *handlers) findSymbol(ctx context.Context, _ *mcp.CallToolRequest, in model.SymbolRequest) (*mcp.CallToolResult, result[model.Page[model.Node]], error) {
	var zero result[model.Page[model.Node]]
	if err := in.Validate(); err != nil {
		return nil, zero, toolFailure(h.log, err)
	}
	page, err := h.explore.Symbol(ctx, in)
	if err != nil {
		return nil, zero, toolFailure(h.log, err)
	}
	return nil, ok(h, page), nil
}

// references answers codectx_references. Like findSymbol it passes
// SemanticSource and Profile through untouched.
func (h *handlers) references(ctx context.Context, _ *mcp.CallToolRequest, in model.ReferenceRequest) (*mcp.CallToolResult, result[model.Page[model.ReferenceOccurrence]], error) {
	var zero result[model.Page[model.ReferenceOccurrence]]
	if err := in.Validate(); err != nil {
		return nil, zero, toolFailure(h.log, err)
	}
	page, err := h.explore.References(ctx, in)
	if err != nil {
		return nil, zero, toolFailure(h.log, err)
	}
	return nil, ok(h, page), nil
}
