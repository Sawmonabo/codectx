package mcpserver

import (
	"context"
	"errors"
	"log/slog"
	"os"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/Sawmonabo/codectx/internal/app"
	"github.com/Sawmonabo/codectx/internal/config"
	"github.com/Sawmonabo/codectx/internal/model"
)

// serverInstructions is the one block of prose a client sees before it calls
// anything. It is deliberately short: Section 19.2 requires compact labels so
// tool schemas do not consume unnecessary client context, and the same reason
// applies to the server's own instructions.
//
// It names no analysis engine: which engine produced a fact is an internal
// detail and never part of the wire vocabulary.
const serverInstructions = `codectx serves one workspace over stdio. Open a context session with ` +
	`codectx_context_plan, read the required files through codectx_read_source (the only tool that ` +
	`returns source bytes), acknowledge or waive each one, then close under the session's expected ` +
	`version. Every answer is a bounded page pinned to one index generation; failures are typed ` +
	`CTX_* tool errors, not protocol errors.`

const serverDescription = "Bounded, generation-pinned code context and review-gate tools for one workspace."

// Options is everything mcpserver.New needs. The three facade fields are the
// NARROW INTERFACES of internal/app, never *app.Services: *Services is a
// concrete struct and cannot be faked, and it satisfies all three, so
// internal/cli/mcp.go hands ws.Services() to each field while tests hand an
// in-package fake. There is no Diagnose field: Section 19.2 registers no doctor
// tool, so the server would hold a service no handler could call.
type Options struct {
	Index   app.IndexService
	Explore app.ExploreService
	Context app.ContextService

	// Config supplies the Section 20 bounds limitMiddleware and readSource
	// enforce. A zero value is not accepted: see New.
	Config config.Config
	// Build supplies the schema version every result envelope carries.
	Build model.BuildInfo
	// Spans registers a function to be called with every stage of an
	// indexing run as it finishes, and is how codectx_refresh_index reports
	// progress and logs stages while a run is going. It takes the rows in
	// their model shape rather than a recorder of its own, so what a client
	// is told a stage cost is the same row codectx_index_status returns and
	// the CLI prints -- there is no second accounting here.
	//
	// It is called ONCE, while the server is built. Nil is a server that
	// reports no progress, which is what a composition that records nothing
	// should be: the tools are unaffected.
	Spans func(func(model.StageRecord))
	// Logger MUST write to stderr. stdout is the SDK's framing channel; a
	// single byte of ours on it corrupts the session. Nil means "build the
	// stderr logger yourself".
	Logger *slog.Logger
}

// Server is the codectx MCP server: the SDK server with the 23 Section 19.2
// tools registered and the Section 19.3 bounds installed as receiving
// middleware.
//
// It owns no wire code. The SDK owns framing, dispatch, schema validation and
// tool-error packing (Section 19.1); writing an adapter beside it would be the
// duplicate implementation Section 30.1 forbids.
type Server struct {
	mcp *mcp.Server
	// h is the same *handlers register bound to every tool. limitMiddleware
	// reads its cfg, so the bounds and the handlers can never disagree about
	// which configuration is in force.
	h      *handlers
	limits limits
}

// New builds the server: metadata, options, the bounds middleware, then the 23
// tools. It registers no prompts, resources or completion handlers — Section
// 19.3 scopes V1 to tools — and sets no KeepAlive, which the SDK documents as
// unavailable for protocol versions >= 2026-07-28.
func New(opts Options) (*Server, error) {
	if opts.Index == nil || opts.Explore == nil || opts.Context == nil {
		return nil, &model.Error{
			Code:    model.CodeArgumentInvalid,
			Message: "mcpserver.New requires all three facade services",
		}
	}
	logger := opts.Logger
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(os.Stderr, nil))
	}

	s := &Server{h: &handlers{
		index:   opts.Index,
		explore: opts.Explore,
		context: opts.Context,
		cfg:     opts.Config,
		build:   opts.Build,
		log:     logger,
	}}
	// The bounds are resolved from the handlers' own cfg, not from a second
	// copy on Server: the gate and the handlers it gates can then never
	// disagree about which configuration is in force.
	lim, err := newLimits(s.h.cfg)
	if err != nil {
		return nil, err
	}
	s.limits = lim
	// One subscription for the process, fanned out per call: the source
	// publishes on a goroutine whose progress every live reader waits on, so
	// nothing downstream of it may block, and a per-call subscription would
	// need a source that could be unsubscribed.
	if opts.Spans != nil {
		s.h.spans = newSpanHub()
		opts.Spans(s.h.spans.publish)
	}
	s.mcp = mcp.NewServer(
		&mcp.Implementation{
			Name:        "codectx",
			Title:       "codectx",
			Description: serverDescription,
			Version:     opts.Build.Version,
		},
		&mcp.ServerOptions{
			Instructions: serverInstructions,
			Logger:       logger,
			// tools/list pages at the same bound every tool answer pages at,
			// so a client meets one page size, not two.
			PageSize: s.h.cfg.Resources.MaxPageItems,
		},
	)
	// Middleware first, tools second: AddReceivingMiddleware wraps the session's
	// method handler, so a call can never reach a tool without passing the
	// bounds, whatever order registration happens in.
	s.mcp.AddReceivingMiddleware(s.limitMiddleware())
	register(s.mcp, s.h)
	return s, nil
}

// Serve runs the server over stdio until ctx is canceled or the client
// disconnects.
//
// A canceled context is a CLEAN shutdown, not a failure: the SDK's Run returns
// ctx.Err() in that case, and turning it into a non-zero exit would make every
// ordinary SIGINT look like a crash. Only cancellation is treated that way — a
// serve context that expired is a truncated session, not a shutdown, and every
// other error is the caller's to report.
func (s *Server) Serve(ctx context.Context) error {
	err := s.mcp.Run(ctx, &mcp.StdioTransport{})
	if errors.Is(err, context.Canceled) {
		return nil
	}
	return err
}
