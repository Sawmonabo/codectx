package link

import (
	"slices"
)

// externalOverrides emits what the marked method d at ref, declared on
// owner, overrides outside the unit, by what its language's mark proves
// (wire.Export.Override); found reports that d already overrides a method
// of the unit.
//
//   - TypeScript: `override` proves a member of the one class the
//     `extends` chain names (TS4112, TS4113: an interface-only member is no
//     override), so the candidate is that chain's class outside the unit,
//     when no class of the unit along it declares the member.
//   - Python: `typing.override` proves the member of exactly one ancestor,
//     the first along the method resolution order that defines it (PEP 698).
//     When no class of the unit along the order defines it, that ancestor is
//     one of the bases outside the unit of the classes along the order (the
//     order places none of them, having no declaration of theirs), so each
//     is published, a set of n when there are several: one of them holds.
//   - Java and C++: `@Override` or `override` proves at least one overridden
//     method (JLS §9.6.4.4, [class.virtual]¶5), and a method overrides every
//     supertype's of its signature at once (JLS §8.4.8.1, [class.virtual]¶2).
//     The candidates are the supertypes outside the unit reached along paths
//     on which no type of the unit declares the method (externalCandidates):
//     with no override of the unit and one candidate, that one is proven;
//     otherwise each candidate is published as one (DetailSupertypeCandidate,
//     "at least one of n"), so a call on any of them reaches d.
//   - Rust: an impl item implements its impl's trait (rustTraitOverrides);
//     only an item whose trait path the worker could not spell
//     (Decl.Trait empty) falls back to the self type's single external
//     trait, when the type has exactly one.
//
// A method with no mark gets nothing: the mark is the only proof that a
// type outside the unit has the member.
func (h *hierarchy) externalOverrides(fc fileCtx, owner, ref Ref, d *Decl, found bool, emit func(Binding) error) error {
	if !d.Override {
		return nil
	}
	publish := func(candidates []entry, detail string) error {
		for _, e := range candidates {
			if err := emit(Binding{Kind: FactOverrides, Site: ref, Detail: detail,
				External: externalMethod(fc.family, e.Aux, d.Name)}); err != nil {
				return err
			}
		}
		return nil
	}
	switch fc.family {
	case "js":
		if found {
			return nil
		}
		classes, err := h.externalCandidates(owner, d.Name, func(e entry) bool { return e.Flags&flagInterface == 0 })
		if err != nil {
			return err
		}
		return publish(classes, DetailSupertype)
	case "python":
		if found {
			return nil
		}
		order, err := h.pythonMRO(owner)
		if err != nil {
			return err
		}
		if order == nil {
			order = []Ref{owner}
		}
		var bases []entry
		for _, t := range order {
			err := h.links.lookup(keyExternalSuper+refKey(t), func(e entry) error {
				if !slices.ContainsFunc(bases, func(b entry) bool { return b.Aux == e.Aux }) {
					bases = append(bases, e)
				}
				return nil
			})
			if err != nil {
				return err
			}
		}
		return publish(bases, DetailSupertype)
	case "java", "c":
		candidates, err := h.externalCandidates(owner, d.Name, nil)
		if err != nil {
			return err
		}
		if fc.family == "java" && !found && javaObjectMethods[d.Name] &&
			!slices.ContainsFunc(candidates, func(e entry) bool { return e.Flags&flagInterface == 0 }) {
			candidates = append(candidates, entry{Aux: "java.lang.Object"})
		}
		if len(candidates) == 1 && !found {
			return publish(candidates, DetailSupertype)
		}
		return publish(candidates, DetailSupertypeCandidate)
	}
	if found {
		return nil
	}
	external, err := h.externalSupers(fc.family, owner, d.Name)
	if err != nil || len(external) != 1 {
		return err
	}
	return publish(external, DetailSupertype)
}

// externalCandidates is every supertype outside the unit of typ that may
// declare method, each once, breadth first along typ's supertypes of the
// unit: a supertype of the unit that itself declares method is a target of
// its own, and the walk does not go past it, since what is above it is
// overridden through it. keep, when set, filters the external supertypes.
func (h *hierarchy) externalCandidates(typ Ref, method string, keep func(entry) bool) ([]entry, error) {
	var out []entry
	seen := map[Ref]bool{typ: true}
	queue := []Ref{typ}
	for len(queue) > 0 {
		t := queue[0]
		queue = queue[1:]
		err := h.links.lookup(keyExternalSuper+refKey(t), func(e entry) error {
			if (keep == nil || keep(e)) && !slices.ContainsFunc(out, func(o entry) bool { return o.Aux == e.Aux }) {
				out = append(out, e)
			}
			return nil
		})
		if err != nil {
			return nil, err
		}
		err = h.links.lookup(keySuper+refKey(t), func(e entry) error {
			if seen[e.Ref] {
				return nil
			}
			seen[e.Ref] = true
			declares := false
			if err := h.methods(e.Ref, method, func(entry) error { declares = true; return nil }); err != nil || declares {
				return err
			}
			queue = append(queue, e.Ref)
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	return out, nil
}

// externalOverrideSet is the set size of an external override published
// with detail, among the overrides of one declaration: the number of its
// external overrides of that detail when it is a set of candidates (two or
// more of DetailSupertype, every DetailSupertypeCandidate), 0 otherwise.
func externalOverrideSet(group []idBinding, b idBinding) int64 {
	if b.Kind != FactOverrides || b.External == "" ||
		b.Detail != DetailSupertype && b.Detail != DetailSupertypeCandidate {
		return 0
	}
	var n int64
	for _, o := range group {
		if o.Kind == FactOverrides && o.External != "" && o.Detail == b.Detail {
			n++
		}
	}
	if n < 2 {
		return 0
	}
	return n
}
