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
// Membership against the spooled remainder is answered in BATCH, one level at
// a time (the external-memory BFS duplicate-elimination of Munagala and Ranade,
// "I/O-complexity of graph algorithms", SODA 1999: sort the level's candidates
// and sweep the visited set once, instead of seeking per node). The spool is
// a forward-only stream, so a per-node probe would be a full scan per node;
// one scan per LEVEL is what keeps the I/O the same order the spool copy
// already costs, and the scan stops as soon as the level's last candidate is
// answered rather than always running to the end of the spool. Levels per page are bounded by the page item ceiling, because
// a level that yields no row ends the walk and one that yields rows spends
// page items.
//
// Sources consulted: Munagala/Ranade (SODA 1999) for the level-batched
// duplicate elimination; Mehlhorn/Meyer, "External-memory breadth-first search
// with sublinear I/O" (ESA 2002) for why the semi-external variant keeps only
// a bounded front in RAM.

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
		want[id] = struct{}{}
	}
	if len(want) == 0 {
		return nil
	}
	// The sweep ENDS as soon as every candidate is answered: a level whose
	// neighbours were all admitted earlier costs the prefix of the spool that
	// holds them, not the whole cumulative set. Only the unanswered remainder
	// costs a full pass, and that is the case where a full pass is the answer
	// ("none of these was admitted"), so no scan reads further than the fact it
	// is looking for.
	//
	// Batching SEVERAL levels into one sweep -- the other half of the finding
	// -- is not available to a BFS: level n+1's candidates are the neighbours
	// of the nodes level n admits, so they do not exist until level n's sweep
	// has already answered. Supplying them would mean reading level n+1's edges
	// before level n was admitted, which is a second pass over the edge rows
	// and a second frontier in heap -- strictly worse than the pass it saves.
	found := 0
	err := v.stream(ctx, func(id model.NodeID) error {
		if _, ok := want[id]; !ok {
			return nil
		}
		if _, seen := v.probed[id]; seen {
			// The spool may hold a node twice (a carried record and the
			// visited record the issuing page wrote); counting it twice would
			// end the sweep before every candidate was answered.
			return nil
		}
		v.probed[id] = struct{}{}
		found++
		if found == len(want) {
			return errWarmComplete
		}
		return nil
	})
	if errors.Is(err, errWarmComplete) {
		return nil
	}
	return err
}

// errWarmComplete ends a membership sweep that has answered every candidate it
// was given. It never leaves warm: the stream is a forward-only replay with no
// state of its own beyond the open spool, which the caller closes either way,
// so stopping early is indistinguishable from reaching the end.
var errWarmComplete = errors.New("membership sweep answered every candidate")

// newlyAdmitted is what this page must append to the fresh spool, in the frozen
// NodeID order. The carried nodes are deliberately absent: they are copied
// forward from the previous spool instead, so no node is written twice.
func (v *visitedSet) newlyAdmitted() []model.NodeID {
	out := make([]model.NodeID, 0, len(v.added))
	for id := range v.added {
		out = append(out, id)
	}
	sortNodeIDs(out)
	return out
}

// sortNodeIDs puts node ids in the frozen ascending order the spool and the
// emission order share.
func sortNodeIDs(ids []model.NodeID) {
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
}
