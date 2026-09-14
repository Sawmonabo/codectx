package delta

import (
	"context"
	"errors"

	"github.com/Sawmonabo/codectx/internal/model"
	"github.com/Sawmonabo/codectx/internal/provider"
	"github.com/Sawmonabo/codectx/internal/storage/sqlite"
)

// build is the half of a delta that is the same for every provider: open the
// unit, run the import through provider.RunUnit, and record the run.
//
// RunUnit is reused rather than reimplemented because everything it owns is
// wanted here unchanged — the byte-bounded sink over the pool, the cancellation
// the sink propagates, the validation that the result names this run and
// succeeded, the single-ownership discipline that discards the sink before the
// writer is touched again, and the Fail that deletes every row of a unit that
// did not get there. The only difference a delta needs is what happens between
// "the provider is done" and "the unit is sealed", and that is exactly where
// UnitOutput.Seal sits: output.Seal carries the predecessor over and stores
// the fresh delta state before it seals, so a failure in either is a failure
// of the unit and RunUnit fails it.
type build struct {
	store  *sqlite.Store
	p      provider.Provider
	limits provider.Limits
	pool   *provider.Pool
	req    Request

	// index runs the provider's delta-aware import in place of IndexUnit.
	index func(ctx context.Context, req provider.UnitRequest, sink provider.Sink) (model.ProviderResult, error)
	// complete runs with the writer solely owned, after the sink has been
	// discarded and before the unit is sealed.
	complete func(ctx context.Context, w *sqlite.UnitWriter) error

	res Result
}

func (b *build) run(ctx context.Context) (Result, error) {
	if err := b.req.validate(); err != nil {
		return Result{}, err
	}
	w, err := b.store.BeginUnit(ctx, b.req.Generation, b.req.Build, b.req.Inputs)
	if err != nil {
		return Result{}, err
	}
	out := &output{UnitWriter: w, store: b.store, complete: b.complete}
	result, runErr := provider.RunUnit(ctx, indexFunc{Provider: b.p, fn: b.index}, b.req.Unit, out, b.limits, b.pool)
	// The run is recorded on both paths and under a context that survives the
	// cancellation a sink failure raises: a provider run left running is a row
	// no later generation can complete.
	if err := b.store.CompleteProviderRun(context.WithoutCancel(ctx), result, provider.CodeOf(runErr)); err != nil {
		runErr = errors.Join(runErr, err)
	}
	if runErr != nil {
		return Result{}, runErr
	}
	b.res.Unit = b.req.Build.Spec.ID
	b.res.Result = result
	return b.res, nil
}

// output is the storage side of a delta-built unit: provider.StoreUnit with
// the carry-over and the delta state wedged in front of the seal.
//
// It embeds the concrete writer, not provider.UnitOutput: the batch sink
// reaches its destination's keyed puts through a provider.DeltaSink type
// assertion, and a wrapper that embedded the narrower interface would hide
// them, so a keyed import would refuse to write at all.
type output struct {
	*sqlite.UnitWriter
	store    *sqlite.Store
	complete func(ctx context.Context, w *sqlite.UnitWriter) error
}

var (
	_ provider.UnitOutput = (*output)(nil)
	_ provider.DeltaSink  = (*output)(nil)
)

func (o *output) Seal(ctx context.Context) error {
	if o.complete != nil {
		if err := o.complete(ctx, o.UnitWriter); err != nil {
			return err
		}
	}
	return o.store.SealUnit(ctx, o.UnitWriter)
}

// indexFunc is the provider with its whole-unit import replaced by the
// delta-aware one. Descriptor and Detect are the provider's own, so the unit
// RunUnit validates is keyed by the tool that produced it.
type indexFunc struct {
	provider.Provider
	fn func(context.Context, provider.UnitRequest, provider.Sink) (model.ProviderResult, error)
}

func (i indexFunc) IndexUnit(ctx context.Context, req provider.UnitRequest, sink provider.Sink) (model.ProviderResult, error) {
	return i.fn(ctx, req, sink)
}
