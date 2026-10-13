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
		"req.cookies.sid -> return",
		"req.hostname -> return",
		"rq.query.token -> return",
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
		"c.Bind(nil) -> return",
	})
}

// TestDRFViewRequestReads is REST framework's request: an `@api_view`
// function's parameter, a viewset's action and `@action` method, and
// `self.request` in any method of a view class, read with REST framework's
// own members beside Django's; a class that is no view reads nothing.
// Mutations caught: data or query_params not modelled (their reads lost),
// the view class not followed through a generics module import, actions
// not read (no list read), `@action` not read (no recent read),
// `self.request` not read (no get_queryset read), and the view-class gate
// dropped (Plain's method read).
func TestDRFViewRequestReads(t *testing.T) {
	checkInputs(t, "views.py", `from rest_framework import generics, viewsets
from rest_framework.decorators import action, api_view


@api_view(["POST"])
def create(request):
    return request.data["name"]


class Base(viewsets.ModelViewSet):
    pass


class Users(Base):
    def list(self, request):
        return request.query_params.get("page")

    @action(detail=False)
    def recent(self, request):
        return request.COOKIES["sid"]

    def get_queryset(self):
        return self.request.query_params["q"]


class Items(generics.ListAPIView):
    def get(self, request):
        return request.META["HTTP_X"]


class Plain:
    def list(self, request):
        return request.data["x"]
`, []string{
		"create djangorestframework body name | request.data[\"name\"]",
		"get djangorestframework header HTTP_X | request.META[\"HTTP_X\"]",
		"get_queryset djangorestframework query q | self.request.query_params[\"q\"]",
		"list djangorestframework query page | request.query_params.get(\"page\")",
		"recent djangorestframework cookie sid | request.COOKIES[\"sid\"]",
		"request.META[\"HTTP_X\"] -> return",
		"request.COOKIES[\"sid\"] -> return",
		"request.data[\"name\"] -> return",
		"request.query_params.get(\"page\") -> return",
		"self.request.query_params[\"q\"] -> return",
	})
}

// TestDjangoClassViewSelfRequest is `self.request` in any method of a
// Django class-based view, which View.setup sets before dispatch. Mutation
// caught: self.request not read in a Django view (get_context_data reads
// nothing).
func TestDjangoClassViewSelfRequest(t *testing.T) {
	checkInputs(t, "views.py", `from django.views.generic import TemplateView


class Home(TemplateView):
    def get_context_data(self, **kwargs):
        return {"q": self.request.GET["q"]}
`, []string{
		"get_context_data django query q | self.request.GET[\"q\"]",
		"self.request.GET[\"q\"] -> return",
	})
}

// TestSpringTypeResolvedParameters is a Spring handler's parameter that no
// binding annotation marks and that a resolver Spring runs before the
// project's own decides by its type: a multipart file is a file read keyed
// by its name, the servlet request the request object, an InputStream the
// raw body, an HttpEntity the body, and the response, the model or a Map
// nothing; a parameter another annotation marks and a method of no
// controller read nothing. Mutations caught: the multipart rule dropped
// (no file read), the type-based inputs dropped (no request, raw or body
// read), a skipped type published, the controller gate dropped (helper's
// parameter read), and a foreign annotation ignored (the @CurrentUser
// parameter left to the default).
func TestSpringTypeResolvedParameters(t *testing.T) {
	checkInputs(t, "Upload.java", `package demo;

import java.io.InputStream;
import java.util.Map;
import jakarta.servlet.http.HttpServletRequest;
import jakarta.servlet.http.HttpServletResponse;
import org.springframework.http.HttpEntity;
import org.springframework.stereotype.Controller;
import org.springframework.ui.Model;
import org.springframework.web.bind.annotation.PostMapping;
import org.springframework.web.multipart.MultipartFile;

@Controller
class Upload {
    @PostMapping("/u")
    String save(MultipartFile file, HttpServletRequest req, HttpServletResponse res, InputStream in,
                HttpEntity<String> entity, Model model, Map<String, String> map, @CurrentUser HttpServletRequest who) {
        return "";
    }
}

class Helper {
    @PostMapping("/h")
    String help(MultipartFile file) {
        return "";
    }
}
`, []string{
		"save spring body  | HttpEntity<String> entity",
		"save spring file file | MultipartFile file",
		"save spring raw  | InputStream in",
		"save spring request  | HttpServletRequest req",
	})
}
