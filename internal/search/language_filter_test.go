package search

import (
	"testing"

	"github.com/Sawmonabo/codectx/internal/model"
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
