package worker

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"

	ts "github.com/tree-sitter/go-tree-sitter"

	"github.com/Sawmonabo/codectx/internal/provider/treesitter/flow"
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
	// C cannot absorb a template: the C grammar reads `class` as a type name
	// and `public:` as a label, so a class body alone would parse clean.
	const cppHeader = "template <typename T> struct S { T v; };\n"
	// C++ cannot absorb an identifier-list function definition, whose
	// parameters are declared between the declarator and the body (C17
	// §6.9.1, the form C23 removed): C++ has no such form ([dcl.fct.def]),
	// so its grammar recovers with errors where the C grammar parses clean.
	// A C name that is a C++ keyword is no such source: the C++ grammar
	// parses `int class = 1;` clean.
	const cHeader = "int f(a) int a; { return a; }\n"
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
			src: cppHeader, kept: "cpp", reason: lang.HeaderFallbackKept, parsers: 2, wantDecl: "S"},
		{name: "a C header parsed as C++ first falls back", req: wire.Request{Language: "cpp", Fallback: "c", Path: "x.h"},
			src: cHeader, kept: "c", reason: lang.HeaderFallbackKept, parsers: 2, wantDecl: "f"},
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

// TestPanickingFunctionDegradesOnlyItself protects decision 9's failure
// scope: a defect in one callable's lowering must cost that callable's facts
// alone, disclosed with its span and cause, never the worker, the file or the
// callables after it. The lowering here panics for the second of three Go
// functions and lowers the others as the real one does. Mutations that fail
// it: removing the per-function recover (the panic escapes the pass), sending
// the half-encoded message instead of the failed one, dropping the span or
// the cause, or ending the walk at the first failure (the third function's
// message is missing).
func TestPanickingFunctionDegradesOnlyItself(t *testing.T) {
	const src = "package p\n\nfunc a(x int) int {\n\ty := x + 1\n\treturn y\n}\n\n" +
		"func b(x int) int {\n\treturn x\n}\n\nfunc c(x int) int {\n\tz := x * 2\n\treturn z\n}\n"
	const cause = "a lowering defect in b"
	p, err := NewParser("go")
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	tree := Parse(p, []byte(src), nil)
	if tree == nil {
		t.Fatal("the parser produced no tree")
	}
	flat, err := Flatten(tree, "go")
	tree.Close()
	if err != nil {
		t.Fatalf("flatten: %v", err)
	}
	goLower, _ := LoweringFor("go")
	target := uint(strings.Index(src, "func b"))
	failing := &Lowering{language: goLower.language, callables: goLower.callables, ambient: goLower.ambient,
		lower: func(l *Lowering, b *flow.Builder, fn Node, text []byte, s *Scratch) {
			if fn.StartByte() == target {
				panic(cause)
			}
			goLower.lower(l, b, fn, text, s)
		}}
	failing.resolve()

	var w state
	defer w.release()
	w.collect(failing, flat)
	var out bytes.Buffer
	if err := w.functions(&out, failing, []byte(src)); err != nil {
		t.Fatal(err)
	}
	var got []wire.Function
	for {
		kind, payload, err := wire.ReadMessage(&out, 0)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil || kind != wire.KindFunction {
			t.Fatalf("message %d: kind %d, %v", len(got), kind, err)
		}
		f, err := wire.DecodeFunction(payload)
		if err != nil {
			t.Fatalf("message %d: %v", len(got), err)
		}
		got = append(got, f)
	}
	if len(got) != 3 {
		t.Fatalf("%d function messages, want 3, one per callable", len(got))
	}
	end := uint32(strings.Index(src, "func c") - 2)
	if f := got[1]; !f.Failed || f.Span != (wire.Span{Start: uint32(target), End: end}) || f.Cause != cause ||
		len(f.Vars)+len(f.Reads)+len(f.Writes) != 0 {
		t.Fatalf("the panicking function's message is %+v, want failed over [%d, %d) with cause %q and no facts",
			f, target, end, cause)
	}
	for _, i := range []int{0, 2} {
		if f := got[i]; f.Failed || len(f.Reads) == 0 || len(f.Writes) == 0 {
			t.Fatalf("function %d lost its facts beside the failed one: %+v", i, f)
		}
	}
}
