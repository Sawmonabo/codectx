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
