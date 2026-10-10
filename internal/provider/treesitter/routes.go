package treesitter

import (
	"cmp"
	"strings"
	"unicode/utf8"

	"github.com/Sawmonabo/codectx/internal/model"
	"github.com/Sawmonabo/codectx/internal/provider/treesitter/wire"
)

// routeTable is the file's framework routes (ADR-0017 decision 8): a
// per-framework table, read syntactically over the file's decorators and
// annotations, its call sites, their arguments and the receiver and argument
// types the worker recovered inside the file. It carries no rule of any one
// repository.
//
// A framework's routes are read only when the file imports it (routeImports);
// a decorator or a call that is not one of the framework's route forms yields
// nothing. Each route is one endpoint per method and per route it declares:
// its route as written, its method when it names one, and its prefix
// resolved, with the full path, only when every prefix between the route and
// an application root is visible in this file as a literal (routeObj). A
// route registered by a call needs the call's argument positions
// (wire.Site.Arguments); without them it is not published, never guessed. A
// decorator or annotation route reads the decorator's own argument spans.
//
// Two endpoints that share a descriptor on different receivers are told apart
// by the receiver as written; exact duplicates are settled by content in
// endpoints.
func (b *builder) routeTable() []endpointFact {
	s := newRouteScan(b)
	if s == nil {
		return nil
	}
	switch b.lang.Name {
	case "python":
		s.python()
	case "javascript", "typescript", "tsx":
		s.express()
	case "go":
		s.goRoutes()
	case "java":
		s.spring()
	}
	return s.facts()
}

// routeScan is the state of one file's route table.
type routeScan struct {
	b *builder
	// link is the worker's link tables, nil when the file's dependence or
	// link pass failed: no receiver, argument type or argument position is
	// then known, so no call-registered route is read and every decorator
	// route's prefix is unresolved.
	link *wire.Link
	// siteAt is each worker site's index by its callee token, the first at a
	// token as linkTable joins them; refAt each call reference's index by its
	// callee token, the first likewise.
	siteAt map[wire.Span]int
	refAt  map[wire.Span]int
	// imports maps each local name an import binds to the import, and
	// frameworks holds every framework the file imports.
	imports    map[string]routeImport
	frameworks map[string]bool
	// objs are the application and router objects this file builds, by the
	// worker index of the site that builds them.
	objs map[int]*routeObj
	// methodsOn is, for gorilla/mux, the `.Methods(...)` site called on each
	// site's result, by the worker index of the site it is called on.
	methodsOn map[int]int
	out       []routeFact
}

// routeImport is one local name an import binds: the module path as written
// and the name it denotes there, empty when it is the local name.
type routeImport struct{ path, original string }

// routeFact is one route before identities are settled: the endpoint and
// the receiver it was registered on, as written.
type routeFact struct {
	endpointFact
	receiver string
}

// newRouteScan indexes the file's sites, call references and imports, or is
// nil when the file imports no framework the table reads.
func newRouteScan(b *builder) *routeScan {
	s := &routeScan{b: b, link: b.ex.link, imports: map[string]routeImport{}, frameworks: map[string]bool{},
		objs: map[int]*routeObj{}}
	s.routeImports()
	if len(s.frameworks) == 0 {
		return nil
	}
	s.refAt = make(map[wire.Span]int, len(b.ex.refs))
	for i, r := range b.ex.refs {
		if r.Kind != "call" {
			continue
		}
		k := wire.Span{Start: r.NameStart, End: r.NameEnd}
		if _, dup := s.refAt[k]; !dup {
			s.refAt[k] = i
		}
	}
	if s.link != nil {
		s.siteAt = make(map[wire.Span]int, len(s.link.Sites))
		for i, site := range s.link.Sites {
			if _, dup := s.siteAt[site.Name]; !dup {
				s.siteAt[site.Name] = i
			}
		}
	}
	return s
}

// routeImports reads the file's imports: the local names each binds and the
// frameworks it names.
func (s *routeScan) routeImports() {
	if s.b.lang.Name == "java" {
		// A mapping annotation written with its package is evidence of
		// Spring without an import.
		for i := range s.b.decls {
			for _, d := range s.b.decls[i].Decorators {
				if strings.HasPrefix(d.Name, springAnnotations+".") {
					s.frameworks[fwSpring] = true
				}
			}
		}
	}
	for _, imp := range s.b.ex.imports {
		if fw := frameworkOf(s.b.lang.Name, imp.Path); fw != "" {
			s.frameworks[fw] = true
		}
		names := imp.Names
		if s.b.lang.Name == "go" {
			names = []string{goImportName(imp)}
		}
		for i, n := range names {
			ri := routeImport{path: imp.Path}
			if i < len(imp.Originals) {
				ri.original = imp.Originals[i]
			}
			if _, dup := s.imports[n]; !dup && n != "" {
				s.imports[n] = ri
			}
		}
	}
}

// goImportName is the name a Go import binds: its alias, or its package's
// name, which for a module path ending in a major version suffix (`/v5`) is
// the element before it.
func goImportName(imp wire.Import) string {
	path := strings.Trim(imp.Path, "\"`")
	last := path[strings.LastIndex(path, "/")+1:]
	if len(imp.Names) > 0 && imp.Names[0] != last {
		return imp.Names[0]
	}
	if goMajorVersion(last) {
		if rest := path[:max(strings.LastIndex(path, "/"), 0)]; rest != "" {
			return rest[strings.LastIndex(rest, "/")+1:]
		}
	}
	return last
}

// goMajorVersion reports a module path element that is a major version
// suffix: `v` and digits, v2 or later.
func goMajorVersion(e string) bool {
	if len(e) < 2 || e[0] != 'v' || e == "v0" || e == "v1" {
		return false
	}
	for _, c := range e[1:] {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

// goModule is a Go import path without its major version suffix.
func goModule(path string) string {
	if i := strings.LastIndex(path, "/"); i >= 0 && goMajorVersion(path[i+1:]) {
		return path[:i]
	}
	return path
}

// Framework names, as an endpoint's metadata spells them.
const (
	fwFlask   = "flask"
	fwFastAPI = "fastapi"
	fwDjango  = "django"
	fwExpress = "express"
	fwSpring  = "spring"
	fwNetHTTP = "net/http"
	fwGorilla = "gorilla/mux"
	fwChi     = "chi"
	fwGin     = "gin"
	fwEcho    = "echo"
)

// frameworkOf is the framework an import path of language names, or empty.
func frameworkOf(language, path string) string {
	switch language {
	case "python":
		switch {
		case path == "flask" || strings.HasPrefix(path, "flask."):
			return fwFlask
		case path == "fastapi" || strings.HasPrefix(path, "fastapi."):
			return fwFastAPI
		case path == "django.urls" || path == "django.conf.urls":
			return fwDjango
		}
	case "javascript", "typescript", "tsx":
		if path == "express" {
			return fwExpress
		}
	case "go":
		switch goModule(path) {
		case "net/http":
			return fwNetHTTP
		case "github.com/gorilla/mux":
			return fwGorilla
		case "github.com/go-chi/chi":
			return fwChi
		case "github.com/gin-gonic/gin":
			return fwGin
		case "github.com/labstack/echo":
			return fwEcho
		}
	case "java":
		if path == springAnnotations || strings.HasPrefix(path, springAnnotations+".") {
			return fwSpring
		}
	}
	return ""
}

// routeObj is an application or a router object the file builds, by the
// site that builds it, with what its prefix is made of.
type routeObj struct {
	fw string
	// root is an application root: an object whose routes are mounted at
	// the application's top unless this file mounts the object elsewhere.
	root bool
	// own is the prefix the object's construction declares (APIRouter
	// prefix=, Blueprint url_prefix=), ownKnown false when the construction
	// names one that is not a literal or its arguments are not known.
	own      string
	ownKnown bool
	// atts are the places this file attaches the object: mounts on a parent
	// object, or serving it.
	atts []routeAttach
	// state 1 is being resolved, 2 resolved, with prefix and ok.
	state  uint8
	prefix string
	ok     bool
}

// routeAttach is one place the file attaches an object: a mount on parent
// with a prefix, or serving it as the program's handler (served).
type routeAttach struct {
	served bool
	parent *routeObj
	// prefix is the mount's prefix, known false when it is not a literal or
	// the mount's arguments are not known.
	prefix string
	known  bool
	// override reports a mount prefix that replaces the object's own (a
	// Flask url_prefix= at registration) rather than adding to it.
	override bool
}

// resolve is an object's full prefix and whether every part of it resolved:
// an object attached nowhere resolves only when it is an application root;
// one attached once resolves when its parent does and the mount's prefix is
// a literal; one attached more than once is mounted at several prefixes and
// resolves to none of them.
func (s *routeScan) resolve(o *routeObj) (string, bool) {
	if o == nil {
		return "", false
	}
	switch o.state {
	case 1:
		return "", false
	case 2:
		return o.prefix, o.ok
	}
	o.state = 1
	prefix, ok := s.compute(o)
	o.state, o.prefix, o.ok = 2, prefix, ok
	return prefix, ok
}

func (s *routeScan) compute(o *routeObj) (string, bool) {
	switch len(o.atts) {
	case 0:
		if o.root && o.ownKnown {
			return o.own, true
		}
		return "", false
	case 1:
	default:
		return "", false
	}
	a := o.atts[0]
	if !a.known {
		return "", false
	}
	base := ""
	if !a.served {
		p, ok := s.resolve(a.parent)
		if !ok {
			return "", false
		}
		base = p
	}
	pre := a.prefix
	if !a.override {
		if !o.ownKnown {
			return "", false
		}
		pre = joinPrefix(pre, o.own)
	}
	return joinPrefix(base, pre), true
}

// joinPrefix joins two prefixes with one slash between them.
func joinPrefix(a, b string) string {
	switch {
	case a == "":
		return b
	case b == "":
		return a
	}
	return strings.TrimSuffix(a, "/") + "/" + strings.TrimPrefix(b, "/")
}

// joinRoute is the full path of a route under a resolved prefix: an empty
// route is the prefix itself, the root when there is none.
func joinRoute(prefix, route string) string {
	if route == "" {
		return cmp.Or(prefix, "/")
	}
	return joinPrefix(prefix, route)
}

// routeText is a route as the source writes it: a string literal's value, or
// the expression as written when it is not one.
type routeText struct {
	written string
	value   string
	literal bool
	// path is the part of a literal that is a URL path when the route
	// leads with something else (a Go pattern's host); empty when the
	// whole value is the path.
	path string
}

// route is what the endpoint's metadata calls the route as written: the
// literal's value, the literal itself when its value is empty (a route field
// is never empty), or the expression.
func (r routeText) route() string {
	if !r.literal || r.value == "" {
		return r.written
	}
	return r.value
}

// emit adds one endpoint per method (one with no method when methods is
// empty) for a route registered on obj, whose prefix is resolved only when
// the route is a literal and obj's prefix resolves.
func (s *routeScan) emit(fw string, methods []string, r routeText, obj *routeObj, at wire.Span, handlers []handler, receiver string) {
	prefix, ok := "", false
	if r.literal {
		prefix, ok = s.resolve(obj)
	}
	s.emitPrefixed(fw, methods, r, prefix, ok, at, handlers, receiver)
}

// emitPrefixed is emit with the prefix already decided.
func (s *routeScan) emitPrefixed(fw string, methods []string, r routeText, prefix string, resolved bool, at wire.Span, handlers []handler, receiver string) {
	if r.route() == "" {
		// An argument with no text names no route.
		return
	}
	if len(methods) == 0 {
		methods = []string{""}
	}
	for _, m := range methods {
		f := routeFact{receiver: receiver}
		f.meta.Framework, f.meta.Method, f.meta.Route = fw, m, r.route()
		f.meta.Prefix = model.PrefixUnresolved
		if resolved && r.literal {
			f.meta.Prefix, f.meta.Path = model.PrefixResolved, joinRoute(prefix, cmp.Or(r.path, r.value))
		}
		f.descriptor = f.meta.Route
		if m != "" {
			f.descriptor = m + " " + f.meta.Route
		}
		f.at, f.handlers = at, handlers
		s.out = append(s.out, f)
	}
}

// facts are the routes as endpoint facts: two that share a descriptor on
// different receivers carry the receiver as their disambiguator.
func (s *routeScan) facts() []endpointFact {
	receivers := map[string]map[string]bool{}
	for _, f := range s.out {
		if receivers[f.descriptor] == nil {
			receivers[f.descriptor] = map[string]bool{}
		}
		receivers[f.descriptor][f.receiver] = true
	}
	out := make([]endpointFact, len(s.out))
	for i, f := range s.out {
		if len(receivers[f.descriptor]) > 1 {
			f.disambiguator = f.receiver
		}
		out[i] = f.endpointFact
	}
	return out
}

// text is the source bytes of a span the worker sent, or empty when the
// span does not fit the pinned bytes or cuts a UTF-8 sequence: such a span
// names no text, and a route is never read from it.
func (s *routeScan) text(at wire.Span) string {
	if at.End > uint32(len(s.b.src)) || at.Start > at.End {
		return ""
	}
	t := string(s.b.src[at.Start:at.End])
	if !utf8.ValidString(t) {
		return ""
	}
	return t
}

// site is the worker site at a call reference's callee token, or nil.
func (s *routeScan) site(r *wire.Ref) (*wire.Site, int) {
	if s.link == nil {
		return nil, -1
	}
	i, ok := s.siteAt[wire.Span{Start: r.NameStart, End: r.NameEnd}]
	if !ok {
		return nil, -1
	}
	return &s.link.Sites[i], i
}

// resultSite is the worker site whose result a type names, or -1.
func (s *routeScan) resultSite(t wire.Type) int {
	if s.link == nil || t.Kind != wire.TypeResult || uint64(t.Index) >= uint64(len(s.link.Sites)) {
		return -1
	}
	return int(t.Index)
}

// siteRef is the call reference a worker site joins, or nil.
func (s *routeScan) siteRef(i int) *wire.Ref {
	if s.link == nil || i < 0 || i >= len(s.link.Sites) {
		return nil
	}
	j, ok := s.refAt[s.link.Sites[i].Name]
	if !ok {
		return nil
	}
	return &s.b.ex.refs[j]
}

// callee is a site's callee token as written.
func (s *routeScan) callee(i int) string {
	if s.link == nil || i < 0 || i >= len(s.link.Sites) {
		return ""
	}
	return s.text(s.link.Sites[i].Name)
}

// routeArg is one argument of a route form: its keyword, empty for a
// positional argument, its value's span and the value as written.
type routeArg struct {
	kw   string
	at   wire.Span
	text string
}

// siteArgs are a site's arguments, or nil when the worker sent no argument
// positions for it.
func (s *routeScan) siteArgs(site *wire.Site) []routeArg {
	if site == nil || len(site.Arguments) == 0 {
		return nil
	}
	out := make([]routeArg, len(site.Arguments))
	for i, a := range site.Arguments {
		if a.At.End > uint32(len(s.b.src)) || a.At.Start > a.At.End {
			// The link table drops and counts an argument that does not fit
			// the pinned bytes; a route is not read from it.
			return nil
		}
		kw := a.Keyword
		if a.Spread != wire.SpreadNone {
			kw = "*"
		}
		out[i] = routeArg{kw: kw, at: a.At, text: strings.TrimSpace(s.text(a.At))}
	}
	return out
}

// decoratorArgs are a decorator's arguments, a keyword argument's keyword
// (`methods=` in Python, `path =` in a Java element-value pair) split from
// its value.
func (s *routeScan) decoratorArgs(d wire.Decorator) []routeArg {
	out := make([]routeArg, 0, len(d.Arguments))
	for _, a := range d.Arguments {
		text := s.text(a)
		kw, off := keywordOf(text)
		at := wire.Span{Start: a.Start + uint32(off), End: a.End}
		out = append(out, routeArg{kw: kw, at: at, text: strings.TrimSpace(text[off:])})
	}
	return out
}

// keywordOf splits a keyword argument as written, `name = value`, into its
// keyword and the offset of its value; a positional argument has no keyword.
func keywordOf(text string) (string, int) {
	i := 0
	for i < len(text) && isSpace(text[i]) {
		i++
	}
	start := i
	for i < len(text) && isIdent(text[i], i == start) {
		i++
	}
	if i == start {
		return "", 0
	}
	kw := text[start:i]
	for i < len(text) && isSpace(text[i]) {
		i++
	}
	if i >= len(text) || text[i] != '=' || i+1 < len(text) && text[i+1] == '=' {
		return "", 0
	}
	i++
	for i < len(text) && isSpace(text[i]) {
		i++
	}
	return kw, i
}

func isIdent(c byte, first bool) bool {
	return c == '_' || c == '$' || 'a' <= c && c <= 'z' || 'A' <= c && c <= 'Z' || !first && '0' <= c && c <= '9'
}

// argument is the argument at position pos, or the keyword argument kw,
// whichever the call writes, and whether it does: a positional argument
// counts only before the first keyword or spread argument.
func argument(args []routeArg, pos int, kw string) (routeArg, bool) {
	n := 0
	for _, a := range args {
		if a.kw == "*" {
			break
		}
		if a.kw != "" {
			continue
		}
		if n == pos {
			return a, true
		}
		n++
	}
	if kw != "" {
		for _, a := range args {
			if a.kw == kw {
				return a, true
			}
		}
	}
	return routeArg{}, false
}

// positional are the arguments before the first keyword or spread one.
func positional(args []routeArg) []routeArg {
	for i, a := range args {
		if a.kw != "" {
			return args[:i]
		}
	}
	return args
}

// routeOf is an argument as a route: its literal value, or the expression
// as written.
func (s *routeScan) routeOf(a routeArg) routeText {
	v, ok := stringLiteral(s.b.lang.Name, a.text)
	return routeText{written: a.text, value: v, literal: ok}
}

// stringLiteral is the value of text when it is one string literal of
// language with no escape and no interpolation, and whether it is.
func stringLiteral(language, text string) (string, bool) {
	text = strings.TrimSpace(text)
	if language == "python" {
		i := 0
		for i < len(text) && i < 2 && strings.IndexByte("rRuUbB", text[i]) >= 0 {
			i++
		}
		raw := strings.ContainsAny(text[:i], "rR")
		text = text[i:]
		for _, q := range []string{`"""`, `'''`, `"`, `'`} {
			if len(text) >= 2*len(q) && strings.HasPrefix(text, q) && strings.HasSuffix(text, q) {
				body := text[len(q) : len(text)-len(q)]
				if strings.Contains(body, q[:1]) || !raw && strings.Contains(body, `\`) {
					return "", false
				}
				return body, true
			}
		}
		return "", false
	}
	if len(text) < 2 {
		return "", false
	}
	q, body := text[0], text[1:len(text)-1]
	if text[len(text)-1] != q || strings.IndexByte(body, q) >= 0 {
		return "", false
	}
	switch {
	case q == '"':
	case q == '\'' && (language == "javascript" || language == "typescript" || language == "tsx"):
	case q == '`' && language == "go":
		return body, true
	case q == '`' && (language == "javascript" || language == "typescript" || language == "tsx"):
		if strings.Contains(body, "${") {
			return "", false
		}
	default:
		return "", false
	}
	if strings.Contains(body, `\`) {
		return "", false
	}
	return body, true
}

// elements are the elements of a list, tuple, set or array literal as
// written, or text alone when it is not one.
func elements(text string) []string {
	text = strings.TrimSpace(text)
	if len(text) < 2 {
		return []string{text}
	}
	open, close := text[0], text[len(text)-1]
	if !(open == '[' && close == ']' || open == '(' && close == ')' || open == '{' && close == '}') {
		return []string{text}
	}
	var out []string
	for _, e := range splitTop(text[1 : len(text)-1]) {
		if e = strings.TrimSpace(e); e != "" {
			out = append(out, e)
		}
	}
	return out
}

// splitTop splits text at its commas outside brackets and string literals.
func splitTop(text string) []string {
	var out []string
	depth, start := 0, 0
	var quote byte
	for i := 0; i < len(text); i++ {
		c := text[i]
		switch {
		case quote != 0:
			if c == '\\' {
				i++
			} else if c == quote {
				quote = 0
			}
		case c == '"' || c == '\'' || c == '`':
			quote = c
		case c == '(' || c == '[' || c == '{':
			depth++
		case c == ')' || c == ']' || c == '}':
			depth--
		case c == ',' && depth == 0:
			out = append(out, text[start:i])
			start = i + 1
		}
	}
	return append(out, text[start:])
}

// literalMethods are the methods a list of method literals names, upper
// case, and false when any element is not a literal (or is longer than a
// method's bound): one endpoint with no
// method then stands for the route, since which methods it takes is not
// written.
func (s *routeScan) literalMethods(text string) ([]string, bool) {
	var out []string
	for _, e := range elements(text) {
		v, ok := stringLiteral(s.b.lang.Name, e)
		if !ok || v == "" || len(v) > model.MaxIdentifierBytes {
			return nil, false
		}
		out = append(out, strings.ToUpper(v))
	}
	return out, len(out) > 0
}

// handlerAt is the handler an argument names: a function literal written in
// place, a name or a dotted name spelled as a call of it would be, or none
// for any other expression (a wrapper call, an index), which leaves the
// endpoint without a handler rather than guessing one.
func (s *routeScan) handlerAt(a routeArg) []handler {
	at := a.at
	for _, fn := range s.b.ex.functions {
		if fn.Span == at {
			return []handler{fnHandler(at)}
		}
	}
	if r, ok := s.nameRef(a.text, at); ok {
		return []handler{refHandler(r)}
	}
	return nil
}

// nameRef is a reference to text when it is a name or a dotted name with no
// space in it, at span at.
func (s *routeScan) nameRef(text string, at wire.Span) (wire.Ref, bool) {
	if text == "" || uint32(len(text)) != at.End-at.Start {
		return wire.Ref{}, false
	}
	for _, part := range strings.Split(text, ".") {
		if part == "" {
			return wire.Ref{}, false
		}
		for i := 0; i < len(part); i++ {
			if !isIdent(part[i], i == 0) {
				return wire.Ref{}, false
			}
		}
	}
	r := wire.Ref{Kind: "call", Start: at.Start, End: at.End, NameStart: at.Start, NameEnd: at.End, Name: text,
		Scope: s.b.innermost(at)}
	if dot := strings.LastIndexByte(text, '.'); dot >= 0 {
		r.Qualified, r.Name, r.NameStart = true, text[dot+1:], at.Start+uint32(dot)+1
		if q := text[:dot]; len(q) <= wire.MaxQualifierBytes {
			r.Qualifier = q
		}
		_, r.QualifierIsImport = s.imports[text[:dot]]
	}
	return r, true
}

// refSpan is a call reference's whole call.
func refSpan(r *wire.Ref) wire.Span { return wire.Span{Start: r.Start, End: r.End} }

// argType is the type the worker recovered for argument i of a site.
func argType(site *wire.Site, i int) wire.Type {
	if site == nil || i < 0 || i >= len(site.Args) {
		return wire.Type{}
	}
	return site.Args[i]
}

// calls visits every call reference of the file that a worker site joins,
// in source order, with its site and index.
func (s *routeScan) calls(visit func(r *wire.Ref, site *wire.Site, i int)) {
	if s.link == nil {
		return
	}
	for j := range s.b.ex.refs {
		r := &s.b.ex.refs[j]
		if r.Kind != "call" {
			continue
		}
		k := wire.Span{Start: r.NameStart, End: r.NameEnd}
		if s.refAt[k] != j {
			continue
		}
		if site, i := s.site(r); site != nil {
			visit(r, site, i)
		}
	}
}

// attachArgs records, for each argument of a mount site that is the result
// of a site building an object, the attachment the mount makes; at is the
// argument position that holds the object. mount is called with the
// object, the argument index and the site, and returns the attachment.
func (s *routeScan) attachArgs(site *wire.Site, mount func(o *routeObj, i int) (routeAttach, bool)) {
	for i, t := range site.Args {
		if o := s.objs[s.resultSite(t)]; o != nil {
			if a, ok := mount(o, i); ok {
				o.atts = append(o.atts, a)
			}
		}
	}
}

// objOf is the object a receiver type names: one this file builds, or nil.
func (s *routeScan) objOf(t wire.Type) *routeObj {
	if i := s.resultSite(t); i >= 0 {
		return s.objs[i]
	}
	return nil
}
