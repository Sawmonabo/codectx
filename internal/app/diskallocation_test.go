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
//   - the stand-in returned as observed: the resource block would publish a
//     figure nobody measured as the host's free space.
func TestFreeDiskAllocationRecordsWhatWasMeasured(t *testing.T) {
	dir := t.TempDir()

	full, observed := freeDiskAllocation(dir, 0)
	if !observed || full < 0 {
		t.Fatalf("a readable directory yielded %d, observed %v; want a measured figure", full, observed)
	}
	// Below 1 GiB free the floor leaves nothing, which is zero and not negative.
	if withFloor, _ := freeDiskAllocation(dir, 1<<30); withFloor > max(full-(1<<30), 0) {
		t.Fatalf("the floor was not kept back: %d free of %d with a 1 GiB floor", withFloor, full)
	}
	// A floor larger than the device is a real reading of a host with nothing
	// to give, which is zero and is not the stand-in.
	if atFloor, observed := freeDiskAllocation(dir, full+(1<<40)); atFloor != 0 || !observed {
		t.Fatalf("a host below its floor yielded %d, observed %v; want 0, observed", atFloor, observed)
	}
	// No figure at all: the stand-in, which is neither zero nor unlimited. A
	// missing leaf under a directory that exists is what the platform call
	// actually fails on, which is the unmeasurable reading this asserts.
	unmeasured, observed := freeDiskAllocation(filepath.Join(dir, "no-such-directory"), 0)
	if unmeasured != unobservedFreeDiskBytes || observed {
		t.Fatalf("an unmeasurable directory yielded %d, observed %v; want the stand-in %d, unobserved",
			unmeasured, observed, unobservedFreeDiskBytes)
	}
}

// resources.max_temp_bytes is one total over every consumer of temporary disk
// it bounds, so an operator ceiling of X admits at most X across the shared
// runner and the query spools together, and the unlimited default stays
// unlimited on both. Mutations this fails on:
//   - the runner handed the whole ceiling beside the spools' share: X admits
//     X plus an eighth of X;
//   - a share rounding to zero on a small ceiling: that consumer reads zero as
//     unlimited and the ceiling no longer bounds it;
//   - the default split into anything but two unlimited shares.
func TestTemporaryCeilingHoldsAcrossEveryConsumer(t *testing.T) {
	if runner, spools := tempDiskShares(0); runner != 0 || spools != 0 {
		t.Fatalf("the unlimited default split into runner %d and spools %d; want both unlimited", runner, spools)
	}
	for _, total := range []int64{2, 7, spoolBudgetDivisor, 1<<30 + 3} {
		runner, spools := tempDiskShares(total)
		if runner <= 0 || spools <= 0 {
			t.Fatalf("a ceiling of %d gave runner %d and spools %d; a zero share is unlimited", total, runner, spools)
		}
		if runner+spools != total {
			t.Fatalf("a ceiling of %d admits %d (runner %d + spools %d)", total, runner+spools, runner, spools)
		}
	}
}
