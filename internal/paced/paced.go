// Package paced holds the process's disk manners: the window that bounds
// what it hands the disk at once, and the file removals that keep to it.
//
// A process that frees gigabytes with one unlink hands the filesystem every
// freed extent at once; on a filesystem that discards freed space as it goes,
// the journal commit that frees them issues every discard before it
// completes, and every other process's sync waits behind it. Measured on a
// virtual machine: removing 4.4 GB of an analyzer's export and staging at
// the end of an index stalled the whole host for over a minute. A file
// larger than the window is therefore shrunk one window at a time, with the
// freed window's work waited for between steps, the way a storage engine's
// delete scheduler removes its files.
//
// The engine's own writes are bounded the same way by the file-system shim
// in internal/storage/pacedvfs, which uses this package's window and wait.
package paced

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sync/atomic"
)

// steps counts the windows freed so far, one per truncation step.
var steps atomic.Int64

// Steps reports how many windows the process has freed one at a time since
// it started: a removal's cost in disk work, for diagnostics.
func Steps() int64 { return steps.Load() }

// Window is the number of bytes handed to the disk between two waits: the
// bytes written to one file before the writer waits for the window before
// them, and the bytes freed from one file before the freeing waits. It is
// small enough that a host with a small bounce-buffer pool never sees more
// than a few thousand pages in flight and large enough that a sequential
// device stays busy between submissions.
const Window = 8 << 20

// ShrinkForRemoval empties the named file a window at a time so that the
// unlink which follows frees nothing, and reports only the failures that make
// the removal itself unsafe. Anything that is not a regular file larger than
// the window is left to the unlink whole, and so is a file already gone.
//
// A shrink refused for permissions is one of those: emptying a file needs
// write on the FILE, unlinking it needs write on the DIRECTORY holding it, so
// a process may be entitled to remove a file it may not truncate -- an
// analyzer's read-only output among them. Pacing is how a removal is
// performed, never whether it is allowed, so such a file is unlinked whole.
// It is the one case where more than a window of extents is freed at once, and
// it is bounded by what the process did not write itself.
func ShrinkForRemoval(path string) error {
	st, err := os.Lstat(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return err
	}
	if !st.Mode().IsRegular() || st.Size() <= Window {
		return nil
	}
	err = Shrink(path, 0)
	if errors.Is(err, fs.ErrNotExist) || errors.Is(err, fs.ErrPermission) {
		return nil
	}
	return err
}

// Remove deletes one file or empty directory. A regular file larger than the
// window is truncated down one window at a time first, each step synced so
// the freed window is dealt with before the next is freed.
func Remove(path string) error {
	if err := ShrinkForRemoval(path); err != nil {
		return err
	}
	return os.Remove(path)
}

// RemoveAll removes a tree the way Remove removes a file: every regular
// file larger than the window is shrunk first, then the tree is unlinked.
// Symbolic links are removed, never followed.
func RemoveAll(dir string) error {
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			return err
		}
		if !d.Type().IsRegular() {
			return nil
		}
		return ShrinkForRemoval(path)
	})
	if err != nil {
		return err
	}
	return os.RemoveAll(dir)
}

// Shrink truncates the named file down to size one window at a time, waiting
// after each step for the freed window to be dealt with. A file already at
// or below size is left alone.
func Shrink(path string, size int64) error {
	f, err := os.OpenFile(path, os.O_WRONLY, 0)
	if err != nil {
		return err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return err
	}
	return shrinkFile(f, st.Size(), size)
}

// shrinkFile is Shrink on an open file whose current size the caller knows.
func shrinkFile(f *os.File, cur, size int64) error {
	for cur > size {
		cur -= Window
		if cur < size {
			cur = size
		}
		if err := f.Truncate(cur); err != nil {
			return err
		}
		if err := syncData(f); err != nil {
			return err
		}
		steps.Add(1)
	}
	return nil
}
