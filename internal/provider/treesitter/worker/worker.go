// Package worker is the parser side of the tree-sitter provider: a
// subprocess that owns every native object (parser, tree, query, query
// cursor) and never touches storage, identities or the repository. The parent
// streams one file at a time over stdin as framed bytes; the worker parses
// it with the pinned grammar, runs the structural query pack and answers with
// framed, language-neutral facts. It is started only through the shared
// process runner, so its lifetime, environment and output are bounded there.
//
// Native lifecycle (go-tree-sitter v0.25.0, files read in full: parser.go,
// tree.go, query.go, node.go, tree_cursor.go, language.go, allocator.go):
// Parser.Close, Tree.Close, Query.Close and QueryCursor.Close free the C
// objects; nothing is finalized by the garbage collector. Parsing uses
// Parser.ParseWithOptions with a chunked read callback and nil options: the
// options path saves a cgo pointer per call that the binding never releases
// (parser.go:350), so it is not used; the deprecated timeout, cancellation
// flag and ParseCtx APIs are not used either. Cancellation is the parent's:
// it terminates the process through the runner, which releases everything
// at once. QueryCursor.Matches is used rather than Captures, whose Next
// mallocs a TSQueryMatch it never frees (query.go:1049).
package worker

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"

	ts "github.com/tree-sitter/go-tree-sitter"

	"github.com/Sawmonabo/codectx/internal/provider/treesitter/lang"
	"github.com/Sawmonabo/codectx/internal/provider/treesitter/wire"
)

// Exit codes. A non-zero exit after hello is a worker defect the parent
// treats as unhealthy; a failure before hello is reported on stderr.
const (
	exitOK       = 0
	exitBadInput = 1
	exitMismatch = 2
)

// Main runs the worker loop until stdin ends or ctx is done. It is the entry
// the hidden `codectx __ts-worker` subcommand calls (ruling R8-1).
func Main(ctx context.Context, stdin io.Reader, stdout, stderr io.Writer) int {
	w := &state{parsers: map[string]*ts.Parser{}, queries: map[string]*ts.Query{}}
	defer w.close()
	if err := w.verify(); err != nil {
		fmt.Fprintln(stderr, "treesitter worker:", err)
		return exitMismatch
	}
	in := bufio.NewReaderSize(stdin, 64<<10)
	out := bufio.NewWriterSize(stdout, 64<<10)
	names := make([]string, 0, len(lang.All))
	for _, l := range lang.All {
		names = append(names, l.Name)
	}
	if err := wire.WriteJSON(out, wire.KindHello, wire.Hello{PID: os.Getpid(), Fingerprint: lang.Fingerprint(), Languages: names}, wire.MaxFactFrameBytes); err != nil {
		return exitBadInput
	}
	if err := out.Flush(); err != nil {
		return exitBadInput
	}
	for ctx.Err() == nil {
		kind, payload, err := wire.Read(in, wire.MaxFactFrameBytes)
		if errors.Is(err, io.EOF) {
			return exitOK
		}
		if err != nil || kind != wire.KindRequest {
			fmt.Fprintln(stderr, "treesitter worker: malformed request frame")
			return exitBadInput
		}
		var req wire.Request
		if err := json.Unmarshal(payload, &req); err != nil || req.SourceBytes > wire.MaxSourceBytes {
			fmt.Fprintln(stderr, "treesitter worker: malformed request")
			return exitBadInput
		}
		kind, src, err := wire.Read(in, int(req.SourceBytes))
		if err != nil || kind != wire.KindSource || len(src) != int(req.SourceBytes) {
			fmt.Fprintln(stderr, "treesitter worker: malformed source frame")
			return exitBadInput
		}
		if err := w.serve(out, req, src); err != nil {
			return exitBadInput
		}
		if err := out.Flush(); err != nil {
			return exitBadInput
		}
	}
	return exitOK
}

// state is the worker's native ownership: at most one parser and one compiled
// query per language, created lazily and closed when the loop ends.
type state struct {
	parsers map[string]*ts.Parser
	queries map[string]*ts.Query
}

func (w *state) close() {
	for _, p := range w.parsers {
		p.Close()
	}
	for _, q := range w.queries {
		q.Close()
	}
}

// verify checks every linked grammar against the pin table: ABI and, where
// the grammar carries it, its own version. A mismatch means the binary was
// built against grammars the fingerprint does not describe.
func (w *state) verify() error {
	for _, l := range lang.All {
		g, ok := grammars[l.Name]
		if !ok {
			return fmt.Errorf("language %q is pinned but not linked", l.Name)
		}
		tl := ts.NewLanguage(g.language())
		if abi := tl.AbiVersion(); abi != l.ABI {
			return fmt.Errorf("grammar %s has ABI %d, pinned %d", l.Name, abi, l.ABI)
		}
		if m := tl.Metadata(); m != nil {
			got := strconv.Itoa(int(m.MajorVersion)) + "." + strconv.Itoa(int(m.MinorVersion)) + "." + strconv.Itoa(int(m.PatchVersion))
			if got != l.Metadata {
				return fmt.Errorf("grammar %s reports version %s, pinned %q", l.Name, got, l.Metadata)
			}
		} else if l.Metadata != "" {
			return fmt.Errorf("grammar %s carries no version metadata, pinned %q", l.Name, l.Metadata)
		}
	}
	return nil
}

// serve answers one request. A per-file failure is an error frame and the
// worker stays healthy; a write failure ends the loop.
func (w *state) serve(out io.Writer, req wire.Request, src []byte) error {
	l, ok := lang.Lookup(req.Language)
	g := grammars[req.Language]
	if !ok || g == nil {
		return wire.WriteJSON(out, wire.KindError, wire.Error{Code: "CTX_ARGUMENT_INVALID", Message: "unsupported language " + req.Language}, wire.MaxFactFrameBytes)
	}
	parser, query, err := w.tools(l, g)
	if err != nil {
		return wire.WriteJSON(out, wire.KindError, wire.Error{Code: "CTX_INTERNAL", Message: err.Error()}, wire.MaxFactFrameBytes)
	}
	tree := parser.ParseWithOptions(func(offset int, _ ts.Point) []byte {
		if offset >= len(src) {
			return nil
		}
		return src[offset:min(offset+parseChunkBytes, len(src))]
	}, nil, nil)
	if tree == nil {
		parser.Reset()
		return wire.WriteJSON(out, wire.KindError, wire.Error{Code: "CTX_PROVIDER_OUTPUT_INVALID", Message: "the parser produced no tree"}, wire.MaxFactFrameBytes)
	}
	defer tree.Close()
	root := tree.RootNode()
	em := &emitter{w: out}
	ex := &extraction{g: g, l: l, path: req.Path, src: src, maxRecords: req.MaxRecordsPerFile}
	if err := ex.run(query, root, em); err != nil {
		return err
	}
	done := wire.Done{Package: ex.pkg, SyntaxErrors: root.HasError(), Truncated: ex.truncated || em.truncated}
	if rss, ok := wire.ResidentBytes(); ok {
		done.RSSBytes = uint64(rss)
	}
	return wire.WriteJSON(out, wire.KindDone, done, wire.MaxFactFrameBytes)
}

// tools returns the language's parser and compiled query, creating them once.
func (w *state) tools(l lang.Language, g *grammar) (*ts.Parser, *ts.Query, error) {
	tl := ts.NewLanguage(g.language())
	p, ok := w.parsers[l.Name]
	if !ok {
		p = ts.NewParser()
		if err := p.SetLanguage(tl); err != nil {
			p.Close()
			return nil, nil, err
		}
		w.parsers[l.Name] = p
	}
	q, ok := w.queries[l.Name]
	if !ok {
		var qerr *ts.QueryError
		q, qerr = ts.NewQuery(tl, l.Query)
		if qerr != nil {
			return nil, nil, fmt.Errorf("query pack %s: %s", l.Name, qerr.Error())
		}
		w.queries[l.Name] = q
	}
	return p, q, nil
}

// emitter writes fact frames, dropping a record whose encoding would exceed
// the per-frame cap and reporting the truncation instead of a bypass.
type emitter struct {
	w         io.Writer
	truncated bool
}

func (e *emitter) put(kind wire.Kind, v any) error {
	err := wire.WriteJSON(e.w, kind, v, wire.MaxFactFrameBytes)
	if errors.Is(err, wire.ErrFrameTooLarge) {
		e.truncated = true
		return nil
	}
	return err
}

func (e *emitter) decl(d wire.Decl) error  { return e.put(wire.KindDecl, d) }
func (e *emitter) imp(i wire.Import) error { return e.put(wire.KindImport, i) }
func (e *emitter) ref(r wire.Ref) error    { return e.put(wire.KindRef, r) }
