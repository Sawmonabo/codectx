package pagination

import (
	"context"
	"fmt"
	"sync"

	"github.com/Sawmonabo/codectx/internal/model"
)

// PageClamps collects every page bound a reader resolved differently from what
// its caller asked for, so the clamp reaches the actor as
// "requested N, effective M" instead of happening silently.
//
// The clamp itself is legitimate (class A: model.MaxPageItems is a hard wire
// ceiling and a page is always continuable by cursor, so nothing is lost). What
// the scale posture forbids is the SILENCE: a caller that asks for 1000 and is
// served 200 with no continuation notice cannot tell a clamped page from the
// end of the answer. A zero is "no caller bound" on the edge batch limit, so
// it resolves to 200 here rather than being refused.
//
// It lives here, beside the cursors and spools the "continue with the cursor"
// half of its message points at, rather than in the storage package that
// records into it: the query engine and the context compiler drain a collector
// into their answer's notices, and a package that must not depend on the store
// to report a clamp would otherwise have to import it for this type alone.
// storage/sqlite records into it and re-exports the two names its callers
// already use.
//
// It travels on the context rather than through every reader signature. Two
// dozen reader methods on the pinned reader return rows only, and threading an
// out-parameter through all of them (and through their callers in the context
// compiler, the graph engine and the app layer) would be a far larger change
// than the observation is worth. Carrying an out-of-band observation sink on the
// context is the same shape as net/http/httptrace.WithClientTrace, which exists
// for exactly this reason.
// See https://pkg.go.dev/net/http/httptrace#WithClientTrace.
type PageClamps struct {
	mu    sync.Mutex
	notes map[string]struct{}
	order []string
}

type pageClampKey struct{}

// WithPageClamps returns a context that collects page-bound clamps and the
// collector to read them from once the reads are done. A caller that builds a
// model.QueryMeta installs one and drains it into QueryMeta.Notices.
func WithPageClamps(ctx context.Context) (context.Context, *PageClamps) {
	c := &PageClamps{notes: map[string]struct{}{}}
	return context.WithValue(ctx, pageClampKey{}, c), c
}

// record notes one clamp, deduplicated: a keyset walk calls the same reader
// once per page and the actor needs the fact, not the repetition.
func (c *PageClamps) record(requested, effective int) {
	c.add(fmt.Sprintf("a page bound was clamped: requested %d, effective %d (the wire ceiling is %d; "+
		"continue with the cursor to read the rest)", requested, effective, model.MaxPageItems))
}

func (c *PageClamps) add(note string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, seen := c.notes[note]; seen {
		return
	}
	c.notes[note] = struct{}{}
	c.order = append(c.order, note)
}

// RecordUnbounded notes a request that named no bound of its own and was served
// at the wire ceiling. It is recorded only where the caller's zero is a USER
// setting that means unlimited -- the edge batch limit -- and not at the many
// internal sites that pass 0 simply because they never had a page bound to
// pass, where it would be noise rather than news.
func RecordUnbounded(ctx context.Context, effective int) {
	c, ok := ctx.Value(pageClampKey{}).(*PageClamps)
	if !ok {
		return
	}
	note := fmt.Sprintf("a page bound was resolved: requested unlimited, effective %d "+
		"(the wire ceiling; continue with the cursor to read the rest)", effective)
	c.add(note)
}

// Notices is what was clamped, in the order it was first observed. It is empty
// when every request was served at the size it asked for.
func (c *PageClamps) Notices() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.order...)
}

// PageLimit resolves a requested page size against the wire ceiling and reports
// the resolution to the context's collector when the two differ. A request of 0
// is "no caller-side bound" and resolves to the ceiling; that is a resolution,
// not a clamp of a number the caller chose, so only a positive request above the
// ceiling is reported.
func PageLimit(ctx context.Context, limit int) int {
	if limit <= 0 || limit > model.MaxPageItems {
		if limit > model.MaxPageItems {
			if c, ok := ctx.Value(pageClampKey{}).(*PageClamps); ok {
				c.record(limit, model.MaxPageItems)
			}
		}
		return model.MaxPageItems
	}
	return limit
}
