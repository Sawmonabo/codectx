package treesitter_test

import "testing"

// TestGoHandlerWrappers is a handler written as a call: a conversion to
// net/http's HandlerFunc is the function it converts, and any other call
// builds the handler, so the endpoint routes to the function it calls.
// Mutations caught: a wrapper handler left with no routes_to, the
// conversion routed to HandlerFunc instead of show, and the middleware's
// argument taken as the handler.
func TestGoHandlerWrappers(t *testing.T) {
	c := extracted(t, newProvider(t), "main.go", `package main

import "net/http"

func show(w http.ResponseWriter, r *http.Request) {}

func auth(h http.HandlerFunc) http.HandlerFunc { return h }

func main() {
	http.Handle("/show", http.HandlerFunc(show))
	http.Handle("/private", auth(show))
}
`)
	checkRoutes(t, c, map[string]route{
		"/show":    {framework: "net/http", route: "/show", prefix: "resolved", path: "/show", handlers: []string{"show"}},
		"/private": {framework: "net/http", route: "/private", prefix: "resolved", path: "/private", handlers: []string{"auth"}},
	})
}

// TestExpressDestructuredRouter is `const { Router } = require("express")`:
// the Router it binds builds a router like express.Router(), mounted with
// use. Mutation caught: the destructured binding not read as Express's
// Router (the route's prefix unresolved).
func TestExpressDestructuredRouter(t *testing.T) {
	c := extracted(t, newProvider(t), "server.js", `const express = require("express");
const { Router } = require("express");

const app = express();
const api = Router();

function list(req, res) { res.send([]); }

api.get("/items", list);
app.use("/api", api);
`)
	checkRoutes(t, c, map[string]route{
		"GET /items": {framework: "express", method: "GET", route: "/items", prefix: "resolved", path: "/api/items", handlers: []string{"list"}},
	})
}

// TestGoStripPrefixAndServerLiteral is a ServeMux mounted through
// http.StripPrefix, whose routes sit below the stripped prefix, and a
// router served by a Server literal's Handler field. Mutations caught: the
// StripPrefix mount not followed (the inner route unresolved), the
// registration's pattern joined instead of the stripped prefix (path
// /api//users), and the Server literal not read (the gorilla route
// unresolved).
func TestGoStripPrefixAndServerLiteral(t *testing.T) {
	c := extracted(t, newProvider(t), "main.go", `package main

import (
	"net/http"

	"github.com/gorilla/mux"
)

func users(w http.ResponseWriter, r *http.Request) {}
func items(w http.ResponseWriter, r *http.Request) {}

func main() {
	api := http.NewServeMux()
	api.HandleFunc("/users", users)
	http.Handle("/api/", http.StripPrefix("/api", api))

	r := mux.NewRouter()
	r.HandleFunc("/items", items)
	srv := &http.Server{Addr: ":8080", Handler: r}
	srv.ListenAndServe()
}
`)
	got := c.routes(t)
	want := map[string]string{"/users": "/api/users", "/items": "/items"}
	for d, path := range want {
		rs := got[d]
		if len(rs) != 1 || rs[0].prefix != "resolved" || rs[0].path != path {
			t.Errorf("endpoint %q = %+v, want path %s", d, rs, path)
		}
	}
}

// TestEchoMatch is echo's Match, whose first argument is a slice of
// methods: one endpoint per method. Mutation caught: Match not read (no
// endpoints), its methods read as one.
func TestEchoMatch(t *testing.T) {
	c := extracted(t, newProvider(t), "main.go", `package main

import (
	"net/http"

	"github.com/labstack/echo/v4"
)

func save(c echo.Context) error { return nil }

func main() {
	e := echo.New()
	e.Match([]string{http.MethodPut, "PATCH"}, "/items", save)
}
`)
	checkRoutes(t, c, map[string]route{
		"PUT /items":   {framework: "echo", method: "PUT", route: "/items", prefix: "resolved", path: "/items", handlers: []string{"save"}},
		"PATCH /items": {framework: "echo", method: "PATCH", route: "/items", prefix: "resolved", path: "/items", handlers: []string{"save"}},
	})
}

// TestSpringClassMethodsAndSeveralPrefixes is a class-level
// `@RequestMapping` that names two prefixes and a method: every method
// route is mapped below each prefix, and its methods are the union of the
// class's and its own (Spring's RequestMethodsRequestCondition.combine).
// Mutations caught: several prefixes left unresolved, one prefix dropped,
// the class's method ignored (only GET), and a method mapping naming none
// given any method instead of the class's.
func TestSpringClassMethodsAndSeveralPrefixes(t *testing.T) {
	c := extracted(t, newProvider(t), "Api.java", `package demo;

import org.springframework.web.bind.annotation.*;

@RestController
@RequestMapping(value = {"/a", "/b"}, method = RequestMethod.POST)
class Api {
    @GetMapping("/x")
    String x() { return ""; }

    @RequestMapping("/y")
    String y() { return ""; }
}
`)
	got := c.routes(t)
	for _, w := range []struct{ descriptor, path string }{
		{"GET /x", "/a/x"}, {"GET /x", "/b/x"}, {"POST /x", "/a/x"}, {"POST /x", "/b/x"},
		{"POST /y", "/a/y"}, {"POST /y", "/b/y"},
	} {
		found := false
		for _, r := range got[w.descriptor] {
			found = found || r.prefix == "resolved" && r.path == w.path
		}
		if !found {
			t.Errorf("no endpoint %q at %s; all: %+v", w.descriptor, w.path, got)
		}
	}
	if n := len(got["GET /x"]) + len(got["POST /x"]) + len(got["POST /y"]); n != 6 || len(got) != 3 {
		t.Errorf("endpoints %+v, want exactly the six", got)
	}
}

// TestDjangoQualifiedPath is a urlconf written through the module,
// `from django import urls` then `urls.path(...)`, with a qualified
// `urls.include(...)`. Mutation caught: the qualified call not read (no
// endpoint).
func TestDjangoQualifiedPath(t *testing.T) {
	c := extracted(t, newProvider(t), "urls.py", `from django import urls

from . import views

urlpatterns = [
    urls.path("about/", views.about),
    urls.path("blog/", urls.include("blog.urls")),
]
`)
	checkRoutes(t, c, map[string]route{
		"about/": {framework: "django", route: "about/", prefix: "unresolved", handlers: []string{"about"}},
	})
}

// TestNestJSControllerRoutes is a NestJS controller: each route decorator
// imported from `@nestjs/common` on a method of a `@Controller` class is
// one endpoint per route, an array one per element, and none the
// controller's own path, every one unresolved since the application's
// global prefix is set in another file. Mutations caught: the decorators
// not read (no endpoints), a method of a class that is no controller read,
// a same-named decorator from another package read, and a path guessed
// from the controller alone.
func TestNestJSControllerRoutes(t *testing.T) {
	c := extracted(t, newProvider(t), "cats.controller.ts", `import { Controller, Get, Post } from "@nestjs/common";
import { Get as Fetch } from "./local";

@Controller("cats")
export class CatsController {
  @Get()
  findAll() { return []; }

  @Post(["new", "create"])
  create() { return {}; }
}

export class Plain {
  @Get("x")
  x() { return 1; }

  @Fetch("y")
  y() { return 2; }
}
`)
	checkRoutes(t, c, map[string]route{
		`GET ""`:      {framework: "nestjs", method: "GET", route: `""`, prefix: "unresolved", handlers: []string{"findAll"}},
		"POST new":    {framework: "nestjs", method: "POST", route: "new", prefix: "unresolved", handlers: []string{"create"}},
		"POST create": {framework: "nestjs", method: "POST", route: "create", prefix: "unresolved", handlers: []string{"create"}},
	})
}
