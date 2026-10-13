package treesitter

import (
	"cmp"
	"maps"
	"strings"

	"github.com/Sawmonabo/codectx/internal/model"
)

// fwDRF is Django REST framework, as a request input's metadata names it.
const fwDRF = "djangorestframework"

// drfRequest is REST framework's Request (api-guide/requests): Django's
// HttpRequest members, which it extends by composition, and its own data
// (the parsed body, files included), query_params (Django's GET), stream
// (the raw body) and content_type.
var drfRequest = func() inputTable {
	t := maps.Clone(djangoRequest)
	t["data"] = keyedAttr(model.InputBody)
	t["query_params"] = keyedAttr(model.InputQuery)
	t["stream"] = attr(model.InputRaw)
	t["content_type"] = attr(model.InputHeader)
	return t
}()

// drfViewModules are the REST framework modules whose classes are views:
// APIView, the generic views and the viewsets.
var drfViewModules = []string{"rest_framework.views.", "rest_framework.generics.", "rest_framework.viewsets."}

// drfActions are the viewset actions a router routes to, beside the HTTP
// method handlers an APIView dispatches to (djangoViewMethods).
var drfActions = map[string]bool{"list": true, "create": true, "retrieve": true, "update": true,
	"partial_update": true, "destroy": true}

// drfViews reads, in a file importing rest_framework, the request each REST
// framework view receives: the first parameter of a function an
// `@api_view` decorates, and of each HTTP method handler, viewset action
// and `@action` method of a class extending, directly or through classes of
// the file, a REST framework view; and `self.request` in every method of
// such a class, which the view sets before it dispatches.
func (s *inputScan) drfViews() {
	if !s.importsUnder("rest_framework") {
		return
	}
	s.lex()
	memo := map[int]int8{}
	for i := range s.b.decls {
		d := &s.b.decls[i]
		switch {
		case d.kind == model.NodeFunction && d.Parent < 0 && s.drfDecorated(i, "api_view"):
			s.firstParamReads(fwDRF, drfRequest, d.NameEnd, d.Start, d.End, false)
		case d.kind == model.NodeMethod && d.Parent >= 0 && s.b.decls[d.Parent].kind == model.NodeClass &&
			s.viewClass(d.Parent, memo, drfViewModules):
			if djangoViewMethods[d.Name] || drfActions[d.Name] || s.drfDecorated(i, "action") {
				s.firstParamReads(fwDRF, drfRequest, d.NameEnd, d.Start, d.End, true)
			}
			s.selfRequestReads(fwDRF, drfRequest, i)
		}
	}
}

// importsUnder reports an import of the package root or a module under it.
func (s *inputScan) importsUnder(root string) bool {
	for _, imp := range s.rs.imports {
		if imp.path == root || strings.HasPrefix(imp.path, root+".") {
			return true
		}
	}
	return false
}

// drfDecorated reports declaration i decorated by REST framework's
// decorator name (`@api_view([...])`, `@decorators.action(...)`), imported
// from rest_framework.
func (s *inputScan) drfDecorated(i int, name string) bool {
	for _, dec := range s.b.decls[i].Decorators {
		head, rest, dotted := strings.Cut(dec.Name, ".")
		last := head
		if dotted {
			last = rest[strings.LastIndexByte(rest, '.')+1:]
		}
		imp, ok := s.rs.imports[head]
		if last == name && ok && (imp.path == "rest_framework" || strings.HasPrefix(imp.path, "rest_framework.")) {
			return true
		}
	}
	return false
}

// selfRequestReads reads `self.request` (under the method's own first
// parameter name) in method i's body.
func (s *inputScan) selfRequestReads(fw string, table inputTable, i int) {
	d := &s.b.decls[i]
	params := s.paramList(s.openAfter(d.NameEnd, d.End))
	if len(params) == 0 || !s.ident(params[0].lo) || params[0].hi != params[0].lo+1 {
		return
	}
	lo, hi := s.tokensIn(d.Start, d.End)
	s.reads(fw, []string{s.text(params[0].lo), "request"}, table, lo, hi)
}

// viewClass reports a class extending, directly or through classes of
// this file, a class whose dotted name (the import's module joined to the
// base as written) lies under one of modules. memo is 1 being visited, 2
// a view, 3 not one.
func (s *inputScan) viewClass(i int, memo map[int]int8, modules []string) bool {
	switch memo[i] {
	case 1, 3:
		return false
	case 2:
		return true
	}
	memo[i] = 1
	view := false
	for _, base := range s.pyDottedBases(i) {
		head, rest, _ := strings.Cut(base, ".")
		if imp, ok := s.rs.imports[head]; ok {
			full := imp.path
			if s.rs.fromImport(head) {
				full += "." + cmp.Or(imp.original, head)
			}
			if rest != "" {
				full += "." + rest
			}
			for _, m := range modules {
				if strings.HasPrefix(full, m) {
					view = true
				}
			}
			continue
		}
		if rest != "" {
			continue
		}
		for _, j := range s.b.byName[base] {
			if s.b.decls[j].kind == model.NodeClass && s.viewClass(j, memo, modules) {
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

// pyDottedBases are the base classes class declaration i writes that are
// a name or a dotted name, as written.
func (s *inputScan) pyDottedBases(i int) []string {
	d := &s.b.decls[i]
	var out []string
	for _, p := range s.paramList(s.openAfter(d.NameEnd, d.SigEnd)) {
		k := p.lo
		if !s.ident(k) {
			continue
		}
		for k+2 < p.hi && s.punct(k+1, '.') && s.ident(k+2) {
			k += 2
		}
		if k+1 == p.hi {
			out = append(out, string(s.b.src[s.toks[p.lo].start:s.toks[k].end]))
		}
	}
	return out
}
