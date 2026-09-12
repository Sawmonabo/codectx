// Package process owns the lifecycle of every child this application starts:
// Git, parser workers, SCIP indexers, language servers and deep analyzers all
// run through the one runner here. There is no second implementation, because
// process-tree termination, output bounding and environment control are only
// safe if they are applied uniformly.
//
// There is no shell. A Spec names an absolute executable and a literal argument
// array, so no value a repository controls can become an expansion, a glob or a
// second command. The child's environment is the allowlist the caller supplies
// and nothing else: the parent's environment, including whatever credentials it
// holds, is never inherited.
//
// Clearing the environment is not a network boundary. A child can still open a
// socket; denying that requires an OS sandbox, container or namespace, which is
// a deployment control this package does not provide and does not claim.
package process

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/Sawmonabo/codectx/internal/model"
)

// Spec is one child process to run. Every bound is explicit: Section 6 requires
// a finite limit on every process, stream and temporary allocation.
type Spec struct {
	// Path is the absolute path of an approved executable. A bare name is
	// rejected rather than resolved through PATH, because what PATH resolves to
	// is not what any profile approved.
	Path string
	// Args is the literal argument array, excluding argv[0].
	Args []string
	// Dir is the absolute private working directory for this run.
	Dir string
	// Env is the complete child environment as KEY=VALUE pairs. It is never
	// merged with the parent's environment; an empty Env means the child runs
	// with no environment at all.
	Env []string
	// Stdin is the optional bounded input. At most MaxStdinBytes are copied and
	// the child's stdin is then closed, so a child cannot stall the parent by
	// refusing to read.
	Stdin         io.Reader
	MaxStdinBytes int64
	// Stdout and Stderr optionally receive the streams. When either is nil the
	// stream is captured into Result instead. The byte limits apply in both
	// cases: a caller-supplied writer does not opt out of termination.
	Stdout         io.Writer
	Stderr         io.Writer
	MaxStdoutBytes int64
	MaxStderrBytes int64
	// Timeout bounds the whole run. Grace is how long the process tree has to
	// exit after a graceful stop before it is forced.
	Timeout time.Duration
	Grace   time.Duration
	// MemoryReservationBytes and DiskReservationBytes are the resources this
	// run is admitted against. They are accounting inputs, not enforcement: a
	// native child can temporarily exceed a reservation, and only an OS control
	// can prevent that.
	MemoryReservationBytes int64
	DiskReservationBytes   int64
}

// Result is the outcome of one run. It is returned even when the run failed, so
// a caller can record what was captured before the failure.
type Result struct {
	ExitCode int
	// Stdout and Stderr hold the captured bytes for the streams that had no
	// caller-supplied writer.
	Stdout []byte
	Stderr []byte
	// StdoutBytes and StderrBytes count what the child actually produced up to
	// the limit, including bytes handed to a caller-supplied writer.
	StdoutBytes int64
	StderrBytes int64
	Duration    time.Duration
	// TimedOut and Canceled distinguish the two reasons a tree is terminated;
	// Section 22 requires cancellation not to be reported as a crash.
	TimedOut bool
	Canceled bool
	// OutputTruncated reports that a stream reached its limit and the tree was
	// terminated for it.
	OutputTruncated bool
	// Signaled reports that the child was killed by a signal rather than
	// exiting, which is the normal outcome of a forced termination.
	Signaled bool
}

// Limits are the runner-wide admission bounds. All three are required: a zero
// bound would mean unlimited, which Section 20.2 forbids.
type Limits struct {
	MaxConcurrent     int
	MemoryBudgetBytes int64
	DiskBudgetBytes   int64
}

// Runner starts children under the platform's process-tree control and accounts
// their reservations. It is safe for concurrent use.
type Runner struct {
	limits Limits
	slots  chan struct{}

	mu         sync.Mutex
	memoryUsed int64
	diskUsed   int64
}

// NewRunner returns a runner bound by limits.
func NewRunner(limits Limits) (*Runner, error) {
	if limits.MaxConcurrent <= 0 || limits.MemoryBudgetBytes <= 0 || limits.DiskBudgetBytes <= 0 {
		return nil, resourceLimit("runner limits are concurrency %d, memory %d and disk %d; every bound must be positive",
			limits.MaxConcurrent, limits.MemoryBudgetBytes, limits.DiskBudgetBytes)
	}
	return &Runner{limits: limits, slots: make(chan struct{}, limits.MaxConcurrent)}, nil
}

// Run executes one child and returns when it and its whole process tree have
// exited. The tree is terminated on cancellation, on timeout and when either
// output stream reaches its limit.
func (r *Runner) Run(ctx context.Context, spec Spec) (Result, error) {
	if err := spec.validate(); err != nil {
		return Result{}, err
	}
	release, err := r.reserve(ctx, spec)
	if err != nil {
		return Result{}, err
	}
	defer release()
	return r.run(ctx, spec)
}

func (s Spec) validate() error {
	if !filepath.IsAbs(s.Path) || strings.ContainsRune(s.Path, 0) {
		return trustRequired("%q is not an absolute executable path; PATH resolution is not an approval", s.Path)
	}
	info, err := os.Stat(s.Path)
	if err != nil {
		return trustRequired("%q cannot be inspected: %v", s.Path, err)
	}
	if !info.Mode().IsRegular() {
		return trustRequired("%q is not a regular file", s.Path)
	}
	if !filepath.IsAbs(s.Dir) || strings.ContainsRune(s.Dir, 0) {
		return trustRequired("the working directory %q is not an absolute private directory", s.Dir)
	}
	for i, a := range s.Args {
		if strings.ContainsRune(a, 0) {
			return invalidArgument("args[%d] contains a NUL byte", i)
		}
	}
	for i, e := range s.Env {
		key, _, ok := strings.Cut(e, "=")
		if !ok || key == "" || strings.ContainsRune(e, 0) {
			return invalidArgument("env[%d] is not a KEY=VALUE pair", i)
		}
	}
	if s.Timeout <= 0 || s.Grace <= 0 {
		return resourceLimit("timeout %s and grace %s must both be positive; an unbounded child is never admitted", s.Timeout, s.Grace)
	}
	if s.MaxStdoutBytes <= 0 || s.MaxStderrBytes <= 0 {
		return resourceLimit("output limits are %d and %d; both must be positive", s.MaxStdoutBytes, s.MaxStderrBytes)
	}
	if s.Stdin != nil && s.MaxStdinBytes <= 0 {
		return resourceLimit("stdin is supplied without a positive byte bound")
	}
	if s.MemoryReservationBytes < 0 || s.DiskReservationBytes < 0 {
		return invalidArgument("reservations are memory %d and disk %d; neither may be negative",
			s.MemoryReservationBytes, s.DiskReservationBytes)
	}
	return nil
}

// reserve admits one run against the runner's concurrency and byte budgets.
// A reservation larger than the whole budget is refused immediately rather than
// waiting for capacity that can never appear.
func (r *Runner) reserve(ctx context.Context, spec Spec) (func(), error) {
	if spec.MemoryReservationBytes > r.limits.MemoryBudgetBytes {
		return nil, resourceLimit("the run reserves %d bytes of memory, over the runner budget of %d",
			spec.MemoryReservationBytes, r.limits.MemoryBudgetBytes)
	}
	if spec.DiskReservationBytes > r.limits.DiskBudgetBytes {
		return nil, resourceLimit("the run reserves %d bytes of disk, over the runner budget of %d",
			spec.DiskReservationBytes, r.limits.DiskBudgetBytes)
	}
	select {
	case r.slots <- struct{}{}:
	case <-ctx.Done():
		return nil, canceled(ctx.Err())
	}
	r.mu.Lock()
	overMemory := r.memoryUsed+spec.MemoryReservationBytes > r.limits.MemoryBudgetBytes
	overDisk := r.diskUsed+spec.DiskReservationBytes > r.limits.DiskBudgetBytes
	if overMemory || overDisk {
		r.mu.Unlock()
		<-r.slots
		return nil, resourceLimit("the run does not fit the remaining runner budget")
	}
	r.memoryUsed += spec.MemoryReservationBytes
	r.diskUsed += spec.DiskReservationBytes
	r.mu.Unlock()

	return func() {
		r.mu.Lock()
		r.memoryUsed -= spec.MemoryReservationBytes
		r.diskUsed -= spec.DiskReservationBytes
		r.mu.Unlock()
		<-r.slots
	}, nil
}

func (r *Runner) run(ctx context.Context, spec Spec) (Result, error) {
	started := time.Now()
	var result Result

	cmd := exec.Command(spec.Path, spec.Args...)
	cmd.Dir = spec.Dir
	// An os/exec Cmd with a nil Env inherits the parent's environment. The
	// child must never see it, so an empty allowlist is an empty environment,
	// not an absent one. Do not "simplify" this to cmd.Env = spec.Env.
	cmd.Env = spec.Env
	if cmd.Env == nil {
		cmd.Env = []string{}
	}

	job, err := newJobControl()
	if err != nil {
		return result, internalError("the process group cannot be created: %v", err)
	}
	defer job.close()
	if err := job.prepare(cmd); err != nil {
		return result, internalError("the process group cannot be configured: %v", err)
	}

	stdin, err := configureStdin(cmd, spec)
	if err != nil {
		return result, err
	}
	// The pipes are created here rather than through Cmd.StdoutPipe so that
	// this package owns both ends: closing the read end is what unblocks a
	// drain whose writer is a descendant that has not yet exited.
	outPipe, err := newStreamPipe(spec.Stdout, spec.MaxStdoutBytes, &cmd.Stdout)
	if err != nil {
		return result, err
	}
	defer outPipe.closeAll()
	errPipe, err := newStreamPipe(spec.Stderr, spec.MaxStderrBytes, &cmd.Stderr)
	if err != nil {
		return result, err
	}
	defer errPipe.closeAll()

	if err := cmd.Start(); err != nil {
		return result, unavailable("%s could not be started: %v", filepath.Base(spec.Path), err)
	}
	if err := job.started(cmd); err != nil {
		job.terminate(cmd, true)
		cmd.Wait()
		return result, internalError("the child could not be placed under process-tree control: %v", err)
	}
	// The parent's copies of the write ends must be closed or the drains never
	// see end of file, however promptly the child exits.
	outPipe.closeWriter()
	errPipe.closeWriter()

	// The two streams are drained concurrently: draining one after the other
	// deadlocks as soon as the child fills the pipe buffer of the other.
	drained := make(chan struct{})
	var drains sync.WaitGroup
	drains.Add(2)
	go func() { defer drains.Done(); outPipe.drain() }()
	go func() { defer drains.Done(); errPipe.drain() }()
	go func() { drains.Wait(); close(drained) }()
	if stdin != nil {
		go stdin.pump()
	}

	timer := time.NewTimer(spec.Timeout)
	defer timer.Stop()

	waiter := newWaiter(cmd)
	reason := waitForExit(ctx, timer.C, job, cmd, spec, outPipe, errPipe, waiter)
	waitErr := waiter.wait()

	// Reaping the direct child says nothing about its descendants: they may
	// still hold the write ends of the pipes. The drains are given the same
	// grace, and then their read ends are closed so no goroutine outlives the
	// run.
	select {
	case <-drained:
	case <-time.After(spec.Grace):
		outPipe.closeReader()
		errPipe.closeReader()
		<-drained
	}

	result.Duration = time.Since(started)
	result.Stdout, result.StdoutBytes = outPipe.captured()
	result.Stderr, result.StderrBytes = errPipe.captured()
	result.ExitCode, result.Signaled = exitStatus(waitErr)
	// Truncation is reported whenever it happened, including when the child
	// exited on its own just after crossing the limit.
	result.OutputTruncated = outPipe.limited() || errPipe.limited()

	switch reason {
	case stopOutputLimit:
		return result, resourceLimit("%s exceeded its output limit and was terminated", filepath.Base(spec.Path))
	case stopTimeout:
		result.TimedOut = true
		return result, timedOut("%s exceeded its %s timeout and was terminated", filepath.Base(spec.Path), spec.Timeout)
	case stopCanceled:
		result.Canceled = true
		return result, canceled(ctx.Err())
	}
	if err := outPipe.err(); err != nil {
		return result, err
	}
	if err := errPipe.err(); err != nil {
		return result, err
	}
	if waitErr != nil {
		return result, unavailable("%s exited with status %d", filepath.Base(spec.Path), result.ExitCode)
	}
	return result, nil
}

// jobControl is the platform's process-tree mechanism: a process group on Unix
// and a Job Object on Windows. The runner calls prepare before starting the
// child, started immediately after it is created, terminate to stop the whole
// tree gracefully and then forcibly, and close to release the mechanism.
//
// Both implementations control the entire tree. Signalling only the direct
// child would leave a tool's own workers running, still holding the
// materialization the caller believes is finished.
type jobControl interface {
	prepare(cmd *exec.Cmd) error
	started(cmd *exec.Cmd) error
	terminate(cmd *exec.Cmd, force bool)
	close()
}

// stopReason records why a tree was terminated, so the typed error and the
// Result flags agree.
type stopReason int

const (
	stopNone stopReason = iota
	stopCanceled
	stopTimeout
	stopOutputLimit
)

// waitForExit blocks until the child exits or something requires the tree to be
// stopped, then performs the graceful-then-forced termination.
func waitForExit(ctx context.Context, timeout <-chan time.Time, job jobControl, cmd *exec.Cmd,
	spec Spec, outPipe, errPipe *streamPipe, w *waiter) stopReason {
	var reason stopReason
	select {
	case <-w.exited:
		return stopNone
	case <-ctx.Done():
		reason = stopCanceled
	case <-timeout:
		reason = stopTimeout
	case <-outPipe.limitHit:
		reason = stopOutputLimit
	case <-errPipe.limitHit:
		reason = stopOutputLimit
	}

	// Graceful stop for the whole tree, then a forced one for whatever ignored
	// it. Both are addressed to the group or job, never to one process: a child
	// that spawned its own workers must not leave them running.
	w.terminate(job, cmd, false)
	select {
	case <-w.exited:
	case <-time.After(spec.Grace):
		w.terminate(job, cmd, true)
		<-w.exited
	}
	return reason
}

// waiter reaps the child exactly once and guards termination against it.
//
// A process group may be signalled safely only while the group still exists,
// and an unreaped leader is what keeps it alive. Once the child has been
// reaped its group may be gone, and its identifier could in principle name a
// different group later, so no signal is sent after that point.
type waiter struct {
	exited chan struct{}
	result chan error

	mu     sync.Mutex
	reaped bool
}

func newWaiter(cmd *exec.Cmd) *waiter {
	w := &waiter{exited: make(chan struct{}), result: make(chan error, 1)}
	go func() {
		err := cmd.Wait()
		w.mu.Lock()
		w.reaped = true
		w.mu.Unlock()
		w.result <- err
		close(w.exited)
	}()
	return w
}

func (w *waiter) wait() error { return <-w.result }

func (w *waiter) terminate(job jobControl, cmd *exec.Cmd, force bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.reaped {
		return
	}
	job.terminate(cmd, force)
}

// exitStatus extracts the child's exit code, reporting a signal death as the
// conventional 128+signal rather than as an unexplained -1.
func exitStatus(err error) (int, bool) {
	if err == nil {
		return 0, false
	}
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		return -1, false
	}
	if code := exitErr.ExitCode(); code >= 0 {
		return code, false
	}
	if sig, ok := signalOf(exitErr); ok {
		return 128 + sig, true
	}
	return -1, true
}

func configureStdin(cmd *exec.Cmd, spec Spec) (*stdinPump, error) {
	if spec.Stdin == nil {
		// A nil Stdin makes os/exec give the child the null device. It never
		// receives the parent's, so it can neither read the operator's terminal
		// nor block waiting for input that will not arrive.
		return nil, nil
	}
	pr, pw, err := os.Pipe()
	if err != nil {
		return nil, internalError("a stdin pipe cannot be created: %v", err)
	}
	cmd.Stdin = pr
	return &stdinPump{src: spec.Stdin, dst: pw, limit: spec.MaxStdinBytes, childEnd: pr}, nil
}

// stdinPump copies a bounded amount of input to the child and then closes its
// stdin, so a child waiting for more input sees end of file instead of hanging.
type stdinPump struct {
	src      io.Reader
	dst      *os.File
	childEnd *os.File
	limit    int64
}

func (p *stdinPump) pump() {
	io.Copy(p.dst, io.LimitReader(p.src, p.limit))
	p.dst.Close()
	p.childEnd.Close()
}

func trustRequired(format string, args ...any) *model.Error {
	return (&model.Error{Code: model.CodeTrustRequired, Message: fmt.Sprintf(format, args...)}).
		WithRemediation("Approve the tool with an absolute path and a version constraint in the user configuration.")
}

func resourceLimit(format string, args ...any) *model.Error {
	return &model.Error{Code: model.CodeResourceLimit, Message: fmt.Sprintf(format, args...)}
}

func timedOut(format string, args ...any) *model.Error {
	return &model.Error{Code: model.CodeProviderTimeout, Message: fmt.Sprintf(format, args...), Retryable: true}
}

func unavailable(format string, args ...any) *model.Error {
	return &model.Error{Code: model.CodeProviderUnavailable, Message: fmt.Sprintf(format, args...)}
}

func invalidArgument(format string, args ...any) *model.Error {
	return &model.Error{Code: model.CodeArgumentInvalid, Message: fmt.Sprintf(format, args...)}
}

func internalError(format string, args ...any) *model.Error {
	return &model.Error{Code: model.CodeInternal, Message: fmt.Sprintf(format, args...)}
}

// canceled reports a run the caller stopped.
//
// The result is joined rather than replaced: errors.Is still recognizes
// context.Canceled and context.DeadlineExceeded, so a caller can distinguish a
// deadline from a cancellation, while errors.As still finds a typed
// *model.Error. That typing is not cosmetic: internal/cli maps an untyped
// error to the exit-2 usage class, which would report a deliberate
// cancellation as an invalid command line.
//
// Section 22 lists no cancellation family, so this uses the deadline family of
// the same exit-7 class ("hard query/resource/deadline limit, or explicit
// incomplete work" in Section 18.2). A dedicated CTX_CANCELED code in
// internal/model would be more precise; see the Task 3 report.
func canceled(err error) error {
	if err == nil {
		err = context.Canceled
	}
	typed := &model.Error{
		Code:        model.CodeQueryDeadline,
		Message:     "the run was canceled before it completed",
		Remediation: "run the operation again when it should finish",
	}
	if errors.Is(err, context.DeadlineExceeded) {
		typed.Message = "the caller's deadline expired before the run completed"
	}
	return errors.Join(typed, err)
}
