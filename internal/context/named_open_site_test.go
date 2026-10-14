package context

import (
	"encoding/json"
	"testing"

	"github.com/Sawmonabo/codectx/internal/model"
)

// TestNamedOpenSiteReadsAReflectiveSiteAConstantNames pins which open sites
// the compiler looks candidates up for: a site of a named reason, and a
// reflective site a constant names (model.OpenSiteMetadata.Named); never a
// reflective site named after the API, nor a computed one.
//
// Mutation: read the reason alone -> the named reflective site admits no
// candidates.
func TestNamedOpenSiteReadsAReflectiveSiteAConstantNames(t *testing.T) {
	for _, c := range []struct {
		meta model.OpenSiteMetadata
		want bool
	}{
		{model.OpenSiteMetadata{Reason: model.ReasonReceiverUntyped}, true},
		{model.OpenSiteMetadata{Reason: model.ReasonReflective, Named: true}, true},
		{model.OpenSiteMetadata{Reason: model.ReasonReflective}, false},
		{model.OpenSiteMetadata{Reason: model.ReasonComputed}, false},
	} {
		c.meta.Resolution, c.meta.Shape = model.ResolutionUnresolved, model.CallShapeMember
		raw, err := json.Marshal(c.meta)
		if err != nil {
			t.Fatal(err)
		}
		got, err := namedOpenSite(model.Node{ID: "n", Metadata: raw})
		if err != nil || got != c.want {
			t.Errorf("namedOpenSite(%+v) = %v, %v; want %v", c.meta, got, err, c.want)
		}
	}
}
