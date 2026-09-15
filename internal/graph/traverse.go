package graph

import (
	"context"
	"errors"
	"fmt"
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
	reasonFrontierBytes = "frontier memory budget exhausted"
	// reasonDepth is the depth bound. Before this it was the ONE stop that
	// reported nothing at all: the loop simply fell out with a live frontier
	// and Truncated=false, so a depth-limited answer read as a complete one.
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
	var me *model.Error
	return errors.As(err, &me) && me.Code == model.CodeQueryDeadline
}

// edgeRowOverheadBytes is the fixed per-row cost of holding one edge of a
// frontier level in memory: the edgeRow struct, its model.Relation, the string
// headers inside both, and the map entry that records the relation as
// collected. It is a deliberately conservative ESTIMATE rather than a
// measurement -- Limits.FrontierBytes is a ceiling on memory, and an estimate
// that reads low would let the ceiling be crossed before the walk noticed.
const edgeRowOverheadBytes = 256

// edgeRowBytes estimates what one edgeRow costs, adding the identifiers it
// actually carries to the fixed overhead.
func edgeRowBytes(r edgeRow) int64 {
	return edgeRowOverheadBytes + int64(len(r.owner.Node)+len(r.owner.Via)+
		len(r.rel.ID)+len(r.rel.From)+len(r.rel.To)+len(r.rel.Kind)+len(r.neighbor))
}

// expand is the ONE batched BFS. visit is called once per admitted edge in the
// frozen (depth asc, NodeID asc) order and may return errStopExpansion to end
// the walk, which expand reports as a clean finish: the visitor stopped
// deliberately and owns whatever truncation flag it set.
//
// expand does one Adjacency.Edges round trip per frontier batch per keyset
// page, never one per node: a hub with 40 out-edges costs one call, not 40.
//
// It enforces only the bounds it is given -- MaxDepth, the deadline and
// cancellation. MaxVisited and MaxEdges are deliberately absent from
// expandOptions: budget carries the CUMULATIVE, cursor-carried counts already
// spent, and the caller resolves the caps from Limits and the request, so the
// visitor is the one place that compares the two. That keeps a resumed page
// from spending a fresh budget.
func expand(ctx context.Context, a Adjacency, seeds []model.NodeID, o expandOptions,
	visit func(frontierState, model.Relation) error) (walkState, error) {
	if a == nil {
		return walkState{}, (&model.Error{Code: model.CodeInternal,
			Message: "graph expansion requires an adjacency reader"}).WithDetail("operation", "expand")
	}
	if o.Budget == nil {
		return walkState{}, (&model.Error{Code: model.CodeInternal,
			Message: "graph expansion requires a budget"}).WithDetail("operation", "expand")
	}
	if visit == nil {
		return walkState{}, (&model.Error{Code: model.CodeInternal,
			Message: "graph expansion requires a visitor"}).WithDetail("operation", "expand")
	}
	batch := o.BatchSize
	if batch <= 0 {
		batch = adjacencyBatch
	}

	// The cumulative admitted-node set. Its heap footprint is the bounded
	// front of visitedSet, never the walk: the authoritative set is the
	// continuation spool the resume streams from.
	admitted := newVisitedSet(nil)
	if o.Resume != nil {
		admitted.stream = o.Resume.Visited
		admitted.filter = o.Resume.Filter
	}
	// A relation is admitted at most once for the whole walk: a cycle, an
	// overlapping batch or a DirectionBoth edge whose two endpoints are both on
	// the frontier must not be counted or emitted twice.
	admittedRel := map[model.RelationID]bool{}
	var (
		frontier []frontierState
		// carry is the NEXT level as the page that issued the cursor had
		// already built it, held aside until the re-read level completes.
		carry     []frontierState
		depth     int
		skipOwner model.NodeID
		skipKey   model.RelationID
	)
	if o.Resume == nil {
		// Seeds enter at depth 0 in request order, de-duplicated, then sorted by
		// NodeID: the frozen (depth asc, NodeID asc) order starts here.
		for _, s := range seeds {
			if s == "" || admitted.has(s) {
				continue
			}
			admitted.add(s)
			frontier = append(frontier, frontierState{Node: s})
		}
		o.Budget.visited += int64(len(frontier))
		o.Budget.pageVisited += int64(len(frontier))
	} else {
		// A resume never re-enters the seeds: they are already in the visited
		// set the issuing page spooled, and re-admitting them would spend the
		// cumulative visited budget a second time for the same nodes. That set
		// is NOT materialized here -- it is streamed from the spool one level
		// at a time; only the resumed frontier's own nodes enter the front.
		depth = o.Resume.Cursor.Depth
		skipOwner, skipKey = o.Resume.Cursor.LastOwner, o.Resume.Cursor.LastKey
		for _, fs := range o.Resume.Frontier {
			if fs.Depth == depth {
				frontier = append(frontier, fs)
			} else {
				carry = append(carry, fs)
			}
			// Seeding admittedRel with the relation each frontier node was
			// DISCOVERED by is what keeps a DirectionBoth walk from emitting one
			// edge on two pages. Re-reading a frontier node returns the edge that
			// reached it, which the issuing page already admitted; every node
			// this page expands is either in this spooled frontier (Via-seeded
			// here) or was discovered by this page itself (covered by the
			// in-page admittedRel below).
			if fs.Via != "" {
				admittedRel[fs.Via] = true
			}
			admitted.carry(fs.Node)
		}
		sort.Slice(carry, func(i, j int) bool { return carry[i].Node < carry[j].Node })
	}
	sort.Slice(frontier, func(i, j int) bool { return frontier[i].Node < frontier[j].Node })

	// Expanding level `depth` produces nodes at depth+1, so the bound is
	// crossed when depth+1 would exceed it. config.Limit.Exceeded is the whole
	// test: an unlimited bound is never exceeded, so the walk is bounded by the
	// graph, by the page budgets below and by the deadline instead.
	for ; len(frontier) > 0 && !o.MaxDepth.Exceeded(int64(depth+1)); depth++ {
		if err := checkWalk(ctx, o.Budget); err != nil {
			if !deadlineStop(err, o) {
				return walkState{}, err
			}
			// Out of time between levels, with edges already admitted: the
			// standing frontier becomes the continuation, exactly as a spent
			// page budget's does.
			o.Budget.deadlineHit = true
			return walkState{Depth: depth, Frontier: frontier, Admitted: admitted,
				LevelBoundary: true}, nil
		}
		level := make(map[model.NodeID]frontierState, len(frontier))
		nodes := make([]model.NodeID, 0, len(frontier))
		for _, st := range frontier {
			level[st.Node] = st
			nodes = append(nodes, st.Node)
		}

		rows, err := levelEdges(ctx, a, nodes, level, o, batch, admittedRel, skipOwner, skipKey)
		if err != nil {
			return walkState{}, err
		}
		// Only the level a cursor stopped inside is skipped; every level after
		// it is read whole.
		skipOwner, skipKey = "", ""
		// A level that spent the frontier budget is still emitted -- the edges
		// it did read are facts -- but the walk stops after it rather than
		// expanding a level it knows is incomplete.

		// One sequential pass over the spooled cumulative set answers the
		// whole level's membership questions at once; see visited.go for why
		// a per-node probe over a forward-only spool is not affordable.
		candidates := make([]model.NodeID, 0, len(rows))
		for _, row := range rows {
			candidates = append(candidates, row.neighbor)
		}
		if err := admitted.warm(ctx, candidates); err != nil {
			return walkState{}, err
		}

		next := carry
		carry = nil
		for _, row := range rows {
			if err := visit(row.owner, row.rel); err != nil {
				if errors.Is(err, errStopExpansion) {
					// A deliberate mid-level stop. The continuation re-reads
					// THIS level from the row just admitted and keeps the next
					// level as far as it was built, so no edge is read twice and
					// none is skipped.
					stopped := make([]frontierState, 0, len(frontier)+len(next))
					stopped = append(stopped, frontier...)
					stopped = append(stopped, next...)
					return walkState{Depth: depth, Frontier: stopped, Admitted: admitted}, nil
				}
				return walkState{}, err
			}
			admittedRel[row.rel.ID] = true
			o.Budget.edges++
			o.Budget.pageEdges++
			if admitted.has(row.neighbor) {
				continue
			}
			admitted.add(row.neighbor)
			o.Budget.visited++
			o.Budget.pageVisited++
			next = append(next, frontierState{
				Depth: depth + 1,
				Cost:  row.owner.Cost + Cost(row.rel.Kind),
				Node:  row.neighbor,
				Via:   row.rel.ID,
				Route: appendRoute(row.owner.Route, row.rel.ID),
			})
		}
		if o.Budget.frontierHit || o.Budget.deadlineHit {
			// The frontier byte ceiling -- and, for a paged traversal, the
			// query deadline -- SPILL rather than stopping: the level
			// as far as it was read, plus the next level as far as it was
			// built, become the continuation the caller spools. The resumed
			// page re-reads this level from (lastOwner, lastKey), so no edge is
			// read twice and none is skipped -- exactly the deliberate
			// mid-level stop above, reached by a different trigger.
			stopped := make([]frontierState, 0, len(frontier)+len(next))
			stopped = append(stopped, frontier...)
			stopped = append(stopped, next...)
			// A deadline can trip on this level's FIRST reader check, before it
			// collected a row. Then nothing here advanced the keyset position
			// and the one the page carries still names the previous level's
			// last row -- which, applied to this level, would drop every row
			// whose owner sorts below it. The frontier-byte spill cannot reach
			// this: its `spent > 0` guard means it always collected a row.
			return walkState{Depth: depth, Frontier: stopped, Admitted: admitted,
				LevelBoundary: len(rows) == 0}, nil
		}
		sort.Slice(next, func(i, j int) bool { return next[i].Node < next[j].Node })
		frontier = next
	}
	// A walk that falls out of the loop with an EMPTY frontier ran to
	// completion. One that still holds a frontier ran out of depth: those nodes
	// are admitted but their edges were never read, which is a truncation the
	// caller must be told about. Reporting it is the whole of row 14 -- see
	// DepthLimited for why no continuation is minted for it.
	if len(frontier) > 0 {
		return walkState{Depth: depth, Frontier: frontier, Admitted: admitted, DepthLimited: true}, nil
	}
	return walkState{Depth: depth, Admitted: admitted}, nil
}

// walkState is where a walk stopped. A page that stopped on its item limit
// turns it into the continuation the next page resumes from; a walk that ran to
// completion leaves Frontier empty and mints nothing.
type walkState struct {
	// Depth is the level that was being expanded when the walk stopped.
	Depth int
	// Frontier holds that level together with the next level as far as it was
	// built. Each record carries its own depth, so a resumed walk measures
	// MaxDepth from the original seeds rather than from its own frontier.
	Frontier []frontierState
	// Admitted is the cumulative admitted-node set. Only its bounded front is
	// in heap; the remainder is the continuation spool (visited.go).
	Admitted *visitedSet
	// LevelBoundary records that the walk stopped BETWEEN levels rather than
	// inside one: the level in Frontier has not been read at all yet. The
	// keyset position the page last emitted belongs to the level BEFORE it, so
	// a continuation must not carry it -- applied to the new level it would
	// silently drop every row whose owner sorts below that node. Only the
	// deadline stops here; every other stop is mid-level by construction.
	LevelBoundary bool
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

// edgeRow is one edge of a level attributed to the frontier node it left from,
// so visit receives that node's own state rather than a reconstructed one.
type edgeRow struct {
	owner    frontierState
	rel      model.Relation
	neighbor model.NodeID
}

// levelEdges reads every edge leaving one frontier level and returns them in
// the frozen (NodeID asc, RelationID asc) order. Nodes are sent in batches of
// at most batch ids, and each batch is keyset-paged by relation id, so the
// number of round trips grows with the frontier divided by the batch size --
// never with the frontier itself.
// skipOwner/skipKey, when set, are a resumed page's keyset position: every row
// at or before (skipOwner, skipKey) in the frozen emission order belongs to an
// earlier page and is dropped before it costs a frontier byte.
//
// That keyset resume is only sound if what the previous read KEPT was a true
// prefix of the frozen order -- otherwise the rows it never read that sort
// BELOW its last admitted row are dropped here and never seen again, which is
// a walk silently shrinking under a memory ceiling rather than spilling under
// it. A node chunk is keyset-paged by RELATION id, not by (owner, relation),
// so a chunk of several owners cut in the middle keeps exactly such a
// non-prefix. Two facts make an honest cut available anyway:
//
//   - CHUNK boundaries are frozen-order boundaries. `nodes` is ascending and
//     chunks are contiguous, so every row a chunk collects is owned by one of
//     its own nodes: an edge with both ends on the frontier is attributed to
//     the LOWER id, which lies in the earlier chunk and is collected there.
//     A cut after a completed chunk is therefore a prefix.
//   - a SINGLE-node chunk has one owner, so relation order is the frozen order
//     within it and a mid-chunk cut is a prefix too.
//
// So the ceiling cuts only at those two places. When it binds inside a chunk of
// several owners, that chunk's rows are rolled back -- rows, bytes and the
// collected-relation set alike -- and its node range is re-read one node at a
// time until the ceiling binds again, which it must, on a boundary that is now
// exact. The re-read costs round trips only for the one chunk a spill lands in,
// and only for as many of its nodes as the remaining budget holds.
func levelEdges(ctx context.Context, a Adjacency, nodes []model.NodeID, level map[model.NodeID]frontierState,
	o expandOptions, batch int, admittedRel map[model.RelationID]bool,
	skipOwner model.NodeID, skipKey model.RelationID) ([]edgeRow, error) {
	var (
		rows  []edgeRow
		spent int64
	)
	// The graph never asks for more rows than the wire may serve. The keyset
	// loop treats a short page as "read on", so asking for the node-chunk size
	// would cost nothing but an extra round trip -- and it would make the
	// storage layer record a page-bound resolution on EVERY traversal answer,
	// turning a disclosure that exists for real news into noise. It is derived
	// from the ORIGINAL batch, so the single-node re-read above keeps the same
	// wire page and does not turn into one round trip per edge.
	rowLimit := batch
	if rowLimit > model.MaxPageItems {
		rowLimit = model.MaxPageItems
	}
	collected := map[model.RelationID]bool{}

	// readChunk reads one contiguous node range to exhaustion, appending its
	// rows. It reports whether the read was CUT short -- by the frontier byte
	// ceiling or by the deadline, which the budget flags tell apart -- rather
	// than having reached the end of the range's edges.
	readChunk := func(chunk []model.NodeID) (bool, error) {
		after := model.RelationID("")
		for {
			if err := checkWalk(ctx, o.Budget); err != nil {
				if !deadlineStop(err, o) {
					return false, err
				}
				// The page ran out of time mid-level. The rows read so far are
				// facts and are kept when the cut is on an exact boundary; the
				// caller stops after this level and mints a continuation, and
				// the resumed page re-reads the level from the keyset position,
				// so no edge is lost and none is read twice.
				o.Budget.deadlineHit = true
				return true, nil
			}
			page, err := a.Edges(ctx, chunk, o.Direction, o.Kinds, after, rowLimit)
			if err != nil {
				return false, err
			}
			for _, rel := range page {
				if admittedRel[rel.ID] || collected[rel.ID] {
					continue
				}
				owner, neighbor, ok := attribute(rel, o.Direction, level)
				if !ok {
					// The reader returned an edge touching no frontier node.
					// Dropping it is the only honest answer -- attributing it
					// to an arbitrary node would invent a path.
					continue
				}
				row := edgeRow{owner: owner, rel: rel, neighbor: neighbor}
				if skipKey != "" && (row.owner.Node < skipOwner ||
					(row.owner.Node == skipOwner && row.rel.ID <= skipKey)) {
					continue
				}
				if spent > 0 && spent+edgeRowBytes(row) > o.FrontierBytes {
					// The level does not fit in the configured frontier budget:
					// spill what was read and let the continuation carry on,
					// rather than accumulating an unbounded hub in memory under
					// a bound the configuration says exists.
					//
					// `spent > 0` is what makes that terminate. A budget smaller
					// than ONE edge row would otherwise trip here before any row
					// was admitted, and since the resumed page re-reads the level
					// from the same keyset position it would trip at the same row
					// again: a cursor chain that returns no edge and never ends.
					// Every level-read therefore admits at least one edge --
					// exceeding the byte bound by at most one row -- which is the
					// same trade every other per-page budget here makes.
					o.Budget.frontierHit = true
					return true, nil
				}
				spent += edgeRowBytes(row)
				collected[rel.ID] = true
				rows = append(rows, row)
			}
			if len(page) == 0 {
				break
			}
			// A short page is NOT exhaustion: Adjacency.Edges promises "at
			// most limit rows", and the shipped reader clamps any limit above
			// model.MaxPageItems down to it, so a full frontier chunk returns
			// fewer rows than asked for on every page. Only an EMPTY page ends
			// the keyset walk; the advance below is what makes the next one
			// reachable.
			after = page[len(page)-1].ID
		}
		return false, nil
	}
	// rollback undoes an inexact cut: the rows, the bytes AND the collected
	// relation ids, which the re-read must be allowed to collect again or it
	// would skip them as duplicates and lose exactly what this fix restores.
	rollback := func(markRows int, markSpent int64) {
		for _, r := range rows[markRows:] {
			delete(collected, r.rel.ID)
		}
		rows, spent = rows[:markRows], markSpent
	}

chunks:
	for start := 0; start < len(nodes); start += batch {
		end := start + batch
		if end > len(nodes) {
			end = len(nodes)
		}
		markRows, markSpent := len(rows), spent
		cut, err := readChunk(nodes[start:end])
		if err != nil {
			return nil, err
		}
		if !cut {
			continue
		}
		if end-start == 1 {
			// One owner: relation order is the frozen order, so the cut is a
			// prefix and the keyset position resumes it exactly.
			break chunks
		}
		rollback(markRows, markSpent)
		if o.Budget.deadlineHit {
			// Out of time: there is none to spend re-reading. Dropping this
			// chunk's partial rows costs the next page a re-read of the range
			// and loses nothing -- a level whose every row was rolled back
			// carries no keyset position at all (walkState.LevelBoundary).
			break chunks
		}
		// The ceiling bound inside a chunk of several owners. Re-read the range
		// one owner at a time so it binds on an exact boundary instead.
		o.Budget.frontierHit = false
		for i := start; i < end; i++ {
			if cut, err = readChunk(nodes[i : i+1]); err != nil {
				return nil, err
			}
			if cut {
				break chunks
			}
		}
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].owner.Node != rows[j].owner.Node {
			return rows[i].owner.Node < rows[j].owner.Node
		}
		return rows[i].rel.ID < rows[j].rel.ID
	})
	return rows, nil
}

// attribute decides which end of an edge is the frontier node that reached it.
// Under DirectionBoth an edge may have both ends on the frontier; the lower
// NodeID owns it, so the emission order is a function of the facts alone.
func attribute(rel model.Relation, dir model.Direction,
	level map[model.NodeID]frontierState) (frontierState, model.NodeID, bool) {
	from, fromOK := level[rel.From]
	to, toOK := level[rel.To]
	switch dir {
	case model.DirectionOutgoing:
		return from, rel.To, fromOK
	case model.DirectionIncoming:
		return to, rel.From, toOK
	}
	switch {
	case fromOK && toOK:
		if rel.From <= rel.To {
			return from, rel.To, true
		}
		return to, rel.From, true
	case fromOK:
		return from, rel.To, true
	case toOK:
		return to, rel.From, true
	}
	return frontierState{}, "", false
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
	deadline := e.now().Add(e.limits.QueryTimeout)
	// The caller's own context, kept aside: a page the deadline ended still has
	// to be DELIVERED -- its endpoints hydrated and its continuation minted --
	// and both of those run after the walk's deadline has passed. See the
	// finish context below.
	parent := ctx
	ctx, cancel := context.WithDeadline(ctx, deadline)
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
	// The cumulative admitted-node set of THIS walk, append-only state in a
	// retained directory (visitedstore.go), exactly as impact and the package
	// rollup keep theirs. A resumed page reopens the one its predecessor left;
	// a first page creates one only if it actually mints a continuation, which
	// is why this is nil here and filled in at the mint below: a neighbours
	// query that fits one page -- the common one -- would otherwise pay a
	// fixed-size filter file and a manifest for state nothing will ever read.
	retain := resumeRetained(resume)
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
		lastOwner  model.NodeID
		lastKey    model.RelationID
	)
	endpoints := map[model.NodeID]bool{}
	for _, s := range req.Start {
		endpoints[s] = true
	}
	visit := func(owner frontierState, rel model.Relation) error {
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
		relations = append(relations, rel)
		// The keyset position a continuation resumes from: the owner names the
		// frontier node whose chunk the row came from, the relation id the row.
		lastOwner, lastKey = owner.Node, rel.ID
		endpoints[rel.From] = true
		endpoints[rel.To] = true
		return nil
	}
	state, err := expand(ctx, e.adjacency, req.Start, expandOptions{
		Direction:     dir,
		Kinds:         kinds,
		MaxDepth:      maxDepth,
		Budget:        b,
		BatchSize:     adjacencyBatch,
		FrontierBytes: e.limits.FrontierBytes,
		DeadlineStops: true,
		Resume:        resume,
	}, visit)
	if err != nil {
		return model.GraphResult{}, err
	}

	// Delivering a deadline-stopped page needs a live context: `ctx` is past
	// its deadline by construction, so hydrating the endpoints and writing the
	// continuation spool on it would fail with the very CTX_QUERY_DEADLINE this
	// stop exists to replace. The grace runs on the CALLER's context, so a
	// caller-set deadline still bounds it, and it is the same query_timeout the
	// walk had -- delivery is bounded work (one batched node read of at most a
	// page of endpoints, one spool write), not more walking.
	finish := ctx
	if b.deadlineHit {
		var cancelFinish context.CancelFunc
		finish, cancelFinish = context.WithTimeout(parent, e.limits.QueryTimeout)
		defer cancelFinish()
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
	// A level cut short by the frontier budget is truncation the caller must
	// see; the visitor's own reason is more specific, so it wins when both hold.
	if reason == "" && b.frontierHit {
		reason = reasonFrontierBytes
	}
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
	if state.LevelBoundary {
		// The stop was between levels: the emitted keyset position belongs to
		// the level already finished, so the continuation starts the frontier's
		// own level from its beginning.
		lastOwner, lastKey = "", ""
	}
	var nextCursor string
	if len(state.Frontier) > 0 && !state.DepthLimited {
		// The cumulative set is NOT materialized into a slice here: only the
		// nodes THIS page admitted are, and every earlier page's run stays
		// where that page wrote it. The resumed frontier is deliberately left
		// out -- every node that ever reaches a frontier was added by the page
		// that discovered it, so it is already in that page's own run.
		if retain == nil {
			if retain, err = openRetainedWalk(e.walkScratchDir(), e.visitedFilterBytes()); err != nil {
				return model.GraphResult{}, err
			}
		}
		nextCursor, err = e.nextTraversalCursor(finish, b, continuation{
			Endpoint:  endpoint,
			QueryHash: queryHash,
			Depth:     state.Depth,
			LastOwner: lastOwner,
			LastKey:   lastKey,
			Frontier:  state.Frontier,
			Visited:   state.Admitted.addedNodes(),
			Retain:    retain,
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
