package mcpserver

import (
	"context"
	"errors"
	"strconv"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/Sawmonabo/codectx/internal/config"
	"github.com/Sawmonabo/codectx/internal/model"
)

// limits is the Section 19.3 bound set, resolved from configuration once at
// construction so no per-call path re-reads or re-derives it.
//
// The semaphore is a buffered channel rather than a counter: a channel send is
// the only acquire primitive that can be selected against ctx.Done(), and an
// acquire that cannot be abandoned is the unbounded wait Section 6 forbids.
//
// There is ONE traversal gate in this process and it is not here. How many
// graph traversals run at once (config.GraphSlots) is held by the engine's own
// gate in internal/app, which every caller passes through -- a command as well
// as a tool call -- and a second gate over the same slot count, sized from the
// same figure and reachable only from this server, could only drift from it or
// refuse a call the one gate would have admitted.
type limits struct {
	// maxParamsBytes caps the RAW tools/call arguments frame, before any
	// decoding. resources.max_metadata_response_bytes serves as the MCP REQUEST
	// frame cap: a request larger than the largest answer the server may return
	// cannot produce a serviceable call.
	//
	// The per-text-field bound (resources.max_query_text_bytes) is a different
	// bound and stays where it belongs, in each request type's Validate().
	//
	// This middleware bounds REQUESTS ONLY and applies no response ceiling, per
	// the wave-F ruling on review finding F1. What bounds an answer instead:
	// the generic tools are bounded by resources.max_page_items plus the
	// bounded record fields of each model type, and codectx_read_source -- the
	// only tool that returns source bytes -- is bounded by
	// resources.max_source_response_bytes (7 MiB) in its own handler. A
	// response gate here would have to marshal every answer twice to weigh it,
	// which is the speculative infrastructure policy.md forbids, and at 256 KiB
	// it would refuse every source chunk above that.
	maxParamsBytes int64
	// calls bounds outstanding tool calls INDEPENDENTLY of parser concurrency:
	// a query gate and an indexing gate are different resources and sharing one
	// would let either starve the other. Both are derived from this machine's
	// cores (config.QuerySlots, config.ParserWorkers) and neither refuses a
	// call -- a call that finds no slot waits.
	calls chan struct{}
	// timeout is the per-call deadline a tool handler runs under when neither
	// the client's request nor the configuration asks for something else. Zero
	// is "no deadline", which is what resources.query_timeout defaults to: the
	// answer is complete or it is nothing, never a page the client has to tune
	// a bound to get past.
	timeout time.Duration
}

// newLimits resolves the bounds and FAILS CLOSED on a non-positive one.
//
// config.Validate already rejects every one of these as "must be positive", so
// a zero here means New was handed a configuration that never went through
// Load. Clamping it to a private default would hide that; refusing to start
// reports it while stdout is still silent.
func newLimits(cfg config.Config) (limits, error) {
	r := cfg.Resources
	for _, b := range []struct {
		key string
		v   int64
	}{
		{"resources.max_metadata_response_bytes", r.MaxMetadataResponseBytes},
		{"resources.max_page_items", int64(r.MaxPageItems)},
		// resources.query_timeout is deliberately absent: zero is its
		// "unlimited" spelling and its default, and a tool call that carries no
		// deadline of its own is meant to return the COMPLETE answer rather
		// than a page the client has to tune a timeout to get past.
	} {
		if b.v <= 0 {
			return limits{}, &model.Error{
				Code:    model.CodeConfigInvalid,
				Message: b.key + " must be positive to serve MCP",
			}
		}
	}
	return limits{
		maxParamsBytes: r.MaxMetadataResponseBytes,
		calls:          make(chan struct{}, config.QuerySlots()),
		timeout:        r.QueryTimeout.Std(),
	}, nil
}

// acquire takes the tool-call gate. The slot it takes is released by the
// returned function; a traversal takes the process's one graph slot further
// down, inside the engine, where every caller takes it.
//
// Waiting is bounded by ctx: a caller that goes away, or a call that has spent
// its deadline queuing, is refused rather than parked forever.
func (l limits) acquire(ctx context.Context) (func(), *model.Error) {
	select {
	case l.calls <- struct{}{}:
		return func() { <-l.calls }, nil
	case <-ctx.Done():
		return nil, waitFailed(ctx, "waiting for a tool-call slot")
	}
}

// waitFailed names why a bounded wait ended, keeping the Section 22 codes
// apart: a spent deadline is CTX_QUERY_DEADLINE and retryable, a caller that
// went away is CTX_CANCELED and is not. Collapsing both into CTX_INTERNAL —
// which is what an untyped error would become at toolFailure — would tell the
// model a retryable condition was a bug.
func waitFailed(ctx context.Context, what string) *model.Error {
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return &model.Error{
			Code:      model.CodeQueryDeadline,
			Message:   "the query deadline expired " + what,
			Retryable: true,
		}
	}
	return &model.Error{Code: model.CodeCanceled, Message: "the call was canceled " + what}
}

// limitMiddleware enforces the Section 19.3 bounds that must hold BEFORE a
// handler runs. It is installed with (*mcp.Server).AddReceivingMiddleware, so
// it wraps the session's method handler and no tools/call can route around it.
//
// It never manufactures a JSON-RPC error. Digest §5 reserves protocol errors
// for the SDK (unknown tool, malformed frame, unsupported method); a refused
// call is a DOMAIN failure and goes back as a tool error carrying its Section
// 22 code, which is also the only shape a client can read the code out of.
func (s *Server) limitMiddleware() mcp.Middleware {
	return func(next mcp.MethodHandler) mcp.MethodHandler {
		return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
			call, isCall := req.(*mcp.CallToolRequest)
			if !isCall {
				return next(ctx, method, req)
			}

			// The frame is still json.RawMessage here: AddTool's wrapper
			// unmarshals and schema-validates inside next, so this cap is paid
			// before the allocation it exists to prevent, not after it.
			if size := int64(len(call.Params.Arguments)); size > s.limits.maxParamsBytes {
				return refusal(&model.Error{
					Code:    model.CodeResourceLimit,
					Message: "tool arguments exceed the configured request bound",
					Details: map[string]string{
						"bytes": strconv.FormatInt(size, 10),
						"limit": strconv.FormatInt(s.limits.maxParamsBytes, 10),
					},
					Retryable: false,
				}), nil
			}

			// The deadline is installed BEFORE the gate, so it bounds the queue
			// wait as well as the handler: time spent waiting for a slot is time
			// the caller is waiting, and a gate that could be queued on without
			// a deadline is the unbounded wait Section 6 forbids.
			// resources.query_timeout is the DEFAULT deadline and not a
			// ceiling: a client whose request already carries one has said how
			// long the call may run, and context.WithTimeout would silently
			// take the smaller of the two, expiring a raised budget at the
			// configured default. Zero is "no deadline", the default: a tool
			// call answers in full rather than handing back a continuation the
			// client never asked for.
			var cancel context.CancelFunc
			ctx, cancel = model.QueryDeadline(ctx, s.limits.timeout)
			defer cancel()

			release, failed := s.limits.acquire(ctx)
			if failed != nil {
				return refusal(failed), nil
			}
			defer release()

			res, err := next(ctx, method, req)
			if err != nil {
				return res, err
			}
			// The handler returned under a context that had already expired.
			// The SDK has packed whatever the facade said about it — often an
			// untyped error, which toolFailure reduces to CTX_INTERNAL — so
			// restate it as the typed outcome the deadline actually produced.
			//
			// This deliberately discards an answer that raced the deadline and
			// won: an answer produced after the caller's budget was spent is not
			// serviceable, and reporting the bound is more honest than shipping
			// a result whose freshness the bound no longer covers.
			if ctx.Err() != nil {
				return refusal(waitFailed(ctx, "in "+call.Params.Name)), nil
			}
			return res, nil
		}
	}
}

// refusal is the middleware's tool error. It goes through toolError so a bound
// refusal is byte-identical on the wire to the same code raised inside a
// handler: one error surface, not two.
func refusal(err *model.Error) *mcp.CallToolResult {
	return &mcp.CallToolResult{
		IsError: true,
		Content: []mcp.Content{&mcp.TextContent{Text: (&toolError{err: err}).Error()}},
	}
}
