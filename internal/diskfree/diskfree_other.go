//go:build !unix

package diskfree

// Available reports the figure as unmeasured on a platform this build has no
// portable syscall for. A zero here would read as a full disk.
func Available(dir string) (uint64, bool) { return 0, false }
