package mcpserver

// L1 SERVER + BOUNDS owns this file. L0 freezes the contract here and
// implements none of it:
//
//	type Options struct {
//	    Index    app.IndexService
//	    Explore  app.ExploreService
//	    Context  app.ContextService
//	    Diagnose app.DiagnoseService
//	    Config   config.Config
//	    Build    model.BuildInfo
//	    Logger   *slog.Logger   // STDERR; stdout is the SDK's framing
//	}
//
//	func New(opts Options) (*Server, error)
//	func (s *Server) Serve(ctx context.Context) error
//
// New builds mcp.NewServer(&mcp.Implementation{Name: "codectx", Version:
// opts.Build.Version, Title, Description}, &mcp.ServerOptions{Instructions,
// Logger, PageSize: resources.max_page_items}), installs limitMiddleware via
// AddReceivingMiddleware and calls register(s, h) with a *handlers holding the
// four narrow interfaces. Serve runs it over mcp.StdioTransport{} and shuts
// down cleanly on context cancel, exiting 0.
//
// Gotchas that are already settled, so nobody re-litigates them:
//   - There is NO product-owned JSON-RPC framing. The SDK owns the wire
//     (Section 19.1); writing an adapter would be the duplicate implementation
//     Section 30.1 forbids.
//   - Do not set KeepAlive: it is documented as unavailable for protocol
//     versions >= 2026-07-28.
//   - Do not register prompts, resources or completion handlers: Section 19.3
//     scopes V1 to tools.
//   - ServerOptions.Logger must write to stderr. One fmt.Print anywhere in this
//     package corrupts the stdio session.
