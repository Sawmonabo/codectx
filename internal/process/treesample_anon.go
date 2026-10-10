//go:build windows || (darwin && cgo)

package process

import (
	"sync"
	"sync/atomic"
	"time"
)

// treeSampled is false here: these platforms are sampled for the running
// tree's anonymous resident memory only, which the allocation adds back
// (anonBytes), and not for its resident peak or its transferred bytes. The
// runner reports those figures as unavailable rather than as zero, so a
// caller never reads "no measurement" as "no memory" or as "moved nothing"
// (Section 22).
const treeSampled = false

// treeSampler keeps the running tree's summed anonymous resident set as of
// the last sweep that found the tree (treeSweeper, per platform), sampled
// while the tree runs. It is read while the run is going, by the
// allocation's add-back of the product's own residency (Runner.
// ReservedResidentBytes, CPUProgress.AnonResidentBytes).
type treeSampler struct {
	stop     chan struct{}
	done     chan struct{}
	once     sync.Once
	anon     atomic.Int64
	anonSeen atomic.Bool
}

// treeSweep is one sweep's figure over the tree: the summed anonymous
// resident set, whether the sweep found any member (found), and whether it
// read the figure for every member it found (read).
type treeSweep struct {
	anon  int64
	found bool
	read  bool
}

// startTreeSampler begins sampling the tree rooted at the child pid every
// interval. It is nil, and every figure unavailable, where the platform gives
// no way to address the tree (treeSweeper).
func startTreeSampler(pid int, interval time.Duration) *treeSampler {
	sweep := treeSweeper(pid)
	if sweep == nil {
		return nil
	}
	s := &treeSampler{stop: make(chan struct{}), done: make(chan struct{})}
	go func() {
		defer close(s.done)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			// The sweep comes first so that a child which exits inside the
			// first interval is still measured at least once.
			if sums := sweep(); sums.found {
				// Kept across sweeps that find nothing: a sweep after the tree
				// exits must not drop the figure back to zero. A sweep that
				// could not read some member has no figure for the tree, so
				// the reading becomes unavailable rather than the partial sum.
				s.anon.Store(sums.anon)
				s.anonSeen.Store(sums.read)
			}
			select {
			case <-s.stop:
				return
			case <-ticker.C:
			}
		}
	}()
	return s
}

// stopSampling ends the sweep and joins the goroutine. It is idempotent, and
// it never returns before the goroutine has finished: no goroutine this
// package starts outlives the run that started it. The sample is empty: no
// peak or byte counters are sampled here, and its zero ioSampled is what makes
// the runner report the byte figures as absent.
func (s *treeSampler) stopSampling() treeSample {
	if s == nil {
		return treeSample{}
	}
	s.once.Do(func() { close(s.stop) })
	<-s.done
	return treeSample{}
}

// cpuTicks never has a measurement here: the tree's processor time is not
// sampled, so the stall watchdog is left with the pipes and the named output
// files, and a CPUProgress reader with its own fallback.
func (s *treeSampler) cpuTicks() (int64, bool) { return 0, false }

// anonBytes reports the tree's summed anonymous resident set as of the last
// sweep that found it. ok is false until a sweep has read it for every member
// it found, and after a sweep that could not: no measurement, not zero.
func (s *treeSampler) anonBytes() (int64, bool) {
	if s == nil || !s.anonSeen.Load() {
		return 0, false
	}
	return s.anon.Load(), true
}
