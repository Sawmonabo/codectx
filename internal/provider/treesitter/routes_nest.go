package treesitter

import (
	"cmp"

	"github.com/Sawmonabo/codectx/internal/model"
	"github.com/Sawmonabo/codectx/internal/provider/treesitter/wire"
)

// nestMethods are NestJS's route decorators of `@nestjs/common`, each with
// the method it names; `All` names none.
var nestMethods = map[string]string{"Get": "GET", "Post": "POST", "Put": "PUT", "Delete": "DELETE",
	"Patch": "PATCH", "Options": "OPTIONS", "Head": "HEAD", "QueryMethod": "QUERY", "All": ""}

// nest reads the file's NestJS routes: each route decorator on a method of
// a class `@Controller(...)` decorates is one endpoint per route it names
// (a string, an array, or none for the controller's own path), routed to
// the method. Nest joins the controller's path, and then the application's
// global prefix and the RouterModule paths, which another file sets, so no
// route resolves here: each is `unresolved`, its route the method's own,
// and the controller's path a prefix the linker would join.
func (s *routeScan) nest() {
	if !s.frameworks[fwNest] {
		return
	}
	for i := range s.b.decls {
		d := &s.b.decls[i]
		if d.kind != model.NodeMethod || d.Parent < 0 || !s.nestController(s.b.decls[d.Parent].Decorators) {
			continue
		}
		for _, a := range d.Decorators {
			name, ok := s.nestImported(a.Name)
			if !ok {
				continue
			}
			method, ok := nestMethods[name]
			if !ok {
				continue
			}
			methods := []string{method}
			if method == "" {
				methods = nil
			}
			at := wire.Span{Start: a.Start, End: a.End}
			for _, r := range s.nestRoutes(s.decoratorArgs(a)) {
				s.emitPrefixed(fwNest, methods, r, "", false, at, []handler{declHandler(i)}, s.b.decls[d.Parent].Name)
			}
		}
	}
}

// nestImported is the name a decorator imported from `@nestjs/common` has
// there, and false for any other decorator.
func (s *routeScan) nestImported(name string) (string, bool) {
	imp, ok := s.imports[name]
	if !ok || imp.path != "@nestjs/common" {
		return "", false
	}
	return cmp.Or(imp.original, name), true
}

// nestController reports a class decorated with NestJS's `@Controller`.
func (s *routeScan) nestController(decorators []wire.Decorator) bool {
	for _, a := range decorators {
		if n, ok := s.nestImported(a.Name); ok && n == "Controller" {
			return true
		}
	}
	return false
}

// nestRoutes are the routes a route decorator names: its first argument, a
// string or an array of them, or the empty route when it passes none.
func (s *routeScan) nestRoutes(args []routeArg) []routeText {
	pos := positional(args)
	if len(pos) == 0 {
		return []routeText{{written: `""`, literal: true}}
	}
	var out []routeText
	for _, e := range elements(pos[0].text) {
		v, lit := stringLiteral(s.b.lang.Name, e)
		out = append(out, routeText{written: e, value: v, literal: lit})
	}
	return out
}
