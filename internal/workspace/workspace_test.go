package workspace

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/Sawmonabo/codectx/internal/model"
)

// safeTree is the one shared workspace safety fixture. It builds a repository
// containing the path shapes that decide ordering and confinement:
//
//   - "a.txt" and "a/b", whose correct relative order ('.' sorts before '/')
//     differs from a naive per-directory name sort;
//   - a ".git" directory and a data directory, which are excluded
//     unconditionally;
//   - a vendor directory, excluded by policy;
//   - a symlink pointing outside the root, which must never be traversed.
//
// It returns the opened root and the absolute path of the outside target.
func safeTree(t *testing.T) (Root, string) {
	t.Helper()
	base := t.TempDir()
	root := filepath.Join(base, "repo")
	outside := filepath.Join(base, "outside.txt")
	mustWrite(t, outside, "outside")
	for _, rel := range []string{
		"a.txt",
		"a/b",
		"z.txt",
		"sub/dir/file.go",
		".git/config",
		"vendor/lib.go",
		"data/blobs/object",
	} {
		mustWrite(t, filepath.Join(root, filepath.FromSlash(rel)), rel)
	}
	if err := os.Symlink(outside, filepath.Join(root, "escape")); err != nil {
		t.Skipf("symlinks are unavailable in this environment: %v", err)
	}
	if err := os.Symlink(filepath.Join(root, "a.txt"), filepath.Join(root, "inside-link")); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	opened, err := Discover(root)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	t.Cleanup(func() { opened.Close() })
	if !opened.HasGit {
		t.Fatalf("Discover(%q) did not report the .git directory", root)
	}
	if opened.Path != root {
		t.Fatalf("Discover returned root %q, want %q", opened.Path, root)
	}
	return opened, outside
}

func mustWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func testPolicy(root Root) Policy {
	return Policy{DataDir: filepath.Join(root.Path, "data"), MaxFiles: 1000}
}

// TestRootConfinement protects the repository-safety invariant of Section 21:
// a path that leaves the workspace must be rejected by the opener itself, not
// by string inspection that a symlink or an absolute path can defeat. Serving
// bytes from outside the workspace would attribute unrelated content to the
// repository under analysis.
func TestRootConfinement(t *testing.T) {
	root, outsideAbs := safeTree(t)
	for _, tc := range []struct {
		name string
		rel  string
	}{
		{"parent traversal", "../outside.txt"},
		{"embedded traversal", "sub/../../outside.txt"},
		{"absolute path", outsideAbs},
		{"nul byte", "a\x00.txt"},
		{"symlink leaving the root", "escape"},
		{"symlink inside the root", "inside-link"},
		{"empty path", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, err := root.Open(tc.rel)
			if err == nil {
				f.Close()
				t.Fatalf("Open(%q) succeeded; it must not reach outside the workspace or open a non-regular file", tc.rel)
			}
			var typed *model.Error
			if !errors.As(err, &typed) || typed.Code != model.CodePathEscape {
				t.Fatalf("Open(%q) returned %v, want a typed %s", tc.rel, err, model.CodePathEscape)
			}
		})
	}
}

// TestWalkOrderIsCanonical protects determinism: the walk must stream files in
// lexicographic order of their root-relative path, which is not the same as a
// per-directory name sort. A different order changes the manifest hash and
// therefore the SnapshotID for identical bytes.
func TestWalkOrderIsCanonical(t *testing.T) {
	root, _ := safeTree(t)
	want := []string{"a.txt", "a/b", "sub/dir/file.go", "z.txt"}
	for run := range 2 {
		got := collect(t, root, testPolicy(root))
		if len(got) != len(want) {
			t.Fatalf("run %d walked %v, want %v", run, got, want)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("run %d walked %v, want %v", run, got, want)
			}
		}
	}
}

// TestWalkForceIncludeOverridesPolicy proves the Section 10.2 rule that a
// tracked path wins over an ignore or policy exclusion, while the unconditional
// exclusions stay unconditional.
func TestWalkForceIncludeOverridesPolicy(t *testing.T) {
	root, _ := safeTree(t)
	policy := testPolicy(root)
	policy.Ignore = func(rel string, isDir bool) bool { return rel == "a.txt" }
	policy.ForceInclude = func(rel string) bool {
		return rel == "vendor/lib.go" || rel == "a.txt" || rel == ".git/config" || rel == "data/blobs/object"
	}

	// Without a directory-level answer, an excluded directory is not read at
	// all. A file hook alone must not turn every excluded directory into a full
	// traversal: that is what makes a large ignored tree an expense, and its
	// entry cap a way to abort the whole walk.
	assertWalk(t, root, policy, []string{"a.txt", "a/b", "sub/dir/file.go", "z.txt"})

	var asked []string
	policy.ForceIncludeDir = func(relDir string) bool {
		asked = append(asked, relDir)
		return relDir == "vendor"
	}
	assertWalk(t, root, policy, []string{"a.txt", "a/b", "sub/dir/file.go", "vendor/lib.go", "z.txt"})
	for _, dir := range asked {
		if dir == "sub" || dir == "a" {
			t.Errorf("ForceIncludeDir was consulted for %q, which is not excluded", dir)
		}
	}
}

func assertWalk(t *testing.T, root Root, policy Policy, want []string) {
	t.Helper()
	got := collect(t, root, policy)
	if len(got) != len(want) {
		t.Fatalf("walked %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("walked %v, want %v", got, want)
		}
	}
}

// TestWalkRejectsFollowedEscape protects the opt-in symlink path: enabling
// follow_symlinks must not become a way out of the workspace.
func TestWalkRejectsFollowedEscape(t *testing.T) {
	root, _ := safeTree(t)
	policy := testPolicy(root)
	policy.FollowSymlinks = true
	err := Walk(context.Background(), root, policy, func(File) error { return nil })
	var typed *model.Error
	if !errors.As(err, &typed) || typed.Code != model.CodePathEscape {
		t.Fatalf("Walk returned %v, want a typed %s for a symlink leaving the root", err, model.CodePathEscape)
	}
}

func collect(t *testing.T, root Root, policy Policy) []string {
	t.Helper()
	var got []string
	if err := Walk(context.Background(), root, policy, func(f File) error {
		got = append(got, f.Path)
		return nil
	}); err != nil {
		t.Fatalf("Walk: %v", err)
	}
	return got
}
