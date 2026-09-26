package lsp

import (
	"context"
	"io"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Sawmonabo/codectx/internal/admission"
	"github.com/Sawmonabo/codectx/internal/index/plan"
	"github.com/Sawmonabo/codectx/internal/model"
	"github.com/Sawmonabo/codectx/internal/process"
	"github.com/Sawmonabo/codectx/internal/provider/dependence"
)

// TestSymbolFieldsAreBoundedAndTheCutIsRecorded protects the R5 contract at
// this boundary. The storage ceilings no longer reject an over-long value, so
// a producer that does not truncate serves the server's whole string into a
// model record -- a 3000-byte detail becomes a 3000-byte Signature. Every
// Symbol in this package is built by newSymbol, so bounding there covers the
// document-symbol, workspace-symbol and call-hierarchy paths at once.
func TestSymbolFieldsAreBoundedAndTheCutIsRecorded(t *testing.T) {
	name := strings.Repeat("n", model.MaxNameBytes+7)
	detail := strings.Repeat("d", 3000)
	sym := newSymbol(name, detail, name, model.NodeFunction, Location{Path: "main.go"})

	if len(sym.Name) != model.MaxNameBytes || len(sym.Container) != model.MaxNameBytes {
		t.Fatalf("name %d bytes, container %d bytes, want both bounded to %d", len(sym.Name), len(sym.Container), model.MaxNameBytes)
	}
	if len(sym.Detail) > model.MaxSignatureBytes {
		t.Fatalf("detail is %d bytes; a signature over %d reaches the record whole", len(sym.Detail), model.MaxSignatureBytes)
	}
	if got := sym.TruncatedFields["name"]; got != len(name) {
		t.Fatalf("truncated_fields[name] = %d, want the original length %d", got, len(name))
	}
	if got := sym.TruncatedFields["container"]; got != len(name) {
		t.Fatalf("truncated_fields[container] = %d, want the original length %d", got, len(name))
	}
	if _, cut := sym.TruncatedFields["signature"]; cut {
		t.Fatalf("a %d-byte detail under the %d-byte ceiling was reported as cut", len(detail), model.MaxSignatureBytes)
	}

	long := strings.Repeat("s", model.MaxSignatureBytes+1)
	if sym = newSymbol("f", long, "", model.NodeFunction, Location{}); sym.TruncatedFields["signature"] != len(long) {
		t.Fatalf("truncated_fields[signature] = %d, want the original length %d", sym.TruncatedFields["signature"], len(long))
	}
}

// TestOneAllocationAdmitsEngineUnitsAndServersTogether protects ADR-0010
// decision 5: engine runs, external indexers and language servers are admitted
// while the SUM of what they reserve fits the one machine-derived allocation.
//
// Failure mode, and the reason this test exists: when the language-server
// manager keeps a running total of its own, that total and the heavy-unit
// scheduler's are both bounded by the same allocation and nothing sums them.
// One process then reserves twice the memory the machine was measured to have
// -- engine units filling the allocation and a server admitted on top of them
// -- and the host it is running on freezes.
//
// The allocation here is 8 GiB and the two reservations are 6 GiB and 4 GiB,
// so every assertion below is about the sum and never about a count: 6 + 4
// does not fit and 6 alone or 4 alone does.
//
// Mutation: give the manager its own total again -- add `used int64` to
// Manager and admit in slot on `m.used == 0 || m.used+bytes <= allocation`
// instead of reserving from the shared ledger -> the server is admitted while
// the engine unit holds 6 GiB of the 8 GiB allocation.
func TestOneAllocationAdmitsEngineUnitsAndServersTogether(t *testing.T) {
	const giB int64 = 1 << 30
	led, err := admission.NewLedger(8*giB, 64<<30)
	if err != nil {
		t.Fatalf("the admission ledger was refused: %v", err)
	}
	runner, err := process.NewRunner(process.Limits{MaxConcurrent: 4, MemoryBudgetBytes: 64 * giB})
	if err != nil {
		t.Fatal(err)
	}
	mgr, err := New(Options{Runner: runner, DataDir: t.TempDir(), Admission: led})
	if err != nil {
		t.Fatal(err)
	}
	// The manager and the heavy-unit scheduler over the SAME ledger, which is
	// what the composition root builds.
	sched := plan.NewScheduler(led)

	unit := dependence.Reservation{HeapCapBytes: 6 * giB}
	if unit.Bytes() != 6*giB {
		t.Fatalf("the unit reserves %d bytes, want %d; this test's arithmetic no longer holds", unit.Bytes(), 6*giB)
	}
	releaseUnit, err := sched.Admit(context.Background(), unit)
	if err != nil {
		t.Fatalf("an idle allocation refused one 6 GiB unit: %v", err)
	}

	// 6 + 4 over 8: the server waits for the unit rather than being admitted
	// beside it. A waiter is never refused, only ordered, so a deadline is how
	// a test observes the wait.
	key := serverKey{snapshot: model.SnapshotID("s"), profile: "p", root: "proj"}
	waiting, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
	defer cancel()
	if _, _, err := mgr.slot(waiting, key, admission.Reservation{MemoryBytes: 4 * giB}); err == nil {
		t.Fatal("a 4 GiB server was admitted while a 6 GiB unit held the 8 GiB allocation; the process is reserving more memory than the machine has")
	}

	// The unit gives its room back and the same server is admitted at once.
	releaseUnit()
	e, starter, err := mgr.slot(context.Background(), key, admission.Reservation{MemoryBytes: 4 * giB})
	if err != nil {
		t.Fatalf("the waiting server was not admitted once the unit released: %v", err)
	}
	if !starter {
		t.Fatal("the first open of a server was not made its starter")
	}
	close(e.ready)

	// The sum holds in the other direction too: 4 + 2 fits and is admitted,
	// 4 + 6 does not and waits. Without one ledger the unit would see an empty
	// total and be admitted on top of the server.
	fits, err := sched.Admit(context.Background(), dependence.Reservation{HeapCapBytes: 2 * giB})
	if err != nil {
		t.Fatalf("a 2 GiB unit was refused beside a 4 GiB server in an 8 GiB allocation: %v", err)
	}
	fits()
	blocked, cancelBlocked := context.WithTimeout(context.Background(), 250*time.Millisecond)
	defer cancelBlocked()
	if _, err := sched.Admit(blocked, unit); err == nil {
		t.Fatal("a 6 GiB unit was admitted while a 4 GiB server held room in the 8 GiB allocation")
	}
	if err := mgr.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if allocation, reserved := led.Snapshot(); allocation != 8*giB || reserved != 0 {
		t.Fatalf("the ledger holds %d of %d bytes after everything released; a leaked reservation shrinks the allocation for the life of the process", reserved, allocation)
	}
}

// fakeCPU is a hand-driven stand-in for the runner's processor-time handle, so
// a test can say "the child is computing" and "the child is idle" without a
// child. Unsampled is the platform that cannot observe a running tree at all.
type fakeCPU struct {
	ticks     atomic.Int64
	unsampled bool
}

func (f *fakeCPU) Ticks() (int64, bool) {
	if f.unsampled {
		return 0, false
	}
	return f.ticks.Load(), true
}

// TestAHangDetectorWatchesTheServersWorkAndNotItsChatter protects the one
// invariant the request hang detector exists for: it must never end work that
// is progressing, and it must still end work that is not.
//
// Two failure modes, both of which a wire-frame-only signal has.
//
// A server computing an answer on a large project emits nothing for the whole
// window -- no partial result, no progress notification -- and is cancelled
// mid-work. That is a deadline on progressing work wearing a hang detector's
// name, and it is why the processor-time signal has to be read.
//
// A genuinely wedged request is kept alive indefinitely by a chatty sibling on
// the same connection: the byte count is per-connection while the hang is
// per-request, so another request's answers, a diagnostics stream or a log
// line speak for the wedged one and it is never detected at all.
//
// Mutation: make progress return `c.moved.Load()` unconditionally -- the
// per-connection wire count the detector used to watch -> the silent computing
// server below is declared stalled, and the wedged request under sibling
// chatter below is not.
func TestAHangDetectorWatchesTheServersWorkAndNotItsChatter(t *testing.T) {
	const window = 200 * time.Millisecond

	t.Run("a silent but computing server is not cancelled", func(t *testing.T) {
		cpu := &fakeCPU{}
		c := newConn(nopStream{}, 1<<20, 0, 1, cpu, nil)
		ctx, stalled, stop := c.watchProgress(context.Background(), window)
		defer stop()
		// Not one byte moves on the wire for several windows; only the child's
		// processor time advances, which is exactly a server indexing.
		deadline := time.After(3 * window)
		tick := time.NewTicker(window / 16)
		defer tick.Stop()
		for {
			select {
			case <-tick.C:
				cpu.ticks.Add(1)
			case <-ctx.Done():
				t.Fatalf("a server consuming processor time was cancelled after %s of silence; stalled=%v", window, stalled())
			case <-deadline:
				return
			}
		}
	})

	t.Run("a wedged request is not saved by a sibling's chatter", func(t *testing.T) {
		cpu := &fakeCPU{}
		c := newConn(nopStream{}, 1<<20, 0, 1, cpu, nil)
		ctx, stalled, stop := c.watchProgress(context.Background(), window)
		defer stop()
		// The connection is busy -- another request's frames keep arriving --
		// while the child does no work at all. The watched request is hung.
		go func() {
			tick := time.NewTicker(window / 16)
			defer tick.Stop()
			for {
				select {
				case <-tick.C:
					c.moved.Add(4096)
				case <-ctx.Done():
					return
				}
			}
		}()
		select {
		case <-ctx.Done():
			if !stalled() {
				t.Fatal("the request ended without the detector claiming it; a caller would report this as its own cancellation")
			}
		case <-time.After(5 * window):
			t.Fatalf("a request whose server consumed no processor time for %s was never declared hung while a sibling moved bytes", window)
		}
	})

	t.Run("without processor-time sampling the wire count still speaks", func(t *testing.T) {
		// On a platform that cannot observe a running tree there is no other
		// signal, and killing a live session would be worse than masking a
		// wedged request. The bytes must be watched there and only there.
		c := newConn(nopStream{}, 1<<20, 0, 1, &fakeCPU{unsampled: true}, nil)
		ctx, _, stop := c.watchProgress(context.Background(), window)
		defer stop()
		deadline := time.After(3 * window)
		tick := time.NewTicker(window / 16)
		defer tick.Stop()
		for {
			select {
			case <-tick.C:
				c.moved.Add(4096)
			case <-ctx.Done():
				t.Fatal("a connection moving bytes was declared hung on a platform with no processor-time signal")
			case <-deadline:
				return
			}
		}
	})
}

// nopStream is a stream that never speaks and accepts every write: the
// connection under test is driven by its counters, not by a peer.
type nopStream struct{}

func (nopStream) Read([]byte) (int, error)    { return 0, io.EOF }
func (nopStream) Write(p []byte) (int, error) { return len(p), nil }
