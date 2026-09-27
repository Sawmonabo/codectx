//go:build !linux

package worker

import "github.com/Sawmonabo/codectx/internal/residency"

// nativeHost reports every reading as unavailable: outside Linux the worker
// has no per-process status file and no resettable peak, so residency.Read
// answers every figure absent.
func nativeHost() host {
	return host{
		read:       residency.Read,
		returnHeap: func() bool { return false },
		resetPeak:  func() bool { return false },
	}
}
