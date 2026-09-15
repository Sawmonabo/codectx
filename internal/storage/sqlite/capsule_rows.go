package sqlite

import (
	"context"
	"database/sql"
	"strings"
	"time"

	"github.com/Sawmonabo/codectx/internal/model"
)

// capsuleRowBatch is how many capsule records one INSERT carries.
//
// It is a working-set size, not a limit on how many records a capsule may hold:
// a list of any length is written as however many batches it takes, and the
// writer's peak heap is one batch of encoded records regardless. 512 keeps the
// statement's parameter count (5 per row) well inside SQLite's default variable
// ceiling while amortising the per-statement cost across a large seal.
const capsuleRowBatch = 512

// capsuleRowColumns is the per-row parameter count of the batched INSERT.
const capsuleRowColumns = 5

// writeCapsuleRows streams a sealing capsule's eight record lists into
// context_capsule_rows, inside the caller's write transaction.
//
// It runs in the transaction that inserted the capsule row, after the seal's
// "a capsule already exists" check, so the rows and the capsule they belong to
// commit together: a capsule is never visible without its records, a second
// seal never reaches this function, and there is no window in which another
// writer could clobber a half-written list.
//
// Each list is streamed once and written in batches, so the memory the seal
// costs is a function of the batch size and not of how many records the session
// observed. The counts are the ones already hashed into the capsule's identity;
// a list that streams a different number of records than its count would seal an
// identity its own rows do not reproduce, so it is refused here and the whole
// transaction rolls back.
func writeCapsuleRows(ctx context.Context, tx *sql.Tx, sessionRaw []byte,
	counts model.CapsuleCounts, src model.CapsuleListSource) error {
	if src == nil {
		return internal("capsule rows: the seal named no record source")
	}
	args := make([]any, 0, capsuleRowBatch*capsuleRowColumns)
	flush := func() error {
		if len(args) == 0 {
			return nil
		}
		rows := len(args) / capsuleRowColumns
		var b strings.Builder
		b.WriteString(`INSERT INTO context_capsule_rows(session_id, list, ordinal, row_key, row_json) VALUES `)
		for i := 0; i < rows; i++ {
			if i > 0 {
				b.WriteByte(',')
			}
			b.WriteString(`(?, ?, ?, ?, ?)`)
		}
		_, err := tx.ExecContext(ctx, b.String(), args...)
		args = args[:0]
		return wrap("context_capsule_rows", err)
	}
	for _, list := range model.CapsuleListOrder {
		var written int64
		err := src.Rows(ctx, list, func(row model.CapsuleRow) error {
			if err := row.Validate(); err != nil {
				return err
			}
			if row.List != list {
				return internal("capsule rows: the " + string(list) + " list streamed a " + string(row.List) + " record")
			}
			if row.Ordinal != written {
				return internal("capsule rows: the " + string(list) + " list skipped an ordinal")
			}
			written++
			args = append(args, sessionRaw, string(row.List), row.Ordinal, row.Key, string(row.JSON))
			if len(args) >= capsuleRowBatch*capsuleRowColumns {
				return flush()
			}
			return nil
		})
		if err != nil {
			return err
		}
		if err := flush(); err != nil {
			return err
		}
		if written != counts.Of(list) {
			return internal("capsule rows: the " + string(list) + " list wrote a different number of records than its sealed count")
		}
	}
	return nil
}

// CapsuleRows reads one keyset page of one list of a session's sealed capsule.
//
// after is the cursor a previous page ended on -- a row's own key, not an
// offset -- and empty starts at the first record. A cursor naming no row in
// this list is refused as CTX_CURSOR_INVALID rather than silently restarting
// the list, which would make a continuation read as a complete answer.
//
// The caller decides whether there is a next page from the capsule's count for
// this list and the last row's ordinal, so the last page is the last page: no
// extra round trip and no empty trailing page.
func (s *Store) CapsuleRows(ctx context.Context, session model.SessionID, actor string,
	list model.CapsuleList, after string, limit int) ([]model.CapsuleRow, error) {
	if !list.Valid() {
		return nil, invalid("capsule list %q is not a capsule list", string(list))
	}
	if limit <= 0 {
		return nil, invalid("capsule rows: a page size of %d reads nothing", limit)
	}
	var out []model.CapsuleRow
	err := s.read(ctx, func(tx *sql.Tx) error {
		rec, err := s.session(ctx, tx, session, actor, time.Now())
		if err != nil {
			if typed, ok := err.(*model.Error); !ok || typed.Code != model.CodeSessionExpired {
				return err
			}
		}
		sessionRaw, _ := model.DecodeID(string(rec.ID))
		var sealed int
		err = tx.QueryRowContext(ctx, `SELECT 1 FROM context_capsules WHERE session_id = ?`, sessionRaw).Scan(&sealed)
		if isNoRows(err) {
			return invalid("session %s has no capsule", session)
		}
		if err != nil {
			return wrap("context_capsules", err)
		}
		// -1 rather than 0: ordinals start at 0, so a first page must admit it.
		var afterOrdinal int64 = -1
		if after != "" {
			err = tx.QueryRowContext(ctx,
				`SELECT ordinal FROM context_capsule_rows WHERE session_id = ? AND list = ? AND row_key = ?`,
				sessionRaw, string(list), after).Scan(&afterOrdinal)
			if isNoRows(err) {
				return (&model.Error{Code: model.CodeCursorInvalid,
					Message: "cursor names no record in this capsule projection"}).
					WithDetail("endpoint", "context_capsule")
			}
			if err != nil {
				return wrap("context_capsule_rows", err)
			}
		}
		rows, err := tx.QueryContext(ctx,
			`SELECT ordinal, row_key, row_json FROM context_capsule_rows
				WHERE session_id = ? AND list = ? AND ordinal > ? ORDER BY ordinal LIMIT ?`,
			sessionRaw, string(list), afterOrdinal, limit)
		if err != nil {
			return wrap("context_capsule_rows", err)
		}
		defer rows.Close()
		for rows.Next() {
			row := model.CapsuleRow{List: list}
			var payload string
			if err := rows.Scan(&row.Ordinal, &row.Key, &payload); err != nil {
				return wrap("context_capsule_rows", err)
			}
			row.JSON = []byte(payload)
			out = append(out, row)
		}
		return wrap("context_capsule_rows", rows.Err())
	})
	return out, err
}
