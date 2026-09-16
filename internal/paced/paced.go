// Package paced holds the process's disk manners: the window that bounds what
// it hands the disk at once, the pace at which it gives disk space back, and
// the removals that keep to both.
//
// Freeing is the expensive act, and its cost is not the caller's to see. On a
// machine whose root filesystem discards freed blocks and whose disk is a
// sparse image on its host, a burst of frees leaves the host owing work it
// never reports: every disk request in flight about a minute later waits a
// minute, and nothing inside the machine can observe or wait for it. Measured
// both ways -- 5 GB freed in one burst hung the disk 64 seconds; the same
// 5 GB freed a window at a time with a data sync and a quarter-second wait
// between windows hung nothing.
//
// So this package frees nothing where it is asked to. A removal renames its
// file or tree into a to-free set -- a rename frees nothing and costs a
// directory entry -- and returns, and one reclaimer per process gives the
// space back at FreeInterval per Window, on its own goroutine, for as long as
// the process runs. The sets are on the disk, so a run that exits or crashes
// with removals queued leaves them for the next process to resume at the same
// pace; nothing is ever freed faster because it is old. A path under no
// registered set is freed in place, at the same pace.
//
// The engine's own writes are bounded the same way by the file-system shim in
// internal/storage/pacedvfs, which uses this package's window and wait.
package paced

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sync/atomic"
)

// steps counts the truncation steps this package has taken. It is not a
// figure anything discloses -- what an operator reads is bytes, FreedBytes,
// because that is what the disk charges for -- but whether a given removal
// truncated at all, and in how many steps, is the difference between freeing
// a window at a time and freeing in a burst, and nothing else distinguishes
// the two once the bytes are the same.
var steps atomic.Int64

// Window is the number of bytes handed to the disk between two waits: the
// bytes written to one file before the writer waits for the window before
// them, and the bytes freed before the reclaimer waits. It is small enough
// that a host with a small bounce-buffer pool never sees more than a few
// thousand pages in flight and large enough that a sequential device stays
// busy between submissions.
const Window = 8 << 20

// ShrinkForRemoval empties the named file a window at a time so that the
// unlink which follows frees nothing. It is the in-place form, for the one
// caller that must have the file empty when it returns: the engine's file
// system shim, whose delete the engine itself is waiting on. Anything that is
// not a regular file larger than the window is left to the unlink whole, and
// so is a file already gone.
//
// A shrink refused for permissions is one of those: emptying a file needs
// write on the FILE, unlinking it needs write on the DIRECTORY holding it, so
// a process may be entitled to remove a file it may not truncate. Pacing is
// how a removal is performed, never whether it is allowed, so such a file is
// unlinked whole. So is a file another name still reaches: see shrinkable.
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
	dir, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer dir.Close()
	_, err = reclaim.empty(path, st.Size(), "", dir)
	return err
}

// Remove deletes one file or empty directory: it renames the path into the
// to-free set that serves it and returns, leaving the reclaimer to give the
// space back at the pace. A path no set serves is freed in place, at the same
// pace. Removing what is not there is not a failure.
func Remove(path string) error { return RemoveFor("", path) }

// RemoveAll removes a tree the way Remove removes a file. Symbolic links are
// removed, never followed.
func RemoveAll(dir string) error { return RemoveAllFor("", dir) }

// Shrink truncates the named file down to size one window at a time, waiting
// after each step for the freed window to be dealt with. A file already at or
// below size is left alone.
//
// It is a truncation, not a removal: the caller keeps the file and goes on
// writing it, so there is nothing to rename away and the shrink happens where
// it is asked for.
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
		prev := cur
		cur = max(cur-Window, size)
		if err := f.Truncate(cur); err != nil {
			return err
		}
		if err := syncData(f); err != nil {
			return err
		}
		steps.Add(1)
		Freed(prev - cur)
	}
	return nil
}
