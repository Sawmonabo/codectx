package link

import (
	"context"
	"slices"
	"strings"

	"github.com/Sawmonabo/codectx/internal/model"
	"github.com/Sawmonabo/codectx/internal/pagination"
)

// goConversions records the interfaces outside the unit that t's
// conversions prove its types implement (keyGoExternalImpl): a value of a
// type of the unit (or a pointer to one) assigned to a variable of a named
// type outside it compiles only if that type is an interface the value's
// type implements ("Assignability": a value of a defined type is assignable
// to a different named type only when that type is an interface it
// implements, and a pointer type literal only to an interface or to a
// defined type of that very pointer type, which a package outside the unit
// cannot declare without importing it). The proof needs no source of the
// interface, so it holds when the build reads none (goExternal), and it
// names no method, so it proves no `overrides`. A declared type the build
// read as no interface, or the empty interface, proves nothing.
func (h *hierarchy) goConversions(ctx context.Context, links *pagination.ExternalSort[entry], fc fileCtx, t *Table) error {
	names := goUnitNames{h: h, fc: fc}
	for _, c := range t.Conversions {
		form := "." + c.Interface
		if q, name, ok := strings.Cut(c.Interface, "."); ok {
			form = ":" + q + "." + name
		}
		q, err := goIdentity(ctx, h.goTypes(), names, form)
		if err != nil {
			return err
		}
		pkg, name, qualified := cutLast(q, ".")
		if !qualified && q != "error" || strings.Contains(q, " ") || q == goIdentityUnknown {
			continue
		}
		if qualified {
			in, err := h.inUnit(pkg)
			if err != nil {
				return err
			}
			if in || h.goExt.knownNonInterface(pkg, name) {
				continue
			}
		}
		err = h.typesIn(fc, c.Type, func(sub entry) error {
			if sub.Flags&flagInterface != 0 {
				return nil
			}
			return links.Add(entry{Key: keyGoExternalImpl + refKey(sub.Ref), Ref: sub.Ref, Aux: q, Sig: DetailSupertypeInterface})
		})
		if err != nil {
			return err
		}
	}
	return nil
}

// knownNonInterface reports a type the build read as declared by package p
// under name and not an interface.
func (x *goExternal) knownNonInterface(p, name string) bool {
	if x == nil {
		return false
	}
	pkg := x.pkgs[p]
	if pkg == nil {
		return false
	}
	t := pkg.types[name]
	return t != nil && t.file >= 0 && !t.alias && !t.iface
}

// goExternalImplements emits the implements of the Go type at ref toward
// interfaces outside the unit (keyGoExternalImpl), each interface once, by
// its method set when both a method set and a conversion prove it.
func (h *hierarchy) goExternalImplements(fc fileCtx, ref Ref, emit func(Binding) error) error {
	detail := map[string]string{}
	var order []string
	err := h.links.lookup(keyGoExternalImpl+refKey(ref), func(e entry) error {
		if _, seen := detail[e.Aux]; !seen {
			order = append(order, e.Aux)
		}
		if detail[e.Aux] != DetailMethodSet {
			detail[e.Aux] = e.Sig
		}
		return nil
	})
	if err != nil {
		return err
	}
	for _, q := range order {
		if err := emit(Binding{Kind: FactImplements, Site: ref, Detail: detail[q], External: externalType(fc.family, q)}); err != nil {
			return err
		}
	}
	return nil
}

// goExternalOverrides emits the overrides of the Go method d at ref,
// declared on owner, toward the methods of interfaces outside the unit
// owner implements by its method set (keyGoExternalOverride), own the
// method's recorded keys (goOwnKeys).
func (h *hierarchy) goExternalOverrides(fc fileCtx, owner, ref Ref, d *Decl, own string, emit func(Binding) error) error {
	// An interface declared per platform may be satisfied by several of
	// its declarations: one override each.
	var seen []string
	return h.links.lookup(keyGoExternalOverride+refKey(owner)+d.Name, func(e entry) error {
		if slices.Contains(seen, e.Aux) || !goKeysMeet(own, goMethodKeysAux([]string{e.Sig})) {
			return nil
		}
		seen = append(seen, e.Aux)
		return emit(Binding{Kind: FactOverrides, Site: ref, Detail: DetailMethodSet,
			External: externalMethod(fc.family, e.Aux, d.Name)})
	})
}

// goExternalCoverage adds to the implements and overrides rows of a Go unit
// how many packages outside the unit the build read and how many had no
// readable source, and how many of the unit's methods have a signature
// naming a type by its unresolved spelling (matched only by the same
// spelling) or one that could not be spelled (matched by no method). The
// rows keep the state the unit's facts give them: the relations toward a
// package with no readable source are what conversions prove, and the
// counts say how many packages and methods that holds for.
func goExternalCoverage(rows []model.CapabilityState, h *hierarchy) ([]model.CapabilityState, error) {
	if h == nil || h.links == nil {
		return rows, nil
	}
	var read, unavailable, unresolved, unknown uint32
	found := false
	err := h.links.lookup(keyGoCoverage, func(e entry) error {
		if e.Tag == 1 {
			unresolved, unknown = e.MinArgs, e.MaxArgs
			return nil
		}
		read, unavailable, found = e.MinArgs, e.MaxArgs, true
		return nil
	})
	if err != nil || !found {
		return rows, err
	}
	for i := range rows {
		switch model.RelationKind(rows[i].Capability) {
		case model.RelImplements, model.RelOverrides:
			rows[i] = rows[i].WithDetail("go_external_packages", itoaInt(int(read))).
				WithDetail("go_external_packages_unavailable", itoaInt(int(unavailable))).
				WithDetail("go_method_signatures_unresolved", itoaInt(int(unresolved))).
				WithDetail("go_method_signatures_unknown", itoaInt(int(unknown)))
		}
	}
	return rows, nil
}
