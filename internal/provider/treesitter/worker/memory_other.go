//go:build !linux

package worker

// nativeHost reports every reading as unavailable: outside Linux the worker
// has no per-process status file and no resettable peak.
func nativeHost() host {
	return host{
		status:     func() (string, bool) { return "", false },
		returnHeap: func() bool { return false },
		resetPeak:  func() bool { return false },
	}
}
