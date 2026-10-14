package bench

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/Sawmonabo/codectx/internal/model"
)

// TestAResolvedCallIsOneCallee walks the generated corpus's fan-out function,
// whose 40 calls each name a function of another package the linker binds.
// The structural provider also names each of them by a file-less call
// placeholder; served beside the bound target, every call would be two
// callees and two impact entries. Callees answer exactly the 40 bound
// functions, each located, and an impact walk one hop out admits no file-less
// callee. Mutation caught: the packed adjacency keeping a placeholder edge the
// linker's edge covers (80 callees, 40 of them file-less).
func TestAResolvedCallIsOneCallee(t *testing.T) {
	if testing.Short() {
		t.Skip("indexes the generated corpus through the product composition; run without -short")
	}
	f := fixture(t)
	ctx := context.Background()
	fanOut := f.seeds[0]
	callees := map[model.NodeID]model.Node{}
	var relations int
	req := model.GraphRequest{Start: []model.NodeID{fanOut}, Relations: []model.RelationKind{model.RelCalls},
		Direction: model.DirectionOutgoing, MaxDepth: 1}
	for {
		res, err := f.svc.Graph(ctx, req)
		if err != nil {
			t.Fatalf("callees: %v", err)
		}
		relations += len(res.Relations)
		for _, n := range res.Nodes {
			if n.ID != fanOut {
				callees[n.ID] = n
			}
		}
		if req.Page.Cursor = res.Meta.NextCursor; req.Page.Cursor == "" {
			break
		}
	}
	if relations != 40 || len(callees) != 40 {
		t.Errorf("callees of the fan-out function: %d relations to %d nodes, want 40 and 40", relations, len(callees))
	}
	for _, n := range callees {
		if n.FileID == "" || !strings.HasPrefix(n.Name, corpusPrefix+"GoRun") {
			t.Errorf("callee %q (%s) is not a located corpus function", n.Name, n.Kind)
		}
	}

	impact := model.ImpactRequest{Start: []model.NodeID{fanOut}, Relations: []model.RelationKind{model.RelCalls},
		Direction: model.DirectionOutgoing, MaxDepth: 1}
	entries := 0
	for {
		res, err := f.svc.Impact(ctx, impact)
		if err != nil {
			t.Fatalf("impact: %v", err)
		}
		for _, e := range res.Entries {
			entries++
			if e.FileID == "" {
				t.Errorf("impact admitted the file-less callee %q (%s)", e.Name, e.Kind)
			}
		}
		if impact.Page.Cursor = res.Meta.NextCursor; impact.Page.Cursor == "" {
			break
		}
	}
	if entries != 40 {
		t.Errorf("impact one hop out of the fan-out function: %d entries, want 40", entries)
	}
}

// TestAnUnresolvedCallKeepsItsPlaceholder indexes two Python modules: run
// calls a function and a class of the other module, a method of that class,
// and a function of its own module around a call of a standard-library module
// no member file is. The linker binds the first four; the last is a proven
// external call it publishes no edge for, so its placeholder is the only
// fact of that call and stays. Mutations caught: dropping every placeholder
// edge (getcwd disappears); matching sites by overlap instead of equal bytes
// (the bound outer(...) call's range contains the getcwd call's, so getcwd
// disappears); keeping covered placeholders (helper, Box and load appear
// twice).
func TestAnUnresolvedCallKeepsItsPlaceholder(t *testing.T) {
	if testing.Short() {
		t.Skip("indexes a repository through the product composition; run without -short")
	}
	ctx := context.Background()
	repo := t.TempDir()
	files := map[string]string{
		"a.py": `import os
from b import helper, Box


def outer(value):
    return value


def run():
    helper()
    Box().load()
    return outer(os.getcwd())
`,
		"b.py": `def helper():
    return 1


class Box:
    def load(self):
        return 2
`,
	}
	for name, src := range files {
		if err := os.WriteFile(filepath.Join(repo, name), []byte(src), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	svc := openCorpus(t, ctx, repo, nil)
	page, err := svc.Search(ctx, model.SearchRequest{Query: "run", Kinds: []model.NodeKind{model.NodeFunction}})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	var run model.NodeID
	for _, h := range page.Items {
		if h.Name == "run" {
			run = h.NodeID
		}
	}
	if run == "" {
		t.Fatal("search found no function run")
	}
	res, err := svc.Graph(ctx, model.GraphRequest{Start: []model.NodeID{run}, Relations: []model.RelationKind{model.RelCalls},
		Direction: model.DirectionOutgoing, MaxDepth: 1})
	if err != nil {
		t.Fatalf("callees: %v", err)
	}
	var got []string
	for _, n := range res.Nodes {
		if n.ID == run {
			continue
		}
		where := "located"
		if n.FileID == "" {
			where = "file-less"
		}
		got = append(got, n.Name+" "+where)
	}
	slices.Sort(got)
	want := []string{"Box located", "getcwd file-less", "helper located", "load located", "outer located"}
	if !slices.Equal(got, want) || len(res.Relations) != len(want) {
		t.Errorf("callees of run = %v over %d relations, want %v, one relation each", got, len(res.Relations), want)
	}
}
