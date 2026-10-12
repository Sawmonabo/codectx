//go:build !linux

package worker

import "github.com/Sawmonabo/codectx/internal/residency"

// nativeHost reads the process's residency where the platform reports it
// (residency.Read: macOS built with cgo and Windows report the resident set
// and its lifetime peak, and macOS the anonymous part too) and reports the
// rest as unavailable: outside Linux the worker cannot reset its resident
// peak or return its C heap, so it reports its base and anonymous figures
// where they are read and never a file's need, which would be read off a peak
// that spans earlier files.
func nativeHost() host {
	return host{
		read:       residency.Read,
		returnHeap: func() bool { return false },
		resetPeak:  func() bool { return false },
	}
}
