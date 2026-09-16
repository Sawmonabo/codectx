package sqlite

import (
	"context"
	"database/sql"
	"time"

	"github.com/Sawmonabo/codectx/internal/model"
)

// WatchHeartbeat is what one running watch publishes about itself so a second
// process can read it: which watch it is, which process owns it, when it last
// published, when its last pass completed, how many notification events are
// still pending, and the deadline the writer promised to refresh before.
//
// SessionID is the watch's own identity, minted once per watch. It, and not the
// pid, is what tells two watching processes apart: a pid is reused by the
// operating system, so a row keyed by one could be claimed by an unrelated
// process that inherited the number.
//
// ExpiresAt is the writer's own deadline and the only liveness signal. A reader
// compares it to the clock and never recomputes it from configuration: the two
// processes need not share a configuration, and a reader that derived the
// window would grant or deny liveness the writer never promised.
//
// BeatAt is when the writer last published the row. It is reported beside the
// watch, never judged: deriving liveness from it would be the reader
// reconstructing the writer's window, which is what ExpiresAt exists to prevent.
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
	SessionID     string
	WriterPID     int
	BeatAt        time.Time
	LastPassAt    *time.Time
	PendingEvents *int64
	ExpiresAt     time.Time
}

// Live reports whether the process that published this row is still refreshing
// it, which is the one rule this product answers "is that process still there"
// by: the writer's own unelapsed deadline, compared against the reader's clock.
// It is the rule the run ledger judges a run by, stated once here so that every
// surface reading a heartbeat -- the status listing, the doctor's check, the
// resource block -- and the sweep below cannot come to different conclusions
// about the same row. No pid is probed: probing one is neither portable nor
// race-free, and the number may since have been reused.
func (hb WatchHeartbeat) Live(now time.Time) bool { return hb.ExpiresAt.After(now) }

// maxWatchHeartbeats bounds one read of the heartbeat rows. It is the page
// width every other unpaged list in a result carries (Section 6 requires an
// explicit finite bound on every response), not a ceiling on how many watches a
// workspace may have: the rows of processes that stopped are deleted by the
// sweep below, so what survives it is the set of watches that were live when
// somebody last beat.
const maxWatchHeartbeats = model.MaxRecordsPerResult

// RecordWatchHeartbeat publishes hb as this watch's live heartbeat, replacing
// whatever this same watch left. There is one row per watching process and no
// history: this is liveness, and a watch that has ended leaves nothing worth
// keeping.
//
// The same write deletes the repository's rows whose deadline has elapsed, by
// the same rule and the same clock reading a reader judges them by, so what a
// reader is told is alive and what this sweep is willing to delete can never
// disagree. Without it the rows of processes that died without withdrawing
// would accumulate for ever. The sweep runs after the upsert, so the beat being
// published can never be its own victim.
func (s *Store) RecordWatchHeartbeat(ctx context.Context, repo model.RepositoryID, hb WatchHeartbeat) error {
	raw, err := idBlob("repository_id", string(repo))
	if err != nil {
		return err
	}
	session, err := idBlob("session_id", hb.SessionID)
	if err != nil {
		return err
	}
	if hb.WriterPID <= 0 {
		return invalid("a watch heartbeat needs the writing process id")
	}
	if hb.BeatAt.IsZero() {
		return invalid("a watch heartbeat needs the time it was published")
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
		_, err := tx.ExecContext(ctx, `INSERT INTO watch_heartbeat(repository_id, session_id, writer_pid, beat_at, last_pass_at, pending_events, expires_at)
			VALUES(?, ?, ?, ?, ?, ?, ?) ON CONFLICT(repository_id, session_id) DO UPDATE SET
				writer_pid = excluded.writer_pid, beat_at = excluded.beat_at,
				last_pass_at = excluded.last_pass_at,
				pending_events = excluded.pending_events, expires_at = excluded.expires_at`,
			raw, session, hb.WriterPID, formatTime(hb.BeatAt), lastPass, pending, formatTime(hb.ExpiresAt))
		if err != nil {
			return wrap("watch_heartbeat", err)
		}
		_, err = tx.ExecContext(ctx, `DELETE FROM watch_heartbeat WHERE repository_id = ? AND NOT (expires_at > ?)`,
			raw, formatTime(hb.BeatAt))
		return wrap("watch_heartbeat", err)
	})
}

// ClearWatchHeartbeat removes this watch's own heartbeat and no other's. A
// watch that ends deliberately withdraws its claim at once rather than leaving a
// fresh row that reads as live coverage until it expires; expiry remains the
// answer for a process that died without getting here.
func (s *Store) ClearWatchHeartbeat(ctx context.Context, repo model.RepositoryID, session string) error {
	raw, err := idBlob("repository_id", string(repo))
	if err != nil {
		return err
	}
	sessionRaw, err := idBlob("session_id", session)
	if err != nil {
		return err
	}
	return s.write(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `DELETE FROM watch_heartbeat WHERE repository_id = ? AND session_id = ?`, raw, sessionRaw)
		return wrap("watch_heartbeat", err)
	})
}

// WatchHeartbeats reads every heartbeat row this repository holds, freshest
// deadline first. An empty result is a workspace no watch has ever published in,
// which is a different answer from a row that has expired: nothing has run here,
// rather than something ran and stopped.
//
// Expired rows are returned rather than filtered, because whether a row is live
// is the caller's judgement, made against ExpiresAt through Live -- and an
// expired row is the only evidence that a watch ran here and stopped, which is
// what the doctor must warn about. This method reports what was written and
// interprets nothing.
//
// The order is by deadline and then by session, so two readers see one order.
// It is the deadline and not the beat time because two watches need not share a
// configuration, and under different windows the two orders diverge; the
// deadline is the stamp liveness is judged by, so it is the one that ranks.
func (s *Store) WatchHeartbeats(ctx context.Context, repo model.RepositoryID) ([]WatchHeartbeat, error) {
	raw, err := idBlob("repository_id", string(repo))
	if err != nil {
		return nil, err
	}
	var out []WatchHeartbeat
	err = s.read(ctx, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, `SELECT session_id, writer_pid, beat_at, last_pass_at, pending_events, expires_at
			FROM watch_heartbeat WHERE repository_id = ?
			ORDER BY expires_at DESC, session_id LIMIT ?`, raw, maxWatchHeartbeats)
		if err != nil {
			return wrap("watch_heartbeat", err)
		}
		defer rows.Close()
		for rows.Next() {
			var session []byte
			var pid int64
			var beat string
			var lastPass sql.NullString
			var pending sql.NullInt64
			var expires string
			if err := rows.Scan(&session, &pid, &beat, &lastPass, &pending, &expires); err != nil {
				return wrap("watch_heartbeat", err)
			}
			beatAt, err := parseTime(beat)
			if err != nil {
				return err
			}
			expiresAt, err := parseTime(expires)
			if err != nil {
				return err
			}
			hb := WatchHeartbeat{SessionID: idHex(session), WriterPID: int(pid), BeatAt: beatAt, ExpiresAt: expiresAt}
			if lastPass.Valid {
				at, err := parseTime(lastPass.String)
				if err != nil {
					return err
				}
				hb.LastPassAt = &at
			}
			if pending.Valid {
				n := pending.Int64
				hb.PendingEvents = &n
			}
			out = append(out, hb)
		}
		return wrap("watch_heartbeat", rows.Err())
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}
