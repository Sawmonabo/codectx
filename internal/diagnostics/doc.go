// Package diagnostics builds the Section 22 doctor report and the Section 23
// resource accounting block.
//
// Import rule, checkable by a reviewer: this package imports internal/model,
// internal/config, internal/toolchain (for toolchain.Status only), and the
// standard library. It must NOT import internal/app, internal/cli or
// internal/mcpserver -- internal/app composes this package, never the reverse.
// It must not import internal/storage/sqlite either: it depends on the narrow
// interfaces frozen in diagnostics.go, never on *sqlite.Store,
// *toolchain.Resolver or *app.Services, because a concrete struct cannot be
// faked and the doctor checks must be provable without a database on disk.
//
// This package opens no database and starts no process of its own. Every
// measurement arrives through a reader the composition root hands it.
package diagnostics
