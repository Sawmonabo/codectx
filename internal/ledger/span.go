package ledger

import (
	"context"
	"sync/atomic"
	"time"
)

// An Outcome is what became of a run or a span. 'planned' is a unit that has a
// row before anything ran it, so a unit that never reaches its work still
// exists; 'running' is the value a span carries between its start and its end;
// 'interrupted' is what a span that was still running when its run ended is
// left as, which is neither a success nor a recorded failure but a measurement
// that was cut off; 'unavailable' is work that reached no output because
// something it needed was absent, and it carries the reason.
type Outcome string

const (
	OutcomePlanned     Outcome = "planned"
	OutcomeRunning     Outcome = "running"
	OutcomeOK          Outcome = "ok"
	OutcomeFailed      Outcome = "failed"
	OutcomeSubdivided  Outcome = "subdivided"
	OutcomeReused      Outcome = "reused"
	OutcomeSkipped     Outcome = "skipped"
	OutcomeInterrupted Outcome = "interrupted"
	OutcomeUnavailable Outcome = "unavailable"
)

// ReasonNotAdmitted is the reason of a unit whose span was still 'planned' when
// its run ended: nothing ever started it, so it was never admitted to the work
// its plan named. It is the ledger's own reason rather than a provider's,
// because no provider was ever reached to state one.
const ReasonNotAdmitted = "the unit was planned but never reached admission"

// A Kind is what opened a run: an index run, the deferred publication of a
// generation whose run had already ended, or the per-process overlay a
// language server's startup hangs under when no run is live.
type Kind string

const (
	KindIndex    Kind = "index"
	KindDeferred Kind = "deferred"
	KindOverlay  Kind = "overlay"
)

// Why a span carries no CPU figures. The empty value means the figures are
// there; the others name what the process could not measure, so a reader never
// has to guess whether a null is a zero.
const (
	// CPUAttributed is the value of a span whose CPU columns are populated.
	CPUAttributed = ""
	// CPUOverlapped is in-process work that ran beside other goroutines. The
	// process-wide counters measure the process, and attributing them to one
	// stage would be a guess; a null is the honest answer.
	CPUOverlapped = "overlapped"
	// CPUUnsampled is a platform that does not expose the counters at all.
	CPUUnsampled = "unsampled"
)

// Measured is what a stage knows about its own cost when it ends. Every field
// is a pointer because the platform cannot give all of them everywhere: a
// field it cannot give is absent, never zero, so a reader is never shown a
// measurement nobody made.
//
// The child-process fields are filled by whatever ran the child from the
// accounting it already holds -- the resource usage at exit, the tree peak the
// sampler observed, the byte counters read before exit. An in-process stage
// leaves them nil.
type Measured struct {
	CPUUserMS *int64
	CPUSysMS  *int64
	// CPUUnattributed names why CPUUserMS and CPUSysMS are nil. It must be
	// empty when they are set and one of CPUOverlapped or CPUUnsampled when
	// they are not.
	CPUUnattributed string
	PeakRSSBytes    *uint64
	ReadBytes       *uint64
	WriteBytes      *uint64
	// ItemsIn and ItemsOut override the span's own counters for a stage that
	// only knows its totals at the end. A stage that counted as it went leaves
	// them nil and the counters stand.
	ItemsIn  *int64
	ItemsOut *int64
	// DiagnosticCode and Failure carry a failed span's typed error. They are
	// filled from the error End is given and a caller normally leaves them
	// empty.
	DiagnosticCode string
	Failure        string
}

// A Span is one bracketed piece of work: a run's root, a stage, a unit under a
// stage, a part of a subdivided unit. A nil *Span is valid and does nothing,
// which is what a caller that was handed no run gets, so instrumenting a code
// path never requires the caller to know whether a ledger is open.
type Span struct {
	run    *Run
	seq    int64
	parent int64 // the parent's seq; -1 when this span has no parent
	// started is both the timestamp written to the row and the reading the
	// wall is measured from: a time.Time carries a monotonic reading that Sub
	// uses and Format drops, so the wall does not move when the clock is
	// adjusted and the stored timestamp is still the wall clock's.
	started  time.Time
	stage    string
	scopeKey string
	provider string

	itemsIn  atomic.Int64
	itemsOut atomic.Int64
	ended    atomic.Bool
}

// AddIn and AddOut count what went into and came out of this span -- files
// walked, records emitted, rows staged, bytes imported. They are one atomic
// add per item and never touch the bus, so a stage can count per file while a
// span is a thing that costs seconds. The collector snapshots them at every
// flush, which is how a reader in another process sees a live span advance.
func (s *Span) AddIn(n int64) {
	if s == nil {
		return
	}
	s.itemsIn.Add(n)
}

func (s *Span) AddOut(n int64) {
	if s == nil {
		return
	}
	s.itemsOut.Add(n)
}

// In and Out read the counters back, for a caller that reports its own totals.
func (s *Span) In() int64 {
	if s == nil {
		return 0
	}
	return s.itemsIn.Load()
}

func (s *Span) Out() int64 {
	if s == nil {
		return 0
	}
	return s.itemsOut.Load()
}

// spanContextKey carries the innermost open span, so a nested Start finds its
// parent without any caller naming it: a provider's inner stages nest under the
// unit span the coordinator opened without the provider knowing this package's
// shape.
type spanContextKey struct{}

// runContextKey carries the run a context's work belongs to. Start reads it,
// so a package that only opens spans never holds a *Run.
type runContextKey struct{}

// FromContext returns the innermost open span, or nil where none is open.
func FromContext(ctx context.Context) *Span {
	s, _ := ctx.Value(spanContextKey{}).(*Span)
	return s
}

// RunFromContext returns the run a context's work belongs to, or nil.
func RunFromContext(ctx context.Context) *Run {
	r, _ := ctx.Value(runContextKey{}).(*Run)
	return r
}

// Start opens a span under whatever span the context already carries, and
// returns a context a nested Start will find it in. With no run in the context
// it returns the context unchanged and a nil span whose methods do nothing.
//
// It allocates the span's ordinal and does one non-blocking send. It never
// waits: a full bus drops the event and counts it on the run row.
func Start(ctx context.Context, stage, scopeKey string) (context.Context, *Span) {
	return StartProvider(ctx, stage, scopeKey, "")
}

// Plan opens a span for work that has been decided on but not started. The row
// exists from the moment the plan names the unit, so a unit that is never run
// -- because nothing admitted it, or because the run ended first -- still has a
// row that says so, instead of leaving no trace at all.
//
// It returns no context: a planned span has no work under it yet. Begin makes
// it the parent of what runs.
func Plan(ctx context.Context, stage, scopeKey, provider string) *Span {
	run := RunFromContext(ctx)
	if run == nil {
		return nil
	}
	parent := int64(-1)
	if p := FromContext(ctx); p != nil {
		parent = p.seq
	}
	span := &Span{run: run, seq: run.seq.Add(1) - 1, parent: parent, started: time.Now(),
		stage: stage, scopeKey: scopeKey, provider: provider}
	run.c.publish(event{kind: eventStart, span: span, planned: true})
	return span
}

// Begin moves a planned span to running and returns a context a nested Start
// finds it in. The span's wall is measured from here and not from the plan, so
// a unit's cost stays the cost of running it and never the time it waited in a
// queue; what the row says about the wait is that it was 'planned' until now.
func (s *Span) Begin(ctx context.Context) context.Context {
	if s == nil {
		return ctx
	}
	s.started = time.Now()
	s.run.c.publish(event{kind: eventBegin, span: s})
	return context.WithValue(ctx, spanContextKey{}, s)
}

// StartProvider is Start for a span a provider owns, which records which
// provider produced it.
func StartProvider(ctx context.Context, stage, scopeKey, provider string) (context.Context, *Span) {
	run := RunFromContext(ctx)
	if run == nil {
		return ctx, nil
	}
	parent := int64(-1)
	if p := FromContext(ctx); p != nil {
		parent = p.seq
	}
	now := time.Now()
	span := &Span{
		run:      run,
		seq:      run.seq.Add(1) - 1,
		parent:   parent,
		started:  now,
		stage:    stage,
		scopeKey: scopeKey,
		provider: provider,
	}
	run.c.publish(event{kind: eventStart, span: span})
	return context.WithValue(ctx, spanContextKey{}, span), span
}

// End closes the span. A second call does nothing: a stage that ends its span
// on both a normal return and a deferred cleanup records one end, not two.
//
// err fills the failed span's diagnostic code and message when the caller did
// not fill them itself. The message is the error's own text; no source bytes
// and no raw analyzer output reach this row.
func (s *Span) End(outcome Outcome, m Measured, err error) {
	if s == nil || !s.ended.CompareAndSwap(false, true) {
		return
	}
	if err != nil {
		if m.DiagnosticCode == "" {
			m.DiagnosticCode = diagnosticCode(err)
		}
		if m.Failure == "" {
			m.Failure = err.Error()
		}
	}
	if m.ItemsIn != nil {
		s.itemsIn.Store(*m.ItemsIn)
	}
	if m.ItemsOut != nil {
		s.itemsOut.Store(*m.ItemsOut)
	}
	end := time.Now()
	s.run.c.publish(event{
		kind:     eventEnd,
		span:     s,
		outcome:  outcome,
		measured: m,
		endWall:  end,
		wallMS:   end.Sub(s.started).Milliseconds(),
	})
}
