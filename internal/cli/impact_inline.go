package cli

import (
	"strings"

	"github.com/Sawmonabo/codectx/internal/model"
)

// writeInlineImpact renders the reasons and routes an impact answer served
// inline beside its entries: an answer one page of each view holds arrives
// whole in one response, with no answer token to read those views by later.
func writeInlineImpact(b *strings.Builder, result model.ImpactResult) {
	if len(result.Reasons) > 0 {
		writeImpactReasons(b, result.Reasons)
	}
	if len(result.Routes) > 0 {
		writeImpactRoutes(b, result.Routes)
	}
}

// reachLabel names an impact entry's tier in the entries table: nothing for
// the call tier, which ranks first, and the tier otherwise.
func reachLabel(r model.ImpactReach) string {
	if r == "" {
		return ""
	}
	return "  reach " + string(r)
}
