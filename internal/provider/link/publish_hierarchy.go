package link

import (
	"github.com/Sawmonabo/codectx/internal/model"
)

// hierarchy publishes declaration d's `extends`, `implements` and
// `overrides`, group, each to its target's node or its external node,
// located at the declaration, and its visibility facts, each a
// `references` to the macro definition, its detail the visibility and, when
// the declaration's macros reach several definitions, its set size their
// number.
func (f *filePublication) hierarchy(d *Decl, group []idBinding) error {
	definitions := int64(0)
	for _, b := range group {
		if b.Kind == FactVisibility {
			definitions++
		}
	}
	if definitions < 2 {
		definitions = 0
	}
	for _, b := range group {
		var kind model.RelationKind
		set := int64(0)
		switch b.Kind {
		case FactVisibility:
			kind, set = model.RelReferences, definitions
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
		f.relation(d.Node, kind, target, located(d.Range), "", b.Detail, "", set)
	}
	return nil
}
