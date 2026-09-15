package workspace

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/Sawmonabo/codectx/internal/model"
	"github.com/Sawmonabo/codectx/internal/pagination"
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
	// An empty directory: ReadDir reports io.EOF for it, which once aborted
	// the whole walk (and every snapshot capture) as CTX_PATH_ESCAPE.
	if err := os.MkdirAll(filepath.Join(root, "empty"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
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

// TestWalkDirsAdmitsEmptyDirectoriesAndPrunesExcluded protects the watch set's
// derivation, which is the only consumer of WalkDirs. Two failure modes are
// life-or-death for freshness, and both are silent:
//
//   - an admitted directory that holds no file yet must still be emitted, or
//     no watch is placed on it and the first file written there is invisible
//     to notification while the index keeps reporting complete coverage;
//   - an excluded directory must not be emitted unless the policy claims a
//     forced path lives inside it, or an ignored build tree is watched and one
//     `npm install` overflows the queue into a full reconciliation of a tree
//     that is not indexed at all.
func TestWalkDirsAdmitsEmptyDirectoriesAndPrunesExcluded(t *testing.T) {
	root, _ := safeTree(t)
	policy := testPolicy(root)

	// "empty" holds no file at all and must still be admitted; ".git", the
	// data directory and "vendor" must not be.
	assertWalkDirs(t, root, policy, []string{".", "a", "empty", "sub", "sub/dir"})

	// A forced path inside an excluded tree makes that tree's directories
	// watchable again, exactly as Walk descends into them.
	policy.ForceIncludeDir = func(relDir string) bool { return relDir == "vendor" }
	assertWalkDirs(t, root, policy, []string{".", "a", "empty", "sub", "sub/dir", "vendor"})
}

func assertWalkDirs(t *testing.T, root Root, policy Policy, want []string) {
	t.Helper()
	var got []string
	if err := WalkDirs(context.Background(), root, policy, func(dir string) error {
		got = append(got, dir)
		return nil
	}); err != nil {
		t.Fatalf("WalkDirs: %v", err)
	}
	if len(got) != len(want) {
		t.Fatalf("WalkDirs yielded %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("WalkDirs yielded %v, want %v", got, want)
		}
	}
}

// TestFileBudgetIsReportedNotRefused protects the scale-posture ruling at the
// walk's own budget: a repository holding more files than a user-set
// workspace.max_files is captured in full, and the operator learns the budget
// was passed. The behaviour this replaces refused the whole repository, so a
// test that asserts only the refusal would pin the defect.
func TestFileBudgetIsReportedNotRefused(t *testing.T) {
	root, _ := safeTree(t)
	for i := range 5 {
		mustWrite(t, filepath.Join(root.Path, "f"+strconv.Itoa(i)+".go"), "package p\n")
	}
	// Unlimited is the default: it emits the whole tree and reports nothing.
	policy := testPolicy(root)
	var reports []string
	policy.OnSkip = func(rel, reason string) { reports = append(reports, reason+" "+rel) }
	whole := 0
	if err := Walk(t.Context(), root, policy, func(File) error { whole++; return nil }); err != nil {
		t.Fatalf("Walk with an unlimited budget: %v", err)
	}
	if whole < 5 || len(reports) != 0 {
		t.Fatalf("unlimited walk emitted %d files with reports %q, want the whole tree and no report", whole, reports)
	}

	// A user-set budget far below the tree emits exactly the same files and
	// reports the budget once.
	policy.MaxFiles, reports = 2, nil
	emitted := 0
	if err := Walk(t.Context(), root, policy, func(File) error { emitted++; return nil }); err != nil {
		t.Fatalf("Walk over a repository past its file budget: %v", err)
	}
	if emitted != whole {
		t.Fatalf("the walk emitted %d files under a budget of 2, want all %d: a budget reports, it never clamps", emitted, whole)
	}
	if len(reports) != 1 || !strings.HasPrefix(reports[0], SkipFileBudget+" ") {
		t.Fatalf("reports = %q, want exactly one %s report", reports, SkipFileBudget)
	}
}

// TestLongPathSkipsOnePathNotTheWalk protects the skip-and-report contract for
// model.MaxPathBytes: one unrepresentable path must cost that path and nothing
// else. Failing the walk -- the behaviour this replaces -- turned a single deep
// generated path into a repository that cannot be captured at all.
//
// The tree is built by descending one component at a time, because the whole
// path is longer than the PATH_MAX a single syscall argument may carry.
func TestLongPathSkipsOnePathNotTheWalk(t *testing.T) {
	root, _ := safeTree(t)
	mustWrite(t, filepath.Join(root.Path, "ok.go"), "package p\n")

	component := strings.Repeat("d", 200)
	func() {
		t.Chdir(root.Path)
		for depth := 0; depth*(len(component)+1) <= model.MaxPathBytes; depth++ {
			if err := os.Mkdir(component, 0o700); err != nil {
				t.Fatalf("mkdir at depth %d: %v", depth, err)
			}
			if err := os.Chdir(component); err != nil {
				t.Fatalf("chdir at depth %d: %v", depth, err)
			}
		}
		if err := os.WriteFile("buried.go", []byte("package p\n"), 0o600); err != nil {
			t.Fatalf("write the buried file: %v", err)
		}
	}()

	policy := testPolicy(root)
	var reports []string
	policy.OnSkip = func(_, reason string) { reports = append(reports, reason) }
	var seen []string
	if err := Walk(t.Context(), root, policy, func(f File) error { seen = append(seen, f.Path); return nil }); err != nil {
		t.Fatalf("Walk over a tree holding one over-long path: %v", err)
	}
	if !slices.Contains(seen, "ok.go") {
		t.Fatalf("the walk emitted %d files and not ok.go; one over-long path must not cost the rest of the repository", len(seen))
	}
	if !slices.Contains(reports, SkipPathTooLong) {
		t.Fatalf("reports = %q, want a %s report: a skipped path that nothing reports is a silent loss", reports, SkipPathTooLong)
	}
}

// TestDiscoveryAscendsPastAnyNestingDepth protects the discovery invariant a
// fixed ascent cap silently broke: a workspace nested deeper than the cap was
// answered with itself as the root and HasGit false -- a wrong answer, not a
// refused one, so every command that traverses it captured the wrong tree. The
// ascent needs no cap because filepath.Dir is lexical and strictly shortens the
// path, so the parent == dir test is the only termination the loop can need.
func TestDiscoveryAscendsPastAnyNestingDepth(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "repo")
	mustWrite(t, filepath.Join(root, ".git", "config"), "[core]\n")

	const depth = 300 // past the 256-component cap this replaces.
	deepest := root
	func() {
		t.Chdir(root)
		for range depth {
			if err := os.Mkdir("d", 0o700); err != nil {
				t.Fatalf("mkdir at depth: %v", err)
			}
			if err := os.Chdir("d"); err != nil {
				t.Fatalf("chdir: %v", err)
			}
			deepest = filepath.Join(deepest, "d")
		}
	}()

	opened, err := Discover(deepest)
	if err != nil {
		t.Fatalf("Discover %d components below the root: %v", depth, err)
	}
	defer opened.Close()
	if opened.Path != root || !opened.HasGit {
		t.Fatalf("Discover(%d deep) = %q hasGit=%v, want the repository root %q with its Git directory",
			depth, opened.Path, opened.HasGit, root)
	}
}

// TestWideDirectoryIsStreamedNotBuffered protects the memory invariant of a
// flat repository: one directory's width must not be a heap structure. A level
// wider than the sort buffer spills to a sorted run, so the live entry set
// stays inside the buffer plus the merge's fan-in however many children the
// directory holds -- and the order it yields is byte-identical to the order the
// in-memory sort produces, which is what the capture's digest is pinned to.
func TestWideDirectoryIsStreamedNotBuffered(t *testing.T) {
	root, _ := safeTree(t)
	const width = 500
	for i := range width {
		// Names in a deliberately unsorted arrival order: the listing's order
		// is the filesystem's, and the walk's order must be the key's.
		mustWrite(t, filepath.Join(root.Path, "wide", "f"+strconv.Itoa((i*7919)%width)+".go"), "package p\n")
	}
	policy := testPolicy(root)
	policy.MaxFiles = 0

	walkWith := func(sortBuf int) ([]string, int) {
		t.Helper()
		var got []string
		w := &walker{root: root, policy: policy, sortBuf: sortBuf,
			dataDirRel: dataDirRelative(root.Path, policy.DataDir),
			visit:      func(f File) error { got = append(got, f.Path); return nil }}
		if err := w.walkDir(t.Context(), ".", 0, false); err != nil {
			t.Fatalf("walk with sort buffer %d: %v", sortBuf, err)
		}
		return got, w.peakLive
	}

	// The whole tree in one buffer: the reference order and the reference the
	// spilling walk must reproduce exactly.
	whole, wholePeak := walkWith(width * 4)
	if len(whole) < width {
		t.Fatalf("the reference walk emitted %d files, want at least the %d wide children", len(whole), width)
	}

	const buf = 32
	spilled, peak := walkWith(buf)
	if !slices.Equal(whole, spilled) {
		t.Fatalf("a spilled level yielded a different order than the buffered one:\n spilled %v\n whole   %v", spilled, whole)
	}
	// The envelope: the buffer, the merge's live records, and the handful of
	// entries the small directories on the path to "wide" retain.
	envelope := buf + pagination.MaxSortFanIn + 64
	if peak > envelope {
		t.Fatalf("a %d-child directory held %d entries live at once (buffer %d, envelope %d); "+
			"one directory's width is a repository-sized heap structure again", width, peak, buf, envelope)
	}
	if wholePeak <= envelope {
		t.Fatalf("the reference walk held %d entries live with a buffer of %d, want more than the envelope %d: "+
			"the assertion above would pass even if nothing streamed", wholePeak, width*4, envelope)
	}
}
