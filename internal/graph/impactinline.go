package graph

import (
	"context"

	"github.com/Sawmonabo/codectx/internal/model"
	"github.com/Sawmonabo/codectx/internal/pagination"
)

// serveInline answers a ranked impact answer in the one response when one page
// of every view the request reads holds that whole view: at most limit entries
// and limit package pairs (the entries view pages the two together), at most
// limit reasons, and at most limit routes, which an entry count within limit
// already guarantees (an entry has one route or none).
//
// Such an answer is served whole and retains nothing: no spool, no lease, no
// answer token, no continuation. The bound is the page bound each view already
// serves under, so an inline response is never larger than the pages a client
// would otherwise fetch one view at a time. An answer that overflows any view
// it reads answers false, and the caller spools it exactly as before.
//
// page holds the first limit records of run, which is the whole run whenever
// the answer can be inline, so the check reads nothing beyond what the caller
// already holds in heap.
func (e *Engine) serveInline(ctx context.Context, run *pagination.SortedRun[impactRecord],
	pairRun *pagination.SortedRun[pairRecord], page []impactRecord, retain *retainedWalk,
	req model.ImpactRequest, limit int, answer *impactAnswer, meta *model.QueryMeta) (impactServed, bool, error) {
	if !answerFitsOnePage(run.Len(), pairRun.Len(), page, req, limit) {
		return impactServed{}, false, nil
	}
	var out impactServed
	if req.Reads(model.ImpactViewEntries) {
		pairs := make([]model.PackageEdge, 0, pairRun.Len())
		if err := pairRun.Each(func(r pairRecord) error {
			if err := ctx.Err(); err != nil {
				return typedContextError(ctx, err)
			}
			pairs = append(pairs, r.edge())
			return nil
		}); err != nil {
			return impactServed{}, false, err
		}
		answer.Packages = pairs
		entries, err := e.impactPage(ctx, page, 0, answer, meta)
		if err != nil {
			return impactServed{}, false, err
		}
		out.Entries = entries
	}
	if req.Reads(model.ImpactViewReasons) {
		out.Reasons, _ = cutReasons(page, 0, 0, limit)
	}
	if req.Reads(model.ImpactViewRoutes) {
		routes, err := retain.routeReader()
		if err != nil {
			return impactServed{}, false, err
		}
		out.Routes, err = routesOf(page, 0, routes)
		if cerr := routes.close(); err == nil && cerr != nil {
			err = internalErr("graph: the route table: " + cerr.Error())
		}
		if err != nil {
			return impactServed{}, false, err
		}
		if err := impactPhaseError(ctx, e.attachRouteEvidence(deliverCtx(ctx), out.Routes), meta); err != nil {
			return impactServed{}, false, err
		}
	}
	return out, true, nil
}

// answerFitsOnePage reports whether one page of limit items holds every view
// req reads of a ranked answer of total records and pairTotal package pairs,
// page being its first limit records.
func answerFitsOnePage(total, pairTotal int64, page []impactRecord, req model.ImpactRequest, limit int) bool {
	if total > int64(limit) || int64(len(page)) != total {
		return false
	}
	if req.Reads(model.ImpactViewEntries) && pairTotal > int64(limit) {
		return false
	}
	if req.Reads(model.ImpactViewReasons) {
		reasons := 0
		for _, r := range page {
			reasons += len(r.Reasons)
			if reasons > limit {
				return false
			}
		}
	}
	return true
}
