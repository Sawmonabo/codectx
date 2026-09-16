package paced

import (
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
// The files are the caller's named outputs read one level deep, which is the
// same set the stall watchdog sums: a step's output file, or its output
// directory, whose entries are a property of the writer's own vocabulary.
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
		entries, err := os.ReadDir(path)
		if err != nil {
			continue
		}
		for _, entry := range entries {
			if entry.IsDir() {
				continue
			}
			o.hand(filepath.Join(path, entry.Name()), final)
		}
	}
}

// hand submits every whole window of one file past the offset already handed
// over, waiting for each before the next, and on the final sweep the tail as
// well. A platform without range writeback has nothing finer than the file's
// own sync, which the final sweep issues once per file.
func (o *Outputs) hand(path string, final bool) {
	f, err := os.Open(path)
	if err != nil {
		return
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return
	}
	end, sent := info.Size(), o.sent[path]
	for end-sent >= Window {
		if !WriteRange(int(f.Fd()), sent, Window) {
			o.sent[path] = end
			if final {
				_ = f.Sync()
			}
			return
		}
		sent += Window
		o.sent[path] = sent
		outputWindows.Add(1)
	}
	if !final || end == sent {
		return
	}
	if WriteRange(int(f.Fd()), sent, end-sent) {
		outputWindows.Add(1)
	} else {
		_ = f.Sync()
	}
	o.sent[path] = end
}
