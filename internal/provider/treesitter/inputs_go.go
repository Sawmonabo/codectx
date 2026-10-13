package treesitter

import (
	"strings"

	"github.com/Sawmonabo/codectx/internal/model"
)

// ginContext is gin's *gin.Context (gin-gonic/gin context.go): its request
// getters, keyed by their first argument, and its bind methods, each
// reading the part its binding names. Bind and ShouldBind pick the binding
// by the request's method and content type (binding.Default), so they read
// combined. A bind whose binding is an argument (BindWith, ShouldBindWith,
// ShouldBindBodyWith) names no part and reads nothing; FullPath is the
// route's own template, no input. The request is the field Request, read
// with net/http's table.
var ginContext = func() inputTable {
	t := inputTable{
		"Param": argAttr(model.InputPath), "Params": keyedAttr(model.InputPath),
		"FormFile": argAttr(model.InputFile), "MultipartForm": callAttr(model.InputForm),
		"GetHeader": argAttr(model.InputHeader), "ContentType": callAttr(model.InputHeader),
		"ClientIP": callAttr(model.InputHeader), "Cookie": argAttr(model.InputCookie),
		"GetRawData": callAttr(model.InputRaw),
		"Bind":       callAttr(model.InputCombined), "ShouldBind": callAttr(model.InputCombined),
		"Request": {sub: goRequest},
	}
	for _, m := range []string{"Query", "DefaultQuery", "GetQuery", "QueryArray", "GetQueryArray", "QueryMap", "GetQueryMap"} {
		t[m] = argAttr(model.InputQuery)
	}
	for _, m := range []string{"PostForm", "DefaultPostForm", "GetPostForm", "PostFormArray", "GetPostFormArray", "PostFormMap", "GetPostFormMap"} {
		t[m] = argAttr(model.InputForm)
	}
	for suffix, src := range map[string]model.InputSource{"JSON": model.InputBody, "XML": model.InputBody,
		"YAML": model.InputBody, "TOML": model.InputBody, "Plain": model.InputBody, "Query": model.InputQuery,
		"Header": model.InputHeader, "Uri": model.InputPath} {
		t["Bind"+suffix], t["ShouldBind"+suffix] = callAttr(src), callAttr(src)
	}
	for _, f := range []string{"JSON", "XML", "YAML", "TOML", "Plain"} {
		t["ShouldBindBodyWith"+f] = callAttr(model.InputBody)
	}
	return t
}()

// echoContext is echo's Context (labstack/echo context.go; an interface in
// v4, a struct pointer in v5): its path, query, form, file and cookie
// getters, Bind, which binds the path parameters, the query of a GET,
// DELETE or HEAD and then the body (DefaultBinder.Bind), so it reads
// combined, and the request, the call Request(), read with net/http's
// table. RealIP and Scheme read forwarding headers; Path is the route's own
// template, no input; Get reads the context's store, not the request.
var echoContext = inputTable{
	"Request": {call: true, sub: goRequest},
	"Param":   argAttr(model.InputPath), "ParamOr": argAttr(model.InputPath),
	"ParamValues": callAttr(model.InputPath), "PathValues": callAttr(model.InputPath),
	"QueryParam": argAttr(model.InputQuery), "QueryParamOr": argAttr(model.InputQuery),
	"QueryParams": {source: model.InputQuery, call: true, keyed: true}, "QueryString": callAttr(model.InputQuery),
	"FormValue": argAttr(model.InputCombined), "FormParams": callAttr(model.InputForm),
	"FormFile": argAttr(model.InputFile), "MultipartForm": callAttr(model.InputForm),
	"Cookie": argAttr(model.InputCookie), "Cookies": callAttr(model.InputCookie),
	"RealIP": callAttr(model.InputHeader), "Scheme": callAttr(model.InputHeader),
	"Bind": callAttr(model.InputCombined),
}

// goContexts reads every gin `*gin.Context` and echo `echo.Context` (or
// `*echo.Context`) parameter of the file's functions, methods and function
// literals, in the function's body, in a file that imports the framework.
func (s *inputScan) goContexts() {
	for name, imp := range s.rs.imports {
		var fw, typ string
		var table inputTable
		var star bool
		switch goModule(strings.Trim(imp.path, "\"`")) {
		case "github.com/gin-gonic/gin":
			fw, typ, table, star = fwGin, "Context", ginContext, true
		case "github.com/labstack/echo":
			fw, typ, table = fwEcho, "Context", echoContext
		default:
			continue
		}
		s.lex()
		s.goTypedParams(name, typ, star, func(param string, lo, hi int) {
			s.reads(fw, []string{param}, table, lo, hi)
		})
	}
}

// goTypedParams calls visit with each parameter of type pkg.typ (a pointer
// to it when star; either when not) of a function with a body: its name
// and the token range of the body.
func (s *inputScan) goTypedParams(pkg, typ string, star bool, visit func(name string, lo, hi int)) {
	for i := 1; i+3 < len(s.toks); i++ {
		if !s.ident(i) || !(s.punct(i-1, '(') || s.punct(i-1, ',')) {
			continue
		}
		k := i + 1
		if s.punct(k, '*') {
			k++
		} else if star {
			continue
		}
		if !s.ident(k) || s.text(k) != pkg || !s.punct(k+1, '.') || !s.ident(k+2) || s.text(k+2) != typ {
			continue
		}
		open := s.enclosingOpen(i)
		if open < 0 || !s.punct(open, '(') {
			continue
		}
		body := s.goBody(int(s.match[open]))
		if body < 0 {
			continue
		}
		visit(s.text(i), body+1, int(s.match[body]))
	}
}
