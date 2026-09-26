//go:build linux

package paced

import (
	"os"

	"golang.org/x/sys/unix"
)

// WaitWindow waits for the pages of fd already submitted to the disk and
// then submits every page dirtied since, so that one window is in flight at
// a time. It reports false where the filesystem cannot do that, and the
// caller falls back to a sync of the file.
func WaitWindow(fd int) bool {
	return unix.SyncFileRange(fd, 0, 0, unix.SYNC_FILE_RANGE_WAIT_BEFORE|unix.SYNC_FILE_RANGE_WRITE) == nil
}

// WriteRange submits one range of fd to the disk and returns when it is
// written back: it waits for whatever of the range was already submitted,
// submits the rest, and waits for that too, so the caller holds exactly one
// range in flight. It reports false where the filesystem cannot do that, and
// the caller falls back to a sync of the file.
func WriteRange(fd int, offset, length int64) bool {
	return unix.SyncFileRange(fd, offset, length,
		unix.SYNC_FILE_RANGE_WAIT_BEFORE|unix.SYNC_FILE_RANGE_WRITE|unix.SYNC_FILE_RANGE_WAIT_AFTER) == nil
}

// syncData waits for the file's data and the metadata a later read needs,
// which after a truncation is the journal commit that frees the window.
func syncData(f *os.File) error { return unix.Fdatasync(int(f.Fd())) }
