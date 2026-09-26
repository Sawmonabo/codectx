package flow

// Arena is the reset slab arena one worker uses for every function it
// analyses. The zero value is ready to use. It is not safe for concurrent
// use: one Arena per worker goroutine.
//
// Backing is pointer-free and typed only — int32, uint32 and uint64 slabs,
// plus the builder's construction scratch lists whose elements hold no
// pointer — so the collector never scans it. There is no preallocation
// constant: backing grows to the need of the functions seen.
//
// Lifetime: every slice, Graph, Edges and PostDom handed out is valid until
// the next Begin, which reclaims all of it.
//
// Release rule, applied at Begin: if the previous function used more than
// 1 MiB (Bytes + ScratchBytes), every backing array is dropped, so one large
// function does not set the worker's footprint for the rest of the run.
// Otherwise backing is kept; a previous function that spilled across several
// slabs has them consolidated into one slab of their total, so a worker in
// steady state allocates nothing per function.
type Arena struct {
	// b is the one Builder, reset by every Begin.
	b Builder
}

// releaseBytes is the previous-function usage above which Begin drops every
// backing array (Bytes + ScratchBytes).
const releaseBytes = 1 << 20

// Begin reclaims everything handed out since the previous Begin (applying the
// release rule), starts one function spanning fn and returns its Builder.
// The Builder starts with Entry (span fn) and Exit (span {fn.End, fn.End})
// created and the current fringe {Entry}; see Builder.
func (a *Arena) Begin(fn Span) *Builder {
	panic("flow: Arena.Begin is not implemented")
}

// Int32s returns n zeroed int32s from the slabs, counted in Bytes. The slice's
// capacity equals its length (a full slice expression), so an append copies
// rather than writing into a neighbouring allocation.
func (a *Arena) Int32s(n int) []int32 {
	panic("flow: Arena.Int32s is not implemented")
}

// Uint64s returns n zeroed uint64s from the slabs, counted in Bytes, with
// capacity equal to length as Int32s.
func (a *Arena) Uint64s(n int) []uint64 {
	panic("flow: Arena.Uint64s is not implemented")
}

// Bytes is the slab bytes handed out since Begin: the two CSRs Builder.Finish
// builds plus every analysis structure (reverse post-order, dominator arrays,
// augmentation and strongly-connected-component scratch, frontiers, SSA
// values and operands, worklists, emitted pairs). These are exactly the
// structures bounded by 96·N + 64 bytes; the benchmarks report the ratio.
func (a *Arena) Bytes() int {
	panic("flow: Arena.Bytes is not implemented")
}

// ScratchBytes is the construction scratch used since Begin: node records,
// raw edges, uses, variable spans, fringe and frame stacks, labels and
// pending gotos. The Graph's node metadata and uses are read from it. It is
// outside the 96·N + 64 bound and reported separately.
func (a *Arena) ScratchBytes() int {
	panic("flow: Arena.ScratchBytes is not implemented")
}

// Retained is the backing capacity, in bytes, the arena currently holds
// across slabs and scratch, whether or not it is in use.
func (a *Arena) Retained() int {
	panic("flow: Arena.Retained is not implemented")
}
