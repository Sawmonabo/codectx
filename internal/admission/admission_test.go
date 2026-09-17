package admission

import (
	"context"
	"testing"
	"time"
)

// Disk gates admission, and it gates it on the same queue memory does.
// Mutations this fails on:
//   - pump checking only the memory sum: the second child is admitted beside
//     the first although their staged bytes together exceed the free space the
//     host has, which is a run filling the device;
//   - a zero disk reservation blocking on a full disk dimension: work that
//     stages nothing would queue behind work that does, for no resource;
//   - release not returning the disk bytes: the ledger would leak its disk
//     allocation until nothing with staged bytes was ever admitted again.
func TestDiskIsAdmittedOnTheSameQueueAsMemory(t *testing.T) {
	l, err := NewLedger(8<<30, 10<<30)
	if err != nil {
		t.Fatalf("the ledger was refused: %v", err)
	}
	ctx := context.Background()
	first, err := l.ReserveWith(ctx, Reservation{MemoryBytes: 1 << 30, DiskBytes: 6 << 30}, nil)
	if err != nil {
		t.Fatalf("the first child was not admitted: %v", err)
	}

	// Fits the memory allocation with room to spare and does not fit the disk
	// one, so it must wait.
	admitted := make(chan error, 1)
	go func() {
		release, rerr := l.ReserveWith(ctx, Reservation{MemoryBytes: 1 << 30, DiskBytes: 6 << 30}, nil)
		admitted <- rerr
		if rerr == nil {
			release()
		}
	}()
	select {
	case rerr := <-admitted:
		t.Fatalf("a child whose staged bytes do not fit the free space did not wait for them (error: %v)", rerr)
	case <-time.After(50 * time.Millisecond):
	}

	first()
	select {
	case rerr := <-admitted:
		if rerr != nil {
			t.Fatalf("the waiting child was refused rather than admitted: %v", rerr)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the waiting child was not admitted after the disk it waited for was released")
	}
}

// A host with nothing to spare serializes staged work; it never refuses it and
// never treats the emptiness as room. Mutation: make a zero disk allocation
// mean "unlimited" -- the reading this replaces, where max_temp_bytes at 0 let
// every reservation through -- and the second child is admitted beside the
// first on a device with no free space at all.
func TestAZeroDiskAllocationSerializesRatherThanAdmits(t *testing.T) {
	l, err := NewLedger(8<<30, 0)
	if err != nil {
		t.Fatalf("the ledger was refused: %v", err)
	}
	ctx := context.Background()
	first, err := l.ReserveWith(ctx, Reservation{MemoryBytes: 1 << 30, DiskBytes: 1 << 20}, nil)
	if err != nil {
		t.Fatalf("the first child must still run alone rather than be refused: %v", err)
	}
	ctx2, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
	defer cancel()
	if release, rerr := l.ReserveWith(ctx2, Reservation{MemoryBytes: 1 << 30, DiskBytes: 1 << 20}, nil); rerr == nil {
		release()
		t.Fatal("a second staging child was admitted on a host whose free space is already at its floor")
	}
	first()
}
