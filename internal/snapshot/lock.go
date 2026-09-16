package snapshot

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Sawmonabo/codectx/internal/fslock"
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
// CTX_WORKSPACE_BUSY naming whoever recorded themselves in the file. wait <= 0
// tries exactly once.
//
// operation is what the acquiring process is doing, in the words a person would
// recognise on the refusal another process is about to read -- an index, a
// refresh, a watch. It is required: a holder that stayed anonymous would leave
// the waiter with the refusal that motivated recording a holder at all.
func LockWorkspace(ctx context.Context, dataDir, operation string, wait time.Duration) (*WorkspaceLock, error) {
	if !filepath.IsAbs(dataDir) {
		return nil, invalid("the data directory must be an absolute path")
	}
	// Refused before anything is opened or locked: this is a defect in the
	// caller, and screening it here means no acquisition that has already
	// succeeded can ever be undone by the record it was asked to write.
	if strings.TrimSpace(operation) == "" {
		return nil, internal("workspace lock: the acquiring operation must be named")
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
		held, err := fslock.TryLock(f)
		if err != nil {
			f.Close()
			return nil, internal("workspace lock: %v", err)
		}
		if held {
			l := &WorkspaceLock{f: f}
			l.record(operation)
			return l, nil
		}
		if !time.Now().Add(lockPollInterval).Before(deadline) {
			err := busy(readHolder(f))
			f.Close()
			return nil, err
		}
		select {
		case <-ctx.Done():
			f.Close()
			return nil, model.Canceled(ctx.Err())
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
		// The record goes before the lock does, and its failure is discarded:
		// a waiter that arrives between this release and the next acquisition
		// must be told that nobody recorded themselves rather than handed the
		// name of a process that has already let go.
		l.f.Truncate(0)
		if err := fslock.Unlock(l.f); err != nil {
			l.err = internal("workspace unlock: %v", err)
		}
		if err := l.f.Close(); err != nil && l.err == nil {
			l.err = internal("workspace lock close: %v", err)
		}
	})
	return l.err
}

// maxHolderRecord bounds both the record written into the lock file and the
// read that parses it back. The record is one line of two fields, so anything
// longer is a file this product did not write.
const maxHolderRecord = 256

// holder is what the process holding the lock recorded about itself: its
// process id and the operation it is running. A holder that recorded nothing --
// an empty file, a file this product did not write, a read that failed -- is
// reported as absent rather than guessed at.
//
// The record is a courtesy and the advisory lock is the contract. Nothing here
// may fail an acquisition, and a recorded pid is what was written and not a
// claim that the process is alive: the flock is that claim.
type holder struct {
	pid       int
	operation string
}

// record writes this holder into the lock file it has just acquired, as the one
// line `pid<space>operation`. Every error is discarded: the lock is already
// held, and a file the waiter cannot parse costs it the holder's name and
// nothing else.
func (l *WorkspaceLock) record(operation string) {
	// One line, whatever the caller passed: a name carrying a newline would
	// otherwise forge a second record for a reader to parse.
	operation = strings.Join(strings.Fields(operation), " ")
	line := strconv.Itoa(os.Getpid()) + " " + operation
	if len(line) > maxHolderRecord {
		line = line[:maxHolderRecord]
	}
	if l.f.Truncate(0) != nil {
		return
	}
	l.f.WriteAt([]byte(line), 0)
}

// readHolder parses the record out of the lock file. A waiter may read it while
// another process holds the advisory lock, which is the point: the refusal is
// written by the process that could not have the lock.
func readHolder(f *os.File) (holder, bool) {
	buf := make([]byte, maxHolderRecord)
	n, err := f.ReadAt(buf, 0)
	if n == 0 && err != nil {
		return holder{}, false
	}
	pidText, operation, split := strings.Cut(strings.TrimSpace(string(buf[:n])), " ")
	if !split {
		return holder{}, false
	}
	pid, convErr := strconv.Atoi(pidText)
	operation = strings.TrimSpace(operation)
	if convErr != nil || pid <= 0 || operation == "" {
		return holder{}, false
	}
	return holder{pid: pid, operation: operation}, true
}

// busy builds the refusal a waiter reports, naming the holder when one recorded
// itself. An unrecorded holder is stated as such: the lock is held either way,
// and inventing a holder would be worse than admitting there is no name.
func busy(h holder, recorded bool) *model.Error {
	e := &model.Error{Code: model.CodeWorkspaceBusy, Retryable: true,
		Remediation: "wait for the running index, refresh or watch process to finish, or stop it"}
	if !recorded {
		e.Message = "another codectx process holds the workspace indexing lock and did not record which process it is"
		return e
	}
	e.Message = "the " + h.operation + " running in process " + strconv.Itoa(h.pid) + " holds the workspace indexing lock"
	return e.WithDetail("holder_pid", strconv.Itoa(h.pid)).WithDetail("holder_operation", h.operation)
}
