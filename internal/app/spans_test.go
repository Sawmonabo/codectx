package app

import (
	"context"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/Sawmonabo/codectx/internal/ledger"
	"github.com/Sawmonabo/codectx/internal/model"
)

// TestASlowSubscriberNeverPacesTheRun guards the one rule the whole accounting
// rests on: measuring a run must never change how long it takes. A ledger
// subscriber runs ON the collector's goroutine, which is the goroutine every
// live reader's view waits on, and the surfaces below it write to terminals,
// sockets and log shippers that can stop reading at any moment. If publishing
// could wait on one of them, an operator running `codectx index | less` and
// walking away would slow the index itself -- and every figure the ledger then
// reported about that run would be a measurement of the reporting.
//
// A dropped row is the accepted cost: the rows are bounded, the drop is
// counted, and the missing lines are a missing progress line, never a wrong
// number.
//
// Mutation: make spanFanout.publish a direct call (or a blocking send) instead
// of a non-blocking send onto the bounded queue.
func TestASlowSubscriberNeverPacesTheRun(t *testing.T) {
	fanout := newSpanFanout()

	// A subscriber that has stopped reading entirely: the terminal that is not
	// being drained, the client that went away.
	release := make(chan struct{})
	var once sync.Once
	fanout.subscribe(func(model.StageRecord) { <-release })
	t.Cleanup(func() { once.Do(func() { close(release) }) })

	// Far more rows than the queue can hold, published from this goroutine as
	// the collector publishes them from its own.
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < spanQueueDepth*4; i++ {
			fanout.publish(ledger.SpanRow{Seq: int64(i), Stage: "walk", Outcome: ledger.OutcomeOK})
		}
	}()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("publishing finished stages blocked on a subscriber that stopped reading; " +
			"the run would have been paced by the accounting of it")
	}

	if dropped := fanout.dropped.Load(); dropped == 0 {
		t.Fatal("a queue that could not be drained dropped nothing, so publishing must have waited " +
			"for the subscriber rather than refusing the row")
	}
	once.Do(func() { close(release) })
	fanout.stop(nil)
}

// TestStageLogLineCarriesTransferredBytesAndOnlyWhenTaken guards the structured
// line against the two ways a measurement goes wrong in a log: silently missing,
// and silently invented.
//
// A log shipper and a proof recorder read this line and nothing else, so a
// recorded field the line omits is a fact that never leaves the process --
// transferred bytes were carried by the row, promised by this function's own
// doc comment and never emitted. And a stage that ran no child has no counters
// at all, where a zero would read as a stage that moved nothing rather than as
// a figure nobody took.
//
// Mutation: emit the two attributes unconditionally, dereferencing through a
// zero default, and the absent case fails.
func TestStageLogLineCarriesTransferredBytesAndOnlyWhenTaken(t *testing.T) {
	read, write := uint64(4096), uint64(8192)
	measured := attrsOfStageLog(t, model.StageRecord{Stage: "treesitter", ReadBytes: &read, WriteBytes: &write})
	if measured["read_bytes"] != uint64(4096) || measured["write_bytes"] != uint64(8192) {
		t.Fatalf("the line carries read_bytes=%v write_bytes=%v; the row recorded 4096 and 8192",
			measured["read_bytes"], measured["write_bytes"])
	}
	absent := attrsOfStageLog(t, model.StageRecord{Stage: "walk"})
	if _, ok := absent["read_bytes"]; ok {
		t.Fatalf("a stage nobody measured logged read_bytes=%v", absent["read_bytes"])
	}
	if _, ok := absent["write_bytes"]; ok {
		t.Fatalf("a stage nobody measured logged write_bytes=%v", absent["write_bytes"])
	}
}

// attrsOfStageLog logs one row through the real logSpan and returns the
// attributes the handler was given, so the assertions are over what a shipper
// receives and not over a rendered string.
func attrsOfStageLog(t *testing.T, row model.StageRecord) map[string]any {
	t.Helper()
	h := &captureHandler{attrs: map[string]any{}}
	logSpan(slog.New(h))(row)
	return h.attrs
}

type captureHandler struct{ attrs map[string]any }

func (h *captureHandler) Enabled(context.Context, slog.Level) bool { return true }

func (h *captureHandler) Handle(_ context.Context, rec slog.Record) error {
	rec.Attrs(func(a slog.Attr) bool {
		h.attrs[a.Key] = a.Value.Any()
		return true
	})
	return nil
}

func (h *captureHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *captureHandler) WithGroup(string) slog.Handler      { return h }
