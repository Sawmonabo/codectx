package context

import (
	"fmt"
	"strings"
	"testing"

	"github.com/Sawmonabo/codectx/internal/config"
	"github.com/Sawmonabo/codectx/internal/model"
)

// TestConfiguredReasonPathsSurviveToTheStoredEntry pins the class-G chain this
// wave removes: context.max_reason_paths_per_entry is honoured by the ranking
// lane, and neither the scope lane, the budget lane nor model.ContextEntry's
// own validator may silently re-clip the result back to the smaller model
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

	// budget: the projection onto the stored entry does not re-clip the count,
	// and a route it DOES have to clip is counted rather than shortened in
	// silence.
	stored, clipped := evidencePaths(paths)
	if len(stored) != routes || clipped != 0 {
		t.Fatalf("evidencePaths stored %d routes and reported %d clipped, want all %d and none clipped: the budget lane re-clipped what rank honoured",
			len(stored), clipped, routes)
	}
	long := []model.RelationPath{{Relations: make([]model.RelationID, model.MaxRelationsPerPath+1)}}
	for i := range long[0].Relations {
		long[0].Relations[i] = model.RelationID(hexID(byte(i)))
	}
	if got, clipped := evidencePaths(long); len(got[0]) != model.MaxRelationsPerPath || clipped != 1 {
		t.Fatalf("a route of %d relations stored %d relations and reported %d clipped; want %d stored and the cut reported once",
			model.MaxRelationsPerPath+1, len(got[0]), clipped, model.MaxRelationsPerPath)
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

// hexID is a syntactically valid 64-hex-character identity: these tests assert
// count semantics, not identity derivation.
func hexID(seed byte) string {
	return strings.Repeat(fmt.Sprintf("%02x", seed), 32)
}

// TestBoundedExplanationsReachTheManifestNotices is the row-17/F15 invariant:
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
	notices := c.manifestNotices(scopeResult{ReasonsDropped: dropped, ReasonsTruncated: truncated},
		plan{RelationsClipped: 3})
	if len(notices) != 4 {
		t.Fatalf("the compile disclosed %d notices %q, want one per cut", len(notices), notices)
	}
	joined := strings.Join(notices, "\n")
	for _, want := range []string{
		fmt.Sprintf("requested %d, effective %d", model.MaxPageItems+500, model.MaxPageItems),
		"2 selection reason(s) past", "1 selection reason(s) were truncated", "3 evidence route(s)",
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
	if n := (&Compiler{}).manifestNotices(scopeResult{}, plan{}); len(n) != 0 {
		t.Fatalf("an unbounded compile disclosed %q", n)
	}
}
