package flow

// Arena is the reset slab arena one worker uses for every function it
// analyses. The zero value is ready to use. It is not safe for concurrent
// use: one Arena per worker goroutine.
//
// Backing is pointer-free and typed only — int32 and uint64 slabs, plus the
// builder's construction scratch lists — so the collector scans none of it
// but the one pointer-bearing exception, the builder's short list of label
// names, which are strings. There is no preallocation constant: a slab that cannot serve a request is
// replaced by one of at least twice its size and at least the request, so
// backing grows to the need of the functions seen.
//
// Lifetime: every slice, Graph, Edges and PostDom handed out is valid until
// the next Begin, which reclaims all of it.
//
// Release rule, applied at Begin: if the previous function used more than
// 1 MiB (Bytes + ScratchBytes), every backing array is dropped, so one large
// function does not set the worker's footprint for the rest of the run.
// Otherwise backing is kept; a previous function that spilled across several
// slabs has them consolidated into one slab of everything it was handed, so
// a worker in steady state allocates nothing per function.
type Arena struct {
	// b is the one Builder, reset by every Begin.
	b Builder
	// i32 and u64 back Int32s and Uint64s.
	i32 slab[int32]
	u64 slab[uint64]
}

// releaseBytes is the previous-function usage above which Begin drops every
// backing array (Bytes + ScratchBytes).
const releaseBytes = 1 << 20

// Begin reclaims everything handed out since the previous Begin (applying the
// release rule), starts one function spanning fn and returns its Builder.
// The Builder starts with Entry (span fn) and Exit (span {fn.End, fn.End})
// created and the current fringe {Entry}; see Builder.
func (a *Arena) Begin(fn Span) *Builder {
	if a.Bytes()+a.ScratchBytes() > releaseBytes {
		a.i32 = slab[int32]{}
		a.u64 = slab[uint64]{}
		a.b.release()
	} else {
		a.i32.recycle()
		a.u64.recycle()
	}
	a.b.reset(a, fn)
	return &a.b
}

// Int32s returns n zeroed int32s from the slabs, counted in Bytes. The slice's
// capacity equals its length (a full slice expression), so an append copies
// rather than writing into a neighbouring allocation.
func (a *Arena) Int32s(n int) []int32 { return a.i32.take(n) }

// Uint64s returns n zeroed uint64s from the slabs, counted in Bytes, with
// capacity equal to length as Int32s.
func (a *Arena) Uint64s(n int) []uint64 { return a.u64.take(n) }

// Bytes is the slab bytes handed out since Begin: the two CSRs Builder.Finish
// builds plus every analysis structure (reverse post-order, dominator arrays,
// augmentation and strongly-connected-component scratch, frontiers, SSA
// values and operands, worklists, emitted pairs). These are exactly the
// structures bounded by 96·N + 64 bytes; the benchmarks report the ratio.
// A structure grown by need (a list32, a list64, the SSA memo table) counts
// every array it outgrew, since each stays allocated until the next Begin.
func (a *Arena) Bytes() int { return 4*a.i32.handed() + 8*a.u64.handed() }

// ScratchBytes is the construction scratch used since Begin: node records,
// raw edges, uses, variable spans, fringe and frame stacks, labels and
// pending gotos. The Graph's node metadata and uses are read from it. It is
// outside the 96·N + 64 bound and reported separately.
func (a *Arena) ScratchBytes() int { return a.b.scratchBytes() }

// Retained is the backing capacity, in bytes, the arena currently holds
// across slabs and scratch, whether or not it is in use.
func (a *Arena) Retained() int {
	return 4*a.i32.capacity() + 8*a.u64.capacity() + a.b.scratchRetained()
}

// slab is one element type's backing: a current array handed out front to
// back, plus the count of arrays it replaced since Begin. A replaced array
// is no longer referenced by the arena, but the views handed out from it keep
// it allocated until the next Begin, so its size stays in the accounting.
type slab[T int32 | uint64] struct {
	buf []T
	// used is the elements of buf handed out since Begin.
	used int
	// spilled and spilledCap are the elements handed out from, and the
	// capacity of, the arrays buf replaced since Begin.
	spilled, spilledCap int
}

// take hands out n zeroed elements. buf is zeroed up to its hand-out point
// by recycle and a new array is zeroed by the runtime, so take clears
// nothing itself.
func (s *slab[T]) take(n int) []T {
	if n > len(s.buf)-s.used {
		s.spilled += s.used
		s.spilledCap += len(s.buf)
		s.buf = make([]T, max(n, 2*len(s.buf)))
		s.used = 0
	}
	lo, hi := s.used, s.used+n
	s.used = hi
	return s.buf[lo:hi:hi]
}

// recycle reclaims everything handed out, keeping the backing. A function
// that spilled gets one array of everything it was handed, so the same
// function next time is served from one slab without spilling; otherwise the
// handed-out prefix is zeroed for reuse.
func (s *slab[T]) recycle() {
	if s.spilledCap > 0 {
		s.buf = make([]T, s.spilled+s.used)
	} else {
		clear(s.buf[:s.used])
	}
	s.used, s.spilled, s.spilledCap = 0, 0, 0
}

// handed is the element count handed out since Begin.
func (s *slab[T]) handed() int { return s.spilled + s.used }

// capacity is the element capacity allocated for the current function: the
// current array plus the arrays it replaced.
func (s *slab[T]) capacity() int { return len(s.buf) + s.spilledCap }

// list32 is an arena-backed int32 list grown by need: when full it moves to
// an arena array of twice its capacity (at least one element). The outgrown
// array stays counted in Bytes.
type list32 struct{ s []int32 }

func (l *list32) push(a *Arena, v int32) {
	if len(l.s) == cap(l.s) {
		l.s = grow(a.Int32s(max(1, 2*cap(l.s))), l.s)
	}
	l.s = append(l.s, v)
}

// list64 is list32 for uint64.
type list64 struct{ s []uint64 }

func (l *list64) push(a *Arena, v uint64) {
	if len(l.s) == cap(l.s) {
		l.s = grow(a.Uint64s(max(1, 2*cap(l.s))), l.s)
	}
	l.s = append(l.s, v)
}

// grow copies old into the front of the fresh array to and returns it with
// old's length and to's capacity.
func grow[T int32 | uint64](to, old []T) []T { return to[:copy(to, old)] }
