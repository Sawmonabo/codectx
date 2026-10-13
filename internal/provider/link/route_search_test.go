package link_test

import (
	"slices"
	"testing"

	"github.com/Sawmonabo/codectx/internal/model"
)

// endpointDocuments are the search documents of the endpoints a capture
// holds, each "path body", and false when an endpoint has none or a
// document names no endpoint, located where its endpoint is.
func endpointDocuments(t *testing.T, l linked, c *capture) []string {
	t.Helper()
	paths := l.paths(t)
	byNode := map[model.NodeID]model.SearchUnit{}
	for _, d := range c.search {
		n, ok := c.nodes[d.NodeID]
		if !ok || n.Kind != model.NodeEndpoint || d.Kind != model.NodeEndpoint {
			t.Fatalf("search document %s names %s, which is no endpoint the unit published", d.ID, d.NodeID)
		}
		if n.Range == nil || d.Bytes.Start != n.Range.Start.Byte || d.Bytes.End != n.Range.End.Byte || d.Name != n.Name {
			t.Fatalf("search document of %s at %v named %q, its endpoint at %v named %q", n.Name, d.Bytes, d.Name, n.Range, n.Name)
		}
		byNode[d.NodeID] = d
	}
	var out []string
	for id, n := range c.nodes {
		if n.Kind != model.NodeEndpoint {
			continue
		}
		d, ok := byNode[id]
		if !ok {
			t.Fatalf("endpoint %s of %s carries no search document", n.Name, paths[n.FileID])
		}
		out = append(out, paths[n.FileID]+" "+d.Body)
	}
	slices.Sort(out)
	return out
}

// TestLinkerEndpointsCarryASearchDocument links a unit with each endpoint
// the linker publishes: a route an Express walk across files resolves, a
// package.json `bin` entry and an inherited Java `main`. Each carries the
// search document the structural provider gives an endpoint (its framework,
// method, route and path, else its name), located at the endpoint. The
// inherited main's document is the one the structural provider publishes
// for the same class's main when it decides it, so moving the decision
// between providers moves no document.
// Mutation: the linker publishes no document for its endpoints -> "carries
// no search document".
// Mutation: a search domain of the linker's own -> the two main documents
// differ in identity.
func TestLinkerEndpointsCarryASearchDocument(t *testing.T) {
	l := linkSources(t, newStructural(t), map[string]string{
		"package.json": `{"name": "tool", "bin": {"tool": "./bin/cli.js"}}`,
		"bin/cli.js":   "function run() {}\nrun();\n",
		"app.js": `const express = require("express");
const users = require("./users");
const app = express();
app.use("/users", users);
`,
		"users.js": `const express = require("express");
const router = express.Router();
router.get("/:id", (req, res) => {});
module.exports = router;
`,
	})
	want := []string{"app.js express GET /:id /users/:id", "bin/cli.js bin:tool"}
	if got := endpointDocuments(t, l, l.out); !slices.Equal(got, want) {
		t.Errorf("linker endpoint documents\ngot  %q\nwant %q", got, want)
	}

	base := "package p;\npublic class Base {\n    public static void main(String[] args) {}\n}\n"
	byLinker := linkSources(t, newStructural(t), map[string]string{"p/Base.java": base,
		"p/Flip.java": "package p;\npublic class Flip extends Base {\n    void main() {}\n}\n"})
	if got, want := endpointDocuments(t, byLinker, byLinker.out), []string{"p/Flip.java main"}; !slices.Equal(got, want) {
		t.Fatalf("inherited main documents %q, want %q", got, want)
	}
	byStructural := linkSources(t, newStructural(t), map[string]string{"p/Base.java": base,
		"p/Flip.java": "package p;\npublic class Flip extends Base {\n    public static void main(String[] a) {}\n}\n"})
	flip := byStructural.h.File(t, "p/Flip.java").ID
	var own []string
	for _, d := range byStructural.structural.search {
		if d.Kind == model.NodeEndpoint && d.FileID == flip {
			own = append(own, d.ID)
		}
	}
	var linked []string
	for _, d := range byLinker.out.search {
		linked = append(linked, d.ID)
	}
	if len(own) != 1 || !slices.Equal(own, linked) {
		t.Errorf("Flip's main document is %v when the structural provider decides it and %v when the linker does", own, linked)
	}
}
