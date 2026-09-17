package ledger

import (
	"context"
	"database/sql"
	"encoding/hex"
	"errors"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Sawmonabo/codectx/internal/model"
)

// The bus and the collector's batching are structural, not settings. A run
// produces hundreds to a few thousand events, so a bus of this depth is never
// reached by a healthy run and a run that does reach it has a defect worth
// reporting rather than a number worth raising.
const (
	// busDepth is the bounded channel between every run goroutine and the one
	// collector. A send that would block is dropped and counted instead.
	busDepth = 4096
	// flushInterval and flushEvents are the collector's two triggers,
	// whichever comes first. The interval is what bounds how stale a live
	// reader's view of a running span can be.
	flushInterval = 250 * time.Millisecond
	flushEvents   = 256
)

// LiveWindow is how far ahead of itself a run's collector stamps the liveness
// deadline it publishes on the run row, and so how long after a process dies
// its run still reads as live. It is a multiple of the flush interval rather
// than a duration chosen on its own: the collector renews the stamp on the
// flush it already performs, and the window has only to cover the longest a
// flush can legitimately be late -- a write waiting up to busyTimeout for the
// single writer that retention also takes, and a subscriber running on the
// collector's own goroutine. A hundred and twenty intervals is thirty seconds,
// six times that wait.
//
// It is exported because it is also the answer to the question a process
// waiting for the workspace lock asks: how long a holder may legitimately show
// no fresh stamp before it is treated as no longer progressing. That waiter
// reuses this window rather than choosing a second number, because the two
// questions are the same question.
const LiveWindow = 120 * flushInterval

// eventKind distinguishes the things a run's goroutine tells the collector.
type eventKind int

const (
	eventStart eventKind = iota
	eventBegin
	eventEnd
	// eventFinish says a run has ended. It crosses the bus rather than being
	// read off the run struct so that the collector sees it behind every end
	// the run already published: the bus is first-in-first-out and one
	// goroutine drains it, so by the time this arrives every span that did end
	// has been written, and the rows still 'planned' are exactly the work
	// nothing ever started.
	eventFinish
	// eventFlush is a barrier a caller waits on. It crosses the bus for the
	// same reason eventFinish does: the bus is first-in-first-out and one
	// goroutine drains it, so everything published before this event has been
	// written by the time the collector answers it.
	eventFlush
)

// An event is what crosses the bus. It is small and owns nothing the producing
// goroutine keeps writing to except the span's counters, which are atomics the
// collector reads rather than fields it copies.
type event struct {
	kind eventKind
	span *Span
	// run is set instead of span on an event about the run itself, which
	// belongs to no span.
	run *Run
	// planned marks a start event whose row is written as 'planned' rather
	// than 'running': the work exists but nothing has begun it.
	planned bool
	// ack answers an eventFlush, with the failure of the flush it waited on if
	// there was one. It is buffered, so the collector never waits on a caller
	// that has given up.
	ack      chan error
	outcome  Outcome
	measured Measured
	endWall  time.Time
	wallMS   int64
}

// A Ledger is a process's stable handle on the run ledger. It is composed once
// and lives as long as the workspace; what comes and goes underneath it is its
// COLLECTOR -- the database, the bus and the one goroutine that writes --
// which ATTACHES when this process takes the cross-process workspace lock for
// a building operation and DETACHES when that hold ends.
//
// The split is the one-writer-per-ledger invariant made structural. A process
// that answers questions and occasionally builds -- a server -- would
// otherwise hold a writer on this file for its whole session while holding the
// workspace for the beat that builds, and the person's own index beside it
// would be a second writer on a file whose separate-database design exists so
// there is only ever one.
//
// A DETACHED handle records nothing, exactly as a nil *Ledger does: NewRun
// returns a nil run, published events are dropped, and every query answers
// empty. That is what lets the surfaces that are handed this handle at
// composition -- the language-server manager's overlay run among them -- be
// instrumented unconditionally and degrade to recording nothing rather than
// writing without the lock.
//
// A nil *Ledger is a valid ledger that records nothing, exactly as a nil *Span
// is a valid span that records nothing: it opens nil runs, and every method on
// it does nothing and reports no failure. This is what lets a composition
// without a ledger -- a test of a package that records, or a process where
// opening the file failed -- be instrumented at all. The alternative is a
// guard at every one of the dozens of stage sites, where the first one anybody
// forgets panics the run it was meant to measure.
type Ledger struct {
	dir string

	// mu guards cur and stopped. It is held across an attach and a detach, so
	// the finalizing collector is gone before the next one exists and no two
	// collectors of this handle ever hold the writer at once.
	mu      sync.Mutex
	cur     *collector
	stopped bool

	// Subscriptions belong to the handle and not to an attachment: the log
	// line, the command line and the MCP server register once at composition
	// and must still be fed by the collector of every later hold.
	subMu sync.Mutex
	subs  []func(SpanRow)
}

// A collector is one attachment: the writer connection, the bus, and the
// goroutine that drains it. Its quit/done/stopOnce machinery is per
// attachment, because detaching finalizes exactly as stopping used to --
// drain, flush, write every still-open span as interrupted and every run's
// finish, then close the pool.
type collector struct {
	l  *Ledger
	db *sql.DB

	bus chan event
	// quit is what stops this collector. The bus is never closed: publish's
	// send is non-blocking, and a non-blocking send on a CLOSED channel fires
	// its case and panics, so closing the bus would crash any goroutine still
	// ending a span after a detach -- exactly what a deferred End in a
	// goroutine outliving its coordinator does. After quit closes, a late
	// event fills the buffer and is then dropped and counted, which is what a
	// dropped event already means.
	quit chan struct{}
	// writeMu guards the writer connection. The collector holds it for a
	// flush; the retention calls hold it for a delete. Nothing else writes.
	writeMu sync.Mutex

	done     chan struct{}
	stopOnce sync.Once
	stopErr  error
	// finalErr is the collector's last write, captured so a failure to close
	// the open spans reaches the caller of Detach instead of vanishing.
	finalErr error

	runsMu sync.Mutex
	runs   []*Run
}

// A Run is one recorded run: an index attempt, a deferred publication, or the
// per-process overlay. It allocates its spans' ordinals and carries the run
// row's own totals, which the collector writes at every flush.
//
// A nil *Run is the run of a ledger that records nothing: its methods do
// nothing, and the context it returns carries no run, so every span opened
// under it is a nil span.
type Run struct {
	c     *collector
	id    []byte
	idHex string
	kind  Kind
	repo  []byte

	seq     atomic.Int64
	dropped atomic.Int64
	// dirty is set whenever the run row's own contents change -- its totals,
	// its generation, its outcome, its dropped count -- so an idle flush
	// writes nothing at all rather than rewriting rows that have not moved.
	dirty atomic.Bool
	// discarded marks a run whose rows have been deleted and which this
	// process must therefore stop writing: the collector skips its events
	// rather than re-creating the row behind the delete.
	discarded atomic.Bool
	// stopped marks a run its collector has already finalized, which is every
	// run of a detached attachment. Such a run records nothing further: its
	// rows are closed, its bus has no drainer, and a surface that cached the
	// handle -- the language-server manager's per-process overlay run -- would
	// otherwise publish into a dead attachment for the life of the process.
	stopped atomic.Bool

	mu      sync.Mutex
	started time.Time
	// expires is the liveness deadline last written to the run row, held here
	// so the collector renews it before it lapses rather than on every flush.
	expires      time.Time
	generationID *int64
	outcome      Outcome
	finished     *time.Time
	totals       Totals
	// inserted is set by the collector once the run row exists, so the row is
	// written once and updated thereafter.
	inserted bool
}

// Totals are the run row's own counts, which the run reports as it learns
// them. ProcessPeakRSSBytes is nil where the platform does not expose the
// process's peak resident size.
//
// ProcessPeakRSSBytes is the kernel's high-water mark for THIS PROCESS, read
// once the run has ended, and not a mark taken over the run alone: in a
// one-shot command the two are the same figure, while in a long-lived process
// serving a fourth refresh the mark may have been set by the first.
type Totals struct {
	FileCount           int64
	SourceBytes         int64
	UnitsPlanned        int64
	UnitsSucceeded      int64
	UnitsFailed         int64
	UnitsSubdivided     int64
	ProcessPeakRSSBytes *uint64
}

// New composes the handle on the run ledger in dir -- the directory the index
// store lives in, under the same lock. It opens NOTHING: the file is opened,
// and the collector that writes it started, only by Attach.
func New(dir string) *Ledger { return &Ledger{dir: dir} }

// Attach opens the ledger file for writing and starts its collector. The
// caller must already hold the cross-process workspace lock for this
// workspace, and must Detach before it gives that lock up.
//
// A handle that already has a collector, or one that has been stopped, is a
// composition defect rather than a condition to recover from: the first is two
// writers in one process and the second is a hold taken after Close.
func (l *Ledger) Attach(ctx context.Context) error {
	if l == nil {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.stopped {
		return internal("the run ledger has been stopped and cannot record another hold")
	}
	if l.cur != nil {
		return internal("the run ledger already has a collector; one hold attaches it and the same hold detaches it")
	}
	path := Path(l.dir)
	if err := ensureDir(path); err != nil {
		return err
	}
	db, err := openPool(path, writerPragmas(), "immediate", 1, false)
	if err != nil {
		return err
	}
	if err := initSchema(ctx, db, path); err != nil {
		db.Close()
		return err
	}
	c := &collector{l: l, db: db, bus: make(chan event, busDepth),
		quit: make(chan struct{}), done: make(chan struct{})}
	l.cur = c
	go c.collect()
	return nil
}

// Detach finishes this handle's collector and closes the ledger file: it
// drains what is buffered, flushes it, writes every span still open as
// interrupted and every run's finish, and closes the pool. The handle survives
// and records nothing until the next Attach.
//
// It is the LAST thing a hold does before it releases the workspace lock.
// Release the lock first and the waiter acquiring on its next poll opens its
// collector while this one is still flushing -- two writers on the file again,
// which is the whole defect this split exists to remove.
func (l *Ledger) Detach() error {
	if l == nil {
		return nil
	}
	l.mu.Lock()
	c := l.cur
	l.cur = nil
	l.mu.Unlock()
	return c.stop()
}

// current is the attachment a call must go through, and nil when this handle
// records nothing -- because it is nil, because no hold has attached one, or
// because it has been stopped.
func (l *Ledger) current() *collector {
	if l == nil {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.cur
}

// stop finalizes one attachment and closes its database. It is idempotent and
// returns the first failure; a nil collector is a handle that was never
// attached and has nothing to finish.
func (c *collector) stop() error {
	if c == nil {
		return nil
	}
	c.stopOnce.Do(func() {
		close(c.quit)
		<-c.done
		c.stopErr = c.finalErr
		if err := c.db.Close(); err != nil && c.stopErr == nil {
			c.stopErr = wrap("close", err)
		}
	})
	return c.stopErr
}

// NewRun records a new run and returns its handle. The run id is 32 random
// bytes, allocated before any generation exists, so a run that fails before
// publication still has a complete ledger. A ledger that records nothing --
// nil, or a handle no hold has attached a collector to -- opens a run that
// records nothing, and validates nothing, because there is nothing to record.
//
// The run belongs to the attachment that was current when it opened. When that
// attachment detaches the run is finalized with it and records nothing
// further, so a caller that cached one across a hold degrades to the same
// no-op a composition with no ledger already gets.
func (l *Ledger) NewRun(kind Kind, repositoryID string) (*Run, error) {
	c := l.current()
	if c == nil {
		return nil, nil
	}
	switch kind {
	case KindIndex, KindDeferred, KindOverlay:
	default:
		return nil, invalid("run kind %q is not one of index, deferred or overlay", string(kind))
	}
	repo, err := model.DecodeID(repositoryID)
	if err != nil {
		return nil, err
	}
	idHex, err := model.NewRandomID()
	if err != nil {
		return nil, err
	}
	id, err := hex.DecodeString(idHex)
	if err != nil {
		return nil, internal("run identifier: " + err.Error())
	}
	run := &Run{c: c, id: id, idHex: idHex, kind: kind, repo: repo,
		started: time.Now(), outcome: OutcomeRunning}
	run.dirty.Store(true)
	c.runsMu.Lock()
	c.runs = append(c.runs, run)
	c.runsMu.Unlock()
	return run, nil
}

// ID is the run's public identifier: the same 32 bytes as lowercase hex, and
// empty for a run that records nothing.
func (r *Run) ID() string {
	if r == nil {
		return ""
	}
	return r.idHex
}

// Context returns a context whose spans belong to this run. Every package that
// records is handed one of these and calls Start; none of them holds a *Run.
func (r *Run) Context(ctx context.Context) context.Context {
	if r == nil {
		return ctx
	}
	return context.WithValue(ctx, runContextKey{}, r)
}

// AttachGeneration records the generation this run produced, once publication
// has opened one. A run that never reaches it leaves the column null.
func (r *Run) AttachGeneration(id int64) {
	if r == nil {
		return
	}
	r.mu.Lock()
	r.generationID = &id
	r.mu.Unlock()
	r.dirty.Store(true)
}

// Report records the run row's totals as the run learns them. The last call
// before the collector's next flush is what a reader sees.
func (r *Run) Report(t Totals) {
	if r == nil {
		return
	}
	r.mu.Lock()
	r.totals = t
	r.mu.Unlock()
	r.dirty.Store(true)
}

// Finish records how the run ended, and tells the collector so, because the
// end of a run is itself an ending to record: the units the plan named that
// nothing ever started are closed here, through the same publication every
// other span end goes through. Spans still open when the ledger stops are
// written interrupted regardless of what the run says about itself.
func (r *Run) Finish(outcome Outcome) {
	if r == nil {
		return
	}
	now := time.Now()
	r.mu.Lock()
	r.outcome = outcome
	r.finished = &now
	r.mu.Unlock()
	r.dirty.Store(true)
	r.c.publish(event{kind: eventFinish, run: r})
}

// Dropped is how many of this run's events the bus refused. It is on the run
// and not on an event because a drop happens exactly when no event gets
// through: an event carrying the count would be lost at the only moment it
// matters.
func (r *Run) Dropped() int64 {
	if r == nil {
		return 0
	}
	return r.dropped.Load()
}

// publish is the one non-blocking send. A bus with no room drops the event and
// counts it on the run row; the run itself never waits on its own accounting.
//
// A run its collector has already finalized publishes nothing at all: nobody
// drains a detached attachment's bus, so an event sent there would be retained
// until the process exits and would be counted as a drop on a row that is
// already closed.
func (c *collector) publish(e event) {
	run := e.run
	if run == nil {
		run = e.span.run
	}
	if run.stopped.Load() {
		return
	}
	select {
	case c.bus <- e:
	default:
		run.dropped.Add(1)
		run.dirty.Store(true)
	}
}

// refreshDue reports whether this run's published liveness deadline is close
// enough to lapsing that the collector should renew it on this flush. Renewing
// at half the window leaves a whole window of slack for a late flush, and
// leaves an idle live run writing twice a window instead of four times a
// second. A run that has ended publishes nothing further: its outcome, not its
// stamp, is what a reader then judges it by.
func (r *Run) refreshDue(now time.Time) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.inserted && r.outcome == OutcomeRunning && !now.Before(r.expires.Add(-LiveWindow/2))
}

// anyRefreshDue reports whether any run's liveness deadline needs renewing, so
// a flush with nothing else to do still keeps a live run's claim current.
func (c *collector) anyRefreshDue(now time.Time) bool {
	c.runsMu.Lock()
	runs := append([]*Run(nil), c.runs...)
	c.runsMu.Unlock()
	for _, run := range runs {
		if run.refreshDue(now) {
			return true
		}
	}
	return false
}

// retire drops a run the collector has finished with: it has ended, its rows
// and its totals are written, and the sweep of what it never started is done.
//
// The list has to be bounded because a long-lived process opens a run per
// refresh and never closes the ledger -- a server would otherwise hold one
// entry per run it has ever done, and the two per-flush walks over this list
// would grow with it for the life of the process. What a retired run is still
// asked for is read from the database by its identifier, not from here.
//
// It is called only from the collector, after the flush that completed the
// run's sweep, so nothing is still writing the run when it goes.
func (c *collector) retire(run *Run) {
	c.runsMu.Lock()
	defer c.runsMu.Unlock()
	for i, held := range c.runs {
		if held == run {
			c.runs = append(c.runs[:i], c.runs[i+1:]...)
			return
		}
	}
}

// anyDirty reports whether any run row has moved since the last flush wrote
// it. It is what lets an idle collector open no transaction at all.
func (c *collector) anyDirty() bool {
	c.runsMu.Lock()
	defer c.runsMu.Unlock()
	for _, run := range c.runs {
		if run.dirty.Load() {
			return true
		}
	}
	return false
}

// Subscribe registers a function the collector calls with every span that
// finishes, in the order the collector writes them. It is the one place a
// finished span is published: the log line, the progress notification and the
// per-span message all come from here, never from the caller's own goroutine,
// so every surface sees the same event once.
//
// The function runs on the collector goroutine. It must not block: a
// subscriber that waits delays every later flush, and with it every live
// reader's view.
func (l *Ledger) Subscribe(fn func(SpanRow)) {
	if l == nil {
		return
	}
	l.subMu.Lock()
	defer l.subMu.Unlock()
	l.subs = append(l.subs, fn)
}

// IndexRunID is the identifier of the indexing run this process is recording
// at this moment, and false when it is recording none. It exists because a
// process records more than one run at a time -- a language server's start
// hangs under a per-process overlay run that can be open while an index run
// is going -- so a surface that follows an indexing run has to be able to say
// which of them a span it is handed came from.
//
// Runs are held in the order they opened and a process indexes under one
// cross-process owner, so the last one still running is the one a caller is
// asking about. A run that has finished is not it: its rows are complete, and
// answering with it would hand a later caller a run that is over.
func (l *Ledger) IndexRunID() (string, bool) {
	c := l.current()
	if c == nil {
		return "", false
	}
	c.runsMu.Lock()
	runs := append([]*Run(nil), c.runs...)
	c.runsMu.Unlock()
	for i := len(runs) - 1; i >= 0; i-- {
		run := runs[i]
		if run.kind != KindIndex {
			continue
		}
		run.mu.Lock()
		running := run.outcome == OutcomeRunning
		run.mu.Unlock()
		if running {
			return run.idHex, true
		}
	}
	return "", false
}

func (l *Ledger) notify(row SpanRow) {
	l.subMu.Lock()
	subs := l.subs
	l.subMu.Unlock()
	for _, fn := range subs {
		fn(row)
	}
}

// Flush returns once the collector has written everything published before
// the call: the spans that ended, the run rows whose totals or outcome moved,
// and the rows a finished run left planned, which the collector closes in
// batches and this waits out in full.
//
// It exists because finishing a run moves in-memory state, and the row turns
// terminal only at the next flush: a read straight after a run returns would
// otherwise render a finished run as still running, with its last stages
// missing. It is a barrier and not a wait: nothing is slept on and nothing is
// polled, the collector answers the event when the write it was already going
// to do has committed.
//
// Nothing a run records goes through here, so the bus keeps its non-blocking
// discipline: the barrier's own send waits, because its caller is a reader
// that asked to wait, while every producer's publish still drops rather than
// block. A ledger that records nothing, and one whose collector has already
// stopped and therefore written everything it had, return at once.
func (l *Ledger) Flush(ctx context.Context) error {
	c := l.current()
	if c == nil {
		return nil
	}
	ack := make(chan error, 1)
	select {
	case c.bus <- event{kind: eventFlush, ack: ack}:
	case <-c.done:
		return nil
	case <-ctx.Done():
		return wrap("flush the ledger", ctx.Err())
	}
	select {
	case err := <-ack:
		return err
	case <-c.done:
		return nil
	case <-ctx.Done():
		return wrap("flush the ledger", ctx.Err())
	}
}

// Stop retires the handle for good: it detaches whatever collector is still
// attached -- draining, flushing, writing every span still open as interrupted
// and every run's finish, and closing the database -- and refuses any further
// attach.
//
// It is what the composition's Close calls, and it is the backstop for a hold
// that ended without detaching: a collector a leaked hold left open still
// holds a writer on this file, and the process must not exit leaving it there.
// It is safe to call more than once and returns the first failure.
func (l *Ledger) Stop() error {
	if l == nil {
		return nil
	}
	l.mu.Lock()
	c := l.cur
	l.cur = nil
	l.stopped = true
	l.mu.Unlock()
	return c.stop()
}

// DeleteRuns removes the runs that produced the given generations, with their
// spans. It is what the index store's retention calls when it deletes a
// generation, so the two lifetimes stay the same one; this package does not
// wire it, because it does not know when a generation goes.
func (l *Ledger) DeleteRuns(ctx context.Context, generationIDs []int64) error {
	c := l.current()
	if c == nil || len(generationIDs) == 0 {
		return nil
	}
	return c.writeTx(ctx, func(tx *sql.Tx) error {
		stmt, err := tx.PrepareContext(ctx, `DELETE FROM runs WHERE generation_id = ?`)
		if err != nil {
			return wrap("delete runs", err)
		}
		defer stmt.Close()
		for _, id := range generationIDs {
			if _, err := stmt.ExecContext(ctx, id); err != nil {
				return wrap("delete runs", err)
			}
		}
		return nil
	})
}

// DeleteOverlayRuns removes the overlay runs of processes that are no longer
// running: an overlay run has no generation, so nothing else would ever
// collect it. A process that exits cleanly removes its own through DiscardRun,
// which is ordered against its own writer; this is the collection pass's call
// for the ones that did not.
func (l *Ledger) DeleteOverlayRuns(ctx context.Context, runIDs []string) error {
	c := l.current()
	if c == nil || len(runIDs) == 0 {
		return nil
	}
	raw := make([][]byte, 0, len(runIDs))
	for _, id := range runIDs {
		decoded, err := model.DecodeID(id)
		if err != nil {
			return err
		}
		raw = append(raw, decoded)
	}
	return c.writeTx(ctx, func(tx *sql.Tx) error {
		stmt, err := tx.PrepareContext(ctx, `DELETE FROM runs WHERE run_id = ? AND kind = 'overlay'`)
		if err != nil {
			return wrap("delete overlay runs", err)
		}
		defer stmt.Close()
		for _, id := range raw {
			if _, err := stmt.ExecContext(ctx, id); err != nil {
				return wrap("delete overlay runs", err)
			}
		}
		return nil
	})
}

// DiscardRun deletes a run and everything recorded under it, and stops this
// process recording it at all. It is what a run that exists only for the life
// of one process does at clean exit: the per-process overlay a language
// server's start hangs under belongs to no generation, so the retention that
// deletes a generation's runs would never reach it and the file would keep one
// run and its spans per process for ever.
//
// The run is forgotten BEFORE the delete, and forgotten for good: an event of
// its still on the bus, and the interrupted-marking pass a stopping ledger
// makes over every run it knows, would otherwise write the row back in behind
// the delete. The delete itself takes the writer, so a flush already in
// progress finishes first and this sees the row it wrote.
func (l *Ledger) DiscardRun(ctx context.Context, run *Run) error {
	// The run must belong to the attachment that is current: a run of an
	// earlier hold has already been finalized and its rows closed, and
	// deleting it through THIS collector would be one attachment reaching into
	// another's accounting. A caller that cached such a run gets the same
	// no-op a ledger that records nothing gives it; the sweep of overlay runs
	// whose writer is gone is what collects the row it left.
	if run == nil {
		return nil
	}
	c := l.current()
	if c == nil || run.c != c {
		return nil
	}
	run.discarded.Store(true)
	c.runsMu.Lock()
	for i, r := range c.runs {
		if r == run {
			c.runs = append(c.runs[:i], c.runs[i+1:]...)
			break
		}
	}
	c.runsMu.Unlock()
	return c.writeTx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `DELETE FROM runs WHERE run_id = ?`, run.id)
		return wrap("discard the run", err)
	})
}

// notLive is the negation of the liveness judgement LatestRun makes: a run is
// live while it says 'running' and the deadline its own writer published has
// not elapsed. Every sweep uses this same expression with the same bound clock
// reading, so what a reader is told is alive and what a sweep is willing to
// delete can never disagree -- the one bug that would matter here is deleting
// the record of a process another surface has just called live.
const notLive = `NOT (outcome = 'running' AND expires_at > ?)`

// DeleteRunsWithoutGeneration removes the runs no generation will ever reach:
// a run that ended without publishing anything -- a tick that found nothing to
// publish never attaches a generation -- and one whose process died before it
// could. DeleteRuns is keyed by generation, so those rows have no other way
// out, and the ticks that produce them are periodic: without this pass they
// accumulate without bound.
//
// It deletes only runs that are not live: a run that has not reached its
// generation yet is exactly what a run in progress looks like. One pass
// deletes at most a page, because it is called periodically and the next pass
// takes the rest.
func (l *Ledger) DeleteRunsWithoutGeneration(ctx context.Context) error {
	c := l.current()
	if c == nil {
		return nil
	}
	return c.writeTx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `DELETE FROM runs WHERE run_id IN (
			SELECT run_id FROM runs WHERE generation_id IS NULL AND `+notLive+
			` ORDER BY started_at LIMIT ?)`, formatTime(time.Now()), model.MaxRecordsPerResult)
		return wrap("delete runs without a generation", err)
	})
}

// OverlayRuns lists the overlay runs whose writer is gone: an overlay run that
// finished, and one whose process died without stopping its ledger and whose
// published deadline has since elapsed. A live overlay run -- another process
// serving a language server right now -- is never listed, so the caller can
// hand everything it gets straight to DeleteOverlayRuns.
func (l *Ledger) OverlayRuns(ctx context.Context) ([]string, error) {
	c := l.current()
	if c == nil {
		return nil, nil
	}
	var ids []string
	rows, err := c.db.QueryContext(ctx, `SELECT run_id FROM runs WHERE kind = 'overlay' AND `+notLive+
		` ORDER BY started_at LIMIT ?`, formatTime(time.Now()), model.MaxRecordsPerResult)
	if err != nil {
		return nil, wrap("overlay runs", err)
	}
	defer rows.Close()
	for rows.Next() {
		var raw []byte
		if err := rows.Scan(&raw); err != nil {
			return nil, wrap("overlay runs", err)
		}
		ids = append(ids, hex.EncodeToString(raw))
	}
	return ids, wrap("overlay runs", rows.Err())
}

// writeTx runs fn in one immediate write transaction on the single writer,
// serialized against the collector's flushes.
func (c *collector) writeTx(ctx context.Context, fn func(tx *sql.Tx) error) error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	tx, err := c.db.BeginTx(ctx, nil)
	if err != nil {
		return wrap("begin", err)
	}
	if err := fn(tx); err != nil {
		tx.Rollback()
		return err
	}
	return wrap("commit", tx.Commit())
}

// diagnosticCode reports a typed error's code, and the internal family for an
// error that carries none, so a failed span always names a family.
func diagnosticCode(err error) string {
	var typed *model.Error
	if errors.As(err, &typed) {
		return typed.Code
	}
	return model.CodeInternal
}
