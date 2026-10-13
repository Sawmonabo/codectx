package treesitter

import (
	"cmp"
	"slices"
	"strconv"

	"github.com/Sawmonabo/codectx/internal/model"
	"github.com/Sawmonabo/codectx/internal/provider/link"
)

// Request inputs of handlers named across files (docs/providers-link.md,
// Request inputs across files): a function no route of its own file names
// may be a handler a route of another file names through an import. Its
// file records the reads of its first parameter under each request table
// its language has, and its routes' files record the handlers they name
// through imports; the linker joins the two.

// handlerTables are, by language, the frameworks whose handler receives
// the request as its first parameter, with their request tables.
func handlerTables(language string) []struct {
	fw    string
	table inputTable
} {
	switch {
	case language == "python":
		return []struct {
			fw    string
			table inputTable
		}{{fwDjango, djangoRequest}, {fwStarlette, starletteRequest}}
	case jsLanguage(language):
		return []struct {
			fw    string
			table inputTable
		}{{fwExpress, expressRequest}}
	}
	return nil
}

// handlerCandidates records, for each module-level function of the file,
// the reads of its first parameter under each framework's request table
// (handlerTables), as pending reads the linker publishes only when a route
// of another file names the function as a handler of that framework
// (link.RuleHandler). A read the file's own scans already published is
// not recorded again. It runs after every other scan of the file.
func (s *inputScan) handlerCandidates() {
	tables := handlerTables(s.b.lang.Name)
	if len(tables) == 0 {
		return
	}
	for i := range s.b.decls {
		d := &s.b.decls[i]
		if d.Parent >= 0 || d.kind != model.NodeFunction {
			continue
		}
		s.lex()
		ordinal := map[string]int{}
		for _, tb := range tables {
			n := len(s.out)
			s.firstParamReads(tb.fw, tb.table, d.NameEnd, d.Start, d.End, false)
			reads := slices.Clone(s.out[n:])
			s.out = s.out[:n]
			slices.SortFunc(reads, func(x, y inputRead) int { return cmp.Compare(x.at.Start, y.at.Start) })
			for _, r := range reads {
				// Another table may read the same expression.
				delete(s.seen, r.at)
				rng, err := s.b.rangeOf(r.at.Start, r.at.End)
				if err != nil {
					continue
				}
				id := tb.fw + ":" + string(r.input.Source) + ":" + r.input.Key
				ordinal[id]++
				var cut truncations
				name := cut.cut("name", collapse(string(s.b.src[r.at.Start:r.at.End])), model.MaxNameBytes)
				s.b.pendingInputs = append(s.b.pendingInputs, link.PendingInput{Owner: int32(i), Range: *rng, Name: name,
					Framework: tb.fw, Param: id + "#" + strconv.Itoa(ordinal[id]), Rule: link.RuleHandler,
					Source: r.input.Source, Key: r.input.Key})
			}
		}
	}
}

// routeHandlerNames are the handlers the file's routes name through an
// import (importedHandlers), each with its route's framework, once.
func (s *routeScan) routeHandlerNames() []link.RouteHandler {
	var out []link.RouteHandler
	add := func(fw string, hs []handler) {
		for _, raw := range s.importedHandlers(hs) {
			out = append(out, link.RouteHandler{Framework: fw, Imported: raw})
		}
	}
	for i := range s.out {
		add(s.out[i].meta.Framework, s.out[i].handlers)
	}
	for _, e := range s.djangoEntries {
		add(fwDjango, e.handlers)
	}
	slices.SortFunc(out, func(a, b link.RouteHandler) int {
		return cmp.Or(cmp.Compare(a.Framework, b.Framework), cmp.Compare(a.Imported, b.Imported))
	})
	return slices.Compact(out)
}
