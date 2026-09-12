//go:build unix

package lsp

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net/textproto"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

// The fake language server. It is the test binary re-executed with
// CODECTX_LSP_FAKE=1 through the real process runner, so the protocol is
// exercised over real pipes against a real child. It speaks the base protocol
// with its own minimal framing and its own position arithmetic, independent
// of the client's, so a shared bug cannot cancel out.
//
// It records what happened to it in events.log in its working directory, one
// line per event, which the test reads back.
//
// Behaviour by request (positions are zero-based lines):
//
//	initialize                       must be first; chooses CODECTX_LSP_FAKE_ENCODING if offered
//	initialized                      then sends logMessage, workspace/configuration, workspace/applyEdit
//	textDocument/definition          line 0: held until the next request arrives (out-of-order)
//	                                 otherwise: the identifier at the position
//	textDocument/references          line 1: exits at once without answering (crash)
//	                                 line 3: held until $/cancelRequest, then RequestCancelled
//	                                 otherwise: the identifier at the position
//	textDocument/typeDefinition      two locations outside the root and the identifier
//	textDocument/implementation      one https URI
//	textDocument/documentSymbol      the functions of the file, nested under one container
//	textDocument/prepareCallHierarchy the identifier at the position
//	callHierarchy/incomingCalls      one caller: the function on line 3 calling at "héllo(1)"
//	callHierarchy/outgoingCalls      one callee with a call site on the queried
//	                                 item's own line ("y }")
//	workspace/symbol                 not advertised; recorded if ever received
//	shutdown, exit                   recorded; exit terminates the process

type fakeServer struct {
	in     *bufio.Reader
	out    io.Writer
	events *os.File
	enc    string
	// held is a definition request on line 0 waiting for the next request.
	held *message
	// heldRefs is the references request waiting for its cancel.
	heldRefs *message
	// replaying is set while the held request is finally answered.
	replaying bool
	nextID    int
}

func runFakeServer() int {
	events, err := os.OpenFile("events.log", os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return 2
	}
	defer events.Close()
	f := &fakeServer{in: bufio.NewReader(os.Stdin), out: os.Stdout, events: events, enc: "utf-16"}
	f.event("pid=%d", os.Getpid())
	f.event("argv=%s", strings.Join(os.Args[1:], " "))
	initialized := false
	for {
		payload, err := f.read()
		if err != nil {
			f.event("read-error=%v", err)
			return 1
		}
		var msg message
		if err := json.Unmarshal(payload, &msg); err != nil {
			f.event("bad-json")
			return 1
		}
		if msg.Method == "" {
			f.onResponse(msg)
			continue
		}
		if !initialized && msg.Method != "initialize" {
			f.event("before-initialize=%s", msg.Method)
			return 1
		}
		if msg.Method == "initialize" {
			initialized = true
		}
		if f.held != nil && msg.ID != nil {
			held := f.held
			f.held = nil
			// The newer request is answered first, then the held one.
			if code := f.handle(msg); code >= 0 {
				return code
			}
			f.replaying = true
			f.handle(*held)
			f.replaying = false
			continue
		}
		if code := f.handle(msg); code >= 0 {
			return code
		}
	}
}

// handle dispatches one request or notification. It returns an exit code to
// terminate with, or -1 to continue.
func (f *fakeServer) handle(msg message) int {
	f.event("recv=%s", msg.Method)
	switch msg.Method {
	case "initialize":
		var p initializeParams
		json.Unmarshal(msg.Params, &p)
		want := os.Getenv("CODECTX_LSP_FAKE_ENCODING")
		for _, offered := range p.Capabilities.General.PositionEncodings {
			if offered == want {
				f.enc = want
			}
		}
		f.event("encoding=%s", f.enc)
		f.reply(msg, map[string]any{
			"capabilities": map[string]any{
				"positionEncoding":       f.enc,
				"textDocumentSync":       1,
				"definitionProvider":     true,
				"referencesProvider":     map[string]any{},
				"implementationProvider": true,
				"typeDefinitionProvider": true,
				"documentSymbolProvider": true,
				"callHierarchyProvider":  true,
			},
			"serverInfo": map[string]any{"name": "fake", "version": "1.2.3"},
		}, nil)
	case "initialized":
		f.notify("window/logMessage", map[string]any{"type": 3, "message": "fake server ready"})
		f.request("workspace/configuration", map[string]any{"items": []any{map[string]any{"section": "gopls"}, map[string]any{"section": "other"}}})
		f.request("workspace/applyEdit", map[string]any{"label": "hostile", "edit": map[string]any{"changes": map[string]any{}}})
	case "textDocument/didOpen":
		var p didOpenParams
		json.Unmarshal(msg.Params, &p)
		f.event("opened=%s", filepath.Base(p.TextDocument.URI))
	case "textDocument/didClose":
	case "textDocument/definition", "textDocument/prepareCallHierarchy", "textDocument/references", "textDocument/typeDefinition", "textDocument/implementation":
		var p textDocumentPositionParams
		json.Unmarshal(msg.Params, &p)
		switch {
		case msg.Method == "textDocument/definition" && p.Position.Line == 0 && !f.replaying:
			f.held = &msg
			f.event("holding-definition")
			return -1
		case msg.Method == "textDocument/references" && p.Position.Line == 1:
			f.event("crashing")
			return 3
		case msg.Method == "textDocument/references" && p.Position.Line == 3:
			f.heldRefs = &msg
			f.event("holding-references")
			return -1
		}
		ident, err := f.identifierAt(p.TextDocument.URI, p.Position)
		if err != nil {
			f.reply(msg, nil, &rpcError{Code: -32803, Message: err.Error()})
			return -1
		}
		loc := map[string]any{"uri": p.TextDocument.URI, "range": ident.rng}
		switch msg.Method {
		case "textDocument/definition":
			f.reply(msg, []any{map[string]any{"targetUri": p.TextDocument.URI, "targetRange": ident.rng, "targetSelectionRange": ident.rng}}, nil)
		case "textDocument/references":
			f.reply(msg, []any{loc}, nil)
		case "textDocument/typeDefinition":
			root := filepath.Dir(uriPath(p.TextDocument.URI))
			f.reply(msg, []any{
				map[string]any{"uri": "file:///etc/hostname", "range": ident.rng},
				map[string]any{"uri": "file://" + filepath.ToSlash(root) + "/../../../../etc/passwd", "range": ident.rng},
				loc,
			}, nil)
		case "textDocument/implementation":
			f.reply(msg, []any{map[string]any{"uri": "https://example.com/source.go", "range": ident.rng}}, nil)
		case "textDocument/prepareCallHierarchy":
			f.reply(msg, []any{f.item(p.TextDocument.URI, ident)}, nil)
		}
	case "textDocument/documentSymbol":
		var p documentSymbolParams
		json.Unmarshal(msg.Params, &p)
		funcs := f.functions(p.TextDocument.URI)
		children := make([]any, 0, len(funcs))
		for _, fn := range funcs {
			children = append(children, map[string]any{"name": fn.text, "kind": 12, "range": fn.lineRange, "selectionRange": fn.rng})
		}
		f.reply(msg, []any{map[string]any{"name": "main", "kind": 4, "range": children[0].(map[string]any)["range"], "selectionRange": children[0].(map[string]any)["selectionRange"], "children": children}}, nil)
	case "callHierarchy/incomingCalls":
		var p callHierarchyCallsParams
		json.Unmarshal(msg.Params, &p)
		var it callHierarchyItem
		json.Unmarshal(p.Item, &it)
		funcs := f.functions(it.URI)
		caller := funcs[len(funcs)-1]
		site, _ := f.find(it.URI, it.Name+"(", 3)
		f.reply(msg, []any{map[string]any{"from": f.item(it.URI, caller), "fromRanges": []any{site.rng}}}, nil)
	case "callHierarchy/outgoingCalls":
		var p callHierarchyCallsParams
		json.Unmarshal(msg.Params, &p)
		var it callHierarchyItem
		json.Unmarshal(p.Item, &it)
		funcs := f.functions(it.URI)
		callee := funcs[len(funcs)-1]
		// The site of an outgoing call is in the queried item's own document,
		// not the peer's: "y }" is on the queried function's line.
		site, err := f.find(it.URI, "y }", 2)
		if err != nil {
			f.reply(msg, nil, &rpcError{Code: -32803, Message: err.Error()})
			return -1
		}
		f.reply(msg, []any{map[string]any{"to": f.item(it.URI, callee), "fromRanges": []any{site.rng}}}, nil)
	case "workspace/symbol":
		f.event("seen-workspace-symbol")
		f.reply(msg, []any{}, nil)
	case "$/cancelRequest":
		var p cancelParams
		json.Unmarshal(msg.Params, &p)
		if f.heldRefs != nil && string(*f.heldRefs.ID) == strconv.Itoa(int(p.ID)) {
			f.event("cancelled=%d", p.ID)
			f.reply(*f.heldRefs, nil, &rpcError{Code: rpcRequestCancelled, Message: "cancelled"})
			f.heldRefs = nil
		}
	case "shutdown":
		f.event("shutdown")
		f.reply(msg, nil, nil)
	case "exit":
		f.event("exit")
		return 0
	default:
		if msg.ID != nil {
			f.reply(msg, nil, &rpcError{Code: rpcMethodNotFound, Message: "fake does not implement " + msg.Method})
		}
	}
	return -1
}

// onResponse records how the client answered the fake's own requests.
func (f *fakeServer) onResponse(msg message) {
	id := ""
	if msg.ID != nil {
		id = string(*msg.ID)
	}
	switch {
	case msg.Error != nil:
		f.event("response=%s error=%d", id, msg.Error.Code)
	default:
		f.event("response=%s result=%s", id, string(msg.Result))
	}
}

// ident is one identifier found in a file with its range in the negotiated
// encoding.
type ident struct {
	text      string
	rng       lspRange
	lineRange lspRange
}

// identifierAt reads the materialized file itself, converts the position to a
// byte offset in the negotiated encoding with its own arithmetic, and returns
// the identifier there.
func (f *fakeServer) identifierAt(uri string, pos position) (ident, error) {
	data, err := os.ReadFile(uriPath(uri))
	if err != nil {
		return ident{}, err
	}
	lines := strings.SplitAfter(string(data), "\n")
	if int(pos.Line) >= len(lines) {
		return ident{}, fmt.Errorf("line %d past end", pos.Line)
	}
	line := strings.TrimRight(lines[pos.Line], "\r\n")
	col, err := f.byteColumn(line, pos.Character)
	if err != nil {
		return ident{}, err
	}
	isIdent := func(r rune) bool { return r == '_' || unicode.IsLetter(r) || unicode.IsDigit(r) }
	start, end := col, col
	for start > 0 {
		r, size := utf8.DecodeLastRuneInString(line[:start])
		if !isIdent(r) {
			break
		}
		start -= size
	}
	for end < len(line) {
		r, size := utf8.DecodeRuneInString(line[end:])
		if !isIdent(r) {
			break
		}
		end += size
	}
	if start == end {
		return ident{}, fmt.Errorf("no identifier at line %d unit %d", pos.Line, pos.Character)
	}
	return ident{
		text: line[start:end],
		rng: lspRange{Start: position{Line: pos.Line, Character: f.units(line[:start])},
			End: position{Line: pos.Line, Character: f.units(line[:end])}},
		lineRange: lspRange{Start: position{Line: pos.Line}, End: position{Line: pos.Line, Character: f.units(line)}},
	}, nil
}

// find locates needle on a zero-based line and returns the identifier that
// starts it.
func (f *fakeServer) find(uri, needle string, line int) (ident, error) {
	data, err := os.ReadFile(uriPath(uri))
	if err != nil {
		return ident{}, err
	}
	lines := strings.SplitAfter(string(data), "\n")
	text := strings.TrimRight(lines[line], "\r\n")
	i := strings.Index(text, needle)
	if i < 0 {
		return ident{}, fmt.Errorf("%q not on line %d", needle, line)
	}
	return f.identifierAt(uri, position{Line: uint32(line), Character: f.units(text[:i])})
}

// functions lists "func NAME" declarations in the file.
func (f *fakeServer) functions(uri string) []ident {
	data, _ := os.ReadFile(uriPath(uri))
	var out []ident
	for i, line := range strings.SplitAfter(string(data), "\n") {
		text := strings.TrimRight(line, "\r\n")
		if !strings.HasPrefix(text, "func ") {
			continue
		}
		id, err := f.identifierAt(uri, position{Line: uint32(i), Character: f.units("func ")})
		if err == nil {
			out = append(out, id)
		}
	}
	return out
}

func (f *fakeServer) item(uri string, id ident) map[string]any {
	return map[string]any{"name": id.text, "kind": 12, "uri": uri, "range": id.lineRange, "selectionRange": id.rng, "data": map[string]any{"k": id.text}}
}

// units counts the negotiated encoding's units in s.
func (f *fakeServer) units(s string) uint32 {
	switch f.enc {
	case "utf-8":
		return uint32(len(s))
	case "utf-32":
		return uint32(utf8.RuneCountInString(s))
	}
	var n uint32
	for _, r := range s {
		n++
		if r > 0xFFFF {
			n++
		}
	}
	return n
}

// byteColumn converts a column in the negotiated encoding to a byte index.
func (f *fakeServer) byteColumn(line string, units uint32) (int, error) {
	if f.enc == "utf-8" {
		if int(units) > len(line) {
			return 0, fmt.Errorf("column past line")
		}
		return int(units), nil
	}
	var n uint32
	for i, r := range line {
		if n == units {
			return i, nil
		}
		n++
		if f.enc == "utf-16" && r > 0xFFFF {
			n++
		}
	}
	if n == units {
		return len(line), nil
	}
	return 0, fmt.Errorf("column past line")
}

func uriPath(uri string) string {
	u, _ := url.Parse(uri)
	return filepath.FromSlash(u.Path)
}

// read is the fake's own framing: MIME headers, then Content-Length bytes.
func (f *fakeServer) read() ([]byte, error) {
	hdr, err := textproto.NewReader(f.in).ReadMIMEHeader()
	if err != nil {
		return nil, err
	}
	n, err := strconv.Atoi(hdr.Get("Content-Length"))
	if err != nil {
		return nil, fmt.Errorf("bad Content-Length %q", hdr.Get("Content-Length"))
	}
	buf := make([]byte, n)
	_, err = io.ReadFull(f.in, buf)
	return buf, err
}

func (f *fakeServer) send(v any) {
	payload, _ := json.Marshal(v)
	fmt.Fprintf(f.out, "Content-Length: %d\r\n\r\n%s", len(payload), payload)
}

func (f *fakeServer) reply(req message, result any, rpcErr *rpcError) {
	out := map[string]any{"jsonrpc": "2.0", "id": req.ID}
	if rpcErr != nil {
		out["error"] = rpcErr
	} else {
		out["result"] = result
	}
	f.send(out)
}

func (f *fakeServer) notify(method string, params any) {
	f.send(map[string]any{"jsonrpc": "2.0", "method": method, "params": params})
}

// request sends a server-initiated request with a string id, which the client
// must echo verbatim.
func (f *fakeServer) request(method string, params any) {
	f.nextID++
	f.send(map[string]any{"jsonrpc": "2.0", "id": fmt.Sprintf("srv-%d", f.nextID), "method": method, "params": params})
}

func (f *fakeServer) event(format string, args ...any) {
	fmt.Fprintf(f.events, format+"\n", args...)
}
