package sqlite_test

import (
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/Sawmonabo/codectx/internal/model"
)

// TestCapabilityReportPublishesEveryRow protects the invariant that a
// generation's capability report is never refused for its length and never
// read back short. Two bounds used to break it together: Activate refused a
// report of more than model.MaxCapabilityStates entries, which made a large
// repository's index unpublishable, and the read path carried a bare
// `LIMIT 256`, which silently dropped the tail of the report that
// QueryMeta.Completeness is derived from -- so every answer over-claimed.
//
// Mutation proof: restore either bound (the `len(capabilities) >
// model.MaxCapabilityStates` refusal in units.go Activate, or `LIMIT ?` with
// model.MaxCapabilityStates in query.go Capabilities) and this test fails --
// the first on Activate, the second on the row count.
func TestCapabilityReportPublishesEveryRow(t *testing.T) {
	t.Parallel()
	f := newFixture(t, filepath.Join(t.TempDir(), "codectx.db"))
	ctx := f.ctx

	a := f.file("pkg/a.go", "package pkg\nfunc A() {}\n")
	snap := f.snapshot("one", a)
	gen, err := f.s.BeginGeneration(ctx, f.repo, snap.ID, model.H("semantic"), "main")
	if err != nil {
		t.Fatalf("BeginGeneration: %v", err)
	}
	run := f.run(gen)
	f.unit(gen, run, a)
	if err := f.s.CompleteProviderRun(ctx, model.ProviderResult{RunID: run, State: model.RunSucceeded,
		RecordsEmitted: 1, BytesProcessed: 20}, ""); err != nil {
		t.Fatalf("CompleteProviderRun: %v", err)
	}

	// 300 distinct (provider_id, capability, scope_key) triples -- past the
	// old 256 on both paths, and distinct so no fold is in play: this test
	// isolates the storage bounds, not the index package's aggregation.
	const rows = 300
	caps := make([]model.CapabilityState, 0, rows)
	for i := range rows {
		caps = append(caps, model.CapabilityState{
			ProviderID: providerID,
			Capability: fmt.Sprintf("structure-%03d", i),
			Scope:      fmt.Sprintf("scope-%03d", i),
			State:      model.CapabilityFresh,
		})
	}
	if _, err := f.s.Activate(ctx, gen, 0, model.HealthFresh, caps, "norm-v1"); err != nil {
		t.Fatalf("Activate with %d capability rows: %v", rows, err)
	}

	reader, err := f.s.PinGeneration(ctx, f.repo, 0, time.Minute)
	if err != nil {
		t.Fatalf("PinGeneration: %v", err)
	}
	defer reader.Close()
	got, err := reader.Capabilities(ctx)
	if err != nil {
		t.Fatalf("Capabilities: %v", err)
	}
	if len(got) != rows {
		t.Fatalf("Capabilities returned %d rows, want all %d; the read path dropped the tail", len(got), rows)
	}
	// The whole list must still validate: a completeness report longer than
	// any one constant anticipated may not fail the answer it describes.
	if err := (model.IndexStatus{Binding: reader.Binding(), Health: model.HealthFresh,
		Coherence: model.CoherenceSnapshot, CaptureConsistency: snap.CaptureConsistency,
		Completeness: got}).Validate(); err != nil {
		t.Fatalf("a %d-row completeness report failed validation: %v", rows, err)
	}
}
