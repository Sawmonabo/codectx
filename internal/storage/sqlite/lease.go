package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/Sawmonabo/codectx/internal/model"
	"modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"
)

// ownerLeaseConflict is the refusal the owner-unique index produces when a
// second lease is acquired for one owner. It is typed here rather than left as
// the driver's constraint text so a caller whose acquire is idempotent -- an
// interrupted session re-open, which must end with the one lease it already
// has -- can recognise it without matching on SQL.
func ownerLeaseConflict(kind model.LeaseOwnerKind) *model.Error {
	return (&model.Error{Code: model.CodeVersionConflict,
		Message: "this owner already holds a retention lease"}).
		WithDetail("conflict", "owner_lease").
		WithDetail("owner_kind", string(kind))
}

// OwnerLeaseConflict reports whether err is that refusal, so an idempotent
// acquire can treat the lease the owner already holds as its own answer.
func OwnerLeaseConflict(err error) bool {
	var typed *model.Error
	return errors.As(err, &typed) && typed.Details["conflict"] == "owner_lease"
}

// insertLease is the ONE write path into retention_leases. Every lease --
// a cursor's, a session's, a staging build's and the query lease a pinned
// reader takes -- is validated and written here, so model.Lease.Validate
// cannot be bypassed by a second statement elsewhere and the owner-unique
// index applies to all of them.
//
// It takes the caller's transaction rather than opening one: PinGeneration
// must resolve the generation and take its lease atomically, and a nested
// write would deadlock against the single writer connection.
func insertLease(ctx context.Context, tx *sql.Tx, lease model.Lease, ownerRef any) error {
	if err := lease.Validate(); err != nil {
		return err
	}
	idRaw, err := idBlob("lease.id", lease.ID)
	if err != nil {
		return err
	}
	snapRaw, err := optionalBlob("lease.snapshot_id", string(lease.SnapshotID))
	if err != nil {
		return err
	}
	var genArg any
	if lease.GenerationID != 0 {
		genArg = int64(lease.GenerationID)
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO retention_leases(id, generation_id, snapshot_id, owner_kind, owner_ref, expires_at)
		VALUES(?, ?, ?, ?, ?, ?)`,
		idRaw, genArg, snapRaw, string(lease.OwnerKind), ownerRef, formatTime(lease.ExpiresAt))
	if ownerRef != nil && isUniqueViolation(err) {
		return ownerLeaseConflict(lease.OwnerKind)
	}
	return wrap("retention_leases", err)
}

// isUniqueViolation reports whether err is a uniqueness rejection rather than
// any other constraint. wrap() collapses every SQLITE_CONSTRAINT to one code,
// which cannot tell "this owner is already leased" from a malformed row.
func isUniqueViolation(err error) bool {
	var se *sqlite.Error
	return errors.As(err, &se) &&
		(se.Code() == sqlite3.SQLITE_CONSTRAINT_UNIQUE || se.Code() == sqlite3.SQLITE_CONSTRAINT_PRIMARYKEY)
}

// AcquireLease records a retention lease. The generation and snapshot it names
// must exist; leases on staging or failed generations are permitted so a
// build's own state stays retained.
//
// ownerRef names the owner the lease is taken for -- a session id, say -- and
// makes the lease findable from its owner, so the transaction that ends the
// owner ends the lease with it (releaseOwnerLease, and AdvanceSession's close).
// At most one live lease exists per (owner kind, owner ref):
// a second acquire is refused with OwnerLeaseConflict rather than pinning the
// generation twice. An owner with no stable identity of its own passes "".
func (s *Store) AcquireLease(ctx context.Context, lease model.Lease, ownerRef string) error {
	owner, err := optionalBlob("lease.owner_ref", ownerRef)
	if err != nil {
		return err
	}
	return s.write(ctx, func(tx *sql.Tx) error {
		if lease.GenerationID != 0 {
			if _, err := s.generationRow(ctx, tx, lease.GenerationID, ""); err != nil {
				return err
			}
		}
		return insertLease(ctx, tx, lease, owner)
	})
}

// RenewLease extends a live lease. An expired or released lease is
// CTX_CURSOR_INVALID: continuation never silently repins.
func (s *Store) RenewLease(ctx context.Context, id string, expiresAt time.Time) error {
	raw, err := idBlob("lease.id", id)
	if err != nil {
		return err
	}
	return s.write(ctx, func(tx *sql.Tx) error {
		return exec1(ctx, tx, &model.Error{Code: model.CodeCursorInvalid, Message: "lease has expired or was released"},
			`UPDATE retention_leases SET expires_at = ? WHERE id = ? AND expires_at > ?`, formatTime(expiresAt), raw, formatTime(time.Now()))
	})
}

// ReleaseLease drops a lease; a lease already gone is not an error.
func (s *Store) ReleaseLease(ctx context.Context, id string) error {
	raw, err := idBlob("lease.id", id)
	if err != nil {
		return err
	}
	return s.write(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `DELETE FROM retention_leases WHERE id = ?`, raw)
		return wrap("retention_leases", err)
	})
}

// releaseOwnerLease ends the lease an owner holds, inside the transaction that
// ended the owner -- closing a session -- so the lease goes atomically with the
// change that ended it rather than leaving a window in which the owner is gone
// and its lease is not, or waiting out the TTL. An owner holding no lease is
// not an error: this is the end of a lifecycle, not an assertion about one.
func releaseOwnerLease(ctx context.Context, tx *sql.Tx, kind model.LeaseOwnerKind, ownerRef []byte) error {
	_, err := tx.ExecContext(ctx, `DELETE FROM retention_leases WHERE owner_kind = ? AND owner_ref = ?`,
		string(kind), ownerRef)
	return wrap("retention_leases", err)
}

// LeaseExpiry reports when a live lease expires. A lease that was released or
// never existed is CTX_CURSOR_INVALID, so spools and cursors that consult it
// end with their lease rather than with the expiry they were created under.
func (s *Store) LeaseExpiry(ctx context.Context, id string) (time.Time, error) {
	raw, err := idBlob("lease.id", id)
	if err != nil {
		return time.Time{}, err
	}
	var expires string
	err = s.read(ctx, func(tx *sql.Tx) error {
		err := tx.QueryRowContext(ctx, `SELECT expires_at FROM retention_leases WHERE id = ?`, raw).Scan(&expires)
		if isNoRows(err) {
			return &model.Error{Code: model.CodeCursorInvalid, Message: "lease has expired or was released"}
		}
		return wrap("retention_leases", err)
	})
	if err != nil {
		return time.Time{}, err
	}
	return parseTime(expires)
}
