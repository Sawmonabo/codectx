package link_test

import (
	"context"
	"slices"
	"testing"

	"github.com/Sawmonabo/codectx/internal/provider/link"
	"github.com/Sawmonabo/codectx/internal/provider/providertest"
)

// TestPlanUnits protects the project boundaries and the closure a unit's
// identity is keyed on.
//
// Failure modes: a module nested in another module planned into both would
// link and publish every call it holds twice; a lock file of the project
// missing from its closure lets the unit be reused across a dependency change
// it never saw.
//
// Mutation: drop the Excluded check from Unit.Contains -> the outer module
// owns tool/tool.go and the test fails. Second mutation: drop closureMarkers
// from OwnsInput -> go.sum is not owned and the test fails.
func TestPlanUnits(t *testing.T) {
	repo := map[string]string{
		"go.mod":           "module example.com/app\n\ngo 1.27\n",
		"go.sum":           "",
		"app.go":           "package app\n\nfunc Run(x int) int {\n\ty := x + 1\n\treturn y\n}\n",
		"tool/go.mod":      "module example.com/app/tool\n\ngo 1.27\n",
		"tool/tool.go":     "package tool\n\nfunc Help() string { return \"help\" }\n",
		"docs/overview.md": "# overview\n",
	}
	const rootScope = "pkg:go:"
	h := providertest.New(t, repo)
	p, err := link.PlanUnits(context.Background(), h.View)
	if err != nil {
		t.Fatal(err)
	}
	plan := p.Units
	var keys []string
	for _, u := range plan {
		keys = append(keys, u.ScopeKey)
	}
	want := []string{rootScope, "pkg:go:tool"}
	if !slices.Equal(keys, want) {
		t.Fatalf("plan = %v, want %v", keys, want)
	}
	if plan[0].Contains("tool/tool.go") {
		t.Error("the outer module owns the nested module's file; it would be linked twice")
	}
	if !plan[1].Contains("tool/tool.go") {
		t.Error("the nested module does not own its own file")
	}
	if plan[0].Files != 1 {
		t.Errorf("outer module folded %d files, want 1; a unit with no files is never planned", plan[0].Files)
	}
	for path, owned := range map[string]bool{"app.go": true, "go.mod": true, "go.sum": true, "docs/overview.md": false} {
		if plan[0].OwnsInput(path) != owned {
			t.Errorf("the root module's closure holds %s = %v, want %v", path, !owned, owned)
		}
	}
}

