package index

import (
	"context"
	"io"
	"log/slog"
	"maps"
	"strconv"
	"strings"
	"testing"

	"github.com/Sawmonabo/codectx/internal/index/plan"
	"github.com/Sawmonabo/codectx/internal/model"
	"github.com/Sawmonabo/codectx/internal/provider"
	"github.com/Sawmonabo/codectx/internal/provider/scip"
	"github.com/Sawmonabo/codectx/internal/workspace"
)

// TestCapabilityFoldSumsCollapsedCounts protects the aggregate's honesty above
// the reporting threshold.
//
// Above model.MaxCapabilityStates the report is collapsed per provider
// capability AND state and every fold is rewritten to the workspace scope,
// which regenerates the duplicate primary key the first fold removed: the
// partial fold and the failure fold both land at the workspace scope. So the
// key must be closed on the far side of the collapse and not only before it --
// otherwise the insert fails on a constraint and takes `codectx index` down
// instead of publishing the degraded generation it built.
//
// The second assertion is the one the FX-G21-A fold left open. The two rows
// that collide there stand for DISJOINT scope sets, so the survivor must carry
// their sum: keeping only the winner's `scopes` reported a smaller set than the
// row represents while the report claimed nothing was omitted.
//
// Mutation proof: in foldCollapsed, revert `scopes` to `countDetail(out[i])`
// and the scopes assertion fails with 256 against the expected 301.
func TestCapabilityFoldSumsCollapsedCounts(t *testing.T) {
	t.Parallel()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	const scopes = model.MaxCapabilityStates + 44
	big := newCapabilityReport()
	for i := range scopes {
		big.add(model.CapabilityState{ProviderID: scip.ID, Capability: "references",
			Scope:          "pkg:go:" + strconv.Itoa(i),
			State:          model.CapabilityPartial,
			DiagnosticCode: model.CodeProviderOutputInvalid})
	}
	big.addFailure(scip.ID, "references", "pkg:java:", model.CodeProviderTimeout)

	bounded := big.finish(log)
	seen := map[string]int{}
	for _, s := range bounded {
		seen[s.ProviderID+"\x00"+s.Capability+"\x00"+s.Scope]++
	}
	for key, n := range seen {
		if n > 1 {
			t.Fatalf("the bound published %d rows for primary key %q: %+v", n, key, bounded)
		}
	}
	// Under-claim, never over-claim: the most severe state is the survivor.
	if len(bounded) != 1 || bounded[0].State != model.CapabilityFailed {
		t.Fatalf("the bounded report is %+v, want one failed row for the capability", bounded)
	}
	if got, want := bounded[0].Details["scopes"], strconv.Itoa(scopes+1); got != want {
		t.Fatalf("the surviving row reports scopes=%q, want %q: the fold dropped the less severe row's count", got, want)
	}
	if got := bounded[0].Details["units_failed"]; got != "1" {
		t.Fatalf("the surviving row reports units_failed=%q, want %q", got, "1")
	}
}

// TestCapabilityFoldKeepsDegradationDetails protects the one channel a
// degradation that is not severe enough to change a state has.
//
// Two rows that collide on the fold key are two units of one capability, and
// what each gave up is true of the published row: keeping only the first one's
// details reported one unit's dropped records and silently dropped the other's,
// which is the silence the scale posture forbids. A fresh row's details are
// kept for the same reason -- clearing the map made `partial` the only way a
// provider could be heard at all, so a run that lost nothing had to over-claim
// a degradation to report an admitted-oversize count.
//
// Mutation proof: restore the first-wins fold in `add`
// (`r.rows[key] = existing.WithDetail(scopesDetail, ...)`) and the partial
// assertions fail on records_dropped=2 and one reason; restore `s.Details =
// nil` on the fresh row and the fresh assertion fails on an empty
// truncated_fields.
func TestCapabilityFoldKeepsDegradationDetails(t *testing.T) {
	t.Parallel()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	r := newCapabilityReport()
	r.add(model.CapabilityState{ProviderID: scip.ID, Capability: "references", Scope: "pkg:go:a",
		State: model.CapabilityPartial, DiagnosticCode: model.CodeResourceLimit,
		Details: map[string]string{"records_dropped": "2", "reason": "resource_limit"}})
	r.add(model.CapabilityState{ProviderID: scip.ID, Capability: "references", Scope: "pkg:go:a",
		State: model.CapabilityPartial, DiagnosticCode: model.CodeResourceLimit,
		Details: map[string]string{"records_dropped": "3", "reason": "unverified_binding"}})
	r.add(model.CapabilityState{ProviderID: scip.ID, Capability: "definitions", Scope: "pkg:go:b",
		State:   model.CapabilityFresh,
		Details: map[string]string{"truncated_fields": "7", "scope_key": "pkg:go:b"}})

	rows := map[string]model.CapabilityState{}
	for _, s := range r.finish(log) {
		rows[s.Capability] = s
	}
	partial, ok := rows["references"]
	if !ok {
		t.Fatalf("the report published no references row: %+v", rows)
	}
	if got := partial.Details["records_dropped"]; got != "5" {
		t.Fatalf("the colliding rows report records_dropped=%q, want %q: the counts of one unit were dropped", got, "5")
	}
	if got := partial.Details["reason"]; got != "resource_limit,unverified_binding" {
		t.Fatalf("the colliding rows report reason=%q, want both reasons deduped and joined in sorted order", got)
	}
	if got := partial.Details[scopesDetail]; got != "2" {
		t.Fatalf("the colliding rows report scopes=%q, want %q", got, "2")
	}
	fresh, ok := rows["definitions"]
	if !ok {
		t.Fatalf("the report published no definitions row: %+v", rows)
	}
	if got := fresh.Details["truncated_fields"]; got != "7" {
		t.Fatalf("the fresh row reports truncated_fields=%q, want %q: its degradation reaches no caller", got, "7")
	}
	if got, ok := fresh.Details[scopeKeyDetail]; ok {
		t.Fatalf("the fresh row kept scope_key=%q; the fold rewrote it to the workspace scope", got)
	}
}

// TestCapabilityFoldMergesInOrderIndependently protects the determinism the
// merged details are published under. They fold into details_json and from
// there into the AnalysisKey, while the rows themselves come from unit workers
// that run concurrently: two identical runs whose units finished in a
// different order must key identically. The union only stays a function of the
// set if a union too wide for model.MaxDetailBytes is cut between values --
// cutting at a byte offset leaves a fragment that the next fold carries as a
// value of its own, and which fragment that is depends on which row folded
// first.
//
// Mutation proof: cut the union with
// `model.TruncateField(strings.Join(parts, detailSeparator), model.MaxDetailBytes)`
// instead of dropping whole values, and the reverse-order maps differ on
// `reason`.
func TestCapabilityFoldMergesInOrderIndependently(t *testing.T) {
	t.Parallel()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	wide := func(prefix string) string {
		reasons := make([]string, 0, 30)
		for i := range 30 {
			reasons = append(reasons, prefix+"_reason_"+strconv.Itoa(100+i)+"_of_a_widely_degraded_unit")
		}
		return strings.Join(reasons, ",")
	}
	first := model.CapabilityState{ProviderID: scip.ID, Capability: "references", Scope: "pkg:go:a",
		State: model.CapabilityPartial, DiagnosticCode: model.CodeResourceLimit,
		Details: map[string]string{"reason": wide("alpha")}}
	second := first
	second.Details = map[string]string{"reason": wide("omega")}

	merged := func(rows ...model.CapabilityState) model.CapabilityState {
		r := newCapabilityReport()
		for _, row := range rows {
			r.add(row)
		}
		out := r.finish(log)
		if len(out) != 1 {
			t.Fatalf("the report published %d rows, want one: %+v", len(out), out)
		}
		return out[0]
	}
	forward, reverse := merged(first, second), merged(second, first)
	if !maps.Equal(forward.Details, reverse.Details) {
		t.Fatalf("the fold published %v folding forward and %v folding in reverse; the merge must be a function of the set",
			forward.Details, reverse.Details)
	}
	if got := forward.Details[truncatedDetail]; got != "reason" {
		t.Fatalf("the fold reports details_truncated=%q, want %q: the cut list is published as if it were whole", got, "reason")
	}
	if got := len(forward.Details["reason"]); got > model.MaxDetailBytes {
		t.Fatalf("the merged reason is %d bytes, want at most %d", got, model.MaxDetailBytes)
	}
	for _, value := range strings.Split(forward.Details["reason"], ",") {
		if !strings.HasSuffix(value, "_of_a_widely_degraded_unit") {
			t.Fatalf("the merged reason carries the fragment %q; the cut split a value", value)
		}
	}
}

// coverageProvider is the smallest provider the coverage pass reads: it asks
// for the descriptor and nothing else.
type coverageProvider struct{ desc model.ProviderDescriptor }

func (p coverageProvider) Descriptor() model.ProviderDescriptor { return p.desc }
func (p coverageProvider) Detect(context.Context, workspace.Root, workspace.Policy) (provider.Detection, error) {
	return provider.Detection{}, nil
}
func (p coverageProvider) IndexUnit(context.Context, provider.UnitRequest, provider.Sink) (model.ProviderResult, error) {
	return model.ProviderResult{}, nil
}

// TestCoverageReportsAFailedDeferredScopeAsPartial protects what `codectx
// status` says after a late publication.
//
// Failure mode: one deferred unit of a provider fails and the other nine
// publish, and every capability of that provider is reported `unavailable:
// units_deferred` -- a repository whose control dependence, data dependence,
// reads, writes and calls are all in the generation and queryable reads as a
// provider nobody has heard from. A scope that failed is not a scope still
// running: it is a failure, published as `partial` with that scope named,
// because the other scopes' facts are there.
//
// Mutation proof: in coveredProviders, drop the failedScopes case so a scope
// that failed falls through to `deferred` again.
func TestCoverageReportsAFailedDeferredScopeAsPartial(t *testing.T) {
	t.Parallel()
	const id, capability = "dependence", "calls"
	const sealedScope, failedScope = "pkg:javascript:app", "pkg:java:qa"

	g := &generation{
		caps: newCapabilityReport(),
		sel: provider.Selection{Active: []provider.Provider{coverageProvider{desc: model.ProviderDescriptor{
			ID: id, Version: "1", Capabilities: []string{capability}}}}},
		sealed:       map[string]bool{plan.Key(id, sealedScope): true},
		failedScopes: map[string]string{plan.Key(id, failedScope): model.CodeProviderOutputInvalid},
	}
	g.plan.Units = func(yield func(plan.Unit) error) error {
		for _, scope := range []string{sealedScope, failedScope} {
			if err := yield(plan.Unit{ProviderID: id, ScopeKey: scope, Deferred: true}); err != nil {
				return err
			}
		}
		return nil
	}
	if err := g.coverage(); err != nil {
		t.Fatalf("coverage: %v", err)
	}
	rows := g.caps.finish(slog.New(slog.NewTextHandler(io.Discard, nil)))
	if len(rows) != 1 {
		t.Fatalf("coverage published %d rows, want one: %+v", len(rows), rows)
	}
	row := rows[0]
	if row.State != model.CapabilityPartial {
		t.Errorf("capability state %q, want partial: nine scopes published and one failed", row.State)
	}
	if row.Details[scopeKeyDetail] != failedScope {
		t.Errorf("the row names scope %q, want the scope that failed (%q)", row.Details[scopeKeyDetail], failedScope)
	}
	if row.DiagnosticCode != model.CodeProviderOutputInvalid {
		t.Errorf("diagnostic code %q, want the failure's own", row.DiagnosticCode)
	}
	if row.Details["reason"] == "units_deferred" {
		t.Error("a scope that failed was reported as still running")
	}
}
