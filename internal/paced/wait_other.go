//go:build !linux

package paced

import "os"

// WaitWindow is never available on this platform; callers sync the file
// instead, which with at most two windows dirty is a bounded wait.
func WaitWindow(fd int) bool { return false }

// syncData waits for the file's data and metadata.
func syncData(f *os.File) error { return f.Sync() }
