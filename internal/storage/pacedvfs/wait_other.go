//go:build !linux

package pacedvfs

// waitMode selects the wrapped file's own sync as the wait on a platform
// without range writeback: with at most two windows dirty it is a bounded
// wait, and it needs nothing from the wrapped file object's layout.
func waitMode(inner uintptr, zName uintptr) (int32, int32) { return bySync, 0 }

// waitRange is never selected on this platform.
func waitRange(fd int32) bool { return false }
