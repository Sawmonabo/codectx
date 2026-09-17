package app

import (
	"path/filepath"
	"testing"
)

// What the children may stage is derived from the space that is actually
// free, and a host that reports none is recorded as unmeasured rather than as
// empty or as infinite. Mutations this fails on:
//   - an unreadable directory yielding zero: the product would serialize every
//     staging child on a host that may have terabytes free, and would report
//     an absent measurement as a measurement of nothing;
//   - an unreadable directory yielding an unlimited allocation: the gate would
//     admit every child at once on the one dimension whose exhaustion fails
//     every writer on the device;
//   - the floor not being subtracted: the children would be admitted to fill
//     the space the host is promised to keep.
func TestFreeDiskAllocationRecordsWhatWasMeasured(t *testing.T) {
	dir := t.TempDir()

	full := freeDiskAllocation(dir, 0)
	if full <= 0 {
		t.Fatalf("a readable directory yielded no allocation: %d", full)
	}
	if withFloor := freeDiskAllocation(dir, 1<<30); withFloor > full-(1<<30) {
		t.Fatalf("the floor was not kept back: %d free of %d with a 1 GiB floor", withFloor, full)
	}
	// A floor larger than the device is a real reading of a host with nothing
	// to give, which is zero and is not the stand-in.
	if atFloor := freeDiskAllocation(dir, full+(1<<40)); atFloor != 0 {
		t.Fatalf("a host below its floor yielded %d, want 0", atFloor)
	}
	// No figure at all: the stand-in, which is neither zero nor unlimited.
	unmeasured := freeDiskAllocation(filepath.Join(dir, "no-such-directory"), 0)
	if unmeasured != unobservedFreeDiskBytes {
		t.Fatalf("an unmeasurable directory yielded %d, want the stand-in %d", unmeasured, unobservedFreeDiskBytes)
	}
}
