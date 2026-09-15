package pagination_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Sawmonabo/codectx/internal/model"
	"github.com/Sawmonabo/codectx/internal/pagination"
)

// TestCursorTokensRejectForgeryExpiryAndPurpose protects the one signing
// primitive shared by search/graph/context cursors and source receipts.
//
// Failure mode: a token that verifies after a payload flip lets a client repin
// a query onto another generation or claim a receipt for bytes it never got; an
// expired token that verifies resurrects a released lease; a cursor accepted as
// a receipt (or the reverse) grants coverage credit from a search page.
func TestCursorTokensRejectForgeryExpiryAndPurpose(t *testing.T) {
	dir := t.TempDir()
	signer, err := pagination.OpenSigner(dir)
	if err != nil {
		t.Fatalf("OpenSigner: %v", err)
	}
	now := time.Date(2026, 9, 11, 12, 0, 0, 0, time.UTC)
	cursor := pagination.Cursor{
		Endpoint:     "search",
		GenerationID: 7,
		AnalysisKey:  model.AnalysisKey(model.H("analysis", "a")),
		QueryHash:    model.H("query", "q"),
		LastKey:      "0000:abc",
		LeaseID:      model.H("lease", "l"),
		ExpiresAt:    now.Add(15 * time.Minute),
	}
	token, err := signer.EncodeCursor(cursor)
	if err != nil {
		t.Fatalf("EncodeCursor: %v", err)
	}
	if len(token) > model.MaxTokenBytes {
		t.Fatalf("token is %d bytes, exceeds MaxTokenBytes", len(token))
	}
	got, err := signer.DecodeCursor(token, "search", now)
	if err != nil {
		t.Fatalf("DecodeCursor: %v", err)
	}
	if got != cursor {
		t.Fatalf("round trip = %+v, want %+v", got, cursor)
	}

	// A second signer over the same directory must load the same key, or a
	// long-running MCP process and a CLI invocation would reject each other's
	// cursors.
	again, err := pagination.OpenSigner(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := again.DecodeCursor(token, "search", now); err != nil {
		t.Fatalf("second signer over the same key dir rejected the token: %v", err)
	}

	tampered := []byte(token)
	tampered[len(tampered)/2] ^= 0x01
	rejects := map[string]func() error{
		"tampered byte": func() error { _, err := signer.DecodeCursor(string(tampered), "search", now); return err },
		"expired":       func() error { _, err := signer.DecodeCursor(token, "search", now.Add(16*time.Minute)); return err },
		"wrong endpoint": func() error {
			_, err := signer.DecodeCursor(token, "graph", now)
			return err
		},
		"cursor presented as receipt": func() error {
			_, err := signer.Verify(token, pagination.PurposeReceipt, now)
			return err
		},
		"oversized": func() error {
			_, err := signer.Verify(strings.Repeat("A", model.MaxTokenBytes+1), pagination.PurposeCursor, now)
			return err
		},
	}
	for name, check := range rejects {
		err := check()
		var typed *model.Error
		if !errors.As(err, &typed) || typed.Code != model.CodeCursorInvalid {
			t.Errorf("%s: got %v, want %s", name, err, model.CodeCursorInvalid)
		}
	}
	if _, err := signer.Sign(pagination.PurposeReceipt, []byte("receipt"), now.Add(time.Minute)); err != nil {
		t.Fatalf("Sign(receipt): %v", err)
	}
}

// leaseTable is the in-memory LeaseStore the spool test drives; storage owns
// the real rows and is exercised in its own package.
type leaseTable struct {
	mu     sync.Mutex
	expiry map[string]time.Time
}

func (l *leaseTable) AcquireLease(_ context.Context, lease model.Lease, _ string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.expiry[lease.ID] = lease.ExpiresAt
	return nil
}

func (l *leaseTable) RenewLease(_ context.Context, id string, expiresAt time.Time) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if _, ok := l.expiry[id]; !ok {
		return &model.Error{Code: model.CodeCursorInvalid, Message: "lease has expired or was released"}
	}
	l.expiry[id] = expiresAt
	return nil
}

func (l *leaseTable) ReleaseLease(_ context.Context, id string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.expiry, id)
	return nil
}

func (l *leaseTable) LeaseExpiry(_ context.Context, id string) (time.Time, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	t, ok := l.expiry[id]
	if !ok {
		return time.Time{}, &model.Error{Code: model.CodeCursorInvalid, Message: "lease has expired or was released"}
	}
	return t, nil
}

// TestSpoolsFollowTheirLease protects disk-backed continuation state.
//
// Failure mode: a spool whose header is still buffered looks like garbage to a
// concurrent sweep and is deleted under a live query; a spool that outlives a
// renewed lease is rejected by its own original expiry, or one whose lease was
// released is still served; two queries admitted against one byte budget
// overrun it together.
func TestSpoolsFollowTheirLease(t *testing.T) {
	ctx := context.Background()
	leases := &leaseTable{expiry: map[string]time.Time{}}
	now := time.Date(2026, 9, 11, 12, 0, 0, 0, time.UTC)
	spools, err := pagination.NewSpools(t.TempDir(), 7000, leases)
	if err != nil {
		t.Fatalf("NewSpools: %v", err)
	}
	lease := model.Lease{ID: model.H("lease", "s"), GenerationID: 7, OwnerKind: model.LeaseQuery, ExpiresAt: now.Add(time.Minute)}
	if err := leases.AcquireLease(ctx, lease, ""); err != nil {
		t.Fatal(err)
	}
	cursor := pagination.Cursor{Endpoint: "graph", GenerationID: 7, AnalysisKey: model.AnalysisKey(model.H("analysis", "a")),
		QueryHash: model.H("query", "q"), LeaseID: lease.ID, ExpiresAt: lease.ExpiresAt}
	sp, err := spools.Create(cursor)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	cursor.SpoolID = sp.ID()
	// The header is durable before Create returns, so a sweep that runs while
	// the query is still appending sees a live spool, not an empty file.
	if live, err := spools.Sweep(ctx, now); err != nil || live == 0 {
		t.Fatalf("Sweep during an open spool = %d live bytes %v; want the header counted, not the spool deleted", live, err)
	}
	headerOnly, _ := spools.Sweep(ctx, now)
	if err := sp.Append([]byte("frontier-1")); err != nil {
		t.Fatalf("Append: %v", err)
	}
	// The appended frame is still buffered; a sweep must reconcile the budget
	// to the bytes reserved, not to what has reached disk, or the open spool's
	// frames would be given away to another query.
	if live, err := spools.Sweep(ctx, now); err != nil || live != headerOnly+4+int64(len("frontier-1")) {
		t.Fatalf("Sweep with a buffered frame = %d live bytes %v, want %d (header + reserved frame)", live, err, headerOnly+4+int64(len("frontier-1")))
	}
	if err := sp.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	// The lease was renewed past the header's original expiry; the spool
	// follows the lease, not the stale header.
	if err := leases.RenewLease(ctx, lease.ID, now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	var records []string
	if err := spools.Open(ctx, cursor, now.Add(30*time.Minute), func(rec []byte) error { records = append(records, string(rec)); return nil }); err != nil {
		t.Fatalf("Open after renewal: %v", err)
	}
	if len(records) != 1 || records[0] != "frontier-1" {
		t.Fatalf("records = %q, want the appended frontier", records)
	}
	// The byte budget is shared: two spools admitted concurrently cannot both
	// write up to the whole cap.
	second, err := spools.Create(cursor)
	if err != nil {
		t.Fatalf("Create(second): %v", err)
	}
	big := make([]byte, 3500)
	if err := second.Append(big); err != nil {
		t.Fatalf("first large append within budget: %v", err)
	}
	third, err := spools.Create(cursor)
	if err != nil {
		t.Fatalf("Create(third): %v", err)
	}
	if err := third.Append(big); err == nil {
		t.Fatal("two spools together exceeded the shared byte budget")
	} else if typed, ok := err.(*model.Error); !ok || typed.Code != model.CodeResourceLimit {
		t.Fatalf("over-budget append = %v, want %s", err, model.CodeResourceLimit)
	}
	second.Close()
	third.Close()
	// Releasing the lease ends the continuation even though the header's
	// expiry has not passed.
	if err := leases.ReleaseLease(ctx, lease.ID); err != nil {
		t.Fatal(err)
	}
	if err := spools.Open(ctx, cursor, now, func([]byte) error { return nil }); err == nil {
		t.Fatal("Open served a spool whose lease was released")
	} else if typed, ok := err.(*model.Error); !ok || typed.Code != model.CodeCursorInvalid {
		t.Fatalf("Open after release = %v, want %s", err, model.CodeCursorInvalid)
	}
	if live, err := spools.Sweep(ctx, now); err != nil || live != 0 {
		t.Fatalf("Sweep after release = %d live bytes %v, want every spool of the released lease removed", live, err)
	}
}
