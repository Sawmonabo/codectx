package treesitter

import (
	"slices"

	"github.com/Sawmonabo/codectx/internal/model"
	"github.com/Sawmonabo/codectx/internal/provider/treesitter/wire"
)

// appFrameworks are the frameworks whose application object is a program
// entry point when the module binds it: what a WSGI or ASGI server, or
// Node, is pointed at (PEP 3333, the ASGI specification, Express's
// `express()`), found by content, never by the file's name.
var appFrameworks = map[string]bool{fwFlask: true, fwFastAPI: true, fwStarlette: true, fwExpress: true}

// applications adds one `endpoint`, descriptor "application", for each
// application object a module-level binding holds: a root object a
// framework of appFrameworks builds (`app = Flask(__name__)`,
// `const app = express()`) and a Django application factory's result
// (`application = get_wsgi_application()`). The endpoint routes_to the
// binding's declaration, so "what serves this module" is a kind filter.
// Two in one file share the descriptor and are told apart by the binding's
// name. An object built inside a function, or a call that is only part of
// the binding's value (`app = wrap(Flask(__name__))`), is no module-level
// application object and publishes nothing.
func (s *routeScan) applications() {
	sites := make([]int, 0, len(s.objs)+len(s.apps))
	for i, o := range s.objs {
		if o.root && appFrameworks[o.fw] {
			sites = append(sites, i)
		}
	}
	sites = append(sites, s.apps...)
	slices.Sort(sites)
	for _, i := range slices.Compact(sites) {
		r := s.siteRef(i)
		if r == nil {
			continue
		}
		d := s.binding(refSpan(r))
		if d < 0 {
			continue
		}
		f := routeFact{receiver: s.b.decls[d].Name}
		f.descriptor, f.at, f.handlers = "application", refSpan(r), []handler{declHandler(d)}
		s.out = append(s.out, f)
	}
}

// binding is the module-level variable or constant declaration whose value
// is exactly the call at (the declaration ends where the call does), or -1.
func (s *routeScan) binding(at wire.Span) int {
	for i := range s.b.decls {
		d := &s.b.decls[i]
		if d.Start > at.Start {
			break
		}
		if d.Parent < 0 && (d.kind == model.NodeVariable || d.kind == model.NodeConstant) &&
			d.Start <= at.Start && at.End == d.End {
			return i
		}
	}
	return -1
}
