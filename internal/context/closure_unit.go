package context

import (
	"context"
	"errors"
	"slices"

	"github.com/Sawmonabo/codectx/internal/graph"
	"github.com/Sawmonabo/codectx/internal/model"
	"github.com/Sawmonabo/codectx/internal/pagination"
)

// errClosureCut is a closure walk page the query deadline truncated, on a
// compile that may halt and run the closure again.
var errClosureCut = errors.New("the query deadline cut the kept-file closure")

// closureStage is the seed sink's spools while the closure runs: what the
// closure admits goes here and reaches the sink only when the closure
// finishes, so a closure the deadline cuts leaves nothing behind.
type closureStage struct {
	cand, entity *pagination.ExternalSort[candRec]
	path         *pagination.ExternalSort[pathRec]
	hop          *pagination.ExternalSort[hopRec]
}

// closeKeptFilesUnit runs the round's closure as one unit (ADR-0016 decision
// 5). Its admissions are staged and committed to the sink when it finishes.
// When the query deadline lands inside it on a compile that may halt
// (stop), the stage, the arrival counter and the scope verdict are put back
// as they were before it, the reached set is reopened for the checkpoint, and
// the pass ends with halted set: the continuation runs the closure again
// whole, so the plan is the one an uninterrupted compile makes. It is run
// again once: a closure the deadline cuts twice in a row cannot fit one call,
// and its second run narrows the scope on the cut as any truncated walk does,
// the same rule the main walk applies to a page that stalls.
func (c *Compiler) closeKeptFilesUnit(ctx context.Context, in *seedIngest, eng *graph.Engine,
	gen model.GenerationID, stop func() bool) (bool, error) {
	if in.closure == nil {
		return false, nil
	}
	haltable := stop != nil && !in.closureRetried
	if !haltable {
		return false, c.closeKeptFiles(ctx, in, eng, gen, false)
	}
	stage, err := openClosureStage(in.sorts)
	if err != nil {
		return false, err
	}
	seq := in.seq
	scope := in.scope
	scope.Completeness = slices.Clone(scope.Completeness)
	main := closureStage{cand: in.candSort, entity: in.entitySort, path: in.pathSort, hop: in.hopSort}
	in.candSort, in.entitySort, in.pathSort, in.hopSort = stage.cand, stage.entity, stage.path, stage.hop
	runErr := c.closeKeptFiles(ctx, in, eng, gen, true)
	in.candSort, in.entitySort, in.pathSort, in.hopSort = main.cand, main.entity, main.path, main.hop
	if runErr == nil {
		return false, stage.commit(in)
	}
	if !(errors.Is(runErr, errClosureCut) || isQueryDeadline(ctx, runErr)) || !stop() {
		if errors.Is(runErr, errClosureCut) {
			// The cut was the deadline's, but this call may not halt after
			// all: the closure narrows the scope as an unhaltable one does.
			in.scope.ScopeComplete = false
			return false, stage.commit(in)
		}
		return false, runErr
	}
	in.seq, in.scope, in.closureRetried = seq, scope, true
	return true, c.reopenReached(in)
}

// openClosureStage opens the stage's spools in the compile's sort area with
// the sink's own orders.
func openClosureStage(s *compileSorts) (closureStage, error) {
	var st closureStage
	var err error
	if st.cand, err = newSort[candRec](s, "closure-stage-cand", lessCandSeq, sizeOfCand); err != nil {
		return st, err
	}
	if st.entity, err = newSort[candRec](s, "closure-stage-entity", lessEntityID, sizeOfCand); err != nil {
		return st, err
	}
	if st.path, err = newSort[pathRec](s, "closure-stage-path", lessPathSeq, sizeOfPath); err != nil {
		return st, err
	}
	st.hop, err = newSort[hopRec](s, "closure-stage-hop", lessHopSeq, sizeOfHop)
	return st, err
}

// commit replays every staged record into the sink's spools. Each record keeps
// the seq it was admitted under, and each spool orders and folds what it holds,
// so the sink ends as if the closure had written to it directly.
func (st closureStage) commit(in *seedIngest) error {
	if err := replay(in.sorts, st.cand, in.candSort); err != nil {
		return err
	}
	if err := replay(in.sorts, st.entity, in.entitySort); err != nil {
		return err
	}
	if err := replay(in.sorts, st.path, in.pathSort); err != nil {
		return err
	}
	return replay(in.sorts, st.hop, in.hopSort)
}

// replay adds every record of from, in its order, to into.
func replay[T any](s *compileSorts, from, into *pagination.ExternalSort[T]) error {
	run, err := sortedRun(s, from)
	if err != nil {
		return err
	}
	return run.Each(into.Add)
}

// reopenReached rebuilds the closure's reached set as an open sort for the
// checkpoint: the closure read it to its end, and the passIngest boundary
// carries it pre-fold. It holds the same identities, the walk's roots among
// them, which the next run adds again and the fold keeps once.
func (c *Compiler) reopenReached(in *seedIngest) error {
	cl := in.closure
	run := cl.reachedRun
	if run == nil {
		var err error
		if run, err = sortedRun(in.sorts, cl.reached); err != nil {
			return err
		}
	}
	reached, err := newIDSort(in.sorts, "closure-reached")
	if err != nil {
		return err
	}
	if err := run.Each(reached.Add); err != nil {
		return err
	}
	cl.reached, cl.reachedRun = reached, nil
	return nil
}
