package treesitter

import (
	"bytes"
	"strings"

	"github.com/Sawmonabo/codectx/internal/provider/link"
	"github.com/Sawmonabo/codectx/internal/provider/treesitter/wire"
)

// nestFactory is what jsTarget names `NestFactory.create`, the call that
// creates a NestJS HTTP application.
const nestFactory = "@nestjs/core.NestFactory.create"

// nestLinks is the file's open routing, out (nil for none), with the
// file's NestJS routes, each on a mount of its own located at its decorator
// (link.RouteMount.Nest), and what the file sets on the applications it
// creates (nestAppScan).
func (s *routeScan) nestLinks(out *routeLinks) *routeLinks {
	if !s.frameworks[fwNest] {
		return out
	}
	app := s.nestAppScan()
	if len(s.nestOut) == 0 && app == nil {
		return out
	}
	if out == nil {
		out = &routeLinks{}
	}
	for _, r := range s.nestOut {
		rng, err := s.b.rangeOf(r.at.Start, r.at.End)
		if err != nil {
			continue
		}
		out.routing.Nest = append(out.routing.Nest, r.NestRoute)
		out.routing.Mounts = append(out.routing.Mounts, link.RouteMount{Site: -1, Arg: -1, Framework: fwNest, Object: -1,
			Range: *rng, Nest: int32(len(out.routing.Nest))})
		out.sites = append(out.sites, -1)
	}
	out.routing.NestApp = app
	return out
}

// nestOnly is the routing of a file that imports no framework but may set
// a NestJS application's global prefix or versioning.
func (s *routeScan) nestOnly() *routeLinks {
	app := s.nestAppScan()
	if app == nil {
		return nil
	}
	return &routeLinks{routing: link.Routing{NestApp: app}}
}

// mayConfigureNest reports an ECMAScript file whose bytes name a setting of
// a NestJS application, which only its calls tell apart from any other
// method of that name.
func mayConfigureNest(b *builder) bool {
	switch b.lang.Name {
	case "javascript", "typescript", "tsx":
		return bytes.Contains(b.src, []byte("setGlobalPrefix")) || bytes.Contains(b.src, []byte("enableVersioning"))
	}
	return false
}

// nestAppScan is what the file sets on NestJS applications (link.NestApp),
// nil when it creates none and sets nothing: each `NestFactory.create`
// call, and each `setGlobalPrefix` and `enableVersioning` call by the
// application its receiver is. A call on an application the file creates
// applies to it; one on a testing application (the result of a
// `createNestApplication` call, or any application a file importing
// `@nestjs/testing` does not create by NestFactory), which serves no
// deployment, is not read;
// any other receiver is an application the file does not create, which no
// table ties to one (Open). A file naming RouterModule from `@nestjs/core`
// is marked, with its register calls (nestRouters), and the file's
// `@Module` classes are recorded with their controllers (nestModules).
func (s *routeScan) nestAppScan() *link.NestApp {
	app := &link.NestApp{}
	factories := map[int]bool{}
	testing := map[int]bool{}
	// A file importing `@nestjs/testing` builds testing applications; one
	// it does not create by NestFactory is one of them, however the
	// variable holding it is assigned.
	testingFile := false
	for _, imp := range s.imports {
		testingFile = testingFile || imp.path == "@nestjs/testing"
	}
	s.calls(func(r *wire.Ref, site *wire.Site, i int) {
		switch {
		case s.jsTarget(site, i) == nestFactory:
			factories[i] = true
			app.Apps++
		case r.Qualified && r.Name == "createNestApplication":
			testing[i] = true
		}
	})
	s.calls(func(r *wire.Ref, site *wire.Site, _ int) {
		if !r.Qualified || r.Name != "setGlobalPrefix" && r.Name != "enableVersioning" {
			return
		}
		on := s.resultSite(site.Receiver)
		switch {
		case testing[on] || testingFile && !factories[on]:
			return
		case !factories[on]:
			app.Open = true
			return
		}
		args := s.siteArgs(site)
		if r.Name == "setGlobalPrefix" {
			p := link.NestPrefix{}
			if len(args) == 1 && args[0].kw == "" {
				p.Value, p.Known = stringLiteral(s.b.lang.Name, args[0].text)
			}
			app.Prefixes = append(app.Prefixes, p)
			return
		}
		app.Versioning = append(app.Versioning, s.nestVersioning(args, len(site.Args)))
	})
	for name, imp := range s.imports {
		if imp.path == "@nestjs/core" && (imp.original == "RouterModule" || imp.original == "" && name == "RouterModule") {
			app.RouterModule = true
		}
	}
	s.nestModules(app)
	if app.RouterModule {
		s.nestRouters(app)
	}
	if app.Apps == 0 && !app.Open && !app.RouterModule && len(app.Modules) == 0 && !app.ModulesUnknown {
		return nil
	}
	return app
}

// nestVersioning reads `enableVersioning([options])`: no argument is URI
// versioning (the method's default), an object literal its `type`
// (VersioningType imported from `@nestjs/common`), `prefix` and
// `defaultVersion`; anything else is unknown. n is the call's argument
// count, which the positions sent must match.
func (s *routeScan) nestVersioning(args []routeArg, n int) link.NestVersioning {
	if n == 0 {
		return link.NestVersioning{Known: true, URI: true, Prefix: "v"}
	}
	if len(args) != 1 || args[0].kw != "" {
		return link.NestVersioning{}
	}
	props, ok := nestObject(args[0].text)
	if !ok {
		return link.NestVersioning{}
	}
	v := link.NestVersioning{Known: true, Prefix: "v"}
	typ, member, ok := strings.Cut(props["type"], ".")
	if name, imported := s.nestImported(strings.TrimSpace(typ)); !ok || !imported || name != "VersioningType" {
		return link.NestVersioning{}
	}
	switch strings.TrimSpace(member) {
	case "URI":
		v.URI = true
	case "HEADER", "MEDIA_TYPE", "CUSTOM":
	default:
		return link.NestVersioning{}
	}
	if p, ok := props["prefix"]; ok {
		switch lit, isLit := stringLiteral(s.b.lang.Name, p); {
		case isLit:
			v.Prefix = lit
		case p == "false":
			v.Prefix = ""
		default:
			return link.NestVersioning{}
		}
	}
	if d, ok := props["defaultVersion"]; ok {
		v.Default = s.nestVersion(d)
	}
	return v
}
