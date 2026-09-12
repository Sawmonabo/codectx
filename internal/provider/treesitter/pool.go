package treesitter

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"sync"
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
type pool struct {
	runner   *process.Runner
	cmd      WorkerCommand
	dir      string
	idleTTL  time.Duration
	parseTTL time.Duration
	memory   int64
	slots    chan struct{}

	mu     sync.Mutex
	idle   []*worker
	live   map[*worker]bool
	closed bool
	wg     sync.WaitGroup

	started, exited, parses, retries uint64
}

// worker is one parser subprocess owned by the pool.
type worker struct {
	p      *pool
	cancel context.CancelFunc
	in     *io.PipeWriter
	out    *io.PipeReader
	done   chan struct{}
	// result and runErr are written by the run goroutine before done closes.
	result process.Result
	runErr error

	pid      int
	rss      uint64
	parses   int
	bytesIn  int64
	bytesOut int64
	started  time.Time
	timer    *time.Timer
}

func newPool(runner *process.Runner, cmd WorkerCommand, dir string, max int, idleTTL, parseTTL time.Duration, memory int64) *pool {
	return &pool{runner: runner, cmd: cmd, dir: dir, idleTTL: idleTTL, parseTTL: parseTTL, memory: memory,
		slots: make(chan struct{}, max), live: map[*worker]bool{}}
}

// acquire takes a worker slot, reusing an idle worker or starting one.
func (p *pool) acquire(ctx context.Context) (*worker, error) {
	select {
	case p.slots <- struct{}{}:
	case <-ctx.Done():
		return nil, model.Canceled(ctx.Err())
	}
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		<-p.slots
		return nil, &model.Error{Code: model.CodeInternal, Message: "the treesitter provider is closed"}
	}
	if n := len(p.idle); n > 0 {
		w := p.idle[n-1]
		p.idle = p.idle[:n-1]
		w.timer.Stop()
		p.mu.Unlock()
		return w, nil
	}
	p.mu.Unlock()
	w, err := p.start(ctx)
	if err != nil {
		<-p.slots
		return nil, err
	}
	return w, nil
}

// release returns a worker after a parse. A worker that is unhealthy or has
// consumed its share of a lifetime bound is stopped; otherwise it goes idle
// under a TTL timer.
func (p *pool) release(w *worker, healthy bool) {
	defer func() { <-p.slots }()
	if !healthy || w.exhausted() {
		w.stop(!healthy)
		return
	}
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		w.stop(false)
		return
	}
	p.idle = append(p.idle, w)
	w.timer = time.AfterFunc(p.idleTTL, func() { p.expire(w) })
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

// close stops every worker and waits for the runner to reap each one.
func (p *pool) close() {
	p.mu.Lock()
	p.closed = true
	idle := p.idle
	p.idle = nil
	var busy []*worker
	for w := range p.live {
		busy = append(busy, w)
	}
	p.mu.Unlock()
	for _, w := range idle {
		w.timer.Stop()
		w.stop(false)
	}
	for _, w := range busy {
		w.stop(true)
	}
	p.wg.Wait()
}

// start launches one worker through the runner and reads its hello frame.
func (p *pool) start(ctx context.Context) (*worker, error) {
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	wctx, cancel := context.WithCancel(context.Background())
	w := &worker{p: p, cancel: cancel, in: inW, out: outR, done: make(chan struct{}), started: time.Now()}
	spec := process.Spec{
		Path: p.cmd.Path, Args: p.cmd.Args, Dir: p.dir,
		Stdin: inR, MaxStdinBytes: workerStdinBudget,
		Stdout: outW, MaxStdoutBytes: workerStdoutBudget, MaxStderrBytes: workerStderrBytes,
		Timeout: workerLifetime, Grace: workerGrace, MemoryReservationBytes: p.memory,
	}
	p.mu.Lock()
	p.live[w] = true
	p.started++
	p.mu.Unlock()
	p.wg.Add(1)
	go func() {
		defer p.wg.Done()
		w.result, w.runErr = p.runner.Run(wctx, spec)
		// Unblock any parent write or read: the worker cannot answer any more.
		inR.CloseWithError(errWorkerGone)
		outW.CloseWithError(errWorkerGone)
		p.mu.Lock()
		delete(p.live, w)
		p.exited++
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
		if w.runErr != nil {
			// The runner's typed refusal (trust, admission, start failure)
			// or termination explains the missing hello better than the pipe.
			return nil, w.runErr
		}
		if ctx.Err() != nil {
			return nil, model.Canceled(ctx.Err())
		}
		return nil, (&model.Error{Code: model.CodeProviderUnavailable, Message: "the parser worker did not start: " + err.Error()}).
			WithDetail("worker_stderr", w.stderrLine())
	}
	w.pid = hello.PID
	return w, nil
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
			if len(ex.decls) >= wire.MaxDeclsPerFile {
				return nil, errors.New("worker exceeded the declaration bound")
			}
			ex.decls = append(ex.decls, d)
		case wire.KindImport:
			var i wire.Import
			if err := json.Unmarshal(payload, &i); err != nil {
				return nil, err
			}
			if len(ex.imports) >= wire.MaxImportsPerFile {
				return nil, errors.New("worker exceeded the import bound")
			}
			ex.imports = append(ex.imports, i)
		case wire.KindRef:
			var r wire.Ref
			if err := json.Unmarshal(payload, &r); err != nil {
				return nil, err
			}
			if len(ex.refs) >= wire.MaxRefsPerFile {
				return nil, errors.New("worker exceeded the reference bound")
			}
			ex.refs = append(ex.refs, r)
		case wire.KindDone:
			if err := json.Unmarshal(payload, &ex.done); err != nil {
				return nil, err
			}
			w.rss = ex.done.RSSBytes
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
// live and idle worker counts, lifetime counters and resident memory. A
// value that cannot be measured is -1, never zero.
type Stats struct {
	LiveWorkers    int    `json:"live_workers"`
	IdleWorkers    int    `json:"idle_workers"`
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
	s := Stats{LiveWorkers: len(p.live), IdleWorkers: len(p.idle), WorkersStarted: p.started, WorkersExited: p.exited,
		Parses: p.parses, Retries: p.retries, ParentRSSBytes: residentBytes("/proc/self/statm")}
	for w := range p.live {
		s.WorkerPIDs = append(s.WorkerPIDs, w.pid)
		if w.rss == 0 || s.WorkerRSSBytes < 0 {
			s.WorkerRSSBytes = -1
			continue
		}
		s.WorkerRSSBytes += int64(w.rss)
	}
	return s
}

// residentBytes reads an RSS from a statm file, -1 when unavailable.
func residentBytes(statm string) int64 {
	data, err := os.ReadFile(statm)
	if err != nil {
		return -1
	}
	fields := strings.Fields(string(data))
	if len(fields) < 2 {
		return -1
	}
	pages, err := strconv.ParseInt(fields[1], 10, 64)
	if err != nil {
		return -1
	}
	return pages * int64(os.Getpagesize())
}

func short(s string) string {
	if len(s) > 12 {
		return s[:12]
	}
	return s
}
