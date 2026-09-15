package sqlite

import (
	"context"
	"database/sql"
)

// stmtCache holds the prepared statements of ONE write transaction.
//
// The unit writer's hot loops -- a fact's input-file check, a relation
// endpoint's canonical lookup, an evidence row's native-key probe, the
// dictionary upserts and the evidence insert -- issue the SAME handful of SQL
// texts once per row. Handed straight to *sql.Tx, each of those calls prepares
// the statement, runs it and finalises it, so the engine PARSES the text again
// for every row: measured on a single unit holding 16 000 node facts and
// 16 000 relation facts, sqlite3Prepare accounted for a quarter of the whole
// publication's CPU, all of it re-parsing a dozen distinct queries.
//
// The cache prepares each text once per transaction and reuses the handle for
// every subsequent row, which is the single-held-statement pattern QPERF-2
// established for the read paths. It changes no SQL, no bound value and no
// result: a *sql.Stmt prepared on a Tx runs on that Tx, so the rows a cached
// statement sees are exactly the rows the ad-hoc call saw.
//
// Lifetime is the transaction's. Every statement is closed when the
// transaction's closure returns, and the cache is bound to the *sql.Tx it was
// built for: a caller that reaches it with a different transaction gets a
// statement prepared on that transaction instead, never a handle from a
// finished one. Size is the number of DISTINCT SQL texts in the writer's
// paths -- a dozen -- and never a function of rows, facts or repository size.
type stmtCache struct {
	tx *sql.Tx
	by map[string]*sql.Stmt
}

func newStmtCache(tx *sql.Tx) *stmtCache {
	return &stmtCache{tx: tx, by: make(map[string]*sql.Stmt, 16)}
}

// prepare returns the transaction's handle for query, preparing it on first
// sight. A cache that does not belong to tx prepares an uncached statement for
// the caller to close, which keeps the helper correct for any caller rather
// than only for the writer that installed it.
func (c *stmtCache) prepare(ctx context.Context, tx *sql.Tx, query string) (stmt *sql.Stmt, cached bool, err error) {
	if c == nil || c.tx != tx {
		stmt, err := tx.PrepareContext(ctx, query)
		return stmt, false, err
	}
	if stmt, ok := c.by[query]; ok {
		return stmt, true, nil
	}
	stmt, err = tx.PrepareContext(ctx, query)
	if err != nil {
		return nil, false, err
	}
	c.by[query] = stmt
	return stmt, true, nil
}

// close finalises every statement the transaction prepared through the cache.
// It is called as the transaction's closure returns; the error of a Close is
// dropped deliberately, as SQLite reports a statement's real failure from the
// step that produced it and this one runs on the way out of a path that has
// already decided its outcome.
func (c *stmtCache) close() {
	if c == nil {
		return
	}
	for _, stmt := range c.by {
		stmt.Close()
	}
	clear(c.by)
}

// queryRow, query and exec are the three call shapes the writer's per-row
// helpers use. Each runs through the cached handle, so the text is parsed once
// per transaction instead of once per row.
func (c *stmtCache) queryRow(ctx context.Context, tx *sql.Tx, query string, args ...any) *sql.Row {
	stmt, cached, err := c.prepare(ctx, tx, query)
	if err != nil {
		// Fall back to the transaction so the caller still gets a *sql.Row
		// carrying the real error rather than a nil dereference.
		return tx.QueryRowContext(ctx, query, args...)
	}
	if !cached {
		defer stmt.Close()
	}
	return stmt.QueryRowContext(ctx, args...)
}

func (c *stmtCache) exec(ctx context.Context, tx *sql.Tx, query string, args ...any) (sql.Result, error) {
	stmt, cached, err := c.prepare(ctx, tx, query)
	if err != nil {
		return nil, err
	}
	if !cached {
		defer stmt.Close()
	}
	return stmt.ExecContext(ctx, args...)
}
