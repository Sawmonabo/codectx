//go:build !linux

package process

import "time"

// treeSampled is false here: no portable equivalent of /proc gives a running
// process tree's resident memory, nor its transferred bytes. The runner reports
// both figures as unavailable rather than as zero, so a caller never reads "no
// measurement" as "no memory" or as "moved nothing" (Section 22).
const treeSampled = false

// treeSampler is the inert form for platforms with no tree sampling. It starts
// no goroutine, so there is nothing to join.
type treeSampler struct{}

func startTreeSampler(int, time.Duration) *treeSampler { return nil }

// stopSampling returns the empty sample: nothing was observed, and its zero
// ioSampled is what makes the runner report the byte figures as absent.
func (s *treeSampler) stopSampling() treeSample { return treeSample{} }

// cpuTicks is always zero here: with no tree sampling there is no CPU signal,
// so the stall watchdog is left with the portable byte counters alone.
func (s *treeSampler) cpuTicks() int64 { return 0 }
