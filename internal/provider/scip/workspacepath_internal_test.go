package scip

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/Sawmonabo/codectx/internal/model"
	"github.com/Sawmonabo/codectx/internal/provider"
)

// TestAProjectPathIsResolvedAgainstTheWorkspace protects two halves of one
// rule. A project unit's document that reaches a workspace file through `../`
// must be admitted under that file's workspace path, or its facts are lost
// while the file is served; and a path that still escapes the workspace, or an
// absolute one, must stay refused, or the index would name bytes outside the
// snapshot. Dropping the clean in workspacePath fails the first row; joining
// an absolute path fails the fourth.
func TestAProjectPathIsResolvedAgainstTheWorkspace(t *testing.T) {
	for _, c := range []struct {
		prefix, raw, want string
		admitted          bool
	}{
		{"app", "../shared/x.ts", "shared/x.ts", true},
		{"app", "src/x.ts", "app/src/x.ts", true},
		{"app", "../../cache/x.go", "../cache/x.go", false},
		{"app", "/etc/x", "/etc/x", false},
		{"app", "..", ".", false},
		{"", "a/../b.go", "a/../b.go", false},
	} {
		got := workspacePath(c.prefix, c.raw)
		if got != c.want || rootRelative(got) != c.admitted {
			t.Errorf("workspacePath(%q, %q) = %q admitted %v, want %q admitted %v",
				c.prefix, c.raw, got, rootRelative(got), c.want, c.admitted)
		}
	}
}

// pathsView is a snapshot view that lists its paths and nothing else, or only
// the ones a selection names.
type pathsView struct {
	model.SnapshotView
	paths []string
}

func (v pathsView) EachFile(_ context.Context, sel model.FileSelection, fn func(model.FileVersion) error) error {
	for _, p := range v.paths {
		if len(sel.Paths) > 0 && !slices.Contains(sel.Paths, p) {
			continue
		}
		if err := fn(model.FileVersion{Path: p, Status: model.FileTracked}); err != nil {
			return err
		}
	}
	return nil
}

// TestEveryDocumentIsAdmittedByTheInnermostProjectAlone protects the
// partition of documents between the units of projects of one kind: each
// document is admitted by the unit of the innermost project holding it and
// refused by every other unit, whether that unit encloses the project or
// reaches the document through `../`; and a document outside every project
// is admitted by no unit at all, counted and degraded. Two units admitting one
// path publish its nodes, relations and aliases twice, under two scopes:
// duplicated facts that no error reports.
//
// Mutation: make ownerOf answer ownUnit without consulting the recorded
// projects, and the workspace-root unit admits tools/gen/main.go and
// tools/deep/x.go as well; make loadProjects skip the workspace-root project,
// and the nested units admit main.go; answer ownUnit for a path outside the
// unit's root that no project holds, and the sibling units app and web both
// admit shared/x.go.
func TestEveryDocumentIsAdmittedByTheInnermostProjectAlone(t *testing.T) {
	ctx := context.Background()
	work := t.TempDir()
	for _, c := range []struct {
		name  string
		paths []string
		roots []string
		docs  []string
		// owner is the unit that admits each document; a document absent
		// from it is admitted by no unit.
		owner map[string]string
	}{
		{name: "nested", paths: []string{"go.mod", "main.go", "tools/go.mod", "tools/gen/main.go",
			"tools/deep/go.mod", "tools/deep/x.go"},
			roots: []string{"", "tools", "tools/deep"},
			docs:  []string{"main.go", "tools/gen/main.go", "tools/deep/x.go"},
			owner: map[string]string{"main.go": "", "tools/gen/main.go": "tools", "tools/deep/x.go": "tools/deep"}},
		{name: "siblings", paths: []string{"app/go.mod", "app/main.go", "web/go.mod", "shared/x.go"},
			roots: []string{"app", "web"},
			docs:  []string{"app/main.go", "shared/x.go"},
			owner: map[string]string{"app/main.go": "app"}},
	} {
		view := pathsView{paths: c.paths}
		for _, root := range c.roots {
			sc, err := openScratch(ctx, work)
			if err != nil {
				t.Fatalf("openScratch: %v", err)
			}
			im := &importer{sc: sc, profile: &Profile{Kind: KindGo, Root: root}, req: provider.UnitRequest{Content: view}}
			if err := im.openDelta(ctx); err != nil {
				t.Fatalf("openDelta: %v", err)
			}
			if err := im.loadProjects(ctx); err != nil {
				t.Fatalf("loadProjects: %v", err)
			}
			unowned := int64(0)
			for i, p := range c.docs {
				admitted, err := im.seeDocument(ctx, document{index: int64(i), path: p})
				if err != nil {
					t.Fatalf("%s: unit %q, seeDocument(%q): %v", c.name, root, p, err)
				}
				owner, owned := c.owner[p]
				if want := owned && owner == root; admitted != want {
					t.Errorf("%s: unit %q admitted %q = %v, want %v (owner %q, owned %v)",
						c.name, root, p, admitted, want, owner, owned)
				}
				if !owned && (root == "" || !strings.HasPrefix(p, root+"/")) {
					unowned++
				}
			}
			if im.unowned.n != unowned || (unowned > 0) != (im.partialCode == model.CodeProviderOutputInvalid) {
				t.Errorf("%s: unit %q counted %d document(s) in no project with partial code %q, want %d and a degraded capability when any",
					c.name, root, im.unowned.n, im.partialCode, unowned)
			}
			if err := sc.close(); err != nil {
				t.Fatalf("close: %v", err)
			}
		}
	}
}
