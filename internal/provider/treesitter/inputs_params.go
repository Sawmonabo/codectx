package treesitter

import (
	"strings"

	"github.com/Sawmonabo/codectx/internal/model"
)

// paramRange is one parameter's tokens [lo, hi).
type paramRange struct{ lo, hi int }

// paramList is the parameters of the list whose opening parenthesis is
// token open, split at its top-level commas; empty ones are dropped.
func (s *inputScan) paramList(open int) []paramRange {
	if !s.punct(open, '(') || s.match[open] < 0 {
		return nil
	}
	end := int(s.match[open])
	var out []paramRange
	lo := open + 1
	for k := open + 1; k <= end; k++ {
		if k < end && s.match[k] > 0 {
			k = int(s.match[k])
			continue
		}
		if k == end || s.punct(k, ',') {
			if k > lo {
				out = append(out, paramRange{lo, k})
			}
			lo = k + 1
		}
	}
	return out
}

// openAfter is the first `(` token at or after byte at and before byte end,
// or -1.
func (s *inputScan) openAfter(at, end uint32) int {
	lo, hi := s.tokensIn(at, end)
	for k := lo; k < hi; k++ {
		if s.punct(k, '(') {
			return k
		}
	}
	return -1
}

// firstParamReads reads the request object a function receives as its
// first parameter (its second, after a method's `self`), among the tokens
// of the function's bytes [start, end); the parameter list is the first one
// at or after byte from. An ECMAScript arrow function's lone unparenthesized
// parameter (`req => ...`) is read too. A parameter that is not a plain name
// (a destructuring pattern) names no receiver.
func (s *inputScan) firstParamReads(fw string, table inputTable, from, start, end uint32, skipSelf bool) {
	lo, hi := s.tokensIn(start, end)
	name, body := "", -1
	k := lo
	for k < hi && s.ident(k) && (s.text(k) == "async" || s.text(k) == "function") {
		k++
	}
	if jsLanguage(s.b.lang.Name) && s.ident(k) && s.punct(k+1, '=') && s.punct(k+2, '>') && from == start {
		name, body = s.text(k), k+3
	} else {
		open := s.openAfter(from, end)
		if open < 0 {
			return
		}
		params := s.paramList(open)
		if skipSelf {
			if len(params) == 0 {
				return
			}
			params = params[1:]
		}
		if jsLanguage(s.b.lang.Name) && len(params) > 0 && s.ident(params[0].lo) && s.text(params[0].lo) == "this" {
			// A TypeScript `this` parameter is erased: no argument binds it.
			params = params[1:]
		}
		if fw == fwExpress && s.jsArity(params) == 4 {
			// Express runs a function of four parameters as error-handling
			// middleware, `(err, req, res, next)`: the request is second.
			params = params[1:]
		}
		if len(params) == 0 || !s.ident(params[0].lo) {
			return
		}
		name, body = s.text(params[0].lo), int(s.match[open])+1
	}
	s.reads(fw, []string{name}, table, body, hi)
}

// jsArity is a function's `length` as ECMAScript counts it: its
// parameters before the first with a default value or the rest parameter.
func (s *inputScan) jsArity(params []paramRange) int {
	for n, p := range params {
		if s.punct(p.lo, '.') {
			return n
		}
		for k := p.lo; k < p.hi; k++ {
			if s.match[k] > 0 {
				k = int(s.match[k])
				continue
			}
			if s.punct(k, '=') && !s.punct(k+1, '=') && !s.punct(k+1, '>') && !s.punct(k-1, '=') {
				return n
			}
		}
	}
	return len(params)
}

// djangoViewMethods are the methods of a class-based view that receive the
// request (django.views.generic.base.View).
var djangoViewMethods = map[string]bool{"get": true, "post": true, "put": true, "patch": true, "delete": true,
	"head": true, "options": true, "trace": true, "dispatch": true, "setup": true, "http_method_not_allowed": true}

// djangoViews reads the request of each Django view of the file: a function
// a route of this file routes to or a Django decorator decorates, and the
// request-receiving methods of a class extending a Django view class.
func (s *inputScan) djangoViews() {
	django := false
	for _, imp := range s.rs.imports {
		if imp.path == "django" || strings.HasPrefix(imp.path, "django.") {
			django = true
			break
		}
	}
	if !django {
		return
	}
	s.lex()
	views := map[int]bool{}
	classes := map[int]bool{}
	routed, _ := s.handlerSpans(fwDjango, nil)
	for _, d := range routed {
		if s.b.decls[d].kind == model.NodeClass {
			classes[d] = true
		} else {
			views[d] = true
		}
	}
	memo := map[int]int8{}
	for i := range s.b.decls {
		d := &s.b.decls[i]
		switch d.kind {
		case model.NodeClass:
			if s.djangoViewClass(i, memo) {
				classes[i] = true
			}
		case model.NodeFunction, model.NodeMethod:
			for _, dec := range d.Decorators {
				head, _, _ := strings.Cut(dec.Name, ".")
				if imp, ok := s.rs.imports[head]; ok && strings.HasPrefix(imp.path, "django.") {
					views[i] = true
				}
			}
		}
	}
	for i := range s.b.decls {
		d := &s.b.decls[i]
		if d.kind == model.NodeMethod && d.Parent >= 0 && classes[d.Parent] {
			// View.setup sets self.request before the view dispatches.
			s.selfRequestReads(fwDjango, djangoRequest, i)
			if djangoViewMethods[d.Name] {
				views[i] = true
			}
		}
	}
	for i := range s.b.decls {
		if views[i] {
			d := &s.b.decls[i]
			s.firstParamReads(fwDjango, djangoRequest, d.NameEnd, d.Start, d.End, d.kind == model.NodeMethod)
		}
	}
}

// pyBases are the heads of the base classes class declaration i writes,
// each with its first token.
func (s *inputScan) pyBases(i int) []string {
	d := &s.b.decls[i]
	open := s.openAfter(d.NameEnd, d.SigEnd)
	var out []string
	for _, p := range s.paramList(open) {
		if s.ident(p.lo) {
			out = append(out, s.text(p.lo))
		}
	}
	return out
}

// djangoViewClass reports a class extending, directly or through classes of
// this file, a class a Django views module exports. memo is 1 being
// visited, 2 a view, 3 not one.
func (s *inputScan) djangoViewClass(i int, memo map[int]int8) bool {
	switch memo[i] {
	case 1, 3:
		return false
	case 2:
		return true
	}
	memo[i] = 1
	view := false
	for _, base := range s.pyBases(i) {
		if imp, ok := s.rs.imports[base]; ok {
			if strings.HasPrefix(imp.path, "django.") && strings.Contains(imp.path, "views") {
				view = true
			}
			continue
		}
		for _, j := range s.b.byName[base] {
			if s.b.decls[j].kind == model.NodeClass && s.djangoViewClass(j, memo) {
				view = true
			}
		}
	}
	memo[i] = 3
	if view {
		memo[i] = 2
	}
	return view
}

// The kinds of a FastAPI parameter's type annotation (FastAPI's
// dependencies.utils.analyze_param).
const (
	pyScalar byte = iota
	pyComplex
	pyFile
	pyRequest
	pySkip
	pyUnknown
	pyNone
)

// pyScalars are the annotation heads FastAPI reads as scalars; pyComplexes
// those it reads as a body (a sequence or a mapping); pyMarkers the
// parameter functions naming where a parameter is read from.
var (
	pyScalars = map[string]bool{"int": true, "float": true, "str": true, "bool": true, "bytes": true, "UUID": true,
		"date": true, "datetime": true, "time": true, "timedelta": true, "Decimal": true, "Any": true, "Literal": true,
		"EmailStr": true, "HttpUrl": true, "AnyUrl": true}
	pyComplexes = map[string]bool{"list": true, "List": true, "tuple": true, "Tuple": true, "set": true, "Set": true,
		"frozenset": true, "FrozenSet": true, "Sequence": true, "deque": true, "dict": true, "Dict": true,
		"Mapping": true, "MutableMapping": true}
	pyMarkers = map[string]model.InputSource{"Path": model.InputPath, "Query": model.InputQuery,
		"Header": model.InputHeader, "Cookie": model.InputCookie, "Body": model.InputBody, "Form": model.InputForm,
		"File": model.InputFile, "Depends": "", "Security": ""}
)

// fastAPIHandlers reads the parameters of each handler of framework fw's
// routes: for Starlette, the request its first parameter receives; for
// FastAPI, each parameter by FastAPI's own rule.
func (s *inputScan) fastAPIHandlers(fw string) {
	decls, _ := s.handlerSpans(fw, nil)
	if len(decls) == 0 {
		return
	}
	s.lex()
	if fw == fwStarlette {
		for _, i := range decls {
			d := &s.b.decls[i]
			s.firstParamReads(fwStarlette, starletteRequest, d.NameEnd, d.Start, d.End, false)
		}
		return
	}
	pathNames := map[int]map[string]bool{}
	for i := range s.b.endpointFacts {
		f := &s.b.endpointFacts[i]
		if f.meta.Framework != fw {
			continue
		}
		for _, h := range f.handlers {
			if h.decl < 0 {
				continue
			}
			if pathNames[h.decl] == nil {
				pathNames[h.decl] = map[string]bool{}
			}
			for _, n := range routeParams(f.meta.Route) {
				pathNames[h.decl][n] = true
			}
		}
	}
	for _, i := range decls {
		d := &s.b.decls[i]
		if d.kind != model.NodeFunction {
			continue
		}
		open := s.openAfter(d.NameEnd, d.End)
		for _, p := range s.paramList(open) {
			s.fastAPIParam(p, pathNames[i], d.End)
		}
	}
}

// routeParams are the parameter names a route template writes, `{name}` or
// `{name:converter}`.
func routeParams(route string) []string {
	var out []string
	for {
		i := strings.IndexByte(route, '{')
		if i < 0 {
			return out
		}
		j := strings.IndexByte(route[i:], '}')
		if j < 0 {
			return out
		}
		name, _, _ := strings.Cut(route[i+1:i+j], ":")
		out = append(out, strings.TrimSpace(name))
		route = route[i+j+1:]
	}
}

// fastAPIParam publishes the read one FastAPI handler parameter makes: the
// source its marker names (`Query()`, `Annotated[int, Header()]`), else the
// path when the route writes its name, else a file for an UploadFile, a
// body for a model, a sequence or a mapping, a query for a scalar. A
// Request or WebSocket parameter is the request object, whose reads in the
// handler's body are read by Starlette's table. A dependency, a response,
// a background-tasks object, or a type this file does not declare publishes
// nothing.
func (s *inputScan) fastAPIParam(p paramRange, pathNames map[string]bool, end uint32) {
	if !s.ident(p.lo) {
		return
	}
	name := s.text(p.lo)
	if name == "self" || name == "cls" {
		return
	}
	colon, eq := -1, -1
	for k := p.lo + 1; k < p.hi; k++ {
		if s.match[k] > 0 {
			k = int(s.match[k])
			continue
		}
		switch {
		case colon < 0 && eq < 0 && s.punct(k, ':'):
			colon = k
		case eq < 0 && s.punct(k, '=') && !s.punct(k+1, '='):
			eq = k
		}
	}
	annEnd := p.hi
	if eq >= 0 {
		annEnd = eq
	}
	var typ paramRange
	var markers []paramRange
	if colon >= 0 {
		typ = paramRange{colon + 1, annEnd}
		if inner, ok := s.pyGeneric(typ, "Annotated"); ok && len(inner) > 0 {
			typ, markers = inner[0], inner[1:]
		}
	}
	if eq >= 0 {
		markers = append(markers, paramRange{eq + 1, p.hi})
	}
	at := func(src model.InputSource, key string) {
		s.add(fwFastAPI, src, key, s.toks[p.lo].start, s.toks[p.hi-1].end)
	}
	for _, m := range markers {
		marker, open := s.pyCall(m)
		src, ok := pyMarkers[marker]
		if !ok {
			continue
		}
		if src == "" {
			return
		}
		key, convert := name, src == model.InputHeader
		for _, a := range s.paramList(open) {
			if s.ident(a.lo) && s.punct(a.lo+1, '=') {
				switch s.text(a.lo) {
				case "alias":
					if v, ok := s.str(a.lo + 2); ok {
						key, convert = v, false
					}
				case "convert_underscores":
					if s.ident(a.lo+2) && s.text(a.lo+2) == "False" {
						convert = false
					}
				}
			}
		}
		if convert {
			key = strings.ReplaceAll(key, "_", "-")
		}
		at(src, key)
		return
	}
	kind := pyScalar
	if colon >= 0 {
		kind = s.pyTypeKind(typ, map[int]int8{})
	}
	switch {
	case kind == pyRequest:
		s.reads(fwFastAPI, []string{name}, starletteRequest, p.hi, s.tokenEnd(end))
	case kind == pySkip || kind == pyUnknown:
	case pathNames[name]:
		at(model.InputPath, name)
	case kind == pyFile:
		at(model.InputFile, name)
	case kind == pyComplex:
		at(model.InputBody, name)
	default:
		at(model.InputQuery, name)
	}
}

// tokenEnd is the index just past the last token before byte end.
func (s *inputScan) tokenEnd(end uint32) int {
	_, hi := s.tokensIn(0, end)
	return hi
}

// pyCall is the last name of the call a range starts with (`Query` of
// `fastapi.Query(...)`) and its opening parenthesis, or "" and -1.
func (s *inputScan) pyCall(r paramRange) (string, int) {
	k := r.lo
	if !s.ident(k) {
		return "", -1
	}
	for s.punct(k+1, '.') && s.ident(k+2) {
		k += 2
	}
	if k+1 < r.hi && s.punct(k+1, '(') {
		return s.text(k), k + 1
	}
	return "", -1
}

// pyGeneric is the arguments of a subscripted annotation whose head is
// name (`Annotated[int, Query()]`), split at its top-level commas.
func (s *inputScan) pyGeneric(r paramRange, name string) ([]paramRange, bool) {
	head, open := s.pyHead(r)
	if head != name || open < 0 {
		return nil, false
	}
	return s.subscript(open), true
}

// pyHead is the last name of the dotted name a range starts with and the
// `[` after it, or -1 when it is not subscripted.
func (s *inputScan) pyHead(r paramRange) (string, int) {
	k := r.lo
	if !s.ident(k) {
		return "", -1
	}
	for s.punct(k+1, '.') && s.ident(k+2) {
		k += 2
	}
	if k+1 < r.hi && s.punct(k+1, '[') && s.match[k+1] > 0 {
		return s.text(k), k + 1
	}
	return s.text(k), -1
}

// subscript is the elements of the subscript whose `[` is token open.
func (s *inputScan) subscript(open int) []paramRange {
	end := int(s.match[open])
	var out []paramRange
	lo := open + 1
	for k := open + 1; k <= end; k++ {
		if k < end && s.match[k] > 0 {
			k = int(s.match[k])
			continue
		}
		if k == end || s.punct(k, ',') {
			if k > lo {
				out = append(out, paramRange{lo, k})
			}
			lo = k + 1
		}
	}
	return out
}

// pyTypeKind is how FastAPI reads a parameter of the annotation r: a union
// (`X | None`, Optional, Union) is complex when any member is, unknown when
// any member is a type this file does not declare.
func (s *inputScan) pyTypeKind(r paramRange, memo map[int]int8) byte {
	var members []paramRange
	lo := r.lo
	for k := r.lo; k <= r.hi; k++ {
		if k < r.hi && s.match[k] > 0 {
			k = int(s.match[k])
			continue
		}
		if k == r.hi || s.punct(k, '|') {
			if k > lo {
				members = append(members, paramRange{lo, k})
			}
			lo = k + 1
		}
	}
	kinds := make([]byte, 0, len(members))
	for _, m := range members {
		head, open := s.pyHead(m)
		var kind byte
		switch {
		case head == "None":
			kind = pyNone
		case head == "Optional" || head == "Union" || head == "Annotated":
			if open < 0 {
				return pyUnknown
			}
			args := s.subscript(open)
			if head == "Annotated" && len(args) > 0 {
				args = args[:1]
			}
			sub := make([]byte, 0, len(args))
			for _, a := range args {
				sub = append(sub, s.pyTypeKind(a, memo))
			}
			kind = unionKind(sub)
		case head == "Request" || head == "HTTPConnection" || head == "WebSocket":
			kind = pyRequest
		case head == "Response" || head == "BackgroundTasks" || head == "SecurityScopes":
			kind = pySkip
		case head == "UploadFile":
			kind = pyFile
		case pyComplexes[head]:
			kind = pyComplex
		case pyScalars[head]:
			kind = pyScalar
		default:
			kind = s.pyClassKind(head, memo)
		}
		kinds = append(kinds, kind)
	}
	return unionKind(kinds)
}

// unionKind is the kind of a union of kinds.
func unionKind(kinds []byte) byte {
	out := pyNone
	for _, k := range kinds {
		switch {
		case k == pyNone:
		case out == pyNone:
			out = k
		case k == pyUnknown || out == pyUnknown:
			out = pyUnknown
		case k == pyComplex || out == pyComplex:
			out = pyComplex
		case k != out:
			out = pyUnknown
		}
	}
	if out == pyNone {
		return pyScalar
	}
	return out
}

// pyClassKind is how FastAPI reads a parameter typed by the class name: a
// body for a class of this file that is a Pydantic model or a dataclass
// (directly or through classes of this file), a scalar for any other class
// of this file, and unknown for a name this file does not declare as a
// class, whose kind only its own module tells.
func (s *inputScan) pyClassKind(name string, memo map[int]int8) byte {
	kind := pyUnknown
	for _, i := range s.b.byName[name] {
		if s.b.decls[i].kind != model.NodeClass {
			continue
		}
		k := s.pyDeclClassKind(i, memo)
		if kind == pyUnknown || kind == k {
			kind = k
		} else {
			return pyUnknown
		}
	}
	return kind
}

// pyDeclClassKind is pyClassKind for the class declaration i. memo is 1
// being visited, else the kind plus 2.
func (s *inputScan) pyDeclClassKind(i int, memo map[int]int8) byte {
	if m := memo[i]; m == 1 {
		return pyScalar
	} else if m > 1 {
		return byte(m - 2)
	}
	memo[i] = 1
	kind := pyScalar
	for _, dec := range s.b.decls[i].Decorators {
		if dec.Name == "dataclass" || strings.HasSuffix(dec.Name, ".dataclass") {
			kind = pyComplex
		}
	}
	for _, base := range s.pyBases(i) {
		if kind != pyScalar {
			break
		}
		if imp, ok := s.rs.imports[base]; ok {
			switch {
			case imp.path == "pydantic" || strings.HasPrefix(imp.path, "pydantic."):
				kind = pyComplex
			case imp.path == "enum":
			default:
				kind = pyUnknown
			}
			continue
		}
		if k := s.pyClassKind(base, memo); k != pyUnknown || len(s.b.byName[base]) > 0 {
			kind = k
		}
	}
	memo[i] = int8(kind) + 2
	return kind
}

// goRequests reads every *http.Request parameter of the file's functions,
// methods and function literals, in the function's body: its methods and
// fields, and the path parameters gorilla/mux's Vars and chi's URLParam
// read from it.
func (s *inputScan) goRequests() {
	var httpName, muxName, chiName string
	for name, imp := range s.rs.imports {
		switch goModule(strings.Trim(imp.path, "\"`")) {
		case "net/http":
			if strings.Trim(imp.path, "\"`") == "net/http" {
				httpName = name
			}
		case "github.com/gorilla/mux":
			muxName = name
		case "github.com/go-chi/chi":
			chiName = name
		}
	}
	if httpName == "" {
		return
	}
	s.lex()
	for i := 1; i+4 < len(s.toks); i++ {
		if !s.ident(i) || !s.punct(i+1, '*') || !s.ident(i+2) || s.text(i+2) != httpName || !s.punct(i+3, '.') ||
			!s.ident(i+4) || s.text(i+4) != "Request" || !(s.punct(i-1, '(') || s.punct(i-1, ',')) {
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
		name := s.text(i)
		end := int(s.match[body])
		s.reads(fwNetHTTP, []string{name}, goRequest, body+1, end)
		s.goPathReads(name, muxName, chiName, body+1, end)
	}
}

// enclosingOpen is the innermost opening bracket around token i, or -1.
func (s *inputScan) enclosingOpen(i int) int {
	for k := i - 1; k >= 0; k-- {
		if s.match[k] > int32(i) {
			return k
		}
	}
	return -1
}

// goBody is the opening brace of the body after the parameter list closing
// at token close, or -1 when none follows on its line (a function type).
func (s *inputScan) goBody(close int) int {
	for k := close + 1; k < len(s.toks); k++ {
		if s.toks[k].nl {
			return -1
		}
		switch {
		case s.punct(k, '{'):
			if s.match[k] < 0 {
				return -1
			}
			if s.ident(k-1) && (s.text(k-1) == "interface" || s.text(k-1) == "struct") {
				k = int(s.match[k])
				continue
			}
			return k
		case s.punct(k, '(') || s.punct(k, '['):
			if s.match[k] < 0 {
				return -1
			}
			k = int(s.match[k])
		}
	}
	return -1
}

// goPathReads reads gorilla/mux's `mux.Vars(r)["k"]` and chi's
// `chi.URLParam(r, "k")` of the request named name among tokens [lo, hi).
func (s *inputScan) goPathReads(name, muxName, chiName string, lo, hi int) {
	for i := lo; i+4 < hi; i++ {
		if !s.ident(i) || !s.punct(i+1, '.') || !s.ident(i+2) || !s.punct(i+3, '(') || s.punct(i-1, '.') ||
			!s.ident(i+4) || s.text(i+4) != name || s.match[i+3] < 0 {
			continue
		}
		pkg, fn, c := s.text(i), s.text(i+2), int(s.match[i+3])
		switch {
		case muxName != "" && pkg == muxName && fn == "Vars" && c == i+5:
			key, last := "", c
			if s.punct(c+1, '[') && s.punct(c+3, ']') {
				if v, ok := s.str(c + 2); ok {
					key, last = v, c+3
				}
			}
			s.add(fwGorilla, model.InputPath, key, s.toks[i].start, s.toks[last].end)
			i = last
		case chiName != "" && pkg == chiName && fn == "URLParam" && s.punct(i+5, ','):
			key := ""
			if c == i+7 {
				key, _ = s.str(i + 6)
			}
			s.add(fwChi, model.InputPath, key, s.toks[i].start, s.toks[c].end)
			i = c
		}
	}
}

// springParams are the Spring MVC parameter annotations and the part of the
// request each binds.
var springParams = map[string]model.InputSource{"RequestParam": model.InputCombined, "PathVariable": model.InputPath,
	"RequestHeader": model.InputHeader, "CookieValue": model.InputCookie, "RequestBody": model.InputBody,
	"RequestPart": model.InputForm, "MatrixVariable": model.InputPath, "ModelAttribute": model.InputCombined}

// spring reads every method parameter a Spring MVC binding annotation
// marks: the read is the parameter, keyed by the annotation's literal name
// (`@RequestParam("id")`, `name = "id"`, `value = "id"`) or, when it writes
// none, by the parameter's own name, which Spring binds then.
func (s *inputScan) spring() {
	if !s.rs.frameworks[fwSpring] {
		return
	}
	s.lex()
	for i := range s.b.decls {
		d := &s.b.decls[i]
		if d.kind != model.NodeMethod {
			continue
		}
		open := s.openAfter(d.NameEnd, d.SigEnd)
		for _, p := range s.paramList(open) {
			s.springParam(p)
		}
	}
}

// springParam publishes the read one annotated parameter makes.
func (s *inputScan) springParam(p paramRange) {
	src, key, found, named := model.InputSource(""), "", false, false
	k := p.lo
	for k < p.hi && s.punct(k, '@') && s.ident(k+1) {
		k++
		first := k
		for s.punct(k+1, '.') && s.ident(k+2) {
			k += 2
		}
		ann := s.text(k)
		qualified := string(s.b.src[s.toks[first].start:s.toks[k].end])
		k++
		open := -1
		if s.punct(k, '(') && s.match[k] > 0 {
			open, k = k, int(s.match[k])+1
		}
		as, ok := springParams[ann]
		if !ok || found {
			continue
		}
		if imp, imported := s.rs.imports[ann]; imported && imp.path != springAnnotations+"."+ann && qualified == ann {
			continue
		}
		if qualified != ann && qualified != springAnnotations+"."+ann {
			continue
		}
		src, found, named = as, true, ann != "RequestBody" && ann != "ModelAttribute"
		for j, a := range s.paramList(open) {
			if v, ok := s.str(a.lo); ok && j == 0 && a.hi == a.lo+1 {
				key = v
			}
			if s.ident(a.lo) && s.punct(a.lo+1, '=') && (s.text(a.lo) == "value" || s.text(a.lo) == "name") {
				if v, ok := s.str(a.lo + 2); ok {
					key = v
				}
			}
		}
	}
	if !found || p.hi-1 <= k || !s.ident(p.hi-1) {
		return
	}
	if key == "" && named {
		key = s.text(p.hi - 1)
	}
	s.add(fwSpring, src, key, s.toks[p.lo].start, s.toks[p.hi-1].end)
}
