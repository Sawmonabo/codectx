package ledger_test

import (
	"context"
	"os"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/Sawmonabo/codectx/internal/ledger"
	"github.com/Sawmonabo/codectx/internal/model"
)

// repositoryID is a fixed 32-byte identity in the wire shape every repository
// id has; the ledger never interprets it.
const repositoryID = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func openLedger(t *testing.T) (*ledger.Ledger, string) {
	t.Helper()
	dir := t.TempDir()
	l := ledger.New(dir)
	err := l.Attach(context.Background())
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

// TestUnopenedLedgerRecordsNothing protects the package's rule that
// instrumentation is unconditionally callable. A composition with no ledger --
// any test of a package that records, or a process where opening the ledger
// file failed -- must be able to open runs and spans that do nothing. Without
// it every one of the dozens of stage sites needs its own nil check, and the
// first one anybody forgets panics the run it was there to measure.
func TestUnopenedLedgerRecordsNothing(t *testing.T) {
	var l *ledger.Ledger
	run, err := l.NewRun(ledger.KindIndex, repositoryID)
	if err != nil || run != nil {
		t.Fatalf("a ledger that records nothing opened run %v (%v), want no run and no failure", run, err)
	}
	base := context.Background()
	ctx := run.Context(base)
	if ctx != base {
		t.Fatal("a run that records nothing put itself in the context")
	}
	spanCtx, span := ledger.Start(ctx, "seal", "")
	span.AddIn(3)
	span.End(ledger.OutcomeOK, ledger.Measured{}, nil)
	run.AttachGeneration(7)
	run.Report(ledger.Totals{FileCount: 1})
	run.Finish(ledger.OutcomeOK)
	l.Subscribe(func(ledger.SpanRow) { t.Error("a ledger that records nothing published a span") })
	if span != nil || spanCtx != ctx || run.ID() != "" || run.Dropped() != 0 {
		t.Fatalf("span %v, run id %q, dropped %d: a run that records nothing reported something",
			span, run.ID(), run.Dropped())
	}
	overlay, err := l.OverlayRuns(ctx)
	if len(overlay) != 0 || err != nil {
		t.Fatalf("overlay runs %v (%v), want none and no failure", overlay, err)
	}
	if err := l.DeleteRuns(ctx, []int64{7}); err != nil {
		t.Fatalf("delete runs: %v", err)
	}
	if err := l.DeleteOverlayRuns(ctx, []string{repositoryID}); err != nil {
		t.Fatalf("delete overlay runs: %v", err)
	}
	if err := l.Stop(); err != nil {
		t.Fatalf("stop a ledger that was never opened: %v", err)
	}
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

// TestUnadmittedUnitReachesASubscriberLikeAnyOtherEnding protects the live
// surfaces against going silent on the endings that matter most. A unit the
// plan named and nothing ever started is the one an operator is waiting to be
// told about, and it is the only ending that never came from a stage's own
// End: it is discovered when the run ends. If that discovery writes the row
// directly instead of publishing it, the structured log line and the tool
// notification are missing for exactly those units, and the only way to learn
// of them is to go and read the rows afterwards.
//
// Mutation: close the planned rows with the raw UPDATE again instead of
// sweepPlanned. The unadmitted scope below never reaches the subscriber.
func TestUnadmittedUnitReachesASubscriberLikeAnyOtherEnding(t *testing.T) {
	l, _ := openLedger(t)
	var mu sync.Mutex
	published := map[string]ledger.SpanRow{}
	// The subscriber runs on the collector's goroutine, so the map it fills is
	// read under the same lock and never while that goroutine is mid-flush.
	l.Subscribe(func(row ledger.SpanRow) {
		mu.Lock()
		defer mu.Unlock()
		published[row.ScopeKey] = row
	})
	seen := func(scope string) (ledger.SpanRow, bool) {
		mu.Lock()
		defer mu.Unlock()
		row, ok := published[scope]
		return row, ok
	}

	run, ctx := newRun(t, l)
	_, ran := ledger.StartProvider(ctx, "engine_unit", "pkg/ran", "structural")
	ran.End(ledger.OutcomeOK, ledger.Measured{}, nil)
	ledger.Plan(ctx, "engine_unit", "pkg/unadmitted", "structural")
	run.Finish(ledger.OutcomeOK)

	// Published when the run ends, not when the process does: a client waiting
	// on this run's notifications is gone by the time the ledger stops.
	var row ledger.SpanRow
	deadline := time.Now().Add(20 * flushIntervalForTest)
	for {
		var ok bool
		if row, ok = seen("pkg/unadmitted"); ok {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the run ended and the unit nothing admitted reached no subscriber: the log line and the tool notification an operator is waiting for were never published")
		}
		time.Sleep(flushIntervalForTest / 5)
	}
	if _, ok := seen("pkg/ran"); !ok {
		t.Fatal("the unit that ended during the run reached no subscriber")
	}
	if row.Outcome != ledger.OutcomeUnavailable || row.DiagnosticCode != model.CodeProviderUnavailable ||
		row.Failure != ledger.ReasonNotAdmitted {
		t.Fatalf("published %q/%q/%q, want unavailable with the not-admitted reason",
			row.Outcome, row.DiagnosticCode, row.Failure)
	}
	// Nobody measured this unit, so the row states no measurement: a stamped
	// finish and a zero wall would be a figure nobody observed.
	if row.FinishedAt != nil || row.WallMS != 0 || row.Running {
		t.Fatalf("published finish %v wall %d running %v, want no measurement at all",
			row.FinishedAt, row.WallMS, row.Running)
	}
	if err := l.Stop(); err != nil {
		t.Fatalf("stop: %v", err)
	}
}

// TestSpansPastOnePageAreCountedAsOmitted protects a bounded answer from being
// read as the whole of it. The page is what the surfaces render; the omitted
// count is the only thing that says the run had more. Computed wrongly here it
// would render faithfully at every surface and still be wrong, and an operator
// would take a thousand stages for all of them.
//
// Mutation: return the count as zero from Reader.spans. The assertion below
// reads 0 where the run left three spans out.
func TestSpansPastOnePageAreCountedAsOmitted(t *testing.T) {
	const beyond = 3
	l, dir := openLedger(t)
	run, ctx := newRun(t, l)
	for i := 0; i < model.MaxRecordsPerResult+beyond; i++ {
		_, span := ledger.Start(ctx, "engine_unit", "pkg/"+strconv.Itoa(i))
		span.End(ledger.OutcomeOK, ledger.Measured{}, nil)
	}
	run.Finish(ledger.OutcomeOK)
	if err := l.Stop(); err != nil {
		t.Fatalf("stop: %v", err)
	}
	// Stated as its own precondition: a dropped event would leave fewer spans
	// recorded than were opened, and the count below would stop matching for a
	// reason that has nothing to do with what this test protects.
	if dropped := run.Dropped(); dropped != 0 {
		t.Fatalf("the bus dropped %d events; the recording, not the count, is what differs", dropped)
	}
	view := latest(t, dir)
	if len(view.Spans) != model.MaxRecordsPerResult {
		t.Fatalf("the page carries %d spans, want the %d-wide bound every list in the product carries",
			len(view.Spans), model.MaxRecordsPerResult)
	}
	if view.SpansOmitted != beyond {
		t.Fatalf("the page omitted %d spans and says %d: a partial view that reports itself as whole",
			beyond, view.SpansOmitted)
	}
}

// TestARunReadsItsOwnRowsAndNotAnotherLiveRuns protects the answer a run gets
// when it reports on itself.
//
// One process holds one ledger and more than one live run: a deferred
// publication ticks while an index runs. The latest-run query sorts a live run
// first, so a run that asked for "the latest run of this repository" would be
// handed the other one and would report another run's stages as its own -- a
// completion block about work the caller never asked for. Asking by id is what
// makes the answer the run's own.
//
// Flush is what makes the answer complete: the finish and the last spans are
// in-memory state until the collector writes them, so the run reads as still
// running until the barrier has returned.
func TestARunReadsItsOwnRowsAndNotAnotherLiveRuns(t *testing.T) {
	l, dir := openLedger(t)
	defer l.Stop()
	index, indexCtx := newRun(t, l)
	deferredRun, err := l.NewRun(ledger.KindDeferred, repositoryID)
	if err != nil {
		t.Fatalf("new deferred run: %v", err)
	}
	_, publishing := ledger.Start(deferredRun.Context(context.Background()), "collection", "")
	publishing.AddOut(2)

	_, activation := ledger.Start(indexCtx, "activation", "")
	activation.AddIn(9)
	activation.End(ledger.OutcomeOK, ledger.Measured{}, nil)
	index.Report(ledger.Totals{FileCount: 4})
	index.Finish(ledger.OutcomeOK)
	// The deferred run stays live and unfinished, exactly as a tick that is
	// still publishing when the index returns.
	if err := l.Flush(context.Background()); err != nil {
		t.Fatalf("flush: %v", err)
	}

	reader, open, err := ledger.OpenReader(context.Background(), dir)
	if err != nil || !open {
		t.Fatalf("open the reader: %v, present=%v", err, open)
	}
	defer reader.Close()
	view, found, err := reader.Run(context.Background(), index.ID())
	if err != nil || !found {
		t.Fatalf("read the run by id: %v, found=%v", err, found)
	}
	if view.Run.RunID != index.ID() || view.Run.Kind != ledger.KindIndex {
		t.Fatalf("asking for run %s answered with run %s of kind %q: the run was handed another run's account",
			index.ID(), view.Run.RunID, view.Run.Kind)
	}
	if view.Run.Outcome != ledger.OutcomeOK || view.Run.FinishedAt == nil {
		t.Errorf("the run reads %q with finished_at %v after the flush its finish crossed: a run that has ended reads as live",
			view.Run.Outcome, view.Run.FinishedAt)
	}
	if len(view.Spans) != 1 || view.Spans[0].Stage != "activation" || view.Spans[0].RunID != index.ID() {
		t.Fatalf("the run's spans are %+v, want its own single activation: the page carries another run's stages or not its own",
			view.Spans)
	}
	// The hazard is real and not hypothetical: the other run is what the
	// latest-run query answers with while it is live.
	latestView, found, err := reader.LatestRun(context.Background(), repositoryID, 0)
	if err != nil || !found {
		t.Fatalf("latest run: %v, found=%v", err, found)
	}
	if latestView.Run.RunID != deferredRun.ID() {
		t.Fatalf("the latest run is %s, want the live deferred run %s; the fixture no longer poses the hazard the read by id exists for",
			latestView.Run.RunID, deferredRun.ID())
	}
	publishing.End(ledger.OutcomeOK, ledger.Measured{}, nil)
	deferredRun.Finish(ledger.OutcomeOK)
}

// TestDetachAndAttachAgainRecordsBothHolds protects the invariant the handle
// exists for: the writer on ledger.db lives exactly as long as the workspace
// hold, and a handle that outlives one hold still records the next.
//
// The failure modes, all silent. A detach that does not finalize leaves the
// first hold's spans reading 'running' for ever, so a reader is shown work
// that is still going in a process that has moved on. A handle that cannot
// attach again records nothing after its first hold, which for a server is
// every refresh but the first. And a run cached across the detach -- the
// per-process overlay a language server's start hangs under -- must record
// NOTHING further rather than write into an attachment nobody drains.
func TestDetachAndAttachAgainRecordsBothHolds(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	l := ledger.New(dir)

	// A handle no hold has attached records nothing at all: this is what the
	// language-server manager's overlay run gets in a process that is only
	// answering questions.
	detachedRun, err := l.NewRun(ledger.KindOverlay, repositoryID)
	if err != nil || detachedRun != nil {
		t.Fatalf("a detached handle opened run %v (err %v): it would be a writer with no workspace lock", detachedRun, err)
	}
	if _, err := os.Stat(ledger.Path(dir)); !os.IsNotExist(err) {
		t.Fatalf("composing the handle created %s: the file is opened by a hold, not by a composition", ledger.Path(dir))
	}

	if err := l.Attach(ctx); err != nil {
		t.Fatalf("attach: %v", err)
	}
	first, firstCtx := newRun(t, l)
	_, stage := ledger.Start(firstCtx, "capture", "")
	stage.AddIn(3)
	// Deliberately never ended: it is what a hold that detached mid-stage
	// leaves, and finalizing is what must close it.
	overlay, err := l.NewRun(ledger.KindOverlay, repositoryID)
	if err != nil || overlay == nil {
		t.Fatalf("overlay run: %v", err)
	}
	if err := l.Detach(); err != nil {
		t.Fatalf("detach: %v", err)
	}

	firstID, overlayID := first.ID(), overlay.ID()
	view := runByID(t, dir, firstID)
	if view.Run.Outcome != ledger.OutcomeInterrupted || len(view.Spans) != 1 {
		t.Fatalf("after the detach the first hold's run reads %q with %d spans, want interrupted with its one stage",
			view.Run.Outcome, len(view.Spans))
	}
	if view.Spans[0].Outcome == ledger.OutcomeRunning {
		t.Fatalf("the stage %q is still 'running' after the detach: a reader is shown work no process is doing",
			view.Spans[0].Stage)
	}

	// The second hold. The run cached across the detach is the hazard: it must
	// write nothing into either attachment.
	if err := l.Attach(ctx); err != nil {
		t.Fatalf("attach again: %v", err)
	}
	_, cached := ledger.Start(overlay.Context(ctx), "server_start", "")
	cached.End(ledger.OutcomeOK, ledger.Measured{}, nil)
	overlay.Finish(ledger.OutcomeOK)
	if err := l.DiscardRun(ctx, overlay); err != nil {
		t.Fatalf("discarding a run of the previous hold: %v", err)
	}
	second, secondCtx := newRun(t, l)
	_, activation := ledger.Start(secondCtx, "activation", "")
	activation.End(ledger.OutcomeOK, ledger.Measured{}, nil)
	second.Finish(ledger.OutcomeOK)
	if err := l.Flush(ctx); err != nil {
		t.Fatalf("flush: %v", err)
	}
	if err := l.Stop(); err != nil {
		t.Fatalf("stop: %v", err)
	}

	if view := runByID(t, dir, second.ID()); view.Run.Outcome != ledger.OutcomeOK || len(view.Spans) != 1 {
		t.Fatalf("the second hold's run reads %q with %d spans, want ok with its one stage: "+
			"the handle stopped recording after its first hold", view.Run.Outcome, len(view.Spans))
	}
	// Still exactly what the first detach wrote: the second attachment did not
	// reopen the first hold's rows.
	if view := runByID(t, dir, firstID); len(view.Spans) != 1 || view.Run.Outcome != ledger.OutcomeInterrupted {
		t.Fatalf("the first hold's run now reads %q with %d spans: a later attachment rewrote a closed hold's account",
			view.Run.Outcome, len(view.Spans))
	}
	// The run cached across the detach keeps the account its own attachment
	// closed: interrupted, with no span from the second hold and no finish of
	// its own. The row itself stays -- an overlay run whose writer is gone is
	// what the collection pass sweeps -- but nothing of the second hold is in
	// it.
	cachedView := runByID(t, dir, overlayID)
	if cachedView.Run.Outcome != ledger.OutcomeInterrupted || len(cachedView.Spans) != 0 {
		t.Fatalf("the run cached across the detach reads %q with %d spans, want interrupted with none: "+
			"it kept recording into an attachment that had already finalized it",
			cachedView.Run.Outcome, len(cachedView.Spans))
	}
}

func runByID(t *testing.T, dir, id string) ledger.RunView {
	t.Helper()
	reader, open, err := ledger.OpenReader(context.Background(), dir)
	if err != nil || !open {
		t.Fatalf("open the reader: %v, present=%v", err, open)
	}
	defer reader.Close()
	view, found, err := reader.Run(context.Background(), id)
	if err != nil || !found {
		t.Fatalf("read run %s: %v, found=%v", id, err, found)
	}
	return view
}
