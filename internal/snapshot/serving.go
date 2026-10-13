package snapshot

import (
	"os"
	"path/filepath"
	"sync"

	"github.com/Sawmonabo/codectx/internal/fslock"
	"github.com/Sawmonabo/codectx/internal/model"
)

// servingLockFileName is the presence lock beside the workspace lock.
const servingLockFileName = "serving.lock"

// ServingLockPath is the presence lock under a data directory: the lock every
// long-lived session open on the workspace -- an MCP server, a watch -- holds
// SHARED for its whole life, and a reset takes EXCLUSIVE while it removes.
//
// It is a file of its own, not the workspace lock taken shared: a session
// that held the workspace lock shared would refuse every index another
// process starts, which is exactly what such a session must never do while it
// is idle between the things it builds; and one process that held it shared
// could not take it exclusive through its own next acquisition. Indexing never
// touches this file. Like the workspace lock, it lives inside the directory it
// guards and a reset leaves it in place.
func ServingLockPath(dataDir string) string { return filepath.Join(dataDir, servingLockFileName) }

// ServingLock is one hold of the presence lock, shared or exclusive. It is
// released by Close or by the process ending.
type ServingLock struct {
	f    *os.File
	once sync.Once
	err  error
}

// HoldServing takes the presence lock shared for a session that stays open on
// dataDir, creating the directory (0700) and the file (0600) as needed. It
// does not wait: a reset holding it exclusive is emptying the workspace, and a
// session opened on it now would open what is being removed, so the session
// is refused with a retryable CTX_WORKSPACE_BUSY.
func HoldServing(dataDir string) (*ServingLock, error) {
	return takeServing(dataDir, fslock.TryLockShared, func() *model.Error {
		return &model.Error{Code: model.CodeWorkspaceBusy, Retryable: true,
			Message:     "a reset is emptying this workspace's data directory",
			Remediation: "start the session again once the reset has finished"}
	})
}

// LockServingExclusive takes the presence lock exclusive for a reset. It does
// not wait: a session holding it shared is open on the workspace -- possibly
// idle, holding no workspace lock -- and removing its data under it would
// leave it serving files that are gone. Several sessions can hold it at once,
// so the refusal names none of them.
func LockServingExclusive(dataDir string) (*ServingLock, error) {
	return takeServing(dataDir, fslock.TryLock, func() *model.Error {
		return &model.Error{Code: model.CodeWorkspaceBusy, Retryable: true,
			Message:     "an MCP server or a watch is open on this workspace",
			Remediation: "stop every `codectx mcp serve` and `codectx watch` of this repository, then run the reset again"}
	})
}

func takeServing(dataDir string, try func(*os.File) (bool, error), busy func() *model.Error) (*ServingLock, error) {
	if !filepath.IsAbs(dataDir) {
		return nil, invalid("the data directory must be an absolute path")
	}
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return nil, ioError("data directory", err)
	}
	f, err := os.OpenFile(ServingLockPath(dataDir), os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, ioError("serving lock file", err)
	}
	held, err := try(f)
	if err != nil {
		f.Close()
		return nil, internal("serving lock: %v", err)
	}
	if !held {
		f.Close()
		return nil, busy()
	}
	return &ServingLock{f: f}, nil
}

// Close releases the hold. It is idempotent and safe on a nil lock.
func (l *ServingLock) Close() error {
	if l == nil {
		return nil
	}
	l.once.Do(func() {
		if err := fslock.Unlock(l.f); err != nil {
			l.err = internal("serving unlock: %v", err)
		}
		if err := l.f.Close(); err != nil && l.err == nil {
			l.err = internal("serving lock close: %v", err)
		}
	})
	return l.err
}
