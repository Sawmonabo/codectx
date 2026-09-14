package mcpserver

// L4 SESSION owns this file (digest §4 rows 12-17). Session id and actor id
// come from TOOL ARGUMENTS, never from MCP wire state, and the facade's
// Validate() is not replaced by SDK schema validation: required-ness in an
// inferred schema comes only from the absence of omitempty, so exactly-one-of
// and distinctness rules still need Validate().
//
// Every handler here is the thin shape of digest §1.5 — Validate, one
// ContextService call, wrap in the shared envelope. Validate runs on this side
// of the seam rather than being left to the facade because h.context is the
// app.ContextService INTERFACE: the wire contract a client sees must not depend
// on which implementation is behind it.

import (
	"context"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/Sawmonabo/codectx/internal/model"
)

// contextPlan answers codectx_context_plan. Plan returns both halves, so
// planOutput carries the manifest and the opened session's status together and
// a client needs no second round trip to learn the session it just opened.
func (h *handlers) contextPlan(ctx context.Context, _ *mcp.CallToolRequest, in model.PlanRequest) (*mcp.CallToolResult, result[planOutput], error) {
	var zero result[planOutput]
	if err := in.Validate(); err != nil {
		return nil, zero, toolFailure(h.log, err)
	}
	plan, status, err := h.context.Plan(ctx, in)
	if err != nil {
		return nil, zero, toolFailure(h.log, err)
	}
	return nil, ok(h, planOutput{Plan: plan, Status: status}), nil
}

// contextStatus answers codectx_context_status, pairing the coverage page with
// the session status in statusOutput.
//
// statusInput is split into the facade's two arguments here; both are validated
// because SessionStatus takes the page separately and a composite tool input
// has no Validate of its own.
func (h *handlers) contextStatus(ctx context.Context, _ *mcp.CallToolRequest, in statusInput) (*mcp.CallToolResult, result[statusOutput], error) {
	var zero result[statusOutput]
	req := model.SessionRequest{SessionID: in.SessionID, ActorID: in.ActorID}
	if err := req.Validate(); err != nil {
		return nil, zero, toolFailure(h.log, err)
	}
	if err := in.Page.Validate(); err != nil {
		return nil, zero, toolFailure(h.log, err)
	}
	coverage, status, err := h.context.SessionStatus(ctx, req, in.Page)
	if err != nil {
		return nil, zero, toolFailure(h.log, err)
	}
	return nil, ok(h, statusOutput{Coverage: coverage, Status: status}), nil
}

// contextNext answers codectx_context_next. METADATA ONLY: model.NextContextItem
// names the next required file, its content hash and its byte offset, and
// carries no content field at all — only codectx_read_source may return source
// bytes. Nothing is projected away here because there is nothing to project
// away; the guarantee is the answer type's.
func (h *handlers) contextNext(ctx context.Context, _ *mcp.CallToolRequest, in model.SessionRequest) (*mcp.CallToolResult, result[model.NextContextItem], error) {
	var zero result[model.NextContextItem]
	if err := in.Validate(); err != nil {
		return nil, zero, toolFailure(h.log, err)
	}
	item, err := h.context.Next(ctx, in)
	if err != nil {
		return nil, zero, toolFailure(h.log, err)
	}
	return nil, ok(h, item), nil
}

// contextEntries answers codectx_context_entries with one paged projection of
// the session's current manifest.
func (h *handlers) contextEntries(ctx context.Context, _ *mcp.CallToolRequest, in model.ContextPageRequest) (*mcp.CallToolResult, result[model.ContextPage], error) {
	var zero result[model.ContextPage]
	if err := in.Validate(); err != nil {
		return nil, zero, toolFailure(h.log, err)
	}
	page, err := h.context.Entries(ctx, in)
	if err != nil {
		return nil, zero, toolFailure(h.log, err)
	}
	return nil, ok(h, page), nil
}

// contextInclude answers codectx_context_include. ExpectedVersion travels
// verbatim: the compare-and-swap that stops two clients extending scope from
// the same version is the facade's, and re-deriving or defaulting it here would
// silently disarm it.
func (h *handlers) contextInclude(ctx context.Context, _ *mcp.CallToolRequest, in model.IncludeRequest) (*mcp.CallToolResult, result[model.SessionStatus], error) {
	var zero result[model.SessionStatus]
	if err := in.Validate(); err != nil {
		return nil, zero, toolFailure(h.log, err)
	}
	status, err := h.context.Include(ctx, in)
	if err != nil {
		return nil, zero, toolFailure(h.log, err)
	}
	return nil, ok(h, status), nil
}

// readSource answers codectx_read_source. It is the ONLY handler that may
// return source bytes and the only one bounded by
// resources.max_source_response_bytes rather than the metadata ceiling.
//
// The response is returned whole, receipt included: a receipt proves the bytes
// were issued and is what codectx_context_acknowledge confirms, so dropping or
// rewriting it would make coverage unprovable. It is never logged — a receipt
// in a log line is a credential in a log line, and a second actor could replay
// it against the session it was issued for.
//
// The bound is checked here and not left to L1's middleware because the
// middleware's ceiling is resources.max_metadata_response_bytes, which this one
// response type is explicitly exempt from (digest §6). It bounds the chunk
// content — the term the ceiling exists for — and not the serialized envelope:
// the exact wire accounting is internal/coverage's, which sizes the chunk it
// issues against this same key plus its envelope allowance. An over-budget body
// therefore means the ContextService behind this interface mis-sized it, and
// refusing is the honest answer.
//
// An unconfigured key is CTX_INTERNAL naming it, never a resource-limit
// refusal, matching internal/coverage's Limits check: a zero ceiling would
// otherwise refuse every non-empty chunk and tell the caller to ask for less.
func (h *handlers) readSource(ctx context.Context, _ *mcp.CallToolRequest, in model.ReadChunkRequest) (*mcp.CallToolResult, result[model.ReadChunkResponse], error) {
	var zero result[model.ReadChunkResponse]
	if err := in.Validate(); err != nil {
		return nil, zero, toolFailure(h.log, err)
	}
	res, err := h.context.Read(ctx, in)
	if err != nil {
		return nil, zero, toolFailure(h.log, err)
	}
	limit := h.cfg.Resources.MaxSourceResponseBytes
	if limit <= 0 {
		return nil, zero, toolFailure(h.log, &model.Error{
			Code:    model.CodeInternal,
			Message: "resources.max_source_response_bytes is not configured",
		})
	}
	if int64(len(res.Content)) > limit {
		return nil, zero, toolFailure(h.log, &model.Error{
			Code:        model.CodeResourceLimit,
			Message:     "the source chunk exceeds the configured source response bound",
			Remediation: "request a smaller max_bytes, or raise resources.max_source_response_bytes",
		})
	}
	return nil, ok(h, res), nil
}
