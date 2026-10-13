package model

import (
	"strings"
	"testing"
)

// TestRepoPathAdmitsOnlyPathsInsideTheRoot protects the one rule the search
// filters, the exact path tier, context seeds, file unit scopes and source
// reads hold a path to: a path is a file of the repository only in the clean,
// slash-separated, root-relative spelling the capture stores, and a path that
// names the root, climbs above it, is absolute or is longer than a stored path
// is none.
//
// Mutations it catches (each fails a row below): dropping the climb check
// ("../x" admitted), the root check ("." admitted), the absolute check ("/etc"
// admitted), the cleanliness check ("a//b", "a/./b" or "a/" admitted), or the
// length bound; and, for CleanRepoPath, not cleaning ("a/./b" refused instead
// of read as "a/b").
func TestRepoPathAdmitsOnlyPathsInsideTheRoot(t *testing.T) {
	for _, c := range []struct {
		in        string
		isRepo    bool
		clean     string
		cleanRepo bool
	}{
		{"a/b.go", true, "a/b.go", true},
		{"..x/y", true, "..x/y", true},
		{"", false, ".", false},
		{".", false, ".", false},
		{"..", false, "..", false},
		{"../x", false, "../x", false},
		{"a/../../x", false, "../x", false},
		{"/etc/hosts", false, "/etc/hosts", false},
		{"a//b", false, "a/b", true},
		{"a/./b", false, "a/b", true},
		{"./a", false, "a", true},
		{"a/", false, "a", true},
		{"a/..", false, ".", false},
		{strings.Repeat("d/", MaxPathBytes/2) + "f", false, strings.Repeat("d/", MaxPathBytes/2) + "f", false},
	} {
		if got := IsRepoPath(c.in); got != c.isRepo {
			t.Errorf("IsRepoPath(%.40q) = %v, want %v", c.in, got, c.isRepo)
		}
		if got, ok := CleanRepoPath(c.in); got != c.clean || ok != c.cleanRepo {
			t.Errorf("CleanRepoPath(%.40q) = %.40q, %v, want %.40q, %v", c.in, got, ok, c.clean, c.cleanRepo)
		}
	}
}
