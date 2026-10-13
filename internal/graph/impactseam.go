package graph

import (
	"slices"

	"github.com/Sawmonabo/codectx/internal/model"
)

// openSitesUncounted is the notice an impact answer carries when its reader
// cannot look declarations up by name: the open call sites a reached node is a
// candidate of cannot be listed, so open_sites_by_depth is absent because it
// was not counted, never because the count is zero.
const openSitesUncounted = "impact: open call sites were not counted (open_sites_by_depth is absent, not zero): " +
	"this index's reader does not look declarations up by name"

// openSiteSeam settles, before any walk, what an impact request can answer
// about open call sites on this engine's reader. A reader with the declaration
// lookup answers both. A reader without it refuses a request that names
// may_call with the answer graph.Candidates gives on that reader, before the
// precise walk is paid for rather than after it, and answers any other request
// with the notice that its open-site counts are absent. A reader that cannot be
// had at all is the walk's own error to report, so it answers neither here.
func (e *Engine) openSiteSeam(kinds []model.RelationKind) (string, error) {
	r, err := e.Reader()
	if err != nil || declarationReader(r) != nil {
		return "", nil
	}
	if slices.Contains(kinds, model.RelMayCall) {
		return "", candidatesUnavailable()
	}
	return openSitesUncounted, nil
}
