package admission

import (
	"math"
	"testing"
)

// TestDarwinAvailableIsFreeLessSpeculativePlusFileBacked protects the macOS
// figure every admission there is derived from. Mutations that fail it:
//   - dropping the clamp: free less speculative wraps around when the two
//     counters, read a moment apart, report more speculative pages than free
//     ones, and the host reads as having almost no memory or, through the
//     overflow check, as unobserved;
//   - not subtracting speculative pages, or adding inactive ones: the figure
//     counts read-ahead twice;
//   - multiplying by a fixed 4 KiB page instead of the page size handed in:
//     a 16 KiB-page machine reads a quarter of what it has;
//   - a product past int64 reported as a figure: the allocation reads a
//     wrapped, negative or meaningless amount of memory.
func TestDarwinAvailableIsFreeLessSpeculativePlusFileBacked(t *testing.T) {
	const page16k = 16 << 10
	cases := []struct {
		name                              string
		free, speculative, external, page uint64
		want                              int64
		ok                                bool
	}{
		{"16 KiB pages", 1000, 200, 300, page16k, 1100 * page16k, true},
		{"speculative above free clamps to zero", 100, 250, 300, page16k, 300 * page16k, true},
		{"no page size is no reading", 1000, 200, 300, 0, 0, false},
		{"a product past int64 is no reading", math.MaxInt64 / page16k, 0, 1, page16k, 0, false},
		{"a page count past uint64 is no reading", 10, 0, math.MaxUint64, 1, 0, false},
	}
	for _, c := range cases {
		got, ok := darwinAvailable(c.free, c.speculative, c.external, c.page)
		if got != c.want || ok != c.ok {
			t.Errorf("%s: darwinAvailable(%d, %d, %d, %d) = (%d, %t), want (%d, %t)",
				c.name, c.free, c.speculative, c.external, c.page, got, ok, c.want, c.ok)
		}
	}
}

// TestWindowsAvailableIsTheSmallerOfPhysicalAndCommit protects the Windows
// figure. Mutations that fail it: taking available physical memory alone (a
// job's commit limit, which is how a container's memory limit is enforced,
// is then ignored and its children over-admitted), taking the larger of the
// two, or reading a failed call's zeroed structure as a host with nothing
// available instead of as no reading.
func TestWindowsAvailableIsTheSmallerOfPhysicalAndCommit(t *testing.T) {
	const giB64 = uint64(1) << 30
	cases := []struct {
		name     string
		returned uintptr
		status   memoryStatusEx
		want     int64
		ok       bool
	}{
		{"commit below physical", 1, memoryStatusEx{AvailPhys: 12 * giB64, AvailPageFile: 3 * giB64}, 3 * giB, true},
		{"physical below commit", 1, memoryStatusEx{AvailPhys: 2 * giB64, AvailPageFile: 9 * giB64}, 2 * giB, true},
		{"a failed call is no reading", 0, memoryStatusEx{}, 0, false},
		{"a figure past int64 is no reading", 1, memoryStatusEx{AvailPhys: math.MaxUint64, AvailPageFile: math.MaxUint64}, 0, false},
	}
	for _, c := range cases {
		got, ok := windowsAvailable(c.returned, c.status)
		if got != c.want || ok != c.ok {
			t.Errorf("%s: windowsAvailable = (%d, %t), want (%d, %t)", c.name, got, ok, c.want, c.ok)
		}
	}
}

// TestAnUnreadableHostIsAdmittedAgainstTheStandIn protects the one rule for a
// host whose memory cannot be read: the 8 GiB stand-in, reported as not
// observed, whatever figure the unobserved reading happens to carry. It is
// the only way an unobserved reading becomes an allocation: the derivation
// itself (derivedAllocation) takes a figure, not a reading, so there is no
// second unobserved branch to disagree with this one. Mutations that fail it:
// SchedulingAllocation deriving an allocation from an unobserved reading's
// bytes (a zero reading then runs every child alone, and a stale one admits
// against memory nobody measured), returning zero or "no bound" for it, or
// reporting the stand-in as observed, which publishes it as a measurement.
func TestAnUnreadableHostIsAdmittedAgainstTheStandIn(t *testing.T) {
	const footprint = 512 << 20
	for _, m := range []Machine{{}, {AvailableBytes: 64 * giB}} {
		got, observed := m.SchedulingAllocation(footprint)
		if got != UnobservedAllocationBytes || observed {
			t.Errorf("unobserved %+v: SchedulingAllocation = (%d, %t), want the stand-in (%d, false)",
				m, got, observed, UnobservedAllocationBytes)
		}
	}
	// The same figure observed is derived, not stood in for: half of 64 GiB,
	// below 64 GiB less the footprint and the margin.
	got, observed := Machine{AvailableBytes: 64 * giB, Observed: true}.SchedulingAllocation(footprint)
	if got != 32*giB || !observed {
		t.Errorf("observed 64 GiB: SchedulingAllocation = (%d, %t), want (%d, true)", got, observed, int64(32*giB))
	}
}
