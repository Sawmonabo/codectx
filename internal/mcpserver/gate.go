package mcpserver

// L5 GATE owns this file (digest §4 rows 18-23): the review gate and the
// capsule. Session id and actor id are tool arguments, never wire state.

import (
	"context"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/Sawmonabo/codectx/internal/model"
)

// contextAcknowledge answers codectx_context_acknowledge.
func (h *handlers) contextAcknowledge(ctx context.Context, _ *mcp.CallToolRequest, in model.AcknowledgeRequest) (*mcp.CallToolResult, result[model.SessionStatus], error) {
	var zero result[model.SessionStatus]
	if err := in.Validate(); err != nil {
		return nil, zero, toolFailure(h.log, err)
	}
	status, err := h.context.Acknowledge(ctx, in)
	if err != nil {
		return nil, zero, toolFailure(h.log, err)
	}
	return nil, ok(h, status), nil
}

// contextWaive answers codectx_context_waive. The returned SessionStatus must
// reach the wire unaltered: a waived session still reports
// ready_for_implementation=false, and flattening that away would defeat the
// Section 16.3 gate.
func (h *handlers) contextWaive(ctx context.Context, _ *mcp.CallToolRequest, in model.WaiverRequest) (*mcp.CallToolResult, result[waiveOutput], error) {
	var zero result[waiveOutput]
	if err := in.Validate(); err != nil {
		return nil, zero, toolFailure(h.log, err)
	}
	waiver, status, err := h.context.Waive(ctx, in)
	if err != nil {
		return nil, zero, toolFailure(h.log, err)
	}
	return nil, ok(h, waiveOutput{Waiver: waiver, Status: status}), nil
}

// contextRecord answers codectx_context_record.
func (h *handlers) contextRecord(ctx context.Context, _ *mcp.CallToolRequest, in model.ObservationRequest) (*mcp.CallToolResult, result[recordOutput], error) {
	var zero result[recordOutput]
	if err := in.Validate(); err != nil {
		return nil, zero, toolFailure(h.log, err)
	}
	observation, status, err := h.context.Record(ctx, in)
	if err != nil {
		return nil, zero, toolFailure(h.log, err)
	}
	return nil, ok(h, recordOutput{Observation: observation, Status: status}), nil
}

// contextAdvance answers codectx_context_advance, passing ExpectedVersion
// through verbatim so an optimistic-concurrency conflict is reported, never
// papered over.
func (h *handlers) contextAdvance(ctx context.Context, _ *mcp.CallToolRequest, in model.AdvanceRequest) (*mcp.CallToolResult, result[advanceOutput], error) {
	var zero result[advanceOutput]
	if err := in.Validate(); err != nil {
		return nil, zero, toolFailure(h.log, err)
	}
	workflow, status, err := h.context.Advance(ctx, in)
	if err != nil {
		return nil, zero, toolFailure(h.log, err)
	}
	return nil, ok(h, advanceOutput{Workflow: workflow, Status: status}), nil
}

// contextCapsule answers codectx_context_capsule. The six model.CapsuleView
// values route to Capsule, which reads ONE keyset page of that one list from
// the capsule's stored rows; view="export" routes to Export and is projected to
// capsuleExport — identity and counts only, never the capsule body, which is an
// unbounded record set and could not be served under the metadata ceiling.
//
// The export route validates a model.SessionRequest rather than the whole
// CapsuleRequest, because CapsuleRequest.Validate rejects any view outside the
// six model spellings and "export" is deliberately not one of them: it is a
// wire-only selector this package adds. Page is not consulted on that route —
// the projection is a fixed, small field set with nothing to page through — but
// it is still validated, so no tool input escapes its bound.
func (h *handlers) contextCapsule(ctx context.Context, _ *mcp.CallToolRequest, in model.CapsuleRequest) (*mcp.CallToolResult, result[capsuleOutput], error) {
	var zero result[capsuleOutput]
	if string(in.View) == capsuleViewExport {
		req := model.SessionRequest{SessionID: in.SessionID, ActorID: in.ActorID}
		if err := req.Validate(); err != nil {
			return nil, zero, toolFailure(h.log, err)
		}
		// Page is not consulted on this route, but it is still bounded: the
		// schema marks it required, so a caller sends one, and an out-of-range
		// limit or an oversized cursor must be refused rather than silently
		// accepted.
		if err := in.Page.Validate(); err != nil {
			return nil, zero, toolFailure(h.log, err)
		}
		capsule, err := h.context.Export(ctx, req)
		if err != nil {
			return nil, zero, toolFailure(h.log, err)
		}
		export := exportOf(capsule)
		return nil, ok(h, capsuleOutput{Export: &export}), nil
	}
	if err := in.Validate(); err != nil {
		return nil, zero, toolFailure(h.log, err)
	}
	page, err := h.context.Capsule(ctx, in)
	if err != nil {
		return nil, zero, toolFailure(h.log, err)
	}
	return nil, ok(h, capsuleOutput{Page: &page}), nil
}

// exportOf projects a sealed capsule onto its canonical export metadata.
//
// It copies identity, binding, both hashes, the scope version, the strict-gate
// flag and the creation time, and carries the capsule's eight record counts. No
// capsule record crosses this boundary and none is read to build it: a sealed
// capsule holds counts, and its records are rows read one keyset page at a time
// through the six view spellings, so this projection is a fixed, small field
// set whatever the session recorded.
//
// The count keys are model.CapsuleList's own spellings, which are the view
// spellings, so a count and the view that pages it cannot drift apart.
func exportOf(c model.Capsule) capsuleExport {
	counts := make(map[string]int64, len(model.CapsuleListOrder)+1)
	for _, list := range model.CapsuleListOrder {
		counts[string(list)] = c.Counts.Of(list)
	}
	// Completeness is not one of the paged lists -- it is a small, bounded
	// per-capability status the capsule carries whole -- but an export reader
	// counts it beside the eight.
	counts["completeness"] = int64(len(c.Completeness))
	return capsuleExport{
		SessionID:           c.SessionID,
		ActorID:             c.ActorID,
		Binding:             c.Binding,
		ManifestHash:        c.ManifestHash,
		CanonicalHash:       c.CanonicalHash,
		ScopeVersion:        c.ScopeVersion,
		StrictGateSatisfied: c.StrictGateSatisfied,
		Counts:              counts,
		CreatedAt:           c.CreatedAt,
	}
}

// contextClose answers codectx_context_close. closeInput carries the
// optimistic-concurrency version beside the session request because
// CloseSession takes it as a separate argument; the facade rejects a version
// below 1, so this handler forwards it rather than adding a second guard.
func (h *handlers) contextClose(ctx context.Context, _ *mcp.CallToolRequest, in closeInput) (*mcp.CallToolResult, result[model.SessionStatus], error) {
	var zero result[model.SessionStatus]
	req := model.SessionRequest{SessionID: in.SessionID, ActorID: in.ActorID}
	if err := req.Validate(); err != nil {
		return nil, zero, toolFailure(h.log, err)
	}
	status, err := h.context.CloseSession(ctx, req, in.ExpectedVersion)
	if err != nil {
		return nil, zero, toolFailure(h.log, err)
	}
	return nil, ok(h, status), nil
}
