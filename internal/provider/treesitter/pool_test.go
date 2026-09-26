package treesitter

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/Sawmonabo/codectx/internal/admission"
	"github.com/Sawmonabo/codectx/internal/process"
	"github.com/Sawmonabo/codectx/internal/provider/treesitter/wire"
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

// TestAWorkerHandedBackReachesTheAcquirerQueuedOnTheLedger runs real parser
// workers on a ledger whose allocation holds exactly one of them, with three
// acquirers: the first holds the only room, the second queues on the ledger
// for a worker of its own, and the third arrives once the first has handed
// its worker back.
//
// Failure mode: the parser pool deadlocks on the ledger. A worker handed back
// while an acquirer is queued goes idle, holding the room that acquirer waits
// for; nothing pumps the ledger, a later caller takes the idle worker around
// the queue, and the queued acquirer -- its unit, and the stage it keeps from
// draining -- never returns. Every wait below is on a result, never on a
// clock: the deadlock is caught by the test binary's own timeout.
//
// Mutation: drop `|| p.reserving > 0` from release, returning the worker to
// idle while an acquirer is queued -> the second acquirer is never admitted
// and the test hangs; restore that and pump the ledger through a make-room
// step that stops idle workers -> in the interleavings where it does not
// hang, the third acquirer takes the idle worker ahead of the second.
func TestAWorkerHandedBackReachesTheAcquirerQueuedOnTheLedger(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	if exe, err = filepath.EvalSymlinks(exe); err != nil {
		t.Fatal(err)
	}
	runner, err := process.NewRunner(process.Limits{MaxConcurrent: 4, MemoryBudgetBytes: 4 << 30, DiskBudgetBytes: 1 << 30})
	if err != nil {
		t.Fatal(err)
	}
	const memory = int64(64) << 20
	room, err := admission.NewLedger(memory, 0)
	if err != nil {
		t.Fatal(err)
	}
	p := newPool(runner, room, WorkerCommand{Path: exe, Args: []string{wire.Subcommand}}, t.TempDir(), 2, memory)
	defer p.close()
	ctx := context.Background()

	first, err := p.acquire(ctx)
	if err != nil {
		t.Fatalf("the first acquirer was not admitted on an idle ledger: %v", err)
	}
	type handout struct {
		w   *worker
		err error
	}
	second := make(chan handout, 1)
	go func() {
		w, err := p.acquire(ctx)
		second <- handout{w, err}
	}()
	// The second acquirer is under the pool's bound and finds nothing idle, so
	// it reserves; the first worker is handed back only once it is doing so.
	for {
		p.mu.Lock()
		queued := p.reserving == 1
		p.mu.Unlock()
		if queued {
			break
		}
		runtime.Gosched()
	}
	p.release(first, true)
	third := make(chan handout, 1)
	go func() {
		w, err := p.acquire(ctx)
		third <- handout{w, err}
	}()

	got := <-second
	if got.err != nil {
		t.Fatalf("the acquirer queued on the ledger was refused: %v", got.err)
	}
	// The second worker holds the ledger's only room and nothing is idle, so
	// the third acquirer cannot have been handed anything yet.
	select {
	case late := <-third:
		t.Fatalf("the third acquirer was handed a worker (error: %v) while the second held the only room", late.err)
	default:
	}
	p.release(got.w, true)
	late := <-third
	if late.err != nil {
		t.Fatalf("the third acquirer was refused: %v", late.err)
	}
	p.release(late.w, true)
}
