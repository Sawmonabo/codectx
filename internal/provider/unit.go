package provider

import (
	"context"
	"errors"

	"github.com/Sawmonabo/codectx/internal/model"
	"github.com/Sawmonabo/codectx/internal/storage/sqlite"
)

// UnitOutput is the storage side of one building unit: the writer the sink
// drains into, sealing after whole-unit validation, and discarding on failure.
// Seal is the only path to generation membership; Fail deletes every row the
// unit wrote so unsealed output can never be queried (Section 11.1).
type UnitOutput interface {
	Sink
	Seal(context.Context) error
	Fail(context.Context) error
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
// validation passed. On any other outcome, including a partial run, a write
// failure or cancellation, the unit is failed and its output deleted; failed
// partial output is never attached to a generation.
//
// The provider runs under a child context that the sink cancels when a write
// fails, so every producer goroutine stops promptly. Cleanup uses a context
// that survives that cancellation. The returned result always names req.Run
// and a terminal state; the error explains a non-succeeded state.
func RunUnit(ctx context.Context, p Provider, req UnitRequest, out UnitOutput, limits Limits, pool *Pool) (model.ProviderResult, error) {
	runCtx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	sink, err := NewBatchSink(out, limits, pool, cancel)
	if err != nil {
		return failedResult(req.Run, model.RunFailed, sink), err
	}
	result, err := p.IndexUnit(runCtx, req, sink)
	if err == nil {
		err = sink.Flush(runCtx)
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
	cleanup := context.WithoutCancel(ctx)
	if failErr := out.Fail(cleanup); failErr != nil {
		err = errors.Join(err, failErr)
	}
	return failedResult(req.Run, stateOf(err), sink), err
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

// stateOf maps a failure to its Section 22 run state. A sink write failure
// joined with the cancellation it caused is a failure, not a cancellation.
func stateOf(err error) model.RunState {
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
	if errors.Is(err, context.DeadlineExceeded) {
		return model.RunTimedOut
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
