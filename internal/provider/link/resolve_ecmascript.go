package link

import (
	"context"

	"github.com/Sawmonabo/codectx/internal/model"
	"github.com/Sawmonabo/codectx/internal/pagination"
	"github.com/Sawmonabo/codectx/internal/provider/treesitter/wire"
)

// resolveECMAScript resolves every site of a JavaScript, TypeScript or TSX
// unit: direct binding through imports and scopes, flow-based type inference
// (inferTypes) and field-based resolution, over the unit's own hierarchy
// (emitECMAScript).
func resolveECMAScript(ctx context.Context, u *unit, emit func(Binding) error) error {
	h, err := buildHierarchy(ctx, u)
	if err != nil {
		return err
	}
	defer h.close()
	t, err := inferTypes(ctx, u, h, nil, nil)
	if err != nil {
		return err
	}
	defer t.close()
	return emitECMAScript(ctx, u, h, t, nil, emit)
}

// emitECMAScript emits the facts of the members only names (every member
// when nil), whose sites the inference t bound as part of its fixed point,
// since a site's targets decide what flows into their parameters: one pass
// over their tables emits, per member, what the fixed point bound with each
// bound site's argument flows, the external facts of the sites that reach
// outside the project, the hierarchy facts of its declarations
// (hierarchy.hierarchyFacts), and what its imports resolve to. An import
// resolves against every member's module paths, which the hierarchy's file
// contexts give without reading another member's table.
func emitECMAScript(ctx context.Context, u *unit, h *hierarchy, t *types, only *pagination.SortedRun[int32],
	emit func(Binding) error) error {
	modules, err := newEntrySort(u.dir)
	if err != nil {
		return err
	}
	defer modules.Close()
	var member int32 = -1
	err = u.members(func(model.FileVersion) error {
		member++
		fc, found, err := h.fileContext(member)
		if err != nil || !found {
			return err
		}
		return addECMAScriptModule(member, fc.path, modules)
	})
	if err != nil {
		return err
	}
	err = u.filesOf(ctx, only, func(file int32, _ model.FileVersion, tab *Table) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := h.hierarchyFacts(ctx, file, tab, emit); err != nil {
			return err
		}
		if err := addECMAScriptImports(file, tab, modules); err != nil {
			return err
		}
		fc := contextOf(file, tab)
		for i := range tab.Sites {
			if err := resolveECMAScriptSite(ctx, h, t, fc, tab, Ref{File: file, Index: int32(i)}, emit); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	return joinECMAScriptImports(ctx, modules, emit)
}

// resolveECMAScriptSite emits the facts of one site of tab, the table of the
// file of fc: each binding the inference found, with its argument flows;
// for a member site, the external method of each receiver type outside the
// project, and for a member or static site that of each receiver class of
// the unit that reaches the method only through a supertype outside it
// (externalMember); and, for a site
// nothing bound, a FactExternalSite when it calls through a package's module
// or constructs a class outside the project. A computed site binds nothing.
func resolveECMAScriptSite(ctx context.Context, h *hierarchy, t *types, fc fileCtx, tab *Table, site Ref, emit func(Binding) error) error {
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
		if external, err = externalReceiver(ctx, h, fc, site, s, members, emit); err != nil {
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
		err = h.types(ctx, site.File, s.Qualifier, func(typ Ref) error {
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
		if _, ok := moduleBase(fc.path, spec, false); !ok {
			// A package's module: no member file is it.
			return emit(Binding{Kind: FactExternalSite, Site: site, Detail: DetailDirect})
		}
	}
	if s.Shape == wire.ShapeConstructor {
		_, ok, err := h.externalName(ctx, fc, s.Callee)
		if err != nil || !ok {
			return err
		}
		return emit(Binding{Kind: FactExternalSite, Site: site, Detail: DetailDirect})
	}
	return nil
}

// addECMAScriptModule adds the module records of the member at ordinal
// file, whose path is p, to modules: the file under each module path an
// import may name it by (moduleBases, Tag 0).
func addECMAScriptModule(file int32, p string, modules *pagination.ExternalSort[entry]) error {
	for _, base := range moduleBases(p, false) {
		if err := modules.Add(entry{Key: base, Ref: Ref{File: file, Index: -1}}); err != nil {
			return err
		}
	}
	return nil
}

// addECMAScriptImports adds the import records of the member at ordinal
// file, whose table is tab, to modules: each of its imports whose specifier
// a member file can be under the module path it names (moduleBase, Tag 1,
// Ref.Index the import's index in tab.Imports). A package specifier names
// no member file and adds nothing.
func addECMAScriptImports(file int32, tab *Table, modules *pagination.ExternalSort[entry]) error {
	for i, imp := range tab.Imports {
		base, ok := moduleBase(tab.Path, imp.Path, false)
		if !ok {
			continue
		}
		if err := modules.Add(entry{Key: base, Tag: 1, Ref: Ref{File: file, Index: int32(i)}}); err != nil {
			return err
		}
	}
	return nil
}

// joinECMAScriptImports merges the module records: under each module path,
// the member files it names precede the imports that name it, and each
// import is one FactImport per such file. Only one module path's files are
// held.
func joinECMAScriptImports(ctx context.Context, modules *pagination.ExternalSort[entry], emit func(Binding) error) error {
	run, err := modules.Sorted()
	if err != nil {
		return err
	}
	defer run.Close()
	key, have := "", false
	var files []int32
	return run.Each(func(e entry) error {
		if !have || e.Key != key {
			if err := ctx.Err(); err != nil {
				return err
			}
			key, have, files = e.Key, true, files[:0]
		}
		if e.Tag == 0 {
			files = append(files, e.Ref.File)
			return nil
		}
		for _, f := range files {
			if err := emit(Binding{Kind: FactImport, Site: e.Ref, Target: Ref{File: f, Index: -1}, Detail: DetailImport}); err != nil {
				return err
			}
		}
		return nil
	})
}
