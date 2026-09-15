//go:build !windows

package snapshot

import "syscall"

// openFileLimit reports the process's soft descriptor limit, or 0 when it is
// unknown or effectively unlimited. It sizes the sync window so a group flush
// never holds more temporaries open than the process may have descriptors for.
func openFileLimit() int {
	var lim syscall.Rlimit
	if err := syscall.Getrlimit(syscall.RLIMIT_NOFILE, &lim); err != nil {
		return 0
	}
	if lim.Cur == 0 || uint64(lim.Cur) > 1<<30 {
		return 0
	}
	return int(lim.Cur)
}
