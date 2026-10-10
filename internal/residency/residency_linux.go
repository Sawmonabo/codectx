//go:build linux

package residency

import "os"

// statusPath is the kernel's per-process status file.
const statusPath = "/proc/self/status"

// Read reads this process's status file once (Parse). Every figure is absent
// where the file cannot be read.
func Read() Reading {
	raw, err := os.ReadFile(statusPath)
	if err != nil {
		return Reading{}
	}
	return Parse(string(raw))
}
