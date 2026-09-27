package process

import "sync/atomic"

// CPUProgress carries a running child's consumed processor time, and its
// anonymous resident set, out to the caller that started it.
//
// It exists so a caller that runs its own hang detector over a protocol this
// package cannot see inside -- a framed request/response stream, where a
// wedged request and a long one look identical on the wire -- watches the same
// processor-time signal the stall watchdog here already watches, instead of
// growing a second sampler beside it. A child computing an answer and saying
// nothing is working, and only this signal can say so.
//
// The caller allocates one, hands it to the runner in Spec.CPUProgress and
// reads it while the run is in flight. The runner binds it to the tree sampler
// once the child exists; before that, before a sweep has found the child's
// group, and on a platform that cannot sample a running tree at all, it
// reports nothing observed rather than zero ticks, so
// no caller reads a missing measurement as "used no processor time"
// (Section 22).
type CPUProgress struct {
	sampler atomic.Pointer[treeSampler]
}

// Ticks reports the child tree's summed user and system processor time, in
// whatever unit the platform counts it in, as of the last sweep that found the
// tree. ok is false where there is no measurement: no sampler is bound yet,
// this platform has none, or no sweep has yet found a member of the child's
// group. Once the tree has been seen the last figure is kept, including after
// the child exits. The value is a change signal, never a duration -- a caller
// compares it with the previous reading and asks only whether it moved.
func (p *CPUProgress) Ticks() (int64, bool) {
	if p == nil {
		return 0, false
	}
	return p.sampler.Load().cpuTicks()
}

// AnonResidentBytes reports the child tree's summed anonymous resident set --
// what it holds of its own, not the executable pages it shares -- as of the
// last sweep that found the tree. ok is false where there is no measurement:
// no sampler is bound yet, this platform has none, or the last sweep that
// found the tree could not read it for every member. A caller that adds it to
// a sum leaves an unavailable figure out, never counts it as zero.
func (p *CPUProgress) AnonResidentBytes() (int64, bool) {
	if p == nil {
		return 0, false
	}
	return p.sampler.Load().anonBytes()
}

// bind publishes the run's tree sampler. A nil handle is the ordinary case --
// most callers want no live signal -- and binds nothing.
func (p *CPUProgress) bind(s *treeSampler) {
	if p == nil {
		return
	}
	p.sampler.Store(s)
}
