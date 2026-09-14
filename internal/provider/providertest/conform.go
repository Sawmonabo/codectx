package providertest

import (
	"context"
	"slices"
	"sync"
	"testing"

	"github.com/Sawmonabo/codectx/internal/model"
	"github.com/Sawmonabo/codectx/internal/provider"
	"github.com/Sawmonabo/codectx/internal/workspace"
)

// Func is a provider assembled from functions, for tests that need a
// controlled producer: a fixed descriptor, an optional Detect and an IndexUnit
// body. A nil DetectFn reports an available provider with every declared
// capability.
type Func struct {
	Desc     model.ProviderDescriptor
	DetectFn func(context.Context, workspace.Root, workspace.Policy) (provider.Detection, error)
	IndexFn  func(context.Context, provider.UnitRequest, provider.Sink) (model.ProviderResult, error)
}

func (f Func) Descriptor() model.ProviderDescriptor { return f.Desc }

func (f Func) Detect(ctx context.Context, root workspace.Root, policy workspace.Policy) (provider.Detection, error) {
	if f.DetectFn == nil {
		return provider.Detection{Available: true, Capabilities: f.Desc.Capabilities}, nil
	}
	return f.DetectFn(ctx, root, policy)
}

func (f Func) IndexUnit(ctx context.Context, req provider.UnitRequest, sink provider.Sink) (model.ProviderResult, error) {
	return f.IndexFn(ctx, req, sink)
}

// Succeeded is the result a provider returns for a complete unit.
func Succeeded(req provider.UnitRequest, records, bytes uint64) model.ProviderResult {
	return model.ProviderResult{RunID: req.Run, State: model.RunSucceeded, RecordsEmitted: records, BytesProcessed: bytes}
}

// Recorder wraps a unit output and records the identities that reached it, so
// a test can compare what two runs persisted without activating and querying
// a generation.
type Recorder struct {
	provider.UnitOutput

	mu        sync.Mutex
	Nodes     []model.NodeID
	Relations []model.RelationID
	Aliases   []model.NativeAlias
	Search    []string
}

func (r *Recorder) PutNodes(ctx context.Context, facts []model.NodeFact) error {
	return r.PutKeyedNodes(ctx, facts, nil)
}

// PutKeyedNodes records the identities and forwards the batch with its
// provider fact keys. The Recorder embeds the UnitOutput interface, which does
// not carry the keyed methods, so without this an incremental provider's keyed
// batch reaches a destination that cannot record keys and the sink refuses it
// (provider.DeltaSink) — a property of the harness, not of the provider.
func (r *Recorder) PutKeyedNodes(ctx context.Context, facts []model.NodeFact, keys [][]string) error {
	r.mu.Lock()
	for _, f := range facts {
		r.Nodes = append(r.Nodes, f.Node.ID)
	}
	r.mu.Unlock()
	if d, ok := r.UnitOutput.(provider.DeltaSink); ok {
		return d.PutKeyedNodes(ctx, facts, keys)
	}
	return r.UnitOutput.PutNodes(ctx, facts)
}

func (r *Recorder) PutRelations(ctx context.Context, facts []model.RelationFact) error {
	return r.PutKeyedRelations(ctx, facts, nil)
}

// PutKeyedRelations is PutKeyedNodes for relation facts.
func (r *Recorder) PutKeyedRelations(ctx context.Context, facts []model.RelationFact, keys [][]string) error {
	r.mu.Lock()
	for _, f := range facts {
		r.Relations = append(r.Relations, f.Relation.ID)
	}
	r.mu.Unlock()
	if d, ok := r.UnitOutput.(provider.DeltaSink); ok {
		return d.PutKeyedRelations(ctx, facts, keys)
	}
	return r.UnitOutput.PutRelations(ctx, facts)
}

func (r *Recorder) PutAliases(ctx context.Context, aliases []model.NativeAlias) error {
	r.mu.Lock()
	r.Aliases = append(r.Aliases, aliases...)
	r.mu.Unlock()
	return r.UnitOutput.PutAliases(ctx, aliases)
}

func (r *Recorder) PutSearchUnits(ctx context.Context, docs []model.SearchUnit) error {
	r.mu.Lock()
	for _, d := range docs {
		r.Search = append(r.Search, d.ID)
	}
	r.mu.Unlock()
	return r.UnitOutput.PutSearchUnits(ctx, docs)
}

// Identities is the sorted set of identities persisted, the value two
// source-equivalent runs must agree on.
func (r *Recorder) Identities() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, 0, len(r.Nodes)+len(r.Relations)+len(r.Aliases)+len(r.Search))
	for _, n := range r.Nodes {
		out = append(out, "node:"+string(n))
	}
	for _, rel := range r.Relations {
		out = append(out, "relation:"+string(rel))
	}
	for _, a := range r.Aliases {
		out = append(out, "alias:"+a.ScopeKey+"\x00"+a.NativeKey+"\x00"+string(a.NodeID))
	}
	for _, s := range r.Search {
		out = append(out, "search:"+s)
	}
	slices.Sort(out)
	return slices.Compact(out)
}

// Conform is the shared conformance check every provider runs: its descriptor
// validates, its detection over the workspace validates, one unit over inputs
// succeeds and seals, and a second run over an identical fresh repository
// yields the same unit identity and the same set of persisted identities.
// Failure mode: a provider whose identities depend on discovery order, map
// iteration or wall time would publish different canonical IDs for the same
// bytes, so unchanged facts could never be shared between snapshots and
// unit reuse would silently compare unequal keys.
//
// In the second run every Put is followed by a Flush of the real sink, so
// each record is persisted before the provider's next call. A provider that
// hands a relation, alias or search document to the sink before the node fact
// it references then fails here deterministically (storage rejects the
// unregistered identity), instead of only under pool pressure in production
// where flush timing is not the provider's to control.
func Conform(t *testing.T, p provider.Provider, files map[string]string, scopeKey string, inputs []string) {
	t.Helper()
	if err := p.Descriptor().Validate(); err != nil {
		t.Fatalf("descriptor: %v", err)
	}
	var runs [2]struct {
		unit model.UnitID
		ids  []string
	}
	for i := range runs {
		h := New(t, files)
		det, err := p.Detect(context.Background(), h.Root, h.Policy)
		if err != nil {
			t.Fatalf("Detect: %v", err)
		}
		if err := det.Validate(p.Descriptor()); err != nil {
			t.Fatalf("Detect result: %v", err)
		}
		u := h.Plan(t, p, scopeKey, inputs)
		rec := &Recorder{UnitOutput: h.Begin(t, u, inputs)}
		run := p
		if i == 1 {
			run = flushEveryPut{Provider: p}
		}
		result, err := provider.RunUnit(h.ctx, run, u.Request, rec, Limits, h.Pool)
		if err != nil {
			t.Fatalf("RunUnit: %v", err)
		}
		if result.State != model.RunSucceeded {
			t.Fatalf("run state = %s, want succeeded", result.State)
		}
		if state, ok := h.UnitState(t, u.Build.Spec.ID); !ok || state != model.UnitSealed {
			t.Fatalf("unit state after a succeeded run = %q (exists %v), want sealed", state, ok)
		}
		runs[i].unit, runs[i].ids = u.Build.Spec.ID, rec.Identities()
	}
	if runs[0].unit != runs[1].unit {
		t.Fatalf("unit identity differs across identical repositories: %s vs %s", runs[0].unit, runs[1].unit)
	}
	if !slices.Equal(runs[0].ids, runs[1].ids) {
		t.Fatalf("persisted identities differ across identical repositories:\n%v\n%v", runs[0].ids, runs[1].ids)
	}
}

// flushable is the sink RunUnit hands a provider: the Sink methods plus the
// batch flush the harness triggers after every call.
type flushable interface {
	provider.DeltaSink
	Flush(context.Context) error
}

// flushEveryPut wraps a provider so the sink it sees persists after every
// Put. The wrapper changes when rows reach storage, never which rows.
type flushEveryPut struct {
	provider.Provider
}

func (w flushEveryPut) IndexUnit(ctx context.Context, req provider.UnitRequest, sink provider.Sink) (model.ProviderResult, error) {
	fs, ok := sink.(flushable)
	if !ok {
		return model.ProviderResult{}, &model.Error{Code: model.CodeInternal, Message: "conformance run received a sink that cannot be flushed"}
	}
	return w.Provider.IndexUnit(ctx, req, flushing{fs})
}

// flushing forwards each Put to the real sink and then flushes it.
type flushing struct {
	sink flushable
}

func (f flushing) PutNodes(ctx context.Context, facts []model.NodeFact) error {
	return f.then(ctx, f.sink.PutNodes(ctx, facts))
}

// PutKeyedNodes keeps the keys of an incremental provider's batch on the
// per-put-flush path; dropping them here would make the flush variant of the
// conformance run exercise a different sink contract from the batched one.
func (f flushing) PutKeyedNodes(ctx context.Context, facts []model.NodeFact, keys [][]string) error {
	return f.then(ctx, f.sink.PutKeyedNodes(ctx, facts, keys))
}

func (f flushing) PutRelations(ctx context.Context, facts []model.RelationFact) error {
	return f.then(ctx, f.sink.PutRelations(ctx, facts))
}

// PutKeyedRelations is PutKeyedNodes for relation facts.
func (f flushing) PutKeyedRelations(ctx context.Context, facts []model.RelationFact, keys [][]string) error {
	return f.then(ctx, f.sink.PutKeyedRelations(ctx, facts, keys))
}

func (f flushing) PutAliases(ctx context.Context, aliases []model.NativeAlias) error {
	return f.then(ctx, f.sink.PutAliases(ctx, aliases))
}

func (f flushing) PutSearchUnits(ctx context.Context, docs []model.SearchUnit) error {
	return f.then(ctx, f.sink.PutSearchUnits(ctx, docs))
}

func (f flushing) then(ctx context.Context, err error) error {
	if err != nil {
		return err
	}
	return f.sink.Flush(ctx)
}
