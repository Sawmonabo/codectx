package graph

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/Sawmonabo/codectx/internal/model"
)

// reflectiveCall is `void go(Handler obj) { m.invoke(obj); }` as the linker
// publishes it: the call of the reflection API, the reflective open site
// beside it, a method `invoke` of the project the site's callee spells, and
// `Handler.handle`, which only the reflective call reaches. The `calls` row
// counts the one reflective site.
func reflectiveCall() *callFixture {
	meta, _ := json.Marshal(model.OpenSiteMetadata{Resolution: model.ResolutionUnresolved,
		Reason: model.ReasonReflective, Shape: model.CallShapeMember, Arguments: 1})
	f := newCallFixture("reflective_call", fixtureScenario{
		Nodes: []fixtureDecl{
			{Name: "go", Kind: model.NodeMethod, File: "R.java", Bytes: [2]uint64{0, 60}, Language: "java"},
			{Key: "site", Name: "invoke", Kind: model.NodeMethod, File: "R.java", Bytes: [2]uint64{30, 40},
				Metadata: meta, Language: "java"},
			{Name: "invoke", Kind: model.NodeMethod, File: "Plugin.java", Bytes: [2]uint64{0, 30}, Language: "java"},
			{Name: "Handler.handle", Kind: model.NodeMethod, File: "Handler.java", Bytes: [2]uint64{0, 30},
				Language: "java"},
		},
		Relations: []fixtureFact{{From: "go", Kind: model.RelCalls, To: "site", Occurrences: site(30)}},
	})
	f.caps = []model.CapabilityState{{ProviderID: "linker", Capability: "calls", Scope: "repository",
		State: model.CapabilityFresh,
		Details: map[string]string{"sites": "1", "bound_sites": "0", "open_sites": "0", "computed_sites": "0",
			"external_sites": "1", "reflective_sites": "1"}}}
	return f
}

// TestAReflectiveSiteIsDisclosedAndNamesNoCandidate pins the reflective open
// site (model.ReasonReflective): a callers answer says the project has call
// sites whose target a reflection API chooses at run time, and the site's
// callee, the API's own name, makes no declaration of that name a possible
// callee.
//
// Mutations caught: dropping reflectiveNotice (callers(Handler.handle) says
// nothing of the reflective site); reading a reflective site as named
// (OpenSiteReason.Named), which lists Plugin.invoke as `may_call`.
func TestAReflectiveSiteIsDisclosedAndNamesNoCandidate(t *testing.T) {
	f := reflectiveCall()
	res := callAnswer(t, f, "Handler.handle", model.DirectionIncoming)
	if !slices.ContainsFunc(res.Meta.Notices, func(n string) bool { return strings.Contains(n, "reflection API") }) {
		t.Errorf("callers(Handler.handle) notices = %q, want the reflective call sites disclosed", res.Meta.Notices)
	}
	res = callAnswer(t, f, "go", model.DirectionOutgoing)
	for _, r := range res.Relations {
		if r.Kind == model.RelMayCall {
			t.Errorf("callees(go) lists %s as a possible callee of the reflective site; its callee names the API",
				f.name(r.To))
		}
	}
	res = callAnswer(t, f, "invoke", model.DirectionIncoming)
	if callersOf(f, res)["go"] {
		t.Errorf("callers(invoke) lists go through the reflective site named after the reflection API")
	}
}
