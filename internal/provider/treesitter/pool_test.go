package treesitter

import (
	"context"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"
)

// fakeTicks is a processor-time signal a test controls: moving advances on
// every reading, available false is a platform with no measurement.
type fakeTicks struct {
	n         atomic.Int64
	moving    bool
	available bool
}

func (f *fakeTicks) Ticks() (int64, bool) {
	if !f.available {
		return 0, false
	}
	if f.moving {
		return f.n.Add(1), true
	}
	return f.n.Load(), true
}

// TestHangDetectorEndsOnlyAWorkerThatNeitherComputesNorAnswers runs the
// parser pool's hang detector, with its real window, over ten windows of a
// virtual clock.
//
// Failure mode: a parse of a large file computes for longer than any window
// with nothing on the wire; a detector that watched only the wire, or read an
// unavailable processor-time measurement as zero ticks, would kill it and
// report CTX_PROVIDER_TIMEOUT -- the wall-clock kill, disguised -- and on a
// platform without sampling would kill every long parse.
//
// Mutation: drop the ticks comparison from watchProgress's progress check, or
// treat ok=false as a reading of 0 -> "computing in silence" or "no
// measurement" reports the worker killed. Drop the byte comparison ->
// "answering" is killed. Never fire -> "neither" is not killed.
func TestHangDetectorEndsOnlyAWorkerThatNeitherComputesNorAnswers(t *testing.T) {
	for _, c := range []struct {
		name                         string
		computing, measured, answers bool
		wantKilled                   bool
	}{
		{name: "computing in silence", computing: true, measured: true},
		{name: "answering without processor time", measured: true, answers: true},
		{name: "no measurement", computing: false, measured: false},
		{name: "neither computing nor answering", measured: true, wantKilled: true},
	} {
		t.Run(c.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				var received atomic.Int64
				var killed atomic.Bool
				exited := make(chan struct{})
				stalled, stop := watchProgress(context.Background(), exited,
					&fakeTicks{moving: c.computing, available: c.measured}, &received, hangWindow,
					func() { killed.Store(true) })
				for range 40 {
					time.Sleep(hangWindow / 4)
					if c.answers {
						received.Add(1)
					}
				}
				synctest.Wait()
				stop()
				if killed.Load() != c.wantKilled || stalled() != c.wantKilled {
					t.Fatalf("killed %v, stalled %v; want both %v", killed.Load(), stalled(), c.wantKilled)
				}
			})
		})
	}
}
