package treesitter

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Sawmonabo/codectx/internal/model"
	"github.com/Sawmonabo/codectx/internal/process"
	"github.com/Sawmonabo/codectx/internal/provider/treesitter/wire"
)

// Worker lifetime bounds. Every worker is started through the shared runner
// with these as its Spec limits and is retired by the pool well before it
// could reach one, so a healthy worker is never killed by its own budget
// mid-parse: a parse never begins on a worker past three quarters of any
// bound. maxParsesPerWorker is defense in depth against any per-parse leak
// in the native binding: whatever leaks is released with the process.
const (
	workerLifetime     = time.Hour
	workerGrace        = 2 * time.Second
	workerStdinBudget  = int64(1) << 30
	workerStdoutBudget = int64(1) << 30
	workerStderrBytes  = int64(16) << 10
	maxParsesPerWorker = 2048
	helloTimeout       = 30 * time.Second
	stopTimeout        = workerGrace + 6*time.Second
)

// errWorkerGone is the pipe error the parent sees once the worker's run has
// ended, whatever the reason.
var errWorkerGone = errors.New("treesitter: parser worker exited")

// pool is the bounded lazy set of parser workers (Section 11.3, 23.4): at
// most max live at once, started on demand, kept idle for idleTTL, recycled
// when a lifetime bound approaches and torn down on cancellation.
//
// max bounds live processes, not concurrent parses. A worker occupies its
// place in live from before it is started until the runner has reaped it, so
// an idle worker, and one still shutting down, both still count. Bounding
// callers instead would let a caller start a fresh worker while an expiring
// one is still alive and still holding its runner slot and memory
// reservation, and the runner would refuse the admission the pool itself
// caused.
type pool struct {
	runner   *process.Runner
	cmd      WorkerCommand
	dir      string
	max      int
	idleTTL  time.Duration
	parseTTL time.Duration
	memory   int64

	mu sync.Mutex
	// cond wakes acquirers when a worker becomes idle, a process exits or the
	// pool closes: the three events that can let a waiting caller proceed.
	cond   *sync.Cond
	idle   []*worker
	live   map[*worker]bool
	closed bool
	wg     sync.WaitGroup

	started, exited, parses, retries uint64
}

// worker is one parser subprocess owned by the pool.
type worker struct {
	p      *pool
	ctx    context.Context
	cancel context.CancelFunc
	// in and out are the parent's ends of the worker's stdin and stdout;
	// childIn and childOut are the ends the runner gives the process.
	in       *io.PipeWriter
	out      *io.PipeReader
	childIn  *io.PipeReader
	childOut *io.PipeWriter
	done     chan struct{}
	// result and runErr are written by the run goroutine before done closes.
	result process.Result
	runErr error

	// pid and rss are written by the goroutine driving the worker and read by
	// stats from any goroutine, so both are atomic rather than guarded by the
	// pool lock the writers do not hold.
	pid atomic.Int64
	rss atomic.Uint64
	// The remaining fields belong to the goroutine that holds the worker
	// between acquire and release.
	parses   int
	bytesIn  int64
	bytesOut int64
	started  time.Time
	timer    *time.Timer
}

func newPool(runner *process.Runner, cmd WorkerCommand, dir string, max int, idleTTL, parseTTL time.Duration, memory int64) *pool {
	p := &pool{runner: runner, cmd: cmd, dir: dir, max: max, idleTTL: idleTTL, parseTTL: parseTTL, memory: memory,
		live: map[*worker]bool{}}
	p.cond = sync.NewCond(&p.mu)
	return p
}

// newWorker builds one worker's plumbing. Nothing here can fail and nothing
// here starts a process: it exists so a worker is never registered in the
// pool in a state where stopping it would use a nil pipe or cancel function.
func newWorker(p *pool) *worker {
	childIn, in := io.Pipe()
	out, childOut := io.Pipe()
	ctx, cancel := context.WithCancel(context.Background())
	return &worker{p: p, ctx: ctx, cancel: cancel, in: in, out: out, childIn: childIn, childOut: childOut,
		done: make(chan struct{}), started: time.Now()}
}

// acquire returns a worker to parse with: an idle one when the pool has one,
// otherwise a newly started process, waiting when max processes are already
// alive. It returns promptly on cancellation.
func (p *pool) acquire(ctx context.Context) (*worker, error) {
	p.mu.Lock()
	for {
		if p.closed {
			p.mu.Unlock()
			return nil, &model.Error{Code: model.CodeInternal, Message: "the treesitter provider is closed"}
		}
		if n := len(p.idle); n > 0 {
			w := p.idle[n-1]
			p.idle = p.idle[:n-1]
			w.timer.Stop()
			p.mu.Unlock()
			return w, nil
		}
		if len(p.live) < p.max {
			// The worker is fully constructed — pipes, context, cancel — and
			// takes its place in live and in the wait group under the same hold
			// close waits behind, so neither a second acquirer nor a concurrent
			// close can see a process about to start as absent or half-built.
			w := newWorker(p)
			p.live[w] = true
			p.started++
			p.wg.Add(1)
			p.mu.Unlock()
			if err := p.start(ctx, w); err != nil {
				return nil, err
			}
			return w, nil
		}
		if err := p.wait(ctx); err != nil {
			p.mu.Unlock()
			return nil, err
		}
	}
}

// wait blocks until the pool changes or ctx ends, releasing and retaking p.mu
// the way sync.Cond does. The context watch is what makes a cond wait
// cancelable: it broadcasts once ctx is done so the waiter re-checks.
func (p *pool) wait(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return model.Canceled(err)
	}
	stop := context.AfterFunc(ctx, func() {
		p.mu.Lock()
		defer p.mu.Unlock()
		p.cond.Broadcast()
	})
	p.cond.Wait()
	stop()
	if err := ctx.Err(); err != nil {
		return model.Canceled(err)
	}
	return nil
}

// release returns a worker after a parse. A worker that is unhealthy or has
// consumed its share of a lifetime bound is stopped; otherwise it goes idle
// under a TTL timer. Its place in live is given up only when the process has
// exited, which is what keeps live processes bounded.
func (p *pool) release(w *worker, healthy bool) {
	if !healthy || w.exhausted() {
		w.stop(!healthy)
		return
	}
	p.mu.Lock()
	if !p.live[w] {
		// The process died during or just after this parse — its own lifetime
		// timeout, a crash, an external kill — and the run goroutine has
		// already taken it out of live. Returning it to idle would break
		// idle ⊆ live, make BusyWorkers negative and hand the next caller a
		// dead worker, spending on a certain errWorkerGone the single retry a
		// real parse failure needs. It is still stopped rather than dropped:
		// that closes the parent's end of its stdin and waits out the reap,
		// so the run goroutine — and with it the pool's wait-group entry —
		// completes before this caller moves on.
		p.mu.Unlock()
		w.stop(false)
		return
	}
	if p.closed {
		p.mu.Unlock()
		w.stop(false)
		return
	}
	p.idle = append(p.idle, w)
	w.timer = time.AfterFunc(p.idleTTL, func() { p.expire(w) })
	p.cond.Broadcast()
	p.mu.Unlock()
}

// expire retires an idle worker whose TTL elapsed, unless it was reacquired.
func (p *pool) expire(w *worker) {
	p.mu.Lock()
	i := -1
	for j, x := range p.idle {
		if x == w {
			i = j
		}
	}
	if i >= 0 {
		p.idle = append(p.idle[:i], p.idle[i+1:]...)
	}
	p.mu.Unlock()
	if i >= 0 {
		w.stop(false)
	}
}

// close stops every worker and waits for the runner to reap each one. An idle
// worker exits on the end of its stdin, which takes up to the grace, so the
// idle set is stopped concurrently rather than one grace after another; the
// rest — busy or already shutting down — are killed.
func (p *pool) close() {
	p.mu.Lock()
	p.closed = true
	idle := p.idle
	p.idle = nil
	idleSet := make(map[*worker]bool, len(idle))
	for _, w := range idle {
		idleSet[w] = true
	}
	var busy []*worker
	for w := range p.live {
		if !idleSet[w] {
			busy = append(busy, w)
		}
	}
	p.cond.Broadcast()
	p.mu.Unlock()
	var stopping sync.WaitGroup
	for _, w := range idle {
		w.timer.Stop()
		stopping.Add(1)
		go func() {
			defer stopping.Done()
			w.stop(false)
		}()
	}
	stopping.Wait()
	for _, w := range busy {
		w.stop(true)
	}
	p.wg.Wait()
}

// start launches the registered worker w through the runner and reads its
// hello frame. w already holds its place in live and in the wait group; the
// run goroutine gives both up once the runner has reaped the process,
// whatever happens here.
func (p *pool) start(ctx context.Context, w *worker) error {
	spec := process.Spec{
		Path: p.cmd.Path, Args: p.cmd.Args, Dir: p.dir,
		Stdin: w.childIn, MaxStdinBytes: workerStdinBudget,
		Stdout: w.childOut, MaxStdoutBytes: workerStdoutBudget, MaxStderrBytes: workerStderrBytes,
		Timeout: workerLifetime, Grace: workerGrace, MemoryReservationBytes: p.memory,
	}
	go func() {
		defer p.wg.Done()
		w.result, w.runErr = p.runner.Run(w.ctx, spec)
		// Unblock any parent write or read: the worker cannot answer any more.
		w.childIn.CloseWithError(errWorkerGone)
		w.childOut.CloseWithError(errWorkerGone)
		p.mu.Lock()
		delete(p.live, w)
		// A worker can die while it sits idle — its lifetime timeout, a crash,
		// an external kill — and an exited worker is not reusable, so it leaves
		// the idle list with the live map. Otherwise idle could outnumber live
		// and the next caller would be handed a dead worker, spending on a
		// certain errWorkerGone the one retry a real parse failure needs. Its
		// TTL timer may still fire; expire finds nothing and does nothing.
		p.idle = slices.DeleteFunc(p.idle, func(x *worker) bool { return x == w })
		p.exited++
		p.cond.Broadcast()
		p.mu.Unlock()
		close(w.done)
	}()

	stop := w.watch(ctx, time.Now().Add(helloTimeout))
	kind, payload, err := wire.Read(w.out, wire.MaxFactFrameBytes)
	stop()
	var hello wire.Hello
	if err == nil {
		if kind != wire.KindHello {
			err = errors.New("first frame is not a hello")
		} else if err = json.Unmarshal(payload, &hello); err == nil && hello.Fingerprint != fingerprint {
			err = fmt.Errorf("worker fingerprint %s does not match the provider's %s", short(hello.Fingerprint), short(fingerprint))
		}
	}
	if err != nil {
		w.stop(true)
		// w.result and w.runErr belong to the run goroutine until done
		// closes, and stop gives up after stopTimeout without that having
		// happened, so they are read only behind the same guard stderrLine
		// uses. Reading them on the timed-out branch races the runner.
		select {
		case <-w.done:
			if w.runErr != nil {
				// The runner's typed refusal (trust, admission, start failure)
				// or termination explains the missing hello better than the pipe.
				return w.runErr
			}
		default:
			return &model.Error{Code: model.CodeProviderUnavailable,
				Message: "the parser worker did not start and did not exit within " + stopTimeout.String() + ": " + err.Error()}
		}
		if ctx.Err() != nil {
			return model.Canceled(ctx.Err())
		}
		return (&model.Error{Code: model.CodeProviderUnavailable, Message: "the parser worker did not start: " + err.Error()}).
			WithDetail("worker_stderr", w.stderrLine())
	}
	w.pid.Store(int64(hello.PID))
	return nil
}

// watch cancels the worker when ctx ends or the deadline passes before the
// returned stop function is called. Cancellation kills the process through
// the runner, which is the one cancellation path the worker has: it needs
// no in-process cancellation callback (see package worker).
func (w *worker) watch(ctx context.Context, deadline time.Time) (stop func()) {
	finished := make(chan struct{})
	var once sync.Once
	go func() {
		timer := time.NewTimer(time.Until(deadline))
		defer timer.Stop()
		select {
		case <-finished:
		case <-ctx.Done():
			w.kill()
		case <-timer.C:
			w.kill()
		case <-w.done:
		}
	}()
	return func() { once.Do(func() { close(finished) }) }
}

// kill terminates the worker through the runner and closes the parent's end
// of its stdin, so the runner's stdin copy sees end of file at once instead
// of waiting out its grace on a pipe nobody will write to again.
func (w *worker) kill() {
	w.cancel()
	w.in.Close()
}

// stop ends the worker: gracefully by closing its stdin (it exits on EOF) or
// forcibly through the runner, and waits a bounded time for the reap.
func (w *worker) stop(force bool) {
	if !force {
		w.in.Close()
		select {
		case <-w.done:
			return
		case <-time.After(workerGrace):
		}
	}
	w.kill()
	select {
	case <-w.done:
	case <-time.After(stopTimeout):
	}
}

// exhausted reports that another parse might cross a lifetime bound.
func (w *worker) exhausted() bool {
	return w.parses >= maxParsesPerWorker ||
		w.bytesIn > workerStdinBudget*3/4 ||
		w.bytesOut > workerStdoutBudget*3/4 ||
		time.Since(w.started) > workerLifetime*3/4
}

// stderrLine is the bounded first line of what the worker wrote to stderr,
// available once the run has ended; the worker never writes source there.
func (w *worker) stderrLine() string {
	select {
	case <-w.done:
	default:
		return ""
	}
	line, _, _ := strings.Cut(strings.TrimSpace(string(w.result.Stderr)), "\n")
	if len(line) > 256 {
		line = line[:256]
	}
	return line
}

// extraction is one parsed file as the worker reported it, bounded by the
// same per-file caps the worker applies so a misbehaving child cannot make
// the parent buffer more than a healthy one would send.
type extraction struct {
	decls   []wire.Decl
	imports []wire.Import
	refs    []wire.Ref
	done    wire.Done
}

// perFileError is a worker error frame: the file failed, the worker did not.
type perFileError struct{ err *model.Error }

func (e perFileError) Error() string { return e.err.Error() }
func (e perFileError) Unwrap() error { return e.err }

// parse runs one request on w. The returned error is a perFileError when the
// worker stays healthy, a cancellation or timeout when the caller's context
// ended (the worker was killed), and anything else when the worker broke
// protocol or died, in which case the caller retires it.
func (p *pool) parse(ctx context.Context, w *worker, req wire.Request, src []byte) (*extraction, error) {
	deadline := time.Now().Add(p.parseTTL)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	stop := w.watch(ctx, deadline)
	defer stop()
	ex, err := p.exchange(w, req, src)
	stop()
	p.mu.Lock()
	p.parses++
	p.mu.Unlock()
	w.parses++
	if err == nil {
		return ex, nil
	}
	var perFile perFileError
	if errors.As(err, &perFile) {
		return nil, err
	}
	if ctx.Err() != nil {
		return nil, model.Canceled(ctx.Err())
	}
	if time.Now().After(deadline) {
		return nil, &model.Error{Code: model.CodeProviderTimeout, Message: fmt.Sprintf("parsing %s exceeded %s", req.Path, p.parseTTL), Retryable: true}
	}
	return nil, err
}

// atRequestBound is the parent's half of the per-file record bound, read off
// the SAME request field the worker reads. The check exists to stop a
// misbehaving child from making the parent buffer more than a healthy one would
// send, so it must never be stricter than what the worker was told: a parent
// holding its own constant would kill a healthy worker's output the moment the
// operator raised the limit. A zero bound is unlimited and the check is a
// no-op, which is the shipped default.
func atRequestBound(req wire.Request, have int) bool {
	return req.MaxRecordsPerFile != 0 && uint64(have) >= uint64(req.MaxRecordsPerFile)
}

func (p *pool) exchange(w *worker, req wire.Request, src []byte) (*extraction, error) {
	if err := wire.WriteJSON(w.in, wire.KindRequest, req, wire.MaxFactFrameBytes); err != nil {
		return nil, err
	}
	if err := wire.Write(w.in, wire.KindSource, src); err != nil {
		return nil, err
	}
	w.bytesIn += int64(len(src)) + 64
	ex := &extraction{}
	for {
		kind, payload, err := wire.Read(w.out, wire.MaxFactFrameBytes)
		if err != nil {
			return nil, err
		}
		w.bytesOut += int64(len(payload)) + 5
		switch kind {
		case wire.KindDecl:
			var d wire.Decl
			if err := json.Unmarshal(payload, &d); err != nil {
				return nil, err
			}
			if atRequestBound(req, len(ex.decls)) {
				return nil, errors.New("worker exceeded the declaration bound")
			}
			ex.decls = append(ex.decls, d)
		case wire.KindImport:
			var i wire.Import
			if err := json.Unmarshal(payload, &i); err != nil {
				return nil, err
			}
			if atRequestBound(req, len(ex.imports)) {
				return nil, errors.New("worker exceeded the import bound")
			}
			ex.imports = append(ex.imports, i)
		case wire.KindRef:
			var r wire.Ref
			if err := json.Unmarshal(payload, &r); err != nil {
				return nil, err
			}
			if atRequestBound(req, len(ex.refs)) {
				return nil, errors.New("worker exceeded the reference bound")
			}
			ex.refs = append(ex.refs, r)
		case wire.KindDone:
			if err := json.Unmarshal(payload, &ex.done); err != nil {
				return nil, err
			}
			w.rss.Store(ex.done.RSSBytes)
			return ex, nil
		case wire.KindError:
			var e wire.Error
			if err := json.Unmarshal(payload, &e); err != nil {
				return nil, err
			}
			if !strings.HasPrefix(e.Code, "CTX_") || len(e.Code) > model.MaxIdentifierBytes {
				return nil, errors.New("worker error frame carries no diagnostic code")
			}
			return nil, perFileError{&model.Error{Code: e.Code, Message: "parser worker: " + bound(e.Message, model.MaxDetailBytes)}}
		default:
			return nil, fmt.Errorf("unexpected frame kind %d", kind)
		}
	}
}

// Stats is the aggregate parent-plus-worker resource view of Section 22:
// process and idle worker counts, lifetime counters and resident memory. A
// value that cannot be measured is -1, never zero.
type Stats struct {
	// Processes is every worker process the runner has not yet reaped: busy,
	// idle and shutting down alike. It never exceeds index.max_parser_workers.
	Processes int `json:"processes"`
	// IdleWorkers is the reusable subset of Processes, and BusyWorkers the
	// rest: parsing for a caller, or on their way out. The two are reported
	// separately because one number cannot say whether the pool is saturated
	// with work or merely holding warm processes.
	IdleWorkers    int    `json:"idle_workers"`
	BusyWorkers    int    `json:"busy_workers"`
	WorkersStarted uint64 `json:"workers_started"`
	WorkersExited  uint64 `json:"workers_exited"`
	Parses         uint64 `json:"parses"`
	Retries        uint64 `json:"retries"`
	// WorkerRSSBytes sums the resident set each live worker last reported.
	WorkerRSSBytes int64 `json:"worker_rss_bytes"`
	// ParentRSSBytes is this process's resident set.
	ParentRSSBytes int64 `json:"parent_rss_bytes"`
	// WorkerPIDs are the live workers, for an external check that they exit.
	WorkerPIDs []int `json:"worker_pids,omitempty"`
}

func (p *pool) stats() Stats {
	p.mu.Lock()
	defer p.mu.Unlock()
	// idle ⊆ live, each worker at most once: a worker enters idle only from
	// release while it is still live, and leaves it with the live map in the
	// run goroutine. If that ever broke, BusyWorkers would read negative and
	// acquire would hand out an exited worker. Clamping the number here would
	// report a plausible figure over a corrupt pool, so the inconsistency is
	// fatal instead — the cause is the defect, not the arithmetic.
	if len(p.idle) > len(p.live) {
		panic(fmt.Sprintf("treesitter: pool invariant broken: %d idle workers, %d live", len(p.idle), len(p.live)))
	}
	for _, w := range p.idle {
		if !p.live[w] {
			panic("treesitter: pool invariant broken: an idle worker is not live")
		}
	}
	s := Stats{Processes: len(p.live), IdleWorkers: len(p.idle), BusyWorkers: len(p.live) - len(p.idle),
		WorkersStarted: p.started, WorkersExited: p.exited, Parses: p.parses, Retries: p.retries, ParentRSSBytes: -1}
	if rss, ok := wire.ResidentBytes(); ok {
		s.ParentRSSBytes = rss
	}
	for w := range p.live {
		s.WorkerPIDs = append(s.WorkerPIDs, int(w.pid.Load()))
		rss := w.rss.Load()
		if rss == 0 || s.WorkerRSSBytes < 0 {
			s.WorkerRSSBytes = -1
			continue
		}
		s.WorkerRSSBytes += int64(rss)
	}
	return s
}

func short(s string) string {
	if len(s) > 12 {
		return s[:12]
	}
	return s
}
