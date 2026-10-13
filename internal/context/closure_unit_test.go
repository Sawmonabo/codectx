package context

import (
	"testing"
	"time"

	"github.com/Sawmonabo/codectx/internal/model"
)

// TestADeadlineInsideTheClosureEndsThePassBeforeIt pins the closure's halt
// (closeKeptFilesUnit): on a compile that may halt, a closure walk page the
// query deadline cuts leaves nothing behind -- no admitted record, the arrival
// counter and the scope verdict as they were before the closure -- and ends
// the pass with the closure still to run, so the continuation runs it whole and
// the plan is the uninterrupted compile's. A second cut on a later leg does not
// halt again: it narrows the scope, as any truncated walk does.
//
// Mutation caught: the closure narrowing the scope on the deadline's cut
// instead of halting (the first call answers halted=false with the scope
// incomplete), or its admissions written straight to the sink (a rerun would
// admit them twice).
func TestADeadlineInsideTheClosureEndsThePassBeforeIt(t *testing.T) {
	fx := newContextFixture(t)
	rels := []model.Relation{fx.edge(fx.Specs[0].path, model.RelCalls, fx.Specs[1].path)}
	clock := time.Now()
	calls := 0
	// Every scan steps the engine clock past its deadline, so the closure
	// walk's first page is the deadline's cut.
	eng := stalledScopeEngine(t, fx, rels, &clock, &calls, 0)

	c := intCompiler(t, fx, fx.Now)
	reader, err := c.store.PinGeneration(fx.ctx, c.repo, 0, c.cfg.Storage.QueryCursorTTL.Std())
	if err != nil {
		t.Fatalf("PinGeneration: %v", err)
	}
	defer reader.Close()
	sorts := openSorts(t, fx.Cfg)
	in, err := c.newSeedIngest(sorts)
	if err != nil {
		t.Fatalf("newSeedIngest: %v", err)
	}
	const seq = 7
	in.seq = seq
	kept := fx.File(fx.Specs[0].path).ID
	in.closure = keptClosureOf(roundInputs{Kept: []model.FileID{kept}},
		seedStop{Stopped: true, NextContribution: 1000, Tau: 1000})
	if in.closure.reached, err = newIDSort(sorts, "closure-reached"); err != nil {
		t.Fatalf("newIDSort: %v", err)
	}
	in.closure.reader, in.closure.query = reader, "task"

	halted, err := c.closeKeptFilesUnit(fx.ctx, in, eng, fx.Gen, func() bool { return true })
	if err != nil {
		t.Fatalf("closeKeptFilesUnit: %v", err)
	}
	if !halted {
		t.Fatalf("a closure the deadline cut did not end the pass (scope complete %v); the plan would be "+
			"the fraction the deadline reached", in.scope.ScopeComplete)
	}
	if in.seq != seq || !in.scope.ScopeComplete || in.candSort.Len() != 0 || in.entitySort.Len() != 0 {
		t.Fatalf("the cut closure left seq %d (want %d), scope complete %v, %d spooled and %d entity records; "+
			"it must leave the sink as it found it", in.seq, seq, in.scope.ScopeComplete,
			in.candSort.Len(), in.entitySort.Len())
	}
	if !in.closureRetried || in.closure.reached == nil || in.closure.reachedRun != nil {
		t.Fatal("the cut closure did not record its retry or reopen its reached set for the checkpoint")
	}

	again, err := c.closeKeptFilesUnit(fx.ctx, in, eng, fx.Gen, func() bool { return true })
	if err != nil {
		t.Fatalf("the second closeKeptFilesUnit: %v", err)
	}
	if again || in.scope.ScopeComplete {
		t.Fatalf("a second cut halted=%v with scope complete %v; it must run to its end and narrow the scope",
			again, in.scope.ScopeComplete)
	}
}
