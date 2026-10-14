package treesitter

import (
	"cmp"
	"slices"
	"strings"

	"github.com/Sawmonabo/codectx/internal/model"
	"github.com/Sawmonabo/codectx/internal/provider/treesitter/lang"
	"github.com/Sawmonabo/codectx/internal/provider/treesitter/wire"
)

// detailRequestInput is the evidence detail of a request-input read's
// `reads` relation.
const detailRequestInput = "request_input"

// inputRead is one request-input read of the file: the reading expression,
// what it reads, and its node once published.
type inputRead struct {
	at    wire.Span
	input model.RequestInput
	node  model.NodeID
}

// inputAttr is one member of a request object the table models: the part of
// the request it reads; keyed when a literal key may follow it (a subscript,
// a getter call, an ECMAScript property); call when it reads only when
// called, argKey when its first argument is then the key; skip for a member
// that reads nothing (a body's Close); sub for the members of it that read
// another part (Go's URL.Query).
type inputAttr struct {
	source              model.InputSource
	keyed, call, argKey bool
	skip                bool
	sub                 map[string]inputAttr
}

// inputTable is a request object's modelled members by name.
type inputTable map[string]inputAttr

func attr(s model.InputSource) inputAttr      { return inputAttr{source: s} }
func keyedAttr(s model.InputSource) inputAttr { return inputAttr{source: s, keyed: true} }
func callAttr(s model.InputSource) inputAttr  { return inputAttr{source: s, call: true} }
func argAttr(s model.InputSource) inputAttr   { return inputAttr{source: s, call: true, argKey: true} }

// The request objects' tables, each from the framework's own reference
// (docs/providers-treesitter.md, Request inputs). A member a table does not
// name publishes nothing.
var (
	// flaskRequest is Flask's `request` (werkzeug.wrappers.Request).
	flaskRequest = inputTable{
		"args": keyedAttr(model.InputQuery), "form": keyedAttr(model.InputForm), "values": keyedAttr(model.InputCombined),
		"files": keyedAttr(model.InputFile), "cookies": keyedAttr(model.InputCookie), "headers": keyedAttr(model.InputHeader),
		"json": keyedAttr(model.InputBody), "get_json": callAttr(model.InputBody), "data": attr(model.InputRaw),
		"get_data": callAttr(model.InputRaw), "stream": attr(model.InputRaw), "view_args": keyedAttr(model.InputPath),
		"query_string": attr(model.InputQuery), "path": attr(model.InputURL), "full_path": attr(model.InputURL),
		"url": attr(model.InputURL), "base_url": attr(model.InputURL), "authorization": attr(model.InputHeader),
		"user_agent": attr(model.InputHeader),
	}
	// djangoRequest is Django's HttpRequest.
	djangoRequest = inputTable{
		"GET": keyedAttr(model.InputQuery), "POST": keyedAttr(model.InputForm), "COOKIES": keyedAttr(model.InputCookie),
		"FILES": keyedAttr(model.InputFile), "META": keyedAttr(model.InputHeader), "headers": keyedAttr(model.InputHeader),
		"body": attr(model.InputRaw), "read": callAttr(model.InputRaw), "readline": callAttr(model.InputRaw),
		"readlines": callAttr(model.InputRaw), "path": attr(model.InputURL), "path_info": attr(model.InputURL),
		"get_full_path": callAttr(model.InputURL), "build_absolute_uri": callAttr(model.InputURL),
		"get_host": callAttr(model.InputHeader), "get_signed_cookie": argAttr(model.InputCookie),
	}
	// starletteRequest is Starlette's Request and WebSocket, the object a
	// FastAPI or Starlette handler receives.
	starletteRequest = inputTable{
		"query_params": keyedAttr(model.InputQuery), "path_params": keyedAttr(model.InputPath),
		"headers": keyedAttr(model.InputHeader), "cookies": keyedAttr(model.InputCookie), "url": attr(model.InputURL),
		"base_url": attr(model.InputURL), "json": callAttr(model.InputBody), "body": callAttr(model.InputRaw),
		"form": callAttr(model.InputForm), "stream": callAttr(model.InputRaw),
		"receive_text": callAttr(model.InputBody), "receive_bytes": callAttr(model.InputBody),
		"receive_json": callAttr(model.InputBody), "iter_text": callAttr(model.InputBody),
		"iter_bytes": callAttr(model.InputBody), "iter_json": callAttr(model.InputBody),
	}
	// expressRequest is Express's req.
	expressRequest = inputTable{
		"query": keyedAttr(model.InputQuery), "params": keyedAttr(model.InputPath), "body": keyedAttr(model.InputBody),
		"cookies": keyedAttr(model.InputCookie), "signedCookies": keyedAttr(model.InputCookie),
		"headers": keyedAttr(model.InputHeader), "get": argAttr(model.InputHeader), "header": argAttr(model.InputHeader),
		"param": argAttr(model.InputCombined), "originalUrl": attr(model.InputURL), "url": attr(model.InputURL),
		"path": attr(model.InputURL), "baseUrl": attr(model.InputURL), "hostname": attr(model.InputHeader),
	}
	// goRequest is net/http's *Request.
	goRequest = inputTable{
		"FormValue": argAttr(model.InputCombined), "PostFormValue": argAttr(model.InputForm),
		"Cookie": argAttr(model.InputCookie), "Cookies": callAttr(model.InputCookie), "FormFile": argAttr(model.InputFile),
		"MultipartReader": callAttr(model.InputForm), "MultipartForm": attr(model.InputForm),
		"Referer": callAttr(model.InputHeader), "UserAgent": callAttr(model.InputHeader),
		"BasicAuth": callAttr(model.InputHeader), "PathValue": argAttr(model.InputPath),
		"Body": {source: model.InputBody, sub: map[string]inputAttr{"Close": {skip: true}}},
		"Form": keyedAttr(model.InputCombined), "PostForm": keyedAttr(model.InputForm),
		"Header": keyedAttr(model.InputHeader), "Trailer": keyedAttr(model.InputHeader), "Host": attr(model.InputHeader),
		"RequestURI": attr(model.InputURL),
		"URL": {source: model.InputURL, sub: map[string]inputAttr{
			"Query": {source: model.InputQuery, call: true, keyed: true}, "RawQuery": attr(model.InputQuery),
			"Path": attr(model.InputURL), "RawPath": attr(model.InputURL),
		}},
	}
)

// inputGetters are the getter methods whose first literal argument is the
// key a keyed member reads (`request.args.get("id")`, `r.Header.Get("X")`).
var inputGetters = map[string]bool{"get": true, "getlist": true, "get_all": true, "getall": true, "getAll": true,
	"Get": true, "Values": true, "ByName": true}

// inputScan is the state of one file's request-input table.
type inputScan struct {
	b    *builder
	rs   *routeScan
	toks []token
	// match is, for each opening bracket token, the index of its closing
	// token, or -1; it is -1 for every other token.
	match []int32
	seen  map[wire.Span]bool
	out   []inputRead
}

// requestInputs publishes the file's request-input reads: each one a
// `variable` node at the reading expression with its RequestInput under
// MetaInput, which the enclosing function (a declaration, a route handler
// literal, or the module) `reads`. The table is gated on the framework's
// import; a read it does not model publishes nothing. A read assigned to a
// variable flows to it (inputFlows), so an alias of the read reaches every
// use through the file's def-use facts.
func (b *builder) requestInputs() error {
	rs := &routeScan{b: b, link: b.ex.link, imports: map[string]routeImport{}, frameworks: map[string]bool{},
		objs: map[int]*routeObj{}}
	rs.routeImports()
	s := &inputScan{b: b, rs: rs, seen: map[wire.Span]bool{}}
	switch b.lang.Name {
	case "python":
		s.python()
	case "javascript", "typescript", "tsx":
		s.express()
	case "go":
		s.goRequests()
		s.goContexts()
	case "java":
		s.spring()
	}
	s.handlerCandidates()
	return b.publishInputs(s.out)
}

// lex tokenizes the file once, on first use.
func (s *inputScan) lex() {
	if s.toks != nil {
		return
	}
	s.toks = lexSource(s.b.lang.Name, s.b.src)
	s.match = make([]int32, len(s.toks))
	var stack []int
	for i, t := range s.toks {
		s.match[i] = -1
		if t.kind != tokPunct {
			continue
		}
		switch c := s.b.src[t.start]; c {
		case '(', '[', '{':
			stack = append(stack, i)
		case ')', ']', '}':
			open := map[byte]byte{')': '(', ']': '[', '}': '{'}[c]
			// An unbalanced closer pops to its own opener, so one stray
			// bracket does not unbalance the rest of the file.
			for k, s0 := range slices.Backward(stack) {
				if s.b.src[s.toks[s0].start] == open {
					s.match[s0] = int32(i)
					stack = stack[:k]
					break
				}
			}
		}
	}
}

// text is token i's bytes.
func (s *inputScan) text(i int) string { return string(s.b.src[s.toks[i].start:s.toks[i].end]) }

// punct reports token i is the punctuation byte c.
func (s *inputScan) punct(i int, c byte) bool {
	return i >= 0 && i < len(s.toks) && s.toks[i].kind == tokPunct && s.b.src[s.toks[i].start] == c
}

// ident reports token i is an identifier.
func (s *inputScan) ident(i int) bool { return i >= 0 && i < len(s.toks) && s.toks[i].kind == tokIdent }

// str is token i's literal value, and false when it is not a plain string
// literal.
func (s *inputScan) str(i int) (string, bool) {
	if i < 0 || i >= len(s.toks) || s.toks[i].kind != tokString {
		return "", false
	}
	return stringLiteral(s.b.lang.Name, s.text(i))
}

// tokensIn is the token index range of the bytes [start, end).
func (s *inputScan) tokensIn(start, end uint32) (int, int) {
	lo, _ := slices.BinarySearchFunc(s.toks, start, func(t token, at uint32) int { return cmp.Compare(t.start, at) })
	hi, _ := slices.BinarySearchFunc(s.toks, end, func(t token, at uint32) int { return cmp.Compare(t.start, at) })
	for hi > lo && s.toks[hi-1].end > end {
		hi--
	}
	return lo, hi
}

// reads records every modelled read of a request object named by the
// dotted receiver recv among the tokens [lo, hi).
func (s *inputScan) reads(fw string, recv []string, table inputTable, lo, hi int) {
	n := 2*len(recv) - 1
	for i := lo; i+n < hi; i++ {
		if !s.receiverAt(i, recv) || s.punct(i-1, '.') {
			continue
		}
		src, key, last, ok := s.member(table, i+n, hi)
		if !ok || src == "" {
			// A member that only leads to others (gin's Request field)
			// reads nothing on its own.
			continue
		}
		s.add(fw, src, key, s.toks[i].start, s.toks[last].end)
		i = last
	}
}

// receiverAt reports the dotted receiver recv starts at token i.
func (s *inputScan) receiverAt(i int, recv []string) bool {
	for k, name := range recv {
		j := i + 2*k
		if !s.ident(j) || s.text(j) != name || k > 0 && !s.punct(j-1, '.') {
			return false
		}
	}
	return true
}

// member reads the member access at token j, `.name` after the receiver:
// the part it reads, its literal key, the last token of the read, and false
// when the table does not model it.
func (s *inputScan) member(table inputTable, j, hi int) (model.InputSource, string, int, bool) {
	if j+1 >= hi || !s.punct(j, '.') || !s.ident(j+1) {
		return "", "", 0, false
	}
	a, ok := table[s.text(j+1)]
	if !ok {
		return "", "", 0, false
	}
	return s.attrRead(a, j+2, hi)
}

// attrRead completes the read of member a, whose name ends before token k.
func (s *inputScan) attrRead(a inputAttr, k, hi int) (model.InputSource, string, int, bool) {
	if a.skip {
		return "", "", 0, false
	}
	last, key := k-1, ""
	if a.call {
		if k >= hi || !s.punct(k, '(') || s.match[k] < 0 || int(s.match[k]) >= hi {
			return "", "", 0, false
		}
		c := int(s.match[k])
		if a.argKey && k+1 < c && (k+2 == c || s.punct(k+2, ',')) {
			key, _ = s.str(k + 1)
		}
		last, k = c, c+1
	}
	if a.sub != nil && k+1 < hi && s.punct(k, '.') && s.ident(k+1) {
		if sa, ok := a.sub[s.text(k+1)]; ok {
			return s.attrRead(sa, k+2, hi)
		}
	}
	if a.keyed && key == "" {
		switch {
		case s.punct(k, '[') && k+2 < hi && s.punct(k+2, ']'):
			if v, ok := s.str(k + 1); ok {
				key, last = v, k+2
			}
		case s.punct(k, '.') && k+3 < hi && s.ident(k+1) && inputGetters[s.text(k+1)] && s.punct(k+2, '(') &&
			s.match[k+2] > 0 && int(s.match[k+2]) < hi && (s.punct(k+4, ')') || s.punct(k+4, ',')):
			if v, ok := s.str(k + 3); ok {
				key, last = v, int(s.match[k+2])
			}
		case jsLanguage(s.b.lang.Name) && s.punct(k, '.') && k+1 < hi && s.ident(k+1) && !s.punct(k+2, '('):
			key, last = s.text(k+1), k+1
		}
	}
	return a.source, key, last, true
}

// add records one read once per range.
func (s *inputScan) add(fw string, src model.InputSource, key string, start, end uint32) {
	at := wire.Span{Start: start, End: end}
	if s.seen[at] {
		return
	}
	s.seen[at] = true
	s.out = append(s.out, inputRead{at: at, input: model.RequestInput{Framework: fw, Source: src, Key: key}})
}

// handlerSpans are the declarations and function literals the file's routes
// of framework fw route to, and the handlers extra names (middleware), each
// once.
func (s *inputScan) handlerSpans(fw string, extra []handler) (decls []int, literals []wire.Span) {
	seenDecl, seenLit := map[int]bool{}, map[wire.Span]bool{}
	groups := make([][]handler, 0, len(s.b.endpointFacts)+1)
	for i := range s.b.endpointFacts {
		if f := &s.b.endpointFacts[i]; f.meta.Framework == fw {
			groups = append(groups, f.handlers)
		}
	}
	groups = append(groups, extra)
	for _, hs := range groups {
		for _, h := range hs {
			switch {
			case h.built:
				// A builder's parameters are not the request.
			case h.decl >= 0 && h.decl < len(s.b.decls) && !seenDecl[h.decl]:
				seenDecl[h.decl] = true
				decls = append(decls, h.decl)
			case h.ref != nil && !h.ref.Qualified:
				// A handler named in this file is the one declaration of
				// that name a call of it would name.
				if ts := s.b.targets(*h.ref); len(ts) == 1 && !seenDecl[ts[0]] {
					seenDecl[ts[0]] = true
					decls = append(decls, ts[0])
				}
			case h.fn != nil && !seenLit[*h.fn]:
				seenLit[*h.fn] = true
				literals = append(literals, *h.fn)
			}
		}
	}
	return decls, literals
}

// publishInputs publishes the reads, in source order, and keeps them for
// inputFlows. A read whose key does not fit its bound keeps no key, and one
// whose native key does not fit is not published; both are counted.
func (b *builder) publishInputs(reads []inputRead) error {
	if len(reads) == 0 {
		return nil
	}
	slices.SortFunc(reads, func(x, y inputRead) int {
		return cmp.Or(cmp.Compare(x.at.Start, y.at.Start), cmp.Compare(x.at.End, y.at.End))
	})
	for _, r := range reads {
		if len(r.input.Key) > model.MaxNameBytes {
			r.input.Key = ""
			b.dropped[structure]++
		}
		if err := r.input.Validate(); err != nil {
			return &model.Error{Code: model.CodeInternal, Message: "request input: " + err.Error()}
		}
		rng, err := b.rangeOf(r.at.Start, r.at.End)
		if err != nil {
			return err
		}
		key := b.rangeKey("input:", r.at.Start, r.at.End)
		if key == "" {
			b.dropped[structure]++
			continue
		}
		var cut truncations
		name := cut.cut("name", collapse(string(b.src[r.at.Start:r.at.End])), model.MaxNameBytes)
		res, err := b.resolve(model.NodeCandidate{ProviderID: lang.ProviderID, ScopeKey: b.scope, NativeKey: key,
			Kind: model.NodeVariable, Language: b.lang.Name, Name: name, QualifiedName: name,
			FileID: b.fv.ID, ContentHash: b.fv.ContentHash, Range: rng})
		if err != nil {
			return err
		}
		meta := map[string]any{model.MetaInput: r.input}
		if len(cut) > 0 {
			meta["truncated_fields"] = cut
		}
		b.putNode(structure, res, rng, key, meta)
		b.putRelation(structure, b.inputOwner(r.at), model.RelReads, res.Node.ID, rng, key, detailRequestInput)
		r.node = res.Node.ID
		b.inputs = append(b.inputs, r)
	}
	return nil
}

// inputOwner is the function a read is made from: the innermost route
// handler literal around it inside its innermost declaration, else that
// declaration, else the file's module.
func (b *builder) inputOwner(at wire.Span) model.NodeID {
	d := b.innermostCallable(at)
	for _, l := range slices.Backward(b.literals) {
		if l.at.Start > at.Start || at.End > l.at.End {
			continue
		}
		if d < 0 || b.decls[d].Start <= l.at.Start && l.at.End <= b.decls[d].End {
			return l.id
		}
	}
	if d >= 0 {
		return b.decls[d].res.Node.ID
	}
	return b.module.Node.ID
}

// holdsCallable reports that the bytes n hold the whole innermost function,
// method or test around at.
func (b *builder) holdsCallable(n, at wire.Span) bool {
	d := b.innermostCallable(at)
	return d >= 0 && n.Start <= b.decls[d].Start && b.decls[d].End <= n.End
}

// innermostCallable is the innermost function, method or test of the file
// whose range contains at, or -1.
func (b *builder) innermostCallable(at wire.Span) int {
	found := -1
	for i := range b.decls {
		d := &b.decls[i]
		if d.Start > at.Start {
			break
		}
		if d.Start <= at.Start && at.End <= d.End && (d.kind == model.NodeFunction || d.kind == model.NodeMethod || d.kind == model.NodeTest) {
			found = i
		}
	}
	return found
}

// inputFlows publishes, for one analysed function, a data_flows_to from
// each request-input read to the variable a definition defines with it,
// at a node of the read's own function that holds the read (`uid = request.args.get("id")` flows to uid), so
// the file's def-use facts carry the read on to every use of the alias.
func (b *builder) inputFlows(f *wire.Function, ids []model.NodeID, nodes []*model.SourceRange) {
	if len(b.inputs) == 0 {
		return
	}
	for _, a := range f.Writes {
		n := f.Nodes[a.Node]
		k, _ := slices.BinarySearchFunc(b.inputs, n.Start, func(r inputRead, at uint32) int { return cmp.Compare(r.at.Start, at) })
		for ; k < len(b.inputs) && b.inputs[k].at.Start < n.End; k++ {
			r := b.inputs[k]
			// A definition whose node holds the whole function the read is
			// in (a `def` statement defining its name, decorated or not) is
			// not the read's: the read runs when the function is called.
			if r.at.End <= n.End && b.innermostCallable(n) == b.innermostCallable(r.at) && !b.holdsCallable(n, r.at) {
				b.dependsOn(r.node, model.RelDataFlowsTo, ids[a.Var], nodes[a.Node], b.rangeKey("input:", r.at.Start, r.at.End), detailFlow)
			}
		}
	}
}

// python reads Flask's `request` anywhere in the file, the request a Django
// view receives, and the parameters of FastAPI and Starlette handlers.
func (s *inputScan) python() {
	for name, imp := range s.rs.imports {
		if imp.path != "flask" && !strings.HasPrefix(imp.path, "flask.") {
			continue
		}
		var recv []string
		switch {
		case s.rs.fromImport(name) && (imp.original == "request" || imp.original == "" && name == "request"):
			recv = []string{name}
		case !s.rs.fromImport(name) && imp.path == "flask":
			recv = []string{name, "request"}
		default:
			continue
		}
		s.lex()
		s.reads(fwFlask, recv, flaskRequest, 0, len(s.toks))
	}
	s.djangoViews()
	s.drfViews()
	s.fastAPIHandlers(fwFastAPI)
	s.fastAPIHandlers(fwStarlette)
}

// express reads the request parameter of each Express route handler.
func (s *inputScan) express() {
	if !s.rs.frameworks[fwExpress] {
		return
	}
	decls, literals := s.handlerSpans(fwExpress, s.b.middleware)
	if len(decls) == 0 && len(literals) == 0 {
		return
	}
	s.lex()
	for _, d := range decls {
		s.firstParamReads(fwExpress, expressRequest, s.b.decls[d].NameEnd, s.b.decls[d].Start, s.b.decls[d].End, false)
	}
	for _, l := range literals {
		s.firstParamReads(fwExpress, expressRequest, l.Start, l.Start, l.End, false)
	}
}
