package treesitter_test

import (
	"slices"
	"testing"

	"github.com/Sawmonabo/codectx/internal/model"
)

// TestFlaskAddURLRule is Flask's call form, the only way to register a
// class-based view. A plain view takes GET; a `View.as_view(...)` view takes
// its class's own methods, which the call does not write, so it names no
// method; `methods=` names them; the view at position 2 is the handler.
// Mutations caught: add_url_rule not read (no endpoints), an as_view route
// given GET, the view read from position 1 (the endpoint name) so `save`
// routes nowhere, and the view's `.as_view(...)` call kept as the handler.
func TestFlaskAddURLRule(t *testing.T) {
	p := newProvider(t)
	c := extracted(t, p, "app.py", `from flask import Flask
from flask.views import MethodView

app = Flask(__name__)


class UserAPI(MethodView):
    def get(self):
        return ""


def index():
    return ""


def save():
    return ""


app.add_url_rule("/", view_func=index)
app.add_url_rule("/users/", view_func=UserAPI.as_view("users"))
app.add_url_rule("/save", "save", save, methods=["POST"])
`)
	checkRoutes(t, c, map[string]route{
		"GET /":      {framework: "flask", method: "GET", route: "/", prefix: "resolved", path: "/", handlers: []string{"index"}},
		"/users/":    {framework: "flask", route: "/users/", prefix: "resolved", path: "/users/", handlers: []string{"UserAPI"}},
		"POST /save": {framework: "flask", method: "POST", route: "/save", prefix: "resolved", path: "/save", handlers: []string{"save"}},
	})
}

// TestFastAPIWebSocketAndCallRoutes is FastAPI's WebSocket decorator beside
// an HTTP GET on the same path, and the call forms add_api_route (GET when
// it passes no methods) and add_api_websocket_route. Mutations caught: the
// websocket verb not read, a WebSocket route spelled GET (it collides with
// the HTTP route on /ws), add_api_route's default method dropped, and its
// `methods=` ignored.
func TestFastAPIWebSocketAndCallRoutes(t *testing.T) {
	p := newProvider(t)
	c := extracted(t, p, "main.py", `from fastapi import FastAPI

app = FastAPI()


@app.websocket("/ws")
async def ws(websocket):
    pass


@app.get("/ws")
def page():
    return ""


def items():
    return []


def change():
    return []


def feed(websocket):
    pass


app.add_api_route("/items", items)
app.add_api_route("/items", change, methods=["POST", "PUT"])
app.add_api_websocket_route("/feed", feed)
`)
	checkRoutes(t, c, map[string]route{
		"WEBSOCKET /ws":   {framework: "fastapi", method: "WEBSOCKET", route: "/ws", prefix: "resolved", path: "/ws", handlers: []string{"ws"}},
		"GET /ws":         {framework: "fastapi", method: "GET", route: "/ws", prefix: "resolved", path: "/ws", handlers: []string{"page"}},
		"GET /items":      {framework: "fastapi", method: "GET", route: "/items", prefix: "resolved", path: "/items", handlers: []string{"items"}},
		"POST /items":     {framework: "fastapi", method: "POST", route: "/items", prefix: "resolved", path: "/items", handlers: []string{"change"}},
		"PUT /items":      {framework: "fastapi", method: "PUT", route: "/items", prefix: "resolved", path: "/items", handlers: []string{"change"}},
		"WEBSOCKET /feed": {framework: "fastapi", method: "WEBSOCKET", route: "/feed", prefix: "resolved", path: "/feed", handlers: []string{"feed"}},
	})
}

// TestStarletteRouteLists is a Starlette application whose routes are a
// module-level list holding a Route to a function (GET), a Route to a class
// (an ASGI application, every method), a WebSocketRoute and a Mount whose
// own list is another binding. Mutations caught: a list held by a binding
// not followed (every prefix unresolved), the Mount's path not joined (path
// /{username}), a class endpoint given GET, and `methods=` ignored.
func TestStarletteRouteLists(t *testing.T) {
	p := newProvider(t)
	c := extracted(t, p, "app.py", `from starlette.applications import Starlette
from starlette.routing import Mount, Route, WebSocketRoute


async def homepage(request):
    pass


async def user(request):
    pass


async def chat(websocket):
    pass


class Health:
    pass


users = [
    Route("/{username}", user, methods=["GET", "DELETE"]),
]

routes = [
    Route("/", homepage),
    Route("/health", Health),
    WebSocketRoute("/ws", chat),
    Mount("/users", routes=users),
]

app = Starlette(routes=routes)
`)
	checkRoutes(t, c, map[string]route{
		"GET /":              {framework: "starlette", method: "GET", route: "/", prefix: "resolved", path: "/", handlers: []string{"homepage"}},
		"/health":            {framework: "starlette", route: "/health", prefix: "resolved", path: "/health", handlers: []string{"Health"}},
		"WEBSOCKET /ws":      {framework: "starlette", method: "WEBSOCKET", route: "/ws", prefix: "resolved", path: "/ws", handlers: []string{"chat"}},
		"GET /{username}":    {framework: "starlette", method: "GET", route: "/{username}", prefix: "resolved", path: "/users/{username}", handlers: []string{"user"}},
		"DELETE /{username}": {framework: "starlette", method: "DELETE", route: "/{username}", prefix: "resolved", path: "/users/{username}", handlers: []string{"user"}},
	})
}

// TestExpressArrayPaths is an Express route whose path is an array: one
// endpoint per element. Mutation caught: the array read as one route
// written `["/users", "/people"]` with no path.
func TestExpressArrayPaths(t *testing.T) {
	p := newProvider(t)
	c := extracted(t, p, "server.js", `const express = require("express");

const app = express();

function list(req, res) { res.send([]); }

app.get(["/users", "/people"], list);
`)
	checkRoutes(t, c, map[string]route{
		"GET /users":  {framework: "express", method: "GET", route: "/users", prefix: "resolved", path: "/users", handlers: []string{"list"}},
		"GET /people": {framework: "express", method: "GET", route: "/people", prefix: "resolved", path: "/people", handlers: []string{"list"}},
	})
}

// TestChiRouteAndGroupClosures is chi's closure forms: Route(p, fn) gives
// fn's router the parent's prefix plus p, nested Routes compose, and
// Group(fn) gives its router exactly the parent's prefix. Mutations caught:
// the closure's parameter read as an unknown router (every prefix
// unresolved), a nested Route's pattern dropped (path /articles/show), and
// a Group given a prefix of its own.
func TestChiRouteAndGroupClosures(t *testing.T) {
	p := newProvider(t)
	c := extracted(t, p, "main.go", `package main

import (
	"net/http"

	"github.com/go-chi/chi/v5"
)

func list(w http.ResponseWriter, r *http.Request)  {}
func show(w http.ResponseWriter, r *http.Request)  {}
func admin(w http.ResponseWriter, r *http.Request) {}

func main() {
	r := chi.NewRouter()
	r.Route("/articles", func(r chi.Router) {
		r.Get("/", list)
		r.Route("/{id}", func(r chi.Router) {
			r.Get("/show", show)
		})
	})
	r.Group(func(g chi.Router) {
		g.Get("/admin", admin)
	})
	http.ListenAndServe(":3000", r)
}
`)
	checkRoutes(t, c, map[string]route{
		"GET /":      {framework: "chi", method: "GET", route: "/", prefix: "resolved", path: "/articles/", handlers: []string{"list"}},
		"GET /show":  {framework: "chi", method: "GET", route: "/show", prefix: "resolved", path: "/articles/{id}/show", handlers: []string{"show"}},
		"GET /admin": {framework: "chi", method: "GET", route: "/admin", prefix: "resolved", path: "/admin", handlers: []string{"admin"}},
	})
}

// TestGorillaMatcherChains is gorilla/mux routes built by matcher chains
// closed by their handler: PathPrefix(p).HandlerFunc(h), and
// Methods(m).Path(p).HandlerFunc(h). Mutations caught: the chains not read
// (no endpoints), Methods dropped (no method on /items), and the route read
// from the wrong matcher.
func TestGorillaMatcherChains(t *testing.T) {
	p := newProvider(t)
	c := extracted(t, p, "main.go", `package main

import (
	"net/http"

	"github.com/gorilla/mux"
)

func files(w http.ResponseWriter, r *http.Request)  {}
func create(w http.ResponseWriter, r *http.Request) {}

func main() {
	r := mux.NewRouter()
	r.PathPrefix("/static/").HandlerFunc(files)
	r.Methods("POST").Path("/items").HandlerFunc(create)
	http.ListenAndServe(":8080", r)
}
`)
	checkRoutes(t, c, map[string]route{
		"/static/":    {framework: "gorilla/mux", route: "/static/", prefix: "resolved", path: "/static/", handlers: []string{"files"}},
		"POST /items": {framework: "gorilla/mux", method: "POST", route: "/items", prefix: "resolved", path: "/items", handlers: []string{"create"}},
	})
}

// applications is, for every `application` endpoint of the file, the names
// of the declarations it routes_to, sorted.
func (c *capture) applications() []string {
	var out []string
	for _, n := range c.endpoints() {
		if n.QualifiedName == "application" {
			out = append(out, c.targets(model.RelRoutesTo, n.ID)...)
		}
	}
	slices.Sort(out)
	return out
}

// TestApplicationObjectsAreEndpoints is the application objects a module
// binds, found by content whatever the file's name: a Flask and an Express
// application and a Django WSGI application each are an `application`
// endpoint routed to their binding, and one built inside a function is not.
// Mutations caught: an application found by its file's name (none here is
// named app.py, wsgi.py or index.js), an application endpoint for the
// function-local object, and one routed to the constructor call's callee
// rather than the binding.
func TestApplicationObjectsAreEndpoints(t *testing.T) {
	p := newProvider(t)
	cases := []struct {
		file, src string
		want      []string
	}{
		{"service.py", `from flask import Flask

server = Flask(__name__)


def make():
    inner = Flask("inner")
    return inner
`, []string{"server"}},
		{"deploy.py", `import os

from django.core.wsgi import get_wsgi_application

os.environ.setdefault("DJANGO_SETTINGS_MODULE", "site.settings")
application = get_wsgi_application()
`, []string{"application"}},
		{"start.js", `const express = require("express");

const web = express();
`, []string{"web"}},
	}
	for _, tc := range cases {
		t.Run(tc.file, func(t *testing.T) {
			c := extracted(t, p, tc.file, tc.src)
			if got := c.applications(); !slices.Equal(got, tc.want) {
				t.Errorf("application endpoints route to %v, want %v", got, tc.want)
			}
		})
	}
}
