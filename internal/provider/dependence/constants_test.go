package dependence

import "testing"

// TestJavaScriptConstantsReproduceTheirDerivation recomputes the two measured
// constants of ADR-0010 from the inputs the ADR records, so that the stated
// derivation and the shipped value cannot drift apart.
//
// Failure mode: a constant whose stated derivation stops reproducing is a
// constant nobody can check. Both of these had stopped: the heap estimate was
// reachable only by dividing binary GiB by decimal MB, and the resident
// allowance by reading a decimal-GB peak as binary GiB. The second one is not
// cosmetic -- under the figure it produced, the reference repository's
// JavaScript unit reserved less than the memory its process tree was measured
// to use, in both reference runs, which is how a host runs out of memory
// against reservations that all appear to fit.
func TestJavaScriptConstantsReproduceTheirDerivation(t *testing.T) {
	// The unit the ADR's tables were measured on, from the reference run's
	// ledger: source_files=4984 source_bytes=164865219.
	const unitSourceBytes int64 = 164_865_219
	// The smallest ceiling that ran the unit at the speed of no ceiling at all,
	// and the research's headroom rule over it (Section 4, observation 3).
	const fullSpeedCeilingBytes int64 = 4 * giB
	const headroomMultiple int64 = 2

	wantHeap := headroomMultiple * fullSpeedCeilingBytes / unitSourceBytes
	if got := heapPerSourceByte[FamilyJavaScript]; got != wantHeap {
		t.Errorf("heapPerSourceByte[javascript] is %d, want %d = %d x %d / %d: the shipped constant no longer reproduces its stated derivation",
			got, wantHeap, headroomMultiple, fullSpeedCeilingBytes, unitSourceBytes)
	}
	// The ADR claims the estimate is twice the full-speed ceiling. Whatever the
	// integer division lands on, that claim must be what the code produces, or
	// the record is describing a different constant.
	if cap := wantHeap * unitSourceBytes; cap < fullSpeedCeilingBytes*199/100 || cap > headroomMultiple*fullSpeedCeilingBytes {
		t.Errorf("the estimate caps this unit at %d bytes, %.3fx the %d-byte full-speed ceiling; the record claims twice",
			cap, float64(cap)/float64(fullSpeedCeilingBytes), fullSpeedCeilingBytes)
	}

	// Non-heap residency, from the only parse step of this family recorded in
	// bytes and at a cap its heap actually filled (reference run ledger:
	// heap_cap_bytes and tree_peak_bytes of pkg:javascript:app). Peak tree
	// residency minus the heap cap under-states wherever the heap was left
	// unused, so a row whose cap was never reached is not evidence.
	const observedHeapCapBytes int64 = 7_913_530_512
	const observedTreePeakBytes int64 = 10_099_015_680

	wantResident := (observedTreePeakBytes - observedHeapCapBytes) / miB
	if got := residentAboveHeap[FamilyJavaScript]; got != wantResident*miB {
		t.Errorf("residentAboveHeap[javascript] is %d MiB, want %d MiB = (%d - %d) / MiB",
			got/miB, wantResident, observedTreePeakBytes, observedHeapCapBytes)
	}
	// The two together are what a unit of this family is admitted against, and
	// the reservation must cover the residency this very unit was measured to
	// reach. A pair that does not cover it is a pair that lets a host be
	// over-committed by reservations that all appear to fit.
	if reservation := wantHeap*unitSourceBytes + residentAboveHeap[FamilyJavaScript]; reservation < observedTreePeakBytes {
		t.Errorf("a %d-byte unit of this family reserves %d bytes, under the %d bytes its process tree was measured to reach",
			unitSourceBytes, reservation, observedTreePeakBytes)
	}
}
