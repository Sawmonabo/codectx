package scratch

import (
	"os"
	"path/filepath"
	"testing"
)

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
	a := For(t.TempDir())

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
	// a spool segment.
	other, err := a.Take(SpoolSegment)
	if err != nil {
		t.Fatalf("take other purpose: %v", err)
	}
	if other.Path() == path {
		t.Fatalf("purpose %s was handed the %s surface %s", SpoolSegment, SortRun, path)
	}
	other.Release()
}

// TestTakeNeverHandsOneSurfaceToTwoTenants protects exclusivity: two callers
// writing one scratch file would interleave their bytes and each would read
// the other's as its own.
func TestTakeNeverHandsOneSurfaceToTwoTenants(t *testing.T) {
	a := For(t.TempDir())
	first, err := a.Take(SpoolSegment)
	if err != nil {
		t.Fatalf("take: %v", err)
	}
	second, err := a.Take(SpoolSegment)
	if err != nil {
		t.Fatalf("take: %v", err)
	}
	if first.Path() == second.Path() {
		t.Fatalf("both tenants hold %s", first.Path())
	}
	if err := a.Empty(); err == nil {
		t.Fatal("Empty removed surfaces that two tenants were holding; it must refuse while a lease is out")
	}
	first.Release()
	second.Release()

	if err := a.Empty(); err != nil {
		t.Fatalf("empty: %v", err)
	}
	if _, err := os.Stat(filepath.Join(a.Root(), string(SpoolSegment))); !os.IsNotExist(err) {
		t.Fatalf("the emptied arena still holds %s: %v", SpoolSegment, err)
	}
}

// TestAnArenaTakesBackTheSurfacesOfAnEarlierRun protects the pool across
// process restarts: a store that forgot last run's surfaces would grow a
// second pool beside the first and the arena would never stop growing.
func TestAnArenaTakesBackTheSurfacesOfAnEarlierRun(t *testing.T) {
	dir := t.TempDir()
	a := For(dir)
	l, err := a.Take(SortRun)
	if err != nil {
		t.Fatalf("take: %v", err)
	}
	path := l.Path()
	l.Release()

	// A fresh arena over the same root is the next run of the same store.
	fresh := &Arena{root: a.root, free: map[Purpose][]string{}, held: map[string]bool{}, next: map[Purpose]int{}, swept: map[Purpose]bool{}}
	again, err := fresh.Take(SortRun)
	if err != nil {
		t.Fatalf("take after restart: %v", err)
	}
	if again.Path() != path {
		t.Fatalf("the restarted store created %s beside the pooled %s", again.Path(), path)
	}
	again.Release()
}
