package scip

import "testing"

// TestAProjectPathIsResolvedAgainstTheWorkspace protects two halves of one
// rule. A project unit's document that reaches a workspace file through `../`
// must be admitted under that file's workspace path, or its facts are lost
// while the file is served; and a path that still escapes the workspace, or an
// absolute one, must stay refused, or the index would name bytes outside the
// snapshot. Dropping the clean in workspacePath fails the first row; joining
// an absolute path fails the fourth.
func TestAProjectPathIsResolvedAgainstTheWorkspace(t *testing.T) {
	for _, c := range []struct {
		prefix, raw, want string
		admitted          bool
	}{
		{"app", "../shared/x.ts", "shared/x.ts", true},
		{"app", "src/x.ts", "app/src/x.ts", true},
		{"app", "../../cache/x.go", "../cache/x.go", false},
		{"app", "/etc/x", "/etc/x", false},
		{"app", "..", ".", false},
		{"", "a/../b.go", "a/../b.go", false},
	} {
		got := workspacePath(c.prefix, c.raw)
		if got != c.want || rootRelative(got) != c.admitted {
			t.Errorf("workspacePath(%q, %q) = %q admitted %v, want %q admitted %v",
				c.prefix, c.raw, got, rootRelative(got), c.want, c.admitted)
		}
	}
}
