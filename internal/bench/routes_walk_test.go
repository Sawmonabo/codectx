package bench

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/Sawmonabo/codectx/internal/model"
)

// TestEndpointsAreSeedsOfACalleeWalk indexes a repository whose one module
// declares two Flask routes and no `__main__` guard, through the product's
// own composition. The endpoint kind filter returns exactly the two routes,
// and a walk outward from both of them over routes_to and calls reaches
// every function either handler calls. The relations are passed explicitly:
// whether routes_to belongs to the default relation set is decided
// elsewhere. Mutations caught: publishing no routes_to (the walk reaches no
// handler), publishing an endpoint for the module (a third hit), and a kind
// filter that lets other kinds through.
func TestEndpointsAreSeedsOfACalleeWalk(t *testing.T) {
	if testing.Short() {
		t.Skip("indexes a repository through the product composition; run without -short")
	}
	ctx := context.Background()
	repo := t.TempDir()
	src := `from flask import Flask

app = Flask(__name__)


def load(name):
    return name.strip()


def render(value):
    return value


def audit(value):
    return value


@app.route("/users")
def users():
    return render(load("u"))


@app.route("/items", methods=["POST"])
def items():
    return audit(render("i"))
`
	if err := os.WriteFile(filepath.Join(repo, "app.py"), []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}
	svc := openCorpus(t, ctx, repo, nil)

	page, err := svc.Search(ctx, model.SearchRequest{Query: "flask", Kinds: []model.NodeKind{model.NodeEndpoint}})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	var names []string
	var seeds []model.NodeID
	for _, h := range page.Items {
		if h.Kind != model.NodeEndpoint {
			t.Errorf("the endpoint filter returned a %s hit %q", h.Kind, h.Name)
		}
		names = append(names, h.Name)
		seeds = append(seeds, h.NodeID)
	}
	slices.Sort(names)
	if want := []string{"GET /users", "POST /items"}; !slices.Equal(names, want) {
		t.Fatalf("endpoint hits = %v, want %v", names, want)
	}

	res, err := svc.Graph(ctx, model.GraphRequest{Start: seeds, Relations: []model.RelationKind{model.RelRoutesTo, model.RelCalls},
		Direction: model.DirectionOutgoing, MaxDepth: 4})
	if err != nil {
		t.Fatalf("graph: %v", err)
	}
	reached := map[string]bool{}
	for _, n := range res.Nodes {
		if n.Kind == model.NodeFunction {
			reached[n.Name] = true
		}
	}
	for _, fn := range []string{"users", "items", "load", "render", "audit"} {
		if !reached[fn] {
			t.Errorf("the walk from both endpoints did not reach %s; reached %v", fn, reached)
		}
	}
}
