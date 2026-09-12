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
	r.mu.Lock()
	for _, f := range facts {
		r.Nodes = append(r.Nodes, f.Node.ID)
	}
	r.mu.Unlock()
	return r.UnitOutput.PutNodes(ctx, facts)
}

func (r *Recorder) PutRelations(ctx context.Context, facts []model.RelationFact) error {
	r.mu.Lock()
	for _, f := range facts {
		r.Relations = append(r.Relations, f.Relation.ID)
	}
	r.mu.Unlock()
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
		result, err := provider.RunUnit(h.ctx, p, u.Request, rec, Limits, h.Pool)
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
