package treesitter

import (
	"encoding/json"
	"strings"

	"github.com/Sawmonabo/codectx/internal/model"
	"github.com/Sawmonabo/codectx/internal/provider/treesitter/lang"
	"github.com/Sawmonabo/codectx/internal/provider/treesitter/wire"
	"github.com/Sawmonabo/codectx/internal/reconcile"
)

// endpointFact is one endpoint of the file, a program entry point (entry.go)
// or a framework route (routes.go), before it is published: an `endpoint`
// node that `routes_to` each of its handlers (ADR-0017 decision 8).
type endpointFact struct {
	// meta is the endpoint's metadata; it is empty for a program entry point.
	meta model.EndpointMetadata
	// descriptor is the endpoint's descriptor path, which with the file and
	// the kind is its identity, never its range: the method and the route as
	// written ("GET /users", or "/users" for a route that names no method),
	// or the entry point's form ("__main__", "main", "script").
	descriptor string
	// disambiguator separates two endpoints of the file that share a
	// descriptor and are told apart by what they are, not by content: the
	// class of a Java `main`, the receiver of a route registered on two
	// routers. Exact duplicates left after it are settled by content
	// (reconcile.SettleDuplicates), as declarations are.
	disambiguator string
	// at is where the endpoint is declared: the route's decorator or
	// annotation, its registration call, the `__main__` guard, the entry
	// function's declaration, the shebang line.
	at wire.Span
	// handlers are the targets the endpoint routes to, in source order.
	handlers []handler
	// isDecl reports an endpoint the worker extracted as a declaration (a
	// `__main__` guard, so the calls its body makes are made from it): its
	// node is declaration decl's, already published with its containment and
	// search document, and only its routes_to relations are added here.
	isDecl bool
	decl   int
}

// handler is one target an endpoint routes to: exactly one of a declaration
// of this file, the file's module, a name this file does not declare
// exactly once, or a function literal the endpoint registers in place.
type handler struct {
	// decl is a declaration of this file by ordinal, or -1.
	decl int
	// module routes to the file's module node: a script runs from its top.
	module bool
	// ref is a handler named by a reference: a name, or a receiver and a
	// name (`views.index`), its token and its range, spelled as a call
	// reference is, so it names the node a call of that name would.
	ref *wire.Ref
	// fn is a function literal the endpoint registers in place (an Express
	// or a Go handler written as the argument). It is published as a
	// function node keyed by its range, the identity of an anonymous
	// function, contained by the declaration around it.
	fn *wire.Span
}

// declHandler, moduleHandler, refHandler and fnHandler build each form.
func declHandler(i int) handler      { return handler{decl: i} }
func moduleHandler() handler         { return handler{decl: -1, module: true} }
func refHandler(r wire.Ref) handler  { r.Kind = "call"; return handler{decl: -1, ref: &r} }
func fnHandler(at wire.Span) handler { return handler{decl: -1, fn: &at} }

// endpoints publishes every endpoint the file declares: the program entry
// points and the framework routes. Each is one `endpoint` node, defined by
// the file's module, keyed by the file, its kind and its descriptor, with
// its metadata, one `routes_to` to each handler and a search document, so a
// kind filter on endpoints finds it. An endpoint whose descriptor or route
// does not fit its bound is not published and is counted, so the file
// reports partial.
func (b *builder) endpoints() error {
	facts := append(b.entryPoints(), b.routeTable()...)
	if len(facts) == 0 {
		return nil
	}
	declared := make([]reconcile.Declared, len(facts))
	for i, f := range facts {
		if f.isDecl {
			continue
		}
		if f.at.End > uint32(len(b.src)) || f.at.Start > f.at.End {
			return outputInvalid("an endpoint's range does not fit the pinned bytes")
		}
		declared[i] = reconcile.Declared{Kind: model.NodeEndpoint, Descriptor: f.descriptor, Disambiguator: f.disambiguator,
			Start: uint64(f.at.Start), End: uint64(f.at.End), Content: reconcile.ContentDigest(b.src[f.at.Start:f.at.End])}
	}
	settled := reconcile.SettleDuplicates(declared)
	for i := range facts {
		f := &facts[i]
		if f.isDecl {
			if err := b.routeDecl(f); err != nil {
				return err
			}
			continue
		}
		if f.descriptor != "" {
			f.disambiguator = settled[i]
		}
		if err := b.publishEndpoint(f); err != nil {
			return err
		}
	}
	return nil
}

// routeDecl adds the routes_to relations of an endpoint that is a
// declaration of the file.
func (b *builder) routeDecl(f *endpointFact) error {
	if f.decl < 0 || f.decl >= len(b.decls) || b.decls[f.decl].kind != model.NodeEndpoint {
		return &model.Error{Code: model.CodeInternal, Message: "an endpoint names a declaration that is not an endpoint"}
	}
	d := &b.decls[f.decl]
	for _, h := range f.handlers {
		to, ok, err := b.handlerNode(h, d.Name, d.rng)
		if err != nil {
			return err
		}
		if ok {
			b.putRelation(structure, d.res.Node.ID, model.RelRoutesTo, to, d.rng, d.Qualified, "")
		}
	}
	return nil
}

// publishEndpoint publishes one endpoint and its routes_to relations.
func (b *builder) publishEndpoint(f *endpointFact) error {
	if err := f.meta.Validate(); err != nil {
		// The route and the path are the source's own text and may exceed
		// their bounds; any other failure is this provider's defect.
		if len(f.meta.Route) > model.MaxPathBytes || len(f.meta.Path) > model.MaxPathBytes {
			b.dropped[structure]++
			return nil
		}
		return &model.Error{Code: model.CodeInternal, Message: "endpoint metadata: " + err.Error()}
	}
	rng, err := b.rangeOf(f.at.Start, f.at.End)
	if err != nil {
		return err
	}
	var cut truncations
	name := cut.cut("name", f.descriptor, model.MaxNameBytes)
	descriptor, dis := f.descriptor, f.disambiguator
	if len(descriptor) > model.MaxQualifiedNameBytes || len(dis) > model.MaxQualifiedNameBytes {
		// A descriptor over its bound is a prefix, not a path: the
		// endpoint is keyed by its range, as a declaration whose qualified
		// name was cut is.
		descriptor, dis = "", ""
	}
	key := "endpoint:" + f.descriptor
	if f.disambiguator != "" {
		key += "#" + f.disambiguator
	}
	if descriptor == "" {
		key = b.rangeKey("endpoint:", f.at.Start, f.at.End)
	}
	if key == "" || len(key) > model.MaxNativeKeyBytes {
		b.dropped[structure]++
		return nil
	}
	qualified := cut.cut("qualified_name", f.descriptor, model.MaxQualifiedNameBytes)
	res, err := b.resolve(model.NodeCandidate{ProviderID: lang.ProviderID, ScopeKey: b.scope, NativeKey: key,
		Kind: model.NodeEndpoint, Language: b.lang.Name, Name: name, QualifiedName: qualified,
		FileID: b.fv.ID, ContentHash: b.fv.ContentHash, Range: rng, Descriptor: descriptor, Disambiguator: dis})
	if err != nil {
		return err
	}
	meta := map[string]any{}
	if raw, err := json.Marshal(f.meta); err == nil {
		_ = json.Unmarshal(raw, &meta)
	}
	if len(cut) > 0 {
		meta["truncated_fields"] = cut
	}
	first := b.putNode(structure, res, rng, key, meta)
	b.putRelation(structure, b.module.Node.ID, model.RelDefines, res.Node.ID, rng, key, "")
	for _, h := range f.handlers {
		to, ok, err := b.handlerNode(h, f.descriptor, rng)
		if err != nil {
			return err
		}
		if ok {
			b.putRelation(structure, res.Node.ID, model.RelRoutesTo, to, rng, key, "")
		}
	}
	if first {
		body := strings.Join(strings.Fields(strings.Join([]string{f.meta.Framework, f.meta.Method, f.meta.Route, f.meta.Path}, " ")), " ")
		if body == "" {
			body = name
		}
		body = cut.cut("body", body, maxDocBytes)
		b.search = append(b.search, model.SearchUnit{
			ID: model.H(searchDomain, string(b.fv.ID), string(res.Node.ID)), NodeID: res.Node.ID, FileID: b.fv.ID, Path: b.fv.Path,
			Kind: model.NodeEndpoint, Name: name, QualifiedName: qualified,
			Bytes: model.ByteRange{Start: uint64(f.at.Start), End: uint64(f.at.End)}, Body: body, TokenCount: int64(len(strings.Fields(body))),
		})
	}
	return nil
}

// handlerNode is the node one handler names, minting it when it is a name
// this file does not declare exactly once or a function literal, and false
// when a bound kept it out (counted).
func (b *builder) handlerNode(h handler, descriptor string, at *model.SourceRange) (model.NodeID, bool, error) {
	switch {
	case h.decl >= 0:
		if h.decl >= len(b.decls) {
			return "", false, &model.Error{Code: model.CodeInternal, Message: "an endpoint names a handler that is not a declaration"}
		}
		return b.decls[h.decl].res.Node.ID, true, nil
	case h.module:
		return b.module.Node.ID, true, nil
	case h.ref != nil:
		return b.handlerRef(*h.ref)
	case h.fn != nil:
		return b.handlerLiteral(*h.fn, descriptor)
	}
	return "", false, nil
}

// handlerRef names a handler written as a name the way a call of that name
// is resolved, and publishes the Section 11.3 call-site alias on its token
// so a semantic provider's reference occurrence at the same bytes joins it.
func (b *builder) handlerRef(r wire.Ref) (model.NodeID, bool, error) {
	if r.Name == "" || len(r.Name) > model.MaxNameBytes || len(r.Qualifier) > wire.MaxQualifierBytes {
		b.dropped[structure]++
		return "", false, nil
	}
	rng, err := b.rangeOf(r.NameStart, r.NameEnd)
	if err != nil {
		return "", false, err
	}
	targets := b.callTargets(r, b.targets(r))
	id, _, ok, err := b.callee(r, targets, rng)
	if err != nil || !ok {
		return id, ok, err
	}
	if key := b.callsiteKey(r.NameStart, r.NameEnd); key != "" {
		b.putAlias(b.scope, key, id)
	} else {
		b.dropped[structure]++
	}
	return id, true, nil
}

// handlerLiteral publishes a function literal an endpoint registers in
// place as a function node named by the endpoint it handles, keyed by its
// range (it has no name, so no descriptor path), contained by the innermost
// declaration around it or defined by the module.
func (b *builder) handlerLiteral(at wire.Span, descriptor string) (model.NodeID, bool, error) {
	rng, err := b.rangeOf(at.Start, at.End)
	if err != nil {
		return "", false, err
	}
	key := b.rangeKey("fn:", at.Start, at.End)
	if key == "" {
		b.dropped[structure]++
		return "", false, nil
	}
	var cut truncations
	name := cut.cut("name", descriptor, model.MaxNameBytes)
	res, err := b.resolve(model.NodeCandidate{ProviderID: lang.ProviderID, ScopeKey: b.scope, NativeKey: key,
		Kind: model.NodeFunction, Language: b.lang.Name, Name: name, FileID: b.fv.ID, ContentHash: b.fv.ContentHash, Range: rng})
	if err != nil {
		return "", false, err
	}
	meta := map[string]any{}
	if len(cut) > 0 {
		meta["truncated_fields"] = cut
	}
	b.putNode(structure, res, rng, key, meta)
	parent, kind := b.module.Node.ID, model.RelDefines
	if i := b.innermost(at); i >= 0 {
		parent, kind = b.decls[i].res.Node.ID, model.RelContains
	}
	b.putRelation(structure, parent, kind, res.Node.ID, rng, key, "")
	return res.Node.ID, true, nil
}

// innermost is the innermost declaration of the file whose range contains
// at, or -1. Declarations are in source order, outer before inner, so the
// last one containing at is the innermost.
func (b *builder) innermost(at wire.Span) int {
	found := -1
	for i := range b.decls {
		d := &b.decls[i]
		if d.Start > at.Start {
			break
		}
		if d.Start <= at.Start && at.End <= d.End {
			found = i
		}
	}
	return found
}
