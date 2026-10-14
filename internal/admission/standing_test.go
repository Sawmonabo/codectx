package admission

import (
	"context"
	"testing"
)

// standingLedger is a ledger of 100 bytes of memory and 1,000 of disk holding
// one standing holding of 30 bytes, the search index writer's place.
func standingLedger(t *testing.T) (*Ledger, *Holding) {
	t.Helper()
	l, err := NewLedger(100, 1000)
	if err != nil {
		t.Fatal(err)
	}
	writer, err := l.Hold(context.Background(), Reservation{MemoryBytes: 30, Standing: true}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(writer.Release)
	return l, writer
}

// coldKey is a tool class no run has measured.
var coldKey = ChildKey{Kind: ChildIndexer, Tool: "cold", Fingerprint: "f"}

// A tool run nothing was measured for is planned within what a standing
// holding leaves of the allocation, so it is admitted beside the holding with
// the sum exactly at the allocation. The standing holding is released only
// after the run's own work, so a plan at the whole allocation waits for a
// release that never comes: indexing stalls with every profile unit parked in
// Hold. The run's makeRoom step cancels its own wait, so a run that would wait
// fails at once instead of hanging. Mutations this fails on:
//   - planning against the whole allocation (l.allocation in room) while the
//     ledger still grants it alone: the run reserves 100 bytes, not 70;
//   - both that and counting standing holdings in the runs-alone rule
//     (l.admitted > 0 in pump): the run waits, and its wait is canceled.
func TestAColdToolIsPlannedWithinWhatStandingHoldingsLeave(t *testing.T) {
	l, _ := standingLedger(t)
	var needs ChildNeeds
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h, err := l.HoldChild(ctx, &needs, coldKey, ChildWork{WorkBytes: 10, Projects: 1}, cancel)
	if err != nil {
		t.Fatalf("a first run waited beside a standing holding that is released only after it: %v", err)
	}
	defer h.Release()
	if got := h.Bytes(); got != 70 {
		t.Fatalf("a first run beside a 30-byte standing holding reserved %d bytes of a 100-byte allocation, want 70", got)
	}
	if allocation, reserved := l.Snapshot(); reserved != allocation {
		t.Fatalf("reserved %d against an allocation of %d, want the sum exactly at the allocation", reserved, allocation)
	}
}

// A reservation that cannot fit beside a standing holding -- the allocation
// was re-derived below what the holding holds -- runs alone among everything
// else rather than never, and still waits for a holding that is not standing.
// Mutations this fails on:
//   - counting standing holdings in the runs-alone rule (l.admitted > 0 in
//     pump): the run waits forever beside the standing holding, and its wait
//     is canceled;
//   - granting a head that does not fit whatever is held (dropping the
//     admitted check): the second run is admitted beside the first.
func TestARunThatCannotFitBesideAStandingHoldingRunsAlone(t *testing.T) {
	l, _ := standingLedger(t)
	l.SetAllocation(20)
	var needs ChildNeeds
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	first, err := l.HoldChild(ctx, &needs, coldKey, ChildWork{WorkBytes: 10, Projects: 1}, cancel)
	if err != nil {
		t.Fatalf("a run that cannot fit beside a standing holding waited instead of running alone: %v", err)
	}

	waited, signal := waitSignal()
	second := holdAsync(l, Reservation{MemoryBytes: 1}, signal)
	select {
	case h := <-second:
		h.Release()
		t.Fatal("a reservation that does not fit ran beside a run that is not standing")
	case <-waited:
	}
	first.Release()
	(<-second).Release()
}

// A standing holding is taken at once, beside a run that holds the whole
// room: the search index's writer opens on the first write of a unit that is
// itself admitted, and that unit releases only after the write returns. The
// writer's makeRoom step cancels its own wait, so a writer that would wait
// fails at once instead of hanging. Mutation this fails on: queueing a
// standing holding like any other reservation (dropping the at-once grant in
// Hold): it waits behind the unit that asked for it.
func TestAStandingHoldingIsTakenBesideTheWorkThatAsksForIt(t *testing.T) {
	l, err := NewLedger(100, 1000)
	if err != nil {
		t.Fatal(err)
	}
	var needs ChildNeeds
	unit, err := l.HoldChild(context.Background(), &needs, coldKey, ChildWork{WorkBytes: 10, Projects: 1}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer unit.Release()
	if got := unit.Bytes(); got != 100 {
		t.Fatalf("a first run with nothing standing reserved %d bytes, want the whole allocation", got)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	writer, err := l.Hold(ctx, Reservation{MemoryBytes: 30, Standing: true}, cancel)
	if err != nil {
		t.Fatalf("a standing holding waited behind the admitted work that asked for it: %v", err)
	}
	writer.Release()
}
