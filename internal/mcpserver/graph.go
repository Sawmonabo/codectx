package mcpserver

// L3 GRAPH + SYMBOL INFO owns this file (digest §4 rows 6 and 8-11).
// internal/mcpserver never touches a store or an engine: each handler makes one
// facade call and wraps it.

import (
	"context"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/Sawmonabo/codectx/internal/model"
)

// symbolInfo answers codectx_symbol_info. L3: call Symbol, then References with
// the generation THREADED from the first answer's Page.Meta.Binding.GenerationID,
// so metadata and evidence cannot come from two generations. This composes two
// frozen facade methods; it does not widen the facade.
func (h *handlers) symbolInfo(_ context.Context, _ *mcp.CallToolRequest, _ symbolInfoInput) (*mcp.CallToolResult, result[symbolInfoOutput], error) {
	var zero result[symbolInfoOutput]
	return nil, zero, h.notImplemented("codectx_symbol_info")
}

// callers answers codectx_callers. L3: build a model.GraphRequest from
// graphInput and set Direction to model.DirectionIncoming from the tool name —
// graphInput carries no direction field by design.
func (h *handlers) callers(_ context.Context, _ *mcp.CallToolRequest, _ graphInput) (*mcp.CallToolResult, result[model.GraphResult], error) {
	var zero result[model.GraphResult]
	return nil, zero, h.notImplemented("codectx_callers")
}

// callees answers codectx_callees, with Direction set to
// model.DirectionOutgoing from the tool name.
func (h *handlers) callees(_ context.Context, _ *mcp.CallToolRequest, _ graphInput) (*mcp.CallToolResult, result[model.GraphResult], error) {
	var zero result[model.GraphResult]
	return nil, zero, h.notImplemented("codectx_callees")
}

// dependencyPath answers codectx_dependency_path.
func (h *handlers) dependencyPath(_ context.Context, _ *mcp.CallToolRequest, _ model.PathRequest) (*mcp.CallToolResult, result[model.PathResult], error) {
	var zero result[model.PathResult]
	return nil, zero, h.notImplemented("codectx_dependency_path")
}

// impact answers codectx_impact.
//
// Out is model.ImpactResult WHOLE: Packages, VisitedCount and EdgeCount are
// what Section 19.2's "affected scope and required boundaries, with
// completeness" needs, and re-projecting to a Page would drop all three.
//
// The facade change landing with Task 17 INT makes ExploreService.Impact return
// (model.ImpactResult, error). Until it lands the interface still returns
// Page[model.ImpactEntry], so this stub calls nothing: the registration and the
// output contract are frozen here and compile either way, and L3 fills the one
// call in once INT has landed.
func (h *handlers) impact(_ context.Context, _ *mcp.CallToolRequest, _ model.ImpactRequest) (*mcp.CallToolResult, result[model.ImpactResult], error) {
	var zero result[model.ImpactResult]
	return nil, zero, h.notImplemented("codectx_impact")
}
