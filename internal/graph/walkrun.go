package graph

import (
	"bufio"
	"context"
	"errors"
	"os"
	"time"

	"github.com/Sawmonabo/codectx/internal/model"
	"github.com/Sawmonabo/codectx/internal/pagination"
)

// This file is ruling P2's first half: the in-process walk-to-completion, and
// the two passes that turn what it admitted into one globally ranked answer.
//
// The three functions below carry the doc comments impactrank.go froze for
// them; they live here rather than beside the frozen record shapes so the
// rollup lane's own fill-in of rankPairs and this lane's fill-in of the walk
// never edit the same lines.

// runWalkToCompletion expands seeds until the walk is EXHAUSTED, not until a
// page is full, calling visit once per admitted edge exactly as expand does.
//
// It is the seam ruling P2 requires: the request that mints the answer must
// see every admitted edge before anything is ranked, and the page-bounded
// expand cannot. Internally it chains expand's own continuation -- the spooled
// resumable frontier -- IN PROCESS, without minting or verifying a signed
// token per internal page, so peak heap stays a function of one internal
// page's frontier and never of the reachable set.
//
// Three things make that chaining honest rather than a loop around a bounded
// walk:
//
//   - the PER-PAGE work counters are reset at each internal boundary and the
//     CUMULATIVE ones are not. budget.pageVisited/pageEdges are what the
//     visitor compares max_visited and max_edges against, so carrying them
//     across the chain would end the whole walk at the first internal page and
//     serve a partial blast radius as a complete one; budget.visited/edges are
//     the answer's disclosed spend and keep growing.
//   - the cumulative admitted-node set is appended to ONE scratch file for the
//     whole run rather than copied spool-to-spool per internal page. A copy per
//     page is O(pages x nodes) -- the cost cursor.go's one-fresh-spool-per-page
//     rule exists to avoid between requests, and no cheaper inside one.
//   - the keyset position is recorded here, from the visitor, because expand
//     does not report it and a mid-level internal boundary must resume exactly
//     where the last ADMITTED row left off.
//
// The query deadline ends a PAGE and never the answer (ruling P3): a deadline
// reached mid-walk returns the walkState the walk had built, with its frontier
// intact for the caller to persist into the `f` cursor, and a nil error.
//
// Owned by lane P-b.
func (e *Engine) runWalkToCompletion(ctx context.Context, seeds []model.NodeID, o expandOptions,
	visit func(frontierState, model.Relation) error) (walkState, error) {
	if o.Budget == nil {
		return walkState{}, (&model.Error{Code: model.CodeInternal,
			Message: "graph expansion requires a budget"}).WithDetail("operation", "run_walk_to_completion")
	}
	var (
		lastOwner model.NodeID
		lastKey   model.RelationID
	)
	// The keyset position is taken AFTER the visitor accepted the row, which is
	// exactly when impactAccumulator records it: a row the visitor refused with
	// errStopExpansion was not admitted, and resuming past it would drop it.
	wrapped := func(fs frontierState, rel model.Relation) error {
		if err := visit(fs, rel); err != nil {
			return err
		}
		lastOwner, lastKey = fs.Node, rel.ID
		return nil
	}

	// The nodes every earlier link of the chain admitted. Nil until the first
	// internal boundary: a walk that finishes in one expand pays no file.
	var scratch *visitedScratch
	release := func() {
		if scratch != nil {
			scratch.release()
		}
	}
	carried := outerVisited(o.Resume)
	for {
		state, err := expand(ctx, e.adjacency, seeds, o, wrapped)
		if err != nil {
			release()
			return walkState{}, err
		}
		state.Carried, state.ReleaseCarried = carried, release
		switch {
		case len(state.Frontier) == 0 || state.DepthLimited:
			// Exhausted, or stopped at the user-set depth bound -- the one stop
			// that is an answer-level truncation rather than an internal page
			// boundary, and the one expand refuses to resume.
			return state, nil
		case o.Budget.deadlineHit:
			// Ruling P3: out of time with the walk unfinished. The frontier
			// this state carries becomes the `f` continuation and the next
			// request carries the walk on; nothing is ranked or served here.
			return state, nil
		}
		// An internal page boundary. Everything it spent against the per-page
		// work budgets is returned, so the next link expands rather than
		// stopping on a budget the previous link exhausted.
		o.Budget.pageVisited, o.Budget.pageEdges = 0, 0
		o.Budget.frontierHit = false
		if err := checkWalk(ctx, o.Budget); err != nil {
			// The deadline can fall between links, where expand never saw it.
			// Handled here rather than by deadlineStop, whose "this page
			// admitted an edge" guard reads the per-page counter this boundary
			// has just reset.
			var typed *model.Error
			if !errors.As(err, &typed) || typed.Code != model.CodeQueryDeadline {
				release()
				return walkState{}, err
			}
			o.Budget.deadlineHit = true
			return state, nil
		}
		if scratch == nil {
			if scratch, err = newVisitedScratch(e.walkScratchDir()); err != nil {
				return walkState{}, err
			}
			carried = chainVisited(outerVisited(o.Resume), scratch.stream)
		}
		if err := scratch.append(state.Admitted.newlyAdmitted()); err != nil {
			release()
			return walkState{}, err
		}
		// The next link resumes from this one exactly as a signed continuation
		// would -- same frontier, same keyset position, same cumulative visited
		// set -- with the signer, the lease and the spool left out, because
		// nothing here outlives the request.
		resume := &resumeState{
			Cursor:   traversalCursor{Depth: state.Depth, LastOwner: lastOwner, LastKey: lastKey},
			Budget:   o.Budget,
			Frontier: state.Frontier,
			Visited:  carried,
			Release:  func() {},
		}
		if state.LevelBoundary {
			// The frontier has not been read at all yet, so the keyset position
			// names the level BEFORE it; carrying it would drop every row whose
			// owner sorts below that node (traverse.go states the rule).
			resume.Cursor.LastOwner, resume.Cursor.LastKey = "", ""
		}
		o.Resume = resume
		// Seeds are not re-entered on a resume; expand reads the frontier
		// instead. Passing them again would be harmless but misleading.
		seeds = nil
	}
}

// outerVisited is the cumulative set the REQUEST arrived with: the spool of the
// cursor being resumed, or nothing on the first request of a walk.
func outerVisited(r *resumeState) visitedStream {
	if r == nil {
		return nil
	}
	return r.Visited
}

// chainVisited replays two cumulative sets as one. Order does not matter to a
// membership sweep, and a node present in both is answered by whichever comes
// first; visitedSet.warm already tolerates a repeat.
func chainVisited(first, second visitedStream) visitedStream {
	if first == nil {
		return second
	}
	return func(ctx context.Context, fn func(model.NodeID) error) error {
		if err := first(ctx, fn); err != nil {
			return err
		}
		return second(ctx, fn)
	}
}

// visitedScratch is the append-only file the internal links of one
// walk-to-completion record their admitted nodes in.
//
// It is NOT a pagination spool: a spool is continuation state bound to a lease
// and a query hash, and this never outlives the request that wrote it. It is a
// plain length-free line file because a model.NodeID is hex and cannot contain
// a separator.
type visitedScratch struct {
	f *os.File
	w *bufio.Writer
}

// newVisitedScratch opens one under dir.
func newVisitedScratch(dir string) (*visitedScratch, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, &model.Error{Code: model.CodeInternal,
			Message: "graph: opening the walk scratch directory: " + err.Error()}
	}
	f, err := os.CreateTemp(dir, "graph-visited-*")
	if err != nil {
		return nil, &model.Error{Code: model.CodeInternal,
			Message: "graph: opening the walk scratch file: " + err.Error()}
	}
	return &visitedScratch{f: f, w: bufio.NewWriter(f)}, nil
}

// append records one internal page's admissions.
func (s *visitedScratch) append(ids []model.NodeID) error {
	for _, id := range ids {
		if _, err := s.w.WriteString(string(id)); err != nil {
			return scratchErr(err)
		}
		if err := s.w.WriteByte('\n'); err != nil {
			return scratchErr(err)
		}
	}
	return s.w.Flush()
}

// stream replays every node recorded so far, from the start of the file. The
// writer is flushed by append, so a replay always sees every completed link.
func (s *visitedScratch) stream(ctx context.Context, fn func(model.NodeID) error) error {
	f, err := os.Open(s.f.Name())
	if err != nil {
		return scratchErr(err)
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 4096), model.MaxIdentifierBytes+1)
	for sc.Scan() {
		if err := ctx.Err(); err != nil {
			return typedContextError(ctx, err)
		}
		if err := fn(model.NodeID(sc.Text())); err != nil {
			return err
		}
	}
	if err := sc.Err(); err != nil {
		return scratchErr(err)
	}
	return nil
}

// release closes and removes the file. It is deferred by whoever consumed the
// walk, because the continuation spill reads the stream after the walk returns.
func (s *visitedScratch) release() {
	_ = s.f.Close()
	_ = os.Remove(s.f.Name())
}

func scratchErr(err error) error {
	return &model.Error{Code: model.CodeInternal, Message: "graph: the walk scratch file: " + err.Error()}
}

// walkScratchDir is where this request's scratch file and sort runs spill: the
// spool store's own sort directory, so a query's temporary files sit in one
// place (pagination.Spools.SortDir states why sort runs are not charged against
// the continuation byte budget). An engine with no spool store has no
// continuations either and falls back to the process temp directory.
func (e *Engine) walkScratchDir() string {
	if e.spools != nil {
		return e.spools.SortDir()
	}
	return os.TempDir()
}

// rankImpact folds and orders every record the completed walk admitted, and
// returns the whole ranked answer as a re-iterable sorted run on disk.
//
// emit is called once and streams the walk's records in; it is a callback
// rather than a slice because the point of the pass is that no caller ever
// holds the answer. Pass 1 keys lessByNode and folds foldImpact, pass 2 keys
// lessByRank and does not fold. Close the returned run.
//
// Owned by lane P-b.
func (e *Engine) rankImpact(ctx context.Context,
	emit func(add func(impactRecord) error) error) (*pagination.SortedRun[impactRecord], error) {
	dir, runBytes := e.walkScratchDir(), pagination.SortRunBytes(e.limits.FrontierBytes)
	pass1, err := pagination.NewExternalSort(dir, "graph-impact-fold-", 0,
		encodeImpactRecord, decodeImpactRecord, lessByNode)
	if err != nil {
		return nil, err
	}
	defer pass1.Close()
	pass1.WithFold(foldImpact).WithRunBytes(runBytes, sizeOfImpactRecord)
	if err := emit(pass1.Add); err != nil {
		return nil, err
	}
	folded, err := pass1.Sorted()
	if err != nil {
		return nil, err
	}
	// The folded run is the input to pass 2 and nothing else; it is closed as
	// soon as pass 2 has read it, so only ONE of the two sorted files is on
	// disk by the time the answer is served.
	defer folded.Close()

	pass2, err := pagination.NewExternalSort(dir, "graph-impact-rank-", 0,
		encodeImpactRecord, decodeImpactRecord, lessByRank)
	if err != nil {
		return nil, err
	}
	defer pass2.Close()
	pass2.WithRunBytes(runBytes, sizeOfImpactRecord)
	if err := folded.Each(func(r impactRecord) error {
		if err := ctx.Err(); err != nil {
			return typedContextError(ctx, err)
		}
		return pass2.Add(r)
	}); err != nil {
		return nil, err
	}
	return pass2.Sorted()
}

// servePage reads at most limit records out of the spool tail names, starting
// at tail.Offset, and returns them with the handle the NEXT page continues
// from. The records after the page are copied straight into a fresh spool, so
// neither the page nor the continuation ever holds the remainder in heap.
//
// c is the binding the spool was written under and now the clock the lease is
// checked against, exactly as pagination.Spools.Open takes them. decode is the
// record codec -- decodeImpactRecord or decodePairRecord -- which is what lets
// one page reader serve both ranked answers; the two can never be crossed,
// because a cursor is bound to the endpoint that issued it.
//
// The returned handle's Offset is how many records of THIS spool the page
// consumed, which is the skip the caller hands to rankedSpoolTail when it
// copies the remainder forward. A cursor minted from that copy therefore always
// carries RankOffset zero: one fresh spool per page (cursor.go) means the next
// spool begins at the next record.
//
// Owned by lane P-b (dispatch), declared by P-a for P-INT.
func servePage[T any](ctx context.Context, spools *pagination.Spools, c pagination.Cursor, now time.Time,
	tail rankedTail, limit int, decode func([]byte) (T, error)) ([]T, rankedTail, error) {
	if spools == nil {
		return nil, rankedTail{}, cursorInvalid("continuation state has expired or was released")
	}
	// The capacity is the page bound the caller already clamped to
	// model.MaxPageItems, so this allocation cannot track the answer.
	out := make([]T, 0, limit)
	at := 0
	header := true
	var total int64
	err := spools.Open(ctx, c, now, func(record []byte) error {
		if err := ctx.Err(); err != nil {
			return typedContextError(ctx, err)
		}
		if header {
			header = false
			h, err := decodeRankedHeader(record)
			if err != nil {
				return err
			}
			total = h.Total
			return nil
		}
		if at++; at <= tail.Offset || len(out) == limit {
			// Before the page, or past it: past-the-page records are neither
			// decoded nor kept -- rankedSpoolTail walks them again straight
			// into the next spool.
			return nil
		}
		v, err := decode(record)
		if err != nil {
			return err
		}
		out = append(out, v)
		return nil
	})
	if err != nil {
		return nil, rankedTail{}, err
	}
	next := rankedTail{
		SpoolID: c.SpoolID, LeaseID: c.LeaseID,
		Offset: tail.Offset + len(out),
		Served: tail.Served + int64(len(out)),
		Total:  total,
	}
	if next.done() {
		// Nothing follows this page: the caller mints no continuation, and the
		// empty SpoolID is what says so.
		next.SpoolID = ""
	}
	return out, next, nil
}

// rankedSpoolTail streams the records after the first skip of a ranked spool
// into the sink that writes the next one, one record at a time. It is the
// continuation-page counterpart of rankedRunTail: the source spool is still
// live -- a consumed cursor's spool and lease are released only after the page
// validates -- so the remainder is copied spool to spool without ever standing
// in heap.
func rankedSpoolTail(ctx context.Context, spools *pagination.Spools, c pagination.Cursor, now time.Time,
	skip int) func(func([]byte) error) error {
	return func(yield func([]byte) error) error {
		at := 0
		header := true
		return spools.Open(ctx, c, now, func(record []byte) error {
			if err := ctx.Err(); err != nil {
				return typedContextError(ctx, err)
			}
			if header {
				// The leading header is not a record: the new spool writes its
				// own, so it is skipped rather than counted.
				header = false
				return nil
			}
			if at++; at <= skip {
				return nil
			}
			return yield(record)
		})
	}
}

// rankedRunTail is the same stream taken off the sorted run on the FIRST page,
// where there is no source spool yet.
func rankedRunTail[T any](ctx context.Context, run *pagination.SortedRun[T], skip int,
	encode func(T) ([]byte, error)) func(func([]byte) error) error {
	return func(yield func([]byte) error) error {
		at := 0
		return run.Each(func(v T) error {
			if err := ctx.Err(); err != nil {
				return typedContextError(ctx, err)
			}
			if at++; at <= skip {
				return nil
			}
			encoded, err := encode(v)
			if err != nil {
				return err
			}
			return yield(encoded)
		})
	}
}
