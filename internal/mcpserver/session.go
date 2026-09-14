package mcpserver

// L4 SESSION owns this file (digest §4 rows 12-17). Session id and actor id
// come from TOOL ARGUMENTS, never from MCP wire state, and the facade's
// Validate() is not replaced by SDK schema validation: required-ness in an
// inferred schema comes only from the absence of omitempty, so exactly-one-of
// and distinctness rules still need Validate().

import (
	"context"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/Sawmonabo/codectx/internal/model"
)

// contextPlan answers codectx_context_plan. L4: Plan returns both halves, so
// fill planOutput with the manifest and the opened session's status.
func (h *handlers) contextPlan(_ context.Context, _ *mcp.CallToolRequest, _ model.PlanRequest) (*mcp.CallToolResult, result[planOutput], error) {
	var zero result[planOutput]
	return nil, zero, h.notImplemented("codectx_context_plan")
}

// contextStatus answers codectx_context_status, pairing the coverage page with
// the session status in statusOutput.
func (h *handlers) contextStatus(_ context.Context, _ *mcp.CallToolRequest, _ statusInput) (*mcp.CallToolResult, result[statusOutput], error) {
	var zero result[statusOutput]
	return nil, zero, h.notImplemented("codectx_context_status")
}

// contextNext answers codectx_context_next. METADATA ONLY: the next action, its
// file identity and its offsets. It must never carry source bytes — only
// codectx_read_source may.
func (h *handlers) contextNext(_ context.Context, _ *mcp.CallToolRequest, _ model.SessionRequest) (*mcp.CallToolResult, result[model.NextContextItem], error) {
	var zero result[model.NextContextItem]
	return nil, zero, h.notImplemented("codectx_context_next")
}

// contextEntries answers codectx_context_entries.
func (h *handlers) contextEntries(_ context.Context, _ *mcp.CallToolRequest, _ model.ContextPageRequest) (*mcp.CallToolResult, result[model.ContextPage], error) {
	var zero result[model.ContextPage]
	return nil, zero, h.notImplemented("codectx_context_entries")
}

// contextInclude answers codectx_context_include.
func (h *handlers) contextInclude(_ context.Context, _ *mcp.CallToolRequest, _ model.IncludeRequest) (*mcp.CallToolResult, result[model.SessionStatus], error) {
	var zero result[model.SessionStatus]
	return nil, zero, h.notImplemented("codectx_context_include")
}

// readSource answers codectx_read_source. It is the ONLY handler that may
// return source bytes and the only one bounded by
// resources.max_source_response_bytes rather than the metadata ceiling. It
// echoes the receipt the facade issued and never logs it.
func (h *handlers) readSource(_ context.Context, _ *mcp.CallToolRequest, _ model.ReadChunkRequest) (*mcp.CallToolResult, result[model.ReadChunkResponse], error) {
	var zero result[model.ReadChunkResponse]
	return nil, zero, h.notImplemented("codectx_read_source")
}
