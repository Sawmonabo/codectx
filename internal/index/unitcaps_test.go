package index

import (
	"fmt"
	"strconv"
	"testing"

	"github.com/Sawmonabo/codectx/internal/model"
	"github.com/Sawmonabo/codectx/internal/provider/link"
	tslang "github.com/Sawmonabo/codectx/internal/provider/treesitter/lang"
)

// A generation's capability report is folded from the rows of every member
// unit, and most members of an incremental run are attached, not built. These
// cases hold the report of a generation that reuses units to the report of one
// that builds them: the counts it sums and the degradations it publishes cover
// every member, never only the units the run rebuilt.

// siteDetails are the `calls` row's site counts the linking unit publishes.
var siteDetails = []string{"sites", "bound_sites", "open_sites", "computed_sites", "external_sites"}

// linkingFixture is the index fixture with the linking provider registered
// beside the file providers, as the composition root registers it, so its
// units publish the `calls` site counts.
func linkingFixture(t *testing.T, files map[string]string) *fixture {
	t.Helper()
	f := newFixture(t, files)
	f.c = f.coordinator(append(f.providers(false), link.New(link.StoreTables(f.store))))
	return f
}

// twoModules is two Go modules, each one linking unit, each with calls that
// resolve inside it.
func twoModules() map[string]string {
	return map[string]string{
		"a/go.mod": "module example.com/a\n\ngo 1.27\n",
		"a/a.go":   "package a\n\nfunc A() int { return helperA() + helperA() }\n\nfunc helperA() int { return 1 }\n",
		"b/go.mod": "module example.com/b\n\ngo 1.27\n",
		"b/b.go":   "package b\n\nfunc B() int { return helperB() }\n\nfunc helperB() int { return 2 }\n",
	}
}

// callSites sums each site count over every `calls` row of the linking
// provider in states.
func callSites(t *testing.T, states []model.CapabilityState) map[string]int64 {
	t.Helper()
	out := map[string]int64{}
	rows := 0
	for _, s := range states {
		if s.ProviderID != link.ProviderID || s.Capability != string(model.RelCalls) {
			continue
		}
		rows++
		for _, key := range siteDetails {
			v, ok := s.Details[key]
			if !ok {
				continue
			}
			n, err := strconv.ParseInt(v, 10, 64)
			if err != nil {
				t.Fatalf("calls detail %s=%q is not a count", key, v)
			}
			out[key] += n
		}
	}
	if rows == 0 {
		t.Fatalf("the generation publishes no %s calls row among %d rows", link.ProviderID, len(states))
	}
	return out
}

// TestANoOpRunPublishesTheReportOfTheRunThatBuiltItsUnits indexes two modules,
// then indexes again with nothing changed, so every unit of the second
// generation is reused. Its capability rows must equal the first generation's,
// the `calls` site counts and their sums included.
//
// Mutation caught: attachReused adds no capability rows for the units it
// attaches (the second report loses the `calls` counts and every unit's
// details, and falls back to the bare coverage rows).
func TestANoOpRunPublishesTheReportOfTheRunThatBuiltItsUnits(t *testing.T) {
	f := linkingFixture(t, twoModules())
	cold, err := f.c.Index(f.ctx, model.IndexRequest{})
	if err != nil {
		t.Fatalf("cold index: %v", err)
	}
	coldSites := callSites(t, cold.Completeness)
	if coldSites["sites"] == 0 || coldSites["bound_sites"] == 0 {
		t.Fatalf("the cold generation counts no bound call sites (%v); the fixture's calls must resolve", coldSites)
	}
	again, err := f.c.Refresh(f.ctx, nil)
	if err != nil {
		t.Fatalf("no-op refresh: %v", err)
	}
	if again.UnitsBuilt != 0 || again.UnitsReused != cold.UnitsBuilt {
		t.Fatalf("an unchanged tree built %d and reused %d units, want 0 built and all %d reused",
			again.UnitsBuilt, again.UnitsReused, cold.UnitsBuilt)
	}
	if got, want := fmt.Sprintf("%+v", again.Completeness), fmt.Sprintf("%+v", cold.Completeness); got != want {
		t.Fatalf("the reusing generation's capability rows differ from the building one's:\nbuilt  %s\nreused %s", want, got)
	}
	if got := callSites(t, again.Completeness); fmt.Sprint(got) != fmt.Sprint(coldSites) {
		t.Fatalf("calls site totals %v after reuse, want %v", got, coldSites)
	}
	if again.Health != cold.Health {
		t.Fatalf("health %q after reuse, want %q", again.Health, cold.Health)
	}
}

// TestAReusedPartialUnitKeepsTheGenerationPartial indexes a tree with a file
// that does not parse cleanly, whose parsing unit seals with a `partial` row,
// then indexes again with nothing changed. The reused unit's row must still be
// published `partial`, with the same diagnostic code, and the generation must
// stay degraded.
//
// Mutations caught: a reused unit's stored rows dropped (the row disappears
// and the generation reads healthy), or read back as fresh (the row's state
// moves).
func TestAReusedPartialUnitKeepsTheGenerationPartial(t *testing.T) {
	const broken = "pkg/broken.go"
	f := newFixture(t, map[string]string{
		"go.mod":    "module example.com/fixture\n\ngo 1.27\n",
		"pkg/ok.go": goFile("pkg", "OK", ""),
		broken:      "package pkg\n\nfunc Broken( {\n\treturn\n",
	})
	scope := "file:" + broken
	partialRow := func(states []model.CapabilityState) (model.CapabilityState, bool) {
		for _, s := range states {
			if s.ProviderID == tslang.ProviderID && s.Scope == scope && s.State == model.CapabilityPartial {
				return s, true
			}
		}
		return model.CapabilityState{}, false
	}
	cold, err := f.c.Index(f.ctx, model.IndexRequest{})
	if err != nil {
		t.Fatalf("cold index: %v", err)
	}
	built, ok := partialRow(cold.Completeness)
	if !ok {
		t.Fatalf("the cold generation publishes no partial %s row for %s: %+v", tslang.ProviderID, scope, cold.Completeness)
	}
	if cold.Health != model.HealthDegraded {
		t.Fatalf("cold health %q with a partial unit, want %q", cold.Health, model.HealthDegraded)
	}
	again, err := f.c.Refresh(f.ctx, nil)
	if err != nil {
		t.Fatalf("no-op refresh: %v", err)
	}
	if again.UnitsBuilt != 0 {
		t.Fatalf("an unchanged tree rebuilt %d units; the partial unit must be reused", again.UnitsBuilt)
	}
	reused, ok := partialRow(again.Completeness)
	if !ok {
		t.Fatalf("the reusing generation publishes no partial row for %s: %+v", scope, again.Completeness)
	}
	if reused.DiagnosticCode != built.DiagnosticCode {
		t.Fatalf("the reused partial row's code is %q, want %q", reused.DiagnosticCode, built.DiagnosticCode)
	}
	if again.Health != model.HealthDegraded {
		t.Fatalf("health %q after reusing a partial unit, want %q", again.Health, model.HealthDegraded)
	}
}

// TestAnIncrementalRunCountsCallSitesOfEveryUnit edits one of two modules and
// refreshes it, so one linking unit is rebuilt and the other reused. The
// refreshed generation's `calls` totals must equal those of a full build of
// the same tree in a fresh store.
//
// Mutation caught: only the rebuilt units' rows are counted (the reused
// module's sites go missing from the totals).
func TestAnIncrementalRunCountsCallSitesOfEveryUnit(t *testing.T) {
	const edited = "b/b.go"
	const after = "package b\n\nfunc B() int { return helperB() + helperB() + helperB() }\n\nfunc helperB() int { return 2 }\n"
	f := linkingFixture(t, twoModules())
	cold, err := f.c.Index(f.ctx, model.IndexRequest{})
	if err != nil {
		t.Fatalf("cold index: %v", err)
	}
	keyA := link.ScopeKeyPrefix(link.FamilyGo) + "a"
	keyB := link.ScopeKeyPrefix(link.FamilyGo) + "b"
	unitA0, errA := f.store.SelectedUnit(f.ctx, cold.Binding.GenerationID, link.ProviderID, keyA)
	unitB0, errB := f.store.SelectedUnit(f.ctx, cold.Binding.GenerationID, link.ProviderID, keyB)
	if errA != nil || errB != nil {
		t.Fatalf("the cold generation selects no linking unit for %s (%v) or %s (%v)", keyA, errA, keyB, errB)
	}

	f.write(edited, after)
	res, err := f.c.Refresh(f.ctx, []string{edited})
	if err != nil {
		t.Fatalf("refresh: %v", err)
	}
	unitA1, errA := f.store.SelectedUnit(f.ctx, res.Binding.GenerationID, link.ProviderID, keyA)
	unitB1, errB := f.store.SelectedUnit(f.ctx, res.Binding.GenerationID, link.ProviderID, keyB)
	if errA != nil || errB != nil {
		t.Fatalf("the refreshed generation selects no linking unit for %s (%v) or %s (%v)", keyA, errA, keyB, errB)
	}
	if unitA1 != unitA0 || unitB1 == unitB0 {
		t.Fatalf("the refresh must reuse module a's linking unit and rebuild module b's: a %s -> %s, b %s -> %s",
			unitA0, unitA1, unitB0, unitB1)
	}

	files := twoModules()
	files[edited] = after
	full := linkingFixture(t, files)
	want, err := full.c.Index(full.ctx, model.IndexRequest{})
	if err != nil {
		t.Fatalf("full index of the edited tree: %v", err)
	}
	wantSites := callSites(t, want.Completeness)
	if got := callSites(t, res.Completeness); fmt.Sprint(got) != fmt.Sprint(wantSites) {
		t.Fatalf("calls site totals %v after an incremental run, want the full build's %v", got, wantSites)
	}
}
