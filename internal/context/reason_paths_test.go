package context

import (
	"fmt"
	"strings"
	"testing"

	"github.com/Sawmonabo/codectx/internal/config"
	"github.com/Sawmonabo/codectx/internal/model"
)

// TestConfiguredReasonPathsSurviveToTheStoredEntry pins the chain from the
// setting to the stored entry: context.max_reason_paths_per_entry is honoured
// by ranking, and neither scope, budgeting nor model.ContextEntry's own
// validator may silently re-clip the result back to the smaller model
// constant. The failure it protects against is a manifest that quietly stores
// three explanation routes when the operator configured more -- or unlimited --
// with nothing in the answer saying routes were discarded.
func TestConfiguredReasonPathsSurviveToTheStoredEntry(t *testing.T) {
	const routes = model.MaxReasonPathsPerEntry + 2
	paths := make([]model.RelationPath, 0, routes)
	for i := 0; i < routes; i++ {
		paths = append(paths, model.RelationPath{
			Relations: []model.RelationID{model.RelationID(hexID(byte(i)))},
		})
	}

	// scope: a configured value ABOVE the model constant is applied as written.
	kept, more := boundPaths(paths, config.Limit(routes))
	if len(kept) != routes || more != 0 {
		t.Fatalf("boundPaths(limit %d) kept %d paths, dropped %d; want %d kept and none dropped",
			routes, len(kept), more, routes)
	}
	// scope: unlimited keeps every route.
	if kept, more = boundPaths(paths, config.Unlimited); len(kept) != routes || more != 0 {
		t.Fatalf("boundPaths(unlimited) kept %d paths, dropped %d; want %d kept and none dropped",
			len(kept), more, routes)
	}
	// scope: a SMALLER configured value still cuts, and reports the cut.
	if kept, more = boundPaths(paths, config.Limit(1)); len(kept) != 1 || more != int64(routes-1) {
		t.Fatalf("boundPaths(limit 1) kept %d paths, dropped %d; want 1 kept and %d dropped",
			len(kept), more, routes-1)
	}

	// budget: the projection onto the stored entry does not re-clip the count.
	stored := evidencePaths(paths)
	if len(stored) != routes {
		t.Fatalf("evidencePaths stored %d routes, want all %d: budgeting re-clipped what rank honoured",
			len(stored), routes)
	}

	// model: the persisted entry is not refused for carrying them.
	entry := model.ContextEntry{
		Ordinal:       0,
		NodeID:        model.NodeID(hexID(0xaa)),
		Requirement:   model.RequirementOptional,
		EvidencePaths: stored,
	}
	if err := entry.Validate(); err != nil {
		t.Fatalf("ContextEntry.Validate refused %d evidence paths: %v", routes, err)
	}
}

// TestARouteIsStoredWholeHoweverLong pins that no length is cut from a route
// on its way to the stored entry: a route of model.MaxRelationsPerPath+1
// relations (and one far past it) survives the ranking pass, the projection
// onto the entry, the entry's validation and the route row's validation with
// every relation in order.
//
// Mutations caught: the ranking pass dropping a route past the old bound;
// evidencePaths clipping it; ContextEntry.Validate or ContextRoute.Validate
// refusing it for its length.
func TestARouteIsStoredWholeHoweverLong(t *testing.T) {
	for _, n := range []int{model.MaxRelationsPerPath + 1, 4 * model.MaxRelationsPerPath} {
		rels := make([]model.RelationID, n)
		relations := make(map[model.RelationID]model.Relation, n)
		for i := range rels {
			rels[i] = model.RelationID(hexID(byte(i))[:62] + fmt.Sprintf("%02x", i/256))
			relations[rels[i]] = model.Relation{ID: rels[i], Kind: model.RelCalls}
		}
		cand := candidate{Paths: []model.RelationPath{{Relations: rels}}}
		routed, err := scoreRoutes(cand, relations, nil, config.Unlimited)
		if err != nil {
			t.Fatalf("scoreRoutes(%d relations): %v", n, err)
		}
		if len(routed.paths) != 1 || routed.morePaths != 0 {
			t.Fatalf("the ranking pass kept %d of one %d-relation route and counted %d more; want it kept",
				len(routed.paths), n, routed.morePaths)
		}
		stored := evidencePaths(routed.paths)
		if len(stored) != 1 || len(stored[0]) != n {
			t.Fatalf("a %d-relation route was stored with %d relations", n, len(stored[0]))
		}
		for i := range rels {
			if stored[0][i] != rels[i] {
				t.Fatalf("relation %d of the stored route is %s, want %s", i, stored[0][i], rels[i])
			}
		}
		entry := model.ContextEntry{NodeID: model.NodeID(hexID(0xaa)), Requirement: model.RequirementOptional,
			EvidencePaths: stored, RouteCount: 1}
		if err := entry.Validate(); err != nil {
			t.Fatalf("ContextEntry.Validate refused a %d-relation route: %v", n, err)
		}
		if err := (model.ContextRoute{Relations: stored[0]}).Validate(); err != nil {
			t.Fatalf("ContextRoute.Validate refused a %d-relation route: %v", n, err)
		}
	}
}

// hexID is a syntactically valid 64-hex-character identity: these tests assert
// count semantics, not identity derivation.
func hexID(seed byte) string {
	return strings.Repeat(fmt.Sprintf("%02x", seed), 32)
}

// TestBoundedExplanationsReachTheManifestNotices guards explanation disclosure:
// the three field cuts the compile applies to an entry's explanation --
// reasons past the per-entry count, a reason past its byte bound, and the page
// size a configuration asked for and could not have -- are DISCLOSED on the
// manifest header rather than applied silently. The cut itself is not the
// defect (the bounds are wire shapes); a manifest that cannot be told apart
// from one that cut nothing is.
func TestBoundedExplanationsReachTheManifestNotices(t *testing.T) {
	reasons := make([]string, 0, model.MaxReasonsPerEntry+2)
	for i := 0; i < model.MaxReasonsPerEntry+2; i++ {
		reasons = append(reasons, "reason")
	}
	reasons[0] = strings.Repeat("x", model.MaxReasonBytes+7)
	kept, dropped, truncated := boundReasons(reasons)
	if len(kept) != model.MaxReasonsPerEntry || dropped != 2 || truncated != 1 {
		t.Fatalf("boundReasons kept %d, reported %d dropped and %d truncated; want %d kept, 2 dropped, 1 truncated",
			len(kept), dropped, truncated, model.MaxReasonsPerEntry)
	}
	if len(kept[0]) != model.MaxReasonBytes {
		t.Fatalf("the truncated reason is %d bytes, want the %d-byte bound", len(kept[0]), model.MaxReasonBytes)
	}

	c := &Compiler{}
	c.cfg.Resources.MaxPageItems = model.MaxPageItems + 500
	notices := c.withPageNotice(planNotices(scopeResult{ReasonsDropped: dropped, ReasonsTruncated: truncated},
		planParts{}, true))
	if len(notices) != 3 {
		t.Fatalf("the compile disclosed %d notices %q, want one per cut", len(notices), notices)
	}
	joined := strings.Join(notices, "\n")
	for _, want := range []string{
		fmt.Sprintf("requested %d, effective %d", model.MaxPageItems+500, model.MaxPageItems),
		"2 selection reason(s) past", "1 selection reason(s) were truncated",
	} {
		if !strings.Contains(joined, want) {
			t.Fatalf("the notices %q do not disclose %q", notices, want)
		}
	}
	// The header must not be refusable over its own disclosure.
	m := model.ContextManifest{Notices: notices}
	for i, note := range m.Notices {
		if strings.TrimSpace(note) == "" || len(note) > model.MaxReasonBytes {
			t.Fatalf("notice %d is %d bytes; ContextManifest.Validate would refuse the compile it explains", i, len(note))
		}
	}
	// A compile under no raised bound and no cut discloses nothing.
	if n := (&Compiler{}).withPageNotice(planNotices(scopeResult{}, planParts{}, true)); len(n) != 0 {
		t.Fatalf("an unbounded compile disclosed %q", n)
	}
}
