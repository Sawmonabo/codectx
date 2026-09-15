package graph_test

import (
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
func TestMemoryGraphConformsToTheReaderPort(t *testing.T) {
	graphtest.RunConformance(t, func(t *testing.T) graph.GraphReader {
		return graph.NewMemoryGraph(conformanceBinding, graphtest.Nodes(), graphtest.Relations())
	})
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
