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
