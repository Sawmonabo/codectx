package treesitter_test

import (
	"slices"
	"strings"
	"testing"

	"github.com/Sawmonabo/codectx/internal/model"
)

// route is one framework endpoint as published: its metadata and the names
// of the nodes it routes_to.
type route struct {
	framework, method, route, prefix, path string
	handlers                               []string
}

// routes is every framework endpoint the file published, by descriptor
// (the endpoint's qualified name); program entry points carry no framework
// and are left out.
func (c *capture) routes(t *testing.T) map[string][]route {
	t.Helper()
	out := map[string][]route{}
	for _, n := range c.nodes {
		if n.Node.Kind != model.NodeEndpoint {
			continue
		}
		m := metadata(t, n.Node)
		fw, _ := m["framework"].(string)
		if fw == "" {
			continue
		}
		str := func(k string) string { v, _ := m[k].(string); return v }
		out[n.Node.QualifiedName] = append(out[n.Node.QualifiedName], route{framework: fw, method: str("method"),
			route: str("route"), prefix: str("prefix"), path: str("path"), handlers: c.targets(model.RelRoutesTo, n.Node.ID)})
	}
	return out
}

// want checks the file published exactly the routes in want, each once.
func checkRoutes(t *testing.T, c *capture, want map[string]route) {
	t.Helper()
	got := c.routes(t)
	for d, w := range want {
		rs := got[d]
		if len(rs) != 1 {
			t.Errorf("%d endpoints %q, want 1; all: %+v", len(rs), d, got)
			continue
		}
		r := rs[0]
		slices.Sort(w.handlers)
		if r.framework != w.framework || r.method != w.method || r.route != w.route || r.prefix != w.prefix ||
			r.path != w.path || !slices.Equal(r.handlers, w.handlers) {
			t.Errorf("endpoint %q = %+v, want %+v", d, r, w)
		}
	}
	for d := range got {
		if _, ok := want[d]; !ok {
			t.Errorf("unexpected endpoint %q: %+v", d, got[d])
		}
	}
}

// TestFlaskRouteDecorators is a Flask route on an application this file
// builds: one GET endpoint whose path resolves, routed to the decorated
// function. A decorator that is not a route publishes nothing, and a
// blueprint this file never registers leaves its route's prefix unresolved
// with no path. Mutations caught: reading any decorator as a route (an
// endpoint for login_required), joining an unresolved prefix as empty (a
// path on /y), and dropping the default method (no GET).
func TestFlaskRouteDecorators(t *testing.T) {
	p := newProvider(t)
	c := extracted(t, p, "app.py", `from flask import Flask, Blueprint

app = Flask(__name__)
bp = Blueprint("bp", __name__)


def login_required(f):
    return f


@app.route("/x")
def handler():
    return "x"


@login_required
def private():
    return "p"


@bp.route("/y", methods=["POST", "PUT"])
def orphan():
    return "y"
`)
	checkRoutes(t, c, map[string]route{
		"GET /x":  {framework: "flask", method: "GET", route: "/x", prefix: "resolved", path: "/x", handlers: []string{"handler"}},
		"POST /y": {framework: "flask", method: "POST", route: "/y", prefix: "unresolved", handlers: []string{"orphan"}},
		"PUT /y":  {framework: "flask", method: "PUT", route: "/y", prefix: "unresolved", handlers: []string{"orphan"}},
	})
}

// TestFastAPIRouteDecorators is a FastAPI path operation on the application
// (resolved) and one on an APIRouter whose include_router is in another
// file (unresolved, no path, although the router names its own prefix).
// Mutations caught: joining the router's own prefix as if it were mounted
// (a path /items/), and taking the method from anything but the verb.
func TestFastAPIRouteDecorators(t *testing.T) {
	p := newProvider(t)
	c := extracted(t, p, "api.py", `from fastapi import APIRouter, FastAPI

app = FastAPI()
router = APIRouter(prefix="/items")


@app.get("/health")
def health():
    return {}


@router.post("/")
def create():
    return {}
`)
	checkRoutes(t, c, map[string]route{
		"GET /health": {framework: "fastapi", method: "GET", route: "/health", prefix: "resolved", path: "/health", handlers: []string{"health"}},
		"POST /":      {framework: "fastapi", method: "POST", route: "/", prefix: "unresolved", handlers: []string{"create"}},
	})
}

// TestSpringRequestMappings is a Spring controller whose class-level
// @RequestMapping is a literal: each method mapping resolves under it, and a
// @RequestMapping naming two methods is one endpoint each. Mutations caught:
// ignoring the class-level prefix (path /users), and publishing one
// endpoint for a mapping that names two methods.
func TestSpringRequestMappings(t *testing.T) {
	p := newProvider(t)
	c := extracted(t, p, "UserController.java", `package demo;

import org.springframework.web.bind.annotation.*;

@RestController
@RequestMapping("/api")
public class UserController {
    @GetMapping("/users")
    public String list() { return ""; }

    @RequestMapping(value = "/users", method = {RequestMethod.POST, RequestMethod.PUT})
    public String save() { return ""; }

    @Override
    public String toString() { return ""; }
}
`)
	checkRoutes(t, c, map[string]route{
		"GET /users":  {framework: "spring", method: "GET", route: "/users", prefix: "resolved", path: "/api/users", handlers: []string{"list"}},
		"POST /users": {framework: "spring", method: "POST", route: "/users", prefix: "resolved", path: "/api/users", handlers: []string{"save"}},
		"PUT /users":  {framework: "spring", method: "PUT", route: "/users", prefix: "resolved", path: "/api/users", handlers: []string{"save"}},
	})
}

// TestExpressRouterMountedOnTheApplication is an Express route on a router
// this file mounts once on the application with app.use("/api", router):
// its path is the mount's prefix joined to the route. It needs the worker's
// call-argument positions (wire.Site.Arguments) and passes once the worker
// publishes them. Mutations caught: not following the mount (prefix
// unresolved), and taking the first argument after the route as the handler
// rather than the last (routes_to the middleware).
func TestExpressRouterMountedOnTheApplication(t *testing.T) {
	p := newProvider(t)
	c := extracted(t, p, "server.js", `const express = require("express");

const app = express();
const router = express.Router();

function auth(req, res, next) { next(); }
function listUsers(req, res) { res.send([]); }

router.get("/users", auth, listUsers);
app.use("/api", router);
app.listen(3000);
`)
	checkRoutes(t, c, map[string]route{
		"GET /users": {framework: "express", method: "GET", route: "/users", prefix: "resolved", path: "/api/users", handlers: []string{"listUsers"}},
	})
}

// TestGoHandleFuncOnTheDefaultMux is a Go 1.22 pattern registered on the
// default mux, an application root: the pattern's method is the endpoint's
// method and the rest its route and path. It needs the worker's
// call-argument positions (wire.Site.Arguments) and passes once the worker
// publishes them. Mutations caught: keeping the method inside the route
// (route "GET /hello"), and resolving the package-level mux as unresolved.
func TestGoHandleFuncOnTheDefaultMux(t *testing.T) {
	p := newProvider(t)
	c := extracted(t, p, "main.go", `package main

import "net/http"

func hello(w http.ResponseWriter, r *http.Request) {}

func main() {
	http.HandleFunc("GET /hello", hello)
	http.ListenAndServe(":8080", nil)
}
`)
	checkRoutes(t, c, map[string]route{
		"GET /hello": {framework: "net/http", method: "GET", route: "/hello", prefix: "resolved", path: "/hello", handlers: []string{"hello"}},
	})
}

// TestDjangoURLConfIsAlwaysUnresolved is a Django urlconf entry: its route
// as written, no method, and a prefix that is always unresolved because the
// including urlconf is chosen elsewhere; an include() entry is no endpoint.
// It needs the worker's call-argument positions (wire.Site.Arguments) and
// passes once the worker publishes them. Mutations caught: publishing a
// path for a Django route, and reading include() as a route.
func TestDjangoURLConfIsAlwaysUnresolved(t *testing.T) {
	p := newProvider(t)
	c := extracted(t, p, "urls.py", `from django.urls import include, path

from . import views

urlpatterns = [
    path("about/", views.about),
    path("blog/", include("blog.urls")),
]
`)
	got := c.routes(t)
	if len(got) != 1 {
		t.Fatalf("%d Django endpoints, want 1: %+v", len(got), got)
	}
	rs := got["about/"]
	if len(rs) != 1 {
		t.Fatalf("no single endpoint about/: %+v", got)
	}
	r := rs[0]
	if r.framework != "django" || r.method != "" || r.route != "about/" || r.prefix != "unresolved" || r.path != "" {
		t.Errorf("endpoint about/ = %+v, want django, no method, route about/, unresolved, no path", r)
	}
	if len(r.handlers) != 1 || !strings.HasSuffix(r.handlers[0], "about") {
		t.Errorf("endpoint about/ routes_to %v, want the view about", r.handlers)
	}
}
