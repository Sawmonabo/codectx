//go:build linux

package sqlite

import (
	"os"
	"time"

	"golang.org/x/sys/unix"
)

// writebackInterval is how often the pacer asks the kernel to start writing
// the store's dirty pages while an ingestion group is open. At 100 ms and any
// disk rate the store can reach, the dirty set stays in the tens of megabytes
// instead of growing to the whole group before the commit's fsync flushes it
// in one burst; a burst that size stalls a host whose disk path has a small
// bounce-buffer pool, and it is what a single fsync of a gigabyte of log does.
const writebackInterval = 100 * time.Millisecond

// writebackPacer initiates writeback of the database and its write-ahead log
// on a fixed cadence. It never waits for the writes to finish and never
// changes what is durable when: durability is still the commit's fsync. It
// only spreads the disk traffic of an ingestion group evenly over the time the
// group takes, the way a database's bytes-per-sync setting does.
type writebackPacer struct {
	stop chan struct{}
	done chan struct{}
}

// startPacer begins pacing for the store's files. groupMu is held.
func (s *Store) startPacer() {
	if s.pacer != nil || s.path == "" {
		return
	}
	p := &writebackPacer{stop: make(chan struct{}), done: make(chan struct{})}
	s.pacer = p
	go p.run(s.path)
}

// stopPacer ends pacing after one last pass. groupMu is held.
func (s *Store) stopPacer() {
	if s.pacer == nil {
		return
	}
	close(s.pacer.stop)
	<-s.pacer.done
	s.pacer = nil
}

func (p *writebackPacer) run(path string) {
	defer close(p.done)
	files := [2]*os.File{}
	names := [2]string{path, path + "-wal"}
	defer func() {
		for _, f := range files {
			if f != nil {
				f.Close()
			}
		}
	}()
	tick := time.NewTicker(writebackInterval)
	defer tick.Stop()
	pass := func() {
		for i, name := range names {
			if files[i] == nil {
				f, err := os.Open(name)
				if err != nil {
					continue // the log does not exist until the first frame
				}
				files[i] = f
			}
			// nbytes 0 means to the end of the file; the call returns once
			// writeback is queued, so the ingestion goroutines never wait
			// on it. A filesystem without the call is not an error: pacing is
			// a courtesy to the host, and the commit still fsyncs.
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
