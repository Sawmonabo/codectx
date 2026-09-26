//go:build unix

package sqlite

import (
	"errors"

	"golang.org/x/sys/unix"
)

// probeDirAccess asks the kernel whether this process may create files in dir.
// One call answers both ways a directory refuses: its permissions (EACCES,
// EPERM) and a filesystem mounted read-only (EROFS). Any other failure is not
// an answer about writability and is returned as it came.
func probeDirAccess(dir string) (dirAccess, error) {
	err := unix.Access(dir, unix.W_OK)
	switch {
	case err == nil:
		return dirWritable, nil
	case errors.Is(err, unix.EACCES), errors.Is(err, unix.EPERM), errors.Is(err, unix.EROFS):
		return dirReadOnly, nil
	}
	return dirAccessUnknown, err
}
