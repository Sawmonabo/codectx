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

// lockPollInterval is how often a waiting acquisition of the workspace lock
// retries and re-reads the holder's progress.
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

// A HolderProbe answers, for the process that currently holds the workspace
// lock, whether it is still making progress and which stage it is in. It is
// INJECTED: the evidence of progress lives in the run ledger beside the index
// cache, and this package neither reads it nor knows it exists.
//
// holder is what the lock file says about the process being waited for, so a
// probe that must diagnose the holder can name it. Its Stage is the probe's own
// output and is always empty on the way in.
//
// progressing is the probe's answer to "has the holder shown a fresh stamp",
// judged by the holder's own published deadline and never recomputed from this
// process's configuration. stage is the innermost work the holder has named,
// empty when it has named none.
//
// A non-nil err ends the wait at once, and LockWorkspace returns it unchanged:
// it is the probe saying that no amount of waiting will make this holder
// readable, which is a different fact from "not progressing" and has a
// different remedy. The whole diagnosis belongs to the probe, because this
// package knows nothing about where the evidence lives; stage and progressing
// are then ignored.
type HolderProbe func(ctx context.Context, holder WaitingHolder) (stage string, progressing bool, err error)

// A WaitingHolder is what a waiter knows about the process it is waiting for:
// what that process recorded in the lock file, and the stage the probe last
// saw it in. PID is zero for a holder that recorded nothing.
//
// Waiting is false on exactly one call: the one that says the wait is over --
// acquired, refused, or ended by the probe's own diagnosis. An observer
// rewriting one line in a terminal needs to be told that, or it leaves the
// command's own first line of output appended to a progress line. It is a field
// rather than the struct's zero value because a holder that recorded nothing
// and has named no stage is otherwise indistinguishable from the end of the
// wait.
type WaitingHolder struct {
	Waiting   bool
	PID       int
	Operation string
	Stage     string
}

// LockWait is the policy an acquisition waits under. There are exactly two:
// TryOnce, and WaitWhileProgressing. It is a policy and not a duration because
// no duration is choosable -- any fixed wait is at once too long for a holder
// that has died and too short for one that is working -- and the question that
// IS answerable is whether the holder is still getting somewhere.
//
// The zero value is TryOnce, so a caller that leaves the field out is refused
// promptly rather than held indefinitely.
type LockWait struct {
	probe   HolderProbe
	observe func(WaitingHolder)
	grace   time.Duration
}

// TryOnce attempts the lock once and reports CTX_WORKSPACE_BUSY when another
// process holds it. It is what a caller answering somebody who is waiting for
// an answer now takes: a server's refresh tool tells the agent the workspace is
// busy rather than holding the call open behind the person's own index.
func TryOnce() LockWait { return LockWait{} }

// WaitWhileProgressing waits for as long as probe keeps saying the holder is
// getting somewhere, and reports CTX_WORKSPACE_BUSY only once it has said
// nothing for grace. No duration bounds it: a holder that keeps working is
// waited out however long it takes. Three things other than the grace end such
// a wait -- the holder letting go, the operator's interrupt, and the probe
// answering with an error, which is returned unchanged and immediately.
//
// grace is what makes this a progress judgement rather than a fixed wait in
// disguise. A holder legitimately shows no fresh stamp for a while -- between
// taking the lock and its collector's first flush, between two renewals, and
// after its run has been finalized while it still holds -- and a waiter that
// refused the instant nothing was advancing would refuse exactly the healthy
// holder this exists for. The caller passes the run ledger's own liveness
// window (ledger.LiveWindow), which already is this product's answer to how
// late a stamp may legitimately be; it is not a second number invented here,
// and nothing in this package may choose one.
//
// observe, when non-nil, is called whenever what it would say changes, and
// always once with the zero WaitingHolder when the wait ends. A wait the probe
// ends on its first answer is reported only by that closing call: there was
// never a wait to show, and printing a line to erase it at once would be worse
// than printing none.
func WaitWhileProgressing(grace time.Duration, probe HolderProbe, observe func(WaitingHolder)) LockWait {
	return LockWait{probe: probe, observe: observe, grace: grace}
}

// LockWorkspace acquires the lock for dataDir, creating the directory (0700)
// and lock file (0600) as needed. When the lock is held elsewhere it behaves as
// wait says: it tries once, or it retries while the holder progresses. Either
// way the failure is a retryable CTX_WORKSPACE_BUSY naming whoever recorded
// themselves in the file -- unless the probe itself ends the wait, in which
// case its error is what the caller gets, unchanged and with no grace spent.
//
// operation is what the acquiring process is doing, in the words a person would
// recognise on the refusal another process is about to read -- an index, a
// refresh, a watch. It is required: a holder that stayed anonymous would leave
// the waiter with the refusal that motivated recording a holder at all.
func LockWorkspace(ctx context.Context, dataDir, operation string, wait LockWait) (*WorkspaceLock, error) {
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
	// Reported to the observer exactly once per change, so a terminal gets one
	// line that is rewritten rather than one line per poll.
	var shown WaitingHolder
	var showing bool
	report := func(next WaitingHolder) {
		if wait.observe == nil || (showing && next == shown) {
			return
		}
		shown, showing = next, true
		wait.observe(next)
	}
	// The wait begins in progress: the holder has just been seen holding, and
	// the grace below is what the very first poll is measured against.
	lastProgress := time.Now()
	for {
		held, err := fslock.TryLock(f)
		if err != nil {
			f.Close()
			return nil, internal("workspace lock: %v", err)
		}
		if held {
			report(WaitingHolder{})
			l := &WorkspaceLock{f: f}
			l.record(operation)
			return l, nil
		}
		h, recorded := readHolder(f)
		refuse := func() (*WorkspaceLock, error) {
			report(WaitingHolder{})
			err := busy(h, recorded)
			f.Close()
			return nil, err
		}
		if wait.probe == nil {
			return refuse()
		}
		stage, progressing, probeErr := wait.probe(ctx, WaitingHolder{Waiting: true, PID: h.pid, Operation: h.operation})
		if probeErr != nil {
			// Not a refusal this package composes: the probe has diagnosed the
			// holder and its error is the answer. Reported as the end of the
			// wait first, so an observer rewriting one line closes it.
			report(WaitingHolder{})
			f.Close()
			return nil, probeErr
		}
		now := time.Now()
		if progressing {
			lastProgress = now
		}
		report(WaitingHolder{Waiting: true, PID: h.pid, Operation: h.operation, Stage: stage})
		if now.Sub(lastProgress) > wait.grace {
			return refuse()
		}
		select {
		case <-ctx.Done():
			report(WaitingHolder{})
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

// DetailHolderPID and DetailHolderOperation are the keys the busy refusal
// carries the holder under. They are named here, where the record is written,
// so a reader that acts on the holder -- a watch reporting the beat it
// skipped -- reads the same key this writes rather than a second spelling of
// it that can drift.
const (
	DetailHolderPID       = "holder_pid"
	DetailHolderOperation = "holder_operation"
)

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
	return e.WithDetail(DetailHolderPID, strconv.Itoa(h.pid)).WithDetail(DetailHolderOperation, h.operation)
}
