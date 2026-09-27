package worker

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"testing"

	ts "github.com/tree-sitter/go-tree-sitter"

	"github.com/Sawmonabo/codectx/internal/provider/treesitter/lang"
	"github.com/Sawmonabo/codectx/internal/provider/treesitter/wire"
)

// TestHeaderParseKeepsFewerErrorBytesAndDisclosesIt protects a header's facts
// from being extracted from the wrong grammar's tree (ADR-0012 decision 10):
// a header whose first parse has errors is parsed once with the fallback, the
// parse with fewer error bytes is the one extracted from, and the choice is
// disclosed in Done; a clean first parse and a request with no fallback are
// parsed once. Mutations that fail it: keep the first parse always, extract
// from the discarded tree, or run the fallback on every file with errors
// whether or not the request names one (the parser count and the missing
// disclosure catch it).
func TestHeaderParseKeepsFewerErrorBytesAndDisclosesIt(t *testing.T) {
	const cppHeader = "class X { public: void f(); };\n"
	cases := []struct {
		name     string
		req      wire.Request
		src      string
		kept     string
		reason   lang.HeaderReason
		parsers  int
		wantDecl string
	}{
		{name: "a C++ header parsed as C first falls back", req: wire.Request{Language: "c", Fallback: "cpp", Path: "x.h"},
			src: cppHeader, kept: "cpp", reason: lang.HeaderFallbackKept, parsers: 2, wantDecl: "X"},
		{name: "a C header parsed as C++ first falls back", req: wire.Request{Language: "cpp", Fallback: "c", Path: "x.h"},
			src: "int class = 1;\n", kept: "c", reason: lang.HeaderFallbackKept, parsers: 2, wantDecl: "class"},
		{name: "a clean first parse is kept without a second", req: wire.Request{Language: "c", Fallback: "cpp", Path: "x.h"},
			src: "int f(void);\n", kept: "c", reason: lang.HeaderClean, parsers: 1},
		{name: "a request with no fallback is parsed once", req: wire.Request{Language: "c", Path: "x.h"},
			src: cppHeader, parsers: 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := &state{parsers: map[string]*ts.Parser{}, queries: map[string]*ts.Query{}}
			defer w.close()
			tc.req.SourceBytes = uint64(len(tc.src))
			var out bytes.Buffer
			done, fail, err := w.parse(&out, tc.req, []byte(tc.src))
			if err != nil || fail != nil {
				t.Fatalf("parse: %v, %+v", err, fail)
			}
			if len(w.parsers) != tc.parsers {
				t.Fatalf("%d parsers were created, want %d", len(w.parsers), tc.parsers)
			}
			h := done.Header
			if tc.kept == "" {
				if h != nil {
					t.Fatalf("a request with no fallback disclosed a header choice %+v", *h)
				}
				if !done.SyntaxErrors {
					t.Fatal("the one parse's errors are not reported")
				}
				return
			}
			if h == nil || h.First != tc.req.Language || h.Kept != tc.kept || h.Reason != tc.reason {
				t.Fatalf("header choice %+v, want first %s kept %s for %s", h, tc.req.Language, tc.kept, tc.reason)
			}
			if tc.reason == lang.HeaderFallbackKept && (!h.FirstErr.Any || h.Fallback.Any || done.SyntaxErrors) {
				t.Fatalf("the kept fallback is not the clean parse: %+v, syntax errors %v", *h, done.SyntaxErrors)
			}
			if tc.wantDecl != "" && !emittedDecl(t, &out, tc.wantDecl) {
				t.Fatalf("no declaration %q was extracted from the kept %s parse", tc.wantDecl, tc.kept)
			}
		})
	}
}

// emittedDecl reports whether the worker's answer holds a declaration named
// name.
func emittedDecl(t *testing.T, r io.Reader, name string) bool {
	t.Helper()
	for {
		kind, payload, err := wire.ReadMessage(r, 0)
		if errors.Is(err, io.EOF) {
			return false
		}
		if err != nil {
			t.Fatal(err)
		}
		if kind != wire.KindDecl {
			continue
		}
		var d wire.Decl
		if err := json.Unmarshal(payload, &d); err != nil {
			t.Fatal(err)
		}
		if d.Name == name {
			return true
		}
	}
}
