package sqlite

import (
	"context"
	"database/sql"
	"time"

	"github.com/Sawmonabo/codectx/internal/model"
)

// WatchHeartbeat is what a running watch publishes about itself so a second
// process can read it: which process owns the watch, when its last pass
// completed, how many notification events are still pending, and the deadline
// the writer promised to refresh before.
//
// ExpiresAt is the writer's own deadline and the only liveness signal. A reader
// compares it to the clock and never recomputes it from configuration: the two
// processes need not share a configuration, and a reader that derived the
// window would grant or deny liveness the writer never promised.
//
// LastPassAt and PendingEvents are pointers because absent and zero are
// different answers. A watch driven only by periodic reconciliation has no
// notification queue to count, and a watch that has not completed a pass yet has
// no pass time and publishes no pending count either; reporting either as zero
// would state that the watch is caught up. A row with no pass time is therefore
// a watch that is running and covering nothing -- waiting for whichever process
// holds the workspace -- which is a third answer beside a covering watch and no
// row at all.
type WatchHeartbeat struct {
	WriterPID     int
	LastPassAt    *time.Time
	PendingEvents *int64
	ExpiresAt     time.Time
}

// RecordWatchHeartbeat publishes hb as the repository's live watch heartbeat,
// replacing whatever the previous writer left. There is one row per repository
// and no history: this is liveness, and a watch that has ended leaves nothing
// worth keeping.
func (s *Store) RecordWatchHeartbeat(ctx context.Context, repo model.RepositoryID, hb WatchHeartbeat) error {
	raw, err := idBlob("repository_id", string(repo))
	if err != nil {
		return err
	}
	if hb.WriterPID <= 0 {
		return invalid("a watch heartbeat needs the writing process id")
	}
	if hb.ExpiresAt.IsZero() {
		return invalid("a watch heartbeat needs the writer's own expiry")
	}
	var lastPass any
	if hb.LastPassAt != nil {
		lastPass = formatTime(*hb.LastPassAt)
	}
	var pending any
	if hb.PendingEvents != nil {
		pending = *hb.PendingEvents
	}
	return s.write(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `INSERT INTO watch_heartbeat(repository_id, writer_pid, last_pass_at, pending_events, expires_at)
			VALUES(?, ?, ?, ?, ?) ON CONFLICT(repository_id) DO UPDATE SET
				writer_pid = excluded.writer_pid, last_pass_at = excluded.last_pass_at,
				pending_events = excluded.pending_events, expires_at = excluded.expires_at`,
			raw, hb.WriterPID, lastPass, pending, formatTime(hb.ExpiresAt))
		return wrap("watch_heartbeat", err)
	})
}

// ClearWatchHeartbeat removes the repository's heartbeat. A watch that ends
// deliberately withdraws its claim at once rather than leaving a fresh row that
// reads as live coverage until it expires; expiry remains the answer for a
// process that died without getting here.
func (s *Store) ClearWatchHeartbeat(ctx context.Context, repo model.RepositoryID) error {
	raw, err := idBlob("repository_id", string(repo))
	if err != nil {
		return err
	}
	return s.write(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `DELETE FROM watch_heartbeat WHERE repository_id = ?`, raw)
		return wrap("watch_heartbeat", err)
	})
}

// WatchHeartbeat reads the repository's heartbeat. The second result is false
// when no watch has ever published one, which is a different answer from an
// expired row: nothing has run here, rather than something ran and stopped.
//
// Whether a row that exists is live is the caller's judgement, made against
// ExpiresAt. This method reports what was written and interprets nothing.
func (s *Store) WatchHeartbeat(ctx context.Context, repo model.RepositoryID) (WatchHeartbeat, bool, error) {
	raw, err := idBlob("repository_id", string(repo))
	if err != nil {
		return WatchHeartbeat{}, false, err
	}
	var hb WatchHeartbeat
	found := false
	err = s.read(ctx, func(tx *sql.Tx) error {
		var pid int64
		var lastPass sql.NullString
		var pending sql.NullInt64
		var expires string
		scanErr := tx.QueryRowContext(ctx, `SELECT writer_pid, last_pass_at, pending_events, expires_at
			FROM watch_heartbeat WHERE repository_id = ?`, raw).Scan(&pid, &lastPass, &pending, &expires)
		if isNoRows(scanErr) {
			return nil
		}
		if scanErr != nil {
			return wrap("watch_heartbeat", scanErr)
		}
		expiresAt, parseErr := parseTime(expires)
		if parseErr != nil {
			return parseErr
		}
		hb = WatchHeartbeat{WriterPID: int(pid), ExpiresAt: expiresAt}
		if lastPass.Valid {
			at, parseErr := parseTime(lastPass.String)
			if parseErr != nil {
				return parseErr
			}
			hb.LastPassAt = &at
		}
		if pending.Valid {
			n := pending.Int64
			hb.PendingEvents = &n
		}
		found = true
		return nil
	})
	if err != nil {
		return WatchHeartbeat{}, false, err
	}
	return hb, found, nil
}
