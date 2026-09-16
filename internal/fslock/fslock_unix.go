//go:build !windows

package fslock

import (
	"errors"
	"os"
	"syscall"
)

// TryLock takes an exclusive advisory lock on f without blocking. It reports
// false when another descriptor holds it.
func TryLock(f *os.File) (bool, error) {
	err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
	if err == nil {
		return true, nil
	}
	if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
		return false, nil
	}
	return false, err
}

// Lock takes an exclusive advisory lock on f, waiting for whichever
// descriptor holds it to let it go. It is what a turn-taking protocol needs
// and TryLock cannot give: a caller that must have the lock, and whose only
// alternative to waiting is retrying in a loop.
func Lock(f *os.File) error {
	for {
		err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX)
		if !errors.Is(err, syscall.EINTR) {
			return err
		}
	}
}

// Unlock releases the lock TryLock took.
func Unlock(f *os.File) error {
	return syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
}

// SyncDir makes a directory entry durable after a publication. POSIX requires
// an fsync on the directory for a new name to survive a crash; a filesystem
// that does not support it reports EINVAL or ENOTSUP, which is not a failure
// of the publication itself.
func SyncDir(path string) error {
	d, err := os.Open(path)
	if err != nil {
		return err
	}
	defer d.Close()
	if err := d.Sync(); err != nil && !errors.Is(err, syscall.EINVAL) && !errors.Is(err, syscall.ENOTSUP) {
		return err
	}
	return nil
}
