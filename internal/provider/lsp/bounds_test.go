package lsp

import (
	"context"
	"strings"
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
	led, err := admission.NewLedger(8 * giB)
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
	if _, _, err := mgr.slot(waiting, key, 4*giB); err == nil {
		t.Fatal("a 4 GiB server was admitted while a 6 GiB unit held the 8 GiB allocation; the process is reserving more memory than the machine has")
	}

	// The unit gives its room back and the same server is admitted at once.
	releaseUnit()
	e, starter, err := mgr.slot(context.Background(), key, 4*giB)
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
