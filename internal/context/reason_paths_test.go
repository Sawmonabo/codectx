package context

import (
	"fmt"
	"strings"
	"testing"

	"github.com/Sawmonabo/codectx/internal/config"
	"github.com/Sawmonabo/codectx/internal/model"
)

// TestConfiguredReasonPathsSurviveToTheStoredEntry pins the class-G chain this
// wave removes: context.max_reason_paths_per_entry is honoured by the ranking
// lane, and neither the scope lane, the budget lane nor model.ContextEntry's
// own validator may silently re-clip the result back to the smaller model
// constant. The failure it protects against is a manifest that quietly stores
// three explanation routes when the operator configured more -- or unlimited --
// with nothing in the answer saying routes were discarded.
func TestConfiguredReasonPathsSurviveToTheStoredEntry(t *testing.T) {
	const routes = model.MaxReasonPathsPerEntry + 2
	paths := make([]model.RelationPath, 0, routes)
	for i := 0; i < routes; i++ {
		paths = append(paths, model.RelationPath{
			Relations: []model.RelationID{model.RelationID(hexID(byte(i)))},
		})
	}

	// scope: a configured value ABOVE the model constant is applied as written.
	kept, more := boundPaths(paths, config.Limit(routes))
	if len(kept) != routes || more != 0 {
		t.Fatalf("boundPaths(limit %d) kept %d paths, dropped %d; want %d kept and none dropped",
			routes, len(kept), more, routes)
	}
	// scope: unlimited keeps every route.
	if kept, more = boundPaths(paths, config.Unlimited); len(kept) != routes || more != 0 {
		t.Fatalf("boundPaths(unlimited) kept %d paths, dropped %d; want %d kept and none dropped",
			len(kept), more, routes)
	}
	// scope: a SMALLER configured value still cuts, and reports the cut.
	if kept, more = boundPaths(paths, config.Limit(1)); len(kept) != 1 || more != int64(routes-1) {
		t.Fatalf("boundPaths(limit 1) kept %d paths, dropped %d; want 1 kept and %d dropped",
			len(kept), more, routes-1)
	}

	// budget: the projection onto the stored entry does not re-clip the count.
	stored := evidencePaths(paths)
	if len(stored) != routes {
		t.Fatalf("evidencePaths stored %d routes, want all %d: the budget lane re-clipped what rank honoured",
			len(stored), routes)
	}

	// model: the persisted entry is not refused for carrying them.
	entry := model.ContextEntry{
		Ordinal:       0,
		NodeID:        model.NodeID(hexID(0xaa)),
		Requirement:   model.RequirementOptional,
		EvidencePaths: stored,
	}
	if err := entry.Validate(); err != nil {
		t.Fatalf("ContextEntry.Validate refused %d evidence paths: %v", routes, err)
	}
}

// hexID is a syntactically valid 64-hex-character identity: these tests assert
// count semantics, not identity derivation.
func hexID(seed byte) string {
	return strings.Repeat(fmt.Sprintf("%02x", seed), 32)
}
