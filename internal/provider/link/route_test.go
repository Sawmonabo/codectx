package link_test

import (
	"encoding/json"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/Sawmonabo/codectx/internal/model"
)

// mountedEndpoints is every endpoint the linker published, written
// "<file> <method> <route> <path or unresolved> -> <handlers>" with
// " set=<n>" when its routes_to carry a set size, sorted.
func (l linked) mountedEndpoints(t *testing.T) []string {
	t.Helper()
	paths := l.paths(t)
	var out []string
	for _, n := range l.out.nodes {
		if n.Kind != model.NodeEndpoint || len(n.Metadata) == 0 {
			// A structural endpoint the linker republishes as the owner of
			// a site it binds (a `__main__` guard) carries no metadata.
			continue
		}
		var m model.EndpointMetadata
		if err := json.Unmarshal(n.Metadata, &m); err != nil {
			t.Fatalf("endpoint %s metadata: %v", n.ID, err)
		}
		if err := m.Validate(); err != nil {
			t.Fatalf("endpoint %s metadata: %v", n.ID, err)
		}
		var handlers []string
		set := ""
		for _, r := range l.out.rels {
			if r.Relation.Kind != model.RelRoutesTo || r.Relation.From != n.ID {
				continue
			}
			handlers = append(handlers, l.out.nodes[r.Relation.To].Name)
			for _, e := range r.Evidence {
				if e.SetSize > 0 {
					set = " set=" + strconv.FormatInt(e.SetSize, 10)
				}
			}
		}
		slices.Sort(handlers)
		path := m.Path
		if m.Prefix == model.PrefixUnresolved {
			path = "unresolved"
		}
		if m.Route == "" {
			// A declared entry point carries no route: its name is its
			// descriptor.
			path = n.Name
		}
		out = append(out, strings.Join(strings.Fields(paths[n.FileID]+" "+m.Method+" "+m.Route+" "+path), " ")+" -> "+
			strings.Join(handlers, ",")+set)
	}
	slices.Sort(out)
	return out
}

// checkMounted links files and checks the endpoints the linker published.
func checkMounted(t *testing.T, files map[string]string, want []string) {
	t.Helper()
	l := linkSources(t, newStructural(t), files)
	got := l.mountedEndpoints(t)
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Fatalf("mounted endpoints\ngot  %q\nwant %q", got, want)
	}
}

// TestRouterMountedFromAnotherFile is FastAPI's "Bigger Applications"
// layout: main.py includes `users.router`, a module-level binding of
// routers/users.py. The linker resolves the mount's argument through the
// import to that file's binding and publishes the router's route at its
// full path, the router's own prefix joined, routed to its handler. This is
// the first case the route walk rests on.
// Mutations caught: the mount argument left unresolved across files (no
// endpoint), resolved by the name alone (a second `router` elsewhere would
// mount too), and the router's own prefix dropped (path /{user_id}).
func TestRouterMountedFromAnotherFile(t *testing.T) {
	checkMounted(t, map[string]string{
		"routers/users.py": `from fastapi import APIRouter

router = APIRouter(prefix="/users")


@router.get("/{user_id}")
def read_user(user_id):
    pass
`,
		"other/users.py": `from fastapi import APIRouter

router = APIRouter(prefix="/other")


@router.get("/x")
def other(x):
    pass
`,
		"main.py": `from fastapi import FastAPI
from routers import users

app = FastAPI()
app.include_router(users.router)
`,
	}, []string{"main.py GET /{user_id} /users/{user_id} -> read_user"})
}

// TestFlaskRegistrationPrefixOverrides is a blueprint registered from
// another file with `url_prefix=`, which replaces the blueprint's own
// prefix (Flask's BlueprintSetupState). Mutation caught: the registration's
// prefix joined to the blueprint's own (path /new/old/x).
func TestFlaskRegistrationPrefixOverrides(t *testing.T) {
	checkMounted(t, map[string]string{
		"views.py": `from flask import Blueprint

bp = Blueprint("views", __name__, url_prefix="/old")


@bp.route("/x")
def x():
    return ""
`,
		"app.py": `from flask import Flask

import views

app = Flask(__name__)
app.register_blueprint(views.bp, url_prefix="/new")
`,
	}, []string{"app.py GET /x /new/x -> x"})
}

// TestNestedMountsGiveASet is a router included through another router that
// the application includes twice: every route reaches two paths, each
// published with the set size 2, and a mount whose prefix is not a literal
// leaves its routes unresolved, with no path. Mutations caught: the second
// mount of one router dropped (one path, no set), a nested hop not followed
// (no endpoint), and an unresolved prefix joined as empty (a path /x).
func TestNestedMountsGiveASet(t *testing.T) {
	checkMounted(t, map[string]string{
		"items.py": `from fastapi import APIRouter

router = APIRouter()


@router.get("/")
def list_items():
    return []
`,
		"api.py": `from fastapi import APIRouter

import items

api_router = APIRouter()
api_router.include_router(items.router, prefix="/items")
`,
		"main.py": `import os

from fastapi import FastAPI

import api

PREFIX = os.environ["PREFIX"]

app = FastAPI()
app.include_router(api.api_router, prefix="/v1")
app.include_router(api.api_router, prefix="/v2")
app.include_router(api.api_router, prefix=PREFIX)
`,
	}, []string{
		"main.py GET / /v1/items/ -> list_items set=3",
		"main.py GET / /v2/items/ -> list_items set=3",
		"main.py GET / unresolved -> list_items set=3",
	})
}

// TestMountCycleEnds is two routers that include each other, one of them
// included by the application: the walk ends where a binding repeats, so
// each route is published once per path it reaches without repeating.
// Mutation caught: no guard on the walk's way (it never ends).
func TestMountCycleEnds(t *testing.T) {
	checkMounted(t, map[string]string{
		"a.py": `from fastapi import APIRouter

import b

ra = APIRouter(prefix="/a")
ra.include_router(b.rb)


@ra.get("/x")
def ax():
    pass
`,
		"b.py": `from fastapi import APIRouter

import a

rb = APIRouter(prefix="/b")
rb.include_router(a.ra)


@rb.get("/y")
def by():
    pass
`,
		"main.py": `from fastapi import FastAPI

import a

app = FastAPI()
app.include_router(a.ra)
`,
	}, []string{
		"main.py GET /x /a/x -> ax",
		"main.py GET /y /a/b/y -> by",
	})
}

// TestDjangoSettingsChain is Django's chain read from the project's own
// source: manage.py names the settings module, the settings name the root
// urlconf, which includes another app's urlconf by its dotted name; the
// view is imported from the app's views module. Mutations caught: the
// chain not followed (no endpoint), the include's prefix dropped (path
// /post/<int:pk>/), a settings module no file names walked anyway (the
// second settings module's ROOT_URLCONF would publish /stale/), and the
// imported view left without its handler.
func TestDjangoSettingsChain(t *testing.T) {
	checkMounted(t, map[string]string{
		"manage.py": `import os
import sys


def main():
    os.environ.setdefault("DJANGO_SETTINGS_MODULE", "mysite.settings")


if __name__ == "__main__":
    main()
`,
		"mysite/settings.py": `ROOT_URLCONF = "mysite.urls"
`,
		"mysite/unused_settings.py": `ROOT_URLCONF = "mysite.stale_urls"
`,
		"mysite/urls.py": `from django.urls import include, path

urlpatterns = [
    path("blog/", include("blog.urls")),
]
`,
		"mysite/stale_urls.py": `from django.urls import path

from blog.views import detail

urlpatterns = [
    path("stale/", detail),
]
`,
		"blog/urls.py": `from django.urls import path

from .views import detail

urlpatterns = [
    path("post/<int:pk>/", detail),
]
`,
		"blog/views.py": `def detail(request, pk):
    pass
`,
	}, []string{"mysite/settings.py post/<int:pk>/ /blog/post/<int:pk>/ -> detail"})
}

// TestASGIMountsAcrossFiles is Starlette's Mount and FastAPI's `mount`
// across files: a FastAPI application mounted under a path with
// `app.mount`, a Starlette route list another module binds mounted with
// `Mount(path, routes=...)`, and a Starlette application mounted with
// `Mount(path, app=...)`; each route below reaches its full path, and an
// application the sub-application itself mounts from a further file is
// walked through it. A router `include_router` names cannot be an
// application. Mutations caught: the ASGI frameworks not one family (the
// Starlette application under the FastAPI one not mounted, no /deep path),
// application roots not
// exported (no /sub path), the root's own mounts left out of its payload
// (no /sub/deep path), the route list not an object (no /users path), and
// include_router allowed to mount an application (a /wrong path).
func TestASGIMountsAcrossFiles(t *testing.T) {
	checkMounted(t, map[string]string{
		"sub.py": `from fastapi import FastAPI

import deep

app = FastAPI()
app.mount("/deep", deep.app)


@app.get("/x")
def sub_x():
    return {}
`,
		"deep.py": `from starlette.applications import Starlette
from starlette.routing import Route


def deep_y(request):
    return None


app = Starlette(routes=[Route("/y", deep_y)])
`,
		"users.py": `from starlette.routing import Route


def users_list(request):
    return None


routes = [Route("/", users_list)]
`,
		"main.py": `from fastapi import FastAPI
from starlette.routing import Mount

import sub
import users

app = FastAPI(routes=[Mount("/users", routes=users.routes)])
app.mount("/sub", sub.app)
app.include_router(sub.app, prefix="/wrong")
`,
	}, []string{
		"main.py GET / /users/ -> users_list",
		"main.py GET /x /sub/x -> sub_x",
		"main.py GET /y /sub/deep/y -> deep_y",
		"sub.py GET /y /deep/y -> deep_y",
	})
}
