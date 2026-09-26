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
	// AcquireLease records the lease. ownerRef names the owner it is taken
	// for, so the lease can be released when that owner ends; an owner with no
	// stable identity of its own passes "". At most one live lease exists per
	// owner, and the store refuses a second one rather than pinning twice.
	AcquireLease(ctx context.Context, lease model.Lease, ownerRef string) error
	RenewLease(ctx context.Context, id string, expiresAt time.Time) error
	ReleaseLease(ctx context.Context, id string) error
	// LeaseExpiry reports when a live lease expires; a released or unknown
	// lease is CTX_CURSOR_INVALID. Spools consult it instead of the expiry
	// their header was created with, which a renewal makes stale.
	LeaseExpiry(ctx context.Context, id string) (time.Time, error)
}

// LeaseRetainer is the optional half of LeaseStore: a store that knows it
// cannot record a lease at all reports so here. A process that opened its
// database read-only is the case that matters -- every write it attempts
// fails -- and an endpoint that asks BEFORE it mints serves its page and says
// there is no continuation, instead of failing the page on the lease write.
//
// It is optional because the contract is the store's own knowledge, not a
// requirement on every implementation: a store that stays silent is taken to
// retain, which is what it did before it was asked.
type LeaseRetainer interface {
	RetainsLeases() bool
}

// Leases acquires, renews and releases retention leases with one configured
// TTL, so every cursor and query in the process retains its generation for the
// same window.
type Leases struct {
	store   LeaseStore
	ttl     time.Duration
	now     func() time.Time
	retains bool
}

// NewLeases wraps store with ttl; a non-positive ttl selects DefaultCursorTTL.
func NewLeases(store LeaseStore, ttl time.Duration) *Leases {
	if ttl <= 0 {
		ttl = DefaultCursorTTL
	}
	retains := true
	if r, ok := store.(LeaseRetainer); ok {
		retains = r.RetainsLeases()
	}
	return &Leases{store: store, ttl: ttl, now: time.Now, retains: retains}
}

// Retains reports whether a lease taken here would actually be recorded. A nil
// *Leases -- an endpoint composed with no lease store at all -- retains
// nothing, so the one test answers both "there is no lease store" and "the
// lease store cannot write", which are the same fact to a caller deciding
// whether it may hand back a continuation.
func (l *Leases) Retains() bool { return l != nil && l.retains }

// Acquire retains gen (and optionally its snapshot) for one TTL on behalf of
// kind and returns the lease, whose ID a cursor carries. The lease is owned by
// nothing but the token that carries its id: it ends when that token's holder
// releases it, or with its TTL.
func (l *Leases) Acquire(ctx context.Context, gen model.GenerationID, snapshot model.SnapshotID, kind model.LeaseOwnerKind) (model.Lease, error) {
	return l.AcquireFor(ctx, gen, snapshot, kind, "")
}

// AcquireFor is Acquire for an owner that outlives the call and can be found
// again -- a read session, whose lease must end when the session closes rather
// than on the TTL. A second acquire for one owner is refused by the store, so
// an idempotent re-open leaves the one lease the owner already holds instead
// of pinning its generation a second time.
func (l *Leases) AcquireFor(ctx context.Context, gen model.GenerationID, snapshot model.SnapshotID,
	kind model.LeaseOwnerKind, ownerRef string) (model.Lease, error) {
	id, err := model.NewRandomID()
	if err != nil {
		return model.Lease{}, err
	}
	lease := model.Lease{ID: id, GenerationID: gen, SnapshotID: snapshot, OwnerKind: kind, ExpiresAt: l.now().Add(l.ttl).UTC()}
	if err := lease.Validate(); err != nil {
		return model.Lease{}, err
	}
	if err := l.store.AcquireLease(ctx, lease, ownerRef); err != nil {
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
