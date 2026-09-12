package snapshot

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/Sawmonabo/codectx/internal/model"
)

// lockPollInterval is how often a bounded wait for the workspace lock retries.
const lockPollInterval = 100 * time.Millisecond

// WorkspaceLock is the cross-process indexing/GC coordination lock of
// Sections 12.3 and 13.2: one lock file under the data directory held with an
// OS advisory lock (flock on Unix, LockFileEx on Windows). Capture, indexing,
// publication, retention collection and startup recovery all take this same
// lock, so a collector can never see a capture in progress and two processes
// can never build competing generations. It lives here because capture is its
// first holder; internal/index and internal/retention reuse it.
//
// The lock is released by Close or by the process ending; the file itself is
// left in place. Holding it is per open file, not per process, so the outermost
// owner acquires it once and hands it down rather than each stage reacquiring.
type WorkspaceLock struct {
	f    *os.File
	once sync.Once
	err  error
}

// LockWorkspace acquires the lock for dataDir, creating the directory (0700)
// and lock file (0600) as needed. When the lock is held elsewhere it retries
// until wait has elapsed or ctx ends, then fails with a retryable
// CTX_WORKSPACE_BUSY. wait <= 0 tries exactly once.
func LockWorkspace(ctx context.Context, dataDir string, wait time.Duration) (*WorkspaceLock, error) {
	if !filepath.IsAbs(dataDir) {
		return nil, invalid("the data directory must be an absolute path")
	}
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return nil, ioError("data directory", err)
	}
	f, err := os.OpenFile(filepath.Join(dataDir, lockFileName), os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, ioError("workspace lock file", err)
	}
	deadline := time.Now().Add(wait)
	for {
		held, err := tryLock(f)
		if err != nil {
			f.Close()
			return nil, internal("workspace lock: %v", err)
		}
		if held {
			return &WorkspaceLock{f: f}, nil
		}
		if !time.Now().Add(lockPollInterval).Before(deadline) {
			f.Close()
			return nil, &model.Error{Code: model.CodeWorkspaceBusy, Retryable: true,
				Message:     "another codectx process holds the workspace indexing lock",
				Remediation: "wait for the running index, refresh or watch process to finish, or stop it"}
		}
		select {
		case <-ctx.Done():
			f.Close()
			return nil, canceled(ctx.Err())
		case <-time.After(lockPollInterval):
		}
	}
}

// Close releases the lock. It is idempotent.
func (l *WorkspaceLock) Close() error {
	if l == nil {
		return nil
	}
	l.once.Do(func() {
		if err := unlock(l.f); err != nil {
			l.err = internal("workspace unlock: %v", err)
		}
		if err := l.f.Close(); err != nil && l.err == nil {
			l.err = internal("workspace lock close: %v", err)
		}
	})
	return l.err
}
