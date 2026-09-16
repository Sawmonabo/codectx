//go:build !linux

package dependence

// ObserveMachine reports the host's available memory as unobserved on every
// platform that does not publish a kernel estimate this package can read
// without a dependency. Section 22 requires an unavailable metric to be
// reported as unavailable: with no observation there is no machine-derived
// allocation and the unit's own estimate stands as its cap. That is the same
// outcome the engine's own default produces, so nothing is lost that was not
// already unbounded — and nothing is invented that would act as a hidden
// ceiling. Admission still has a finite bound here: Machine.SchedulingAllocation
// stands a conservative allocation in for the observation, because a gate with
// no bound is not a gate.
func ObserveMachine() Machine { return Machine{} }
