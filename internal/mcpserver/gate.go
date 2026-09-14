package mcpserver

// L5 GATE owns this file (digest §4 rows 18-23): the review gate and the
// capsule. Session id and actor id are tool arguments, never wire state.

import (
	"context"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/Sawmonabo/codectx/internal/model"
)

// contextAcknowledge answers codectx_context_acknowledge.
func (h *handlers) contextAcknowledge(_ context.Context, _ *mcp.CallToolRequest, _ model.AcknowledgeRequest) (*mcp.CallToolResult, result[model.SessionStatus], error) {
	var zero result[model.SessionStatus]
	return nil, zero, h.notImplemented("codectx_context_acknowledge")
}

// contextWaive answers codectx_context_waive. The returned SessionStatus must
// reach the wire unaltered: a waived session still reports
// ready_for_implementation=false, and flattening that away would defeat the
// Section 16.3 gate.
func (h *handlers) contextWaive(_ context.Context, _ *mcp.CallToolRequest, _ model.WaiverRequest) (*mcp.CallToolResult, result[waiveOutput], error) {
	var zero result[waiveOutput]
	return nil, zero, h.notImplemented("codectx_context_waive")
}

// contextRecord answers codectx_context_record.
func (h *handlers) contextRecord(_ context.Context, _ *mcp.CallToolRequest, _ model.ObservationRequest) (*mcp.CallToolResult, result[recordOutput], error) {
	var zero result[recordOutput]
	return nil, zero, h.notImplemented("codectx_context_record")
}

// contextAdvance answers codectx_context_advance, passing ExpectedVersion
// through verbatim so an optimistic-concurrency conflict is reported, never
// papered over.
func (h *handlers) contextAdvance(_ context.Context, _ *mcp.CallToolRequest, _ model.AdvanceRequest) (*mcp.CallToolResult, result[advanceOutput], error) {
	var zero result[advanceOutput]
	return nil, zero, h.notImplemented("codectx_context_advance")
}

// contextCapsule answers codectx_context_capsule. L5: route the six
// model.CapsuleView values to Capsule and view=capsuleViewExport to Export,
// projected to capsuleExport — CANONICAL METADATA ONLY, never the capsule body,
// whose 8 MiB bound cannot fit the 256 KiB metadata ceiling.
func (h *handlers) contextCapsule(_ context.Context, _ *mcp.CallToolRequest, _ model.CapsuleRequest) (*mcp.CallToolResult, result[capsuleOutput], error) {
	var zero result[capsuleOutput]
	return nil, zero, h.notImplemented("codectx_context_capsule")
}

// contextClose answers codectx_context_close.
func (h *handlers) contextClose(_ context.Context, _ *mcp.CallToolRequest, _ closeInput) (*mcp.CallToolResult, result[model.SessionStatus], error) {
	var zero result[model.SessionStatus]
	return nil, zero, h.notImplemented("codectx_context_close")
}
