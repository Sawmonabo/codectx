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
	"container/list"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
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
	// Stdin is the optional bounded input. At most MaxStdinBytes are copied --
	// zero means no bound -- and the child's stdin is then closed, so a child
	// cannot stall the parent by refusing to read. Unlike the output bounds,
	// a positive stdin bound the input exceeds fails the run: the child would
	// otherwise compute its answer from a prefix nobody agreed to cut. A Stdin that implements io.Closer is closed when the
	// run ends -- on every path, not only on a failure -- so the runner takes
	// ownership of it: hand it a reader whose lifetime is this run, never one
	// the caller reads again afterwards.
	Stdin         io.Reader
	MaxStdinBytes int64
	// Stdout and Stderr optionally receive the streams. When either is nil the
	// stream is captured into Result instead. MaxStdoutBytes and MaxStderrBytes
	// bound that capture buffer and nothing else: zero means no bound, and a
	// caller-supplied writer is never truncated, because its own blocking Write
	// is what paces the child. Crossing the bound drops the excess and sets
	// Result.OutputTruncated; it never terminates the tree and never fails the
	// run -- a stream longer than expected is a large repository, not a hang.
	Stdout         io.Writer
	Stderr         io.Writer
	MaxStdoutBytes int64
	MaxStderrBytes int64
	// Timeout bounds the whole run. Zero means no wall-clock bound: an
	// analysis unit on a monorepo is large, not wedged, and a clock cannot
	// tell the two apart. StallTimeout is what catches a wedged tree -- see
	// below -- and Grace is how long the process tree has to exit after a
	// graceful stop before it is forced.
	Timeout time.Duration
	Grace   time.Duration
	// StallTimeout is a hang detector, not a size limit: how long the tree may
	// make no observable progress at all before it is terminated with the stop
	// reason "stalled". Progress is any byte read from stdout or stderr --
	// counted before any output limit and whether or not the bytes are kept --
	// and, where the platform can sample a running tree, any advance of its
	// consumed CPU time. Zero disables the detector.
	//
	// It replaces the wall clock rather than supplementing it: however large
	// the repository, a wedged process still makes no progress.
	StallTimeout time.Duration
	// ProgressFiles are the outputs this run writes, named so the stall
	// detector can see a tool that writes its answer straight to a file and
	// says nothing on either pipe. Their sizes are summed into the progress
	// signal; a path is stated, never opened, and one that does not exist yet
	// contributes nothing. A named directory is summed one level deep over its
	// regular entries, which is how a step whose output is a directory of
	// files grows.
	//
	// Naming them is not optional for a quiet tool: without this signal such a
	// tool's only progress is its CPU time, which no platform but Linux
	// samples, so a StallTimeout would terminate it mid-work.
	ProgressFiles []string
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
	// OutputTruncated reports that captured bytes the child produced were
	// dropped because a capture bound was reached. What was captured is a
	// complete prefix; the run itself is unaffected.
	OutputTruncated bool
	// Signaled reports that the child was killed by a signal rather than
	// exiting, which is the normal outcome of a forced termination.
	Signaled bool
	// PeakTreeBytes is the highest summed resident set size observed over the
	// whole process tree while it ran, sampled at most treeSampleInterval
	// apart, and more often where a short StallTimeout polls faster. It is
	// the tree sum at one instant, never a sum of per-process historical peaks
	// reached at different instants (Section 22), and it is what the memory
	// governor reports as the observed figure (Section 11.6).
	PeakTreeBytes int64
	// TreeUnsampled reports that this platform cannot observe a running tree's
	// memory, so PeakTreeBytes is unavailable rather than zero. A caller that
	// publishes the figure must omit it, not publish a zero.
	TreeUnsampled bool
}

// treeSampleInterval is how often the process tree's resident memory is summed
// while the child runs. It is the sampling period of the method measured in
// docs/research/10-round3-empirical.md §1: fine enough to catch an analysis
// pass's peak, coarse enough that the sweep costs nothing against a run
// measured in minutes. It is the upper bound on the period, not the period
// itself: a run with a stall timeout short enough to poll faster than this
// samples at the watchdog's poll interval instead, so the CPU signal the
// watchdog reads is never staler than one of its own polls.
const treeSampleInterval = 250 * time.Millisecond

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

	mu         sync.Mutex
	memoryUsed int64
	diskUsed   int64
	// running counts the admissions in flight, against MaxConcurrent.
	running int
	// queue holds the runs waiting for headroom, in arrival order. A run that
	// does not fit the remaining budget waits in it rather than being refused;
	// see reserve and promote.
	queue list.List
	// live counts the children that have been started and whose run has not
	// returned. It is not the admission count: a run that is admitted and then
	// fails before exec has no process, and reporting one would describe memory
	// and descendants that do not exist. Section 23's live-subprocess figure is
	// what is running, not what was allowed to try. Its window is exactly the
	// window in which the run holds an admission slot, so the two accounts
	// cannot disagree -- including the one case where a child outlives the run
	// (it could not be killed), which the run reports as an error and which
	// this counter, like the admission, stops holding.
	live int64
}

// LiveSubprocesses reports how many children this runner has started whose run
// has not yet returned. It is the Section 23 figure, and diagnostics consumes
// it through a one-method interface.
func (r *Runner) LiveSubprocesses() int64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.live
}

// startedChild records a live child and returns the function that records its
// exit. It is called immediately after a successful start, and its result is
// deferred there, so every later failure -- a tree control that cannot hold the
// child, a timeout, a cancelled context, an output limit, a child that exits
// non-zero or one that is never reaped -- unwinds the counter on the way out. A
// counter that only decremented on a successful run would make a report claim
// more live children after every failure, until it claimed a machine full of
// processes that had all exited.
func (r *Runner) startedChild() func() {
	r.mu.Lock()
	r.live++
	r.mu.Unlock()
	return func() {
		r.mu.Lock()
		r.live--
		r.mu.Unlock()
	}
}

// NewRunner returns a runner bound by limits.
func NewRunner(limits Limits) (*Runner, error) {
	if limits.MaxConcurrent <= 0 || limits.MemoryBudgetBytes <= 0 || limits.DiskBudgetBytes <= 0 {
		return nil, resourceLimit("runner limits are concurrency %d, memory %d and disk %d; every bound must be positive",
			limits.MaxConcurrent, limits.MemoryBudgetBytes, limits.DiskBudgetBytes)
	}
	return &Runner{limits: limits}, nil
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
	if !isExecutable(info) {
		// A readable but non-executable path is not an approved tool. Starting
		// it would fail later with a bare EACCES that says nothing about which
		// profile named it.
		return trustRequired("%q is not executable", s.Path)
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
	if s.Timeout < 0 || s.StallTimeout < 0 {
		return resourceLimit("timeout %s and stall timeout %s may not be negative; zero means no bound", s.Timeout, s.StallTimeout)
	}
	if s.Grace <= 0 {
		// Grace is not a bound on the repository but the window a tree gets to
		// exit once it has been asked to, so it is always finite.
		return resourceLimit("grace %s must be positive", s.Grace)
	}
	if s.MaxStdoutBytes < 0 || s.MaxStderrBytes < 0 {
		return resourceLimit("output capture bounds are %d and %d; neither may be negative, and zero means no bound",
			s.MaxStdoutBytes, s.MaxStderrBytes)
	}
	if s.MaxStdinBytes < 0 {
		return resourceLimit("the stdin byte bound is %d; it may not be negative, and zero means no bound", s.MaxStdinBytes)
	}
	if s.MemoryReservationBytes < 0 || s.DiskReservationBytes < 0 {
		return invalidArgument("reservations are memory %d and disk %d; neither may be negative",
			s.MemoryReservationBytes, s.DiskReservationBytes)
	}
	return nil
}

// admission is one run's place in the admission queue. ready is closed when
// the run has been granted its slot and its byte reservations.
type admission struct {
	mem, disk int64
	ready     chan struct{}
	granted   bool
	elem      *list.Element
}

// reserve admits one run against the runner's concurrency and byte budgets. A
// run that does not fit the remaining headroom WAITS for it -- the budgets are
// memory admission, which schedules work rather than rejecting it. The only
// refusal left is the one no amount of waiting can clear: a reservation larger
// than the whole user-set budget, which is reported with both numbers.
//
// Why there is no deadlock. Concurrency and bytes are taken together under one
// lock, so a run never holds a slot while waiting for memory -- the shape that
// would let MaxConcurrent waiters block every runner that could free memory.
// Admission is strictly head-of-line: only the queue's front is considered, so
// a large reservation cannot starve behind an endless stream of small ones, and
// nothing behind the head can consume the headroom the head is waiting for.
// The head is always eventually satisfiable, because its reservations are
// individually within the whole budget (checked above) and every admitted run
// releases everything it took when it returns; once the last one does,
// running, memoryUsed and diskUsed are all zero and the head fits by
// construction. Nothing inside a run re-enters reserve, so no run waits on a
// run behind it. A waiter that never reaches the head still leaves on ctx.
//
// The trade-off this accepts: head-of-line admission can leave headroom idle
// while a large reservation waits. That is the cost of never starving one.
func (r *Runner) reserve(ctx context.Context, spec Spec) (func(), error) {
	if spec.MemoryReservationBytes > r.limits.MemoryBudgetBytes {
		return nil, resourceLimit("the run reserves %d bytes of memory, over the runner budget of %d",
			spec.MemoryReservationBytes, r.limits.MemoryBudgetBytes)
	}
	if spec.DiskReservationBytes > r.limits.DiskBudgetBytes {
		return nil, resourceLimit("the run reserves %d bytes of disk, over the runner budget of %d",
			spec.DiskReservationBytes, r.limits.DiskBudgetBytes)
	}
	w := &admission{mem: spec.MemoryReservationBytes, disk: spec.DiskReservationBytes, ready: make(chan struct{})}
	r.mu.Lock()
	w.elem = r.queue.PushBack(w)
	r.promote()
	granted := w.granted
	r.mu.Unlock()
	if !granted {
		select {
		case <-w.ready:
		case <-ctx.Done():
			r.mu.Lock()
			if !w.granted {
				r.queue.Remove(w.elem)
				w.elem = nil
				r.mu.Unlock()
				return nil, model.Canceled(ctx.Err())
			}
			r.mu.Unlock()
			// Granted in the same moment the context ended: the reservation is
			// held and must be handed back, or it leaks for the runner's life.
			r.release(w)
			return nil, model.Canceled(ctx.Err())
		}
	}
	return func() { r.release(w) }, nil
}

// promote grants queued runs in arrival order. It must be called with r.mu
// held, and stops at the first waiter that does not fit: see reserve for why
// nothing behind the head may overtake it.
func (r *Runner) promote() {
	for e := r.queue.Front(); e != nil; {
		w := e.Value.(*admission)
		if r.running >= r.limits.MaxConcurrent ||
			r.memoryUsed+w.mem > r.limits.MemoryBudgetBytes ||
			r.diskUsed+w.disk > r.limits.DiskBudgetBytes {
			return
		}
		r.running++
		r.memoryUsed += w.mem
		r.diskUsed += w.disk
		w.granted = true
		next := e.Next()
		r.queue.Remove(e)
		w.elem = nil
		close(w.ready)
		e = next
	}
}

// release hands back one admission and wakes whatever now fits.
func (r *Runner) release(w *admission) {
	r.mu.Lock()
	r.running--
	r.memoryUsed -= w.mem
	r.diskUsed -= w.disk
	r.promote()
	r.mu.Unlock()
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
	// Every return below goes through this: the descriptors are closed whether
	// or not the copy ever started, and a copy that did start is given a
	// bounded chance to finish. Without it a failed run leaks two descriptors
	// and parks a goroutine on a pipe nobody will ever read, which a
	// long-lived server does not survive.
	defer stdin.stop(spec.Grace)
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
	exited := r.startedChild()
	defer exited()
	if err := job.started(cmd); err != nil {
		// The child is outside the tree control that failed, so stopping the
		// group or job would reach nothing. It is killed directly, which is
		// also what unblocks the wait: on Windows it is still suspended, and
		// waiting on a suspended process that no job will terminate never
		// returns.
		job.terminate(cmd, true)
		if cmd.Process != nil {
			cmd.Process.Kill()
		}
		cmd.Wait()
		return result, internalError("the child could not be placed under process-tree control: %v", err)
	}
	// The child is a group leader, so its process ID is the group the sampler
	// follows and the runner terminates. Sampling starts only once the tree
	// control holds it, so nothing is sampled that could still escape it.
	// The CPU figure the stall watchdog reads is refreshed by this sampler, so
	// it must be refreshed at least as often as the watchdog polls: a stall
	// timeout short enough to poll faster than treeSampleInterval would
	// otherwise see an unchanged CPU count for a whole window and call a
	// computing tree wedged.
	sampleEvery := treeSampleInterval
	if spec.StallTimeout > 0 {
		sampleEvery = min(sampleEvery, stallPoll(spec.StallTimeout))
	}
	sampler := startTreeSampler(cmd.Process.Pid, sampleEvery)
	defer sampler.stopSampling()
	// The parent's copies of the write ends must be closed or the drains never
	// see end of file, however promptly the child exits.
	outPipe.closeWriter()
	errPipe.closeWriter()
	// The child now has its own copy of the stdin read end, so the parent's is
	// released; otherwise the child never sees end of file.
	stdin.start()

	// The two streams are drained concurrently: draining one after the other
	// deadlocks as soon as the child fills the pipe buffer of the other.
	drained := make(chan struct{})
	var drains sync.WaitGroup
	drains.Add(2)
	go func() { defer drains.Done(); outPipe.drain() }()
	go func() { defer drains.Done(); errPipe.drain() }()
	go func() { drains.Wait(); close(drained) }()

	// A zero timeout is no wall clock at all, and a nil channel blocks forever
	// in a select -- which is exactly the wanted behaviour. A zero-length timer
	// would instead fire immediately and kill every run.
	var deadline <-chan time.Time
	if spec.Timeout > 0 {
		timer := time.NewTimer(spec.Timeout)
		defer timer.Stop()
		deadline = timer.C
	}
	// The watchdog is joined before this function returns, like the sampler:
	// no goroutine this package starts outlives the run that started it.
	watchdog := startStallWatchdog(spec.StallTimeout, outPipe, errPipe, sampler, spec.ProgressFiles)
	defer watchdog.stopWatching()

	waiter := newWaiter(cmd)
	reason, unreaped := waitForExit(ctx, deadline, job, cmd, spec, watchdog, waiter)
	var waitErr error
	if !unreaped {
		waitErr = waiter.wait()
	}

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

	// The tree has exited, so the last sweep has already happened; stopping
	// joins the sampling goroutine before its peak is read.
	result.PeakTreeBytes = sampler.stopSampling()
	result.TreeUnsampled = !treeSampled
	result.Duration = time.Since(started)
	result.Stdout, result.StdoutBytes = outPipe.captured()
	result.Stderr, result.StderrBytes = errPipe.captured()
	result.ExitCode, result.Signaled = exitStatus(waitErr)
	// Truncation is reported whenever it happened, including when the child
	// exited on its own just after crossing the limit.
	result.OutputTruncated = outPipe.limited() || errPipe.limited()
	// The flags describe why the tree was stopped, and they are set from that
	// decision alone. A cancelled run whose child happened to overrun its
	// output while being torn down is still a cancelled run: Section 22
	// requires cancellation not to be reported as a failure, and a Result that
	// denied it would make the two disagree.
	result.TimedOut = reason == stopTimeout || reason == stopStalled
	result.Canceled = reason == stopCanceled

	if unreaped {
		// Nothing was reaped, so there is no status to report; claiming exit 0
		// would describe a process that may still be running as a clean run.
		result.ExitCode = -1
		pid := -1
		if cmd.Process != nil {
			pid = cmd.Process.Pid
		}
		return result, internalError("%s did not exit after being killed", filepath.Base(spec.Path)).
			WithDetail("pid", strconv.Itoa(pid)).
			WithDetail("stop_reason", reason.String())
	}
	// An explicit stop decision is why the run ended, so it names the error.
	switch reason {
	case stopTimeout:
		return result, timedOut("%s exceeded its %s timeout and was terminated", filepath.Base(spec.Path), spec.Timeout)
	case stopStalled:
		// A stall is a timeout in kind -- the tree was terminated for making no
		// progress -- so it reuses the typed code and is distinguished by the
		// stop reason, which is what a caller reports.
		return result, timedOut("%s made no progress for %s and was terminated", filepath.Base(spec.Path), spec.StallTimeout).
			WithDetail("stop_reason", stopStalled.String())
	case stopCanceled:
		return result, model.Canceled(ctx.Err())
	}
	if err := outPipe.err(); err != nil {
		return result, err
	}
	if err := errPipe.err(); err != nil {
		return result, err
	}
	if err := stdin.stop(spec.Grace); err != nil {
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
	stopStalled
)

func (r stopReason) String() string {
	switch r {
	case stopCanceled:
		return "canceled"
	case stopTimeout:
		return "timeout"
	case stopStalled:
		return "stalled"
	default:
		return "exited"
	}
}

// waitForExit blocks until the child exits or something requires the tree to be
// stopped, then performs the graceful-then-forced termination.
//
// The second result reports that the tree did not exit even after the forced
// termination. The wait is bounded because a process wedged in an
// uninterruptible kernel operation cannot be killed at all, and blocking on it
// would hang the caller with no diagnosis.
func waitForExit(ctx context.Context, timeout <-chan time.Time, job jobControl, cmd *exec.Cmd,
	spec Spec, watchdog *stallWatchdog, w *waiter) (stopReason, bool) {
	var reason stopReason
	select {
	case <-w.exited:
		return stopNone, false
	case <-ctx.Done():
		reason = stopCanceled
	case <-timeout:
		reason = stopTimeout
	case <-watchdog.stalledC():
		reason = stopStalled
	}

	// Graceful stop for the whole tree, then a forced one for whatever ignored
	// it. Both are addressed to the group or job, never to one process: a child
	// that spawned its own workers must not leave them running.
	w.terminate(job, cmd, false)
	select {
	case <-w.exited:
	case <-time.After(spec.Grace):
		w.terminate(job, cmd, true)
		select {
		case <-w.exited:
		case <-time.After(forcedExitGrace):
			return reason, true
		}
	}
	return reason, false
}

// forcedExitGrace bounds the wait for a tree that has already been killed.
const forcedExitGrace = 5 * time.Second

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
	return &stdinPump{src: spec.Stdin, dst: pw, limit: spec.MaxStdinBytes, childEnd: pr,
		done: make(chan struct{})}, nil
}

// stdinPump copies a bounded amount of input to the child and then closes its
// stdin, so a child waiting for more input sees end of file instead of hanging.
//
// Both descriptors belong to the pump once it has started and to the runner's
// deferred close until then, so neither leaks on any path.
type stdinPump struct {
	src      io.Reader
	dst      *os.File
	childEnd *os.File
	limit    int64

	done chan struct{}
	// failure is written by pump and read only after done is closed.
	failure error

	mu        sync.Mutex
	started   bool
	closeOnce sync.Once
	stopOnce  sync.Once
	stopErr   error
}

// start releases the parent's copy of the child's end and begins the copy. It
// is a no-op for a run with no stdin.
func (p *stdinPump) start() {
	if p == nil {
		return
	}
	p.mu.Lock()
	p.started = true
	p.mu.Unlock()
	// The child holds its own copy from here on; the parent's would otherwise
	// keep the pipe open forever.
	p.childEnd.Close()
	go p.pump()
}

// stop releases both ends and reports the copy error, once, from whichever
// return path reaches it first.
//
// A copy that has started is given grace to finish on its own; after that the
// write end is closed, which is what unblocks a copy parked on a pipe that a
// surviving descendant still holds open. If even that does not free it, the
// goroutine is blocked inside the caller's own Reader, where nothing here can
// reach it: the run returns anyway rather than waiting on it forever.
func (p *stdinPump) stop(grace time.Duration) error {
	if p == nil {
		return nil
	}
	p.stopOnce.Do(func() {
		p.mu.Lock()
		started := p.started
		p.mu.Unlock()
		if !started {
			p.closeBoth()
			return
		}
		select {
		case <-p.done:
			p.stopErr = p.failure
			return
		case <-time.After(grace):
		}
		p.closeBoth()
		select {
		case <-p.done:
			p.stopErr = p.failure
		case <-time.After(grace):
			// failure is not read here: the pump may still be running, and
			// reading what it writes would be a race.
		}
	})
	return p.stopErr
}

func (p *stdinPump) closeBoth() {
	p.closeOnce.Do(func() {
		// A caller-supplied reader is the only thing the pump can still be
		// parked inside after both pipe ends are gone; closing it is what
		// releases that goroutine.
		if c, ok := p.src.(io.Closer); ok {
			c.Close()
		}
		p.dst.Close()
		p.childEnd.Close()
	})
}

func (p *stdinPump) pump() {
	defer close(p.done)
	src := p.src
	if p.limit > 0 {
		src = io.LimitReader(p.src, p.limit)
	}
	n, err := io.Copy(p.dst, src)
	if err == nil && p.limit > 0 && n == p.limit {
		// io.Copy stops at the bound without saying whether anything was left.
		// Silently truncating a child's input would produce a result computed
		// from bytes the caller never agreed to drop.
		var probe [1]byte
		if extra, _ := p.src.Read(probe[:]); extra > 0 {
			err = resourceLimit("stdin exceeded its %d-byte bound", p.limit)
		}
	}
	if err != nil && !isBrokenPipe(err) {
		// A child that closes stdin early is normal and not a failure; any
		// other copy error means the child received input nobody can account
		// for.
		p.failure = err
	}
	p.closeBoth()
}

// isBrokenPipe reports the ordinary "the child stopped reading" errors.
func isBrokenPipe(err error) bool {
	return errors.Is(err, syscall.EPIPE) ||
		errors.Is(err, io.ErrClosedPipe) ||
		errors.Is(err, os.ErrClosed)
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
