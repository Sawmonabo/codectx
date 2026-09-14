//go:build !linux

package dependence

// ObserveMachine reports the host's available memory as unobserved on every
// platform that does not publish a kernel estimate this package can read
// without a dependency. Section 22 requires an unavailable metric to be
// reported as unavailable: with no observation there is no machine-derived
// allocation, the unit's own estimate stands, and only an explicit
// `unit_memory_ceiling_bytes` can bound it. That is the same outcome the
// engine's own default produces, so nothing is lost that was not already
// unbounded — and nothing is invented that would act as a hidden ceiling.
func ObserveMachine() Machine { return Machine{} }
