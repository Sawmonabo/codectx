package app

import (
	"context"
	"strconv"
	"strings"
	"time"

	"github.com/Sawmonabo/codectx/internal/model"
	"github.com/Sawmonabo/codectx/internal/storage/sqlite"
)

// routesPage answers the routes view of a session's manifest: every entry's
// evidence routes in (entry ordinal, route index) order, a page at a time. An
// entry names how many routes it has; this is where they are read, so a route
// is stored once in full and fetched only when asked for (ADR-0016 decision 7).
//
// The continuation is the entries projection's own signed cursor, its key the
// last route served as "<entry>:<index>".
func (s *Services) routesPage(ctx context.Context, req model.ContextPageRequest, rec sqlite.SessionRecord,
	manifest model.ContextManifest) (model.ContextPage, error) {
	st := s.w.s
	afterEntry, afterIndex, err := s.resumeRoutes(req, rec)
	if err != nil {
		return model.ContextPage{}, s.fail("context entries", err)
	}
	limit := entriesLimit(st.cfg.Resources.MaxPageItems, req.Page.Limit)
	rows, err := st.store.ManifestRoutes(ctx, rec.ManifestID, afterEntry, afterIndex, limit+1)
	if err != nil {
		return model.ContextPage{}, s.fail("context entries", err)
	}
	page := model.ContextPage{
		Meta:       model.QueryMeta{Binding: rec.Binding, Completeness: manifest.Completeness},
		SessionID:  rec.ID,
		ManifestID: rec.ManifestID,
		View:       req.View,
	}
	more := len(rows) > limit
	if more {
		rows = rows[:limit]
	}
	page.Routes = rows
	if more {
		last := rows[len(rows)-1]
		token, why, err := s.entriesCursor(ctx, rec, req.View, routeKey(last.Entry, last.Index))
		if err != nil {
			return model.ContextPage{}, s.fail("context entries", err)
		}
		if token != "" {
			page.Meta.NextCursor = token
		} else {
			page.Meta.Truncated, page.Meta.TruncationReason = true, why
		}
	}
	if err := page.Validate(); err != nil {
		return model.ContextPage{}, s.fail("context entries", err)
	}
	return page, nil
}

// routeKey is a routes cursor's key: the last route served.
func routeKey(entry, index int) string {
	return strconv.Itoa(entry) + ":" + strconv.Itoa(index)
}

// resumeRoutes reads the route a routes cursor ends at, (-1, -1) before the
// first route, with the checks resumeEntries applies to every entries cursor.
func (s *Services) resumeRoutes(req model.ContextPageRequest, rec sqlite.SessionRecord) (int, int, error) {
	if req.Page.Cursor == "" {
		return -1, -1, nil
	}
	c, err := s.w.s.signer.DecodeCursor(req.Page.Cursor, entriesEndpoint, time.Now())
	if err != nil {
		return 0, 0, err
	}
	if c.GenerationID != rec.Binding.GenerationID || c.AnalysisKey != rec.Binding.AnalysisKey {
		return 0, 0, cursorInvalid("cursor was issued against a different generation")
	}
	if c.QueryHash != entriesQueryHash(rec, req.View) {
		return 0, 0, cursorInvalid("cursor was issued for a different session, actor or view")
	}
	if c.SpoolID != "" {
		return 0, 0, cursorInvalid("cursor carries spooled traversal state this endpoint does not produce")
	}
	entry, index, ok := strings.Cut(c.LastKey, ":")
	e, eerr := strconv.Atoi(entry)
	i, ierr := strconv.Atoi(index)
	if !ok || eerr != nil || ierr != nil || e < 0 || i < 0 {
		return 0, 0, cursorInvalid("cursor sort key is not a manifest route")
	}
	return e, i, nil
}
