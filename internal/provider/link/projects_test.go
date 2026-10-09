package link_test

import (
	"context"
	"fmt"
	"slices"
	"testing"

	"github.com/Sawmonabo/codectx/internal/model"
	"github.com/Sawmonabo/codectx/internal/provider/link"
	"github.com/Sawmonabo/codectx/internal/provider/providertest"
)

// TestPlanUnits protects the project boundaries and the closure a unit's
// identity is keyed on.
//
// Failure modes: a module nested in another module planned into both would
// link and publish every call it holds twice; a lock file of the project
// missing from its closure lets the unit be reused across a dependency change
// it never saw; a lock file the snapshot holds as a tombstone that the unit no
// longer recognises leaves the scope eligible for carry (Section 13.3), so the
// planner emits a carry that AttachCarried refuses because one of the unit's
// inputs no longer exists in the snapshot.
//
// Mutation: drop the Excluded check from Unit.Contains -> the outer module
// owns tool/tool.go and the test fails. Second mutation: build OwnsInput from
// the live manifest's marker rows instead of the predicate -> the tombstoned
// go.sum is not owned and the test fails.
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
	if plan[0].Files != 1 || plan[0].Bytes == 0 {
		t.Errorf("outer module folded %d files and %d bytes; the planner weighs a unit from these",
			plan[0].Files, plan[0].Bytes)
	}
	for path, owned := range map[string]bool{"app.go": true, "go.mod": true, "go.sum": true, "docs/overview.md": false} {
		if plan[0].OwnsInput(path) != owned {
			t.Errorf("the root module's closure holds %s = %v, want %v", path, !owned, owned)
		}
	}

	vendored := map[string]string{"go.mod": repo["go.mod"], "app.go": repo["app.go"]}
	for i := 0; i < 2; i++ {
		vendored[fmt.Sprintf("vendored/m%02d/go.sum", i)] = fmt.Sprintf("example.com/m%02d v1.0.0 h1:x=\n", i)
	}
	vh := providertest.New(t, vendored)
	const tombstone = "vendored/m02/go.sum"
	view := withTombstone{SnapshotView: vh.View,
		row: model.FileVersion{ID: model.NewFileID(vh.Repo, tombstone), Path: tombstone, Status: model.FileDeleted}}
	vp, err := link.PlanUnits(context.Background(), view)
	if err != nil {
		t.Fatal(err)
	}
	if len(vp.Units) != 1 || vp.Units[0].ScopeKey != rootScope {
		t.Fatalf("plan = %v, want one %s unit", vp.Units, rootScope)
	}
	if !vp.Units[0].OwnsInput(tombstone) {
		t.Errorf("the unit does not own %s, a lock file the snapshot holds as a tombstone; the scope stays carry-eligible after a member is deleted", tombstone)
	}
}

// withTombstone is a snapshot view with one deleted manifest row appended.
// providertest retains live files only, and a tombstone is precisely the row
// the planner must still resolve to a member. The row is yielded whatever the
// selection says; both callers pass an empty FileSelection. It is appended
// last, and the path is chosen to sort after every fixture path, so the view's
// ascending-path order is preserved.
type withTombstone struct {
	model.SnapshotView
	row model.FileVersion
}

func (v withTombstone) EachFile(ctx context.Context, sel model.FileSelection, fn func(model.FileVersion) error) error {
	if err := v.SnapshotView.EachFile(ctx, sel, fn); err != nil {
		return err
	}
	return fn(v.row)
}
