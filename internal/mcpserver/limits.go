package mcpserver

// L1 SERVER + BOUNDS owns this file. L0 freezes the contract here and
// implements none of it:
//
//	func (s *Server) limitMiddleware() mcp.Middleware
//
// Installed with (*mcp.Server).AddReceivingMiddleware, it enforces the Section
// 19.3 bounds that must exist before a handler runs (digest §6):
//
//   - the raw params frame is capped BEFORE decoding, at
//     resources.max_metadata_response_bytes; a post-decode check would already
//     have paid the allocation the bound exists to prevent;
//   - outstanding tool calls are bounded by a semaphore sized from
//     resources.max_concurrent_queries, with graph tools additionally bounded by
//     resources.max_concurrent_graph_queries — INDEPENDENTLY of parser
//     concurrency, which is a different resource;
//   - each call carries the resources.query_timeout deadline.
//
// Zero on a request field means "the configured default", never unlimited, and
// every default a zero resolves to is itself finite.
