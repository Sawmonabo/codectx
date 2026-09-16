package graph

import (
	"container/list"
	"context"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strconv"

	"github.com/Sawmonabo/codectx/internal/config"
	"github.com/Sawmonabo/codectx/internal/model"
	"github.com/Sawmonabo/codectx/internal/pagination"
)

// Truncation reasons. Each names the exact bound that stopped the walk, so a
// caller can tell "you asked for fewer items" from "this workspace is bigger
// than your budget" from "the facts are not built yet". A truncated answer
// always carries one of these; an answer that silently stopped is a defect.
const (
	reasonEdgeBudget    = "edge budget exhausted"
	reasonVisitedBudget = "visited node budget exhausted"
	reasonPageFull      = "page item limit reached"
	reasonDependence    = "dependence units are still building"
	// reasonDepth is the depth bound. Without it the loop falls out with a
	// live frontier and Truncated=false, so a depth-limited answer reads as a
	// complete one -- the one stop that would report nothing at all.
	reasonDepth = "graph depth budget exhausted"
)

// The per-request query deadline reuses impact.go's reasonDeadline: it is the
// same bound with the same name, and for a PAGED traversal it ends a page
// rather than an answer -- the edges already read are returned and the frontier
// becomes a continuation, instead of the whole page being thrown away with a
// bare CTX_QUERY_DEADLINE.

// deadlineStop reports whether err is the walk's own deadline AND this page has
// something to hand back. A cancellation is never a stop (the caller is gone
// and wants nothing). What counts as "something" depends on the walk: a PAGED
// traversal must have admitted an edge, because a cursor over an empty page it
// did not advance would let a client retry into a chain that never returns a
// row -- and it loses nothing by failing, since the caller still holds the
// cursor it arrived with. A walk that persists its progress outside the page
// (DeadlineResumesEmptyPage) always has the standing frontier and the records
// its earlier legs retained to hand back, so every deadline ends its page.
func deadlineStop(err error, o expandOptions) bool {
	if !o.DeadlineStops {
		return false
	}
	if o.Budget.pageEdges == 0 && !o.DeadlineResumesEmptyPage {
		return false
	}
	// isDeadline, not a typed-only test: a deadline that fell inside a STORAGE
	// read arrives as the context's own untyped error and ends the page exactly
	// as the engine's own typed one does.
	return isDeadline(err)
}

// edgeRowOverheadBytes is the same estimate for a row that still carries
// CANONICAL identifiers: the shortest-path scratch keeps model.Relation rows,
// so its per-row cost is this fixed part plus the lengths of the ids it holds.
const edgeRowOverheadBytes = 256

// edgeScanCheckEvery is how often one level's scan re-reads the clock. A check
// per entry would put a time.Now on the hot path of every decoded adjacency
// entry; a check only between levels would let one hub outlive the deadline by
// the whole of its fan-out. Every 256 entries bounds the overrun by 256
// decodes.
const edgeScanCheckEvery = 256

// levelState is the state one BFS level is in. A level is COLLECTED out of the
// adjacency of the frontier before it, then SERVED to the visitor out of the
// sorted run the transition wrote; those are the only two states a
// continuation can resume into (ADR-0005 Decision 2, docs/queries.md).
type levelState string

const (
	levelCollecting levelState = "collecting"
	levelServing    levelState = "serving"
)

// levelResolveBatch is how many collected entries wait for their canonical ids
// at once, and levelServeBatch how many served records are named at once. Both
// are INTERNAL constants and not user limits: they bound the resident buffer
// and the size of one batched primary-key read, never the work a walk may do or
// the answer it returns. A chunk of frontier owners can deliver millions of
// entries, so resolving "per chunk" without a bound would put the chunk's whole
// fan-out in heap.
const (
	levelResolveBatch = 4096
	levelServeBatch   = 512
)

// routeCacheEntries bounds the ref -> canonical relation id cache a SERVING
// page resolves its ROUTES through. A route element is the admitting relation
// of some earlier level, so the cache is fed by the records the walk has
// already served and is hit rather than read in the common case. It is an
// internal constant: the cache is a memory ceiling, and a walk whose routes do
// not fit in it pays one batched read per page instead of holding a relation
// name table sized by the walk.
const routeCacheEntries = 65536

// errLevelCut ends the streaming of a level -- the frontier chunks of a collect
// or the records of a serve -- because the page stopped. It never escapes the
// function that raises it.
var errLevelCut = errors.New("graph: the level was cut short by the page")

// expand is the ONE batched BFS. visit is called once per emitted edge in the
// canonical (neighbour, owner, relation) order the level was sorted into, and
// may return errStopExpansion to end the walk, which expand reports as a clean
// finish: the visitor stopped deliberately and owns whatever truncation flag it
// set.
//
// One LEVEL is one pipeline, and nothing per-level is ever held whole in heap:
//
//   - COLLECTING streams the frontier admitted at the level before it in
//     chunks, makes ONE GraphReader.Neighbours call per chunk, keeps the
//     entries the direction dedup rule admits, resolves their canonical ids in
//     batches and appends them to the level's run -- resident while it fits the
//     frontier byte ceiling, spilled beyond it.
//   - the TRANSITION sorts that run by the canonical key, writes it once, and
//     commits the level: admitted states first, then the visited bits, then the
//     frontier bits for the level after it.
//   - SERVING hands the sorted run to the visitor from a byte offset, so a page
//     costs one seek and the records it takes.
//
// It enforces only the bounds it is given -- MaxDepth, the deadline and
// cancellation. MaxVisited and MaxEdges are deliberately absent from
// expandOptions: budget carries the CUMULATIVE, cursor-carried counts already
// spent, and the caller resolves the caps from Limits and the request, so the
// visitor is the one place that compares the two. That keeps a resumed page
// from spending a fresh budget.
func expand(ctx context.Context, r GraphReader, seeds []model.NodeID, o expandOptions,
	visit func(frontierState, Edge) error) (walkState, error) {
	switch {
	case r == nil:
		// The typed refusal the engine owes a caller that has not wired the
		// packed reader yet. It names the missing port rather than failing
		// somewhere inside the scan, where the cause would be invisible.
		return walkState{}, (&model.Error{Code: model.CodeInternal,
			Message: "graph traversal requires the packed graph reader"}).WithDetail("operation", "expand")
	case o.Budget == nil:
		return walkState{}, (&model.Error{Code: model.CodeInternal,
			Message: "graph expansion requires a budget"}).WithDetail("operation", "expand")
	case o.Retain == nil:
		return walkState{}, (&model.Error{Code: model.CodeInternal,
			Message: "graph expansion requires a retained walk directory"}).WithDetail("operation", "expand")
	case visit == nil:
		return walkState{}, (&model.Error{Code: model.CodeInternal,
			Message: "graph expansion requires a visitor"}).WithDetail("operation", "expand")
	}
	codes, costs, err := walkKinds(r, o.Kinds)
	if errors.Is(err, errNoSuchKinds) {
		// Every relation kind the request named is absent from this
		// generation's dictionary, so the walk selects nothing. That is an
		// ANSWER of nothing, not a failure: a generation built before an
		// analyzer existed simply has none of its edges, and the walk reports
		// the empty result with no truncation rather than refusing the query.
		return walkState{}, nil
	}
	if err != nil {
		return walkState{}, err
	}
	w := &levelWalk{reader: r, o: o, codes: codes, costs: costs, visit: visit,
		routes: newRouteNames(routeCacheEntries)}
	st := walkState{Level: 1, LevelState: levelCollecting}
	if o.Resume == nil {
		if err := w.seed(ctx, seeds); err != nil {
			return walkState{}, err
		}
	} else {
		// A resume never re-enters the seeds: they are already in the bitset
		// the issuing page committed, and re-admitting them would spend the
		// cumulative visited budget a second time for the same nodes. The
		// frontier is the admitted file of the level before the one being
		// collected, which the retained directory already holds.
		c := o.Resume.Cursor
		st = walkState{Level: c.Level, LevelState: c.LevelState, LevelPos: c.LevelPos,
			RawBytes: c.RawBytes, LevelOffset: c.LevelOffset}
		w.entryLevel = c.Level
		switch c.LevelState {
		case levelServing:
			// The sorted run this leg serves out of.
			o.Retain.hold(levelFileName(sortedLevelPrefix, c.Level))
			if err := o.Retain.alignFrontier(c.Level); err != nil {
				return walkState{}, err
			}
		case levelCollecting:
			// The run collected so far, and the frontier the scan reads -- and
			// the sorted run of this level, which exists only if this cursor
			// has been presented before and its transition committed. A page
			// that committed it and then failed retryably sends the caller
			// back to THIS cursor, and collect below serves that run rather
			// than collecting the level again; a walk that had released it
			// would answer CTX_STORAGE_CORRUPT. Where the transition has not
			// run, the name holds nothing and releaseHeld removes nothing.
			o.Retain.hold(levelFileName(rawLevelPrefix, c.Level))
			o.Retain.hold(levelFileName(admittedLevelPrefix, c.Level-1))
			o.Retain.hold(levelFileName(sortedLevelPrefix, c.Level))
		}
	}
	return w.run(ctx, st)
}

// levelWalk is one request's leg of a walk: the reader it is pinned to, the
// options it runs under, and the two pieces of state a level pipeline carries
// between its stages.
type levelWalk struct {
	reader GraphReader
	o      expandOptions
	codes  []KindCode
	costs  kindCosts
	visit  func(frontierState, Edge) error
	// sorted is the level currently being served, held open across the pages of
	// one request's serve so a page does not reopen and re-count it.
	sorted *sortedLevel
	// routes names the relations a served record's ROUTE holds.
	routes *routeNames
	// resumedCounted marks that the leg's FIRST collected level -- the only one
	// a resume picks up rather than starts -- has been measured.
	resumedCounted bool
	// group is the neighbour group the serve is inside, and groupOpen whether
	// there is one. A group is counted against the visited budget when it ENDS,
	// so a page cut inside one leaves it to the page that finishes it: counting
	// at the START would count it again on that page.
	group     NodeRef
	groupID   model.NodeID
	groupOpen bool
	// entryLevel is the level this leg was RESUMED into, or zero. The files the
	// cursor that named it resumes from are held for the whole page: a
	// RETRYABLE failure later in this page tells the caller to present that
	// same cursor again, and a walk that had already deleted them would answer
	// CTX_STORAGE_CORRUPT -- or, where the deleted file is a frontier, report
	// itself exhausted and serve a fraction of the answer as the whole of it.
	entryLevel int
}

// run drives the pipeline until the walk stops: it is exhausted, it reached the
// depth bound, or the page ended inside one of the two states.
func (w *levelWalk) run(ctx context.Context, st walkState) (walkState, error) {
	for {
		if st.LevelState == levelCollecting {
			stop, err := w.collect(ctx, &st)
			if err != nil {
				return walkState{}, err
			}
			if stop {
				return st, nil
			}
		}
		stop, err := w.serve(ctx, &st)
		if err != nil {
			return walkState{}, err
		}
		if stop {
			return st, nil
		}
	}
}

// seed admits the request's seeds as level 0.
//
// Seeds enter in request order, de-duplicated, then sorted, so two requests
// naming the same seeds differently answer identically. A seed the generation
// cannot see resolves to 0: it has no adjacency and no bit, so it expands to
// nothing -- exactly what an unknown node did before, which is why it is still
// COUNTED against the visited budget and the disclosed visited_count.
func (w *levelWalk) seed(ctx context.Context, seeds []model.NodeID) error {
	ids := dedupeNodes(seeds)
	refs, err := w.reader.Resolve(ctx, ids)
	if err != nil {
		return err
	}
	if len(refs) != len(ids) {
		return internalErr("graph: the reader resolved a different number of seeds than it was given")
	}
	level := make([]frontierState, 0, len(refs))
	for _, ref := range refs {
		w.o.Budget.visited++
		w.o.Budget.pageVisited++
		if ref == 0 {
			continue
		}
		level = append(level, frontierState{Node: ref})
	}
	sortFrontier(level)
	return w.o.Retain.commitSeeds(level)
}

// collect runs one level's COLLECTING state and, when the level completes, its
// transition. It reports whether the walk stops here.
func (w *levelWalk) collect(ctx context.Context, st *walkState) (bool, error) {
	o := w.o
	// A cursor re-presented after its transition COMMITTED must not collect
	// the level again. The transition applied this level's admissions to the
	// visited bits, so a second scan of the same frontier drops every entry
	// the first one kept -- the direction rule reads those bits -- and
	// finish() would rewrite sorted.<level> strictly smaller while
	// writeAdmitted reused the admitted file, leaving the frontier and the
	// counts untouched: relations would vanish from a page that reports itself
	// complete. An existing admitted.<level> IS that commit, so this leg picks
	// the level up where the transition left it, at the head of its sorted
	// run.
	committed, err := o.Retain.committed(st.Level)
	if err != nil {
		return false, err
	}
	if committed {
		// The frontier is whatever the last transition to RUN left, and this
		// level's did not run in this leg: it is the level before it, so the
		// scan that follows this serve would apply the direction rule against
		// the wrong level and drop every edge whose far end this level
		// admitted. It is set outright rather than through alignFrontier,
		// whose skip is about a RESUMED level and not this one.
		if err := o.Retain.setFrontier(st.Level); err != nil {
			return false, err
		}
		st.LevelState, st.LevelPos, st.RawBytes, st.LevelOffset = levelServing, EdgePos{}, 0, 0
		return false, nil
	}
	// The frontier this level expands is the level before it. An empty one is
	// an exhausted walk: nothing admitted anything to expand.
	live, err := o.Retain.hasFrontier(st.Level - 1)
	if err != nil {
		return false, err
	}
	if !live {
		st.More = false
		return true, nil
	}
	// Expanding the frontier at st.Level-1 produces nodes at st.Level, so the
	// bound is crossed when st.Level would exceed it. config.Limit.Exceeded is
	// the whole test: an unlimited bound is never exceeded, so the walk is
	// bounded by the graph, by the page budgets and by the deadline instead.
	if o.MaxDepth.Exceeded(int64(st.Level)) {
		st.DepthLimited = true
		return true, nil
	}
	if err := checkWalk(ctx, o.Budget); err != nil {
		if !deadlineStop(err, o) {
			return false, err
		}
		// Out of time before this level's scan: the frontier stands where it
		// was and the continuation resumes the collect at the same position.
		o.Budget.deadlineHit = true
		st.More = true
		return true, nil
	}
	var c *levelCollector
	if st.RawBytes > 0 {
		if c, err = reopenLevelCollector(o.Retain, st.Level, o.FrontierBytes, st.RawBytes); err != nil {
			return false, err
		}
	} else {
		c = newLevelCollector(o.Retain, st.Level, o.FrontierBytes)
	}
	c.keepRaw = o.Retain.isHeld(levelFileName(rawLevelPrefix, st.Level))
	pos, cut, err := w.scan(ctx, st, c)
	if err != nil {
		return false, err
	}
	if cut {
		// The page ended inside the scan. The collector is SPILLED before the
		// cursor is minted: the byte count it carries has to name bytes that
		// are on disk, or the resumed request truncates the run back past
		// records the scan will not deliver again.
		if err := c.persist(); err != nil {
			return false, err
		}
		st.LevelPos, st.RawBytes, st.More = pos, c.rawBytes(), true
		return true, nil
	}
	// The TRANSITION runs to completion whatever the clock says. It is the one
	// step of the pipeline with no resumable half: the sort consumes the
	// collected run and admitLevel writes the level's admitted file and then
	// applies it to the bitsets, so a deadline landing between those leaves a
	// frontier that is neither the level below nor the level above, and the
	// walk that resumes on it cannot apply the direction rule (ADR-0005). It is
	// bounded local work over records already read -- no adjacency read happens
	// here -- and the page it overruns ends in the SERVING state immediately
	// after, which IS resumable.
	tctx := context.WithoutCancel(ctx)
	sorted, err := c.finish(tctx)
	if err != nil {
		return false, err
	}
	admitted, err := o.Retain.admitLevel(tctx, sorted, st.Level, w.costs)
	if err != nil {
		return false, err
	}
	// The nodes themselves are counted as the level is SERVED, one neighbour
	// group at a time (closeGroup): the visited budget is what a caller sets to
	// bound the work a page does, so charging a whole level's admissions before
	// the first of them is served would spend a 300-node bound on a 300-wide
	// level and serve nothing at all.
	if admitted < 0 {
		return false, internalErr("graph: a level transition admitted a negative number of nodes")
	}
	w.sorted = sorted
	st.LevelState, st.LevelPos, st.RawBytes, st.LevelOffset = levelServing, EdgePos{}, 0, 0
	return false, nil
}

// scan streams the frontier in chunks and collects the entries the direction
// dedup rule keeps, in scan order. It reports the position the scan stopped at
// and whether it was cut short.
//
// One GraphReader.Neighbours call per CHUNK, never one per node: the chunk's
// surrogates go into one ascending scan of the packed adjacency, which is the
// layout's reason for existing. The chunk's own frontier states are the owner
// lookup, so no per-level map exists.
func (w *levelWalk) scan(ctx context.Context, st *walkState, c *levelCollector) (EdgePos, bool, error) {
	o := w.o
	var (
		pending []levelRecord
		pos     EdgePos
		cut     bool
		seen    int
	)
	// flush resolves one batch's canonical ids and appends it to the level.
	// Resolution is batched here rather than per level, because a level is
	// unbounded and a batch is not.
	flush := func() error {
		if len(pending) == 0 {
			return nil
		}
		if err := w.resolveIDs(ctx, pending); err != nil {
			return err
		}
		for _, rec := range pending {
			if err := c.add(rec); err != nil {
				return err
			}
		}
		pending = pending[:0]
		return nil
	}
	from := st.LevelPos
	// A resumed leg decodes the frontier of the level it picks up, chunk by
	// chunk, and nothing else: that is what the resume costs.
	resumed := o.Resume != nil && !w.resumedCounted
	w.resumedCounted = true
	err := o.Retain.eachFrontier(st.Level-1, from.Node, func(chunk []frontierState) error {
		if resumed {
			o.Retain.countResumed(len(chunk))
		}
		owners := make(map[NodeRef]frontierState, len(chunk))
		refs := make([]NodeRef, 0, len(chunk))
		for _, fs := range chunk {
			// The chunk is ascending, so the FIRST record for a surrogate wins;
			// a level never holds one twice, because admission is what puts a
			// node on it and a node is admitted once.
			if _, ok := owners[fs.Node]; ok {
				continue
			}
			owners[fs.Node] = fs
			refs = append(refs, fs.Node)
		}
		// Where this chunk's scan begins. A position is the NEXT entry to
		// deliver, so naming the chunk's first owner is what makes a cut inside
		// the chunk resume at the chunk and not at the level.
		start := from
		from = EdgePos{}
		if start.IsZero() {
			start = EdgePos{Node: refs[0]}
		}
		for {
			// The scan is cut every levelResolveBatch kept entries so that each
			// batch begins at a position the reader reported: a deadline inside
			// the batched id read below can then drop the batch and resume the
			// level exactly where that batch started.
			full := false
			at, err := w.reader.Neighbours(ctx, refs, o.Direction, w.codes, start, func(e Edge) error {
				if seen%edgeScanCheckEvery == 0 {
					if err := checkWalk(ctx, o.Budget); err != nil {
						if !deadlineStop(err, o) {
							return err
						}
						// The page ran out of time mid-level. What the scan
						// collected is fact; the continuation carries the
						// position this stop reports and the run it collected
						// into.
						o.Budget.deadlineHit = true
						cut = true
						return ErrStopScan
					}
				}
				seen++
				owner, ok := owners[e.Owner]
				if !ok {
					// The reader delivered an entry for a node that is not on
					// this chunk. Dropping it is the only honest answer --
					// attributing it to an arbitrary node would invent a path.
					return nil
				}
				keep, err := w.keepEntry(e)
				if err != nil || !keep {
					return err
				}
				if len(pending) >= levelResolveBatch {
					// Stopped BEFORE this entry is taken, so the position the
					// reader reports delivers it again.
					full = true
					return ErrStopScan
				}
				pending = append(pending, levelRecord{Owner: owner, Edge: e})
				return nil
			})
			if err != nil {
				if !deadlineStop(err, o) {
					return err
				}
				// The deadline fell INSIDE the read rather than on the check
				// above it: the reader returned the context's own error.
				o.Budget.deadlineHit = true
				cut = true
			}
			if ferr := flush(); ferr != nil {
				if !deadlineStop(ferr, o) {
					return ferr
				}
				// The deadline fell inside the batched id read. The batch is
				// dropped whole and the level resumes where it began, so the
				// resumed scan delivers exactly those entries again -- naming
				// half a batch would either lose them or serve them twice.
				o.Budget.deadlineHit = true
				pending, pos, cut = pending[:0], start, true
				return errLevelCut
			}
			if cut {
				pos = at
				return errLevelCut
			}
			if !full {
				return nil
			}
			start = at
		}
	})
	if err != nil && !errors.Is(err, errLevelCut) {
		return EdgePos{}, false, err
	}
	return pos, cut, nil
}

// keepEntry is the DIRECTION DEDUP RULE (ADR-0005, docs/queries.md): with V the
// cumulative admitted set and F the frontier being scanned, an OUTGOING X->Y is
// kept iff Y is not in V\F, and an INCOMING Y->X iff Y is not in V.
//
// If Y was admitted at an earlier level, Y's own scan delivered the relation
// then -- as incoming while X was unvisited, or as outgoing from Y. If Y is on
// THIS frontier, the source's outgoing copy is the one kept and Y's incoming
// copy is dropped. Every relation is therefore emitted exactly once per walk,
// with no cumulative relation set and no per-level map. An outgoing-only or
// incoming-only walk sees one owner per relation anyway, so the rule is a
// no-op there -- and it is SKIPPED there, because it is not merely redundant
// but wrong: an outgoing-only walk never scans the incoming copy, so dropping
// X->Y because Y was admitted earlier would drop the only copy there is.
func (w *levelWalk) keepEntry(e Edge) (bool, error) {
	if w.o.Direction != model.DirectionBoth {
		return true, nil
	}
	visited, err := w.o.Retain.bits.test(uint64(e.Neighbour))
	if err != nil || !visited {
		return !visited, err
	}
	if !e.Outgoing {
		return false, nil
	}
	return w.o.Retain.testFrontier(e.Neighbour)
}

// resolveIDs names one batch of collected entries: two batched primary-key
// reads over the batch's deduplicated refs, whatever the level's size. The ids
// travel ON the record, so the sort key is content-derived and no level-sized
// name table exists anywhere.
func (w *levelWalk) resolveIDs(ctx context.Context, recs []levelRecord) error {
	nodes := make([]NodeRef, 0, 2*len(recs))
	rels := make([]RelRef, 0, len(recs))
	for _, r := range recs {
		nodes = append(nodes, r.Edge.Owner, r.Edge.Neighbour)
		rels = append(rels, r.Edge.Rel)
	}
	nodes, rels = ascending(nodes), ascending(rels)
	nodeIDs, err := w.reader.NodeIDs(ctx, nodes)
	if err != nil {
		return err
	}
	if len(nodeIDs) != len(nodes) {
		return internalErr("graph: the reader named a different number of nodes than it was given")
	}
	relIDs, err := w.reader.RelationIDs(ctx, rels)
	if err != nil {
		return err
	}
	if len(relIDs) != len(rels) {
		return internalErr("graph: the reader named a different number of relations than it was given")
	}
	byNode := make(map[NodeRef]model.NodeID, len(nodes))
	for i, ref := range nodes {
		byNode[ref] = nodeIDs[i]
	}
	byRel := make(map[RelRef]model.RelationID, len(rels))
	for i, ref := range rels {
		byRel[ref] = relIDs[i]
		w.routes.remember(ref, relIDs[i])
	}
	for i := range recs {
		recs[i].OwnerID = byNode[recs[i].Edge.Owner]
		recs[i].NodeID = byNode[recs[i].Edge.Neighbour]
		recs[i].RelID = byRel[recs[i].Edge.Rel]
	}
	return nil
}

// serve hands the sorted level to the visitor from the offset the page starts
// at, in batches that are named together. It reports whether the walk stops
// inside this level.
func (w *levelWalk) serve(ctx context.Context, st *walkState) (bool, error) {
	o := w.o
	if w.sorted == nil {
		s, err := openSortedLevel(o.Retain, st.Level)
		if err != nil {
			return false, err
		}
		w.sorted = s
	}
	var (
		batch []levelRecord
		ends  []int64
		stop  bool
		at    = st.LevelOffset
	)
	// take names the batch and hands it to the visitor. A visitor stop leaves
	// `at` on the record it refused, so the continuation delivers that record
	// first and none is served twice or lost.
	take := func() error {
		if len(batch) == 0 {
			return nil
		}
		if err := w.nameBatch(ctx, batch); err != nil {
			if !deadlineStop(err, o) {
				return err
			}
			// The deadline fell inside the batched read that names this
			// batch's routes. The batch is dropped whole and the page ends at
			// the record it began with, which is where `at` already stands, so
			// the continuation serves exactly these records.
			o.Budget.deadlineHit = true
			stop = true
			return errLevelCut
		}
		for i, rec := range batch {
			if w.groupOpen && rec.NodeID != w.groupID {
				if err := w.closeGroup(); err != nil {
					return err
				}
			}
			w.group, w.groupID, w.groupOpen = rec.Edge.Neighbour, rec.NodeID, true
			if err := w.visit(rec.Owner, rec.Edge); err != nil {
				cut := deadlineStop(err, o)
				if !cut && !errors.Is(err, errStopExpansion) {
					return err
				}
				if cut {
					// The VISITOR ran out of time. It is not a pure predicate:
					// the rollup teed off this walk does reads of its own, so
					// the request deadline lands inside the visitor as often as
					// inside the walk's own scan. The page ends exactly as the
					// deliberate stop does.
					o.Budget.deadlineHit = true
				}
				stop = true
				return errLevelCut
			}
			o.Budget.edges++
			o.Budget.pageEdges++
			at = ends[i]
		}
		batch, ends = batch[:0], ends[:0]
		return nil
	}
	err := w.sorted.each(st.LevelOffset, func(r levelRecord, next int64) error {
		batch = append(batch, r)
		ends = append(ends, next)
		if len(batch) < levelServeBatch {
			return nil
		}
		return take()
	})
	if err == nil && !stop {
		err = take()
	}
	if err != nil && !errors.Is(err, errLevelCut) {
		return false, err
	}
	if stop {
		st.LevelOffset, st.More = at, true
		return true, nil
	}
	// The last group of the level ends with the level.
	if err := w.closeGroup(); err != nil {
		return false, err
	}
	// The level has been served whole: this leg is finished with its sorted run
	// and with the frontier it was collected from, and the level after it
	// begins. Neither is DELETED here. A retryable failure later in this page
	// sends the caller back to the cursor this leg was handed, which resumes
	// behind both of them -- deleting them at once is what made that retry
	// answer CTX_STORAGE_CORRUPT for a level the walk itself had served it
	// from. They are held for the page instead, and the mint of the next
	// continuation, which is what supersedes that cursor, removes every one of
	// them the new cursor does not resume from (releaseHeld).
	if err := w.sorted.finished(); err != nil {
		return false, err
	}
	o.Retain.hold(levelFileName(sortedLevelPrefix, st.Level))
	o.Retain.hold(levelFileName(admittedLevelPrefix, st.Level-1))
	w.sorted = nil
	st.Level, st.LevelState, st.LevelOffset = st.Level+1, levelCollecting, 0
	return false, nil
}

// closeGroup charges the neighbour group that has just ended against the
// visited budget, if that neighbour is one this level ADMITTED. The frontier
// bitset is exactly the level's admissions, so one bitset test per group
// answers it; a neighbour the level merely reached again is not a new node and
// is not counted, which is what keeps visited_count the size of the admitted
// set across any number of pages.
func (w *levelWalk) closeGroup() error {
	if !w.groupOpen {
		return nil
	}
	w.groupOpen = false
	admitted, err := w.o.Retain.testFrontier(w.group)
	if err != nil || !admitted {
		return err
	}
	w.o.Budget.visited++
	w.o.Budget.pageVisited++
	return nil
}

// nameBatch resolves the canonical ids one served batch needs. The edge's own
// three ids travel on the record; only the ROUTE relations -- the chain of
// admitting relations behind the owner -- have to be looked up, and they are
// looked up through a bounded cache the records themselves keep warm, so a page
// costs at most one batched read for its routes and never a relation name table
// sized by the walk.
func (w *levelWalk) nameBatch(ctx context.Context, batch []levelRecord) error {
	names := w.o.Names
	if names == nil {
		// The package rollup rides on the same walk and reads refs only.
		return nil
	}
	nodes := make(map[NodeRef]model.NodeID, 2*len(batch))
	rels := make(map[RelRef]model.RelationID, len(batch))
	var missing []RelRef
	for _, r := range batch {
		nodes[r.Edge.Owner] = r.OwnerID
		nodes[r.Edge.Neighbour] = r.NodeID
		rels[r.Edge.Rel] = r.RelID
		w.routes.remember(r.Edge.Rel, r.RelID)
		for _, ref := range r.Owner.Route {
			if _, ok := rels[ref]; ok {
				continue
			}
			id, ok := w.routes.lookup(ref)
			if !ok {
				missing = append(missing, ref)
				continue
			}
			rels[ref] = id
		}
	}
	if len(missing) > 0 {
		missing = ascending(missing)
		ids, err := w.reader.RelationIDs(ctx, missing)
		if err != nil {
			return err
		}
		if len(ids) != len(missing) {
			return internalErr("graph: the reader named a different number of relations than it was given")
		}
		for i, ref := range missing {
			rels[ref] = ids[i]
			w.routes.remember(ref, ids[i])
		}
	}
	// Rebuilt, never extended: a map kept across pages would grow with the
	// walk, which is the repository-sized heap structure the surrogate walk
	// exists to remove.
	names.nodes, names.rels = nodes, rels
	return nil
}

// routeNames is the bounded ref -> canonical relation id cache. It is a plain
// LRU: the routes a page needs are the admitting relations of the levels behind
// it, so the entries a page reads are the entries the pages before it wrote,
// and the oldest are the ones no live route still reaches through.
type routeNames struct {
	max   int
	order *list.List
	byRef map[RelRef]*list.Element
}

// routeName is one cache entry, held by the list so eviction knows its key.
type routeName struct {
	ref RelRef
	id  model.RelationID
}

func newRouteNames(max int) *routeNames {
	return &routeNames{max: max, order: list.New(), byRef: make(map[RelRef]*list.Element)}
}

func (c *routeNames) lookup(ref RelRef) (model.RelationID, bool) {
	el, ok := c.byRef[ref]
	if !ok {
		return "", false
	}
	c.order.MoveToFront(el)
	return el.Value.(routeName).id, true
}

func (c *routeNames) remember(ref RelRef, id model.RelationID) {
	if el, ok := c.byRef[ref]; ok {
		c.order.MoveToFront(el)
		return
	}
	c.byRef[ref] = c.order.PushFront(routeName{ref: ref, id: id})
	for c.order.Len() > c.max {
		el := c.order.Back()
		c.order.Remove(el)
		delete(c.byRef, el.Value.(routeName).ref)
	}
}

// walkState is where a walk stopped. A page that stopped on its item limit
// turns it into the continuation the next page resumes from; a walk that ran to
// completion leaves More false and mints nothing.
type walkState struct {
	// Level is the BFS level the walk stopped in, and LevelState which of its
	// two states it was in. Together with the position below they are the whole
	// of what a continuation resumes: the frontier itself is the admitted file
	// of the level before this one, inside the retained directory.
	Level      int
	LevelState levelState
	// LevelPos is where a COLLECTING level's adjacency scan stopped: the owner
	// and the index of the stored entry that was NOT delivered, so the resumed
	// scan delivers it first.
	LevelPos EdgePos
	// RawBytes is the committed length of that level's collected run, which the
	// resumed request truncates back to before it scans on.
	RawBytes int64
	// LevelOffset is where a SERVING level resumes: the byte offset of the
	// first record the page did not take.
	LevelOffset int64
	// More reports that the walk stopped with work still in front of it. A walk
	// that ran out of frontier leaves it false and mints no continuation.
	More bool
	// DepthLimited records that the walk stopped because the user-set depth
	// bound was reached, with those nodes' edges still unread.
	//
	// A depth stop is REPORTED but not resumable, and it is the only stop of
	// which that is true. Every other bound here is a per-page work budget, so
	// the next page makes progress; the depth bound is a property of the walk
	// and is part of the query hash a cursor is bound to, so a continuation
	// minted for it would resume a walk that is already past the bound, stop at
	// once and mint another -- a cursor chain that never terminates and never
	// returns a row. The honest answer is Truncated + reasonDepth, and the
	// caller's remedy is to raise max_depth.
	DepthLimited bool
}

// kindCosts is the walk's relation-kind cost table, indexed by the generation's
// kind CODE. Cost(kind) takes a model.RelationKind, and asking the reader's
// dictionary for the kind behind every decoded entry would put a map lookup on
// the hot path of the scan; the dictionary is small and fixed for the
// generation, so it is flattened once per walk instead.
type kindCosts []int64

func (c kindCosts) of(code KindCode) int64 {
	if int(code) >= len(c) {
		return 0
	}
	return c[code]
}

// walkKinds resolves the request's relation kinds into this generation's codes
// and flattens the cost table. An empty request means EVERY kind, which the
// reader spells as a nil code slice.
//
// A requested kind the generation's dictionary does not carry contributes no
// code: it selects nothing, which is the honest answer for a kind no relation
// in this generation has. It is NOT an error -- a generation built before an
// analyzer existed simply has none of its edges -- and it is not silently
// widened to "every kind" either, which is what dropping the filter would do.
func walkKinds(r GraphReader, kinds []model.RelationKind) ([]KindCode, kindCosts, error) {
	table := r.Kinds()
	if table == nil {
		return nil, nil, internalErr("graph: the pinned generation has no relation-kind dictionary")
	}
	costs := make(kindCosts, table.Len()+1)
	for code := 1; code <= table.Len(); code++ {
		kind, ok := table.Kind(KindCode(code))
		if !ok {
			continue
		}
		costs[code] = Cost(kind)
	}
	if len(kinds) == 0 {
		return nil, costs, nil
	}
	codes := make([]KindCode, 0, len(kinds))
	for _, kind := range kinds {
		code, ok := table.Code(kind)
		if !ok {
			continue
		}
		codes = append(codes, code)
	}
	if len(codes) == 0 {
		// Every requested kind is absent from this generation. Handing the
		// reader an EMPTY slice would mean "no filter" and walk the whole
		// graph, so the walk is short-circuited with a filter that selects
		// nothing instead.
		return nil, costs, errNoSuchKinds
	}
	slices.Sort(codes)
	return slices.Compact(codes), costs, nil
}

// errNoSuchKinds is the internal signal that the request named only relation
// kinds this generation does not carry. expand's callers turn it into an empty
// walk, never into a failure: an answer of "nothing" is correct there.
var errNoSuchKinds = errors.New("graph: no relation kind of this request is present in the generation")

// sortFrontier puts a level into ascending surrogate order, which is what
// GraphReader.Neighbours requires of the refs it is given and what makes the
// bitset's page cache walk forward.
func sortFrontier(level []frontierState) {
	sort.Slice(level, func(i, j int) bool { return level[i].Node < level[j].Node })
}

// checkWalk fails the walk on the two conditions that are not budgets of
// admitted work: the caller went away, and the wall-clock query deadline
// passed. They are distinct and each has exactly one source -- cancellation
// comes from ctx, the timeout from budget.deadline, which the caller sets from
// the engine clock -- so neither check duplicates the other.
func checkWalk(ctx context.Context, b *budget) error {
	if err := ctx.Err(); err != nil {
		if errors.Is(err, context.Canceled) {
			return &model.Error{Code: model.CodeCanceled, Message: "graph traversal was canceled"}
		}
		return &model.Error{Code: model.CodeQueryDeadline, Message: "graph traversal exceeded its deadline"}
	}
	if !b.deadline.IsZero() && !b.clock().Before(b.deadline) {
		return &model.Error{Code: model.CodeQueryDeadline, Message: "graph traversal exceeded its deadline"}
	}
	return nil
}

// Neighbors expands the request's seeds in the request's own direction over its
// relation allowlist, reporting the direction it walked and the visited and
// edge counts it spent.
func (e *Engine) Neighbors(ctx context.Context, req model.GraphRequest) (model.GraphResult, error) {
	return e.traverse(ctx, req, neighborsEndpoint, req.Direction, req.Relations)
}

// neighborsEndpoint binds a continuation to the operation that issued it, the
// way referenceEndpoint does: a cursor minted by a traversal means nothing to
// another endpoint even at the same generation and seeds, and resumeTraversal
// rejects it.
const neighborsEndpoint = "graph.neighbors"

// resolveBound applies the Section 20.1 zero-value convention: zero takes the
// configured default and a positive request value is honoured only as far as
// that default, so a request can tighten a bound but never raise it.
//
// It is the FINITE form, for the one bound that is never unlimited: the page
// item ceiling. Use resolveLimit for every scale bound -- a silent clamp there
// is the class-G defect this wave removes.
func resolveBound(requested, configured int) int {
	if requested <= 0 || requested > configured {
		return configured
	}
	return requested
}

// resolveLimit resolves one unlimited-capable count bound and, when the request
// asked for more than the configuration allows, returns the notice that says so.
// A request can still only tighten a bound -- raising it is an operator
// decision, not a caller's -- but it is never SILENTLY tightened: the answer
// carries "requested N, effective M" so the caller can tell a small answer
// caused by its own request from one caused by the configuration.
//
// config.Limit.Min owns the comparison, with unlimited as the top of the
// lattice, so an unlimited configuration honours any finite request.
func resolveLimit(key string, requested int, configured config.Limit) (config.Limit, string) {
	if requested <= 0 {
		return configured, ""
	}
	effective := config.Limit(requested).Min(configured)
	if effective.Value() == int64(requested) {
		return effective, ""
	}
	return effective, fmt.Sprintf("%s: requested %d, effective %s (the configured bound)",
		key, requested, effective)
}

// resolvePageItems is resolveBound with the same disclosure obligation.
func resolvePageItems(requested, configured int) (int, string) {
	effective := resolveBound(requested, configured)
	if requested <= 0 || effective == requested {
		return effective, ""
	}
	return effective, fmt.Sprintf("page.limit: requested %d, effective %d (the configured bound)",
		requested, effective)
}

// appendNotice collects the non-empty disclosures a walk accumulated.
func appendNotice(into []string, notice string) []string {
	if notice == "" {
		return into
	}
	return append(into, notice)
}

// traverse is the body behind Neighbors: dir and kinds are what the request
// asked to walk, and endpoint is what a continuation minted here is bound to.
func (e *Engine) traverse(ctx context.Context, req model.GraphRequest, endpoint string, dir model.Direction,
	kinds []model.RelationKind) (res model.GraphResult, err error) {
	// One deferred mapping covers every traversal: a bare
	// context failure from the adjacency reader becomes the Section 8 code for
	// the state it is in, and anything already typed is left alone.
	defer func() { err = typedContextError(ctx, err) }()
	if err := req.Validate(); err != nil {
		return model.GraphResult{}, err
	}
	// The deadline wraps the gate as well as the walk, so waiting for a slot
	// past the request deadline is the resource limit the caller must see, and
	// every adjacency round trip below runs under Section 3's per-request
	// deadline rather than only being checked between expansion steps.
	deadline, bounded := e.queryDeadline(ctx)
	// The caller's own context, kept aside: a page the deadline ended still has
	// to be DELIVERED -- its endpoints hydrated and its continuation minted --
	// and both of those run after the walk's deadline has passed. See the
	// finish context below.
	parent := ctx
	var cancel context.CancelFunc
	if bounded {
		ctx, cancel = context.WithDeadline(ctx, deadline)
	} else {
		// No deadline of any kind: the walk runs until it finishes, its work
		// budgets stop it, or the caller cancels.
		ctx, cancel = context.WithCancel(ctx)
	}
	defer cancel()
	if e.gate != nil {
		if err := e.gate.Acquire(ctx); err != nil {
			return model.GraphResult{}, err
		}
		defer e.gate.Release()
	}
	// Every storage read this answer makes resolves its own page bound against
	// the wire ceiling; a read served at a size other than the one asked for is
	// reported on the answer rather than applied silently. Only the observation
	// sink is imported, never the store: facts still come through Adjacency.
	ctx, clamps := pagination.WithPageClamps(ctx)
	if len(kinds) == 0 {
		kinds = DefaultRelations()
	}

	var notices []string
	maxDepth, notice := resolveLimit("max_depth", req.MaxDepth, e.limits.Depth())
	notices = appendNotice(notices, notice)
	maxVisited, notice := resolveLimit("max_visited", req.MaxVisited, e.limits.Visited())
	notices = appendNotice(notices, notice)
	maxEdges, notice := resolveLimit("max_edges", req.MaxEdges, e.limits.Edges())
	notices = appendNotice(notices, notice)
	maxItems, notice := resolvePageItems(req.Page.Limit, e.limits.MaxPageItems)
	notices = appendNotice(notices, notice)

	// The capability disclosure happens before the walk: a missing dependence
	// edge must not read as a genuine absence of edges.
	caps, deferred, err := e.completeness(ctx, kinds)
	if err != nil {
		return model.GraphResult{}, err
	}

	// The walk's deadline is the SAME instant the context carries, taken before
	// the gate wait: a deadline recomputed after it would outlive the request's
	// own by however long the wait took.
	b := &budget{deadline: deadline, now: e.now}
	// The query hash binds a continuation to this exact normalized walk, so a
	// cursor presented to a differently filtered or differently bounded query is
	// CTX_CURSOR_INVALID rather than a silently repinned answer.
	queryHash := traversalQueryHash(dir, kinds, req.Start, maxDepth.Int(), maxItems)
	var resume *resumeState
	if req.Page.Cursor != "" {
		resume, err = e.resumeTraversal(ctx, req.Page.Cursor, endpoint, queryHash, deadline)
		if err != nil {
			return model.GraphResult{}, err
		}
		// The consumed continuation's spool and lease outlive the walk: the
		// membership probes and the spill that copies the cumulative set
		// forward both read from that spool. Deferring the release here -- not
		// at the end of the happy path -- is what keeps an error return from
		// leaking a spool and a lease for the whole cursor TTL.
		//
		// Terminal outcomes only. A RETRYABLE failure -- CTX_WORKSPACE_BUSY
		// from a contended store, a transient read error, the query deadline --
		// tells the caller to present this SAME cursor again, so the spool, the
		// retained state directory and the lease it names must all still be
		// adoptable. Releasing unconditionally turned one busy edge read on a
		// resumed page into CTX_CURSOR_INVALID and made the whole walk behind
		// that cursor unrecoverable; the state is still bounded, because it
		// expires with the cursor's own lease TTL.
		defer func() {
			if terminalOutcome(err) {
				resume.Release()
			}
		}()
		// The resumed budget carries the earlier pages' cumulative spend by
		// ASSIGNMENT, so replaying one cursor twice neither resets nor doubles it.
		b = resume.Budget
	}
	// The retained state of THIS walk, exactly as impact and the package rollup
	// keep theirs: the frontier the walk commits level by level, the cumulative
	// admitted-node bitset it tests membership against. A resumed page reopens
	// the one its predecessor left.
	//
	// It is opened UNCONDITIONALLY, and not only by a page that mints a
	// continuation. The bitset IS the membership set the walk tests against, so
	// a walk with nowhere to keep it is not a cheaper walk, it is a wrong one.
	// What the one-page query pays for it is a
	// directory, a sparse file whose allocated blocks are only the regions the
	// walk touched, and a manifest -- and it is discarded below when no cursor
	// is minted.
	retain := resumeRetained(resume)
	if retain == nil {
		if retain, err = e.openWalkState(); err != nil {
			return model.GraphResult{}, err
		}
	}
	// Discarded AFTER the continuation below has taken it: a page that mints a
	// cursor detaches the directory, and this then finds nothing to remove.
	defer func() {
		if retain != nil {
			retain.discard()
		}
	}()
	var (
		relations  []model.Relation
		walkReason string
	)
	// names is refilled by the walk before each level's edges are delivered, so
	// this page names the edges it serves from one batched read per level
	// instead of one per edge.
	names := &levelNames{}
	reader, err := e.Reader()
	if err != nil {
		return model.GraphResult{}, err
	}
	kindByCode := kindNames(reader)
	endpoints := map[model.NodeID]bool{}
	for _, s := range req.Start {
		endpoints[s] = true
	}
	visit := func(owner frontierState, edge Edge) error {
		// The visited and edge budgets are compared against THIS page's spend:
		// they are work budgets that end a page, not ceilings that end a walk.
		// Exceeded is strictly greater, so the test is on the spend this row
		// would take the page to.
		switch {
		case maxEdges.Exceeded(b.pageEdges + 1):
			walkReason = reasonEdgeBudget
			return errStopExpansion
		case maxVisited.Exceeded(b.pageVisited + 1):
			walkReason = reasonVisitedBudget
			return errStopExpansion
		case len(relations) >= maxItems:
			walkReason = reasonPageFull
			return errStopExpansion
		}
		from, to := edgeEnds(edge)
		rel := model.Relation{
			ID:   names.rel(edge.Rel),
			From: names.node(from),
			To:   names.node(to),
			Kind: kindAt(kindByCode, edge.Kind),
		}
		if rel.ID == "" || rel.From == "" || rel.To == "" {
			// The generation carries an adjacency entry it publishes no
			// canonical id for one end of. It cannot be served -- a relation
			// with an empty endpoint fails its own validation -- and inventing
			// an id would publish a fact nothing backs. The edge is still
			// COUNTED: the walk read it.
			return nil
		}
		relations = append(relations, rel)
		endpoints[rel.From] = true
		endpoints[rel.To] = true
		return nil
	}
	state, err := expand(ctx, reader, req.Start, expandOptions{
		Direction:     dir,
		Kinds:         kinds,
		MaxDepth:      maxDepth,
		Budget:        b,
		FrontierBytes: e.limits.FrontierBytes,
		DeadlineStops: true,
		Resume:        resume,
		Retain:        retain,
		Names:         names,
	}, visit)
	if err != nil {
		return model.GraphResult{}, err
	}

	// Delivering a deadline-stopped page needs a live context: `ctx` is past
	// its deadline by construction, so hydrating the endpoints and writing the
	// continuation spool on it would fail with the very CTX_QUERY_DEADLINE this
	// stop exists to replace. It runs DETACHED from the caller's context, the
	// way every other delivery step does (deliverCtx in impact.go): the page's
	// contents are already decided, so a deadline or a cancellation landing
	// here would not truncate the answer, it would delete rows the walk
	// admitted with no notice and no cursor that hands them back. The grace is
	// the configured query_timeout, and query_timeout 0 -- no wall clock --
	// leaves it unbounded, because delivery is bounded work by construction
	// (one batched node read of at most a page of endpoints, one spool write)
	// and not more walking.
	finish := ctx
	if b.deadlineHit {
		finish = context.WithoutCancel(parent)
		if e.limits.QueryTimeout > 0 {
			var cancelFinish context.CancelFunc
			finish, cancelFinish = context.WithTimeout(finish, e.limits.QueryTimeout)
			defer cancelFinish()
		}
	}

	ids := make([]model.NodeID, 0, len(endpoints))
	for id := range endpoints {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	nodes, err := e.adjacency.NodesByID(finish, ids)
	if err != nil {
		return model.GraphResult{}, err
	}

	reason := walkReason
	// A page the clock ended is truncated for exactly that reason, and a
	// continuation is minted from its frontier below like any other page stop.
	if reason == "" && b.deadlineHit {
		reason = reasonDeadline
	}
	// A walk that ran out of depth with nodes still unexpanded is truncated and
	// says so: before this it fell out of the loop reporting nothing.
	if reason == "" && state.DepthLimited {
		reason = reasonDepth
	}
	if reason == "" && deferred {
		reason = reasonDependence
	}
	// A continuation is offered for EVERY stop that left a frontier standing --
	// a full page, a spent per-page visited or edge budget, a level the frontier
	// byte ceiling spilled. All three are now per-page budgets, so the resumed
	// page makes progress rather than stopping at once.
	//
	// The depth bound is the single exception, and DepthLimited on walkState
	// carries the reason: it is part of the query hash the cursor is bound to,
	// so a continuation minted for it could only resume a walk already past it.
	var nextCursor string
	if state.More && !state.DepthLimited {
		// Nothing of the walk is materialized here. The frontier is already in
		// the retained level file, committed as each level closed, and the
		// admitted nodes are already bits beside it; the token names the
		// directory rather than a copy of what is in it.
		nextCursor, err = e.nextTraversalCursor(finish, b, continuation{
			Endpoint:    endpoint,
			QueryHash:   queryHash,
			Level:       state.Level,
			LevelState:  state.LevelState,
			LevelPos:    state.LevelPos,
			RawBytes:    state.RawBytes,
			LevelOffset: state.LevelOffset,
			More:        state.More,
			Retain:      retain,
		})
		if err != nil {
			return model.GraphResult{}, err
		}
	}
	result := model.GraphResult{
		Meta: model.QueryMeta{
			Binding:          e.adjacency.Binding(),
			Completeness:     caps,
			Truncated:        reason != "",
			TruncationReason: reason,
			NextCursor:       nextCursor,
			Notices:          append(notices, clamps.Notices()...),
		},
		Direction: dir,
		Nodes:     nodes,
		Relations: relations,
		// The counts are cumulative spend, not per-page tallies, so a
		// continuation resumes from them rather than from zero.
		VisitedCount: b.visited,
		EdgeCount:    b.edges,
		// MaxDepth echoes the effective depth bound the walk ran under, so a
		// caller can tell a shallow answer caused by a tightened bound from one
		// caused by the graph simply ending.
		MaxDepth: maxDepth.Int(),
	}
	if err := result.Validate(); err != nil {
		return model.GraphResult{}, err
	}
	return result, nil
}

// completeness is the ONE capability derivation every graph answer uses. It
// reads the pinned generation's capability report through the same
// Adjacency.Capabilities port search reads it through (search.go:210), so the
// graph family discloses the same rows `search` and `symbol` do rather than
// reporting no capabilities at all.
//
// The deferred-dependence disclosure is folded INTO those rows, not appended to
// them: a deferred row is one of the generation's own capability rows, enriched
// here with the promotion's queue position. Appending would publish the row
// twice and push a full report past model.MaxCapabilityStates, which the
// answer's own Validate then rejects.
//
// deferred reports whether a request touching a dependence-only relation kind
// found deferred units; that, and never the length of rows, is what makes an
// answer truncated -- every generation carries capability rows, so a
// length test would report every answer as incomplete.
//
// In report mode there is no promoter and the rows are disclosed without
// promoting. A failed promotion never fails the query: the answer is still
// correct, just still incomplete.
func (e *Engine) completeness(ctx context.Context, kinds []model.RelationKind) ([]model.CapabilityState, bool, error) {
	rows, err := e.adjacency.Capabilities(ctx)
	if err != nil {
		return nil, false, err
	}
	wanted := map[model.RelationKind]bool{}
	for _, k := range kinds {
		wanted[k] = true
	}
	touches := false
	for _, k := range DependenceOnly() {
		if wanted[k] {
			touches = true
			break
		}
	}
	if !touches {
		return rows, false, nil
	}
	deferred := false
	for i, c := range rows {
		if c.ProviderID != dependenceProviderID || c.Details["reason"] != reasonUnitsDeferred {
			continue
		}
		deferred = true
		if e.promoter == nil {
			continue
		}
		p, err := e.promoter.Promote(ctx, dependenceProviderID, c.Scope)
		if err != nil {
			continue
		}
		// WithDetail clones the map, truncates the value and refuses to
		// grow past MaxCapabilityDetails, so folding a queue position in
		// can neither mutate the reader's row nor build a row that then
		// fails its own Validate and fails the query it was disclosing.
		row := c.WithDetail("units", strconv.Itoa(p.Units)).
			WithDetail("position", strconv.Itoa(p.Position))
		if p.Estimate > 0 {
			// An unmeasured duration is reported as unmeasured, never invented.
			row = row.WithDetail("estimate_ms", strconv.FormatInt(p.Estimate.Milliseconds(), 10))
		}
		rows[i] = row
	}
	return rows, deferred, nil
}

const (
	// dependenceProviderID is the provider whose deferred units make a
	// dependence-only answer incomplete.
	dependenceProviderID = "dependence"
	// reasonUnitsDeferred is the landed addDeferred spelling; no new capability
	// state or diagnostic code is introduced for pending work.
	reasonUnitsDeferred = "units_deferred"
)

// ascending sorts a set of surrogates and removes its duplicates. It is the
// precondition every cumulative-set call has -- the page cache is walked
// forward, so an unordered batch would touch one page many times -- and it is
// applied here rather than assumed of the caller, because an unordered batch
// would merely be slow and nothing downstream would report it.
func ascending[T NodeRef | RelRef](refs []T) []T {
	slices.Sort(refs)
	return slices.Compact(refs)
}

// impactChunkRefs splits a batch of surrogates into reads the storage layer
// can serve in one statement, the same bound impactChunkNodes applies to
// canonical ids. The batch is already ascending, so the chunks are contiguous
// ranges and the side array's parts are touched in order.
func impactChunkRefs[T NodeRef | RelRef](refs []T) [][]T {
	out := make([][]T, 0, len(refs)/adjacencyBatch+1)
	for start := 0; start < len(refs); start += adjacencyBatch {
		end := min(start+adjacencyBatch, len(refs))
		out = append(out, refs[start:end])
	}
	return out
}

// kindNames flattens the generation's relation-kind dictionary into a slice
// indexed by kind code. Codes are dense from 1 and the dictionary is small and
// fixed for the generation, so a walk resolves it once instead of asking the
// table for the kind behind every decoded entry.
func kindNames(r GraphReader) []model.RelationKind {
	table := r.Kinds()
	if table == nil {
		return nil
	}
	out := make([]model.RelationKind, table.Len()+1)
	for code := 1; code <= table.Len(); code++ {
		if kind, ok := table.Kind(KindCode(code)); ok {
			out[code] = kind
		}
	}
	return out
}

// kindAt names the relation kind behind one generation kind code. An
// out-of-range code names nothing, which is how a served relation whose kind
// the generation does not publish is dropped rather than given a guessed one.
func kindAt(kinds []model.RelationKind, code KindCode) model.RelationKind {
	if int(code) >= len(kinds) {
		return ""
	}
	return kinds[code]
}
