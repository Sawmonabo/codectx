//go:build linux || darwin

package paced

import "golang.org/x/sys/unix"

// unlinkRefused reports why dir would refuse to have an entry unlinked from
// it -- this process may not write or search it, or it is on a read-only
// mount -- or nil when it would not. The check is the process's effective
// identity, the one an unlink is decided by.
func unlinkRefused(dir string) error {
	return unix.Faccessat(unix.AT_FDCWD, dir, unix.W_OK|unix.X_OK, unix.AT_EACCESS)
}
