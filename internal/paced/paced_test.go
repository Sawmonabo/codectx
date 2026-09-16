package paced

import (
	"os"
	"path/filepath"
	"testing"
)

func writeFile(t *testing.T, path string, size int64) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(size); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
}

// The requirement: a file larger than the window is freed one window at a
// time, each step waited for, so that no single journal commit frees more
// than a window of extents. Mutation: make shrinkFile truncate straight to
// size (drop the loop) and the step count is zero for every file.
func TestRemoveFreesALargeFileOneWindowAtATime(t *testing.T) {
	dir := t.TempDir()
	large := filepath.Join(dir, "large")
	writeFile(t, large, 3*Window+Window/2)
	before := Steps()
	if err := Remove(large); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(large); !os.IsNotExist(err) {
		t.Fatalf("the file survived its removal: %v", err)
	}
	// 3.5 windows: 3 whole windows and the half that remains are freed in
	// four steps, the last being the truncation to zero.
	if got := Steps() - before; got != 4 {
		t.Fatalf("freed %d windows one at a time; want 4", got)
	}

	small := filepath.Join(dir, "small")
	writeFile(t, small, Window)
	before = Steps()
	if err := Remove(small); err != nil {
		t.Fatal(err)
	}
	if got := Steps() - before; got != 0 {
		t.Fatalf("a file no larger than the window took %d steps; it is unlinked whole", got)
	}
}

func TestRemoveAllShrinksEveryLargeFileBeforeUnlinkingTheTree(t *testing.T) {
	dir := t.TempDir()
	tree := filepath.Join(dir, "tree")
	if err := os.MkdirAll(filepath.Join(tree, "nested"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(tree, "a"), 2*Window+1)
	writeFile(t, filepath.Join(tree, "nested", "b"), Window+1)
	writeFile(t, filepath.Join(tree, "nested", "c"), 16)
	if err := os.Symlink(filepath.Join(tree, "a"), filepath.Join(tree, "link")); err != nil {
		t.Fatal(err)
	}
	before := Steps()
	if err := RemoveAll(tree); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(tree); !os.IsNotExist(err) {
		t.Fatalf("the tree survived its removal: %v", err)
	}
	// a: 2 windows and a byte, three steps; b: a window and a byte, two.
	if got := Steps() - before; got != 5 {
		t.Fatalf("freed %d windows one at a time; want 5", got)
	}
	if err := RemoveAll(filepath.Join(dir, "absent")); err != nil {
		t.Fatalf("removing an absent tree is not a failure: %v", err)
	}
}

func TestShrinkStopsAtTheRequestedSize(t *testing.T) {
	path := filepath.Join(t.TempDir(), "f")
	writeFile(t, path, 4*Window)
	before := Steps()
	if err := Shrink(path, Window+3); err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if st.Size() != Window+3 {
		t.Fatalf("size after Shrink is %d; want %d", st.Size(), Window+3)
	}
	// 4W -> 3W -> 2W -> W+3: three steps, the last one partial.
	if got := Steps() - before; got != 3 {
		t.Fatalf("freed %d windows; want 3", got)
	}
}
