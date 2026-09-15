package provider

import (
	"context"
	"errors"
	"strconv"

	"github.com/Sawmonabo/codectx/internal/model"
	"github.com/Sawmonabo/codectx/internal/storage/sqlite"
)

// UnitOutput is the storage side of one building unit: the writer the sink
// drains into, sealing after whole-unit validation, and discarding on failure.
// Seal is the only path to generation membership; Fail deletes every row the
// unit wrote so unsealed output can never be queried (Section 11.1). Abandon
// is the cancel-path discard: it gives up the unit without the cascading
// delete, leaving the rows for storage's own collection, which is what keeps
// an interrupt from waiting on a rollback nobody is going to read.
type UnitOutput interface {
	Sink
	Seal(context.Context) error
	Fail(context.Context) error
	Abandon(context.Context) error
}

// storeUnit adapts storage's writer and store to UnitOutput. Sealing lives on
// the store rather than the writer, which is why the pair is needed.
type storeUnit struct {
	*sqlite.UnitWriter
	store *sqlite.Store
}

// StoreUnit binds a building unit opened by store.BeginUnit.
func StoreUnit(store *sqlite.Store, w *sqlite.UnitWriter) UnitOutput {
	return storeUnit{UnitWriter: w, store: store}
}

func (u storeUnit) Seal(ctx context.Context) error { return u.store.SealUnit(ctx, u.UnitWriter) }

// RunUnit executes one assigned unit end to end: it runs the provider with a
// batch sink over out, flushes what the provider emitted, and seals the unit
// only when the provider reported a succeeded run and the writer's whole-unit
// validation passed. On a partial run or a write failure the unit is failed
// and its output deleted. A build the caller cancelled abandons the unit
// instead: it is marked failed and its rows are left for storage's collection,
// so the exit path never waits on a rollback. Failed or abandoned output is
// never attached to a generation.
//
// The provider runs under a child context that the sink cancels when a write
// fails, so every producer goroutine stops promptly. Cleanup uses a context
// that survives that cancellation. The returned result always names req.Run
// and a terminal state; the error explains a non-succeeded state.
//
// The writer behind out is single-owner: it has no lock of its own, and Seal
// and Fail must never overlap a write. While the sink is live another
// acquirer may flush its queued batches into the writer to relieve pool
// pressure, so the sink is discarded (which waits for any such write and
// then leaves the pool) before Seal, Fail or Abandon is called on any path. The
// deferred Discard is only the idempotent safety net.
func RunUnit(ctx context.Context, p Provider, req UnitRequest, out UnitOutput, limits Limits, pool *Pool) (model.ProviderResult, error) {
	runCtx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	var result model.ProviderResult
	sink, err := NewBatchSink(runCtx, out, limits, pool, cancel)
	if err == nil {
		defer sink.Discard()
		result, err = p.IndexUnit(runCtx, req, sink)
		if err == nil {
			err = sink.Flush(runCtx)
		}
		// Folded before checkResult so the added details are validated with
		// the rest of the result rather than smuggled past validation.
		result.Capabilities = reportDegradations(result.Capabilities, sink.Degradations(), p.Descriptor(), req.Unit)
		// From here on the writer belongs to this function alone.
		sink.Discard()
	}
	if err == nil {
		err = checkResult(result, req)
	}
	if err == nil && result.State != model.RunSucceeded {
		err = &model.Error{Code: model.CodeProviderOutputInvalid,
			Message: "provider reported a " + string(result.State) + " run; only a succeeded unit is admitted",
			Details: map[string]string{"unit_id": string(req.Unit.ID)}}
	}
	if err == nil {
		err = out.Seal(runCtx)
	}
	if err == nil {
		return result, nil
	}
	// A cancellation caused by a sink failure is reported as that failure, not
	// as a caller stop.
	if cause := context.Cause(runCtx); cause != nil && !errors.Is(cause, context.Canceled) && !errors.Is(err, cause) {
		err = errors.Join(cause, err)
	}
	// The cleanup runs uncancelled so a stop cannot leave a half-discarded
	// unit; that is precisely why a build the caller has already stopped must
	// take the bounded discard rather than the cascading one. The caller's own
	// context is the signal, not the error: a cancellation reaches this
	// function through whatever the provider wrapped it in, and can classify
	// as any failure code.
	cleanup := context.WithoutCancel(ctx)
	discard := out.Fail
	if ctx.Err() != nil {
		discard = out.Abandon
	}
	if failErr := discard(cleanup); failErr != nil {
		err = errors.Join(err, failErr)
	}
	return failedResult(req.Run, stateOf(err), sink), err
}

// reportDegradations folds the sink's counted degradations into the unit's
// capability rows, which is where a generation stores what a run had to give
// up. The row's state is left alone: a bound that was reported rather than
// enforced cost the answer nothing, so calling the capability partial would
// claim a loss that did not happen -- the details say the record was larger
// than the bound the user asked to be told about.
//
// A provider that published no capability row at all has no other place to
// carry this, so one fresh row per declared capability is synthesized; the
// descriptor's list is the provider's own static declaration, so it is small
// and does not grow with the repository.
func reportDegradations(states []model.CapabilityState, degraded []Degradation, desc model.ProviderDescriptor, unit model.UnitSpec) []model.CapabilityState {
	if len(degraded) == 0 {
		return states
	}
	if len(states) == 0 {
		for _, c := range desc.Capabilities {
			states = append(states, model.CapabilityState{
				ProviderID: unit.ProviderID, Capability: c, Scope: unit.ScopeKey, State: model.CapabilityFresh})
		}
	}
	for i, st := range states {
		for _, d := range degraded {
			st = st.WithDetail("over_"+d.Limit, strconv.FormatUint(d.Count, 10)).
				WithDetail(d.Limit, strconv.FormatInt(d.Bound, 10)).
				WithDetail("largest_record_bytes", strconv.FormatInt(d.Largest, 10))
		}
		states[i] = st
	}
	return states
}

// checkResult rejects a result that does not describe this run.
func checkResult(result model.ProviderResult, req UnitRequest) error {
	if err := result.Validate(); err != nil {
		return err
	}
	if result.RunID != req.Run {
		return &model.Error{Code: model.CodeProviderOutputInvalid, Message: "provider result names another run than the one it was given",
			Details: map[string]string{"unit_id": string(req.Unit.ID)}}
	}
	return nil
}

func failedResult(run model.ProviderRunID, state model.RunState, sink *BatchSink) model.ProviderResult {
	r := model.ProviderResult{RunID: run, State: state}
	if sink != nil {
		r.RecordsEmitted, r.BytesProcessed = sink.Records(), sink.Bytes()
	}
	return r
}

// stateOf maps a failure to its Section 22 run state. An expired deadline is
// timed_out whether it arrives bare or typed as CTX_CANCELED (model.Canceled
// joins the two), so it takes precedence over the code. A sink write failure
// joined with the cancellation it caused is a failure, not a cancellation.
func stateOf(err error) model.RunState {
	if errors.Is(err, context.DeadlineExceeded) {
		return model.RunTimedOut
	}
	var typed *model.Error
	if errors.As(err, &typed) {
		switch typed.Code {
		case model.CodeCanceled:
			if !onlyCancellation(err) {
				return model.RunFailed
			}
			return model.RunCanceled
		case model.CodeProviderTimeout:
			return model.RunTimedOut
		}
		return model.RunFailed
	}
	if errors.Is(err, context.Canceled) {
		return model.RunCanceled
	}
	return model.RunFailed
}

// onlyCancellation reports whether every typed error in the tree is a
// cancellation.
func onlyCancellation(err error) bool {
	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		for _, e := range joined.Unwrap() {
			if !onlyCancellation(e) {
				return false
			}
		}
		return true
	}
	var typed *model.Error
	if errors.As(err, &typed) {
		return typed.Code == model.CodeCanceled
	}
	return true
}
