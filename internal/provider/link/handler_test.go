package link_test

import (
	"testing"

	"github.com/Sawmonabo/codectx/internal/model"
)

// TestCallsInAnInlineHandlerAreLinkedFromIt registers a route whose handler
// is an arrow function written in place, calling a function another file
// exports. The structural provider publishes the handler as a function node
// of its own and routes the endpoint to it; the linker's `calls` for the
// site in its body must leave from that node too, so a walk endpoint ->
// handler -> callee reaches the bound callee, and the module that registers
// the route calls nothing of it.
//
// Mutation: publish the site's calls from its enclosing declaration
// (Site.Caller, the module here) -> the module calls helper and the handler
// calls nothing.
func TestCallsInAnInlineHandlerAreLinkedFromIt(t *testing.T) {
	structural := newStructural(t)
	l := linkSources(t, structural, map[string]string{
		"util.js": "export function helper() { return 1; }\n",
		"routes.js": `import { helper } from "./util.js";
const express = require("express");
const app = express();

app.get("/inline", (req, res) => { helper(); });
`,
	})
	var fromHandler, fromModule bool
	for _, r := range l.out.rels {
		if r.Relation.Kind != model.RelCalls || l.out.nodes[r.Relation.To].Name != "helper" {
			continue
		}
		switch from := l.out.nodes[r.Relation.From]; from.Kind {
		case model.NodeFunction:
			fromHandler = fromHandler || from.Name == "GET /inline"
		case model.NodeModule, model.NodeFile:
			fromModule = true
		}
	}
	if !fromHandler {
		t.Error("the inline handler's call of helper is not linked from the handler's node")
	}
	if fromModule {
		t.Error("the module that registers the route is linked as calling helper; the handler calls it")
	}
}
