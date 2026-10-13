package link

import (
	"context"

	"github.com/Sawmonabo/codectx/internal/model"
	"github.com/Sawmonabo/codectx/internal/provider/treesitter/wire"
)

// resolveECMAScript resolves every site of a JavaScript, TypeScript or TSX
// unit: direct binding through imports and scopes, flow-based type inference
// (inferTypes) and field-based resolution, over the unit's own hierarchy and
// module index (emitECMAScript).
func resolveECMAScript(ctx context.Context, u *unit, emit func(Binding) error) error {
	h, err := buildHierarchy(ctx, u, nil)
	if err != nil {
		return err
	}
	defer h.close()
	mods, err := buildModules(ctx, u, h)
	if err != nil {
		return err
	}
	defer mods.close()
	t, err := inferTypes(ctx, u, h, nil, nil, nil)
	if err != nil {
		return err
	}
	defer t.close()
	return emitECMAScript(ctx, u, &flowEmit{h: h, mods: mods, t: t, emit: emit})
}

// emitECMAScript emits the facts of the nodes e.only selects (every node
// when nil), whose sites the inference e.t bound as part of its fixed
// point, since a site's targets decide what flows into their parameters:
// one pass over the selected members' tables emits, per site, what the
// fixed point bound with each bound site's argument flows and the external
// facts of the sites that reach outside the project, and per file node the
// hierarchy facts of its declarations (hierarchy.hierarchyFacts) and what
// its imports resolve to. An import resolves against the module index (every
// member file under each module path an import may name it by,
// buildModules), one point lookup per import, without reading another
// member's table.
func emitECMAScript(ctx context.Context, u *unit, e *flowEmit) error {
	h, emit := e.h, e.emit
	e.start()
	defer e.stop()
	return u.filesOf(ctx, e.only, func(file int32, _ model.FileVersion, tab *Table, sel selection) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if sel.has(fileNode) {
			e.at(Ref{File: file, Index: fileNode})
			if err := h.hierarchyFacts(ctx, file, tab, emit); err != nil {
				return err
			}
			if err := ecmascriptImports(h, e.mods, file, tab, emit); err != nil {
				return err
			}
		}
		fc := contextOf(file, tab)
		for i := range tab.Sites {
			if !sel.has(int32(i)) {
				continue
			}
			site := Ref{File: file, Index: int32(i)}
			e.site(site, tab.Path)
			if err := resolveECMAScriptSite(h, e.t, fc, tab, site, emit); err != nil {
				return err
			}
		}
		return nil
	})
}

// resolveECMAScriptSite emits the facts of one site of tab, the table of the
// file of fc: each binding the inference found, with its argument flows;
// for a member site, the external method of each receiver type outside the
// project, and for a member or static site that of each receiver class of
// the unit that reaches the method only through a supertype outside it
// (externalMember); and, for a site
// nothing bound, a FactExternalSite when it calls through a package's module
// or constructs a class outside the project. A computed site binds nothing.
func resolveECMAScriptSite(h *hierarchy, t *types, fc fileCtx, tab *Table, site Ref, emit func(Binding) error) error {
	s := &tab.Sites[site.Index]
	if s.Computed {
		return nil
	}
	bound := false
	err := t.callsAt(site, func(b binding) error {
		bound = true
		if err := emit(b.call()); err != nil {
			return err
		}
		if !b.Flow {
			return nil
		}
		return emitFlows(h, site, s, b.Target, b.Shift, emit)
	})
	if err != nil {
		return err
	}
	members := typeMembers(tab, s.Receiver)
	external := false
	if s.Shape == wire.ShapeMember {
		if external, err = externalReceiver(h, fc, site, s, members, emit); err != nil {
			return err
		}
		err = t.seek(site, func(r recv) error {
			if r.Role != roleReceiver {
				return nil
			}
			switch r.X.Kind {
			case valInstance, valObject, valExact, valClass:
				ok, err := t.externalMember(h, fc.family, site, r.X.Ref, s.Callee, emit)
				external = external || ok
				return err
			}
			return nil
		})
		if err != nil {
			return err
		}
	}
	if s.Shape == wire.ShapeStatic && s.Qualifier != "" {
		// `C.m()` on a class of the unit, which the scan bound through the
		// class itself.
		err = h.types(site.File, s.Qualifier, func(typ Ref) error {
			ok, err := t.externalMember(h, fc.family, site, typ, s.Callee, emit)
			external = external || ok
			return err
		})
		if err != nil {
			return err
		}
	}
	if bound || external {
		return nil
	}
	for _, m := range members {
		if m.Kind != wire.TypeModule {
			continue
		}
		spec := m.Name
		if sp, _, ok := importedName(m.Name, false); ok {
			spec = sp
		}
		outside, err := h.esOutside(fc.path, spec)
		if err != nil {
			return err
		}
		if outside {
			// A package outside the repository: no member file is it.
			return emit(Binding{Kind: FactExternalSite, Site: site, Detail: DetailDirect})
		}
	}
	if s.Shape == wire.ShapeConstructor {
		_, ok, err := h.externalName(fc, s.Callee)
		if err != nil || !ok {
			return err
		}
		return emit(Binding{Kind: FactExternalSite, Site: site, Detail: DetailDirect})
	}
	return nil
}

// ecmascriptImports emits what each import of the member at ordinal file,
// whose table is tab, resolves to (keyESSpec, under the import's own form):
// one FactImport per member file its specifier resolves to, found in the
// module index mods, or a FactOpenImport when it names a module of the
// repository that no member is. A package outside the repository resolves
// to nothing.
func ecmascriptImports(h *hierarchy, mods *index, file int32, tab *Table, emit func(Binding) error) error {
	for i, imp := range tab.Imports {
		site := Ref{File: file, Index: int32(i)}
		form := esForm(imp.Syntax)
		var targets []string
		answered, open := false, false
		err := h.decls.lookup(keyESSpec+tab.Path+"\x00"+imp.Path, func(e entry) error {
			if e.Flags&form == 0 {
				return nil
			}
			answered = true
			if e.Aux != "" {
				targets = append(targets, e.Aux)
			}
			open = open || e.Flags&esUnresolved != 0
			return nil
		})
		if err != nil {
			return err
		}
		if !answered {
			if base, ok := moduleBase(tab.Path, imp.Path, false); ok {
				targets = append(targets, base)
			}
		}
		if open && len(targets) == 0 {
			if err := emit(Binding{Kind: FactOpenImport, Site: site, Detail: DetailImport}); err != nil {
				return err
			}
			continue
		}
		for _, base := range targets {
			err := mods.lookup(keyESModule+base, func(m entry) error {
				return emit(Binding{Kind: FactImport, Site: site, Target: Ref{File: m.Ref.File, Index: -1}, Detail: DetailImport})
			})
			if err != nil {
				return err
			}
		}
	}
	return nil
}
