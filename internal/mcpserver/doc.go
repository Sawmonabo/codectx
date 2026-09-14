// Package mcpserver exposes the Section 19.2 tool surface of codectx over the
// Model Context Protocol, using the pinned official Go SDK for every wire
// concern.
//
// # Import rule (Task 19 digest §3) — checkable by a reviewer
//
// This package imports exactly:
//
//   - the standard library;
//   - github.com/modelcontextprotocol/go-sdk/mcp — and no other SDK package.
//     In particular never auth, oauthex, sse, streamable or jsonrpc: Section
//     19.3 scopes V1 to tools over stdio, so "no HTTP listener, authentication
//     server, sampling loop or remote transport" is enforced as an import rule
//     rather than promised in prose, and golang-jwt/oauth2 stay out of the
//     build graph;
//   - github.com/google/jsonschema-go/jsonschema — for the ONE input-schema
//     helper in registry.go only (digest §2). It is the schema library the SDK
//     itself infers with, not a second protocol implementation;
//   - internal/model — the request, response and error contracts;
//   - internal/config — the Section 20 bounds;
//   - internal/app — ONLY for the four narrow facade interfaces and their
//     model-typed signatures. app.OpenWorkspace belongs to internal/cli/mcp.go,
//     not here.
//
// internal/app must not import this package, and this package must not import
// internal/cli: the CLI envelope and exit codes are a different adapter's
// shape, and there is no second error-code table (digest §5).
//
// # Channel discipline
//
// stdout is reserved for SDK framing. There is no fmt.Print, no banner and no
// log line on stdout anywhere in this package; every diagnostic goes to the
// stderr *slog.Logger carried on handlers. A single stray stdout write
// corrupts the stdio session.
//
// # File ownership (Task 19 lane plan)
//
//	doc.go, registry.go, handlers.go, result.go  L0
//	server.go, limits.go                         L1
//	explore.go                                   L2
//	graph.go                                     L3
//	session.go                                   L4
//	gate.go                                      L5
//	internal/cli/mcp.go                          INT
package mcpserver
