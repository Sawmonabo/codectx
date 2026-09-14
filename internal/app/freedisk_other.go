//go:build !unix

package app

// freeDiskBytes reports the free-space figure as absent on a platform this
// build has no portable syscall for. Absent is the honest answer: Section 23
// records an unavailable metric as unavailable, and a zero here would read as a
// full disk and fail the free-space check on every run.
func freeDiskBytes(dir string) (*uint64, error) { return nil, nil }
