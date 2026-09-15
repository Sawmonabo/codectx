package graph

import (
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
	// isDeadline, not a typed-only test: a deadline that fell inside a STORAGE
	// read arrives as the context's own untyped error and ends the page exactly
	// as the engine's own typed one does.
	return isDeadline(err)
}

// edgeRowBytes is what one collected edge of a frontier level costs in heap:
// the edgeRow struct -- a frontierState copy, whose Route backing array is
// SHARED with the frontier record rather than copied here, plus one Edge.
//
// It is a CONSTANT, where it used to sum the lengths of four identifier
// strings, because a level holds SURROGATES: every row of every repository
// costs the same, and Limits.FrontierBytes now bounds a count of rows rather
// than a guess at how long their names were. It reads deliberately high -- the
// budget is a memory ceiling, and an estimate that read low would let the
// ceiling be crossed before the walk noticed.
const edgeRowBytes = 128

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

// edgeRow is one edge of a level attributed to the frontier node it left from,
// so visit receives that node's own state rather than a reconstructed one.
type edgeRow struct {
	owner frontierState
	edge  Edge
}

// expand is the ONE batched BFS. visit is called once per admitted edge in the
// frozen (canonical owner asc, canonical relation asc) order and may return
// errStopExpansion to end the walk, which expand reports as a clean finish: the
// visitor stopped deliberately and owns whatever truncation flag it set.
//
// expand does ONE GraphReader.Neighbours scan per frontier LEVEL, never one per
// node and never one per node chunk: the whole level's surrogates go into one
// ascending scan of the packed adjacency, which is the layout's reason for
// existing. A level of 40 000 owners costs one call.
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
	var (
		frontier []frontierState
		// carry is the NEXT level as the page that issued the cursor had
		// already built it, held aside until the re-read level completes.
		carry []frontierState
		depth int
		// from is where this level's scan resumes. It REPLACES the
		// (lastOwner, lastKey) keyset position: the packed reader resumes a
		// scan at a stored-entry index, which is exact, where the keyset pair
		// could only name a position in an order several independent reads had
		// to be stitched into.
		from EdgePos
	)
	if o.Resume == nil {
		if frontier, err = seedFrontier(ctx, r, seeds, o); err != nil {
			return walkState{}, err
		}
		if err := o.Retain.commitLevel(frontier, levelRefs(frontier)); err != nil {
			return walkState{}, err
		}
	} else {
		// A resume never re-enters the seeds: they are already in the bitset
		// the issuing page committed, and re-admitting them would spend the
		// cumulative visited budget a second time for the same nodes. The
		// frontier comes from the retained level file the continuation adopted,
		// whose bits have already been re-applied (cursor.go); that adoption is
		// idempotent because the bitset counts bit transitions.
		depth, from = o.Resume.Cursor.Depth, o.Resume.Cursor.LevelPos
		for _, fs := range o.Resume.Frontier {
			if fs.Depth == depth {
				frontier = append(frontier, fs)
			} else {
				carry = append(carry, fs)
			}
		}
		sortFrontier(carry)
	}
	sortFrontier(frontier)

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
			return walkState{Depth: depth, Frontier: joinLevels(frontier, carry),
				LevelPos: from, LevelBoundary: true}, nil
		}
		rows, pos, err := readLevel(ctx, r, frontier, o, codes, from)
		if err != nil {
			return walkState{}, err
		}
		// Naming the level is a READ and can run out of time like any other.
		// Nothing of this level has been taken, so the page ends here with the
		// level standing at the position the scan reached.
		if err := o.Names.fill(ctx, r, frontier, rows); err != nil {
			if !deadlineStop(err, o) {
				return walkState{}, err
			}
			o.Budget.deadlineHit = true
			return walkState{Depth: depth, Frontier: joinLevels(frontier, carry),
				LevelPos: from, LevelBoundary: true}, nil
		}
		rows = orderLevel(rows, o.Names)
		seen, err := o.Retain.membership(rows)
		if err != nil {
			return walkState{}, err
		}
		// Only the level a cursor stopped inside is resumed from a position;
		// every level after it is read whole.
		from = EdgePos{}

		next := carry
		carry = nil
		// How many of THIS level's rows the visitor has taken. A stop on the
		// first of them is a stop at a level BOUNDARY however it was triggered:
		// nothing here has advanced the scan position, so the one the page
		// carries still names the level before this one.
		taken := 0
		var (
			admitted []NodeRef
			emitted  []RelRef
		)
		// commit writes what this level actually took, in the order ADR-0005
		// Decision 2 froze, before any of it can be reported: records first,
		// then the bits that mark them. It runs on every exit from the loop
		// body, including the ones that end the page.
		commit := func() error {
			return o.Retain.commitTaken(joinLevels(frontier, next), admitted, emitted)
		}
		for _, row := range rows {
			if err := visit(row.owner, row.edge); err != nil {
				stop := deadlineStop(err, o)
				if !stop && !errors.Is(err, errStopExpansion) {
					return walkState{}, err
				}
				if stop {
					// The VISITOR ran out of time. It is not a pure predicate:
					// the rollup teed off this walk does reads of its own, so
					// the request deadline lands inside the visitor as often as
					// inside the walk's own scan. The page ends exactly as the
					// deliberate stop does.
					o.Budget.deadlineHit = true
				}
				// A mid-level stop, and the one cut whose exact position the
				// reader cannot report: the rows after this one were DECODED
				// but never emitted, and the scan position names only what was
				// decoded. The continuation therefore re-reads this level from
				// its BEGINNING, and the cumulative emitted-relation set drops
				// every entry an earlier page already served -- inside the
				// scan, before it can cost a frontier byte, which is what makes
				// the re-read terminate instead of spending the whole ceiling
				// on edges it is about to discard.
				//
				// Nothing is lost and nothing is repeated; what it costs is a
				// re-decode of the level's entries once per page that stops
				// inside it.
				if err := commit(); err != nil {
					return walkState{}, err
				}
				return walkState{Depth: depth, Frontier: joinLevels(frontier, next),
					LevelBoundary: taken == 0}, nil
			}
			taken++
			emitted = append(emitted, row.edge.Rel)
			o.Budget.edges++
			o.Budget.pageEdges++
			if seen[row.edge.Neighbour] {
				continue
			}
			seen[row.edge.Neighbour] = true
			admitted = append(admitted, row.edge.Neighbour)
			o.Budget.visited++
			o.Budget.pageVisited++
			next = append(next, frontierState{
				Depth: depth + 1,
				Cost:  row.owner.Cost + costs.of(row.edge.Kind),
				Node:  row.edge.Neighbour,
				Via:   row.edge.Rel,
				Route: appendRoute(row.owner.Route, row.edge.Rel),
			})
		}
		if err := commit(); err != nil {
			return walkState{}, err
		}
		if o.Budget.frontierHit || o.Budget.deadlineHit {
			// The frontier byte ceiling -- and, for a paged traversal, the
			// query deadline -- SPILL rather than stopping: the level as far as
			// it was read, plus the next level as far as it was built, become
			// the continuation. The resumed page re-reads this level from pos,
			// so no edge is read twice and none is skipped.
			//
			// A deadline can trip on this level's FIRST reader check, before it
			// collected a row. Then nothing here advanced the scan position and
			// the level must be resumed from where the scan stopped, which is
			// what pos carries either way.
			return walkState{Depth: depth, Frontier: joinLevels(frontier, next),
				LevelPos: pos, LevelBoundary: len(rows) == 0}, nil
		}
		sortFrontier(next)
		frontier = next
	}
	// A walk that falls out of the loop with an EMPTY frontier ran to
	// completion. One that still holds a frontier ran out of depth: those nodes
	// are admitted but their edges were never read, which is a truncation the
	// caller must be told about. Reporting it is the whole of row 14 -- see
	// DepthLimited for why no continuation is minted for it.
	if len(frontier) > 0 {
		return walkState{Depth: depth, Frontier: frontier, DepthLimited: true}, nil
	}
	return walkState{Depth: depth}, nil
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
	//
	// It is also what the retained level file holds: the walk spools it BEFORE
	// marking its bits, so a page cut between the two writes is repaired by
	// re-applying the file rather than losing the nodes it names.
	Frontier []frontierState
	// LevelPos is where this level's adjacency scan stopped: the owner and the
	// index of the stored entry that was NOT delivered. It REPLACES the
	// (LastOwner, LastKey) keyset position, which had to name a row in an order
	// stitched together from several independent reads; a scan position is
	// exact, so a resumed level delivers exactly the entries the cut one did
	// not.
	LevelPos EdgePos
	// LevelBoundary records that the walk stopped BETWEEN levels rather than
	// inside one: no row of the level in Frontier has been taken yet. It is
	// reported because the caller discloses it, and because a page that took
	// nothing must not be read as one that made progress.
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

// seedFrontier resolves the request's seeds to surrogates and builds depth 0.
//
// Seeds enter in request order, de-duplicated, then sorted, so two requests
// naming the same seeds differently answer identically. A seed the generation
// cannot see resolves to 0: it has no adjacency and no bit, so it expands to
// nothing -- exactly what an unknown node did before, which is why it is still
// COUNTED against the visited budget and the disclosed visited_count.
func seedFrontier(ctx context.Context, r GraphReader, seeds []model.NodeID,
	o expandOptions) ([]frontierState, error) {
	ids := dedupeNodes(seeds)
	refs, err := r.Resolve(ctx, ids)
	if err != nil {
		return nil, err
	}
	if len(refs) != len(ids) {
		return nil, internalErr("graph: the reader resolved a different number of seeds than it was given")
	}
	frontier := make([]frontierState, 0, len(refs))
	for _, ref := range refs {
		o.Budget.visited++
		o.Budget.pageVisited++
		if ref == 0 {
			continue
		}
		frontier = append(frontier, frontierState{Node: ref})
	}
	sortFrontier(frontier)
	return frontier, nil
}

// sortFrontier puts a level into ascending surrogate order, which is what
// GraphReader.Neighbours requires of the refs it is given and what makes the
// bitset's page cache walk forward.
func sortFrontier(level []frontierState) {
	sort.Slice(level, func(i, j int) bool { return level[i].Node < level[j].Node })
}

// levelRefs is a level's node surrogates, ascending and unique.
func levelRefs(level []frontierState) []NodeRef {
	refs := make([]NodeRef, 0, len(level))
	for _, fs := range level {
		refs = append(refs, fs.Node)
	}
	slices.Sort(refs)
	return slices.Compact(refs)
}

// joinLevels concatenates the level being expanded with the next level as far
// as it was built. Both travel as one continuation: each record carries its own
// depth, so the resumed walk splits them again.
func joinLevels(a, b []frontierState) []frontierState {
	out := make([]frontierState, 0, len(a)+len(b))
	return append(append(out, a...), b...)
}

// readLevel reads one frontier level's edges in ONE GraphReader.Neighbours scan
// over the level's ascending surrogates, starting after from.
//
// It returns the rows it collected and the position to resume at. The scan is
// cut by the frontier byte ceiling and by the deadline, and both cuts are
// EXACT: the reader reports the position of the entry it did not deliver, so
// the level resumes there and neither loses an entry nor repeats one. That is
// what retires the old keyset resume, whose soundness depended on the rows kept
// being a true prefix of an order stitched from independently paged reads --
// the property that forced a chunk to be rolled back and re-read one node at a
// time whenever the ceiling bound in the middle of it.
func readLevel(ctx context.Context, r GraphReader, level []frontierState, o expandOptions,
	codes []KindCode, from EdgePos) ([]edgeRow, EdgePos, error) {
	owners := make(map[NodeRef]frontierState, len(level))
	for _, fs := range level {
		// The level is ascending, so the FIRST record for a surrogate wins; a
		// level never holds one twice, because admission is what puts a node
		// on it and a node is admitted once.
		if _, ok := owners[fs.Node]; !ok {
			owners[fs.Node] = fs
		}
	}
	var (
		rows  []edgeRow
		spent int64
		seen  int
	)
	emitted := o.Retain.emitted
	pos, err := r.Neighbours(ctx, levelRefs(level), o.Direction, codes, from, func(e Edge) error {
		if seen%edgeScanCheckEvery == 0 {
			if err := checkWalk(ctx, o.Budget); err != nil {
				if !deadlineStop(err, o) {
					return err
				}
				// The page ran out of time mid-level. The rows read so far are
				// facts; the caller stops after this level and mints a
				// continuation from the position this stop reports.
				o.Budget.deadlineHit = true
				return ErrStopScan
			}
		}
		seen++
		// Already served by an earlier page or an earlier level of this walk.
		// It is dropped HERE, before it costs a frontier byte, so a level
		// re-read after a mid-level stop makes progress rather than spending
		// its whole ceiling on entries it would discard afterwards.
		done, err := emitted.test(uint64(e.Rel))
		if err != nil {
			return err
		}
		if done {
			return nil
		}
		owner, ok := owners[e.Owner]
		if !ok {
			// The reader delivered an entry for a node that is not on this
			// level. Dropping it is the only honest answer -- attributing it to
			// an arbitrary node would invent a path.
			return nil
		}
		if spent > 0 && spent+edgeRowBytes > o.FrontierBytes {
			// The level does not fit in the configured frontier budget: spill
			// what was read and let the continuation carry on, rather than
			// accumulating an unbounded hub in memory under a bound the
			// configuration says exists.
			//
			// `spent > 0` is what makes that terminate. A budget smaller than
			// ONE row would otherwise trip before any row was collected, and
			// the resumed page would trip at the same entry again: a cursor
			// chain that returns no edge and never ends. Every level therefore
			// collects at least one row -- exceeding the byte bound by at most
			// one -- which is the trade every other per-page budget here makes.
			o.Budget.frontierHit = true
			return ErrStopScan
		}
		spent += edgeRowBytes
		rows = append(rows, edgeRow{owner: owner, edge: e})
		return nil
	})
	if err != nil {
		if !deadlineStop(err, o) {
			return nil, EdgePos{}, err
		}
		// The deadline fell INSIDE the read rather than on the check above it:
		// the reader returned the context's own error. The rows already
		// collected are facts, and the position the reader reported is where
		// the level resumes.
		o.Budget.deadlineHit = true
	}
	return rows, pos, nil
}

// orderLevel puts a level's rows into the frozen emission order and removes the
// duplicate an undirected read produces.
//
// The order is (canonical owner asc, canonical relation asc) and the dedup
// keeps the LOWER CANONICAL owner, both exactly as they were before the walk
// carried surrogates. That is not a detail: surrogate order is a property of
// the BUILD -- a fresh index and a delta-built index of the same tree assign
// different ones -- so ordering or deduplicating on refs would make which route
// survives, and therefore an entry's parent, depth and reasons, depend on how
// the index was produced. ADR-0005 Decision 2 exists to prevent exactly that.
//
// Under DirectionBoth an edge with both ends on the level is delivered twice,
// once from each owner's list. It is ONE edge: keeping both would count it
// twice in edge_count and serve it twice.
func orderLevel(rows []edgeRow, names *levelNames) []edgeRow {
	sort.SliceStable(rows, func(i, j int) bool {
		a, b := rows[i], rows[j]
		if ao, bo := names.node(a.owner.Node), names.node(b.owner.Node); ao != bo {
			return ao < bo
		}
		return names.rel(a.edge.Rel) < names.rel(b.edge.Rel)
	})
	// The two deliveries of one undirected edge sit under DIFFERENT owners, so
	// they are not adjacent in this order and a run-length dedup cannot see
	// them. The set is keyed on the relation surrogate and is sized by the
	// level, which Limits.FrontierBytes already bounds.
	kept := make(map[RelRef]struct{}, len(rows))
	out := rows[:0]
	for _, row := range rows {
		if _, dup := kept[row.edge.Rel]; dup {
			continue
		}
		kept[row.edge.Rel] = struct{}{}
		out = append(out, row)
	}
	return out
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
	// admitted-node bitset and the cumulative emitted-relation bitset. A
	// resumed page reopens the one its predecessor left.
	//
	// It is opened UNCONDITIONALLY now, where a first page used to create one
	// only if it actually minted a continuation. The bitset IS the membership
	// set the walk tests against, so a walk with nowhere to keep it is not a
	// cheaper walk, it is a wrong one. What the one-page query pays for it is a
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
	reader, err := e.consumerReader()
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
		BatchSize:     adjacencyBatch,
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
	var nextCursor string
	if len(state.Frontier) > 0 && !state.DepthLimited {
		// Nothing of the walk is materialized here. The frontier is already in
		// the retained level file, committed as each level closed, and the
		// admitted nodes are already bits beside it; the token names the
		// directory rather than a copy of what is in it.
		nextCursor, err = e.nextTraversalCursor(finish, b, continuation{
			Endpoint:  endpoint,
			QueryHash: queryHash,
			Depth:     state.Depth,
			LevelPos:  state.LevelPos,
			Frontier:  state.Frontier,
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

// ascending sorts a set of surrogates and removes its duplicates. It is the
// precondition every cumulative-set call has -- the page cache is walked
// forward, so an unordered batch would touch one page many times -- and it is
// applied here rather than assumed of the caller, because an unordered batch
// would merely be slow and nothing downstream would report it.
func ascending[T NodeRef | RelRef](refs []T) []T {
	slices.Sort(refs)
	return slices.Compact(refs)
}

// fill resolves one level's canonical ids: the owners and the nodes their edges
// reach, and the relations those edges are plus the relations on the routes
// that reached the owners.
//
// It is TWO batched primary-key reads for the whole level, whatever its size --
// ADR-0005 Decision 2's "one batched read per committed level". The route
// relations are included because a ranking record carries its whole path in
// canonical form, and every element of a route is the admitting edge of some
// earlier level: resolving them here, deduplicated against the level's own
// edges, is what avoids either a per-record round trip or a map of canonical
// relation ids that grows with the walk.
func (n *levelNames) fill(ctx context.Context, r GraphReader, level []frontierState, rows []edgeRow) error {
	if n == nil {
		return nil
	}
	nodes := make([]NodeRef, 0, len(level)+len(rows))
	rels := make([]RelRef, 0, len(rows)+len(level))
	for _, fs := range level {
		nodes = append(nodes, fs.Node)
		if fs.Via != 0 {
			rels = append(rels, fs.Via)
		}
		rels = append(rels, fs.Route...)
	}
	for _, row := range rows {
		nodes = append(nodes, row.edge.Neighbour)
		rels = append(rels, row.edge.Rel)
	}
	nodes, rels = ascending(nodes), ascending(rels)
	nodeIDs, err := r.NodeIDs(ctx, nodes)
	if err != nil {
		return err
	}
	if len(nodeIDs) != len(nodes) {
		return internalErr("graph: the reader named a different number of nodes than it was given")
	}
	relIDs, err := r.RelationIDs(ctx, rels)
	if err != nil {
		return err
	}
	if len(relIDs) != len(rels) {
		return internalErr("graph: the reader named a different number of relations than it was given")
	}
	// Rebuilt, never extended: a map kept across levels would grow with the
	// walk, which is the repository-sized heap structure the surrogate walk
	// exists to remove.
	n.nodes = make(map[NodeRef]model.NodeID, len(nodes))
	n.rels = make(map[RelRef]model.RelationID, len(rels))
	for i, ref := range nodes {
		n.nodes[ref] = nodeIDs[i]
	}
	for i, ref := range rels {
		n.rels[ref] = relIDs[i]
	}
	return nil
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
