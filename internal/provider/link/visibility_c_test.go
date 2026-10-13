package link_test

import (
	"slices"
	"strings"
	"testing"

	"github.com/Sawmonabo/codectx/internal/model"
)

// TestCVisibilityThroughTheProjectsMacros checks that a C declaration
// written with a macro publishes, for each definition of that macro its file
// sees through its includes, a `references` to the definition whose detail
// is the visibility the definition states and whose set size counts the
// definitions reached: one per `#if` arm, through a one-identifier alias,
// and through a header for a name the declaring file alone could not tell
// from its type. A macro defined only in a header the file does not include,
// and a declaration written with no macro, publish nothing.
//
// Mutation: read only the first definition of a name -> f reaches one arm
// and carries no set.
// Mutation: do not follow a one-identifier replacement -> g publishes
// nothing.
// Mutation: drop the include check (read every file's definitions) -> u
// references the unincluded header's EXT2.
// Mutation: send only the proven macros, not the type candidates -> g_foo
// publishes nothing.
// Mutation: publish no set size -> f's three lines lose their set.
func TestCVisibilityThroughTheProjectsMacros(t *testing.T) {
	files := map[string]string{
		"api.h": `#ifdef BUILDING
#define API __declspec(dllexport) // @exp
#elif defined(STATIC)
#define API // @none
#else
#define API __declspec(dllimport) // @imp
#endif
#define VIS __attribute__((visibility("default"))) // @vis
#define LIB VIS // @lib
#define GLIB_AVAILABLE_IN_ALL __attribute__((visibility("hidden"))) // @glib
`,
		"other.h": `#define EXT2 __declspec(dllexport) // @ext2
`,
		"lib.h": `#include "api.h"
API int f(void); // @f
LIB int g(void); // @g
GLIB_AVAILABLE_IN_ALL gboolean g_foo (void); // @gfoo
EXT2 int u(void); // @u
int plain(void); // @plain
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
	want := []string{
		"f f references exp API visibility_dllexport set=3",
		"f f references imp API visibility_dllimport set=3",
		"f f references none API visibility_unspecified set=3",
		"g g references vis VIS visibility_default",
		"gfoo g_foo references glib GLIB_AVAILABLE_IN_ALL visibility_hidden",
	}
	if !slices.Equal(got, want) {
		t.Fatalf("visibility occurrences:\n got %q\nwant %q", got, want)
	}
}
