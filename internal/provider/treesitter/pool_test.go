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

// newOneWorkerPool is a pool of real parser workers over a ledger whose
// allocation holds exactly one of them, so the second worker anyone asks for
// queues on the ledger.
func newOneWorkerPool(t *testing.T) (*pool, *admission.Ledger, int64) {
	t.Helper()
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
	t.Cleanup(p.close)
	return p, room, memory
}

// awaitHead returns once an acquirer of p has been told by the ledger that it
// is the queue's head and does not fit -- the ledger's own queue state, not
// the pool's count of callers about to reserve.
func awaitHead(p *pool) {
	for {
		p.mu.Lock()
		head := false
		for _, a := range p.queued {
			head = head || a.head && a.handed == nil
		}
		p.mu.Unlock()
		if head {
			return
		}
		runtime.Gosched()
	}
}

// awaitQueued returns once n acquirers of p are queued for a worker, so a
// worker handed back after it sees them rather than going idle.
func awaitQueued(p *pool, n int) {
	for {
		p.mu.Lock()
		queued := len(p.queued) == n
		p.mu.Unlock()
		if queued {
			return
		}
		runtime.Gosched()
	}
}

type handout struct {
	w   *worker
	err error
}

func acquireAsync(p *pool) <-chan handout {
	ch := make(chan handout, 1)
	go func() {
		w, err := p.acquire(context.Background())
		ch <- handout{w, err}
	}()
	return ch
}

// TestAWorkerHandedBackGoesToTheLedgerHead runs real parser workers on a
// ledger whose allocation holds exactly one of them.
//
// "pool head": the first acquirer holds the only room and the second is the
// ledger's head. The first worker handed back must reach the second as it is,
// reservation and process together, and the third acquirer, queued once the
// second holds that room, must wait until it is handed back again.
//
// "foreign head": another reserver is the ledger's head and a pool acquirer is
// queued behind it. The worker handed back must be stopped so its room reaches
// that head first; the pool acquirer is admitted only after the head gives the
// room back, with a worker of its own.
//
// Failure modes: an allocation that fits fewer workers than acquirers makes
// every parse an exec, a hello, a parse and an exit when a worker handed back
// is always stopped; a worker kept idle while an acquirer is queued holds the
// room it waits for, nothing pumps the ledger and the acquirer never returns;
// a worker handed to a pool acquirer queued behind another head takes room
// around that head and breaks the ledger's first-in-first-out order. Every
// wait below is on a result or on the ledger's own head signal, never on a
// clock: a deadlock is caught by the test binary's own timeout.
//
// Mutations: stop the worker in release instead of handing it to the head ->
// "pool head" sees a second worker started. Idle it while an acquirer is
// queued -> "pool head" hangs. Hand it to any queued acquirer, head or not ->
// "foreign head" sees the pool acquirer return before the foreign reserver is
// granted.
func TestAWorkerHandedBackGoesToTheLedgerHead(t *testing.T) {
	t.Run("pool head", func(t *testing.T) {
		p, _, _ := newOneWorkerPool(t)
		first, err := p.acquire(context.Background())
		if err != nil {
			t.Fatalf("the first acquirer was not admitted on an idle ledger: %v", err)
		}
		second := acquireAsync(p)
		awaitHead(p)
		p.release(first, true)
		got := <-second
		if got.err != nil {
			t.Fatalf("the acquirer at the ledger head was refused: %v", got.err)
		}
		if got.w != first || p.stats().WorkersStarted != 1 {
			t.Fatalf("the head was given a new worker (%d started) instead of the one handed back", p.stats().WorkersStarted)
		}
		third := acquireAsync(p)
		awaitHead(p)
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
		if late.w != first || p.stats().WorkersStarted != 1 {
			t.Fatalf("the third acquirer was given a new worker (%d started) instead of the one handed back", p.stats().WorkersStarted)
		}
		p.release(late.w, true)
	})

	t.Run("foreign head", func(t *testing.T) {
		p, room, memory := newOneWorkerPool(t)
		first, err := p.acquire(context.Background())
		if err != nil {
			t.Fatalf("the first acquirer was not admitted on an idle ledger: %v", err)
		}
		stuck := make(chan struct{}, 1)
		foreign := make(chan func(), 1)
		go func() {
			release, err := room.ReserveWith(context.Background(), admission.Reservation{MemoryBytes: memory}, func() {
				select {
				case stuck <- struct{}{}:
				default:
				}
			})
			if err != nil {
				t.Errorf("the foreign reserver was refused: %v", err)
				release = func() {}
			}
			foreign <- release
		}()
		<-stuck // the foreign reserver is the ledger's head and does not fit
		second := acquireAsync(p)
		awaitQueued(p, 1)
		p.release(first, true)
		var release func()
		select {
		case got := <-second:
			t.Fatalf("the pool acquirer queued behind a foreign head was handed a worker (error: %v) before that head", got.err)
		case release = <-foreign:
		}
		release()
		got := <-second
		if got.err != nil {
			t.Fatalf("the pool acquirer was refused after the foreign head released: %v", got.err)
		}
		if got.w == first || p.stats().WorkersStarted != 2 {
			t.Fatalf("the pool acquirer was given the worker the foreign head's room was taken from (%d started)", p.stats().WorkersStarted)
		}
		p.release(got.w, true)
	})
}
