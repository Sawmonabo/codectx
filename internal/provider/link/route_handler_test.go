package link_test

import (
	"slices"
	"testing"
)

// TestHandlerReadsNamedAcrossFiles is a handler whose own file no route
// names: a Django view module urls.py names, an Express controller module a
// router names, and a Starlette endpoint module an application names. Each
// handler's request reads publish under the framework of the route that
// names it, and a function no route names, or one a route of another
// framework names, publishes nothing. Mutations: "reads never recorded"
// (handlerCandidates records nothing) loses every read; "handler index
// unread" (isNamed finds no entry) loses the Python reads; "any framework"
// (the route's framework not compared) adds show's headers read, which
// Starlette's table reads too, under Starlette;
// "import not resolved" (isNamed accepts any entry of the name) adds the
// shadow module's reads; "Express names unread" (jsHandlerInputs finds no
// entry) loses the Express reads.
func TestHandlerReadsNamedAcrossFiles(t *testing.T) {
	l := linkSources(t, newStructural(t), map[string]string{
		"site/urls.py": `from django.urls import path

import views

urlpatterns = [path("show/<int:pk>/", views.show)]
`,
		"site/views.py": `def show(request, pk):
    q = request.GET.get("q")
    k = request.headers["X-Key"]
    return q, k


def helper(request):
    return request.COOKIES["sid"]
`,
		"other/views.py": `def show(request, pk):
    return request.GET["other"]
`,
		"api/app.py": `from starlette.applications import Starlette
from starlette.routing import Route

from api import endpoints

app = Starlette(routes=[Route("/items", endpoints.items)])
`,
		"api/endpoints.py": `def items(request):
    return request.query_params["page"]
`,
	})
	got := l.linkedInputs(t)
	want := []string{
		"api/endpoints.py items starlette query page | request.query_params[\"page\"]",
		"site/views.py show django header X-Key | request.headers[\"X-Key\"]",
		"site/views.py show django query q | request.GET.get(\"q\")",
	}
	if !slices.Equal(got, want) {
		t.Fatalf("linked request inputs\ngot  %q\nwant %q", got, want)
	}
	l = linkSources(t, newStructural(t), map[string]string{
		"package.json": `{"name":"app"}`,
		"routes.js": `const express = require("express");
const ctrl = require("./ctrl");
const router = express.Router();
router.get("/users", ctrl.list);
module.exports = router;
`,
		"ctrl.js": `function list(req, res) {
  res.send(req.query.page);
}
function unused(req, res) {
  res.send(req.query.other);
}
module.exports = { list, unused };
`,
	})
	got = l.linkedInputs(t)
	want = []string{"ctrl.js list express query page | req.query.page"}
	if !slices.Equal(got, want) {
		t.Fatalf("linked Express request inputs\ngot  %q\nwant %q", got, want)
	}
}
