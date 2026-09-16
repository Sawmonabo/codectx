package scratch

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/Sawmonabo/codectx/internal/fslock"
)

// newArena is an arena over root that is NOT the process-wide one, so a test
// can hold two at once and watch them claim different instances.
func newArena(root string) *Arena {
	return &Arena{
		root:  filepath.Join(root, dirName),
		free:  map[Purpose][]string{},
		held:  map[string]bool{},
		next:  map[Purpose]int{},
		swept: map[Purpose]bool{},
	}
}

// TestAReleasedSurfaceIsTakenAgainAndNeverShrinks protects the arena's whole
// reason to exist: a run must not free the space it wrote. If Release removed
// the file, or if Take truncated the one it hands out, every surface would be
// freed and recreated per use and a run would hand the host gigabytes of
// deallocation mid-work — the stall this design removes.
//
// It also pins the consequence tenants must respect: because nothing
// truncates, a second tenant that wrote less than the first still finds the
// first tenant's bytes past its own. A tenant that took its length from the
// file instead of from its own header would serve those stale bytes as data.
func TestAReleasedSurfaceIsTakenAgainAndNeverShrinks(t *testing.T) {
	a := newArena(t.TempDir())

	first, f, err := a.TakeFile(SortRun)
	if err != nil {
		t.Fatalf("take: %v", err)
	}
	big := make([]byte, 64<<10)
	for i := range big {
		big[i] = 'a'
	}
	if _, err := f.Write(big); err != nil {
		t.Fatalf("write: %v", err)
	}
	path := first.Path()
	first.Release()

	if _, err := os.Stat(path); err != nil {
		t.Fatalf("the released surface is gone from the disk: %v", err)
	}

	second, f2, err := a.TakeFile(SortRun)
	if err != nil {
		t.Fatalf("second take: %v", err)
	}
	if second.Path() != path {
		t.Fatalf("the second take got %s, want the pooled %s: the arena is not reusing its surfaces", second.Path(), path)
	}
	if _, err := f2.Write([]byte("bb")); err != nil {
		t.Fatalf("second write: %v", err)
	}
	st, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if st.Size() != int64(len(big)) {
		t.Fatalf("the surface is %d bytes after the second tenant wrote 2, want the high-water %d: it was truncated", st.Size(), len(big))
	}
	got := make([]byte, 4)
	if _, err := f2.ReadAt(got, 0); err != nil {
		t.Fatalf("read back: %v", err)
	}
	if string(got) != "bbaa" {
		t.Fatalf("read %q at offset zero, want %q: the second tenant's bytes must overwrite in place with the first tenant's left past them", got, "bbaa")
	}
	second.Release()

	// A second purpose is a separate pool: a sort run is never handed out as
	// a staging database.
	other, err := a.Take(LexicalStage)
	if err != nil {
		t.Fatalf("take other purpose: %v", err)
	}
	if other.Path() == path {
		t.Fatalf("purpose %s was handed the %s surface %s", LexicalStage, SortRun, path)
	}
	other.Release()
}

// TestTakeNeverHandsOneSurfaceToTwoTenants protects exclusivity: two callers
// writing one scratch file would interleave their bytes and each would read
// the other's as its own. It also pins Empty's refusal to pull a surface out
// from under a live tenant, and Discard leaving the slot behind rather than
// pooling a path whose file the tenant moved away.
func TestTakeNeverHandsOneSurfaceToTwoTenants(t *testing.T) {
	a := newArena(t.TempDir())
	first, err := a.Take(SortRun)
	if err != nil {
		t.Fatalf("take: %v", err)
	}
	second, err := a.Take(SortRun)
	if err != nil {
		t.Fatalf("take: %v", err)
	}
	if first.Path() == second.Path() {
		t.Fatalf("both tenants hold %s", first.Path())
	}
	if _, err := a.Empty(); err == nil {
		t.Fatal("Empty removed surfaces that two tenants were holding; it must refuse while a lease is out")
	}

	// A discarded surface has been moved away by its tenant, so it must not
	// come back from the pool: the next take gets a different slot.
	moved := first.Path() + "-moved"
	if err := os.Rename(first.Path(), moved); err != nil {
		t.Fatalf("rename: %v", err)
	}
	first.Discard()
	third, err := a.Take(SortRun)
	if err != nil {
		t.Fatalf("take after discard: %v", err)
	}
	if third.Path() == first.Path() {
		t.Fatalf("the arena handed out %s, whose file the tenant moved to %s", third.Path(), moved)
	}
	second.Release()
	third.Release()

	freed, err := a.Empty()
	if err != nil {
		t.Fatalf("empty: %v", err)
	}
	if freed < 0 {
		t.Fatalf("Empty reported %d bytes freed", freed)
	}
	if _, err := os.Stat(filepath.Join(a.instance, string(SortRun))); !os.IsNotExist(err) {
		t.Fatalf("the emptied arena still holds %s: %v", SortRun, err)
	}
}

// TestTwoLiveProcessesNeverSharePooledFiles protects the pool against the one
// hazard a per-process pool has: SortRun is taken on the query path, where
// several processes serve pages from one data directory at once. If both
// claimed the same instance they would each take slot zero and each read the
// other's bytes. The second half protects the other side of the claim — a
// process that exits must have its files INHERITED, or an abandoned pool
// accumulates on the disk with nothing to sweep it.
func TestTwoLiveProcessesNeverSharePooledFiles(t *testing.T) {
	root := t.TempDir()
	first := newArena(root)
	a, err := first.Take(SortRun)
	if err != nil {
		t.Fatalf("take: %v", err)
	}

	second := newArena(root)
	b, err := second.Take(SortRun)
	if err != nil {
		t.Fatalf("take in the second process: %v", err)
	}
	if a.Path() == b.Path() {
		t.Fatalf("two live processes were handed the same surface %s", a.Path())
	}
	if first.instance == second.instance {
		t.Fatalf("two live processes claimed one instance %s", first.instance)
	}
	path := a.Path()
	a.Release()
	b.Release()

	// The first process exits: its claim goes, its files stay.
	if err := fslock.Unlock(first.lock); err != nil {
		t.Fatalf("unlock: %v", err)
	}
	first.lock.Close()

	third := newArena(root)
	c, err := third.Take(SortRun)
	if err != nil {
		t.Fatalf("take after the first process exited: %v", err)
	}
	if c.Path() != path {
		t.Fatalf("a new process took %s and left the exited process's %s behind: an abandoned pool accumulates", c.Path(), path)
	}
	c.Release()
}
