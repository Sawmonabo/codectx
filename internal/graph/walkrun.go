package graph

import (
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
//   - the cumulative admitted-node set is APPENDED to, never copied. Each link
//     adds one ascending run of its own admissions to the walk's persistent
//     visited store (visitedstore.go), which is the same state the continuation
//     carries to the next request. A copy per page is O(pages x nodes), and
//     appends that reached only a request-scoped scratch file were worse than
//     that: the links' admissions never reached the continuation at all, so the
//     next request re-admitted every one of them and reported the same entity
//     on two pages.
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

	if o.Visited == nil {
		return walkState{}, (&model.Error{Code: model.CodeInternal,
			Message: "graph expansion requires a persistent visited set"}).
			WithDetail("operation", "run_walk_to_completion")
	}
	for {
		spent := o.Budget.edges
		state, err := expand(ctx, e.adjacency, seeds, o, wrapped)
		if err != nil {
			return walkState{}, err
		}
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
		case o.Budget.edges == spent:
			// Checked LAST: a link that admitted nothing because the CLOCK cut it
			// short is the case above, and it is resumable. Only a link the
			// visitor starved with time left is an answer-level stop.
			// The link admitted nothing, so the next one would admit nothing
			// either: the visitor is refusing every row on an ANSWER-level
			// bound it carries itself (impactAccumulator's max_edges and
			// max_visited), which no internal boundary can return. Chaining on
			// would spin forever over the same frontier. The frontier is
			// returned standing, and the caller reports the visitor's reason.
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
				return walkState{}, err
			}
			o.Budget.deadlineHit = true
			return state, nil
		}
		// This link's OWN admissions, as one ascending run of the persistent
		// store. Only its own: the frontier it resumed belongs to the run the
		// previous link wrote. The store is flushed by the append, so the next
		// link's membership sweep reads what this one just admitted.
		grown, err := o.Visited.appendRun(state.Admitted.addedNodes())
		if err != nil {
			return walkState{}, err
		}
		if e.probe != nil {
			e.probe.VisitedBytes += grown
		}
		// The next link resumes from this one exactly as a signed continuation
		// would -- same frontier, same keyset position, same cumulative visited
		// set -- with the signer, the lease and the spool left out, because
		// nothing here outlives the request.
		resume := &resumeState{
			Cursor:   traversalCursor{Depth: state.Depth, LastOwner: lastOwner, LastKey: lastKey},
			Budget:   o.Budget,
			Frontier: state.Frontier,
			Visited:  o.Visited.stream,
			Filter:   o.Visited.filter,
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

// walkScratchDir is where this request's retained state and sort runs spill:
// the spool store's own sort directory, so a query's temporary files sit in one
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
// It reads its input from, and persists its own progress into, the RETAINED
// walk state (walkretain.go): pass 1 keys lessByNode and folds foldImpact,
// pass 2 keys lessByRank and does not fold, and either pass may be cut short by
// the query deadline and resumed by the next request over the runs it had
// already spilled. Ruling P7 used to re-sort the whole retained input on every
// resumed request; it now adopts those runs instead, so a ranking split across
// requests does the work of ONE ranking and serves the identical answer.
//
// Close the returned run.
//
// Owned by lane P-b.
func (e *Engine) rankImpact(ctx context.Context, retain *retainedWalk,
	stats *rankStats) (*pagination.SortedRun[impactRecord], error) {
	prog, err := retain.rankProgress()
	if err != nil {
		return nil, err
	}
	if prog.Pass < 2 {
		if prog, err = e.foldImpactPass(ctx, retain, prog, stats); err != nil {
			return nil, err
		}
	}
	return e.rankImpactPass(ctx, retain, prog, stats)
}

// foldImpactPass is pass 1: every record every leg of the walk appended, folded
// by node identity. Its output -- the input pass 2 reads -- is retained before
// the manifest advances, because the sort removes its own output as soon as it
// has been read and a pass-2 resume would otherwise have nothing to read.
func (e *Engine) foldImpactPass(ctx context.Context, retain *retainedWalk,
	prog rankProgress, stats *rankStats) (rankProgress, error) {
	pass1, err := e.openImpactSort(retain, "graph-impact-fold-", lessByNode, prog.Runs)
	if err != nil {
		return prog, err
	}
	defer pass1.Close()
	// The fold must be re-applied on every resumed request: pagination.AdoptRuns
	// carries the runs, never the functions, and a resumed pass without it would
	// serve one entry per admission instead of one per node.
	pass1.WithFold(foldImpact)

	seen, err := feedRankPass(ctx, e, pass1, prog.Added, retain.eachEntry)
	if err != nil {
		return prog, e.detachRankPass(retain, pass1, 1, seen, err)
	}
	folded, err := pass1.Sorted()
	if err != nil {
		return prog, err
	}
	defer folded.Close()
	stats.observe(pass1.PeakLiveRecords())
	// Sorted CONSUMED the adopted runs, so the manifest must stop naming them
	// before anything else can fail: a manifest pointing at a removed run
	// resumes into a missing file instead of re-running a pass that is cheap
	// to re-run.
	if err := retain.setRankProgress(rankProgress{Pass: 1}); err != nil {
		return prog, err
	}
	out, err := retain.openFolded()
	if err != nil {
		return prog, err
	}
	if err := folded.Each(func(r impactRecord) error {
		if err := ctx.Err(); err != nil {
			return typedContextError(ctx, err)
		}
		b, err := encodeImpactRecord(r)
		if err != nil {
			return err
		}
		return out.append(b)
	}); err != nil {
		_ = out.close()
		return prog, err
	}
	if err := out.close(); err != nil {
		return prog, err
	}
	next := rankProgress{Pass: 2}
	if err := retain.setRankProgress(next); err != nil {
		return prog, err
	}
	return next, nil
}

// rankImpactPass is pass 2: the folded records in ruling P1's served order.
func (e *Engine) rankImpactPass(ctx context.Context, retain *retainedWalk,
	prog rankProgress, stats *rankStats) (*pagination.SortedRun[impactRecord], error) {
	pass2, err := e.openImpactSort(retain, "graph-impact-rank-", lessByRank, prog.Runs)
	if err != nil {
		return nil, err
	}
	defer pass2.Close()
	seen, err := feedRankPass(ctx, e, pass2, prog.Added, retain.eachFolded)
	if err != nil {
		return nil, e.detachRankPass(retain, pass2, 2, seen, err)
	}
	run, err := pass2.Sorted()
	if err != nil {
		return nil, err
	}
	stats.observe(pass2.PeakLiveRecords())
	return run, nil
}

// openImpactSort opens one ranking pass with the frozen codec and the query's
// own run budget, continuing the runs an interrupted request left when there
// are any. New runs still spill into the engine's sort directory; only the runs
// an interruption hands over live in the retained state.
func (e *Engine) openImpactSort(retain *retainedWalk, prefix string,
	compare func(a, b impactRecord) int, runs []string) (*pagination.ExternalSort[impactRecord], error) {
	dir := e.walkScratchDir()
	var (
		sorter *pagination.ExternalSort[impactRecord]
		err    error
	)
	if len(runs) > 0 {
		if e.probe != nil {
			e.probe.AdoptedRuns += len(runs)
		}
		sorter, err = pagination.AdoptRuns(dir, prefix, 0, retain.runPaths(runs),
			encodeImpactRecord, decodeImpactRecord, compare)
	} else {
		sorter, err = pagination.NewExternalSort(dir, prefix, 0,
			encodeImpactRecord, decodeImpactRecord, compare)
	}
	if err != nil {
		return nil, err
	}
	return sorter.WithRunBytes(pagination.SortRunBytes(e.limits.FrontierBytes), sizeOfImpactRecord), nil
}

// feedRankPass replays one pass's input into the sort, SKIPPING the prefix the
// adopted runs already hold, and returns how many input records the sort now
// covers -- the skipped prefix plus what this request added. A deadline ends
// the replay; the count is still the honest one, because the record the
// deadline stopped on was not added.
func feedRankPass(ctx context.Context, e *Engine, sorter *pagination.ExternalSort[impactRecord],
	adopted int64, replay func(func(impactRecord) error) error) (int64, error) {
	seen, addedHere := int64(0), 0
	err := replay(func(r impactRecord) error {
		seen++
		if seen <= adopted {
			// Already inside an adopted run. Re-adding it would fold a record
			// into itself and double every count the fold carries.
			if e.probe != nil {
				e.probe.SkippedRecords++
			}
			return nil
		}
		if err := e.rankInterrupted(ctx, addedHere); err != nil {
			seen--
			return err
		}
		addedHere++
		return sorter.Add(r)
	})
	return seen, err
}

// rankInterrupted reports the query deadline mid-ranking (ruling P7). added is
// how many records THIS request has put into the current pass, which is what
// the test hook counts: a hook that counted the whole pass would stop a resumed
// request at the same record it stopped the first one at and never finish.
func (e *Engine) rankInterrupted(ctx context.Context, added int) error {
	if err := ctx.Err(); err != nil {
		return typedContextError(ctx, err)
	}
	if e.rankStopAfter > 0 && added >= e.rankStopAfter {
		return &model.Error{Code: model.CodeQueryDeadline,
			Message: "graph: the query deadline reached the ranking pass"}
	}
	return nil
}

// detachRankPass persists an interrupted pass and returns the error that
// interrupted it. A stop that is NOT the deadline persists nothing: the
// continuation is never minted for it, so state it left behind would be state
// nothing ever adopts.
//
// The runs and the count are written in ONE manifest, and Runs is the Detach
// return verbatim rather than an append to what was adopted -- an adopting sort
// spills after the runs it took over, so its own Detach already returns both.
func (e *Engine) detachRankPass(retain *retainedWalk, sorter *pagination.ExternalSort[impactRecord],
	pass int, seen int64, cause error) error {
	if !isRankDeadline(cause) {
		return cause
	}
	spilled, err := sorter.Detach()
	if err != nil {
		return err
	}
	names, err := retain.adoptRunFiles(spilled)
	if err != nil {
		return err
	}
	if err := retain.setRankProgress(rankProgress{Pass: pass, Runs: names, Added: seen}); err != nil {
		return err
	}
	return cause
}

// isRankDeadline reports the one interruption a ranking pass may be resumed
// from. It is the same test impactPhaseError applies, kept here so the two
// cannot disagree about which stop mints a continuation.
func isRankDeadline(err error) bool {
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	var typed *model.Error
	return errors.As(err, &typed) && typed.Code == model.CodeQueryDeadline
}

// rankedSpoolHeader reads the leading rankedHeader record of a ranked spool and
// reports where the records after it begin, plus the bytes this read took off
// disk. It is O(1) work per page and it is the KIND guard: a cursor bound to
// this query and lease could otherwise name the frontier spool of the same
// walk, and decodeRankedHeader is what refuses that.
func rankedSpoolHeader(ctx context.Context, spools *pagination.Spools, c pagination.Cursor,
	now time.Time) (rankedHeader, int64, int64, error) {
	var h rankedHeader
	got := false
	end, err := spools.OpenAt(ctx, c, now, 0, func(record []byte) error {
		if err := ctx.Err(); err != nil {
			return typedContextError(ctx, err)
		}
		var err error
		if h, err = decodeRankedHeader(record); err != nil {
			return err
		}
		got = true
		return pagination.ErrStopSpool
	})
	if err != nil {
		return rankedHeader{}, 0, 0, err
	}
	if !got {
		return rankedHeader{}, 0, 0, cursorInvalid("the cursor names a result page this build cannot replay")
	}
	// end is one past the header record, so it is both the offset the first
	// ranked record begins at and the bytes this read consumed from the file.
	return h, end, end, nil
}

// readRankedSection takes at most want records out of a ranked spool starting
// at offset and hands each to take. It returns the offset one past the last
// record it took -- the next page's resume point -- and the bytes it read.
//
// The read STOPS at the page bound rather than streaming to the end of the
// spool, which is what makes a page cost its own page: with the offset carried
// forward, no page ever touches the records of another.
func readRankedSection(ctx context.Context, spools *pagination.Spools, c pagination.Cursor,
	now time.Time, offset int64, want int, take func([]byte) error) (int64, int64, error) {
	if want <= 0 {
		return offset, 0, nil
	}
	taken := 0
	end, err := spools.OpenAt(ctx, c, now, offset, func(record []byte) error {
		if err := ctx.Err(); err != nil {
			return typedContextError(ctx, err)
		}
		if err := take(record); err != nil {
			return err
		}
		if taken++; taken == want {
			return pagination.ErrStopSpool
		}
		return nil
	})
	if err != nil {
		return 0, 0, err
	}
	return end, end - offset, nil
}

// servePage reads ONE page out of a single-section ranked spool at the byte
// offset the presenting cursor carries, and reports where the page after it
// begins.
//
// c is the binding the spool was written under and now the clock the lease is
// checked against, exactly as pagination.Spools.OpenAt takes them. decode is
// the record codec -- decodeImpactRecord or decodePairRecord -- which is what
// lets one page reader serve both ranked answers; the two can never be crossed,
// because a cursor is bound to the endpoint that issued it.
//
// The returned handle names the SAME spool and lease: the spool the first page
// wrote holds the whole ranked remainder and every later page reads its own
// byte range out of it. Nothing is copied forward, so the work a page does is a
// function of the page limit alone.
func servePage[T any](ctx context.Context, spools *pagination.Spools, c pagination.Cursor, now time.Time,
	tail rankedTail, limit int, decode func([]byte) (T, error)) ([]T, rankedTail, int64, error) {
	if spools == nil {
		return nil, rankedTail{}, 0, cursorInvalid("continuation state has expired or was released")
	}
	h, _, read, err := rankedSpoolHeader(ctx, spools, c, now)
	if err != nil {
		return nil, rankedTail{}, 0, err
	}
	remaining := h.Total - tail.Served
	if err := h.checkAgainst(remaining, 0); err != nil {
		return nil, rankedTail{}, 0, err
	}
	want := limit
	if int64(want) > remaining {
		want = int(remaining)
	}
	// The capacity is the page bound the caller already clamped to
	// model.MaxPageItems, so this allocation cannot track the answer.
	out := make([]T, 0, max(want, 0))
	end, n, err := readRankedSection(ctx, spools, c, now, tail.Offset, want, func(record []byte) error {
		v, err := decode(record)
		if err != nil {
			return err
		}
		out = append(out, v)
		return nil
	})
	if err != nil {
		return nil, rankedTail{}, 0, err
	}
	read += n
	next := rankedTail{
		SpoolID: c.SpoolID, LeaseID: c.LeaseID,
		Offset: end,
		Served: tail.Served + int64(len(out)),
		Total:  h.Total,
	}
	if next.Served >= next.Total {
		// Nothing follows this page: the caller mints no continuation, releases
		// the spool, and the empty SpoolID is what says so.
		next.SpoolID = ""
	}
	return out, next, read, nil
}

// rankedRunTail is the stream taken off the sorted run on the FIRST page, where
// there is no source spool yet: it skips the records the page served and writes
// every one after them into the spool every later page will read.
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

// serveRankedSections reads ONE page out of a combined ranked spool: at most
// wantEntities records of the leading entity section at rankOff, and at most
// wantPairs of the package section at pairOff. The cursor carries both offsets
// because the two sections are read independently, and each read stops at its
// own page bound, so neither section's cost depends on what is left in it.
//
// It is impact's page reader. The endpoints that rank a single list use
// servePage above; the two cannot be crossed, because a cursor is bound to the
// endpoint that issued it.
func serveRankedSections(ctx context.Context, spools *pagination.Spools, c pagination.Cursor,
	now time.Time, rankOff, pairOff int64, wantEntities, wantPairs int) (
	[]impactRecord, []pairRecord, int64, int64, int64, error) {
	if spools == nil {
		return nil, nil, 0, 0, 0, cursorInvalid("continuation state has expired or was released")
	}
	// Both capacities are the page bound the caller already clamped to
	// model.MaxPageItems, so neither allocation can track the answer.
	entries := make([]impactRecord, 0, max(wantEntities, 0))
	pairs := make([]pairRecord, 0, max(wantPairs, 0))
	endRank, readRank, err := readRankedSection(ctx, spools, c, now, rankOff, wantEntities,
		func(record []byte) error {
			v, err := decodeImpactRecord(record)
			if err != nil {
				return err
			}
			entries = append(entries, v)
			return nil
		})
	if err != nil {
		return nil, nil, 0, 0, 0, err
	}
	endPair, readPair, err := readRankedSection(ctx, spools, c, now, pairOff, wantPairs,
		func(record []byte) error {
			v, err := decodePairRecord(record)
			if err != nil {
				return err
			}
			pairs = append(pairs, v)
			return nil
		})
	if err != nil {
		return nil, nil, 0, 0, 0, err
	}
	return entries, pairs, endRank, endPair, readRank + readPair, nil
}
