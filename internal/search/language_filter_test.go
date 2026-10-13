package search

import (
	"context"
	"slices"
	"testing"

	"github.com/Sawmonabo/codectx/internal/lang"
	"github.com/Sawmonabo/codectx/internal/model"
	tslang "github.com/Sawmonabo/codectx/internal/provider/treesitter/lang"
)

// TestAHeaderIsFilteredByTheLanguageItsParseKept proves the language filter
// tags a header with the grammar the pinned generation's parse of it kept, not
// with the census's first choice: the repository has C translation units and
// no C++ ones, so headers are parsed as C first, but this header's kept parse
// is C++ (the fallback, as when the C parse had more error bytes). Filtered to
// cpp it is served; filtered to c it is not.
//
// Mutation: bind the filter to the snapshot's tagger alone (drop .In(ctx,
// parses) from hitFilter.pin) and the header is tagged c: served under c and
// dropped under cpp.
func TestAHeaderIsFilteredByTheLanguageItsParseKept(t *testing.T) {
	const header = "inc/shape.h"
	f := newRowsFixtureCounting(t, []rowDecl{{scope: "file:" + header, binding: model.SourceBindingVerified,
		key: "module", name: "shape.h", qualifiedName: header, path: header, kind: model.NodeModule,
		language: "cpp", body: "struct Shape { double area; };"}}, 1, 0)
	s := newService(t, f.opts)
	for _, c := range []struct {
		language string
		served   bool
	}{{"cpp", true}, {"c", false}} {
		page, err := s.Search(f.ctx, model.SearchRequest{Query: "area", Languages: []string{c.language},
			Page: model.PageRequest{Limit: 10}})
		if err != nil {
			t.Fatalf("Search(languages %s): %v", c.language, err)
		}
		served := false
		for _, h := range page.Items {
			if h.Path == header {
				served = true
			}
		}
		if served != c.served {
			t.Errorf("Search(languages %s) served the C++-parsed header: %v, want %v", c.language, served, c.served)
		}
	}
}

// countingParses answers every header with one kept grammar and counts the
// lookups.
type countingParses struct {
	kept    string
	lookups int
}

func (c *countingParses) ParsedLanguage(context.Context, string, string, string) (string, bool, error) {
	c.lookups++
	return c.kept, true, nil
}

// TestAHeaderFilterThatNamesEveryOrNoHeaderGrammarLooksNothingUp proves the
// language filter decides a shared header without a lookup when the kept
// grammar cannot change its verdict: naming both grammars that declare ".h"
// keeps the header, naming neither drops it, and in both cases the filter
// binds no tagger and makes no lookup. Naming one of them still asks the
// generation which grammar the header's parse kept.
//
// Mutations: drop the shortcut (keep tags every header: one lookup per
// header for "c,cpp" and for "go"); decide an extension the filter names only
// some grammars of (the "c" filter then keeps the C++-parsed header without
// asking, or drops a C one).
func TestAHeaderFilterThatNamesEveryOrNoHeaderGrammarLooksNothingUp(t *testing.T) {
	var grammars []string
	for _, l := range tslang.All {
		if slices.Contains(l.Extensions, ".h") {
			grammars = append(grammars, l.Name)
		}
	}
	if len(grammars) < 2 {
		t.Fatalf("the grammars declaring .h are %v; the case needs a shared header", grammars)
	}
	for _, c := range []struct {
		languages []string
		kept      string
		keep      bool
		lookups   int
	}{
		{grammars, "cpp", true, 0},
		{[]string{"go"}, "cpp", false, 0},
		{[]string{"c"}, "cpp", false, 1},
		{[]string{"c"}, "c", true, 1},
	} {
		f, err := newHitFilter(model.SearchRequest{Query: "x", Languages: c.languages})
		if err != nil {
			t.Fatal(err)
		}
		parses := &countingParses{kept: c.kept}
		if !f.headersDecided {
			f.tag = lang.For(model.Snapshot{}).In(context.Background(), parses)
		}
		if got := f.keep("inc/shape.h"); got != c.keep {
			t.Errorf("languages %v, kept %s: keep(inc/shape.h) = %v, want %v", c.languages, c.kept, got, c.keep)
		}
		if parses.lookups != c.lookups || f.headersDecided != (c.lookups == 0) {
			t.Errorf("languages %v: %d lookups, decided %v; want %d", c.languages, parses.lookups, f.headersDecided,
				c.lookups)
		}
		if f.keep("cmd/main.go") != slices.Contains(c.languages, "go") {
			t.Errorf("languages %v: a Go file's verdict is wrong", c.languages)
		}
	}
}
