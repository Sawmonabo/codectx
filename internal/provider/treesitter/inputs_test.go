package treesitter_test

import (
	"slices"
	"testing"

	"github.com/Sawmonabo/codectx/internal/model"
)

// inputs is every request-input read the file published, written
// "<owner> <framework> <source> <key> | <expression>", and every flow from
// one, written "<expression> -> <variable>", sorted.
func (c *capture) inputs(t *testing.T) []string {
	t.Helper()
	var out []string
	for _, n := range c.nodes {
		m := metadata(t, n.Node)
		in, ok := m[model.MetaInput].(map[string]any)
		if !ok {
			continue
		}
		if n.Node.Kind != model.NodeVariable {
			t.Errorf("request input %q is a %s node, want variable", n.Node.Name, n.Node.Kind)
		}
		str := func(k string) string { v, _ := in[k].(string); return v }
		owners := 0
		for _, r := range c.rels {
			if r.Relation.To != n.Node.ID {
				continue
			}
			from, _ := c.byID(r.Relation.From)
			switch r.Relation.Kind {
			case model.RelReads:
				owners++
				out = append(out, from.Name+" "+str("framework")+" "+str("source")+" "+str("key")+" | "+n.Node.Name)
			}
		}
		if owners != 1 {
			t.Errorf("request input %q is read by %d functions, want 1", n.Node.Name, owners)
		}
		for _, to := range c.targets(model.RelDataFlowsTo, n.Node.ID) {
			out = append(out, n.Node.Name+" -> "+to)
		}
	}
	slices.Sort(out)
	return out
}

// checkInputs extracts one file and checks its request-input reads.
func checkInputs(t *testing.T, file, src string, want []string) {
	t.Helper()
	c := extracted(t, newProvider(t), file, src)
	got := c.inputs(t)
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Fatalf("request inputs\ngot  %q\nwant %q", got, want)
	}
}

// TestFlaskRequestReads is Flask's `request` read anywhere in a file that
// imports it: each modelled member with its literal key, read from the
// function it is in, and the read assigned to a variable flowing to it.
// Mutations caught: the import gate dropped (the second file's `request`
// would publish), comments and strings not skipped (`request.cookies` in
// the docstring), an unmodelled member published (`request.method`), the
// subscript or getter key lost, a call-only member read uncalled, and the
// def-use hook dropped (no flow to uid).
func TestFlaskRequestReads(t *testing.T) {
	checkInputs(t, "app.py", `from flask import Flask, request

app = Flask(__name__)


@app.route("/u")
def user():
    """Reads request.cookies nowhere."""
    # request.headers["X"] is a comment.
    uid = request.args.get("uid")
    name = request.form["name"]
    payload = request.get_json()
    if request.method == "POST":
        return uid
    return name, payload
`, []string{
		"user flask body  | request.get_json()",
		"user flask form name | request.form[\"name\"]",
		"user flask query uid | request.args.get(\"uid\")",
		"request.args.get(\"uid\") -> uid",
		"request.form[\"name\"] -> name",
		"request.get_json() -> payload",
	})
	checkInputs(t, "other.py", `def user(request):
    return request.args.get("uid")
`, nil)
}

// TestFastAPIHandlerParameters is FastAPI's rule for a handler's
// parameters: a name the route writes is a path read, a scalar a query
// read, a model this file declares a body read, a marker names its own
// source (a Header's underscores read as hyphens, an alias as the key), a
// dependency reads nothing, a Request parameter is the request object, and
// a type this file does not declare names no source. Mutations caught: the
// route's names not consulted (item_id a query), the model read as a query,
// the header key left with underscores, the alias ignored, Depends
// published, the Request's members not read, and an imported type guessed.
func TestFastAPIHandlerParameters(t *testing.T) {
	checkInputs(t, "main.py", `from typing import Annotated

from fastapi import Depends, FastAPI, Header, Query, Request
from pydantic import BaseModel

from .schemas import Other

app = FastAPI()


class Item(BaseModel):
    name: str


def db():
    return None


@app.put("/items/{item_id}")
async def update(
    item_id: int,
    item: Item,
    q: str | None = None,
    user_agent: Annotated[str | None, Header()] = None,
    tag: str = Query(None, alias="item-tag"),
    session=Depends(db),
    other: Other = None,
    request: Request = None,
):
    return request.headers["x-trace"]
`, []string{
		"update fastapi body item | item: Item",
		"update fastapi header user-agent | user_agent: Annotated[str | None, Header()] = None",
		"update fastapi header x-trace | request.headers[\"x-trace\"]",
		"update fastapi path item_id | item_id: int",
		"update fastapi query item-tag | tag: str = Query(None, alias=\"item-tag\")",
		"update fastapi query q | q: str | None = None",
	})
}

// TestExpressHandlerRequestReads is the first parameter of each Express
// handler, whatever it is named, read in that handler's body only, and
// owned by the handler literal it is in. Mutations caught: the receiver
// taken as the literal name req (the second handler's `request` reads
// nothing), the property key lost, `get`'s argument not read as the key,
// and the read owned by the module instead of the handler.
func TestExpressHandlerRequestReads(t *testing.T) {
	checkInputs(t, "app.js", `const express = require("express");
const app = express();

app.get("/a/:id", (req, res) => {
  res.send(req.params.id + req.get("Host"));
});

app.post("/b", function (request, res) {
  const body = request.body;
  res.send(body);
});

function outside(req) {
  return req.query.x;
}
`, []string{
		"GET /a/:id express header Host | req.get(\"Host\")",
		"GET /a/:id express path id | req.params.id",
		"POST /b express body  | request.body",
		"request.body -> body",
	})
}

// TestGoRequestReads is every *http.Request parameter of a Go file that
// imports net/http, read in its function's body, with gorilla/mux's Vars
// as a path read. Mutations caught: a function type's parameter read into
// the next declaration's body (the Handler type below), `r.Body.Close()`
// read as a body read, the URL's Query chain not followed to its key, and
// mux.Vars not read.
func TestGoRequestReads(t *testing.T) {
	checkInputs(t, "main.go", `package main

import (
	"io"
	"net/http"

	"github.com/gorilla/mux"
)

type Handler func(w http.ResponseWriter, r *http.Request)

func show(w http.ResponseWriter, r *http.Request) {
	defer r.Body.Close()
	id := mux.Vars(r)["id"]
	q := r.URL.Query().Get("q")
	data, _ := io.ReadAll(r.Body)
	w.Write([]byte(id + q + string(data) + r.Header.Get("X-Key")))
}
`, []string{
		"show gorilla/mux path id | mux.Vars(r)[\"id\"]",
		"show net/http body  | r.Body",
		"show net/http header X-Key | r.Header.Get(\"X-Key\")",
		"show net/http query q | r.URL.Query().Get(\"q\")",
		"mux.Vars(r)[\"id\"] -> id",
		"r.Body -> data",
		"r.URL.Query().Get(\"q\") -> q",
	})
}

// TestSpringAnnotatedParameters is each Spring binding annotation on a
// handler's parameters, keyed by its literal name or else the parameter's
// name, and a parameter no binding annotation marks reads nothing.
// Mutations caught: the literal name ignored (key userId), `name =` not
// read, the body given a key, and an unannotated parameter published.
func TestSpringAnnotatedParameters(t *testing.T) {
	checkInputs(t, "UserController.java", `package demo;

import org.springframework.web.bind.annotation.*;

@RestController
class UserController {
    @PostMapping("/users/{id}")
    String save(@PathVariable("id") long userId, @RequestHeader(name = "X-Key") String key,
                @RequestParam String q, @RequestBody User user, String plain) {
        return q;
    }
}
`, []string{
		"save spring body  | @RequestBody User user",
		"save spring combined q | @RequestParam String q",
		"save spring header X-Key | @RequestHeader(name = \"X-Key\") String key",
		"save spring path id | @PathVariable(\"id\") long userId",
	})
}

// TestDjangoViewRequestReads is the request a Django view receives: a
// function view routed in this file and a class-based view's handler
// method, each read under its own parameter name; a returned read flows to
// the function's return, which def-use names by the function. Mutations
// caught: the
// class view's `self` taken as the request (nothing read), a method of a
// class that is not a view read, and the routed view not read.
func TestDjangoViewRequestReads(t *testing.T) {
	checkInputs(t, "views.py", `from django.urls import path
from django.views import View


def detail(req, pk):
    return req.GET.get("page")


class Search(View):
    def get(self, request):
        return request.COOKIES["sid"]


class Plain:
    def get(self, request):
        return request.GET["x"]


urlpatterns = [path("d/<int:pk>/", detail)]
`, []string{
		"detail django query page | req.GET.get(\"page\")",
		"get django cookie sid | request.COOKIES[\"sid\"]",
		"req.GET.get(\"page\") -> detail",
		"request.COOKIES[\"sid\"] -> get",
	})
}
