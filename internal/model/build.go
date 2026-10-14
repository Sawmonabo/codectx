// Package model holds the canonical data contracts shared by every codectx
// service. It depends on no other project package.
package model

import (
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"runtime"
	"sync"
)

// SchemaVersion identifies the machine-readable output contract described in
// Section 18.2. It changes only when that contract changes incompatibly.
const SchemaVersion = "1"

// version and commit are overridden at link time with
// -ldflags "-X github.com/Sawmonabo/codectx/internal/model.version=...".
// No wall-clock build timestamp is embedded, so builds stay reproducible.
var (
	version = "dev"
	commit  = "unknown"
)

// BuildInfo describes the running binary. It is captured once at the process
// boundary and passed down; nothing reads link-time state further in.
type BuildInfo struct {
	Version       string `json:"version"`
	Commit        string `json:"commit"`
	Toolchain     string `json:"toolchain"`
	SchemaVersion string `json:"schema_version"`
}

// ToolVersion is the version of the code this binary runs, and the version of
// every provider it carries: a stored result is reused only under the version
// that wrote it. A release build is the release version it was stamped with.
// Any other build is "dev-" and the first 24 hex digits of the SHA-256 of its
// executable file, so a rebuild of any code is a new version. An upgrade or a
// rebuild therefore re-derives every stored result once. It is computed once
// per process, and a release build reads no file for it.
func ToolVersion() string { return toolVersion() }

var toolVersion = sync.OnceValue(func() string {
	if version != "dev" {
		return version
	}
	// On Linux /proc/self/exe is the file this process was started from even
	// after its path has been replaced.
	path := "/proc/self/exe"
	if runtime.GOOS != "linux" {
		p, err := os.Executable()
		if err != nil {
			panic("this build's version needs its executable, which cannot be named: " + err.Error())
		}
		path = p
	}
	f, err := os.Open(path)
	if err != nil {
		panic("this build's version needs its executable, which cannot be read: " + err.Error())
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		panic("this build's version needs its executable, which cannot be read: " + err.Error())
	}
	return "dev-" + hex.EncodeToString(h.Sum(nil))[:24]
})

// CurrentBuildInfo reports the build identity of this binary.
func CurrentBuildInfo() BuildInfo {
	return BuildInfo{
		Version:       version,
		Commit:        commit,
		Toolchain:     runtime.Version(),
		SchemaVersion: SchemaVersion,
	}
}
