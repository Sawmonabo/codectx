package graph_test

import (
	"cmp"
	"testing"

	"github.com/Sawmonabo/codectx/internal/graph"
	"github.com/Sawmonabo/codectx/internal/graph/graphtest"
	"github.com/Sawmonabo/codectx/internal/model"
)

// TestMemoryGraphConformsToTheReaderPort holds the in-heap reference reader to
// the port's contract. It protects the two invariants a walk cannot survive
// losing: an entry delivered exactly once across a stopped-and-resumed scan
// (anything else reports an entity twice or drops it), and a relation whose
// endpoint is not in the generation never reaching a walk.
// It runs TWICE: once with surrogates in canonical order, once with them
// REVERSED. The port promises nothing about the two orders agreeing -- the
// store assigns its own surrogates -- so a reader that answered correctly only
// while they happened to agree would pass the suite and be wrong against the
// store. The reversed run is what holds the fixture to the port rather than to
// its own numbering.
func TestMemoryGraphConformsToTheReaderPort(t *testing.T) {
	for _, tc := range []struct {
		name  string
		order graph.MemoryGraphOrder
	}{
		{name: "canonical order"},
		{name: "reversed surrogate order", order: reversedSurrogates},
	} {
		t.Run(tc.name, func(t *testing.T) {
			graphtest.RunConformance(t, func(t *testing.T) graph.GraphReader {
				return graph.NewMemoryGraphOrdered(conformanceBinding,
					graphtest.Nodes(), graphtest.Relations(), tc.order)
			})
		})
	}
}

// reversedSurrogates hands surrogate 1 to the LAST canonical id, so a
// surrogate-keyed answer and a canonical-keyed one diverge on every fixture
// built with it.
var reversedSurrogates = graph.MemoryGraphOrder{
	Nodes:     func(a, b model.NodeID) int { return -cmp.Compare(a, b) },
	Relations: func(a, b model.RelationID) int { return -cmp.Compare(a, b) },
}

var conformanceBinding = model.Binding{
	RepositoryID: graphtest.FixtureRepository,
	SnapshotID:   model.SnapshotID(hex64('1')),
	GenerationID: 1,
	AnalysisKey:  model.AnalysisKey(hex64('3')),
}

func hex64(c byte) string {
	b := make([]byte, 64)
	for i := range b {
		b[i] = c
	}
	return string(b)
}
