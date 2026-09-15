// Package toolchain owns every external analyzer and runtime the product
// executes (Section 11.7): the embedded lock that pins them, the private store
// they are installed into, the single network fetcher that brings a payload in,
// the confined extractor that unpacks it and the resolver that hands a caller a
// runnable tool.
//
// Nothing is looked up on PATH and nothing is installed by the user. A tool is
// runnable because the payload the shipped binary pinned hashes to the digest
// the lock records and its entry executable hashes to the digest the lock
// records for it. That check is repeated at every resolution, not once at
// install: a store directory is bytes on a disk the product does not own.
//
// A resolved Tool carries the digests it was verified against and renders them
// as Tool.Fingerprint(). Section 20.2 deliberately keeps `[tools]` out of the
// analysis configuration hash, so that fingerprint -- not configuration -- is
// what a consumer folds into UnitSpec.ProviderVersion and the LSP input digest,
// and it is why swapping a pinned analyzer invalidates the units it produced.
//
// This is the only package in the product that imports net/http. It fetches
// only URLs the embedded lock names -- or those URLs relocated under a
// configured mirror, which replaces the scheme and host and keeps the original
// host as the first path segment -- and `tools.offline` turns every fetch into a
// typed refusal without opening a socket.
//
// Data-directory layout owned by this package:
//
//	<data>/tools/                     store root, 0700
//	<data>/tools/<name>/<version>/    an installed payload tree
//	<data>/tools/<name>/<version>/.complete
//	                                  publication marker carrying the payload digest;
//	                                  a version directory without it is invisible and swept
//	<data>/tools/.staging/<name>/<id>/ a payload being fetched and extracted
//	<data>/tools/.locks/<name>.lock   cross-process serialization of one tool's installs
//
// The store root is whatever Options.StoreDir names, and in the product that
// is always set: a resolved configuration carries tools.cache_dir, which
// defaults to one machine-wide store every checkout on the host shares.
// StoreDir(DataDir) is the fallback for a caller that supplies only a data
// directory. Nothing below the root is created until the first install: a
// resolver built only to report touches no filesystem at all.
//
// The marker lives inside the version directory so that removing the directory
// removes the publication, and an archive entry named ".complete" at the
// payload root is rejected by the extractor so a payload can never forge one.
package toolchain

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"

	"github.com/Sawmonabo/codectx/internal/model"
)

const (
	storeDirName    = "tools"
	stagingDirName  = ".staging"
	locksDirName    = ".locks"
	completeName    = ".complete"
	lockFileSuffix  = ".lock"
	storeDirPerm    = 0o700
	storeFilePerm   = 0o600
	storeExecPerm   = 0o700
	lockFileMode    = 0o600
	markerFileMode  = 0o400
	maxMarkerBytes  = 128
	maxVersionBytes = 128
)

// StoreDir is the managed tool store under a data directory. It is the default
// only: Options.StoreDir overrides it with a store path used verbatim.
func StoreDir(dataDir string) string { return filepath.Join(dataDir, storeDirName) }

// Error helpers. Every failure leaving this package is a *model.Error, and none
// of them carries a filesystem path or a URL: Section 11.7 limits what a fetch
// may record to the tool's name, version, digest, byte count and elapsed time,
// and a data-directory path is not something an error may carry into logs.

func toolError(code, format string, args ...any) *model.Error {
	return &model.Error{Code: code, Message: fmt.Sprintf(format, args...)}
}

func internalError(format string, args ...any) *model.Error {
	return &model.Error{Code: model.CodeInternal, Message: fmt.Sprintf(format, args...)}
}

func invalid(format string, args ...any) *model.Error {
	return &model.Error{Code: model.CodeArgumentInvalid, Message: fmt.Sprintf(format, args...)}
}

func resourceLimit(format string, args ...any) *model.Error {
	return &model.Error{Code: model.CodeResourceLimit, Message: fmt.Sprintf(format, args...)}
}

// ioError maps a filesystem failure to its Section 22 family exactly as
// internal/snapshot does for the CAS: disk exhaustion is its own code,
// cancellation stays cancellation, everything else is internal, and only the
// operation and the operating-system reason survive -- never the path.
func ioError(op string, err error) error {
	var typed *model.Error
	if errors.As(err, &typed) {
		return err
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return model.Canceled(err)
	}
	if errors.Is(err, syscall.ENOSPC) {
		return &model.Error{Code: model.CodeDiskFull, Message: op + ": the tool store's disk is full",
			Remediation: "free disk space or point storage.data_dir at a larger volume"}
	}
	return internalError("%s: %v", op, bareCause(err))
}

// bareCause strips the path from the OS error wrappers, keeping only the
// operating-system reason.
func bareCause(err error) error {
	var pe *fs.PathError
	if errors.As(err, &pe) {
		return pe.Err
	}
	var le *os.LinkError
	if errors.As(err, &le) {
		return le.Err
	}
	var se *os.SyscallError
	if errors.As(err, &se) {
		return se.Err
	}
	return err
}
