// Package model holds the canonical data contracts shared by every codectx
// service. It depends on no other project package.
package model

import "runtime"

// SchemaVersion identifies the machine-readable output contract described in
// Section 18.2. It changes only when that contract changes incompatibly.
const SchemaVersion = "1"

// version and commit are overridden at link time with
// -ldflags "-X github.com/codectx-project/codectx/internal/model.version=...".
// No wall-clock build timestamp is embedded, so builds stay reproducible.
var (
	version = "dev"
	commit  = "unknown"
)

// BuildInfo describes the running binary. It is captured once at the process
// boundary and passed down; nothing reads link-time state further in.
type BuildInfo struct {
	Version       string
	Commit        string
	Toolchain     string
	SchemaVersion string
}

// CurrentBuildInfo reports the build identity of this binary.
func CurrentBuildInfo() BuildInfo {
	return BuildInfo{
		Version:       version,
		Commit:        commit,
		Toolchain:     runtime.Version(),
		SchemaVersion: SchemaVersion,
	}
}
