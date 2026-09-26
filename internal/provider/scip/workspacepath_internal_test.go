package scip

import (
	"context"
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

// pathsView is a snapshot view that lists its paths and nothing else.
type pathsView struct {
	model.SnapshotView
	paths []string
}

func (v pathsView) EachFile(_ context.Context, _ model.FileSelection, fn func(model.FileVersion) error) error {
	for _, p := range v.paths {
		if err := fn(model.FileVersion{Path: p, Status: model.FileTracked}); err != nil {
			return err
		}
	}
	return nil
}

// TestEveryDocumentIsAdmittedByTheInnermostProjectAlone protects the
// partition of documents between the units of nested projects of one kind:
// each document is admitted by the unit of the innermost project holding it
// and refused by every other unit, whether that unit encloses the project or
// reaches the document through `../`. Two units admitting one path publish
// its nodes, relations and aliases twice, under two scopes: duplicated facts
// that no error reports. Mutation: make inOtherProject return false without
// consulting the recorded projects, and the workspace-root unit admits
// tools/gen/main.go and tools/deep/x.go as well; or make loadProjects skip
// the workspace-root project, and the nested units admit main.go.
func TestEveryDocumentIsAdmittedByTheInnermostProjectAlone(t *testing.T) {
	ctx := context.Background()
	view := pathsView{paths: []string{"go.mod", "main.go", "tools/go.mod", "tools/gen/main.go",
		"tools/deep/go.mod", "tools/deep/x.go"}}
	owner := map[string]string{"main.go": "", "tools/gen/main.go": "tools", "tools/deep/x.go": "tools/deep"}
	work := t.TempDir()
	for _, root := range []string{"", "tools", "tools/deep"} {
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
		for i, p := range []string{"main.go", "tools/gen/main.go", "tools/deep/x.go"} {
			admitted, err := im.seeDocument(ctx, document{index: int64(i), path: p})
			if err != nil {
				t.Fatalf("unit %q, seeDocument(%q): %v", root, p, err)
			}
			if want := owner[p] == root; admitted != want {
				t.Errorf("unit %q admitted %q = %v, want %v: %q is owned by the unit %q alone",
					root, p, admitted, want, p, owner[p])
			}
		}
		if err := sc.close(); err != nil {
			t.Fatalf("close: %v", err)
		}
	}
}
