package app

import (
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
