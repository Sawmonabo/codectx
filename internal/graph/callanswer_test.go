package graph

import (
	"context"
	"testing"

	"github.com/Sawmonabo/codectx/internal/model"
)

// TestCallersAnswerLocatesEveryNodeAndCallSite pins a self-locating call
// hierarchy: one callers call returns where the callee and each caller are
// (their stored byte ranges, file and path) and where every call is (each
// call site's range, with its dispatch), so a client needs no follow-up symbol
// or references lookup to learn where anything is.
//
// Mutation caught: hydrating the answer's nodes without their stored interval
// (the delivery read that drops StoredNode.Bytes), or dropping the call
// relation's occurrences.
func TestCallersAnswerLocatesEveryNodeAndCallSite(t *testing.T) {
	f := loadCallFixture(t, "two_subclasses")
	e := f.engine(t)
	base := f.id(t, "Animal.sound")
	res, err := e.Neighbors(context.Background(), model.GraphRequest{
		Start: []model.NodeID{base}, Direction: model.DirectionIncoming,
		Relations: []model.RelationKind{model.RelCalls},
	})
	if err != nil {
		t.Fatalf("callers: %v", err)
	}
	locs := map[model.NodeID]model.NodeLocation{}
	for _, l := range res.Locations {
		locs[l.NodeID] = l
	}
	for _, name := range []string{"Animal.sound", "speak"} {
		id := f.id(t, name)
		got, ok := locs[id]
		if !ok {
			t.Errorf("the answer carries no location for %s; a caller would need a symbol lookup to find it", name)
			continue
		}
		if want := f.located[id]; got != want {
			t.Errorf("%s located at %+v, want the stored %+v", name, got, want)
		}
	}
	call := f.rel(t, "speak", model.RelCalls, "Animal.sound")
	var sites []model.RelationOccurrence
	for _, o := range res.Occurrences {
		if o.RelationID == call {
			sites = append(sites, o)
		}
	}
	want := f.evidence[call]
	if len(sites) != len(want) {
		t.Fatalf("speak -> Animal.sound carries %d call sites, want %d", len(sites), len(want))
	}
	for i, o := range sites {
		if o.EvidenceID != want[i].ID || o.Bytes != *want[i].Bytes || o.FileID != want[i].FileID ||
			o.Dispatch != want[i].Dispatch {
			t.Errorf("call site %d = %+v, want the stored occurrence %+v", i, o, want[i])
		}
	}
}
