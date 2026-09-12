package pagination

import (
	"context"
	"time"

	"github.com/Sawmonabo/codectx/internal/model"
)

// DefaultCursorTTL is the Section 20.1 storage.query_cursor_ttl default.
const DefaultCursorTTL = 15 * time.Minute

// LeaseStore is the narrow storage contract this package needs. The store owns
// the retention_leases rows and the transactions around them; this package
// supplies only TTL policy and identifiers.
type LeaseStore interface {
	AcquireLease(ctx context.Context, lease model.Lease) error
	RenewLease(ctx context.Context, id string, expiresAt time.Time) error
	ReleaseLease(ctx context.Context, id string) error
}

// Leases acquires, renews and releases retention leases with one configured
// TTL, so every cursor and query in the process retains its generation for the
// same window.
type Leases struct {
	store LeaseStore
	ttl   time.Duration
	now   func() time.Time
}

// NewLeases wraps store with ttl; a non-positive ttl selects DefaultCursorTTL.
func NewLeases(store LeaseStore, ttl time.Duration) *Leases {
	if ttl <= 0 {
		ttl = DefaultCursorTTL
	}
	return &Leases{store: store, ttl: ttl, now: time.Now}
}

// Acquire retains gen (and optionally its snapshot) for one TTL on behalf of
// kind and returns the lease, whose ID a cursor carries.
func (l *Leases) Acquire(ctx context.Context, gen model.GenerationID, snapshot model.SnapshotID, kind model.LeaseOwnerKind) (model.Lease, error) {
	id, err := model.NewRandomID()
	if err != nil {
		return model.Lease{}, err
	}
	lease := model.Lease{ID: id, GenerationID: gen, SnapshotID: snapshot, OwnerKind: kind, ExpiresAt: l.now().Add(l.ttl).UTC()}
	if err := lease.Validate(); err != nil {
		return model.Lease{}, err
	}
	if err := l.store.AcquireLease(ctx, lease); err != nil {
		return model.Lease{}, err
	}
	return lease, nil
}

// Renew extends a lease by one TTL from now and returns the new expiry. A
// lease that has already been released or expired is CTX_CURSOR_INVALID: the
// continuation never silently switches to the current active generation.
func (l *Leases) Renew(ctx context.Context, id string) (time.Time, error) {
	expires := l.now().Add(l.ttl).UTC()
	if err := l.store.RenewLease(ctx, id, expires); err != nil {
		return time.Time{}, err
	}
	return expires, nil
}

// Release drops a lease once its owner is done.
func (l *Leases) Release(ctx context.Context, id string) error {
	return l.store.ReleaseLease(ctx, id)
}
