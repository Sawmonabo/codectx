package link_test

import (
	"slices"
	"strings"
	"testing"

	"github.com/Sawmonabo/codectx/internal/model"
)

// TestCVisibilityThroughTheIncludeClosure checks that a C declaration
// written with a macro reaches the macro's definition in a header the file
// includes only through other headers, as the preprocessor reads it: a
// quoted include beside the including file, then an angled one resolved by
// its path suffix, then a header that includes the first one back. A macro
// defined in a header no file of the closure includes publishes nothing.
//
// Mutation: read only the file and the headers it includes directly -> a
// publishes nothing.
// Mutation: no reached set (a header read again on every inclusion) -> the
// b.h / c.h cycle never ends.
// Mutation: drop the closure check (read every file's definitions) -> z
// references far.h's FAR.
// Mutation: resolve an angled include beside the including file only -> c.h
// is never reached and a publishes nothing.
func TestCVisibilityThroughTheIncludeClosure(t *testing.T) {
	files := map[string]string{
		"src/a.c": `#include "b.h"
DEEP int a(void); // @a
FAR int z(void); // @z
`,
		"src/b.h": `#include <sub/c.h>
`,
		"inc/sub/c.h": `#include "../../src/b.h"
#define DEEP __attribute__((visibility("default"))) // @deep
`,
		"far.h": `#define FAR __declspec(dllexport) // @far
`,
	}
	l := linkSources(t, newStructural(t), files)
	paths := l.paths(t)
	var got []string
	for _, r := range l.out.rels {
		if r.Relation.Kind != model.RelReferences {
			continue
		}
		from, to := l.out.nodes[r.Relation.From], l.out.nodes[r.Relation.To]
		for _, e := range r.Evidence {
			if strings.HasPrefix(e.Detail, "visibility_") {
				got = append(got, l.target(paths, from)+" references "+l.target(paths, to)+" "+e.Detail+setSuffix(e))
			}
		}
	}
	slices.Sort(got)
	want := []string{"a a references deep DEEP visibility_default"}
	if !slices.Equal(got, want) {
		t.Fatalf("visibility occurrences:\n got %q\nwant %q", got, want)
	}
}
