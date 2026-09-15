package index

import (
	"io"
	"log/slog"
	"strconv"
	"testing"

	"github.com/Sawmonabo/codectx/internal/model"
	"github.com/Sawmonabo/codectx/internal/provider/scip"
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
