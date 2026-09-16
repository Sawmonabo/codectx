// Package writeback paces the kernel's writeback of files a process fills at
// memory speed.
//
// A process that dirties gigabytes of page cache faster than the disk drains
// it leaves the kernel to flush the backlog on its own schedule: as one burst
// when the dirty ratio is crossed, or when a later fsync forces everything
// out at once. On a host whose disk path has a small bounce-buffer pool that
// burst stalls every other process too. A pacer asks the kernel to start
// writing the named files on a fixed cadence instead, so the disk traffic is
// spread evenly over the time the writer takes and the dirty set stays in the
// tens of megabytes. It never waits for a write to finish and never changes
// what is durable when: durability is still the caller's fsync.
package writeback

import "time"

// Interval is how often a pacer asks the kernel to start writing. At 100 ms
// and any disk rate a single writer can reach, the dirty set stays small.
const Interval = 100 * time.Millisecond

// Pacer streams a set of files to disk on a fixed cadence until stopped.
// Start returns one; on a system without range writeback it does nothing.
type Pacer struct {
	stop chan struct{}
	done chan struct{}
}

// Start begins pacing the named files. A file that does not exist yet is
// retried on each pass, so a log that appears after Start is covered from
// its first bytes.
func Start(paths ...string) *Pacer {
	p := &Pacer{stop: make(chan struct{}), done: make(chan struct{})}
	go p.run(paths)
	return p
}

// Stop ends pacing after one last pass and returns once that pass is queued.
// Stopping a nil pacer is a no-op.
func (p *Pacer) Stop() {
	if p == nil {
		return
	}
	close(p.stop)
	<-p.done
}
