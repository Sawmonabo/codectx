//go:build !linux && !windows && !(darwin && cgo)

package process

import "time"

// treeSampled is false here: this build has no reader of a running process
// tree's memory on this platform (or, on macOS, was built without cgo, which
// the per-process counters need), nor of its transferred bytes. The runner
// reports both figures as unavailable rather than as zero, so a caller never
// reads "no measurement" as "no memory" or as "moved nothing" (Section 22).
const treeSampled = false

// treeAnonSampled reports that no running tree's anonymous resident memory is
// read here, so nothing is added back for a child.
const treeAnonSampled = false

// treeSampler is the inert form for platforms with no tree sampling. It starts
// no goroutine, so there is nothing to join.
type treeSampler struct{}

func startTreeSampler(treeTarget, time.Duration) *treeSampler { return nil }

// stopSampling returns the empty sample: nothing was observed, and its zero
// ioSampled is what makes the runner report the byte figures as absent.
func (s *treeSampler) stopSampling() treeSample { return treeSample{} }

// cpuTicks never has a measurement here: with no tree sampling there is no CPU
// signal, so the stall watchdog is left with the pipes and the named output
// files, and a CPUProgress reader with its own fallback.
func (s *treeSampler) cpuTicks() (int64, bool) { return 0, false }

// anonBytes never has a measurement here, so the allocation leaves a child's
// residency out of its add-back rather than counting it as zero.
func (s *treeSampler) anonBytes() (int64, bool) { return 0, false }
