package link_test

import "testing"

// goRouteFiles is a module whose routers cross files through calls: a chi
// router the program serves mounts a router another package returns
// (`Mount`), which passes itself on to a third package's function, and
// hands chi's `Route` a function value of a fourth; a gin program passes a
// group and its engine to one function, which registers on both, and on a
// group of the first, with a handler of a fifth package. users.Shadow
// assigns its parameter before registering on it, so its route is on
// another router and reaches no path.
var goRouteFiles = map[string]string{
	"go.mod": "module example.com/app\n\ngo 1.22\n",
	"main.go": `package main

import (
	"net/http"

	"github.com/go-chi/chi/v5"

	"example.com/app/api"
	"example.com/app/users"
)

func main() {
	r := chi.NewRouter()
	r.Mount("/api", api.Routes())
	r.Route("/users", users.Routes)
	r.Route("/shadow", users.Shadow)
	http.ListenAndServe(":8080", r)
}
`,
	"api/api.go": `package api

import (
	"net/http"

	"github.com/go-chi/chi/v5"

	"example.com/app/admin"
)

func Routes() chi.Router {
	r := chi.NewRouter()
	r.Get("/status", status)
	admin.Register(r)
	return r
}

func status(w http.ResponseWriter, req *http.Request) {}
`,
	"admin/admin.go": `package admin

import (
	"net/http"

	"github.com/go-chi/chi/v5"
)

func Register(r chi.Router) {
	r.Get("/panel", panel)
}

func panel(w http.ResponseWriter, req *http.Request) {}
`,
	"users/users.go": `package users

import (
	"net/http"

	"github.com/go-chi/chi/v5"
)

func Routes(r chi.Router) {
	r.Get("/{id}", show)
}

func Shadow(r chi.Router) {
	r = chi.NewRouter()
	r.Get("/lost", show)
}

func show(w http.ResponseWriter, req *http.Request) {}
`,
	"web/main.go": `package main

import (
	"github.com/gin-gonic/gin"

	"example.com/app/web/routes"
)

func main() {
	e := gin.Default()
	v1 := e.Group("/v1")
	routes.Register(v1, e)
	e.Run()
}
`,
	"web/routes/routes.go": `package routes

import (
	"github.com/gin-gonic/gin"

	"example.com/app/handlers"
)

func Register(v1 *gin.RouterGroup, root *gin.Engine) {
	u := v1.Group("/users")
	u.GET("/:id", handlers.Show)
	root.GET("/health", health)
}

func health(c *gin.Context) {}
`,
	"handlers/handlers.go": `package handlers

import "github.com/gin-gonic/gin"

func Show(c *gin.Context) {}
`,
}

// TestGoRoutersAcrossFiles is the Go routers that reach another file
// through a call, each route published at its full path on the serving
// program's mount, routed to its handler. Mutations: "result unread"
// (goMountResult records nothing) loses /api/status and /api/panel;
// "nested mount dropped" (the payload carries no mounts) loses
// /api/panel; "function value unread" (chiClosure records no
// JoinFunction) loses /users/{id}; "parameter by position" (targets
// matches p.Pos instead of p.Index) loses /health and drops /v1 from
// /v1/users/:id; "named handler unread" (goNamed returns nothing) loses
// Show; "assigned parameter kept" (goParamObj ignores the writes) adds
// /shadow/lost.
func TestGoRoutersAcrossFiles(t *testing.T) {
	checkMounted(t, goRouteFiles, []string{
		"main.go GET /status /api/status -> status",
		"main.go GET /panel /api/panel -> panel",
		"main.go GET /{id} /users/{id} -> show",
		"web/main.go GET /:id /v1/users/:id -> Show",
		"web/main.go GET /health /health -> health",
	})
}
