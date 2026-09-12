package sqlite

import (
	"context"
	"database/sql"
	"time"

	"github.com/Sawmonabo/codectx/internal/model"
)

// AcquireLease records a retention lease. The generation and snapshot it names
// must exist; leases on staging or failed generations are permitted so a
// build's own state stays retained.
func (s *Store) AcquireLease(ctx context.Context, lease model.Lease) error {
	if err := lease.Validate(); err != nil {
		return err
	}
	idRaw, _ := model.DecodeID(lease.ID)
	snapRaw, err := optionalBlob("lease.snapshot_id", string(lease.SnapshotID))
	if err != nil {
		return err
	}
	var genArg any
	if lease.GenerationID != 0 {
		genArg = int64(lease.GenerationID)
	}
	return s.write(ctx, func(tx *sql.Tx) error {
		if lease.GenerationID != 0 {
			if _, err := s.generationRow(ctx, tx, lease.GenerationID, ""); err != nil {
				return err
			}
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO retention_leases(id, generation_id, snapshot_id, owner_kind, expires_at) VALUES(?, ?, ?, ?, ?)`,
			idRaw, genArg, snapRaw, string(lease.OwnerKind), formatTime(lease.ExpiresAt))
		return wrap("retention_leases", err)
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
