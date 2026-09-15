package pagination

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Sawmonabo/codectx/internal/model"
)

// noLeases is a lease store for a case that never opens a spool: AdoptDir and
// ReadoptDir check the budget and the binding, never the lease.
type noLeases struct{}

func (noLeases) AcquireLease(context.Context, model.Lease, string) error { return nil }
func (noLeases) RenewLease(context.Context, string, time.Time) error     { return nil }
func (noLeases) ReleaseLease(context.Context, string) error              { return nil }
func (noLeases) LeaseExpiry(context.Context, string) (time.Time, error)  { return time.Time{}, nil }

// TestReadoptingAGrownDirectoryChargesItOnce is the incremental-reservation
// invariant. A paged walk extends ONE retained state directory page after page,
// so the directory is re-adopted at every page boundary. AdoptDir measures the
// whole directory and reserves it again while the previous reservation is
// released only afterwards, which made the shared byte budget hold roughly TWO
// copies of the cumulative state at every boundary -- and a walk whose state is
// append-only would still stop being able to mint a continuation at half the
// budget it actually needs.
//
// ReadoptDir transfers the previous reservation inside one critical section and
// claims only the delta, so what the store has charged after the re-adoption is
// the directory's real size and not about twice it.
//
// Mutation (ReadoptDir re-reserving without releasing: `s.transfer(prevID, id,
// size)` replaced by `s.reserve(id, size)`): used comes back at ~2x dirBytes
// and the assertion below fails.
func TestReadoptingAGrownDirectoryChargesItOnce(t *testing.T) {
	store, err := NewSpools(t.TempDir(), 0, noLeases{})
	if err != nil {
		t.Fatalf("NewSpools: %v", err)
	}
	c := Cursor{Endpoint: "graph", GenerationID: 3,
		AnalysisKey: model.AnalysisKey(model.H("analysis", "r")),
		QueryHash:   model.H("query", "r"), LeaseID: model.H("lease", "r"),
		ExpiresAt: time.Now().Add(time.Minute)}

	dir := filepath.Join(t.TempDir(), "retained")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	// A first page's worth of state, then the adoption that page's cursor makes.
	if err := os.WriteFile(filepath.Join(dir, "visited"), make([]byte, 4096), 0o600); err != nil {
		t.Fatal(err)
	}
	id, err := store.AdoptDir(c, dir)
	if err != nil {
		t.Fatalf("AdoptDir: %v", err)
	}
	held := filepath.Join(store.dir, spoolPrefix+id)
	first, err := dirBytes(held)
	if err != nil {
		t.Fatal(err)
	}
	if store.used != first {
		t.Fatalf("after the first adoption the store holds %d bytes for a %d-byte directory", store.used, first)
	}

	// The next page appends to the SAME directory and re-adopts it.
	if err := os.WriteFile(filepath.Join(held, "visited"), make([]byte, 3*4096), 0o600); err != nil {
		t.Fatal(err)
	}
	grown, err := dirBytes(held)
	if err != nil {
		t.Fatal(err)
	}
	if grown <= first {
		t.Fatalf("the directory did not grow (%d bytes, was %d): the case would prove nothing", grown, first)
	}
	next, err := store.ReadoptDir(c, id)
	if err != nil {
		t.Fatalf("ReadoptDir: %v", err)
	}
	after, err := dirBytes(filepath.Join(store.dir, spoolPrefix+next))
	if err != nil {
		t.Fatal(err)
	}
	if store.used != after {
		t.Fatalf("after re-adopting a %d-byte directory the store holds %d bytes; "+
			"a second reservation of the whole directory would read as %d",
			after, store.used, after+grown)
	}
	if n := len(store.reserved); n != 1 {
		t.Fatalf("the store holds %d reservations for one retained directory, want 1", n)
	}
}
