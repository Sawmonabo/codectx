// Package e2e holds the one end-to-end product-boundary scenario of Section
// 25.1. There is no production code in this package: it is this file plus one
// _test.go, and nothing imports it.
//
// What it exists for is the process boundary, and only that. internal/mcpserver
// already drives the tool set over mcp.NewInMemoryTransports, so a test here
// that constructed a server in-process would repeat a covered row while leaving
// the two things nobody else exercises untested: the built binary's Section
// 18.2 envelope on stdout, and `codectx mcp serve` answering a real client over
// real stdio pipes. Every assertion in this package therefore goes through an
// exec'd codectx process.
//
// The import rule a reviewer can check: this package imports neither
// internal/app, internal/cli nor internal/mcpserver -- reaching for any of the
// three would short-circuit the boundary the package is here to prove.
// internal/model is imported, and only for the shared wire vocabulary the two
// adapters already publish (model.SearchRequest, model.Page, model.Error); it
// is never used to reach a service.
package e2e
