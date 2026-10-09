package treesitter

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"os"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Sawmonabo/codectx/internal/admission"
	"github.com/Sawmonabo/codectx/internal/config"
	"github.com/Sawmonabo/codectx/internal/ledger"
	"github.com/Sawmonabo/codectx/internal/model"
	"github.com/Sawmonabo/codectx/internal/process"
	"github.com/Sawmonabo/codectx/internal/provider/treesitter/wire"
	"github.com/Sawmonabo/codectx/internal/residency"
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
// one is still alive, still holding its base on the ledger and its place in
// the runner's concurrency: more than max processes would be alive at once,
// and the fresh worker would wait in the runner's queue behind the one that
// is leaving.
//
// Memory is taken on the process's one reservation ledger in two holdings,
// and nothing else in this pool counts it (ADR-0012 decision 5, "The
// ledger"):
//
//   - A worker's base holding is reserved before the worker is started, at the
//     largest base any worker of this pool has reported, and adjusted to the
//     base the worker reports in its hello and after every file, upward
//     without waiting. The base held is the anonymous part of the worker's
//     resident set, which is its own, and not the pages of the executable
//     every worker shares; where only the whole resident set is reported, it
//     is held instead. Until one worker has reported, no base is known and
//     only that one worker is started: the others wait for its report and are
//     reserved at it. A worker whose hello carries no base at all holds the
//     idle footprint's stand-in (config.UnobservedIdleFootprintBytes), since
//     it is this same binary idle, and new workers are reserved at that. The
//     holding is given back only once the runner has reaped the process.
//   - Each file's increment is a Parse reservation of its predicted need,
//     taken with the worker in hand, after the allocation is re-derived, and
//     given back once the file's Done has adjusted the base. A Parse
//     reservation is granted whenever no other parse holds one, so a file
//     larger than the whole allocation still runs, alone among the parses.
//     Once a file reserved at the structural prior has needed more than the
//     prior in some language, every later file of that language reserved at
//     the prior runs alone among the parses (admission.Reservation.Alone): a
//     prior already seen short there stakes no other parse's room on itself.
//
// Parser workers and every other heavy child of this process are therefore
// admitted against one allocation and one running total.
//
// Every admission is first-in-first-out on the ledger. A worker coming back
// from a parse while an acquirer of this pool is queued there goes one of two
// ways, both in the ledger's order:
//
//   - When that acquirer is the ledger's head, the worker and the base holding
//     it carries are handed to it. The head is next in line for exactly that
//     room, so the handout is the grant the ledger would make, without
//     stopping one process to start the same one again. The ledger tells an
//     acquirer it is the head that does not fit through the make-room step;
//     no other reserver can come before it from then on.
//   - When the head is another reserver -- a unit, a language server, another
//     pool -- the worker is stopped: its base holding returns to the ledger,
//     which pumps, and that head is admitted in order. This pool's acquirer
//     behind it waits its turn.
//
// No worker that is not parsing is held while a reserver other than this
// pool's own parse waits at the ledger's head. A worker is not parsing when it
// is idle, and also when its caller holds it while it waits for the file's
// increment:
//
//   - A worker coming back while one waits and no acquirer of this pool is the
//     head is stopped, never idled.
//   - An acquirer that finds idle workers while such a reserver waits
//     (admission.Ledger.Waiting) stops every one of them and then queues for a
//     worker of its own behind that waiter.
//   - The pool is registered on the ledger as a holder of idle room
//     (admission.Ledger.Holder) for its whole life, so a reserver that reaches
//     the head and does not fit has the pool stop its idle workers at once,
//     whether or not this pool is acquiring or releasing anything, and has one
//     caller waiting for an increment give its worker back (yieldParse). That
//     caller returns the worker through release -- to this pool's acquirer at
//     the head, or stopped -- and asks for a worker again. A stage whose own
//     progress waits on that reserver -- a unit of the same tick reserving
//     behind room these workers hold -- therefore never waits on the stage's
//     drain, and a caller holding a worker never waits for an increment
//     queued behind a head that waits for that worker's room.
//   - When the head is one of this pool's own increments, the workers are left
//     as they are: a Parse reservation at the head that does not fit waits
//     only on parses in flight, whose Done returns their increments, and is
//     granted outright once none is in flight. Stopping workers for it would
//     make every file an exec.
//
// Stopped workers' room returns to the ledger and reaches the head in order.
// With nobody waiting, a worker coming back goes idle and is held for the
// stage like any room an admitted child holds. Room is returned by its holder
// and never taken from it, only room that is not parsing is given back, and
// admission order among the reservers queued on the ledger is kept
// throughout.
//
// Kept idle while an acquirer of this pool is queued, a worker would hold the
// room that acquirer waits for while nothing ever pumps the ledger, and the
// acquirer, its unit and the stage it keeps from draining would wait forever.
//
// Callers waiting for a worker are served largest source bytes first, ties in
// arrival order, from the one queue pending: only its first may take an idle
// worker or queue on the ledger for a new one. There is no work stealing and
// no second count of workers: max is the processor count, and the memory bound
// on how many run is the ledger's sum. A caller already queued on the ledger
// keeps its place there, so a larger file that arrives after it waits behind
// it: the ledger's order is not reopened for size.
type pool struct {
	runner    *process.Runner
	admission *admission.Ledger
	cmd       WorkerCommand
	dir       string
	max       int
	// rederive is Options.Rederive: it is handed the parser workers' part of
	// the product's residency (residentBytes) before each file's increment is
	// reserved, and the ledger's re-derivation before every admission adds
	// the last figure it was handed back to the allocation.
	rederive func(workerResidentBytes int64)

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
	// pending are the callers waiting for a worker, served largest first (see
	// next). A caller leaves it once it holds a worker or queues on the ledger
	// for one, and rejoins it, with its place, when it must ask again.
	pending []*request
	seq     uint64
	// parseWaits are the callers holding a worker while they wait on the
	// ledger for their file's increment.
	parseWaits []*parseWait
	// largestBase is the largest base any worker of this pool has held: what
	// a new worker's base holding is reserved at before it reports its own.
	// sized is whether any worker has reported yet, and until one has, only
	// one worker is started (see acquire).
	largestBase int64
	sized       bool
	// build is this binary's build identity (wire.Build): every worker must
	// state it in its hello, and every need model is keyed by it.
	build string
	wg    sync.WaitGroup
	// totals is the open structural-parse total of every run parsing through
	// this pool, which is what a worker's span hangs off. It is keyed by the
	// run and not held as one span because a deferred publication's tick runs
	// units of its own, and its workers belong to its run rather than to
	// whatever index run happens to be live.
	totals map[*ledger.Run]*stageTotal

	started, exited, parses, retries uint64
	// inFlight counts the parses holding an increment, and maxInFlight is the
	// most that ever did at once. inFlightBytes sums their increments.
	inFlight, maxInFlight int
	inFlightBytes         int64
	// workerCPUMS sums the processor time of every reaped worker, and
	// cpuUnsampled counts the reaped workers that were not measured, any one
	// of which makes the sum unavailable. It is a count so a stage can tell
	// whether one of its own workers was unmeasured.
	workerCPUMS  int64
	cpuUnsampled uint64
	// windows are the open stages' measurement windows (see stageWindow),
	// bounded by the stages open at once.
	windows map[*stageWindow]struct{}
	// unmeasured counts the files whose Done carried no need, and
	// modelFailures the model reads and encodings that failed.
	unmeasured, modelFailures uint64
	// overruns are the files whose need exceeded their increment, per class.
	// The map is bounded by the pinned languages times the size classes.
	overruns map[overrunClass]*Overrun
	// priorShort are the languages in which a file reserved at the structural
	// prior needed more than it; a later file of one reserved at the prior
	// runs alone among the parses, and aloneFiles counts those files. The map
	// is bounded by the pinned languages.
	priorShort map[string]bool
	aloneFiles uint64
	// models are the learned need models of the repositories being indexed,
	// loaded from the run ledger on first use and dropped when the last stage
	// of their repository ends. It is bounded by those repositories times the
	// pinned languages times the size classes.
	models map[needKey]*needModel
	// unregister removes the pool's idle-release step from the ledger; close
	// calls it before it stops the workers.
	unregister func()
}

// acquirer is one caller of acquire waiting on the ledger. Its fields are
// guarded by the pool lock.
type acquirer struct {
	// head is set once the ledger has asked this acquirer to make room: it is
	// then the queue's head and does not fit, and it stays the head until its
	// wait ends, since the queue only grows at its tail.
	head bool
	// handed is the worker release gave this acquirer at the head, with the
	// base holding that worker carries; nil until then.
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

// request is one caller waiting for a worker: its file's source bytes, and
// its arrival, which breaks ties.
type request struct {
	bytes int64
	seq   uint64
}

// request registers nothing; it numbers one caller's arrival for pending.
func (p *pool) request(sourceBytes int64) *request {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.seq++
	return &request{bytes: sourceBytes, seq: p.seq}
}

// next is the pending caller served first: the largest file, the earliest
// arrival among equals. The pool lock must be held.
func (p *pool) next() *request {
	var first *request
	for _, r := range p.pending {
		if first == nil || r.bytes > first.bytes || r.bytes == first.bytes && r.seq < first.seq {
			first = r
		}
	}
	return first
}

// dequeue takes r out of pending and wakes the callers behind it, one of
// which is now first. The pool lock must be held.
func (p *pool) dequeue(r *request) {
	p.pending = slices.DeleteFunc(p.pending, func(x *request) bool { return x == r })
	p.cond.Broadcast()
}

// parseWait is one caller holding a worker while it waits on the ledger for
// its file's increment. Its fields are guarded by the pool lock.
type parseWait struct {
	// head is set once the ledger has asked this wait to make room: it is
	// then the queue's head and does not fit.
	head bool
	// yielded is set when yieldParse asked this caller to give its worker
	// back, and cancel is what ends the wait for it.
	yielded bool
	cancel  context.CancelFunc
}

// parseHead reports whether one of this pool's increments is the ledger's
// head. The pool lock must be held.
func (p *pool) parseHead() bool {
	for _, pw := range p.parseWaits {
		if pw.head {
			return true
		}
	}
	return false
}

// yieldParse asks the caller that most recently began waiting for an
// increment to give its worker back, unless an increment of this pool is the
// ledger's head (see pool). It is called once per stuck pump of the ledger's
// head, so workers are given back one at a time for as long as the head does
// not fit.
func (p *pool) yieldParse() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.parseHead() {
		return
	}
	for i := len(p.parseWaits) - 1; i >= 0; i-- {
		if pw := p.parseWaits[i]; !pw.yielded {
			pw.yielded = true
			pw.cancel()
			return
		}
	}
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
	// holding is the worker's base holding on the admission ledger. The run
	// goroutine releases it once the runner has reaped the process, never
	// before: until then the memory is still held.
	holding *admission.Holding

	// cpu and received are the hang detector's two progress signals: the
	// worker's consumed processor time, sampled by the runner, and the bytes
	// it has answered with.
	cpu      *process.CPUProgress
	received atomic.Int64

	// pid, held and base are written by the goroutine driving the worker and
	// read by stats from any goroutine, so they are atomic rather than guarded
	// by the pool lock the writers do not hold. held is the base holding's
	// figure; base is the base the worker last reported, and -1 while it has
	// reported none.
	pid  atomic.Int64
	held atomic.Int64
	base atomic.Int64
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

func newPool(runner *process.Runner, admit *admission.Ledger, cmd WorkerCommand, dir string, max int,
	rederive func(workerResidentBytes int64)) *pool {
	p := &pool{runner: runner, admission: admit, cmd: cmd, dir: dir, max: max, rederive: rederive, build: wire.Build(),
		live: map[*worker]bool{}, totals: map[*ledger.Run]*stageTotal{}, windows: map[*stageWindow]struct{}{},
		overruns: map[overrunClass]*Overrun{}, priorShort: map[string]bool{}, models: map[needKey]*needModel{}}
	p.cond = sync.NewCond(&p.mu)
	// Idle workers, and workers held by callers waiting for an increment, are
	// room this pool keeps without parsing, so the ledger is told it may ask
	// for it back: a head that does not fit has them given back, unless the
	// head is this pool's own increment (see pool).
	p.unregister = admit.Holder(func() {
		p.mu.Lock()
		own := p.parseHead()
		p.mu.Unlock()
		if own || !p.admission.Waiting() {
			return
		}
		p.drain()
		p.yieldParse()
	})
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
	repository := run.RepositoryID()
	p.mu.Lock()
	defer p.mu.Unlock()
	t := p.totals[run]
	if t == nil {
		spanCtx, span := ledger.Start(run.Context(context.Background()), stageStructuralParse, "")
		t = &stageTotal{ctx: spanCtx, span: span, repository: repository, files: map[model.SnapshotID]*languageFiles{}}
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
		p.dropModels(t.repository)
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
	records := int64(len(ex.decls) + len(ex.imports) + len(ex.refs) + len(ex.functions))
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
	w.base.Store(-1)
	return w
}

// acquire returns a worker to parse r's file with: an idle one when the pool
// has one and no reserver other than this pool's own increment waits on the
// ledger, otherwise a newly started process, waiting when max processes are
// already alive or about to be. Callers are served from pending, largest file
// first. It returns promptly on cancellation.
//
// Idle workers found while such a reserver waits are stopped rather than
// reused (see pool): their room goes back to the ledger, and this caller then
// reserves behind that waiter like any other. Waiting takes the ledger lock
// under the pool lock; nothing takes the two the other way round, since the
// ledger calls into this pool only through the make-room step and the pool's
// idle-release step, both with no ledger lock held, and the pool holds, adjusts
// and releases holdings only with its own lock released.
//
// A new worker's base holding is reserved on the ledger before the worker
// exists, at the largest base this pool has held, with the pool lock released:
// the wait is first-in-first-out behind every other heavy child and can be
// long. While no worker has reported a base there is no figure to reserve at,
// and an unavailable base is not a base of zero, so only the first worker is
// started, at no base: it is held at what it reports in its hello, and every
// other caller waits for that report and is then reserved at it. No worker goes idle while the wait lasts (see release). The wait ends
// in one of two ways: the ledger grants the room and this caller starts a
// worker in it, or, once this caller is the ledger's head, release hands it a
// worker together with that worker's base holding and the wait is withdrawn.
// A grant that lands at the same moment as a handout is given straight back,
// and so is one granted to a pool that closed meanwhile.
func (p *pool) acquire(ctx context.Context, r *request) (*worker, error) {
	p.mu.Lock()
	p.pending = append(p.pending, r)
	for {
		if p.closed {
			p.dequeue(r)
			p.mu.Unlock()
			return nil, closedError()
		}
		if p.next() != r {
			if err := p.wait(ctx); err != nil {
				p.dequeue(r)
				p.mu.Unlock()
				return nil, err
			}
			continue
		}
		if n := len(p.idle); n > 0 {
			if p.admission.Waiting() && !p.parseHead() {
				p.mu.Unlock()
				p.drain()
				p.mu.Lock()
				continue
			}
			w := p.idle[n-1]
			p.idle = p.idle[:n-1]
			p.dequeue(r)
			p.mu.Unlock()
			return w, nil
		}
		if len(p.live)+len(p.queued) < p.max && (p.sized || len(p.live)+len(p.queued) == 0) {
			// Queued on the ledger, this caller keeps its place there, so the
			// next pending caller may queue for a worker of its own.
			p.dequeue(r)
			reserving, cancel := context.WithCancel(ctx)
			a := &acquirer{cancel: cancel}
			p.queued = append(p.queued, a)
			base := p.largestBase
			p.mu.Unlock()
			// The make-room step frees nothing -- idle is empty while this
			// acquirer is queued -- and only marks it the ledger's head, which
			// is what lets release hand it the next worker to come back. It
			// is called with no ledger lock held, so taking the pool lock here
			// inverts no order: release cancels a wait under the pool lock, and
			// the ledger's cancellation takes the ledger lock alone.
			holding, err := p.admission.Hold(reserving, admission.Reservation{MemoryBytes: base}, func() {
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
					holding.Release()
					p.mu.Lock()
				}
				if p.closed {
					// close has already counted w among the busy workers it
					// kills, so it is left to close.
					p.mu.Unlock()
					return nil, closedError()
				}
				if !p.live[w] {
					// The worker died after it was handed over: it is waited
					// out, as release does, and this caller asks again.
					p.mu.Unlock()
					w.stop(false)
					p.mu.Lock()
					p.pending = append(p.pending, r)
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
				holding.Release()
				p.mu.Lock()
				p.pending = append(p.pending, r)
				continue
			}
			// The worker is fully constructed — pipes, context, cancel,
			// base holding — and takes its place in live and in the wait group
			// under the same hold close waits behind, so neither a second
			// acquirer nor a concurrent close can see a process about to start
			// as absent or half-built.
			w := newWorker(p)
			w.holding = holding
			w.held.Store(base)
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
			p.dequeue(r)
			p.mu.Unlock()
			return nil, err
		}
	}
}

func closedError() *model.Error {
	return &model.Error{Code: model.CodeInternal, Message: "the treesitter provider is closed"}
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

// release returns a worker after a parse, or from a caller that yielded it
// while waiting for an increment. An unhealthy worker is stopped. A healthy
// one goes, with its base holding, to the acquirer of this pool that is the
// ledger's head; while any other reserver waits -- an acquirer of this pool
// queued behind another head, or another reserver of the ledger at a head that
// is not this pool's increment -- it is stopped, its base holding goes back to
// the ledger once the runner has reaped it, and the ledger's pump hands that
// room to the head in order (see pool). Otherwise it goes idle, to the first
// pending caller or, with none, for the next parse of this stage, and is
// stopped by the drain that follows the last one. Its place in live is given
// up only when the process has exited, which is what keeps live processes
// bounded.
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
	if p.closed || p.waiting() || p.admission.Waiting() && !p.parseHead() {
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
// there is parse work in flight, and no longer. acquire also calls it within a
// stage, when it finds idle workers while a reserver waits on the ledger, and
// so does the pool's idle-release step on the ledger, when a head does not
// fit; either way their room reaches that reserver in order (see pool).
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
	p.unregister()
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
	//
	// The runner reserves no memory for a worker: the ledger is the one gate,
	// where the worker's base and each file's increment are held, and the
	// runner's own budget never refuses what the ledger admitted.
	spec := process.Spec{
		Path: p.cmd.Path, Args: p.cmd.Args, Dir: p.dir, Env: workerEnv(),
		Stdin: w.childIn, Stdout: w.childOut, MaxStderrBytes: workerStderrBytes,
		Grace: workerGrace, CPUProgress: w.cpu,
	}
	w.span = p.openWorkerSpan(ctx)
	go func() {
		defer p.wg.Done()
		w.result, w.runErr = p.runner.Run(w.ctx, spec)
		// The runner has reaped the process, so its memory is free: this, and
		// not the moment the worker was asked to stop, is when the reservation
		// is given back.
		w.holding.Release()
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
		if w.result.CPUUnsampled {
			p.cpuUnsampled++
		} else {
			p.workerCPUMS += w.result.CPUUserMillis + w.result.CPUSysMillis
		}
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
		} else if err == nil && hello.Build != p.build {
			err = fmt.Errorf("worker build %q does not match the provider's %q", bound(hello.Build, 128), p.build)
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
	if _, ok := heldBase(hello.Memory); !ok {
		// A worker that reports no base is this binary idle, so it holds the
		// idle footprint's stand-in; it is a holding, not a measurement, and
		// the worker's base stays unavailable in Stats.
		p.hold(w, config.UnobservedIdleFootprintBytes)
	}
	p.adjust(w, hello.Memory)
	return nil
}

// heldBase is the base a reading holds a worker at: its anonymous resident
// set, or its whole resident set where only that is reported, and false where
// neither is.
func heldBase(m wire.Memory) (int64, bool) {
	for _, v := range []*uint64{m.AnonBytes, m.BaseBytes} {
		if v != nil && *v <= math.MaxInt64 {
			return int64(*v), true
		}
	}
	return 0, false
}

// adjust sets w's base holding to the base the worker reported (heldBase),
// upward without waiting, and raises the largest base a new worker is
// reserved at. A reading with no base leaves the holding as it is: an
// unavailable base is not a base of zero.
func (p *pool) adjust(w *worker, m wire.Memory) {
	if m.BaseBytes != nil && *m.BaseBytes <= math.MaxInt64 {
		w.base.Store(int64(*m.BaseBytes))
	}
	if held, ok := heldBase(m); ok {
		p.hold(w, held)
	}
}

// hold sets w's base holding to bytes and raises the largest base a new
// worker is reserved at, waking the callers that wait for the first figure.
func (p *pool) hold(w *worker, bytes int64) {
	w.holding.Adjust(bytes)
	w.held.Store(bytes)
	p.mu.Lock()
	p.largestBase = max(p.largestBase, bytes)
	if !p.sized {
		p.sized = true
		p.cond.Broadcast()
	}
	p.mu.Unlock()
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
// extracted, each reassembled whole from its frames, and one dependence
// message per callable in the order the worker sent them. It is held for
// this one file and released with it, which is what bounds the parent's heap
// for a parse (see wire.ChunkBytes).
type extraction struct {
	decls     []wire.Decl
	imports   []wire.Import
	refs      []wire.Ref
	functions []wire.Function
	done      wire.Done
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
		case wire.KindFunction:
			// A message no writer of this protocol produces is a protocol
			// break like any other undecodable frame: the worker is retired.
			f, err := wire.DecodeFunction(payload)
			if err != nil {
				return nil, err
			}
			ex.functions = append(ex.functions, f)
		case wire.KindDone:
			if err := json.Unmarshal(payload, &ex.done); err != nil {
				return nil, err
			}
			p.adjust(w, ex.done.Memory)
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

// increment is what one file is reserved at: its predicted need, and whether
// it runs alone among the parses (see pool).
type increment struct {
	bytes int64
	alone bool
}

// parseHolding is one file's increment as it is held on the ledger.
type parseHolding struct {
	h     *admission.Holding
	bytes int64
}

// reserveParse takes one file's increment, with the worker in hand: it hands
// the workers' residency (residentBytes) to rederive, so the allocation the
// ledger re-derives from the kernel's figure as this reservation queues counts
// it, then holds a Parse reservation, which is granted
// whenever no other parse holds one, and, for a file that runs alone, only
// then. yielded reports that the wait was ended by yieldParse and not
// granted: the caller then gives its worker back through release and asks for
// one again (see pool). A grant that lands as the caller is asked to yield is
// given straight back by the ledger, as every canceled wait's is, and the
// caller yields.
func (p *pool) reserveParse(ctx context.Context, inc increment) (ph *parseHolding, yielded bool, err error) {
	p.rederive(p.residentBytes())
	waiting, cancel := context.WithCancel(ctx)
	defer cancel()
	pw := &parseWait{cancel: cancel}
	p.mu.Lock()
	p.parseWaits = append(p.parseWaits, pw)
	p.mu.Unlock()
	h, err := p.admission.Hold(waiting, admission.Reservation{MemoryBytes: inc.bytes, Parse: true, Alone: inc.alone}, func() {
		p.mu.Lock()
		pw.head = true
		p.mu.Unlock()
	})
	p.mu.Lock()
	defer p.mu.Unlock()
	p.parseWaits = slices.DeleteFunc(p.parseWaits, func(x *parseWait) bool { return x == pw })
	if err != nil {
		return nil, pw.yielded && ctx.Err() == nil, err
	}
	p.inFlight++
	p.inFlightBytes += inc.bytes
	if inc.alone {
		p.aloneFiles++
	}
	p.maxInFlight = max(p.maxInFlight, p.inFlight)
	for w := range p.windows {
		w.maxInFlight = max(w.maxInFlight, p.inFlight)
	}
	return &parseHolding{h: h, bytes: inc.bytes}, false, nil
}

// stageWindow is one open stage's view of the pool's lifetime counters: their
// values when the stage opened, and the most parses that held an increment
// at once while it was open, which a lifetime maximum cannot answer. Its
// fields are guarded by the pool lock.
type stageWindow struct {
	started, unsampled uint64
	cpuMS              int64
	// exclusive is whether no worker was alive when the stage opened. A
	// worker alive then carries processor time spent before the stage, so
	// its reap would add that time to the stage's sum.
	exclusive   bool
	maxInFlight int
}

// openWindow starts measuring one stage.
func (p *pool) openWindow() *stageWindow {
	p.mu.Lock()
	defer p.mu.Unlock()
	w := &stageWindow{started: p.started, unsampled: p.cpuUnsampled, cpuMS: p.workerCPUMS,
		exclusive: len(p.live) == 0, maxInFlight: p.inFlight}
	p.windows[w] = struct{}{}
	return w
}

// closeWindow ends one stage's measurement and answers what the pool did
// while it was open. It is called after the stage's drain, when every worker
// the stage alone used has been reaped and its processor time summed. The
// sum is unavailable when a worker was alive at the open or is still alive
// now -- another run's stage was open beside this one, or a probe's worker
// outlived it -- or when a worker reaped meanwhile was not measured: each
// would make the sum part of the stage's cost, or more than it.
func (p *pool) closeWindow(w *stageWindow) model.StageFigures {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.windows, w)
	f := model.StageFigures{WorkersStarted: int64(p.started - w.started), MaxInFlight: int64(w.maxInFlight)}
	if w.exclusive && len(p.live) == 0 && p.cpuUnsampled == w.unsampled {
		cpu := p.workerCPUMS - w.cpuMS
		f.WorkerCPUMS = &cpu
	}
	return f
}

// endParse gives one file's increment back. Its caller has already adjusted
// the worker's base to the file's Done, so the ledger never admits the next
// file against memory the worker has not yet returned.
func (p *pool) endParse(ph *parseHolding) {
	ph.h.Release()
	p.mu.Lock()
	p.inFlight--
	p.inFlightBytes -= ph.bytes
	p.mu.Unlock()
}

// residentBytes is the parser workers' part of the product's own residency.
// Where every live worker has a live reading of its anonymous resident set
// (process.CPUProgress.AnonResidentBytes), it is the sum of those readings,
// which include what each parse in flight has taken so far. Otherwise it is
// what the pool holds for them: every live worker's base holding and every
// parse's increment in flight, since the memory a parse in flight has taken
// is already gone from the kernel's available figure, and leaving it out
// would have the product's own parses narrow the room for the next file. A
// parse that has not yet reached its increment is then counted at it. The
// increments are not attributed to workers, so one worker without a reading
// -- a platform that samples no running tree, or a worker the first sweep has
// not yet found -- puts the whole pool on the holdings.
func (p *pool) residentBytes() int64 {
	p.mu.Lock()
	defer p.mu.Unlock()
	held, read := p.inFlightBytes, int64(0)
	complete := true
	for w := range p.live {
		held += w.held.Load()
		if b, ok := w.cpu.AnonResidentBytes(); ok {
			read += b
		} else {
			complete = false
		}
	}
	if complete {
		return read
	}
	return held
}

// needKey names one learned need model: the repository and the key the run
// ledger stores it under within that repository.
type needKey struct {
	repository string
	ledger.NeedKey
}

// needModel is one key's decaying histogram, loaded from the run ledger on
// first use. Its lock orders the observations folded into it and the states
// recorded after them.
type needModel struct {
	mu     sync.Mutex
	loaded bool
	h      admission.NeedHistogram
}

// languageFiles is one snapshot's count of files per language, counted once
// per stage: the half-life of every model learned from that snapshot.
type languageFiles struct {
	mu      sync.Mutex
	counted bool
	files   map[string]int64
}

// overrunClass is what overruns are counted per: a language and a size class.
type overrunClass struct {
	language  string
	sizeClass int
}

// fileNeed is what one file is reserved at and learned under.
type fileNeed struct {
	language string
	size     int64
	// reserved is the file's increment: the model's prediction, or the
	// structural prior for the first file of its key, which prior reports.
	reserved increment
	prior    bool
	// model is nil where nothing is learned from the file: no run, no stage,
	// no snapshot to count the half-life in, or a model that could not be
	// read. run, key and halfLife are set with it.
	model    *needModel
	run      *ledger.Run
	key      needKey
	halfLife int64
}

// plan predicts one file's increment and prepares what it is learned under.
// Learning needs a run, the run's parse stage and the snapshot the file
// belongs to, whose files of the language are the half-life; a diagnostic
// probe has no snapshot and is reserved at its prediction but teaches nothing.
// A model the run ledger cannot read is counted and stands aside for this
// file, which is reserved at the prior and learns nothing, and is read again
// for the next.
func (p *pool) plan(ctx context.Context, view model.SnapshotView, language string, size int64,
	languageOf func(model.FileVersion) (string, bool)) (*fileNeed, error) {
	f := &fileNeed{language: language, size: size, reserved: increment{bytes: admission.PriorNeed(size)}, prior: true}
	defer func() {
		if f.prior {
			p.mu.Lock()
			f.reserved.alone = p.priorShort[language]
			p.mu.Unlock()
		}
	}()
	run := ledger.RunFromContext(ctx)
	if run == nil || view == nil {
		return f, nil
	}
	p.mu.Lock()
	t := p.totals[run]
	p.mu.Unlock()
	if t == nil {
		return f, nil
	}
	halfLife, err := p.filesOf(ctx, t, view, language, languageOf)
	if err != nil {
		return nil, err
	}
	key := needKey{repository: t.repository,
		NeedKey: ledger.NeedKey{Language: language, Fingerprint: fingerprint, Build: p.build, SizeClass: admission.SizeClassOf(size)}}
	p.mu.Lock()
	m := p.models[key]
	if m == nil {
		m = &needModel{}
		p.models[key] = m
	}
	p.mu.Unlock()
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.loaded {
		state, ok, err := run.NeedModel(ctx, key.NeedKey)
		if err != nil {
			p.failedModel()
			return f, nil
		}
		// A state that does not decode teaches nothing: UnmarshalBinary leaves
		// the model empty, and the next observation's state supersedes it.
		if ok && m.h.UnmarshalBinary(state) != nil {
			p.failedModel()
		}
		m.loaded = true
	}
	if need, ok := m.h.Predict(size); ok {
		f.reserved, f.prior = increment{bytes: need}, false
	}
	f.model, f.run, f.key, f.halfLife = m, run, key, halfLife
	return f, nil
}

// filesOf is the count of the snapshot's files of language, streamed through
// the snapshot's manifest once per stage; only the per-language counts are
// kept. A count that fails is not kept, and the next file counts again.
func (p *pool) filesOf(ctx context.Context, t *stageTotal, view model.SnapshotView, language string,
	languageOf func(model.FileVersion) (string, bool)) (int64, error) {
	id := view.Header().ID
	p.mu.Lock()
	c := t.files[id]
	if c == nil {
		c = &languageFiles{}
		t.files[id] = c
	}
	p.mu.Unlock()
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.counted {
		files := map[string]int64{}
		err := view.EachFile(ctx, model.FileSelection{}, func(fv model.FileVersion) error {
			if fv.Status == model.FileDeleted {
				return nil
			}
			if name, ok := languageOf(fv); ok {
				files[name]++
			}
			return nil
		})
		if err != nil {
			return 0, err
		}
		c.files, c.counted = files, true
	}
	return c.files[language], nil
}

// dropModels forgets the models of repository once no stage of it is left.
// The pool lock must be held. The run ledger holds every state they reached.
func (p *pool) dropModels(repository string) {
	for _, t := range p.totals {
		if t.repository == repository {
			return
		}
	}
	for k := range p.models {
		if k.repository == repository {
			delete(p.models, k)
		}
	}
}

func (p *pool) failedModel() {
	p.mu.Lock()
	p.modelFailures++
	p.mu.Unlock()
}

// settle takes in one file's Done reading. A file whose need is absent learns
// nothing and is counted unmeasured: an unavailable need is never an
// observation of zero. A need above the file's increment is an overrun,
// counted per class and disclosed; the file has already succeeded. An overrun
// of a file reserved at the structural prior marks its language, whose later
// files reserved at the prior run alone, and warns once per language. A measured
// need is folded into the file's model, when it has one, and recorded on the
// run ledger with the model's state after it.
func (p *pool) settle(f *fileNeed, m wire.Memory) {
	if m.NeedBytes == nil || *m.NeedBytes > math.MaxInt64 {
		p.mu.Lock()
		p.unmeasured++
		p.mu.Unlock()
		return
	}
	need := int64(*m.NeedBytes)
	if need > f.reserved.bytes {
		class := overrunClass{language: f.language, sizeClass: admission.SizeClassOf(f.size)}
		p.mu.Lock()
		if f.prior && !p.priorShort[f.language] {
			p.priorShort[f.language] = true
			slog.Warn("a file reserved at the structural prior needed more than it, so every later first file of its language runs alone among the parses",
				"component", "treesitter", "language", f.language, "size_class", class.sizeClass,
				"reserved_bytes", f.reserved.bytes, "need_bytes", need)
		}
		o := p.overruns[class]
		if o == nil {
			o = &Overrun{Language: class.language, SizeClass: class.sizeClass}
			p.overruns[class] = o
		}
		o.Files++
		if drift := need - f.reserved.bytes; drift > o.LargestDriftBytes {
			o.LargestDriftBytes, o.ReservedBytes, o.NeedBytes = drift, f.reserved.bytes, need
		}
		p.mu.Unlock()
	}
	if f.model == nil {
		return
	}
	f.model.mu.Lock()
	defer f.model.mu.Unlock()
	f.model.h.Observe(need, f.size, f.halfLife)
	state, err := f.model.h.MarshalBinary()
	if err != nil {
		p.failedModel()
		return
	}
	f.run.ObserveNeed(ledger.NeedObservation{Key: f.key.NeedKey, ReservedBytes: f.reserved.bytes, NeedBytes: need, State: state})
}

// Overrun is one class's files whose observed need exceeded the increment
// they were admitted with (ADR-0012 decision 5). Each such file ran and
// succeeded; the drift is need minus reservation, and ReservedBytes and
// NeedBytes are those of the file with the largest.
type Overrun struct {
	Language          string `json:"language"`
	SizeClass         int    `json:"size_class"`
	Files             uint64 `json:"files"`
	LargestDriftBytes int64  `json:"largest_drift_bytes"`
	ReservedBytes     int64  `json:"reserved_bytes"`
	NeedBytes         int64  `json:"need_bytes"`
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
	// MaxParsesInFlight is the most files that held an increment at once.
	MaxParsesInFlight int `json:"max_parses_in_flight"`
	// StageWallMS is the wall time the parse stage was open, from its first
	// enter to its last leave, summed over every stage so far.
	StageWallMS int64 `json:"stage_wall_ms"`
	// WorkerCPUMS sums the processor time of every reaped worker, and is -1
	// when any of them was not measured.
	WorkerCPUMS int64 `json:"worker_cpu_ms"`
	// UnmeasuredFiles are the files whose worker reported no need, from which
	// nothing was learned, and NeedModelFailures the learned models that could
	// not be read or encoded, whose files were reserved at the prior or not
	// recorded.
	UnmeasuredFiles   uint64 `json:"unmeasured_files"`
	NeedModelFailures uint64 `json:"need_model_failures"`
	// Overruns are the classes in which a file needed more than its
	// increment, ordered by language and size class.
	Overruns []Overrun `json:"overruns,omitempty"`
	// PriorShortLanguages are the languages, in order, in which a file
	// reserved at the structural prior needed more than it, and FilesRunAlone
	// the later files of those languages that were reserved at the prior and
	// so ran alone among the parses.
	PriorShortLanguages []string `json:"prior_short_languages,omitempty"`
	FilesRunAlone       uint64   `json:"files_run_alone"`
	// WorkerRSSBytes sums the base each live worker last reported, and is -1
	// when any live worker has reported none.
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
		WorkersStarted: p.started, WorkersExited: p.exited, Parses: p.parses, Retries: p.retries,
		MaxParsesInFlight: p.maxInFlight, WorkerCPUMS: p.workerCPUMS, UnmeasuredFiles: p.unmeasured,
		NeedModelFailures: p.modelFailures, FilesRunAlone: p.aloneFiles, ParentRSSBytes: -1}
	for language := range p.priorShort {
		s.PriorShortLanguages = append(s.PriorShortLanguages, language)
	}
	slices.Sort(s.PriorShortLanguages)
	if p.cpuUnsampled > 0 {
		s.WorkerCPUMS = -1
	}
	for _, o := range p.overruns {
		s.Overruns = append(s.Overruns, *o)
	}
	slices.SortFunc(s.Overruns, func(a, b Overrun) int {
		if c := strings.Compare(a.Language, b.Language); c != 0 {
			return c
		}
		return a.SizeClass - b.SizeClass
	})
	if rss := residency.Read().Resident; rss != nil {
		s.ParentRSSBytes = int64(*rss)
	}
	for w := range p.live {
		s.WorkerPIDs = append(s.WorkerPIDs, int(w.pid.Load()))
		base := w.base.Load()
		if base < 0 || s.WorkerRSSBytes < 0 {
			s.WorkerRSSBytes = -1
			continue
		}
		s.WorkerRSSBytes += base
	}
	return s
}

func short(s string) string {
	if len(s) > 12 {
		return s[:12]
	}
	return s
}
