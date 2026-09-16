package paced

import (
	"os"
	"sync/atomic"
)

// waits counts the window waits the process's own writers have issued.
var waits atomic.Int64

// Waits reports how many windows the process's own file writers have handed
// to the disk one at a time since it started, for diagnostics.
func Waits() int64 { return waits.Load() }

// Writer is a file writer that hands the disk one window at a time: after
// every Window bytes written it waits for the previous window to reach the
// disk and submits the new one, so a stream the process writes itself -- a
// key set, a spool, a sort's runs -- reaches the disk as it is written and
// never as one burst when the kernel's dirty-page timer expires. It is the
// same discipline the engine's writes get from the file-system shim.
type Writer struct {
	f     *os.File
	since int64
}

// NewWriter wraps f. The caller keeps ownership of f and closes it.
func NewWriter(f *os.File) *Writer { return &Writer{f: f} }

// Write writes p to the file and, once a window has accumulated, waits for
// the window before it. A platform without range writeback syncs the file
// instead, which with at most two windows dirty is a bounded wait.
func (w *Writer) Write(p []byte) (int, error) {
	n, err := w.f.Write(p)
	w.since += int64(n)
	if err != nil {
		return n, err
	}
	if w.since >= Window {
		w.since = 0
		waits.Add(1)
		if !WaitWindow(int(w.f.Fd())) {
			if err := w.f.Sync(); err != nil {
				return n, err
			}
		}
	}
	return n, nil
}
