package link

import (
	"github.com/Sawmonabo/codectx/internal/model"
)

// hierarchy publishes declaration d's `extends`, `implements` and
// `overrides`, group, each to its target's node or its external node,
// located at the declaration.
func (f *filePublication) hierarchy(d *Decl, group []idBinding) error {
	for _, b := range group {
		var kind model.RelationKind
		switch b.Kind {
		case FactExtends:
			kind = model.RelExtends
		case FactImplements:
			kind = model.RelImplements
		case FactOverrides:
			kind = model.RelOverrides
		default:
			continue
		}
		target, err := f.target(b)
		if err != nil {
			return err
		}
		f.node(occurrenceOf(f.fv, f.t, d.Endpoint))
		f.relation(d.Node, kind, target, located(d.Range), "", b.Detail, "", 0)
	}
	return nil
}
