package mcpserver

// L3 GRAPH + SYMBOL INFO owns this file (digest §4 rows 6 and 8-11).
// internal/mcpserver never touches a store or an engine: each handler makes one
// facade call and wraps it. Ranking, paging, cursor codecs and truncation
// belong to internal/search and internal/graph; re-deriving any of them here
// would be the duplicate implementation Section 30.1 forbids.

import (
	"context"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/Sawmonabo/codectx/internal/model"
)

// symbolInfo answers codectx_symbol_info: Symbol for the definition metadata,
// then References for the evidence, with the generation THREADED from the first
// answer's Page.Meta.Binding.GenerationID so metadata and evidence cannot come
// from two generations. This composes two frozen facade methods; it neither
// widens the facade nor opens a second call path into either of them.
//
// symbol_info fixes the two operations — resolve and references — which is what
// makes it one answer rather than a general-purpose twin of codectx_find_symbol
// and codectx_references. symbolInfoInput therefore carries no operation field.
func (h *handlers) symbolInfo(ctx context.Context, _ *mcp.CallToolRequest, in symbolInfoInput) (*mcp.CallToolResult, result[symbolInfoOutput], error) {
	var zero result[symbolInfoOutput]

	symReq := model.SymbolRequest{
		GenerationID:   in.GenerationID,
		Query:          in.Query,
		Operation:      model.SymbolResolve,
		SemanticSource: in.SemanticSource,
		Profile:        in.Profile,
		Page:           in.Page,
	}
	if err := symReq.Validate(); err != nil {
		return nil, zero, toolFailure(h.log, err)
	}
	sym, err := h.explore.Symbol(ctx, symReq)
	if err != nil {
		return nil, zero, toolFailure(h.log, err)
	}

	// The evidence is keyed off the best resolved node. Two cases legitimately
	// leave no node id to ask about: a resolve that matched nothing, and an LSP
	// overlay answer, which has no canonical NodeID because nothing was sealed
	// (model.Node.SemanticSource). Neither is a domain failure, so the metadata
	// is returned with an empty evidence page pinned to the same generation
	// rather than calling References with an empty node id, which would raise a
	// misleading CTX_ARGUMENT_INVALID.
	var nodeID model.NodeID
	if len(sym.Items) > 0 {
		nodeID = sym.Items[0].ID
	}
	if nodeID == "" {
		return nil, ok(h, symbolInfoOutput{
			Symbol: sym,
			Evidence: model.Page[model.ReferenceOccurrence]{
				Meta: model.QueryMeta{
					Binding:      sym.Meta.Binding,
					Completeness: sym.Meta.Completeness,
				},
				Items: []model.ReferenceOccurrence{},
			},
		}), nil
	}

	refReq := model.ReferenceRequest{
		GenerationID:   sym.Meta.Binding.GenerationID,
		NodeID:         nodeID,
		Operation:      model.ReferenceReferences,
		SemanticSource: in.SemanticSource,
		Profile:        in.Profile,
		// A FRESH page request, carrying only the caller's limit. The cursor is
		// deliberately not forwarded: ReferenceRequest.Validate rejects a request
		// naming both a cursor and a generation (PageRequest.ValidatePinned),
		// because a cursor already pins its own generation — and the whole point
		// here is that the generation is the one Symbol answered from. Forwarding
		// the cursor would make symbol_info fail for every paging client. The
		// evidence page's own continuation cursor is returned in its Meta, and a
		// client continues it through codectx_references.
		Page: model.PageRequest{Limit: in.Page.Limit},
	}
	if err := refReq.Validate(); err != nil {
		return nil, zero, toolFailure(h.log, err)
	}
	evidence, err := h.explore.References(ctx, refReq)
	if err != nil {
		return nil, zero, toolFailure(h.log, err)
	}

	return nil, ok(h, symbolInfoOutput{Symbol: sym, Evidence: evidence}), nil
}

// callers answers codectx_callers: the bounded inbound call neighborhood.
// Direction comes from the TOOL NAME, not from the arguments — graphInput
// carries no direction field, so a client cannot ask callers for outbound edges.
func (h *handlers) callers(ctx context.Context, _ *mcp.CallToolRequest, in graphInput) (*mcp.CallToolResult, result[model.GraphResult], error) {
	return h.graph(ctx, in, model.DirectionIncoming)
}

// callees answers codectx_callees, with Direction fixed to outgoing the same
// way.
func (h *handlers) callees(ctx context.Context, _ *mcp.CallToolRequest, in graphInput) (*mcp.CallToolResult, result[model.GraphResult], error) {
	return h.graph(ctx, in, model.DirectionOutgoing)
}

// graph is the one body behind both traversal tools: they differ only in the
// direction they fix, so writing it twice would be the drift policy.md forbids.
// Every bound is passed through verbatim — a zero means "the configured
// default", never unlimited (Section 20.1), and resolving it is the engine's
// job, not this package's.
func (h *handlers) graph(ctx context.Context, in graphInput, direction model.Direction) (*mcp.CallToolResult, result[model.GraphResult], error) {
	var zero result[model.GraphResult]
	req := model.GraphRequest{
		GenerationID: in.GenerationID,
		Start:        in.Start,
		Relations:    in.Relations,
		Direction:    direction,
		MaxDepth:     in.MaxDepth,
		MaxVisited:   in.MaxVisited,
		MaxEdges:     in.MaxEdges,
		Page:         in.Page,
	}
	if err := req.Validate(); err != nil {
		return nil, zero, toolFailure(h.log, err)
	}
	res, err := h.explore.Graph(ctx, req)
	if err != nil {
		return nil, zero, toolFailure(h.log, err)
	}
	return nil, ok(h, res), nil
}

// dependencyPath answers codectx_dependency_path: the bounded shortest
// dependency path between two resolved nodes. model.PathRequest is the In type
// verbatim, so this is validate → one facade call → wrap.
func (h *handlers) dependencyPath(ctx context.Context, _ *mcp.CallToolRequest, in model.PathRequest) (*mcp.CallToolResult, result[model.PathResult], error) {
	var zero result[model.PathResult]
	if err := in.Validate(); err != nil {
		return nil, zero, toolFailure(h.log, err)
	}
	res, err := h.explore.Path(ctx, in)
	if err != nil {
		return nil, zero, toolFailure(h.log, err)
	}
	return nil, ok(h, res), nil
}

// impact answers codectx_impact.
//
// Out is model.ImpactResult WHOLE: Packages, VisitedCount and EdgeCount are
// what Section 19.2's "affected scope and required boundaries, with
// completeness" needs, and re-projecting to a model.Page would silently drop all
// three. toolFor parameterizes only the input, so this signature alone fixes the
// tool's output contract — which is why it stays ImpactResult.
//
// The facade change landing with Task 17 INT makes ExploreService.Impact return
// (model.ImpactResult, error). Until it lands the landed interface still returns
// Page[model.ImpactEntry], and the only way to bridge that here would be to
// rebuild an ImpactResult from the page — which would report Packages as absent
// and both counts as zero, presenting a lossy projection as complete impact.
// That is the silent capability reduction Section 30.1 forbids, so this stub
// calls no facade method and reports a typed CTX_INTERNAL instead (ruling A2 of
// the post-T19-L0 rulings). It compiles unchanged on both sides of INT; see the
// L3 report for the exact edits INT makes.
func (h *handlers) impact(_ context.Context, _ *mcp.CallToolRequest, _ model.ImpactRequest) (*mcp.CallToolResult, result[model.ImpactResult], error) {
	var zero result[model.ImpactResult]
	return nil, zero, h.notImplemented("codectx_impact")
}
