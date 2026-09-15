package graph

import (
	"context"
	"errors"
	"sort"

	"github.com/Sawmonabo/codectx/internal/model"
)

// This file is the traversal's cumulative admitted-node set.
//
// A walk must admit every node at most once, so it needs a membership test
// over every node admitted so far -- cumulatively, across every page of the
// continuation chain. Holding that as a map was the ONE structure on this path
// still sized by the walk rather than by the page: a repository-sized BFS
// carried a repository-sized map in heap on every page, which is the OOM the
// scale posture forbids. (`model.NodeID` is a 64-hex content hash, so a
// bitmap is not available -- there is no dense id space to compress.)
//
// The authoritative set therefore lives where the frontier already lives: the
// continuation spool on disk. Heap holds only a bounded FRONT:
//
//   - carried: the nodes of the resumed frontier. Bounded by Limits.FrontierBytes,
//     and already resident -- these are the frontier records themselves.
//   - added: the nodes THIS page admitted. Every admission costs a visited
//     call, and a page stops at max_page_items relations, so this is bounded
//     by the page, not by the graph.
//   - probed: the answers for ONE level's candidate neighbours, cleared at
//     each level. Bounded by Limits.FrontierBytes, the same ceiling that
//     bounds the level's edge rows.
//
// Membership against the spooled remainder is answered in two steps, and
// neither is a scan of the whole set.
//
// First, a Bloom filter over that remainder (visitedFilter, below) is built
// once per page while the resume replay is already decoding every spool record
// to rebuild the frontier. A filter has no false negatives, so a miss PROVES
// the spool cannot hold the node and the level never opens it. That is exactly
// the pathological case: a chain -- and a call chain is one -- reaches freshly
// admitted nodes at every level, so the old scan could never exit early and a
// page paid one full pass per level, up to max_page_items of them.
//
// Second, what the filter cannot rule out is answered in BATCH, one level at a
// time (the external-memory BFS duplicate-elimination of Munagala and Ranade,
// "I/O-complexity of graph algorithms", SODA 1999: sort the level's candidates
// and sweep the visited set once, instead of seeking per node). The spool's
// visited section is itself written in ascending NodeID order, so that sweep is
// a MERGE-JOIN that ends at the first key past the largest candidate rather
// than at the end of the set. Levels per page are bounded by the page item
// ceiling, because a level that yields no row ends the walk and one that yields
// rows spends page items.
//
// Sources consulted: Munagala/Ranade (SODA 1999) for the level-batched
// duplicate elimination; Mehlhorn/Meyer, "External-memory breadth-first search
// with sublinear I/O" (ESA 2002) for why the semi-external variant keeps only
// a bounded front in RAM; Kirsch/Mitzenmacher (ESA 2006) for the two-hash
// filter probes.

// visitedStream replays every node of the spooled cumulative set, in the order
// the spool holds them. It is nil on the first page of a walk, where the whole
// set is the front.
type visitedStream func(ctx context.Context, fn func(model.NodeID) error) error

// visitedSet is the cumulative admitted-node set of one traversal: a bounded
// heap front over an authoritative, disk-resident remainder.
type visitedSet struct {
	// carried is the resumed frontier's nodes. They are already ON the spool
	// (the issuing page wrote a frontier record for each), so they are
	// answered from heap but never re-written by the next spill.
	carried map[model.NodeID]struct{}
	// added is what this page admitted. The next spill appends exactly these.
	added map[model.NodeID]struct{}
	// probed is one level's answers from the spooled remainder. warm rebuilds
	// it per level, so it never accumulates across a page.
	probed map[model.NodeID]struct{}
	stream visitedStream
	// filter summarizes what stream holds, so a level whose candidates are all
	// new is answered from heap without opening the spool at all. It is nil on
	// a first page and whenever no budget was available to build one; a nil
	// filter is simply no information, never a wrong answer.
	filter *visitedFilter
}

// newVisitedSet builds the set over stream, which may be nil on a first page.
func newVisitedSet(stream visitedStream) *visitedSet {
	return &visitedSet{
		carried: map[model.NodeID]struct{}{},
		added:   map[model.NodeID]struct{}{},
		probed:  map[model.NodeID]struct{}{},
		stream:  stream,
	}
}

// carry records a node of the resumed frontier: admitted by an earlier page and
// already on that page's spool, so this page must neither re-admit it nor
// write it again.
func (v *visitedSet) carry(id model.NodeID) { v.carried[id] = struct{}{} }

// add records a node this page admitted.
func (v *visitedSet) add(id model.NodeID) { v.added[id] = struct{}{} }

// has answers membership. It is authoritative ONLY for a node that is in the
// front or was named to the warm call that prepared the current level -- which
// is every node the walk tests, because a candidate is always a neighbour of
// the level being expanded.
func (v *visitedSet) has(id model.NodeID) bool {
	if _, ok := v.carried[id]; ok {
		return true
	}
	if _, ok := v.added[id]; ok {
		return true
	}
	_, ok := v.probed[id]
	return ok
}

// warm answers, in one sequential pass over the spooled remainder, which of
// this level's candidate neighbours were already admitted by an earlier page.
// The previous level's answers are dropped first: they are not needed again,
// and keeping them would make the front grow with the walk.
func (v *visitedSet) warm(ctx context.Context, candidates []model.NodeID) error {
	clear(v.probed)
	if v.stream == nil || len(candidates) == 0 {
		return nil
	}
	want := make(map[model.NodeID]struct{}, len(candidates))
	for _, id := range candidates {
		if v.has(id) {
			// Answered from the front; the spool cannot change the answer.
			continue
		}
		if !v.filter.mayHold(id) {
			// PROVED absent by the page's membership summary: a Bloom filter
			// has no false negatives, so the spool cannot hold this node and
			// sweeping for it would read the whole spool to learn nothing.
			// This is the chain-shaped level -- every candidate freshly
			// reached -- which is the case that used to cost a full pass per
			// level.
			continue
		}
		want[id] = struct{}{}
	}
	if len(want) == 0 {
		// Every candidate was answered from heap: the level costs no I/O.
		return nil
	}
	// The sweep is a MERGE-JOIN, not a scan: the candidates are sorted once and
	// answered in one ordered pass over the stream, which skips past every
	// candidate the stream has already gone by instead of probing for it.
	//
	// The stream is a CONCATENATION of ascending blocks, not one ascending
	// sequence: a cursor's spool is written in ascending NodeID order, but a
	// walk chained in process appends one ascending block per internal link and
	// a resumed walk replays the cursor's spool and then those blocks
	// (walkrun.go). A join that assumed one global order would advance past a
	// candidate in the first block and never look back, reporting a node the
	// walk HAS admitted as absent -- re-admitting it on a later page and
	// reporting the same entity twice. So a key below its predecessor is read
	// for what it is, the start of the next ascending run, and the join
	// restarts at the smallest unanswered candidate. Per run the cost is the
	// merge-join's; the pass ends early only when every candidate is answered,
	// because a run that has passed the largest candidate says nothing about
	// the runs that follow it.
	//
	// Batching SEVERAL levels into one sweep -- the other half of the finding
	// -- is not available to a BFS: level n+1's candidates are the neighbours
	// of the nodes level n admits, so they do not exist until level n's sweep
	// has already answered. Supplying them would mean reading level n+1's edges
	// before level n was admitted, which is a second pass over the edge rows
	// and a second frontier in heap -- strictly worse than the pass it saves.
	sorted := make([]model.NodeID, 0, len(want))
	for id := range want {
		sorted = append(sorted, id)
	}
	sortNodeIDs(sorted)
	next, found, started := 0, 0, false
	var prev model.NodeID
	err := v.stream(ctx, func(id model.NodeID) error {
		if !started || id < prev {
			next = 0
		}
		prev, started = id, true
		for next < len(sorted) && sorted[next] < id {
			// No record of THIS run can answer this candidate any more: the run
			// is ascending and has passed it.
			next++
		}
		if next >= len(sorted) {
			return nil
		}
		if sorted[next] == id {
			if _, seen := v.probed[id]; !seen {
				v.probed[id] = struct{}{}
				found++
			}
			next++
			if found == len(sorted) {
				return errWarmComplete
			}
		}
		return nil
	})
	if errors.Is(err, errWarmComplete) {
		return nil
	}
	return err
}

// errWarmComplete ends a membership sweep that has answered every candidate it
// was given, or has passed the last key that could still answer one. It never
// leaves warm: the stream is a forward-only replay with no state of its own
// beyond the open spool, which the caller closes either way, so stopping early
// is indistinguishable from reaching the end.
var errWarmComplete = errors.New("membership sweep answered every candidate")

// spillable is what this page must contribute to the fresh spool's visited
// SECTION, ascending: the nodes it admitted itself, plus the frontier it
// resumed. The carried nodes belong here because the stream that copies the
// previous spool forward replays that spool's visited section only -- its
// frontier records are answered from this page's front instead (cursor.go), so
// nothing else would carry them forward. Both halves are bounded by the page
// and by the frontier ceiling, never by the walk, and the previous visited
// section stays on disk.
func (v *visitedSet) spillable() []model.NodeID {
	out := make([]model.NodeID, 0, len(v.added)+len(v.carried))
	for id := range v.added {
		out = append(out, id)
	}
	for id := range v.carried {
		out = append(out, id)
	}
	sortNodeIDs(out)
	return out
}

// newlyAdmitted is the name the paging endpoints call spillable by. It is kept
// so the impact and rollup continuations compile unchanged.
func (v *visitedSet) newlyAdmitted() []model.NodeID { return v.spillable() }

// sortNodeIDs puts node ids in the frozen ascending order the spool and the
// emission order share.
func sortNodeIDs(ids []model.NodeID) {
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
}

// visitedFilter is the membership SUMMARY of the spooled cumulative set: a
// Bloom filter built once per page, while the resume replay is already
// decoding every spool record to rebuild the frontier, and consulted before
// any level opens the spool again.
//
// It exists because the sweep it guards is the wrong shape for the one graph
// every call graph is: a chain. A level of freshly reached nodes finds none of
// its candidates on the spool, so no early exit can fire and the level pays a
// full pass; a page may hold as many levels as it holds page items, so a
// chain-shaped page paid levels x |visited| record decodes. A Bloom filter has
// no false negatives, so "this node is not in the filter" is a PROOF that the
// spool does not hold it -- which is exactly the answer the pathological level
// needs, and it costs k memory probes and no I/O. A false positive costs the
// sweep that would have run anyway, so the filter can never change an answer,
// only the work spent reaching it.
//
// Peak heap is a function of the configured per-walk memory ceiling, never of
// the walk: the filter is sized from the cumulative admitted count the cursor
// already carries and then CLAMPED to a fraction of Limits.FrontierBytes. A
// walk larger than the clamp allows simply gets a denser filter -- more false
// positives, more sweeps -- and never a refusal, a truncation or an unbounded
// allocation.
type visitedFilter struct {
	bits []uint64
	m    uint64 // bit count, always a positive multiple of 64
	k    uint32 // probes per key
}

// visitedFilterBitsPerNode is the target filter density. Sixteen bits per node
// puts the false-positive rate near 1 in 2 000 at k=11, which turns a level of
// fresh nodes into zero sweeps rather than one per level; it costs two bytes
// per admitted node, against the ~70 bytes a node already costs on the spool.
const visitedFilterBitsPerNode = 16

// visitedFilterBudgetShare is the fraction of Limits.FrontierBytes the filter
// may claim. The frontier ceiling is the walk's own memory budget, so taking an
// eighth of it keeps the summary strictly smaller than the level it summarizes
// while leaving the frontier the bytes it was given.
const visitedFilterBudgetShare = 8

// newVisitedFilter sizes a filter for estimate nodes within maxBytes of heap.
// It returns nil when there is nothing to summarize or no budget to do it in;
// a nil filter answers "may hold" for everything, which is the same behaviour
// as having no filter at all.
func newVisitedFilter(estimate, maxBytes int64) *visitedFilter {
	if estimate <= 0 || maxBytes <= 0 {
		return nil
	}
	words := (estimate*visitedFilterBitsPerNode + 63) / 64
	if max := maxBytes / 8; words > max {
		words = max
	}
	if words <= 0 {
		return nil
	}
	f := &visitedFilter{bits: make([]uint64, words), m: uint64(words) * 64}
	// k = ln2 * m/n, clamped: one probe is the floor, and past sixteen the
	// probes cost more than the false positives they remove.
	k := (f.m * 693) / (uint64(estimate) * 1000)
	if k < 1 {
		k = 1
	}
	if k > 16 {
		k = 16
	}
	f.k = uint32(k)
	return f
}

// add records that the spooled set holds id.
func (f *visitedFilter) add(id model.NodeID) {
	if f == nil {
		return
	}
	h1, h2 := visitedFilterHash(id)
	for i := uint32(0); i < f.k; i++ {
		b := (h1 + uint64(i)*h2) % f.m
		f.bits[b/64] |= 1 << (b % 64)
	}
}

// mayHold reports whether the spooled set COULD hold id. False is definitive --
// Bloom filters have no false negatives -- so a false answer lets a level skip
// the spool entirely. A nil filter has no information and says true.
func (f *visitedFilter) mayHold(id model.NodeID) bool {
	if f == nil {
		return true
	}
	h1, h2 := visitedFilterHash(id)
	for i := uint32(0); i < f.k; i++ {
		b := (h1 + uint64(i)*h2) % f.m
		if f.bits[b/64]&(1<<(b%64)) == 0 {
			return false
		}
	}
	return true
}

// visitedFilterHash derives the two independent hashes the probes are built
// from (Kirsch and Mitzenmacher, "Less hashing, same performance", ESA 2006:
// k probes of h1+i*h2 are as good as k independent hashes). FNV-1a over the
// whole id, not a prefix parse: a NodeID is a 64-hex content hash in
// production but the type is a string and nothing enforces hex.
func visitedFilterHash(id model.NodeID) (uint64, uint64) {
	const (
		offset64 = 14695981039346656037
		prime64  = 1099511628211
	)
	h := uint64(offset64)
	for i := 0; i < len(id); i++ {
		h ^= uint64(id[i])
		h *= prime64
	}
	// A second, decorrelated hash from the first by the SplitMix64 finalizer;
	// it is forced odd so that h1+i*h2 walks every residue class of m.
	g := h
	g ^= g >> 30
	g *= 0xbf58476d1ce4e5b9
	g ^= g >> 27
	g *= 0x94d049bb133111eb
	g ^= g >> 31
	return h, g | 1
}
