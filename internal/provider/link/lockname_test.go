package link_test

import (
	"testing"

	"github.com/Sawmonabo/codectx/internal/provider/link"
)

// TestLockFilesAreUnitInputs checks that every lock file the manifest
// provider reads is an input of the unit of its language: a change to it
// invalidates the unit. A PEP 751 lock may carry a name
// (`pylock.<name>.toml`, the name without a dot); a dotted name is not a
// lock file.
//
// Mutation: drop the named-lock predicate from OwnsInput -> pylock.dev.toml
// is not an input. Mutation: accept any name -> pylock.a.b.toml is one.
// Mutation: leave npm-shrinkwrap.json or buildscript-gradle.lockfile out of
// the markers -> it is not an input.
func TestLockFilesAreUnitInputs(t *testing.T) {
	cases := []struct {
		family link.Family
		path   string
		owned  bool
	}{
		{link.FamilyPython, "svc/pylock.toml", true},
		{link.FamilyPython, "svc/pylock.dev.toml", true},
		{link.FamilyPython, "svc/pylock.a.b.toml", false},
		{link.FamilyPython, "svc/pylock..toml", false},
		{link.FamilyJavaScript, "svc/npm-shrinkwrap.json", true},
		{link.FamilyJava, "svc/buildscript-gradle.lockfile", true},
	}
	for _, tc := range cases {
		u := link.Unit{ScopeKey: "pkg:" + string(tc.family) + ":svc", Family: tc.family, Root: "svc"}
		if got := u.OwnsInput(tc.path); got != tc.owned {
			t.Errorf("%s unit owns %s = %v, want %v", tc.family, tc.path, got, tc.owned)
		}
	}
}
