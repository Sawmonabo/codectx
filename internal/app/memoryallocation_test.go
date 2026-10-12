package app

import (
	"testing"

	"github.com/Sawmonabo/codectx/internal/admission"
)

// TestAWithheldReadingLeavesTheLedgerAllocationAsItWas protects the
// re-derivation before each admission on a host whose reading fails.
//
// A reading the platform withholds is no figure at all, so the allocation
// the ledger last held stays: the stand-in on a host that never reported one,
// and an earlier observed derivation on one whose reading failed later. The
// second case is what tells keeping the allocation apart from deriving the
// stand-in again.
//
// Mutation proof: drop the `!m.Observed` return in rederive, so it derives
// from the unobserved reading, and the ledger at 3 GiB moves to
// admission.UnobservedAllocationBytes.
func TestAWithheldReadingLeavesTheLedgerAllocationAsItWas(t *testing.T) {
	for _, earlier := range []int64{admission.UnobservedAllocationBytes, 3 << 30} {
		l, err := admission.NewLedger(earlier, 0)
		if err != nil {
			t.Fatalf("NewLedger: %v", err)
		}
		a := &ledgerAllocation{l: l, baseFootprintBytes: 512 << 20,
			observe: func() admission.Machine { return admission.Machine{} }}
		a.rederive()
		if got, _ := l.Snapshot(); got != earlier {
			t.Errorf("after a withheld reading the ledger allocation is %d, want the earlier %d", got, earlier)
		}
	}
}

// TestAnUnobservedReadingIsTheStandInWhateverItsBytes protects the one
// derivation of the allocation from an unobserved reading.
//
// An unobserved reading's bytes measured nothing, so the add-back of the
// product's own residency must not turn them into an observed figure: the
// allocation is the stand-in, reported as not observed.
//
// Mutation proof: drop the `!m.Observed` guard in memoryAllocation and the
// answer is a derivation over 96 GiB, reported as observed.
func TestAnUnobservedReadingIsTheStandInWhateverItsBytes(t *testing.T) {
	got, observed := memoryAllocation(admission.Machine{AvailableBytes: 64 << 30}, 512<<20, 32<<30)
	if got != admission.UnobservedAllocationBytes || observed {
		t.Fatalf("memoryAllocation over an unobserved reading = (%d, %t), want the stand-in (%d, false)",
			got, observed, admission.UnobservedAllocationBytes)
	}
}
