package pagination

import (
	"context"
	"errors"
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

// TestARefusedReadoptionLeavesThePreviousCursorAbleToOpenIt is the ordering
// invariant of the same call.
//
// A re-adoption can be REFUSED: a directory a page grew past what is left of
// the shared byte budget answers CTX_RESOURCE_LIMIT, which is retryable -- the
// caller is told to present the cursor it already holds again. That promise
// only holds if a refused re-adoption changed nothing. Stamping the new id and
// lease into the header before reserving broke it: the directory kept its old
// NAME and its old reservation but carried the new BINDING, and OpenDir
// compares the two, so the cursor the caller was told to retry with answered
// "spool does not belong to this cursor" and the whole walk behind it was gone.
//
// Mutation (the stamp moved back ahead of the reservation, as it was): the
// OpenDir below fails with CTX_CURSOR_INVALID.
func TestARefusedReadoptionLeavesThePreviousCursorAbleToOpenIt(t *testing.T) {
	const budget = 16 << 10
	store, err := NewSpools(t.TempDir(), budget, liveLeases{})
	if err != nil {
		t.Fatalf("NewSpools: %v", err)
	}
	c1 := Cursor{Endpoint: "graph", GenerationID: 3,
		AnalysisKey: model.AnalysisKey(model.H("analysis", "r")),
		QueryHash:   model.H("query", "r"), LeaseID: model.H("lease-1", "r"),
		ExpiresAt: time.Now().Add(time.Minute)}
	c2 := c1
	c2.LeaseID = model.H("lease-2", "r")

	dir := filepath.Join(t.TempDir(), "retained")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "visited"), make([]byte, 4096), 0o600); err != nil {
		t.Fatal(err)
	}
	prevID, err := store.AdoptDir(c1, dir)
	if err != nil {
		t.Fatalf("AdoptDir: %v", err)
	}
	c1.SpoolID = prevID
	if _, err := store.OpenDir(context.Background(), c1, time.Now()); err != nil {
		t.Fatalf("the adopting cursor cannot open its own directory: %v", err)
	}

	// The page grows the retained state past what the budget can hold.
	held := filepath.Join(store.dir, spoolPrefix+prevID)
	if err := os.WriteFile(filepath.Join(held, "runs"), make([]byte, 4*budget), 0o600); err != nil {
		t.Fatal(err)
	}
	switch _, err := store.ReadoptDir(c2, prevID); {
	case err == nil:
		t.Fatal("the re-adoption of a directory four times the budget was allowed")
	case !isCode(err, model.CodeResourceLimit):
		t.Fatalf("the refusal is %v, want %s: only a retryable code tells the caller to present "+
			"the same cursor again", err, model.CodeResourceLimit)
	}

	// The retry the refusal asks for.
	if _, err := store.OpenDir(context.Background(), c1, time.Now()); err != nil {
		t.Fatalf("after a refused re-adoption the previous cursor can no longer open its "+
			"directory: %v", err)
	}
}

// liveLeases is noLeases with an expiry a case that OPENS a directory can pass.
type liveLeases struct{ noLeases }

func (liveLeases) LeaseExpiry(context.Context, string) (time.Time, error) {
	return time.Now().Add(time.Hour), nil
}

// isCode reports whether err is a typed model error carrying code.
func isCode(err error, code string) bool {
	var typed *model.Error
	return errors.As(err, &typed) && typed.Code == code
}
