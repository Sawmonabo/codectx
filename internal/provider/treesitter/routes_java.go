package treesitter

import (
	"slices"
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
// none has no prefix; each literal route it names is a prefix, and a method's
// routes are mapped below every one of them (Spring combines the class's and
// the method's patterns pairwise); a route that is not a literal leaves the
// prefix unresolved. The class's `method =` adds to each method's own
// methods (Spring combines them as a union), and a method mapping naming
// none takes the class's.
func (s *routeScan) spring() {
	if !s.frameworks[fwSpring] {
		return
	}
	for i := range s.b.decls {
		d := &s.b.decls[i]
		if d.kind != model.NodeMethod {
			continue
		}
		cls := springClass{prefixes: []string{""}, resolved: true, methodsKnown: true}
		class := ""
		if d.Parent >= 0 {
			p := &s.b.decls[d.Parent]
			class = p.Name
			cls = s.springClassMapping(p.Decorators)
		}
		for _, a := range d.Decorators {
			method, ok := springMappings[springName(a.Name)]
			if !ok {
				continue
			}
			args := s.decoratorArgs(a)
			methods, known := []string{method}, true
			if method == "" {
				methods, known = s.springRequestMethods(args)
			}
			methods = springUnion(cls, methods, known)
			at := wire.Span{Start: a.Start, End: a.End}
			for _, prefix := range cls.prefixes {
				receiver := class
				if len(cls.prefixes) > 1 {
					receiver = class + " " + prefix
				}
				for _, r := range s.springRoutes(args) {
					s.emitPrefixed(fwSpring, methods, r, prefix, cls.resolved, at, []handler{declHandler(i)}, receiver)
				}
			}
		}
	}
}

// springClass is what a class-level `@RequestMapping` gives its methods'
// mappings: its literal prefixes (one empty prefix for none), resolved
// false when one is not a literal; and its methods, nil for none,
// methodsKnown false when one is not a RequestMethod constant.
type springClass struct {
	prefixes     []string
	resolved     bool
	methods      []string
	methodsKnown bool
}

// springUnion is a method mapping's methods combined with its class's: the
// union, in the method's order then the class's; none when either names
// one that is not a constant, or when both name none (any method).
func springUnion(cls springClass, methods []string, known bool) []string {
	if !known || !cls.methodsKnown {
		return nil
	}
	if len(methods) == 1 && methods[0] == "" {
		methods = nil
	}
	if len(methods) == 0 || len(cls.methods) == 0 {
		// A mapping naming no method takes any; the class's restriction
		// is then the whole set, and a class naming none adds nothing.
		if len(methods) == 0 {
			return cls.methods
		}
		return methods
	}
	out := slices.Clone(methods)
	for _, m := range cls.methods {
		if !slices.Contains(out, m) {
			out = append(out, m)
		}
	}
	return out
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

// springClassMapping is the class-level `@RequestMapping` of a class's
// annotations.
func (s *routeScan) springClassMapping(decorators []wire.Decorator) springClass {
	for _, a := range decorators {
		if springName(a.Name) != "RequestMapping" {
			continue
		}
		args := s.decoratorArgs(a)
		cls := springClass{resolved: true}
		for _, r := range s.springRoutes(args) {
			if !r.literal {
				cls.prefixes, cls.resolved = []string{""}, false
				break
			}
			cls.prefixes = append(cls.prefixes, r.value)
		}
		cls.methods, cls.methodsKnown = s.springRequestMethods(args)
		return cls
	}
	return springClass{prefixes: []string{""}, resolved: true, methodsKnown: true}
}

// springRequestMethods are the methods a `@RequestMapping` names in
// `method =`, nil for none (any method), and false when one is not a
// RequestMethod constant.
func (s *routeScan) springRequestMethods(args []routeArg) ([]string, bool) {
	a, ok := argument(args, -1, "method")
	if !ok {
		return nil, true
	}
	var out []string
	for _, e := range elements(a.text) {
		m := strings.TrimPrefix(strings.TrimPrefix(e, springAnnotations+"."), "RequestMethod.")
		if !springMethods[m] {
			return nil, false
		}
		out = append(out, m)
	}
	return out, true
}
