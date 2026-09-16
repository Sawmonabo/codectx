package ledger_test

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/Sawmonabo/codectx/internal/ledger"
)

// repositoryID is a fixed 32-byte identity in the wire shape every repository
// id has; the ledger never interprets it.
const repositoryID = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func openLedger(t *testing.T) (*ledger.Ledger, string) {
	t.Helper()
	dir := t.TempDir()
	l, err := ledger.Open(context.Background(), dir)
	if err != nil {
		t.Fatalf("open the ledger: %v", err)
	}
	return l, dir
}

func newRun(t *testing.T, l *ledger.Ledger) (*ledger.Run, context.Context) {
	t.Helper()
	run, err := l.NewRun(ledger.KindIndex, repositoryID)
	if err != nil {
		t.Fatalf("new run: %v", err)
	}
	return run, run.Context(context.Background())
}

func latest(t *testing.T, dir string) ledger.RunView {
	t.Helper()
	view, ok := latestIfAny(t, dir)
	if !ok {
		t.Fatal("the ledger holds no run for this repository")
	}
	return view
}

// latestIfAny is latest for a caller that is polling a live run: before the
// collector's first flush there is legitimately no row yet.
func latestIfAny(t *testing.T, dir string) (ledger.RunView, bool) {
	t.Helper()
	reader, open, err := ledger.OpenReader(context.Background(), dir)
	if err != nil {
		t.Fatalf("open the reader: %v", err)
	}
	if !open {
		t.Fatal("no ledger file was written beside the store")
	}
	defer reader.Close()
	view, ok, err := reader.LatestRun(context.Background(), repositoryID, 0)
	if err != nil {
		t.Fatalf("latest run: %v", err)
	}
	return view, ok
}

// TestSpanTreeRoundTrips protects the recording itself: if parents, order or
// counts did not survive the bus and the collector, every surface built on
// these rows would attribute a run's cost to the wrong stage, which is the
// whole point of recording it.
func TestSpanTreeRoundTrips(t *testing.T) {
	l, dir := openLedger(t)
	run, ctx := newRun(t, l)

	stageCtx, stage := ledger.Start(ctx, "structural_parse", "")
	stage.AddIn(3)
	unitCtx, unit := ledger.StartProvider(stageCtx, "engine_unit", "pkg/one", "structural")
	_, part := ledger.Start(unitCtx, "engine_unit", "pkg/one#0")
	part.AddOut(11)
	part.End(ledger.OutcomeOK, ledger.Measured{}, nil)
	unit.End(ledger.OutcomeSubdivided, ledger.Measured{CPUUnattributed: ledger.CPUOverlapped}, nil)
	stage.AddOut(4)
	stage.End(ledger.OutcomeOK, ledger.Measured{}, nil)
	run.Finish(ledger.OutcomeOK)
	if err := l.Stop(); err != nil {
		t.Fatalf("stop the ledger: %v", err)
	}

	view := latest(t, dir)
	if view.Run.RunID != run.ID() {
		t.Fatalf("read back run %s, recorded %s", view.Run.RunID, run.ID())
	}
	if view.Run.Outcome != ledger.OutcomeOK {
		t.Fatalf("run outcome is %q, want ok", view.Run.Outcome)
	}
	if len(view.Spans) != 3 {
		t.Fatalf("read back %d spans, recorded 3", len(view.Spans))
	}
	for i, want := range []struct {
		stage    string
		scope    string
		provider string
		parent   int64 // -1 for a top-level span
		in, out  int64
		outcome  ledger.Outcome
	}{
		{"structural_parse", "", "", -1, 3, 4, ledger.OutcomeOK},
		{"engine_unit", "pkg/one", "structural", 0, 0, 0, ledger.OutcomeSubdivided},
		{"engine_unit", "pkg/one#0", "", 1, 0, 11, ledger.OutcomeOK},
	} {
		got := view.Spans[i]
		if got.Seq != int64(i) {
			t.Fatalf("span %d reads back out of order, as ordinal %d", i, got.Seq)
		}
		parent := int64(-1)
		if got.ParentSeq != nil {
			parent = *got.ParentSeq
		}
		if got.Stage != want.stage || got.ScopeKey != want.scope || got.Provider != want.provider ||
			parent != want.parent || got.ItemsIn != want.in || got.ItemsOut != want.out || got.Outcome != want.outcome {
			t.Fatalf("span %d reads back as %+v, recorded %+v", i, got, want)
		}
		if got.Running {
			t.Fatalf("span %d reads back as running after its end", i)
		}
	}
	if cpu := view.Spans[1].CPUUserMS; cpu != nil {
		t.Fatalf("an overlapped span reports %d ms of CPU; unattributable CPU must be absent, never zero", *cpu)
	}
	if view.Spans[1].CPUUnattributed != ledger.CPUOverlapped {
		t.Fatalf("an overlapped span does not say why its CPU is absent: %q", view.Spans[1].CPUUnattributed)
	}
}

// TestFullBusDropsAndNeverBlocks protects the one guarantee that lets every
// stage record unconditionally: the run's own goroutines never wait on the
// ledger. A blocking send would make a slow flush stall a parser, and the loss
// must be counted on the run row or a reader would read an incomplete ledger
// as a complete one.
//
// Mutation: make publish's send blocking. The production below never returns
// and the test fails on its deadline.
func TestFullBusDropsAndNeverBlocks(t *testing.T) {
	l, dir := openLedger(t)
	release := make(chan struct{})
	// The subscriber runs on the collector goroutine, so blocking it stops the
	// collector draining: the bus fills exactly as it would behind a slow disk.
	l.Subscribe(func(ledger.SpanRow) { <-release })
	run, ctx := newRun(t, l)

	_, first := ledger.Start(ctx, "walk", "")
	first.End(ledger.OutcomeOK, ledger.Measured{}, nil)

	// Give the collector time to reach the blocked subscriber before the bus
	// is filled, so the events below have nowhere to go.
	produced := make(chan struct{})
	go func() {
		defer close(produced)
		time.Sleep(2 * flushIntervalForTest)
		for i := 0; i < 8192; i++ {
			_, span := ledger.Start(ctx, "precise_unit", "scope")
			span.End(ledger.OutcomeOK, ledger.Measured{}, nil)
		}
	}()
	select {
	case <-produced:
	case <-time.After(60 * time.Second):
		close(release)
		t.Fatal("recording blocked on a full bus; a run must never wait on its own accounting")
	}
	if run.Dropped() == 0 {
		t.Fatal("a full bus dropped nothing; either it blocked or it grew without bound")
	}
	close(release)
	run.Finish(ledger.OutcomeOK)
	if err := l.Stop(); err != nil {
		t.Fatalf("stop the ledger: %v", err)
	}
	view := latest(t, dir)
	if view.Run.EventsDropped == 0 {
		t.Fatal("the run row reports no dropped events, so a reader cannot tell this ledger is incomplete")
	}
}

// flushIntervalForTest mirrors the collector's own interval. It is a test's
// own constant on purpose: the product's is not a setting and nothing outside
// the package may read it.
const flushIntervalForTest = 250 * time.Millisecond

// TestReaderSeesRunningCountersAdvance protects the live view: a second
// process reading while a run works must see a running span's counters move,
// which is the difference between a progress display and a frozen one.
//
// Mutation: stop snapshotting running spans on flush. The counters stay at
// zero and the poll below times out.
func TestReaderSeesRunningCountersAdvance(t *testing.T) {
	l, dir := openLedger(t)
	run, ctx := newRun(t, l)
	_, span := ledger.Start(ctx, "lexical_build", "")
	span.AddIn(5)

	awaitCounters := func(wantIn int64) {
		t.Helper()
		deadline := time.Now().Add(30 * time.Second)
		var seen int64
		for time.Now().Before(deadline) {
			view, ok := latestIfAny(t, dir)
			if ok && len(view.Spans) == 1 {
				seen = view.Spans[0].ItemsIn
				if seen == wantIn {
					if !view.Spans[0].Running {
						t.Fatal("a span that has not ended does not read as running")
					}
					return
				}
			}
			time.Sleep(flushIntervalForTest / 5)
		}
		t.Fatalf("a reader on a second connection saw %d items after the span counted %d", seen, wantIn)
	}
	awaitCounters(5)
	span.AddIn(7)
	awaitCounters(12)

	span.End(ledger.OutcomeOK, ledger.Measured{}, nil)
	run.Finish(ledger.OutcomeOK)
	if err := l.Stop(); err != nil {
		t.Fatalf("stop the ledger: %v", err)
	}
}

// TestStopLeavesOpenSpansInterrupted protects the honesty of a cut-off run: a
// span whose run ended before it did must read as interrupted, not vanish (a
// reader would report a stage that never ran) and not read ok (a reader would
// report a success nobody observed).
//
// Mutation: leave the open spans running. The assertion below reads 'running'.
func TestStopLeavesOpenSpansInterrupted(t *testing.T) {
	l, dir := openLedger(t)
	run, ctx := newRun(t, l)
	stageCtx, _ := ledger.Start(ctx, "seal", "")
	_, child := ledger.Start(stageCtx, "activation", "")
	child.AddOut(2)
	if err := l.Stop(); err != nil {
		t.Fatalf("stop the ledger: %v", err)
	}

	view := latest(t, dir)
	if len(view.Spans) != 2 {
		t.Fatalf("a stopped run holds %d spans, recorded 2", len(view.Spans))
	}
	for _, got := range view.Spans {
		if got.Outcome != ledger.OutcomeInterrupted {
			t.Fatalf("a span open when its run ended reads %q, want interrupted", got.Outcome)
		}
		if got.FinishedAt != nil {
			t.Fatalf("an interrupted span carries a finish time of %s; it never finished", got.FinishedAt)
		}
	}
	if view.Run.Outcome != ledger.OutcomeInterrupted {
		t.Fatalf("a run stopped without an outcome reads %q, want interrupted", view.Run.Outcome)
	}
	if view.Run.RunID != run.ID() {
		t.Fatalf("read back run %s, recorded %s", view.Run.RunID, run.ID())
	}
	if _, err := os.Stat(ledger.Path(dir)); err != nil {
		t.Fatalf("the ledger file is not beside the store: %v", err)
	}
}
