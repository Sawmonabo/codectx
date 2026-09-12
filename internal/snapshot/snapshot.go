// Package snapshot captures exact worktree bytes into an immutable manifest
// and the local content-addressed store, serves them back with verified
// integrity, and materializes private copies for external analyzers
// (Section 10).
//
// A snapshot is the bytes actually read from the worktree, never HEAD. Git is
// consulted only for membership, status and provenance through internal/vcs/
// git; every byte comes from the root-confined opener in internal/workspace
// and lands in the CAS before the manifest that names it is written. Reads go
// to the CAS at the recorded content hash and nowhere else: not the live
// checkout, not Git.
//
// Nothing here holds a repository-sized structure in the Go heap. Membership
// is joined and ordered in a private on-disk staging database that lives for
// one capture, the manifest is hashed and imported as a stream, and every read
// buffers at most a bounded number of 64-KiB blocks.
//
// Data-directory layout owned by this package:
//
//	<data>/workspace.lock   cross-process indexing/GC coordination lock
//	<data>/cas/hh/<hash>    immutable blobs named by validated SHA-256
//	<data>/cas/tmp/         private files being written before publication
//	<data>/staging/         per-capture staging databases
//	<data>/materialize/     private analyzer materializations
package snapshot

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/Sawmonabo/codectx/internal/model"
)

const (
	casDirName         = "cas"
	stagingDirName     = "staging"
	materializeDirName = "materialize"
	lockFileName       = "workspace.lock"
	stagingPrefix      = "capture-"
	materializePrefix  = "mat-"
	casTmpDirName      = "tmp"
	casTmpPrefix       = "put-"
)

// CASDir is the content-addressed store location under a data directory.
func CASDir(dataDir string) string { return filepath.Join(dataDir, casDirName) }

// StagingDir holds per-capture staging databases.
func StagingDir(dataDir string) string { return filepath.Join(dataDir, stagingDirName) }

// MaterializeDir holds private analyzer materializations.
func MaterializeDir(dataDir string) string { return filepath.Join(dataDir, materializeDirName) }

// Sweep is startup recovery for this package's temporary state: staging
// databases and unpublished CAS temporaries left by a crashed capture, and
// materializations whose owning process is gone. The caller holds the
// workspace lock, so no capture is in progress; a live materialization is
// recognized by the lock its owner still holds and is left alone. A Repair
// running outside the lock may lose its temporary to this sweep; CAS.publish
// reports that as a retryable busy condition. A file that cannot be removed is
// reported with the rest, never silently skipped.
func Sweep(dataDir string) error {
	var errs []error
	for _, dir := range []struct{ path, prefix string }{
		{StagingDir(dataDir), stagingPrefix},
		{filepath.Join(CASDir(dataDir), casTmpDirName), casTmpPrefix},
	} {
		entries, err := os.ReadDir(dir.path)
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			errs = append(errs, ioError("recovery listing", err))
		}
		for _, e := range entries {
			if !strings.HasPrefix(e.Name(), dir.prefix) {
				continue
			}
			if err := os.Remove(filepath.Join(dir.path, e.Name())); err != nil && !errors.Is(err, fs.ErrNotExist) {
				errs = append(errs, ioError("recovery cleanup", err))
			}
		}
	}
	if err := sweepMaterializations(MaterializeDir(dataDir)); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

// Error helpers. Every failure leaving this package is a *model.Error.

func internal(format string, args ...any) *model.Error {
	return &model.Error{Code: model.CodeInternal, Message: fmt.Sprintf(format, args...)}
}

func invalid(format string, args ...any) *model.Error {
	return &model.Error{Code: model.CodeArgumentInvalid, Message: fmt.Sprintf(format, args...)}
}

func integrity(format string, args ...any) *model.Error {
	return &model.Error{Code: model.CodeSourceIntegrity, Message: fmt.Sprintf(format, args...),
		Remediation: "run `codectx doctor --deep`; a missing blob with recorded Git provenance can be repaired explicitly"}
}

func resourceLimit(format string, args ...any) *model.Error {
	return &model.Error{Code: model.CodeResourceLimit, Message: fmt.Sprintf(format, args...)}
}

// ioError maps a filesystem failure to its Section 22 family: disk exhaustion
// is its own code, cancellation stays cancellation, everything else is
// internal. The message carries the operation and the bare cause (the errno
// behind a *fs.PathError, *os.LinkError or *os.SyscallError), never the path
// text: a repository or data-directory path is not something an error may
// carry into logs.
func ioError(op string, err error) error {
	var typed *model.Error
	if errors.As(err, &typed) {
		return err
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return model.Canceled(err)
	}
	if errors.Is(err, syscall.ENOSPC) {
		return &model.Error{Code: model.CodeDiskFull, Message: op + ": the data directory's disk is full",
			Remediation: "free disk space or move storage.data_dir to a larger volume"}
	}
	return internal("%s: %v", op, bareCause(err))
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

// typed maps any error leaving this package to a *model.Error chain: a bare
// context error from a walk or a staging query becomes the typed cancellation.
func typed(err error) error {
	if err == nil {
		return nil
	}
	var t *model.Error
	if errors.As(err, &t) {
		return err
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return model.Canceled(err)
	}
	return err
}
