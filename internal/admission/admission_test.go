package admission

import (
	"context"
	"testing"
)

// Disk gates admission, and it gates it on the same queue memory does. Every
// wait below is observed through the waiter's own makeRoom step, which the
// ledger calls exactly when the waiter is at the head and does not fit, so no
// case depends on a timer. Mutations this fails on:
//   - pump checking only the memory sum: the disk waiter is admitted beside the
//     first child although their staged bytes together exceed the free space
//     the host has, which is a run filling the device;
//   - pump checking the disk sum for a head that requests no disk: the
//     memory-only child is held behind a disk dimension another child filled,
//     for a resource it does not want;
//   - release not returning the disk bytes: the disk waiter is never admitted
//     after the first child leaves, although a memory-only child is still
//     running, so the runs-alone rule cannot mask the leak.
func TestDiskIsAdmittedOnTheSameQueueAsMemory(t *testing.T) {
	l, err := NewLedger(Allocation{Bytes: 8 << 30, Observed: true}, Allocation{Bytes: 10 << 30, Observed: true})
	if err != nil {
		t.Fatalf("the ledger was refused: %v", err)
	}
	ctx := context.Background()

	// Admitted alone on an idle ledger, above the whole disk allocation: the
	// disk dimension is now past full.
	first, err := l.ReserveWith(ctx, Reservation{MemoryBytes: 1 << 30, DiskBytes: 12 << 30}, nil)
	if err != nil {
		t.Fatalf("the first child was not admitted alone: %v", err)
	}

	// A child that stages nothing fits the memory allocation and must be
	// admitted at once. Were it held, its makeRoom step would run and cancel
	// the wait.
	memCtx, memCancel := context.WithCancel(ctx)
	defer memCancel()
	memoryOnly, err := l.ReserveWith(memCtx, Reservation{MemoryBytes: 1 << 30}, memCancel)
	if err != nil {
		t.Fatalf("a child that stages nothing was held behind a full disk dimension: %v", err)
	}
	defer memoryOnly()

	// Fits the memory allocation with room to spare and does not fit the disk
	// one, so it must wait.
	waited := make(chan struct{}, 1)
	signalWait := func() {
		select {
		case waited <- struct{}{}:
		default:
		}
	}
	admitted := make(chan error, 1)
	go func() {
		release, rerr := l.ReserveWith(ctx, Reservation{MemoryBytes: 1 << 30, DiskBytes: 6 << 30}, signalWait)
		admitted <- rerr
		if rerr == nil {
			release()
		}
	}()
	select {
	case rerr := <-admitted:
		t.Fatalf("a child whose staged bytes do not fit the free space did not wait for them (error: %v)", rerr)
	case <-waited:
	}

	// The memory-only child still runs, so the waiter is re-pumped against
	// the sums and not against an idle ledger: it fits only if the release
	// gave the disk back.
	first()
	select {
	case rerr := <-admitted:
		if rerr != nil {
			t.Fatalf("the waiting child was refused rather than admitted: %v", rerr)
		}
	case <-waited:
		t.Fatal("the waiting child was not admitted after the disk it waited for was released")
	}
}
