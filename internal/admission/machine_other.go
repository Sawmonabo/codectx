//go:build !linux && !windows && !(darwin && cgo)

package admission

// ObserveMachine reports the host's available memory as unobserved where this
// build has no reader for it: a platform other than Linux, macOS and Windows,
// or a macOS build without cgo, which cannot reach the kernel's page counts.
// Section 22 requires an unavailable metric to be reported as unavailable, so
// nothing is invented that would act as a hidden ceiling. Admission still has
// a finite bound here: Machine.SchedulingAllocation stands a conservative
// allocation in for the observation, because a gate with no bound is not a
// gate.
func ObserveMachine() Machine { return Machine{} }
