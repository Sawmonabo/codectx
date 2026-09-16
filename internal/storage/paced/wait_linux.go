//go:build linux

package paced

import (
	"sync/atomic"

	"golang.org/x/sys/unix"
	"modernc.org/libc"
	sqlite3 "modernc.org/sqlite/lib"
)

// layoutTrusted records whether the descriptor read from the wrapped file
// object matched a named file's path, the check an unnamed file cannot make
// for itself.
var layoutTrusted atomic.Bool

// waitMode reads the descriptor the wrapped file system opened and, for a
// named file, checks it against the path before trusting it: the descriptor
// is read from the wrapped file object's own layout, and a layout that no
// longer matches would otherwise wait on some other file. An unnamed
// temporary file has no path to check and inherits the outcome of the last
// named check, since one layout serves every file the process opens.
func waitMode(inner uintptr, zName uintptr) (int32, int32) {
	fd := (*sqlite3.TunixFile)(ptr(inner)).Fh
	if fd < 0 {
		return bySync, 0
	}
	if zName != 0 {
		var byPath, byFd unix.Stat_t
		if unix.Stat(libc.GoString(zName), &byPath) != nil || unix.Fstat(int(fd), &byFd) != nil ||
			byPath.Ino != byFd.Ino || byPath.Dev != byFd.Dev {
			layoutTrusted.Store(false)
			return bySync, 0
		}
		layoutTrusted.Store(true)
	} else if !layoutTrusted.Load() {
		return bySync, 0
	}
	return byRange, fd
}

// waitRange waits for the pages of fd already submitted to the disk and then
// submits every page dirtied since, so that one window is in flight at a
// time. It reports false where the file system cannot do that, and the
// caller falls back to the wrapped sync.
func waitRange(fd int32) bool {
	return unix.SyncFileRange(int(fd), 0, 0, unix.SYNC_FILE_RANGE_WAIT_BEFORE|unix.SYNC_FILE_RANGE_WRITE) == nil
}
