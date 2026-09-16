//go:build linux

package writeback

import (
	"os"
	"time"

	"golang.org/x/sys/unix"
)

func (p *Pacer) run(paths []string) {
	defer close(p.done)
	files := make([]*os.File, len(paths))
	defer func() {
		for _, f := range files {
			if f != nil {
				f.Close()
			}
		}
	}()
	tick := time.NewTicker(Interval)
	defer tick.Stop()
	pass := func() {
		for i, name := range paths {
			if files[i] == nil {
				f, err := os.Open(name)
				if err != nil {
					continue
				}
				files[i] = f
			}
			// nbytes 0 means to the end of the file; the call returns once
			// writeback is queued, so the writer never waits on it. A
			// filesystem without the call is not an error: pacing is a
			// courtesy to the host.
			_ = unix.SyncFileRange(int(files[i].Fd()), 0, 0, unix.SYNC_FILE_RANGE_WRITE)
		}
	}
	for {
		select {
		case <-tick.C:
			pass()
		case <-p.stop:
			pass()
			return
		}
	}
}
