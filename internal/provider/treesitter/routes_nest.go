package treesitter

import (
	"cmp"
	"slices"
	"strings"

	"github.com/Sawmonabo/codectx/internal/model"
	"github.com/Sawmonabo/codectx/internal/provider/link"
	"github.com/Sawmonabo/codectx/internal/provider/treesitter/wire"
)

// nestMethods are NestJS's route decorators of `@nestjs/common`, each with
// the method it names; `All` names none.
var nestMethods = map[string]string{"Get": "GET", "Post": "POST", "Put": "PUT", "Delete": "DELETE",
	"Patch": "PATCH", "Options": "OPTIONS", "Head": "HEAD", "QueryMethod": "QUERY", "All": ""}

// nestRoute is one NestJS route as the linker joins it (link.NestRoute),
// with the decorator it is read from.
type nestRoute struct {
	link.NestRoute
	at wire.Span
}

// nest reads the file's NestJS routes: each route decorator on a method of
// a class `@Controller(...)` decorates is one endpoint per route it names
// (a string, an array, or none for the controller's own path), routed to
// the method. Nest joins the controller's path, and then the application's
// global prefix and the RouterModule paths, which another file sets, so no
// route resolves here: each is `unresolved`, its route the method's own.
// Each is also recorded for the linker with the controller's paths and
// version and the method's (link.NestRoute), which joins them with what
// the file creating the application sets on it.
func (s *routeScan) nest() {
	if !s.frameworks[fwNest] {
		return
	}
	for i := range s.b.decls {
		d := &s.b.decls[i]
		if d.kind != model.NodeMethod || d.Parent < 0 {
			continue
		}
		ctrl, ok := s.nestController(s.b.decls[d.Parent].Decorators)
		if !ok {
			continue
		}
		version := s.nestMethodVersion(d.Decorators)
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
			args := s.decoratorArgs(a)
			for _, r := range s.nestRoutes(args) {
				s.emitPrefixed(fwNest, methods, r, "", false, at, []handler{declHandler(i)}, s.b.decls[d.Parent].Name)
			}
			for _, r := range s.nestRoutes(args) {
				var paths []string
				if r.literal && r.value != "" {
					// No path, or an empty one, is Nest's `/`.
					paths = []string{r.value}
				}
				for _, m := range append(methods[:len(methods):len(methods)], nestAny(methods)...) {
					s.nestOut = append(s.nestOut, nestRoute{at: at, Method: m, Route: r.route(),
						Controllers: ctrl.paths, Paths: paths, Known: r.literal && ctrl.known, Version: version,
						Controller: ctrl.version, Handler: int32(i)})
				}
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

// nestControllerOf is what a class's `@Controller` writes: its paths,
// whether they are literals, and its version.
type nestControllerOf struct {
	paths   []string
	known   bool
	version *link.NestVersion
}

// nestController reads a class decorated with NestJS's `@Controller`, and
// false for any other class: `@Controller()` is the path `/`, a string or
// an array its paths, an options object its `path` (`/` when absent or
// empty) and its `version`.
func (s *routeScan) nestController(decorators []wire.Decorator) (nestControllerOf, bool) {
	for _, a := range decorators {
		n, ok := s.nestImported(a.Name)
		if !ok || n != "Controller" {
			continue
		}
		pos := positional(s.decoratorArgs(a))
		if len(pos) == 0 {
			return nestControllerOf{paths: []string{"/"}, known: true}, true
		}
		text := pos[0].text
		if !strings.HasPrefix(text, "{") {
			paths, known := s.nestStrings(text)
			return nestControllerOf{paths: paths, known: known}, true
		}
		props, ok := nestObject(text)
		if !ok {
			return nestControllerOf{}, true
		}
		c := nestControllerOf{paths: []string{"/"}, known: true}
		if p, ok := props["path"]; ok {
			c.paths, c.known = s.nestStrings(p)
			if c.known && len(c.paths) == 1 && c.paths[0] == "" && !strings.HasPrefix(strings.TrimSpace(p), "[") {
				// `prefixOrOptions.path || defaultPath`.
				c.paths = []string{"/"}
			}
		}
		if v, ok := props["version"]; ok {
			c.version = s.nestVersion(v)
		}
		return c, true
	}
	return nestControllerOf{}, false
}

// nestMethodVersion is the version a method's `@Version` names, nil for
// none.
func (s *routeScan) nestMethodVersion(decorators []wire.Decorator) *link.NestVersion {
	for _, a := range decorators {
		if n, ok := s.nestImported(a.Name); ok && n == "Version" {
			pos := positional(s.decoratorArgs(a))
			if len(pos) == 0 {
				return &link.NestVersion{}
			}
			return s.nestVersion(pos[0].text)
		}
	}
	return nil
}

// nestVersion is a version as written: a string, VERSION_NEUTRAL imported
// from `@nestjs/common`, or an array of them, each once; unknown otherwise.
func (s *routeScan) nestVersion(text string) *link.NestVersion {
	text = strings.TrimSpace(text)
	array := strings.HasPrefix(text, "[")
	v := &link.NestVersion{Known: true}
	for _, e := range elements(text) {
		var x string
		if name, ok := s.nestImported(e); ok && name == "VERSION_NEUTRAL" {
			x = link.NestNeutral
		} else if lit, ok := stringLiteral(s.b.lang.Name, e); ok {
			x = lit
		} else {
			return &link.NestVersion{}
		}
		if !array && x == "" {
			return v
		}
		if !slices.Contains(v.Values, x) {
			v.Values = append(v.Values, x)
		}
	}
	return v
}

// nestStrings are the strings a string literal or an array of them writes,
// and false when any element is no literal.
func (s *routeScan) nestStrings(text string) ([]string, bool) {
	var out []string
	for _, e := range elements(text) {
		v, ok := stringLiteral(s.b.lang.Name, e)
		if !ok {
			return nil, false
		}
		out = append(out, v)
	}
	return out, true
}

// nestObject is an object literal's properties, key to value as written,
// and false when any is no `key: value` pair (a shorthand, a spread, a
// computed key), which hides what the object holds.
func nestObject(text string) (map[string]string, bool) {
	text = strings.TrimSpace(text)
	if !strings.HasPrefix(text, "{") || !strings.HasSuffix(text, "}") {
		return nil, false
	}
	out := map[string]string{}
	for _, e := range splitTop(text[1 : len(text)-1]) {
		if e = strings.TrimSpace(e); e == "" {
			continue
		}
		key, value, ok := strings.Cut(e, ":")
		key = strings.Trim(strings.TrimSpace(key), `"'`)
		if !ok || key == "" || !isIdentText(key) {
			return nil, false
		}
		out[key] = strings.TrimSpace(value)
	}
	return out, true
}

// isIdentText reports a whole identifier.
func isIdentText(t string) bool {
	for i := 0; i < len(t); i++ {
		if !isIdent(t[i], i == 0) {
			return false
		}
	}
	return t != ""
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

// nestAny is the one empty method a route of no method (`@All`) is
// recorded under, none for a route that names one.
func nestAny(methods []string) []string {
	if len(methods) == 0 {
		return []string{""}
	}
	return nil
}
