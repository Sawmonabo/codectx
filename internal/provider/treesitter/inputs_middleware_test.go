package treesitter_test

import "testing"

// TestExpressMiddlewareRequestReads is the request parameter of each
// function Express runs as middleware: every function `use` passes on an
// application the file builds or at a literal path, the functions before a
// route's handler, an array's elements, and a four-parameter function's
// second parameter, which Express runs as error-handling middleware.
// Mutations caught: middleware not read (no read in auth, check, listed or
// the error handler), the receiver gate dropped (`other.use(loose)` reads
// loose's body), the arity rule dropped (the error handler reads err, so
// `err.path` is published and `req.path` is not), the array not flattened
// (listed reads nothing), and a wrapper's builder read as the request
// (`build`'s opts.query published).
func TestExpressMiddlewareRequestReads(t *testing.T) {
	checkInputs(t, "app.js", `const express = require("express");
const app = express();

function auth(req, res, next) {
  return req.cookies.sid;
}

function check(rq, res, next) {
  return rq.query.token;
}

function listed(req, res, next) {
  return req.hostname;
}

function loose(req) {
  return req.body;
}

function build(opts) {
  return opts.query;
}

app.use(express.json());
app.use(auth);
app.use("/admin", [listed]);
app.use(build("admin"));
other.use(loose);
app.get("/x", check, (req, res) => {
  res.send("ok");
});
app.use((err, req, res, next) => {
  res.send(err.path + req.path);
});
`, []string{
		"auth express cookie sid | req.cookies.sid",
		"check express query token | rq.query.token",
		"listed express header  | req.hostname",
		"app.js express url  | req.path",
		"req.cookies.sid -> auth",
		"req.hostname -> listed",
		"rq.query.token -> check",
	})
}

// TestGinAndEchoContextReads is each gin `*gin.Context` and echo
// `echo.Context` parameter read in its function's body: the getters keyed by
// their literal argument, a bind by the part its name binds, the request
// through net/http's table, and nothing for the route's own template or the
// context's store. Mutations caught: the gin table not read (no gin line),
// the echo pointer form required (the v4 interface parameter reads nothing),
// the bind suffix ignored (ShouldBindJSON combined), the request's sub-table
// not followed (`c.Request.Header.Get` lost), and echo's Path, the route's
// template, modelled as a read.
func TestGinAndEchoContextReads(t *testing.T) {
	checkInputs(t, "main.go", `package main

import (
	"github.com/gin-gonic/gin"
	"github.com/labstack/echo/v4"
)

func show(c *gin.Context) {
	id := c.Param("id")
	var in struct{}
	_ = c.ShouldBindJSON(&in)
	_ = c.Request.Header.Get("X-Key")
	_ = c.Request.Context()
	_ = c.FullPath()
	c.JSON(200, id)
}

func list(c echo.Context) error {
	_ = c.QueryParams().Get("page")
	_ = c.Request().Header.Get("X-Trace")
	_ = c.Path()
	_ = c.Get("user")
	return c.Bind(nil)
}
`, []string{
		"list echo combined  | c.Bind(nil)",
		"list echo header X-Trace | c.Request().Header.Get(\"X-Trace\")",
		"list echo query page | c.QueryParams().Get(\"page\")",
		"show gin body  | c.ShouldBindJSON(&in)",
		"show gin header X-Key | c.Request.Header.Get(\"X-Key\")",
		"show gin path id | c.Param(\"id\")",
		"c.Param(\"id\") -> id",
		"c.ShouldBindJSON(&in) -> in",
	})
}
