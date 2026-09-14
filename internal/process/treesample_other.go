//go:build !linux

package process

import "time"

// treeSampled is false here: no portable equivalent of /proc gives a running
// process tree's resident memory. The runner reports the figure as unavailable
// rather than as zero, so a caller never reads "no measurement" as "no memory"
// (Section 22).
const treeSampled = false

// treeSampler is the inert form for platforms with no tree sampling. It starts
// no goroutine, so there is nothing to join.
type treeSampler struct{}

func startTreeSampler(int, time.Duration) *treeSampler { return nil }

func (s *treeSampler) stopSampling() int64 { return 0 }
