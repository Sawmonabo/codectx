package link

import (
	"context"
	"slices"

	"github.com/Sawmonabo/codectx/internal/model"
	"github.com/Sawmonabo/codectx/internal/provider/treesitter/wire"
)

// externalMember emits the external fact of member site, whose receiver
// holds an instance or the class object of typ, a type of the unit, when typ
// neither declares nor inherits method in the project (outsideCall). It
// reports whether it emitted.
func (t *types) externalMember(h *hierarchy, family string, site, typ Ref, method string, emit func(Binding) error) (bool, error) {
	declares := false
	if err := h.declared(typ, method, func(entry) error { declares = true; return nil }); err != nil || declares {
		return false, err
	}
	return t.outsideCall(h, family, site, typ, method, emit)
}

// outsideCall emits the external fact of a call at site of method, which
// the project's part of typ's supertype closure does not declare: with
// exactly one supertype outside the project in the closure, a virtual
// FactCall to that type's method of the name (externalMethod); with
// several, a FactExternalSite, since which of them declares it is unknown.
// A type with no supertype outside the project emits nothing. It reports
// whether it emitted.
func (t *types) outsideCall(h *hierarchy, family string, site, typ Ref, method string, emit func(Binding) error) (bool, error) {
	outside, err := outsideSupertypes(h, typ)
	if err != nil {
		return false, err
	}
	switch len(outside) {
	case 0:
		return false, nil
	case 1:
		return true, emit(Binding{Kind: FactCall, Site: site, Detail: DetailHierarchy, Dispatch: model.DispatchVirtual,
			External: externalMethod(family, outside[0], method)})
	}
	return true, emit(Binding{Kind: FactExternalSite, Site: site, Detail: DetailHierarchy})
}

// outsideSupertypes is the qualified names of the supertypes outside the
// project of typ and of each of its supertypes of the unit, each once
// (hierarchy.externalSupertypes).
func outsideSupertypes(h *hierarchy, typ Ref) ([]string, error) {
	var outside []string
	err := h.externalSupertypes(typ, true, func(e entry) error {
		if !slices.Contains(outside, e.Aux) {
			outside = append(outside, e.Aux)
		}
		return nil
	})
	return outside, err
}

// externalReceiver emits the virtual FactCall of member site s, at site in
// the file of fc, to the external method of each type its receiver names
// that is outside the project (hierarchy.externalName). It reports whether
// it emitted.
func externalReceiver(ctx context.Context, h *hierarchy, fc fileCtx, site Ref, s *Site, members []wire.Type, emit func(Binding) error) (bool, error) {
	emitted := false
	for _, m := range members {
		if m.Kind != wire.TypeNamed {
			continue
		}
		q, ok, err := h.externalName(ctx, fc, m.Name)
		if err != nil {
			return emitted, err
		}
		if !ok {
			continue
		}
		emitted = true
		err = emit(Binding{Kind: FactCall, Site: site, Detail: DetailDirect, Dispatch: model.DispatchVirtual,
			External: externalMethod(fc.family, q, s.Callee)})
		if err != nil {
			return emitted, err
		}
	}
	return emitted, nil
}
