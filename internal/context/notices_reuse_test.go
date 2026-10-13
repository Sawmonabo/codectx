package context

import (
	"slices"
	"strings"
	"testing"

	"github.com/Sawmonabo/codectx/internal/model"
)

// TestAReusedManifestCarriesTheNoticesOfItsCompile pins that the notices a
// compile made are stored with its manifest: the second, identical request
// reuses the stored manifest and answers the same notices, the exclusion count
// among them, with the configuration's page-size notice still first.
//
// Mutation caught: the reuse path rebuilding the notices from empty scope and
// pack verdicts (the exclusion-count notice disappears on reuse), or the
// header write leaving the notices out of the stored header.
func TestAReusedManifestCarriesTheNoticesOfItsCompile(t *testing.T) {
	fx := newContextFixture(t)
	fx.Cfg.Resources.MaxPageItems = model.MaxPageItems + 500
	req := model.ContextRequest{Task: "make `Place` and `NoSuchSymbol` idempotent",
		Phase: model.PhaseVerify, Budget: model.Budget{MaxFiles: 1}}
	first, err := intCompiler(t, fx, fx.Now).Compile(fx.ctx, req)
	if err != nil {
		t.Fatalf("first Compile: %v", err)
	}
	if !strings.Contains(strings.Join(first.Notices, "\n"), excludedViewPointer) {
		t.Fatalf("the first compile's notices %q carry no exclusion count; the fixture no longer exercises it", first.Notices)
	}
	again, err := intCompiler(t, fx, fx.Now).Compile(fx.ctx, req)
	if err != nil {
		t.Fatalf("second Compile: %v", err)
	}
	if again.ID != first.ID {
		t.Fatalf("the second compile answered manifest %s, want the reused %s", again.ID, first.ID)
	}
	if !slices.Equal(again.Notices, first.Notices) {
		t.Fatalf("the reused manifest's notices are %q, want the compile's own %q", again.Notices, first.Notices)
	}
}

// TestAnIncompleteScopeAlwaysNamesTheExcludedView pins that a plan whose scope
// is incomplete with nothing excluded still names the surface that says why,
// on the compile and so on every reuse of its stored notices.
//
// Mutation caught: emitting the pointer only with an exclusion count, so an
// incomplete scope from a degraded capability is reported with no route to
// what is missing.
func TestAnIncompleteScopeAlwaysNamesTheExcludedView(t *testing.T) {
	if got := planNotices(scopeResult{}, planParts{}, false); !slices.Contains(got, excludedViewNotice) {
		t.Fatalf("an incomplete scope with no exclusion disclosed %q, want the excluded-view pointer", got)
	}
	if got := planNotices(scopeResult{}, planParts{}, true); len(got) != 0 {
		t.Fatalf("a complete scope with nothing cut disclosed %q", got)
	}
}
