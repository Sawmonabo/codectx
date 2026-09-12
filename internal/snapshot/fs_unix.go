//go:build !windows

package snapshot

import (
	"errors"
	"os"
	"syscall"
)

// tryLock takes an exclusive advisory lock on f without blocking. It reports
// false when another descriptor holds it.
func tryLock(f *os.File) (bool, error) {
	err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
	if err == nil {
		return true, nil
	}
	if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
		return false, nil
	}
	return false, err
}

func unlock(f *os.File) error {
	return syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
}

// syncDir makes a directory entry durable after a publication. POSIX requires
// an fsync on the directory for a new name to survive a crash; a filesystem
// that does not support it reports EINVAL or ENOTSUP, which is not a failure
// of the publication itself.
func syncDir(path string) error {
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
