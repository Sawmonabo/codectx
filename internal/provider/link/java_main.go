package link

import (
	"github.com/Sawmonabo/codectx/internal/model"
	"github.com/Sawmonabo/codectx/internal/provider/treesitter/lang"
)

// A Java class is launched by the `main` it declares or inherits (JLS SE 25
// §12.1.4): the one with a `String[]` parameter before the one with none, a
// declaration overriding an inherited `main` of the same parameters. The
// structural provider reads a class and the superclasses its own file
// declares; a class whose superclass chain leaves its file, with no
// `String[]` main declared nearer, it leaves to the linker
// (Decl.MainInherited), which follows the chain through the unit's other
// files.

// javaMainFacts emits a FactMain from the class at ref to each method JLS
// §12.1.4 launches it by: along its superclass chain, the nearest `String[]`
// main, else the nearest main with no parameter. A superclass the class's
// file names ambiguously is a path of its own, and each path's choice is a
// candidate. A chain that reaches a superclass outside the unit ends there,
// with what the unit declares.
func (h *hierarchy) javaMainFacts(ref Ref, emit func(Binding) error) error {
	seen := map[Ref]bool{}
	var found []Ref
	err := h.javaMainAlong(ref, nil, map[Ref]bool{}, func(m Ref) error {
		if !seen[m] {
			seen[m] = true
			found = append(found, m)
		}
		return nil
	})
	if err != nil {
		return err
	}
	for _, m := range found {
		if err := emit(Binding{Kind: FactMain, Site: ref, Target: m, Detail: DetailHierarchy}); err != nil {
			return err
		}
	}
	return nil
}

// javaMainAlong yields the main a class launches by along the superclass
// chain from typ, given the nearest no-parameter main below it (noArgs, nil
// for none); onPath holds the classes of the chain so far, so a cyclic
// chain ends.
func (h *hierarchy) javaMainAlong(typ Ref, noArgs *Ref, onPath map[Ref]bool, yield func(Ref) error) error {
	onPath[typ] = true
	defer delete(onPath, typ)
	var args []Ref
	var none *Ref
	err := h.links.lookup(keyJavaMain+refKey(typ), func(e entry) error {
		if e.Flags == MainArgs {
			args = append(args, e.Ref)
		} else if none == nil {
			r := e.Ref
			none = &r
		}
		return nil
	})
	if err != nil {
		return err
	}
	if len(args) > 0 {
		for _, m := range args {
			if err := yield(m); err != nil {
				return err
			}
		}
		return nil
	}
	if noArgs == nil {
		noArgs = none
	}
	var supers []Ref
	err = h.links.lookup(keySuper+refKey(typ), func(e entry) error {
		if e.Flags&flagInterface == 0 && !onPath[e.Ref] {
			supers = append(supers, e.Ref)
		}
		return nil
	})
	if err != nil {
		return err
	}
	if len(supers) == 0 {
		if noArgs != nil {
			return yield(*noArgs)
		}
		return nil
	}
	for _, s := range supers {
		if err := h.javaMainAlong(s, noArgs, onPath, yield); err != nil {
			return err
		}
	}
	return nil
}

// javaMain publishes the main endpoint of the Java class d from its
// FactMains, group: one `endpoint` named `main` in the file's scope, keyed
// by the class's qualified name as the structural provider keys the main of
// a class it resolves itself, located at the class, defined by the file's
// module and routed to each method, with the set size when several
// candidates hold.
func (f *filePublication) javaMain(d *Decl, group []idBinding) error {
	var set int64
	for _, b := range group {
		if b.Kind == FactMain {
			set++
		}
	}
	if set == 0 {
		return nil
	}
	if set == 1 {
		set = 0
	}
	n, err := f.mint(model.NodeCandidate{ProviderID: ProviderID, ScopeKey: lang.ScopePrefix + f.fv.Path,
		NativeKey: fitKey("endpoint:main#" + d.Qualified), Kind: model.NodeEndpoint,
		Language: f.t.Language, Name: "main"}, d.Range, []byte("{}"))
	if err != nil {
		return err
	}
	f.node(occurrenceOf(f.fv, f.t, f.t.Module))
	f.relation(f.t.Module.Node, model.RelDefines, n, located(d.Range), "", "", "", 0)
	for _, b := range group {
		if b.Kind == FactMain {
			f.relation(n, model.RelRoutesTo, b.Target, located(d.Range), "", b.Detail, "", set)
		}
	}
	return nil
}
