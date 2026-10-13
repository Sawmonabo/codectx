package cli

import (
	"fmt"
	"strings"

	"github.com/Sawmonabo/codectx/internal/model"
)

// writeContextRoutes renders one page of the routes view: each route as its
// entry ordinal, its index among the entry's routes and its relations in walk
// order.
func writeContextRoutes(b *strings.Builder, routes []model.ContextRoute) {
	if len(routes) == 0 {
		b.WriteString("routes      none on this page\n")
		return
	}
	fmt.Fprintf(b, "routes      %d on this page\n", len(routes))
	for _, r := range routes {
		rels := make([]string, len(r.Relations))
		for i, id := range r.Relations {
			rels[i] = string(id)
		}
		fmt.Fprintf(b, "  %5d  #%d  %s\n", r.Entry, r.Index, strings.Join(rels, " -> "))
	}
}

// writeContextReasons renders one page of the reasons view: each reason as its
// entry ordinal, its index among the entry's reasons and its text.
func writeContextReasons(b *strings.Builder, reasons []model.ContextReason) {
	if len(reasons) == 0 {
		b.WriteString("reasons     none on this page\n")
		return
	}
	fmt.Fprintf(b, "reasons     %d on this page\n", len(reasons))
	for _, r := range reasons {
		fmt.Fprintf(b, "  %5d  #%d  %s\n", r.Entry, r.Index, tableCell(r.Reason))
	}
}
