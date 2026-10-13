package context

import (
	"context"
	"errors"
	"slices"

	"github.com/Sawmonabo/codectx/internal/graph"
	"github.com/Sawmonabo/codectx/internal/model"
)

// errClosureCut is a closure walk page the query deadline truncated, on a
// compile that may halt and run the closure again.
var errClosureCut = errors.New("the query deadline cut the kept-file closure")

// closeKeptFilesUnit runs the round's closure as one unit (ADR-0016 decision
// 5). Its admissions go straight into the seed sink's spools under a mark
// (pagination.ExternalSort.Mark), which costs nothing while it is set. When
// the query deadline lands inside it on a compile that may halt (stop), the
// spools are rolled back to the mark, the arrival counter and the scope
// verdict are put back as they were before it, the reached set is reopened
// for the checkpoint, and the pass ends with halted set: the continuation runs
// the closure again whole, so the plan is the one an uninterrupted compile
// makes. It is run again once: a closure the deadline cuts twice in a row
// cannot fit one call, and its second run narrows the scope on the cut as any
// truncated walk does, the same rule the main walk applies to a page that
// stalls.
func (c *Compiler) closeKeptFilesUnit(ctx context.Context, in *seedIngest, eng *graph.Engine,
	gen model.GenerationID, stop func() bool) (bool, error) {
	if in.closure == nil {
		return false, nil
	}
	haltable := stop != nil && !in.closureRetried
	if !haltable {
		return false, c.closeKeptFiles(ctx, in, eng, gen, false)
	}
	sink := in.sinkMarks()
	for _, m := range sink {
		if err := m.Mark(); err != nil {
			return false, err
		}
	}
	keep := func() {
		for _, m := range sink {
			m.Keep()
		}
	}
	seq := in.seq
	scope := in.scope
	scope.Completeness = slices.Clone(scope.Completeness)
	runErr := c.closeKeptFiles(ctx, in, eng, gen, true)
	if runErr == nil {
		keep()
		return false, nil
	}
	if !(errors.Is(runErr, errClosureCut) || isQueryDeadline(ctx, runErr)) || !stop() {
		keep()
		if errors.Is(runErr, errClosureCut) {
			// The cut was the deadline's, but this call may not halt after
			// all: the closure narrows the scope as an unhaltable one does.
			in.scope.ScopeComplete = false
			return false, nil
		}
		return false, runErr
	}
	for _, m := range sink {
		if err := m.RollBack(); err != nil {
			return false, err
		}
	}
	in.seq, in.scope, in.closureRetried = seq, scope, true
	return true, c.reopenReached(in)
}

// marker is the part of an external sort the closure's unit uses.
type marker interface {
	Mark() error
	Keep()
	RollBack() error
}

// sinkMarks is the seed sink's spools, every one the closure writes.
func (in *seedIngest) sinkMarks() []marker {
	return []marker{in.candSort, in.entitySort, in.pathSort, in.hopSort}
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
