package mcpserver

import (
	"errors"
	"log/slog"

	"github.com/Sawmonabo/codectx/internal/model"
)

// result is the ONE response envelope every tool returns (digest §5).
//
// internal/cli.Envelope is the CLI's shape and stays there: three of its six
// fields are already carried by the protocol — ok by CallToolResult.IsError,
// command by the request's tool name, error by the tool-error content — so
// reusing it would ship three dead fields, and this package may not import
// internal/cli anyway. Declaring the wrapper once is also what keeps
// schema_version and warnings from being repeated 23 times.
//
// Warnings is tagged without omitempty on purpose: the SDK validates every
// output against the schema it infers from this type, and a nil slice marshals
// to JSON null, which fails `"type": "array"`. Build every envelope with ok so
// the slice is never nil.
type result[T any] struct {
	SchemaVersion string   `json:"schema_version"`
	Warnings      []string `json:"warnings"`
	Data          T        `json:"data"`
}

// ok wraps one facade answer in the shared envelope. It is a free function
// rather than a method because Go does not allow type parameters on methods.
//
// Handler pattern, for the five fill-in lanes:
//
//	func (h *handlers) search(ctx context.Context, _ *mcp.CallToolRequest, in model.SearchRequest) (*mcp.CallToolResult, result[model.Page[model.SearchHit]], error) {
//	    var zero result[model.Page[model.SearchHit]]
//	    if err := in.Validate(); err != nil {
//	        return nil, zero, toolFailure(h.log, err)
//	    }
//	    page, err := h.explore.Search(ctx, in)
//	    if err != nil {
//	        return nil, zero, toolFailure(h.log, err)
//	    }
//	    return nil, ok(h, page), nil
//	}
func ok[T any](h *handlers, data T, warnings ...string) result[T] {
	if warnings == nil {
		warnings = []string{}
	}
	return result[T]{SchemaVersion: h.schemaVersion(), Warnings: warnings, Data: data}
}

// internalMessage is the one fixed message an untyped error is reduced to.
// ToolHandlerFor packs err.Error() straight into the tool-error content, so an
// escaped untyped error would otherwise ship a private absolute path, a SQL
// fragment or the analysis engine's name to the client.
const internalMessage = "internal error"

// toolError is the wire form of a domain failure.
//
// Verified against the SDK at v1.7.0 (mcp/server.go, the handler wrapper
// toolForErr installs): when a typed handler returns a non-nil error the SDK
// DISCARDS the handler's *CallToolResult and builds a fresh one with
// SetError(err), which sets IsError and puts err.Error() in a TextContent
// block. StructuredContent is NOT populated on that path. Digest §5's wording
// ("the structured *model.Error in StructuredContent") therefore does not
// describe v1.7.0; see deviation D1 in the L0 report. What this type does
// instead is make every byte that survives count: the Section 22 code, the
// message and, when the domain supplied one, the remediation.
//
// Unwrap keeps errors.As(err, &*model.Error) working for in-package callers
// and tests, so the typed error is never lost on the server side.
type toolError struct{ err *model.Error }

func (e *toolError) Error() string {
	s := e.err.Code + ": " + e.err.Message
	if e.err.Remediation != "" {
		s += " (remediation: " + e.err.Remediation + ")"
	}
	return s
}

func (e *toolError) Unwrap() error { return e.err }

// toolFailure is the single boundary between a facade error and the wire
// (digest §5). The split is binary and has no third case:
//
//   - a *model.Error from the facade is surfaced as-is, so the model never
//     becomes blind to a domain failure and every code stays the Section 22
//     vocabulary of internal/model/errors.go — no second code table;
//   - any other error becomes CTX_INTERNAL with a fixed message, the real
//     detail going to stderr only;
//   - a protocol error is never manufactured here. Only the SDK raises those
//     (unknown tool, malformed frame, unsupported method).
//
// errors.As, never a type assertion: facade errors are wrapped.
func toolFailure(log *slog.Logger, err error) error {
	if err == nil {
		return nil
	}
	var domain *model.Error
	if errors.As(err, &domain) {
		return &toolError{err: domain}
	}
	if log != nil {
		log.Error("tool call failed with an untyped error", "detail", err.Error())
	}
	return &toolError{err: &model.Error{Code: model.CodeInternal, Message: internalMessage}}
}

// schemaVersion is the accessor every envelope goes through. It is the same
// model.BuildInfo.SchemaVersion value cli.NewRoot puts in its envelope, which
// is the genuinely shared piece — unlike the envelope shape itself.
func (h *handlers) schemaVersion() string { return h.build.SchemaVersion }
