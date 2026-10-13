package provider_test

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/Sawmonabo/codectx/internal/model"
	"github.com/Sawmonabo/codectx/internal/provider"
)

// factSink captures the relation facts the sink hands over.
type factSink struct {
	relations []model.RelationFact
}

func (f *factSink) PutNodes(context.Context, []model.NodeFact) error { return nil }
func (f *factSink) PutRelations(_ context.Context, r []model.RelationFact) error {
	f.relations = append(f.relations, r...)
	return nil
}
func (f *factSink) PutAliases(context.Context, []model.NativeAlias) error    { return nil }
func (f *factSink) PutSearchUnits(context.Context, []model.SearchUnit) error { return nil }

// TestSinkSplitsAFactPastTheBatchIntoRunsOfItsEvidence protects a relation
// with more occurrences than one storage batch holds: the writer refuses a
// batch over index.batch_bytes, so a fact handed on whole would fail its
// unit. It must reach the destination as facts of the same relation, each
// within the batch, whose evidence together is every occurrence once.
//
// Mutations it catches: handing the fact on whole (one fact over the batch),
// dropping or repeating a run, or cutting a run past the batch.
func TestSinkSplitsAFactPastTheBatchIntoRunsOfItsEvidence(t *testing.T) {
	ctx := context.Background()
	rel := model.Relation{ID: model.RelationID(model.H("rel")), From: model.NodeID(model.H("a")),
		Kind: model.RelCalls, To: model.NodeID(model.H("b"))}
	fact := model.RelationFact{Relation: rel}
	for i := range 300 {
		ev := model.Evidence{RelationID: rel.ID, Precision: model.PrecisionSyntax,
			Detail: fmt.Sprintf("%04d %s", i, strings.Repeat("x", 900))}
		ev.ID = model.NewEvidenceID(ev)
		fact.Evidence = append(fact.Evidence, ev)
	}
	limits := provider.Limits{BatchRecords: 1000, BatchBytes: provider.RelationFactBytes(fact) / 7}
	pool, err := provider.NewPool(4 * limits.BatchBytes)
	if err != nil {
		t.Fatal(err)
	}
	runCtx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	dst := &factSink{}
	sink, err := provider.NewBatchSink(runCtx, dst, limits, pool, cancel)
	if err != nil {
		t.Fatal(err)
	}
	defer sink.Discard()
	if err := sink.PutRelations(ctx, []model.RelationFact{fact}); err != nil {
		t.Fatalf("PutRelations: %v", err)
	}
	if err := sink.Flush(ctx); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	if len(dst.relations) < 7 {
		t.Fatalf("the fact reached the destination as %d facts, want it cut into runs that fit the batch", len(dst.relations))
	}
	seen := map[model.EvidenceID]int{}
	for _, f := range dst.relations {
		if f.Relation != rel {
			t.Fatalf("a run carries relation %+v, want %+v", f.Relation, rel)
		}
		if size := provider.RelationFactBytes(f); size > limits.BatchBytes {
			t.Fatalf("a run is %d bytes, past the %d-byte batch", size, limits.BatchBytes)
		}
		for _, ev := range f.Evidence {
			seen[ev.ID]++
		}
	}
	for _, ev := range fact.Evidence {
		if seen[ev.ID] != 1 {
			t.Fatalf("occurrence %s reached the destination %d times, want once", ev.ID, seen[ev.ID])
		}
	}
}
