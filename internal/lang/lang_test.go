package lang

import (
	"context"
	"errors"
	"testing"

	"github.com/Sawmonabo/codectx/internal/model"
)

// fakeParses answers ParsedLanguage from a map keyed by path and counts the
// lookups it served.
type fakeParses struct {
	kept  map[string]string
	err   error
	calls int
}

func (p *fakeParses) ParsedLanguage(_ context.Context, provider, scope, path string) (string, bool, error) {
	p.calls++
	if p.err != nil {
		return "", false, p.err
	}
	if provider != "treesitter" || scope != "file:"+path {
		return "", false, errors.New("lookup names another provider or scope: " + provider + " " + scope)
	}
	l, ok := p.kept[path]
	return l, ok, nil
}

// cSnapshot is a snapshot whose census parses headers as C first.
var cSnapshot = model.Snapshot{CUnits: 3}

// TestBoundTaggerFollowsTheKeptParse pins that a header's tag in a generation
// is the grammar that generation's parse kept, not the census's first choice.
//
// Mutations it catches: Of returning the census's first grammar for a bound
// header (a header kept as C++ after a C first choice would be tagged "c");
// answering "" instead of the first grammar for a header with no parse;
// consulting the generation for a path that is not a shared header.
func TestBoundTaggerFollowsTheKeptParse(t *testing.T) {
	parses := &fakeParses{kept: map[string]string{"inc/kept_cpp.h": "cpp", "inc/kept_c.h": "c"}}
	tag := For(cSnapshot).In(context.Background(), parses)

	for _, tc := range []struct{ path, want string }{
		{"inc/kept_cpp.h", "cpp"},
		{"inc/kept_c.h", "c"},
		{"inc/unparsed.h", "c"},
		{"src/main.c", "c"},
		{"src/main.cc", "cpp"},
	} {
		if got := tag.Of(tc.path); got != tc.want {
			t.Errorf("Of(%s) = %q, want %q", tc.path, got, tc.want)
		}
	}
	if parses.calls != 3 {
		t.Errorf("%d lookups for three headers and two units, want 3", parses.calls)
	}
	if err := tag.Err(); err != nil {
		t.Fatalf("Err = %v", err)
	}
	if got := For(cSnapshot).Of("inc/kept_cpp.h"); got != "c" {
		t.Errorf("unbound Of(header) = %q, want the census's first grammar %q", got, "c")
	}
}

// TestBoundTaggerReportsALookupFailure pins that a failed lookup is reported
// through every copy of the tagger and is never answered as a language.
//
// Mutations it catches: swallowing the error into the census's first grammar
// (a filter would then keep or drop the header on a guess); holding the
// binding by value, so a copy's failure never reaches the tagger the caller
// checks.
func TestBoundTaggerReportsALookupFailure(t *testing.T) {
	failure := errors.New("store unavailable")
	tag := For(cSnapshot).In(context.Background(), &fakeParses{err: failure})
	copied := tag
	if got := copied.Of("inc/a.h"); got != "" {
		t.Errorf("Of after a failed lookup = %q, want \"\"", got)
	}
	if err := tag.Err(); !errors.Is(err, failure) {
		t.Fatalf("Err = %v, want %v", err, failure)
	}
	if got := tag.Of("src/main.c"); got != "c" {
		t.Errorf("Of(non-header) after a failure = %q, want \"c\"", got)
	}
}
