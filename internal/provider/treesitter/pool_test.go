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
	"github.com/Sawmonabo/codectx/internal/ledger"
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

// newTestPool is a pool of at most max real parser workers over a ledger of
// the given allocation.
func newTestPool(t *testing.T, max int, allocation int64) (*pool, *admission.Ledger) {
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
	room, err := admission.NewLedger(allocation, 0)
	if err != nil {
		t.Fatal(err)
	}
	p := newPool(runner, room, WorkerCommand{Path: exe, Args: []string{wire.Subcommand}}, t.TempDir(), max, func(int64) {})
	t.Cleanup(p.close)
	return p, room
}

// newOneWorkerPool is a pool of real parser workers whose first worker is
// started and whose ledger's allocation is then set to that worker's base
// holding -- what it reported, or the idle stand-in where it reported none --
// so it holds exactly one of them and the second worker anyone asks for --
// reserved at that same largest base -- queues on the ledger. It returns the
// first worker, busy, and its holding, which is what a foreign reserver that
// must not fit beside it reserves.
func newOneWorkerPool(t *testing.T) (*pool, *admission.Ledger, *worker, int64) {
	t.Helper()
	p, room := newTestPool(t, 2, 4<<30)
	first, err := p.acquire(context.Background(), p.request(0))
	if err != nil {
		t.Fatalf("the first acquirer was not admitted on an idle ledger: %v", err)
	}
	held := first.held.Load()
	room.SetAllocation(held)
	return p, room, first, held
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

// awaitAdmitted returns the release a reserver is granted on admitted, and
// fails the test once the pool has made no progress for a whole hang window:
// neither its idle count nor its count of exited workers has moved. A pool
// that stops its idle workers moves both within one worker grace, so only a
// pool that holds its idle room around the reserver trips the detector, and a
// slow host never does.
func awaitAdmitted(t *testing.T, p *pool, admitted <-chan func()) func() {
	t.Helper()
	progress := func() [2]uint64 {
		p.mu.Lock()
		defer p.mu.Unlock()
		return [2]uint64{uint64(len(p.idle)), p.exited}
	}
	last, moved := progress(), time.Now()
	sample := time.NewTicker(hangWindow / 600)
	defer sample.Stop()
	for {
		select {
		case release := <-admitted:
			return release
		case <-sample.C:
			if now := progress(); now != last {
				last, moved = now, time.Now()
			} else if time.Since(moved) > hangWindow {
				t.Fatalf("the reserver waited a whole hang window while the pool held %d idle worker(s) and moved nothing", now[0])
			}
		}
	}
}

type handout struct {
	w   *worker
	err error
}

func acquireAsync(p *pool) <-chan handout {
	ch := make(chan handout, 1)
	go func() {
		w, err := p.acquire(context.Background(), p.request(0))
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
// "foreign waiter behind an idle worker": the pool's only worker is idle when
// another reserver queues on the ledger. The next pool acquirer must stop that
// idle worker rather than reuse it, so the waiter is admitted first, and must
// then be given a worker of its own. When that worker is handed back while a
// second foreign reserver waits and no pool acquirer is queued, it must be
// stopped rather than idled, so the second reserver is admitted.
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
// granted. Drop the ledger's Waiting check from acquire together with the
// pool's Holder registration (either one alone stops the idle worker) ->
// "foreign waiter behind an idle worker" sees the pool acquirer handed the
// idle worker while the foreign reserver is still waiting. Drop it from release -> the worker
// handed back goes idle, the second foreign reserver is never admitted and the
// subtest hangs. Drop the pool's Holder registration from newPool -> "foreign
// reserver beside an idle, quiet pool" trips the progress detector.
//
// "foreign reserver beside an idle, quiet pool": the pool's only worker is
// idle and the pool makes no further acquire or release -- a stage whose own
// progress waits on the reserver. The reserver must still be admitted: the
// ledger's head runs the pool's idle-release step, which stops the worker.
//
// "foreign head before a caller's increment": the pool's only worker is held
// by a caller that then waits for its file's increment behind another
// reserver at the ledger's head, which waits for that worker's room. The
// caller must be asked to yield its worker, and the worker it hands back must
// be stopped, so the head is admitted. Mutation: drop yieldParse from the
// pool's idle-release step -> the caller's wait never ends and the subtest
// hangs.
func TestAWorkerHandedBackGoesToTheLedgerHead(t *testing.T) {
	t.Run("pool head", func(t *testing.T) {
		p, _, first, _ := newOneWorkerPool(t)
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
		p, room, first, memory := newOneWorkerPool(t)
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

	t.Run("foreign waiter behind an idle worker", func(t *testing.T) {
		p, room, first, memory := newOneWorkerPool(t)
		p.release(first, true) // nobody waits, so the worker goes idle
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
		<-stuck // the foreign reserver waits behind the idle worker's room
		second := acquireAsync(p)
		var release func()
		select {
		case got := <-second:
			t.Fatalf("the pool acquirer reused the idle worker (error: %v) while a foreign reserver waited", got.err)
		case release = <-foreign:
		}
		release()
		got := <-second
		if got.err != nil {
			t.Fatalf("the pool acquirer was refused after the foreign reserver released: %v", got.err)
		}
		if got.w == first || p.stats().WorkersStarted != 2 {
			t.Fatalf("the pool acquirer was given the idle worker the foreign reserver waited behind (%d started)",
				p.stats().WorkersStarted)
		}
		stuckAgain := make(chan struct{}, 1)
		again := make(chan func(), 1)
		go func() {
			release, err := room.ReserveWith(context.Background(), admission.Reservation{MemoryBytes: memory}, func() {
				select {
				case stuckAgain <- struct{}{}:
				default:
				}
			})
			if err != nil {
				t.Errorf("the second foreign reserver was refused: %v", err)
				release = func() {}
			}
			again <- release
		}()
		<-stuckAgain // the second foreign reserver waits behind the busy worker's room
		p.release(got.w, true)
		(<-again)()
		p.mu.Lock()
		n := len(p.idle)
		p.mu.Unlock()
		if n != 0 {
			t.Fatalf("%d worker(s) went idle while a foreign reserver waited", n)
		}
	})

	t.Run("foreign reserver beside an idle, quiet pool", func(t *testing.T) {
		p, room, w, memory := newOneWorkerPool(t)
		p.release(w, true) // nobody waits, so the worker goes idle
		foreign := make(chan func(), 1)
		go func() {
			release, err := room.ReserveWith(context.Background(), admission.Reservation{MemoryBytes: memory}, nil)
			if err != nil {
				t.Errorf("the foreign reserver was refused: %v", err)
				release = func() {}
			}
			foreign <- release
		}()
		awaitAdmitted(t, p, foreign)()
		p.mu.Lock()
		idle := len(p.idle)
		p.mu.Unlock()
		if idle != 0 {
			t.Fatalf("the foreign reserver was admitted while the pool still held %d idle worker(s)", idle)
		}
	})

	t.Run("foreign head before a caller's increment", func(t *testing.T) {
		p, room, w, memory := newOneWorkerPool(t)
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
		<-stuck // the foreign reserver is the ledger's head and waits for w's room
		ph, yielded, err := p.reserveParse(context.Background(), increment{bytes: memory})
		if !yielded || ph != nil || err == nil {
			t.Fatalf("the caller waiting for its increment behind a foreign head was not asked to yield (granted %v, error %v)",
				ph != nil, err)
		}
		p.release(w, true)
		(<-foreign)()
		p.mu.Lock()
		idle := len(p.idle)
		p.mu.Unlock()
		if idle != 0 {
			t.Fatalf("the yielded worker went idle while the foreign head waited (%d idle)", idle)
		}
	})
}

// TestANeedTheWorkerCouldNotMeasureTeachesNothing settles a file whose Done
// carried no need, then one whose Done carried one.
//
// Failure mode: an unavailable need read as zero is folded into the model as
// a file that needed nothing; enough of them drag the prediction toward zero
// and every later file of the class is admitted short and overruns.
//
// Mutation: settle a nil need as an observation of 0 -> the model predicts
// after the first file, and no file is counted unmeasured. Count an overrun
// without a need -> the first file is disclosed as one.
func TestANeedTheWorkerCouldNotMeasureTeachesNothing(t *testing.T) {
	p, _ := newTestPool(t, 1, 4<<30)
	f := &fileNeed{language: "go", size: 4096, reserved: increment{bytes: 1 << 20}, model: &needModel{loaded: true}, halfLife: 1,
		key: needKey{NeedKey: ledger.NeedKey{Language: "go", Fingerprint: fingerprint, Build: p.build, SizeClass: admission.SizeClassOf(4096)}}}
	p.settle(f, wire.Memory{})
	if _, ok := f.model.h.Predict(4096); ok {
		t.Fatal("a file with no measured need was learned from")
	}
	if s := p.stats(); s.UnmeasuredFiles != 1 || len(s.Overruns) != 0 {
		t.Fatalf("after a file with no need: %d unmeasured, overruns %+v; want 1 and none", s.UnmeasuredFiles, s.Overruns)
	}
	need := uint64(2 << 20)
	p.settle(f, wire.Memory{NeedBytes: &need})
	if _, ok := f.model.h.Predict(4096); !ok {
		t.Fatal("a file with a measured need was not learned from")
	}
	if s := p.stats(); s.UnmeasuredFiles != 1 || len(s.Overruns) != 1 || s.Overruns[0].LargestDriftBytes != 1<<20 {
		t.Fatalf("after a measured overrun: %d unmeasured, overruns %+v; want 1 and one of 1 MiB drift", s.UnmeasuredFiles, s.Overruns)
	}
}
