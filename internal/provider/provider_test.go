package provider_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/Sawmonabo/codectx/internal/model"
	"github.com/Sawmonabo/codectx/internal/provider"
	"github.com/Sawmonabo/codectx/internal/provider/providertest"
	"github.com/Sawmonabo/codectx/internal/workspace"
)

// recordingSink captures every batch the sink hands over.
type recordingSink struct {
	batches [][]model.NativeAlias
}

func (r *recordingSink) PutNodes(context.Context, []model.NodeFact) error         { return nil }
func (r *recordingSink) PutRelations(context.Context, []model.RelationFact) error { return nil }
func (r *recordingSink) PutSearchUnits(context.Context, []model.SearchUnit) error { return nil }
func (r *recordingSink) PutAliases(_ context.Context, a []model.NativeAlias) error {
	r.batches = append(r.batches, a)
	return nil
}

func wantCode(t *testing.T, err error, code string) *model.Error {
	t.Helper()
	var typed *model.Error
	if !errors.As(err, &typed) {
		t.Fatalf("got %v (%T), want *model.Error with code %s", err, err, code)
	}
	if typed.Code != code {
		t.Fatalf("got code %s (%s), want %s", typed.Code, typed.Message, code)
	}
	return typed
}

// TestSinkBoundsAreEnforcedAtTheBoundary protects the Section 11.1 batch
// bounds, which are what keeps provider output from growing the Go heap
// without limit. Failure modes: a byte cap that is only checked after the
// record cap lets a batch of few, large records exceed the memory reservation
// before it is persisted. An oversize single record is the second half: it
// must be admitted and counted, because a record bound the user set is a
// reporting threshold, and failing the unit over one large symbol loses every
// good record of the unit with it.
func TestSinkBoundsAreEnforcedAtTheBoundary(t *testing.T) {
	ctx := context.Background()
	dst := &recordingSink{}
	alias := model.NativeAlias{ScopeKey: "pkg", NativeKey: "Sym", NodeID: model.NodeID(model.H("node"))}
	size := provider.AliasBytes(alias)
	// Three records fit; the fourth would overflow the byte cap long before
	// the record cap of ten.
	limits := provider.Limits{BatchRecords: 10, BatchBytes: 3*size + size/2, MaxRecordBytes: 3*size + size/2}
	pool, err := provider.NewPool(4 * limits.BatchBytes)
	if err != nil {
		t.Fatal(err)
	}
	runCtx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	sink, err := provider.NewBatchSink(runCtx, dst, limits, pool, cancel)
	if err != nil {
		t.Fatal(err)
	}
	defer sink.Discard()
	for i := 0; i < 7; i++ {
		if err := sink.PutAliases(ctx, []model.NativeAlias{alias}); err != nil {
			t.Fatalf("PutAliases(%d): %v", i, err)
		}
	}
	if err := sink.Flush(ctx); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	var sizes []int
	for _, b := range dst.batches {
		sizes = append(sizes, len(b))
	}
	if len(sizes) != 3 || sizes[0] != 3 || sizes[1] != 3 || sizes[2] != 1 {
		t.Fatalf("batch sizes = %v, want [3 3 1]: the byte cap must flush exactly at the boundary", sizes)
	}
	if used := pool.Used(); used != 0 {
		t.Fatalf("pool still charges %d bytes after everything was persisted; reservations leaked", used)
	}

	// An oversize record is admitted and reported, not refused.
	big := alias
	big.NativeKey = strings.Repeat("k", int(limits.MaxRecordBytes))
	bigSize := provider.AliasBytes(big)
	if err := sink.PutAliases(ctx, []model.NativeAlias{big}); err != nil {
		t.Fatalf("a record over max_provider_record_bytes must not fail the unit: %v", err)
	}
	if err := sink.Flush(ctx); err != nil {
		t.Fatalf("Flush after the oversize record: %v", err)
	}
	if len(dst.batches) != 4 || sink.Records() != 8 || pool.Used() != 0 {
		t.Fatalf("the oversize record was not persisted: %d batches, %d records, %d pool bytes", len(dst.batches), sink.Records(), pool.Used())
	}
	degraded := sink.Degradations()
	if len(degraded) != 1 {
		t.Fatalf("Degradations() = %v, want the one bound the record exceeded", degraded)
	}
	want := provider.Degradation{Limit: "max_provider_record_bytes", Bound: limits.MaxRecordBytes, Count: 1, Largest: bigSize}
	if degraded[0] != want {
		t.Fatalf("Degradations() = %+v, want %+v: the report must name the bound, the count and the size", degraded[0], want)
	}
}

// TestFailedWriteCancelsProducersAndDiscardsTheUnit protects the Section 11.1
// rule that failed unsealed output cannot be queried. Failure modes: a
// provider whose write failed but whose goroutines keep producing would spend
// reservations on facts that can never be admitted, and could report a
// succeeded run over half-persisted output; a unit left building or attached
// after the failure would leak partial facts into a generation.
func TestFailedWriteCancelsProducersAndDiscardsTheUnit(t *testing.T) {
	h := providertest.New(t, map[string]string{"a.go": "package a\n"})
	ctx := context.Background()
	a := h.File(t, "a.go")
	// other.go is not one of the unit's declared inputs, so storage refuses the
	// batch when the sink flushes it.
	other := model.FileVersion{ID: model.NewFileID(h.Repo, "other.go"), ContentHash: model.H("other")}
	var canceledSeen bool
	p := providertest.Func{
		Desc: model.ProviderDescriptor{ID: "bad", Version: "1", Capabilities: []string{"structure"}, InvalidationScope: model.InvalidationFile, Required: true},
		IndexFn: func(ctx context.Context, req provider.UnitRequest, sink provider.Sink) (model.ProviderResult, error) {
			for i := 0; i <= providertest.Limits.BatchRecords; i++ {
				cand := model.NodeCandidate{ProviderID: "bad", ScopeKey: "a.go", NativeKey: "f" + string(rune('a'+i%26)) + string(rune('a'+i/26)),
					Kind: model.NodeFunction, Name: "f", FileID: other.ID, ContentHash: other.ContentHash,
					Range: &model.SourceRange{Start: model.Position{Byte: uint64(i), Line: 1}, End: model.Position{Byte: uint64(i + 1), Line: 1}}}
				res, err := req.Resolver.Resolve(ctx, cand)
				if err != nil {
					return model.ProviderResult{}, err
				}
				fact := model.NodeFact{Node: res.Node, CanonicalKey: res.CanonicalKey,
					Evidence: []model.Evidence{providertest.Evidence(req, res.Node.ID, model.PrecisionSyntax, other, cand.Range)}}
				if err := sink.PutNodes(ctx, []model.NodeFact{fact}); err != nil {
					canceledSeen = ctx.Err() != nil
					return model.ProviderResult{}, err
				}
			}
			return providertest.Succeeded(req, 0, 0), nil
		},
	}
	result, unit, err := h.Run(t, p, "a.go", []string{"a.go"})
	wantCode(t, err, model.CodeProviderOutputInvalid)
	if !canceledSeen {
		t.Fatal("the producer's context was not canceled after the write failed")
	}
	if result.State != model.RunFailed {
		t.Fatalf("run state = %s, want failed (a write failure is not a caller cancellation)", result.State)
	}
	if state, exists := h.UnitState(t, unit); exists {
		t.Fatalf("failed unit still exists in state %q; unsealed output must be deleted", state)
	}
	if used := h.Pool.Used(); used != 0 {
		t.Fatalf("pool still charges %d bytes after the failed unit was discarded; the run's reservation would shrink for every later unit", used)
	}
	if _, err := h.Store.Activate(ctx, h.Gen, 0, model.HealthFresh, nil, "norm-v1"); err == nil {
		t.Fatal("generation activated with the failed unit's output attached")
	} else {
		wantCode(t, err, model.CodeVersionConflict)
	}
	// a.go's bytes were never a problem; the file stays readable through the view.
	if _, _, err := h.View.ReadRange(ctx, a.ID, model.ByteRange{Start: 0, End: 7}); err != nil {
		t.Fatalf("snapshot view after a failed unit: %v", err)
	}
}

// TestRegistryRejectsBrokenGraphs protects against false readiness: a
// coordinator scheduling over a graph with a duplicate ID, an unknown
// dependency or a cycle would either run two providers under one identity or
// wait forever for a dependency that can never complete while reporting the
// dependents as pending rather than failed.
func TestRegistryRejectsBrokenGraphs(t *testing.T) {
	desc := func(id string, deps ...string) providertest.Func {
		return providertest.Func{Desc: model.ProviderDescriptor{ID: id, Version: "1", DependsOn: deps, InvalidationScope: model.InvalidationFile}}
	}
	cases := []struct {
		name      string
		providers []provider.Provider
	}{
		{"duplicate id", []provider.Provider{desc("fs"), desc("fs")}},
		{"missing dependency", []provider.Provider{desc("scip", "treesitter")}},
		{"cycle", []provider.Provider{desc("a", "b"), desc("b", "c"), desc("c", "a"), desc("fs")}},
	}
	for _, tc := range cases {
		if _, err := provider.NewRegistry(tc.providers...); err == nil {
			t.Errorf("%s: NewRegistry accepted a graph no coordinator can complete", tc.name)
		} else {
			wantCode(t, err, model.CodeArgumentInvalid)
		}
	}
}

// TestSelectPublishesDetectionDetails protects against false readiness in the
// mixed case: a provider can be available as a whole and unusable in part --
// five of six SCIP indexers installed, the sixth refused -- and the reasons
// exist only in Detection.Details. Select is the one place capability state is
// published, so details dropped here make a partly working provider
// indistinguishable from a healthy one at the only surface a caller can read,
// and the missing language looks like a repository with no such code.
func TestSelectPublishesDetectionDetails(t *testing.T) {
	h := providertest.New(t, map[string]string{"a.go": "package a\n"})
	desc := model.ProviderDescriptor{ID: "scip", Version: "1", Capabilities: []string{"definitions", "references"},
		InvalidationScope: model.InvalidationFile}
	p := providertest.Func{Desc: desc, DetectFn: func(context.Context, workspace.Root, workspace.Policy) (provider.Detection, error) {
		return provider.Detection{Available: true, Capabilities: desc.Capabilities}.
			WithDetail("rust-analyzer", model.CodeToolOverrideInvalid), nil
	}}
	reg, err := provider.NewRegistry(p)
	if err != nil {
		t.Fatal(err)
	}
	sel, err := reg.Select(context.Background(), h.Root, h.Policy, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(sel.Active) != 1 {
		t.Fatalf("Active = %d providers, want the detected one", len(sel.Active))
	}
	if len(sel.States) != len(desc.Capabilities) {
		t.Fatalf("published states = %+v, want one partial row per declared capability", sel.States)
	}
	for i, got := range sel.States {
		if got.ProviderID != "scip" || got.Capability != desc.Capabilities[i] || got.Scope != provider.ScopeWorkspace ||
			got.State != model.CapabilityPartial || got.Details["rust-analyzer"] != model.CodeToolOverrideInvalid {
			t.Fatalf("published state %d = %+v, want a partial workspace row carrying the detection detail", i, got)
		}
		if got.Validate() != nil {
			t.Fatalf("published state %d does not validate: %v", i, got.Validate())
		}
	}
	// One map per row: a caller that edits one published state must not rewrite
	// the others.
	sel.States[0].Details["rust-analyzer"] = "edited"
	if sel.States[1].Details["rust-analyzer"] != model.CodeToolOverrideInvalid {
		t.Fatal("published states share one details map")
	}
	// The other half of the same false-readiness question, in the other
	// direction: a detail that is not a refusal must publish nothing. A pinned
	// payload the first unit fetches and then indexes at full precision is such
	// a detail, and it is on every SCIP kind on a cold store -- published as
	// `partial`, the only surface carrying capability state reports every first
	// run of the product as degraded, and a real refusal beside it becomes
	// indistinguishable from routine.
	pending := providertest.Func{Desc: desc, DetectFn: func(context.Context, workspace.Root, workspace.Policy) (provider.Detection, error) {
		return provider.Detection{Available: true, Capabilities: desc.Capabilities}.
			WithDetail("scip-go", "deferred"), nil
	}}
	pendingReg, err := provider.NewRegistry(pending)
	if err != nil {
		t.Fatal(err)
	}
	pendingSel, err := pendingReg.Select(context.Background(), h.Root, h.Policy, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(pendingSel.Active) != 1 || len(pendingSel.States) != 0 {
		t.Fatalf("a deferred-only detection published %+v, want no capability row", pendingSel.States)
	}
}

// TestOversizeRecordSealsTheUnitAndReportsTheBound protects the degradation
// surface the sink gained for row 22: a record larger than a user-set
// resources.max_provider_record_bytes must seal with the rest of the unit and
// be reported, not fail it. Failure modes: refusing the record loses every
// good fact of the unit with it -- one generated symbol takes the whole file's
// structure out of the index; admitting it silently leaves the caller unable
// to tell that the bound they set was exceeded, which the ruling forbids just
// as firmly as the refusal.
func TestOversizeRecordSealsTheUnitAndReportsTheBound(t *testing.T) {
	h := providertest.New(t, map[string]string{"a.go": "package a\n"})
	ctx := context.Background()
	a := h.File(t, "a.go")
	p := providertest.Func{
		Desc: model.ProviderDescriptor{ID: "big", Version: "1", Capabilities: []string{"structure"}, InvalidationScope: model.InvalidationFile},
		IndexFn: func(ctx context.Context, req provider.UnitRequest, sink provider.Sink) (model.ProviderResult, error) {
			cand := model.NodeCandidate{ProviderID: "big", ScopeKey: "a.go", NativeKey: "f", Kind: model.NodeFunction,
				Name: "f", QualifiedName: strings.Repeat("q", 512), FileID: a.ID, ContentHash: a.ContentHash,
				Range: &model.SourceRange{Start: model.Position{Byte: 0, Line: 1}, End: model.Position{Byte: 1, Line: 1}}}
			res, err := req.Resolver.Resolve(ctx, cand)
			if err != nil {
				return model.ProviderResult{}, err
			}
			fact := model.NodeFact{Node: res.Node, CanonicalKey: res.CanonicalKey,
				Evidence: []model.Evidence{providertest.Evidence(req, res.Node.ID, model.PrecisionSyntax, a, cand.Range)}}
			if err := sink.PutNodes(ctx, []model.NodeFact{fact}); err != nil {
				return model.ProviderResult{}, err
			}
			return providertest.Succeeded(req, 1, 0), nil
		},
	}
	// The bound is set below one node fact's retained-byte estimate, so the
	// single fact this provider publishes is over it.
	limits := provider.Limits{BatchRecords: 64, BatchBytes: 256 << 10, MaxRecordBytes: 256}
	u := h.Plan(t, p, "a.go", []string{"a.go"})
	out := h.Begin(t, u, []string{"a.go"})
	result, err := provider.RunUnit(ctx, p, u.Request, out, limits, h.Pool)
	if err != nil {
		t.Fatalf("a record over max_provider_record_bytes must not fail the unit: %v", err)
	}
	if result.State != model.RunSucceeded {
		t.Fatalf("run state = %s, want succeeded", result.State)
	}
	if state, exists := h.UnitState(t, u.Build.Spec.ID); !exists || state != model.UnitSealed {
		t.Fatalf("unit state = %q (exists %v), want sealed: the oversize record's unit must reach a generation", state, exists)
	}
	if len(result.Capabilities) != 1 {
		t.Fatalf("capabilities = %v, want one synthesized row for the declared capability", result.Capabilities)
	}
	got := result.Capabilities[0]
	if got.State != model.CapabilityFresh || got.Capability != "structure" || got.Scope != u.Request.Unit.ScopeKey {
		t.Fatalf("capability row = %+v, want a fresh structure row for the unit's scope", got)
	}
	if got.Details["over_max_provider_record_bytes"] != "1" || got.Details["max_provider_record_bytes"] != "256" {
		t.Fatalf("capability details = %v, want the count and the bound the user set", got.Details)
	}
	if got.Details["largest_record_bytes"] == "" {
		t.Fatalf("capability details = %v, want the largest record size named", got.Details)
	}
	if err := got.Validate(); err != nil {
		t.Fatalf("the reported row must be storable: %v", err)
	}
}
