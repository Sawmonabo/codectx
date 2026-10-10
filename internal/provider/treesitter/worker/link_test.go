package worker

import (
	"bytes"
	"errors"
	"io"
	"strings"
	"testing"

	ts "github.com/tree-sitter/go-tree-sitter"

	"github.com/Sawmonabo/codectx/internal/provider/treesitter/wire"
)

// linkOf runs the worker over src as language at path and answers the file's
// link message and its function messages. It fails the test when the file's
// dependence or link pass failed or no link message was sent.
func linkOf(t *testing.T, language, path, src string) (wire.Link, []wire.Function) {
	t.Helper()
	w := &state{parsers: map[string]*ts.Parser{}, queries: map[string]*ts.Query{}}
	defer w.close()
	var out bytes.Buffer
	done, fail, err := w.parse(&out, wire.Request{Language: language, Path: path, SourceBytes: uint64(len(src))}, []byte(src))
	if err != nil || fail != nil {
		t.Fatalf("parse: %v, %+v", err, fail)
	}
	if done.DependenceFailure != "" || done.LinkFailure != "" {
		t.Fatalf("the pass failed: dependence %q, link %q", done.DependenceFailure, done.LinkFailure)
	}
	var (
		l     wire.Link
		fns   []wire.Function
		found bool
	)
	for {
		kind, payload, err := wire.ReadMessage(&out, 0)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		switch kind {
		case wire.KindFunction:
			f, err := wire.DecodeFunction(payload)
			if err != nil {
				t.Fatal(err)
			}
			fns = append(fns, f)
		case wire.KindLink:
			if l, err = wire.DecodeLink(payload); err != nil {
				t.Fatal(err)
			}
			found = true
		}
	}
	if !found {
		t.Fatal("no link message was sent")
	}
	return l, fns
}

// textAt is the span of the nth (0-based) occurrence of needle in src.
func textAt(t *testing.T, src, needle string, nth int) wire.Span {
	t.Helper()
	off := 0
	for i := 0; ; i++ {
		j := strings.Index(src[off:], needle)
		if j < 0 {
			t.Fatalf("occurrence %d of %q is not in the source", nth, needle)
		}
		if i == nth {
			return wire.Span{Start: uint32(off + j), End: uint32(off + j + len(needle))}
		}
		off += j + len(needle)
	}
}

// siteNamed is the site of l whose callee token is name, or nil.
func siteNamed(l wire.Link, name wire.Span) *wire.Site {
	for i := range l.Sites {
		if l.Sites[i].Name == name {
			return &l.Sites[i]
		}
	}
	return nil
}

// exportNamed is the export of l whose name token is name, or nil.
func exportNamed(l wire.Link, name wire.Span) *wire.Export {
	for i := range l.Exports {
		if l.Exports[i].Name == name {
			return &l.Exports[i]
		}
	}
	return nil
}

// declaredVar reports whether some function message names the variable
// declared at s, so a read the link message sends joins a variable node.
func declaredVar(fns []wire.Function, s wire.Span) bool {
	for _, f := range fns {
		for _, v := range f.Vars {
			if v == s {
				return true
			}
		}
	}
	return false
}

// wantArguments checks a site's arguments against want: each argument's
// expression text, keyword, spread and the texts of the identifiers its
// reads name, and that every read is a variable a function message names.
func wantArguments(t *testing.T, src string, fns []wire.Function, s *wire.Site, want []argWant) {
	t.Helper()
	if s == nil {
		t.Fatal("the call sent no site")
	}
	if len(s.Arguments) != len(s.Args) {
		t.Fatalf("%d arguments for %d argument types", len(s.Arguments), len(s.Args))
	}
	if len(s.Arguments) != len(want) {
		t.Fatalf("%d arguments %+v, want %d", len(s.Arguments), s.Arguments, len(want))
	}
	for i, a := range s.Arguments {
		w := want[i]
		if got := src[a.At.Start:a.At.End]; got != w.at || a.Keyword != w.keyword || a.Spread != w.spread {
			t.Fatalf("argument %d is %q keyword %q spread %d, want %q keyword %q spread %d", i, got, a.Keyword, a.Spread,
				w.at, w.keyword, w.spread)
		}
		if len(a.Reads) != len(w.reads) {
			t.Fatalf("argument %d (%q) reads %+v, want the variables declared at %v", i, w.at, a.Reads, w.reads)
		}
		for j, r := range a.Reads {
			if r != w.reads[j] {
				t.Fatalf("argument %d (%q) read %d is %q at %v, want %v", i, w.at, j, src[r.Start:r.End], r, w.reads[j])
			}
			if !declaredVar(fns, r) {
				t.Fatalf("argument %d (%q) reads %v, which no function message names", i, w.at, r)
			}
		}
	}
}

// argWant is one expected argument: its expression text, keyword, spread and
// the declaring identifiers of the variables it reads.
type argWant struct {
	at      string
	keyword string
	spread  wire.Spread
	reads   []wire.Span
}
