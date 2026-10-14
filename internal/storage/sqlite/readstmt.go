package sqlite

import (
	"context"
	"database/sql"
	"sync"
)

// readStmts holds the reader pool's prepared statements for read paths whose
// SQL texts are fixed, one handle per text. database/sql keeps one driver
// statement per pooled connection under a handle prepared on the pool, and
// Tx.StmtContext reuses the one already prepared on the transaction's
// connection, so a statement a page runs in its own read transaction binds new
// values instead of parsing its text again. Parsing is not free: parsing was an
// eighth of a tier walk page's CPU when each page prepared its statement.
//
// Only fixed texts may come here (the tier walk's, tier_walk.go, sixteen in
// all), so the map is bounded by the code, never by the data. The handles live
// as long as the store and are closed with it.
type readStmts struct {
	mu sync.Mutex
	by map[string]*sql.Stmt
}

// stmt answers the pool's handle for query, preparing it on first use. It must
// be called outside a read transaction: preparing takes a pooled connection,
// and a caller holding one while every other connection waits on the same
// first preparation would wait forever. The lock is not held while preparing,
// so two first uses may both prepare; the later one is closed.
func (c *readStmts) stmt(ctx context.Context, db *sql.DB, query string) (*sql.Stmt, error) {
	c.mu.Lock()
	st, ok := c.by[query]
	c.mu.Unlock()
	if ok {
		return st, nil
	}
	st, err := db.PrepareContext(ctx, query)
	if err != nil {
		return nil, wrap("prepare", err)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if held, ok := c.by[query]; ok {
		st.Close()
		return held, nil
	}
	if c.by == nil {
		c.by = map[string]*sql.Stmt{}
	}
	c.by[query] = st
	return st, nil
}

// close closes every handle; the store calls it before closing its pools.
func (c *readStmts) close() {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, st := range c.by {
		st.Close()
	}
	clear(c.by)
}
