package treesitter

import (
	"strings"

	"github.com/Sawmonabo/codectx/internal/model"
	"github.com/Sawmonabo/codectx/internal/provider/treesitter/wire"
)

// springAnnotations is the package of Spring's request mapping annotations.
const springAnnotations = "org.springframework.web.bind.annotation"

// springMappings are Spring's request mapping annotations, each with the
// method it names; `RequestMapping` names its methods in `method =`.
var springMappings = map[string]string{
	"GetMapping": "GET", "PostMapping": "POST", "PutMapping": "PUT", "DeleteMapping": "DELETE",
	"PatchMapping": "PATCH", "RequestMapping": "",
}

// springMethods are the constants of Spring's RequestMethod.
var springMethods = map[string]bool{
	"GET": true, "HEAD": true, "POST": true, "PUT": true, "PATCH": true, "DELETE": true, "OPTIONS": true, "TRACE": true,
}

// spring reads the file's Spring request mappings: each mapping annotation
// on a method is one endpoint per route and method it names, routed to the
// method. The class's own `@RequestMapping` is the only prefix: a class with
// none has no prefix, one with a single literal route resolves, and any
// other (a constant, several routes) leaves the prefix unresolved.
func (s *routeScan) spring() {
	if !s.frameworks[fwSpring] {
		return
	}
	for i := range s.b.decls {
		d := &s.b.decls[i]
		if d.kind != model.NodeMethod {
			continue
		}
		prefix, resolved, class := "", true, ""
		if d.Parent >= 0 {
			p := &s.b.decls[d.Parent]
			class = p.Name
			prefix, resolved = s.springPrefix(p.Decorators)
		}
		for _, a := range d.Decorators {
			method, ok := springMappings[springName(a.Name)]
			if !ok {
				continue
			}
			args := s.decoratorArgs(a)
			methods := []string{method}
			if method == "" {
				methods = s.springRequestMethods(args)
			}
			at := wire.Span{Start: a.Start, End: a.End}
			for _, r := range s.springRoutes(args) {
				s.emitPrefixed(fwSpring, methods, r, prefix, resolved, at, []handler{declHandler(i)}, class)
			}
		}
	}
}

// springName is a mapping annotation's simple name, or empty when it is
// qualified by a package other than Spring's.
func springName(name string) string {
	if rest, ok := strings.CutPrefix(name, springAnnotations+"."); ok {
		return rest
	}
	if strings.Contains(name, ".") {
		return ""
	}
	return name
}

// springRoutes are the routes a mapping names in `value`, its alias `path`,
// or its single positional element, one per element of an array; a mapping
// that names none maps the empty route, written `""`.
func (s *routeScan) springRoutes(args []routeArg) []routeText {
	a, ok := argument(args, 0, "value")
	if !ok {
		a, ok = argument(args, -1, "path")
	}
	if !ok {
		return []routeText{{written: `""`, literal: true}}
	}
	var out []routeText
	for _, e := range elements(a.text) {
		v, lit := stringLiteral("java", e)
		out = append(out, routeText{written: e, value: v, literal: lit})
	}
	return out
}

// springPrefix is the class-level `@RequestMapping` prefix: empty and
// resolved when the class has none, its route when it names one literal,
// unresolved otherwise.
func (s *routeScan) springPrefix(decorators []wire.Decorator) (string, bool) {
	for _, a := range decorators {
		if springName(a.Name) != "RequestMapping" {
			continue
		}
		routes := s.springRoutes(s.decoratorArgs(a))
		if len(routes) != 1 || !routes[0].literal {
			return "", false
		}
		return routes[0].value, true
	}
	return "", true
}

// springRequestMethods are the methods a `@RequestMapping` names in
// `method =`: none means any method; one that is not a RequestMethod
// constant leaves the endpoint with no method.
func (s *routeScan) springRequestMethods(args []routeArg) []string {
	a, ok := argument(args, -1, "method")
	if !ok {
		return nil
	}
	var out []string
	for _, e := range elements(a.text) {
		m := strings.TrimPrefix(strings.TrimPrefix(e, springAnnotations+"."), "RequestMethod.")
		if !springMethods[m] {
			return nil
		}
		out = append(out, m)
	}
	return out
}
