//go:build !linux && !windows && !(darwin && cgo)

package residency

// Read reports every figure absent: this build has no reader of the
// process's resident memory on this platform, or, on macOS, was built without
// cgo, which the task's memory counters need.
func Read() Reading { return Reading{} }
