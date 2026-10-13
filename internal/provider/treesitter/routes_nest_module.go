package treesitter

import (
	"strings"

	"github.com/Sawmonabo/codectx/internal/model"
	"github.com/Sawmonabo/codectx/internal/provider/link"
	"github.com/Sawmonabo/codectx/internal/provider/treesitter/wire"
)

// nestRegister is what jsTarget names `RouterModule.register`, the call
// that gives modules their RouterModule paths.
const nestRegister = "@nestjs/core.RouterModule.register"

// nestModules records on app the file's `@Module` classes (the decorator
// imported from `@nestjs/common`) with the controllers each lists, and
// marks app when a module's controllers are no literal list of names the
// file binds (link.NestApp.ModulesUnknown).
func (s *routeScan) nestModules(app *link.NestApp) {
	for i := range s.b.decls {
		d := &s.b.decls[i]
		if d.Parent >= 0 || d.kind != model.NodeClass {
			continue
		}
		for _, a := range d.Decorators {
			if n, ok := s.nestImported(a.Name); !ok || n != "Module" {
				continue
			}
			m, ok := s.nestModule(int32(i), positional(s.decoratorArgs(a)))
			switch {
			case !ok:
				app.ModulesUnknown = true
			case len(m.Controllers) > 0:
				app.Modules = append(app.Modules, m)
			}
			break
		}
	}
}

// nestModule reads `@Module(metadata)`: the names its `controllers` array
// lists, none when it has no such property; false when the metadata is no
// object literal or the array holds anything but names the file binds.
func (s *routeScan) nestModule(decl int32, pos []routeArg) (link.NestModule, bool) {
	m := link.NestModule{Decl: decl}
	if len(pos) == 0 {
		return m, true
	}
	props, ok := nestObject(pos[0].text)
	if !ok || len(pos) > 1 {
		return m, false
	}
	text, ok := props["controllers"]
	if !ok {
		return m, true
	}
	if !strings.HasPrefix(text, "[") {
		return m, false
	}
	for _, e := range elements(text) {
		r, ok := s.nestRef(e)
		if !ok {
			return m, false
		}
		m.Controllers = append(m.Controllers, r)
	}
	return m, true
}

// nestRef is the class a name written in the file denotes: the one
// module-level class it declares by that name, else the import binding it;
// false for any other expression, a namespace import, or a name the file
// does not bind.
func (s *routeScan) nestRef(text string) (link.NestRef, bool) {
	text = strings.TrimSpace(text)
	if !isIdentText(text) {
		return link.NestRef{}, false
	}
	found := -1
	for i := range s.b.decls {
		d := &s.b.decls[i]
		if d.Parent < 0 && d.Name == text {
			if found >= 0 || d.kind != model.NodeClass {
				return link.NestRef{}, false
			}
			found = i
		}
	}
	if found >= 0 {
		return link.NestRef{Local: int32(found)}, true
	}
	imp, ok := s.imports[text]
	if !ok || imp.original == "*" {
		return link.NestRef{}, false
	}
	name := imp.original
	if name == "" {
		name = text
	}
	return link.NestRef{Local: -1, Spec: imp.path, Name: name}, true
}

// nestRouters records on app each `RouterModule.register(routes)` call of
// the file: its route tree flattened (nestFlatten), read from an array
// literal in place or the one module-level binding the argument names.
func (s *routeScan) nestRouters(app *link.NestApp) {
	s.calls(func(ref *wire.Ref, site *wire.Site, i int) {
		if !s.nestRegisters(ref, site, i) {
			return
		}
		r := link.NestRouter{}
		if args := s.siteArgs(site); len(args) == 1 && args[0].kw == "" && len(site.Args) == 1 {
			if text, ok := s.nestTreeText(args[0].text); ok {
				var tree []*nestTree
				if tree, r.Known = s.nestTrees(text); r.Known {
					nestFlatten(tree, &r.Entries)
				}
			}
		}
		app.Routers = append(app.Routers, r)
	})
}

// nestRegisters reports a call of `RouterModule.register`: one jsTarget
// names, or, where the worker types no receiver (a call written in a
// decorator's arguments), a `register` call qualified by the name the
// file imports RouterModule from `@nestjs/core` by.
func (s *routeScan) nestRegisters(ref *wire.Ref, site *wire.Site, i int) bool {
	if s.jsTarget(site, i) == nestRegister {
		return true
	}
	if !ref.Qualified || ref.Name != "register" || !ref.QualifierIsImport || site.Receiver.Kind != wire.TypeUnknown {
		return false
	}
	imp, ok := s.imports[ref.Qualifier]
	return ok && imp.path == "@nestjs/core" && (imp.original == "RouterModule" || imp.original == "" && ref.Qualifier == "RouterModule")
}

// nestTreeText is the array literal a register call's argument writes: in
// place, or the initializer of the one module-level binding it names.
func (s *routeScan) nestTreeText(arg string) (string, bool) {
	if strings.HasPrefix(arg, "[") {
		return arg, true
	}
	if !isIdentText(arg) {
		return "", false
	}
	found := -1
	for i := range s.b.decls {
		d := &s.b.decls[i]
		if d.Parent < 0 && d.Name == arg {
			if found >= 0 || d.kind != model.NodeConstant && d.kind != model.NodeVariable {
				return "", false
			}
			found = i
		}
	}
	if found < 0 {
		return "", false
	}
	text := s.text(wire.Span{Start: s.b.decls[found].Start, End: s.b.decls[found].End})
	eq := strings.IndexByte(text, '=')
	if eq < 0 || eq+1 < len(text) && (text[eq+1] == '=' || text[eq+1] == '>') {
		return "", false
	}
	value := strings.TrimSuffix(strings.TrimSpace(text[eq+1:]), ";")
	if !strings.HasPrefix(value, "[") {
		return "", false
	}
	return strings.TrimSpace(value), true
}

// nestTree is one element of a RouterModule route tree: a route object's
// path (nil when it writes none), module and children, or a module class
// written bare (class).
type nestTree struct {
	path     *string
	module   *link.NestRef
	children []*nestTree
	class    *link.NestRef
}

// nestTrees reads a route tree array literal, false when any element is
// neither a route object of literal paths and class names nor a class name.
func (s *routeScan) nestTrees(text string) ([]*nestTree, bool) {
	if !strings.HasPrefix(text, "[") || !strings.HasSuffix(text, "]") {
		return nil, false
	}
	var out []*nestTree
	for _, e := range elements(text) {
		if !strings.HasPrefix(e, "{") {
			r, ok := s.nestRef(e)
			if !ok {
				return nil, false
			}
			out = append(out, &nestTree{class: &r})
			continue
		}
		props, ok := nestObject(e)
		if !ok {
			return nil, false
		}
		t := &nestTree{}
		if p, ok := props["path"]; ok {
			v, lit := stringLiteral(s.b.lang.Name, p)
			if !lit {
				return nil, false
			}
			t.path = &v
		}
		if m, ok := props["module"]; ok {
			r, ok := s.nestRef(m)
			if !ok {
				return nil, false
			}
			t.module = &r
		}
		if c, ok := props["children"]; ok {
			if t.children, ok = s.nestTrees(c); !ok {
				return nil, false
			}
			if t.children == nil {
				t.children = []*nestTree{}
			}
		}
		out = append(out, t)
	}
	return out, true
}

// nestFlatten is Nest's flattenRoutePaths (router/utils/flatten-route-
// paths.util.ts) over a read tree: a route object with a module and a
// non-empty path gives the module that path; a child object's path becomes
// its parent's joined to its own (normalized), and a child class takes its
// parent's path, before the children are flattened in turn. A child object
// with no path is pushed by Nest as a module itself, which names no class,
// and is dropped here.
func nestFlatten(routes []*nestTree, out *[]link.NestRouterEntry) {
	for _, item := range routes {
		if item.class != nil {
			continue
		}
		parent := ""
		if item.path != nil {
			parent = *item.path
		}
		if item.module != nil && parent != "" {
			*out = append(*out, link.NestRouterEntry{Path: parent, Module: *item.module})
		}
		if item.children == nil {
			continue
		}
		for _, child := range item.children {
			switch {
			case child.class == nil && child.path != nil:
				p := link.NestNormalize(link.NestNormalize(parent) + link.NestNormalize(*child.path))
				child.path = &p
			case child.class != nil:
				*out = append(*out, link.NestRouterEntry{Path: parent, Module: *child.class})
			}
		}
		nestFlatten(item.children, out)
	}
}
