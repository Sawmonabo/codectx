package admission

// The machine reading the composition root derives the memory allocation from.
// The ledger itself observes nothing: the root reads the machine here, derives
// the figure and hands it to NewLedger and SetAllocation.

const (
	kiB = 1 << 10
	giB = 1 << 30
)

// DefaultSafetyMarginBytes is the headroom the machine-derived allocation
// leaves for transient allocation and resident-set variation on top of the
// base footprint its caller derives for this process (Section 23.3: degrade
// concurrency before coverage, and leave headroom rather than allocate to the
// last byte).
const DefaultSafetyMarginBytes int64 = 1 * giB

// UnobservedAllocationBytes is the allocation used where available memory
// could not be read: a platform with no reader (Linux, macOS and Windows each
// have one; ObserveMachine) or a reading that failed. It is not a memory
// ceiling on the work: it is the finite bound every queue and process
// admission must have, standing in for the observation the host withheld.
// Without it such a host would admit every child at once, which is the one
// outcome worse than a conservative allocation. SchedulingAllocation is the
// only place it is applied.
const UnobservedAllocationBytes int64 = 8 * giB

// hostShareDenominator is the second bound on the allocation: whatever the
// machine had available when the run began, the allocation is at most that
// divided by this, so the host keeps at least the rest of it.
//
// It is a design constant and not a setting. The product runs beside the
// editor, the agents and the browser that the person indexing their
// repository is using at the time, and "available memory minus two gigabytes"
// is an allocation that treats the machine as the product's own. Half is the
// share that leaves the machine usable by construction rather
// than by the operator noticing; a unit whose estimate exceeds even that
// still runs, whole, at the allocation, because refusing work for memory is
// not a thing this product does (docs/research/00-synthesis.md Section 8).
const hostShareDenominator = 2

// Machine is the observed memory of the host the allocation is derived from.
// Available is zero and Observed false when it could not be read.
type Machine struct {
	AvailableBytes int64
	Observed       bool
}

// derivedAllocation is the machine-derived allocation over an observed
// figure of available memory: that figure minus this process's base
// footprint minus the safety margin, and never more than the share of the
// machine this product takes (hostShareDenominator), so the host keeps at
// least the rest of what was available. It is zero where available memory is at or below
// the footprint and the margin, which means nothing is left over, and no
// bound is then placed on what one child may reserve: inventing one would be
// a default memory ceiling by another name, and the product has none.
//
// It takes the figure and not a Machine, so it has no unobserved case to get
// wrong: an unobserved reading reaches an allocation only through
// SchedulingAllocation, which stands UnobservedAllocationBytes in for it.
func derivedAllocation(availableBytes, baseFootprint, safetyMargin int64) int64 {
	alloc := min(availableBytes-baseFootprint-safetyMargin, availableBytes/hostShareDenominator)
	if alloc < 0 {
		return 0
	}
	return alloc
}

// SchedulingAllocation is the one allocation every heavy child of this process
// is admitted against, and whether it is an observation.
//
//   - An observed host: the machine-derived allocation (derivedAllocation), observed.
//     Where available memory is at or below this process's footprint and the
//     safety margin, that is zero, which is a real reading and not an absence:
//     nothing is left over for the children, so the admission ledger runs them
//     one at a time (its runs-alone rule) rather than inventing room.
//   - A host whose available memory could not be read -- a platform with no
//     reader, or a reading that failed: UnobservedAllocationBytes, not
//     observed, standing in for the observation the host withheld, because a
//     gate with no bound is not a gate. This is the one place an unobserved
//     reading becomes an allocation.
//
// baseFootprintBytes is what this process holds for itself, which its caller
// derives from this machine and the configuration (config.BaseFootprint). It
// is a parameter and not a constant read from here because a figure typed into
// this package could not follow the core count: the reservations the process
// makes up front grow with the machine, and an allocation computed against a
// fixed figure hands the children memory the parent has already promised
// itself.
func (m Machine) SchedulingAllocation(baseFootprintBytes int64) (allocation int64, observed bool) {
	if !m.Observed {
		return UnobservedAllocationBytes, false
	}
	return derivedAllocation(m.AvailableBytes, baseFootprintBytes, DefaultSafetyMarginBytes), true
}
