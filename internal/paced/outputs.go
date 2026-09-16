package paced

import (
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"
)

// outputWindows counts the windows the process has handed to the disk on
// behalf of a foreign writer.
var outputWindows atomic.Int64

// OutputWindows reports how many windows of a child's named outputs this
// process has handed to the disk one at a time since it started, for
// diagnostics and for the test that proves the pacing happened.
func OutputWindows() int64 { return outputWindows.Load() }

// outputPoll is how often the pacer looks at a writer's outputs for a window
// it has not handed to the disk yet. Like Window it is an implementation
// constant and not a setting: it bounds how long a finished window waits
// before it is submitted, and the submission itself is clocked by the disk.
const outputPoll = 50 * time.Millisecond

// writeRange is the platform's range writeback: the call that hands one window
// of a foreign writer's output to the disk and returns when it is written
// back. It is a variable so the fallback below -- the only path on a platform
// that has no range writeback at all, and the path Stop's contract rests on
// there -- is exercised on a platform that does have one.
var writeRange = WriteRange

// Outputs paces the files a foreign writer -- a child process this process
// cannot instrument -- writes, so that they reach the disk as they are
// written instead of as one burst when the kernel's flusher wakes.
//
// Measured: an analysis child dirtied 1.5 GB of its export at 50 MB/s with
// nothing reaching the disk, and the kernel then submitted 714 MB in one
// second with 407 requests in flight, which is what stalls every other
// process on the machine. The child cannot be asked to pace itself, but its
// outputs are named, so this walks them, keeps the offset of each file it has
// already handed over, and whenever a whole window has accumulated past that
// offset waits for the window before it and submits this one. Exactly one
// window per file is in flight from the pacer, and the pacer runs at the
// disk's rate, never ahead of it.
//
// The files are every regular file under the caller's named outputs: a step's
// output file, or its output directory and everything below it. The depth is
// the writer's own vocabulary and nothing makes it flat, so a sweep that read
// only the top level would leave a child that writes into a subdirectory
// entirely unpaced -- the burst this exists to remove, with the pacer running
// beside it and counting nothing.
type Outputs struct {
	paths []string
	stop  chan struct{}
	done  chan struct{}
	once  sync.Once
	// sent is the offset of each file already handed to the disk.
	sent map[string]int64
}

// StartOutputs begins pacing the named outputs. No paths is no pacer: a nil
// Outputs, whose Stop does nothing, so no caller needs a branch.
func StartOutputs(paths []string) *Outputs {
	if len(paths) == 0 {
		return nil
	}
	o := &Outputs{paths: paths, stop: make(chan struct{}), done: make(chan struct{}), sent: map[string]int64{}}
	go func() {
		defer close(o.done)
		ticker := time.NewTicker(outputPoll)
		defer ticker.Stop()
		for {
			select {
			case <-o.stop:
				return
			case <-ticker.C:
			}
			o.sweep(false)
		}
	}()
	return o
}

// Stop ends the pacing, joins the goroutine and drains what the writer left:
// the tail of every file, handed over the same way. It is idempotent, and the
// caller's outputs are on the disk when it returns.
func (o *Outputs) Stop() {
	if o == nil {
		return
	}
	o.once.Do(func() {
		close(o.stop)
		<-o.done
		o.sweep(true)
	})
}

// sweep hands over every window that has accumulated since the last one. The
// final sweep also hands over each file's tail, which is shorter than a
// window and would otherwise wait for the kernel.
//
// It runs on the pacer's own goroutine, or after that goroutine is joined,
// so sent is never touched by two goroutines at once.
func (o *Outputs) sweep(final bool) {
	for _, path := range o.paths {
		if path == "" {
			continue
		}
		info, err := os.Stat(path)
		if err != nil {
			// A writer creates its output part way through its run, and an
			// unreadable path is the absence of a signal, not a failure.
			continue
		}
		if !info.IsDir() {
			o.hand(path, final)
			continue
		}
		// The whole tree, and re-walked on every sweep: a writer creates
		// its directories part way through its run, so one that was not
		// there at the first sweep is reached at the next.
		_ = filepath.WalkDir(path, func(p string, entry fs.DirEntry, err error) error {
			// An entry that cannot be read, or that the writer removed
			// between the walk and the read, is the absence of a signal
			// rather than a failure: the rest of the tree still has windows
			// owed to the disk.
			if err != nil || entry.IsDir() {
				return nil
			}
			o.hand(p, final)
			return nil
		})
	}
}

// hand submits every whole window of one file past the offset already handed
// over, waiting for each before the next, and on the final sweep the tail as
// well. A platform without range writeback has nothing finer than the file's
// own sync, which the final sweep issues once per file.
func (o *Outputs) hand(path string, final bool) {
	// The kind is settled BEFORE the open, never after it. The entries of a
	// writer's output directory are the writer's own vocabulary, and opening
	// a named pipe blocks until a writer arrives: an open-first pacer would
	// wedge its poll goroutine on the first one, and Stop -- which the runner
	// defers after every child exits and which joins that goroutine -- would
	// hang the run with nothing to time it out. Only a regular file has a
	// range this can hand to the disk, so anything else is skipped here.
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() {
		return
	}
	f, err := os.Open(path)
	if err != nil {
		return
	}
	defer f.Close()
	end, sent := info.Size(), o.sent[path]
	for end-sent >= Window {
		if !writeRange(int(f.Fd()), sent, Window) {
			// This platform has no range writeback, so the file's own sync is
			// the finest thing it offers and it belongs at the end of the
			// writer's work rather than on every poll. The offset is NOT
			// advanced: these bytes are still only in the page cache, and the
			// final sweep below is what owes them to the disk.
			break
		}
		sent += Window
		o.sent[path] = sent
		outputWindows.Add(1)
	}
	if !final || end == sent {
		return
	}
	// The final sweep owes the tail, and where the range write failed it owes
	// everything past the last offset handed over. Stop's contract is that the
	// caller's outputs are on the disk when it returns; the sync is what makes
	// that true on a platform with no range writeback, and a sync that itself
	// fails leaves the offset where it was rather than claiming the bytes.
	if writeRange(int(f.Fd()), sent, end-sent) {
		outputWindows.Add(1)
	} else if err := f.Sync(); err != nil {
		return
	}
	o.sent[path] = end
}
