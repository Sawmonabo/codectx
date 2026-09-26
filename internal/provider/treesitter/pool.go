package treesitter

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Sawmonabo/codectx/internal/admission"
	"github.com/Sawmonabo/codectx/internal/ledger"
	"github.com/Sawmonabo/codectx/internal/model"
	"github.com/Sawmonabo/codectx/internal/process"
	"github.com/Sawmonabo/codectx/internal/provider/treesitter/wire"
)

// A worker has no wall-clock bound of any kind: no lifetime, no per-parse
// deadline, no parse count and no lifetime byte total. The large generated
// file whose size nothing limits is a long parse, not a hung one, and only
// progress can tell the two apart (see hangWindow).
const (
	// workerGrace is the runner's Spec.Grace and the pool's own wait after
	// closing a worker's stdin: how long a worker that has been told to exit
	// may take to do so before it is forced. It bounds an exit, never work --
	// a worker is only ever stopped when it has nothing left to do.
	workerGrace = 2 * time.Second
	// workerStderrBytes bounds the stderr capture buffer, a finite bound on a
	// buffer and not on work: a worker writes a diagnostic there, never
	// source, only its first line is ever read (stderrLine), and the runner
	// drops the excess without ending the run.
	workerStderrBytes = int64(16) << 10
	// hangWindow is the hang detector's window, not a size limit and not a
	// deadline: while a request or the hello is outstanding, a worker that has
	// neither consumed processor time nor sent a byte for this long is
	// declared hung and killed. A parse of any size computes, so its processor
	// time moves for as long as it runs; a minute without one clock tick of
	// it and without one byte on the wire is a worker that is not running.
	// That holds where the platform samples processor time; where it does
	// not, the detector never fires (see watchProgress). It is many times the runner's tree sampling period, so a computing
	// worker is always seen to move within one window.
	hangWindow = time.Minute
)

// errWorkerGone is the pipe error the parent sees once the worker's run has
// ended, whatever the reason.
var errWorkerGone = errors.New("treesitter: parser worker exited")

// pool is the bounded lazy set of parser workers (Section 11.3, 23.4): at
// most max live at once, started on demand, reused while there is parse work
// in flight, drained when there is none, replaced when one fails and torn
// down on cancellation.
//
// max bounds live processes, not concurrent parses. A worker occupies its
// place in live from before it is started until the runner has reaped it, so
// an idle worker, and one still shutting down, both still count. Bounding
// callers instead would let a caller start a fresh worker while a retiring
// one is still alive and still holding its runner slot and memory
// reservation, and the runner would refuse the admission the pool itself
// caused.
//
// Every worker is admitted on the process's one reservation ledger before it
// is started and gives its reservation back only once the runner has reaped
// it, so parser workers and every other heavy child of this process are
// admitted against one allocation and one running total.
//
// Every admission is first-in-first-out on the ledger. A worker coming back
// from a parse while an acquirer of this pool is queued there goes one of two
// ways, both in the ledger's order:
//
//   - When that acquirer is the ledger's head, the worker and the reservation
//     it holds are handed to it. The head is next in line for exactly that
//     room, so the handout is the grant the ledger would make, without
//     stopping one process to start the same one again. The ledger tells an
//     acquirer it is the head that does not fit through the make-room step;
//     no other reserver can come before it from then on.
//   - When the head is another reserver -- a unit, a language server, another
//     pool -- the worker is stopped: its reservation returns to the ledger,
//     which pumps, and that head is admitted in order. This pool's acquirer
//     behind it waits its turn.
//
// No idle worker is reused while an acquirer of this pool is queued, since
// release never idles one then. With none queued, a worker coming back goes
// idle and is held for the stage like any room an admitted child holds; the
// ledger does not tell a room holder that someone is waiting, so a reserver
// that arrives while this pool holds idle workers waits behind them until a
// release of this pool stops one or the stage's drain stops them all. That is
// the order admission itself keeps -- room is returned by its holder, never
// taken -- and it is the one point where a warm worker is kept rather than
// given up.
//
// Kept idle while an acquirer of this pool is queued, a worker would hold the
// room that acquirer waits for while nothing ever pumps the ledger, and the
// acquirer, its unit and the stage it keeps from draining would wait forever.
type pool struct {
	runner    *process.Runner
	admission *admission.Ledger
	cmd       WorkerCommand
	dir       string
	max       int
	memory    int64

	mu sync.Mutex
	// cond wakes acquirers when a worker becomes idle, a process exits, a
	// reservation is handed back or the pool closes: the events that can let a
	// waiting caller proceed.
	cond   *sync.Cond
	idle   []*worker
	live   map[*worker]bool
	closed bool
	// queued are the acquirers waiting on the ledger for a worker, in the
	// order they asked. They count against max beside live, so no more
	// reservations are asked for than the pool may start processes, and they
	// are not in live, because no process of theirs exists yet for close to
	// stop. While one is waiting and not yet handed a worker, release never
	// idles a worker, so idle is empty for as long as any acquirer is queued.
	queued []*acquirer
	wg     sync.WaitGroup
	// totals is the open structural-parse total of every run parsing through
	// this pool, which is what a worker's span hangs off. It is keyed by the
	// run and not held as one span because a deferred publication's tick runs
	// units of its own, and its workers belong to its run rather than to
	// whatever index run happens to be live.
	totals map[*ledger.Run]*stageTotal

	started, exited, parses, retries uint64
}

// acquirer is one caller of acquire waiting on the ledger. Its fields are
// guarded by the pool lock.
type acquirer struct {
	// head is set once the ledger has asked this acquirer to make room: it is
	// then the queue's head and does not fit, and it stays the head until its
	// wait ends, since the queue only grows at its tail.
	head bool
	// handed is the worker release gave this acquirer at the head, with the
	// reservation that worker holds; nil until then.
	handed *worker
	// cancel ends this acquirer's ledger wait once it has been handed a
	// worker, so the wait does not also take a second reservation.
	cancel context.CancelFunc
}

// waiting reports whether an acquirer is queued that has not been handed a
// worker. The pool lock must be held.
func (p *pool) waiting() bool {
	for _, a := range p.queued {
		if a.handed == nil {
			return true
		}
	}
	return false
}

// worker is one parser subprocess owned by the pool.
type worker struct {
	p      *pool
	ctx    context.Context
	cancel context.CancelFunc
	// in and out are the parent's ends of the worker's stdin and stdout;
	// childIn and childOut are the ends the runner gives the process. recv is
	// out as the parent reads it: every frame is read through it, so each byte
	// the worker answers with is counted into received as it arrives.
	in       *io.PipeWriter
	out      *io.PipeReader
	recv     io.Reader
	childIn  *io.PipeReader
	childOut *io.PipeWriter
	done     chan struct{}
	// result and runErr are written by the run goroutine before done closes.
	result process.Result
	runErr error
	// span is this worker's place in the run ledger. It is opened where the
	// process is started and ended in the run goroutine, which is where the
	// child's measured cost first exists.
	span *ledger.Span
	// release gives the worker's reservation back to the admission ledger. The
	// run goroutine calls it once the runner has reaped the process, never
	// before: until then the memory is still held.
	release func()

	// cpu and received are the hang detector's two progress signals: the
	// worker's consumed processor time, sampled by the runner, and the bytes
	// it has answered with.
	cpu      *process.CPUProgress
	received atomic.Int64

	// pid and rss are written by the goroutine driving the worker and read by
	// stats from any goroutine, so both are atomic rather than guarded by the
	// pool lock the writers do not hold.
	pid atomic.Int64
	rss atomic.Uint64
}

// countingReader counts every byte read through it into n.
type countingReader struct {
	r io.Reader
	n *atomic.Int64
}

func (c countingReader) Read(b []byte) (int, error) {
	n, err := c.r.Read(b)
	c.n.Add(int64(n))
	return n, err
}

// pprofDirEnv is the one variable a parser worker inherits, and only when the
// operator set it on the parent. A worker otherwise runs with an empty
// environment by construction, which is what keeps a parse from depending on
// anything but the bytes it is sent; the exception exists because a profile of
// the parse itself cannot be collected any other way -- a worker takes no
// arguments of its own -- and it is inert unless the variable is set.
const pprofDirEnv = "CODECTX_PPROF_DIR"

// workerEnv is the child environment: empty, or the profiling directory alone.
func workerEnv() []string {
	if dir := os.Getenv(pprofDirEnv); dir != "" {
		return []string{pprofDirEnv + "=" + dir}
	}
	return nil
}

func newPool(runner *process.Runner, admit *admission.Ledger, cmd WorkerCommand, dir string, max int, memory int64) *pool {
	p := &pool{runner: runner, admission: admit, cmd: cmd, dir: dir, max: max, memory: memory,
		live: map[*worker]bool{}, totals: map[*ledger.Run]*stageTotal{}}
	p.cond = sync.NewCond(&p.mu)
	return p
}

// enterStage opens the run's structural-parse total on the first unit that
// parses under it, and counts this one in. The total is opened from the run
// alone rather than from the caller's context: the workers under it outlive
// the unit that started them.
func (p *pool) enterStage(ctx context.Context) {
	run := ledger.RunFromContext(ctx)
	if run == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	t := p.totals[run]
	if t == nil {
		spanCtx, span := ledger.Start(run.Context(context.Background()), stageStructuralParse, "")
		t = &stageTotal{ctx: spanCtx, span: span}
		p.totals[run] = t
	}
	t.refs++
}

// leaveStage counts one unit out and ends the run's total when the last one
// leaves. The caller drains the pool first, so the workers' own spans -- and
// with them the child measurements the total's children carry -- are recorded
// before the parent ends. A worker still busy for another run keeps its own
// span and ends it where it exits.
func (p *pool) leaveStage(ctx context.Context) {
	run := ledger.RunFromContext(ctx)
	if run == nil {
		return
	}
	p.mu.Lock()
	t := p.totals[run]
	if t == nil {
		p.mu.Unlock()
		return
	}
	t.refs--
	last := t.refs == 0
	if last {
		delete(p.totals, run)
	}
	p.mu.Unlock()
	if last {
		// The stage's own goroutines run beside everything else in the
		// process, so the parent carries no processor time of its own: the
		// measured cost of this stage is on its children, one per worker
		// process.
		t.span.End(ledger.OutcomeOK, ledger.Measured{CPUUnattributed: ledger.CPUOverlapped}, nil)
	}
}

// count records one parsed file on the worker that parsed it and on its run's
// total. This is why no file is a span: a worker's files are two atomic adds
// on a span that already exists, and the total is the sum of its workers.
func (p *pool) count(ctx context.Context, w *worker, ex *extraction) {
	records := int64(len(ex.decls) + len(ex.imports) + len(ex.refs))
	w.span.AddIn(1)
	w.span.AddOut(records)
	run := ledger.RunFromContext(ctx)
	if run == nil {
		return
	}
	p.mu.Lock()
	t := p.totals[run]
	p.mu.Unlock()
	if t == nil {
		return
	}
	t.span.AddIn(1)
	t.span.AddOut(records)
}

// newWorker builds one worker's plumbing. Nothing here can fail and nothing
// here starts a process: it exists so a worker is never registered in the
// pool in a state where stopping it would use a nil pipe or cancel function.
func newWorker(p *pool) *worker {
	childIn, in := io.Pipe()
	out, childOut := io.Pipe()
	ctx, cancel := context.WithCancel(context.Background())
	w := &worker{p: p, ctx: ctx, cancel: cancel, in: in, out: out, childIn: childIn, childOut: childOut,
		done: make(chan struct{}), cpu: &process.CPUProgress{}}
	w.recv = countingReader{r: out, n: &w.received}
	return w
}

// acquire returns a worker to parse with: an idle one when the pool has one,
// otherwise a newly started process, waiting when max processes are already
// alive or about to be. It returns promptly on cancellation.
//
// A new worker's memory is reserved on the ledger before the worker exists,
// with the pool lock released: the wait is first-in-first-out behind every
// other heavy child and can be long. No worker goes idle while the wait lasts
// (see release). The wait ends in one of two ways: the ledger grants the room
// and this caller starts a worker in it, or, once this caller is the ledger's
// head, release hands it a worker together with that worker's reservation and
// the wait is withdrawn. A grant that lands at the same moment as a handout is
// given straight back, and so is one granted to a pool that closed meanwhile.
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
			p.mu.Unlock()
			return w, nil
		}
		if len(p.live)+len(p.queued) < p.max {
			reserving, cancel := context.WithCancel(ctx)
			a := &acquirer{cancel: cancel}
			p.queued = append(p.queued, a)
			p.mu.Unlock()
			// The make-room step frees nothing -- idle is empty while this
			// acquirer is queued -- and only marks it the ledger's head, which
			// is what lets release hand it the next worker to come back. It
			// is called with no ledger lock held, so taking the pool lock here
			// inverts no order: release cancels a wait under the pool lock, and
			// the ledger's cancellation takes the ledger lock alone.
			release, err := p.admission.ReserveWith(reserving, admission.Reservation{MemoryBytes: p.memory}, func() {
				p.mu.Lock()
				a.head = true
				p.mu.Unlock()
			})
			cancel()
			p.mu.Lock()
			p.queued = slices.DeleteFunc(p.queued, func(x *acquirer) bool { return x == a })
			p.cond.Broadcast()
			if w := a.handed; w != nil {
				if err == nil {
					// Granted as it was handed a worker: the worker's room is
					// this caller's, so the grant goes back to the ledger.
					p.mu.Unlock()
					release()
					p.mu.Lock()
				}
				if p.closed {
					// close has already counted w among the busy workers it
					// kills, so it is left to close.
					p.mu.Unlock()
					return nil, &model.Error{Code: model.CodeInternal, Message: "the treesitter provider is closed"}
				}
				if !p.live[w] {
					// The worker died after it was handed over: it is waited
					// out, as release does, and this caller asks again.
					p.mu.Unlock()
					w.stop(false)
					p.mu.Lock()
					continue
				}
				p.mu.Unlock()
				return w, nil
			}
			if err != nil {
				p.mu.Unlock()
				return nil, err
			}
			if p.closed {
				p.mu.Unlock()
				release()
				p.mu.Lock()
				continue
			}
			// The worker is fully constructed — pipes, context, cancel,
			// reservation — and takes its place in live and in the wait group
			// under the same hold close waits behind, so neither a second
			// acquirer nor a concurrent close can see a process about to start
			// as absent or half-built.
			w := newWorker(p)
			w.release = release
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

// release returns a worker after a parse. An unhealthy worker is stopped. A
// healthy one goes, with its reservation, to the acquirer of this pool that is
// the ledger's head; while an acquirer of this pool is queued behind another
// head it is stopped, its reservation goes back to the ledger once the runner
// has reaped it, and the ledger's pump hands that room to the head in order
// (see pool). With no acquirer queued it goes idle, reusable by the next parse
// of this stage and stopped by the drain that follows the last one. Its place
// in live is given up only when the process has exited, which is what keeps
// live processes bounded.
func (p *pool) release(w *worker, healthy bool) {
	if !healthy {
		w.stop(true)
		return
	}
	p.mu.Lock()
	if !p.live[w] {
		// The process died during or just after this parse — a crash, an
		// external kill — and the run goroutine has
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
	if !p.closed {
		for _, a := range p.queued {
			if a.head && a.handed == nil {
				a.handed = w
				a.cancel()
				p.mu.Unlock()
				return
			}
		}
	}
	if p.closed || p.waiting() {
		p.mu.Unlock()
		w.stop(false)
		return
	}
	p.idle = append(p.idle, w)
	p.cond.Broadcast()
	p.mu.Unlock()
}

// drain stops every worker nobody is using and returns once each has been
// reaped. It is what ends the stage: a worker is warm for exactly as long as
// there is parse work in flight, and no longer.
//
// There is no timer here and no setting. A timer would mean a resting machine
// holds one process per core -- on a sixteen-core host about 320 MB -- for
// however long the timer says, to save the milliseconds a re-execution of this
// binary costs when the next refresh comes. That trade is the wrong way round
// for a product whose whole posture is coexisting with the editor, the browser
// and the agents the person is using at the time.
//
// Busy workers are untouched: they belong to a caller that is parsing, and the
// caller returns them through release, which is what makes the drain that
// follows the last one complete.
func (p *pool) drain() {
	p.mu.Lock()
	idle := p.idle
	p.idle = nil
	p.mu.Unlock()
	// Stopped concurrently rather than one grace after another: an idle worker
	// exits on the end of its stdin, and serialising sixteen of those would
	// make the drain take sixteen graces in the worst case.
	var stopping sync.WaitGroup
	for _, w := range idle {
		stopping.Add(1)
		go func() {
			defer stopping.Done()
			w.stop(false)
		}()
	}
	stopping.Wait()
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
	// Neither pipe carries a lifetime byte total and the run carries no
	// wall-clock bound: the parent writes one request at a time and the worker
	// is paced by reading it, a bound on stdout would bound a caller-supplied
	// writer the runner never truncates, and a lifetime or a total would end a
	// long healthy stage mid-parse. There is no StallTimeout either: an idle
	// pooled worker makes no progress by design, so the hang detector runs
	// only while a request is outstanding (watch), fed the processor time the
	// runner publishes through CPUProgress.
	spec := process.Spec{
		Path: p.cmd.Path, Args: p.cmd.Args, Dir: p.dir, Env: workerEnv(),
		Stdin: w.childIn, Stdout: w.childOut, MaxStderrBytes: workerStderrBytes,
		Grace: workerGrace, CPUProgress: w.cpu, MemoryReservationBytes: p.memory,
	}
	w.span = p.openWorkerSpan(ctx)
	go func() {
		defer p.wg.Done()
		w.result, w.runErr = p.runner.Run(w.ctx, spec)
		// The runner has reaped the process, so its memory is free: this, and
		// not the moment the worker was asked to stop, is when the reservation
		// is given back.
		w.release()
		// The worker's cost is known here and nowhere earlier: the run has
		// returned, so the child has been reaped and its processor time, tree
		// peak and transferred bytes are on the result. The span is ended
		// before done closes, so a drain that waits for the reap cannot return
		// before this worker's cost has been recorded.
		outcome := ledger.OutcomeOK
		if w.runErr != nil {
			outcome = ledger.OutcomeFailed
		}
		w.span.End(outcome, measured(w.result), w.runErr)
		// Unblock any parent write or read: the worker cannot answer any more.
		w.childIn.CloseWithError(errWorkerGone)
		w.childOut.CloseWithError(errWorkerGone)
		p.mu.Lock()
		delete(p.live, w)
		// A worker can die while it sits idle — a crash, an external kill —
		// and an exited worker is not reusable, so it leaves the idle list
		// with the live map. Otherwise idle could outnumber live
		// and the next caller would be handed a dead worker, spending on a
		// certain errWorkerGone the one retry a real parse failure needs.
		p.idle = slices.DeleteFunc(p.idle, func(x *worker) bool { return x == w })
		p.exited++
		p.cond.Broadcast()
		p.mu.Unlock()
		close(w.done)
	}()

	// The hello is watched by the same hang detector as a parse. Until the
	// runner has admitted and started the process there is no processor-time
	// measurement, and a worker queued behind the runner's own admission is
	// waiting, not hung, so that wait is never read as silence.
	stalled, stop := w.watch(ctx)
	kind, payload, err := wire.ReadMessage(w.recv, 0)
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
		// stop returns once the runner has reaped the process, so the run
		// goroutine's result and error are final here.
		w.stop(true)
		if ctx.Err() != nil {
			return model.Canceled(ctx.Err())
		}
		if stalled() {
			return stalledError(fmt.Sprintf("the parser worker made no progress for %s before its hello", hangWindow))
		}
		if w.runErr != nil {
			// The runner's typed refusal (trust, admission, start failure)
			// or termination explains the missing hello better than the pipe.
			return w.runErr
		}
		return (&model.Error{Code: model.CodeProviderUnavailable, Message: "the parser worker did not start: " + err.Error()}).
			WithDetail("worker_stderr", w.stderrLine())
	}
	w.pid.Store(int64(hello.PID))
	return nil
}

// openWorkerSpan opens one worker's span under the total of the run that is
// starting it, or, where that run is parsing outside a stage, under the run
// itself. It is never opened under the caller's context: a pooled worker
// serves the units that follow this one and is reaped long after this unit's
// span has ended.
func (p *pool) openWorkerSpan(ctx context.Context) *ledger.Span {
	run := ledger.RunFromContext(ctx)
	if run == nil {
		return nil
	}
	p.mu.Lock()
	t := p.totals[run]
	p.mu.Unlock()
	parent := run.Context(context.Background())
	if t != nil {
		parent = t.ctx
	}
	_, span := ledger.Start(parent, stageStructuralParse, "")
	return span
}

// watch runs the hang detector over one outstanding hello or request, and
// kills the worker when ctx ends. It returns a predicate that reports whether
// it was the detector that killed it, and a stop function to call once the
// exchange is over. Killing goes through the runner, which is the one
// cancellation path the worker has: it needs no in-process cancellation
// callback (see package worker).
func (w *worker) watch(ctx context.Context) (stalled func() bool, stop func()) {
	return watchProgress(ctx, w.done, w.cpu, &w.received, hangWindow, w.kill)
}

// tickSource is the processor-time half of the progress signal:
// process.CPUProgress in the product.
type tickSource interface {
	Ticks() (int64, bool)
}

// watchProgress is the hang detector. The worker is alive for as long as its
// processor time moves or a byte of its answer arrives; it is declared hung,
// and kill is called, only when both have stood still for window. A pooled
// worker serves one request at a time, so both signals are this request's
// alone: no sibling can speak for a wedged parse.
//
// A processor-time reading that is unavailable (ok false) is no measurement,
// never zero ticks, and silence is not evidence of a hang without it: a parse
// computes and says nothing until it is done, so on the wire a long parse and
// a wedged one look the same. While there is no reading -- before the runner
// has started the process, and for the whole run on a platform that cannot
// sample a running tree -- the detector therefore never fires. On such a
// platform a wedged worker is ended only by the caller's cancellation or by
// its own exit; the alternative, killing on silence alone, would be the
// wall-clock kill this detector exists to replace, applied to every long parse.
//
// The clock is read only here and compared against itself. Polling at a
// quarter of the window bounds the overshoot past it, as the runner's own
// stall watchdog does.
func watchProgress(ctx context.Context, exited <-chan struct{}, cpu tickSource, received *atomic.Int64,
	window time.Duration, kill func()) (stalled func() bool, stop func()) {

	var fired atomic.Bool
	finished := make(chan struct{})
	var once sync.Once
	go func() {
		ticker := time.NewTicker(max(window/4, 10*time.Millisecond))
		defer ticker.Stop()
		var lastTicks, lastBytes int64
		measured := false
		quietSince := time.Now()
		for {
			select {
			case <-finished:
				return
			case <-exited:
				return
			case <-ctx.Done():
				kill()
				return
			case now := <-ticker.C:
				ticks, ok := cpu.Ticks()
				bytes := received.Load()
				if !ok || !measured || ticks != lastTicks || bytes != lastBytes {
					lastTicks, lastBytes, measured, quietSince = ticks, bytes, ok, now
					continue
				}
				if now.Sub(quietSince) >= window {
					fired.Store(true)
					kill()
					return
				}
			}
		}
	}()
	return fired.Load, func() { once.Do(func() { close(finished) }) }
}

// kill terminates the worker through the runner and closes the parent's ends
// of its pipes: stdin, so the runner's stdin copy sees end of file at once
// instead of waiting out its grace on a pipe nobody will write to again, and
// stdout, so a frame the worker had already written fails the runner's copy
// instead of blocking it on a parent that will never read it -- which is what
// lets the run return, and stop wait for it, however the worker was left.
func (w *worker) kill() {
	w.cancel()
	w.in.Close()
	w.out.CloseWithError(errWorkerGone)
}

// stop ends the worker, gracefully by closing its stdin (it exits on end of
// file) or forcibly through the runner, and returns once the runner has
// reaped it. The wait is on the exit, not on a clock: after a kill the runner
// always returns -- it forces the tree after its grace and reports a tree that
// would not die as an error -- so the reap is certain. A graceful stop is
// escalated after workerGrace, which only a worker that ignores the end of its
// input ever reaches.
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
	<-w.done
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

// extraction is one parsed file as the worker reported it: every record it
// extracted, each reassembled whole from its frames. It is held for this one
// file and released with it, which is what bounds the parent's heap for a
// parse (see wire.ChunkBytes).
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
// worker stays healthy, a cancellation when the caller's context ended, a
// CTX_PROVIDER_TIMEOUT when the hang detector found the worker neither
// computing nor answering (the worker was killed in both cases), and anything
// else when the worker broke protocol or died, in which case the caller
// retires it. There is no deadline on the answer: a parse that is working is
// never ended for taking long.
func (p *pool) parse(ctx context.Context, w *worker, req wire.Request, src []byte) (*extraction, error) {
	stalled, stop := w.watch(ctx)
	defer stop()
	ex, err := p.exchange(w, req, src)
	stop()
	p.mu.Lock()
	p.parses++
	p.mu.Unlock()
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
	if stalled() {
		return nil, stalledError(fmt.Sprintf("the parser worker made no progress for %s while parsing %s", hangWindow, bound(req.Path, 256)))
	}
	return nil, err
}

// stalledError is the hang detector's verdict: a timeout in kind, retryable,
// and told apart from any other timeout by its stop reason, the way the
// runner's own stall watchdog reports one.
func stalledError(msg string) *model.Error {
	return (&model.Error{Code: model.CodeProviderTimeout, Message: msg, Retryable: true}).
		WithDetail("stop_reason", "stalled")
}

// exchange sends one request and its source, then reads the answer. Every
// read goes through w.recv, so each byte of every frame -- continuation frames
// included -- is progress the hang detector sees as it arrives.
func (p *pool) exchange(w *worker, req wire.Request, src []byte) (*extraction, error) {
	if err := wire.WriteJSON(w.in, wire.KindRequest, req); err != nil {
		return nil, err
	}
	if err := wire.WriteMessage(w.in, wire.KindSource, src); err != nil {
		return nil, err
	}
	ex := &extraction{}
	for {
		kind, payload, err := wire.ReadMessage(w.recv, 0)
		if err != nil {
			return nil, err
		}
		switch kind {
		case wire.KindDecl:
			var d wire.Decl
			if err := json.Unmarshal(payload, &d); err != nil {
				return nil, err
			}
			ex.decls = append(ex.decls, d)
		case wire.KindImport:
			var i wire.Import
			if err := json.Unmarshal(payload, &i); err != nil {
				return nil, err
			}
			ex.imports = append(ex.imports, i)
		case wire.KindRef:
			var r wire.Ref
			if err := json.Unmarshal(payload, &r); err != nil {
				return nil, err
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
	// idle and shutting down alike. It never exceeds Options.MaxWorkers.
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
