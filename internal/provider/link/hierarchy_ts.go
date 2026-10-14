package link

import (
	"maps"
	"slices"
	"strings"

	"github.com/Sawmonabo/codectx/internal/model"
	"github.com/Sawmonabo/codectx/internal/pagination"
)

// TypeScript relates types by their members, not their names (the
// TypeScript Language Specification 1.8 §3.11.4, Assignment Compatibility;
// §8.2.1, a class need not list an interface it implements): an object of a
// class that has every member an interface names may be passed wherever the
// interface is written, and a call through the interface then runs the
// class's method. A class of the unit structurally implements an interface
// of the unit that it neither declares nor inherits when:
//
//   - the interface declares a method itself (an interface of properties
//     alone dispatches no call, and one that adds nothing to those it
//     extends is reached through them);
//   - each member of the interface, property or method, is a member of the
//     class of that name, its own or one along its superclasses; a static
//     member, a constructor and a `#private` member are no instance member;
//   - for each method of the interface the class implements by a method,
//     the class's method requires no more arguments than the interface's
//     method declares parameters (the checker's compareSignaturesRelated: a
//     call through the interface supplies at most those).
//
// The class then implements the interface (FactImplements, DetailMethodSet)
// and each of its own methods of an interface method's name overrides that
// method (FactOverrides, DetailMethodSet), as a Go type does by its method
// set. Only the unit's declarations are read: a member a supertype outside
// the unit gives the class is not seen (the class does not match), and one
// it gives the interface is not required. A constructor's parameter
// properties, an index signature, an optional member (`m?()`) and an
// accessibility modifier are not read (an optional member is required, a
// `private` member counts), nor are the members' types beyond the
// methods' arity.

// addShapes adds the instance members of each class and interface of the
// ECMAScript table t, file, under keyShape, and each interface that declares
// a method under keyShapeFirst and the least name of the methods it
// declares, which any class implementing it has, with the names of all its
// own members, which a class lacking one of them is rejected by unread.
func addShapes(links *pagination.ExternalSort[entry], file int32, t *Table) error {
	first := map[int32]string{}
	names := map[int32][]string{}
	for i := range t.Decls {
		d := &t.Decls[i]
		p := d.Parent
		if p < 0 || int(p) >= len(t.Decls) || !shaped(t.Decls[p].Kind) || !instanceMember(d) {
			continue
		}
		e := declEntry(Ref{File: file, Index: int32(i)}, d)
		e.Key, e.Aux = keyShape+refKey(Ref{File: file, Index: p}), d.Name
		e.Sig = shapeField
		names[p] = append(names[p], d.Name)
		if callable(d) {
			e.Sig = shapeMethod
			if f, ok := first[p]; t.Decls[p].Kind == model.NodeInterface && (!ok || d.Name < f) {
				first[p] = d.Name
			}
		}
		if err := links.Add(e); err != nil {
			return err
		}
	}
	for p, name := range first {
		own := names[p]
		slices.Sort(own)
		e := entry{Key: keyShapeFirst + name, Ref: Ref{File: file, Index: p}, Sig: strings.Join(slices.Compact(own), "\x00")}
		if err := links.Add(e); err != nil {
			return err
		}
	}
	return nil
}

// The kinds of a keyShape member, in Sig.
const (
	shapeField  = "f"
	shapeMethod = "m"
)

func shaped(k model.NodeKind) bool { return k == model.NodeClass || k == model.NodeInterface }

// instanceMember reports a declaration of a class or interface body an
// object of the type has as a member.
func instanceMember(d *Decl) bool {
	if d.Static || d.Name == "" || d.Name == "constructor" || strings.HasPrefix(d.Name, "#") {
		return false
	}
	return d.Kind == model.NodeField || callable(d)
}

// shape is a type's members by name, its own and those along the supertypes
// a lookup followed, with the type declarations it reached.
type shape struct {
	members map[string][]entry
	reached []Ref
}

// shapeOf gathers typ's members along its supertypes of the unit: a class's
// along its superclasses, an interface's along every type it extends.
// reached holds every type visited, a class's declared interfaces and their
// supertypes included, which a structural relation leaves out: the class
// names them already.
func (h *hierarchy) shapeOf(typ Ref, class bool) (shape, error) {
	s := shape{members: map[string][]entry{}}
	type visit struct {
		ref     Ref
		members bool
	}
	queue := []visit{{typ, true}}
	for len(queue) > 0 {
		v := queue[0]
		queue = queue[1:]
		if slices.Contains(s.reached, v.ref) {
			continue
		}
		s.reached = append(s.reached, v.ref)
		if v.members {
			err := h.links.lookup(keyShape+refKey(v.ref), func(e entry) error {
				s.members[e.Aux] = append(s.members[e.Aux], e)
				return nil
			})
			if err != nil {
				return s, err
			}
		}
		err := h.links.lookup(keySuper+refKey(v.ref), func(e entry) error {
			// A class has no member of an interface it implements but its
			// own.
			queue = append(queue, visit{e.Ref, v.members && (!class || e.Flags&flagInterface == 0)})
			return nil
		})
		if err != nil {
			return s, err
		}
	}
	return s, nil
}

// structuralMemo holds, while one file's facts are emitted, the interfaces
// each class implements by its members (structural) and the shape of each
// interface read. Every read it saves was traced for that file already.
type structuralMemo struct {
	classes map[Ref][]Ref
	ifaces  map[Ref]shape
}

func newStructuralMemo() *structuralMemo {
	return &structuralMemo{classes: map[Ref][]Ref{}, ifaces: map[Ref]shape{}}
}

// structural yields each interface of the unit the class at ref implements
// by its members alone (the rule above), once.
func (h *hierarchy) structural(ref Ref, memo *structuralMemo, yield func(iface Ref) error) error {
	found, ok := memo.classes[ref]
	if !ok {
		var err error
		if found, err = h.conformed(ref, memo); err != nil {
			return err
		}
		memo.classes[ref] = found
	}
	for _, iface := range found {
		if err := yield(iface); err != nil {
			return err
		}
	}
	return nil
}

// conformed is the interfaces the class at ref implements by its members.
func (h *hierarchy) conformed(ref Ref, memo *structuralMemo) ([]Ref, error) {
	class, err := h.shapeOf(ref, true)
	if err != nil {
		return nil, err
	}
	var candidates []Ref
	seen := map[Ref]bool{}
	for _, r := range class.reached {
		seen[r] = true
	}
	for _, name := range slices.Sorted(maps.Keys(class.members)) {
		err := h.links.lookup(keyShapeFirst+name, func(e entry) error {
			if seen[e.Ref] {
				return nil
			}
			seen[e.Ref] = true
			for own := range strings.SplitSeq(e.Sig, "\x00") {
				if _, ok := class.members[own]; !ok {
					return nil
				}
			}
			candidates = append(candidates, e.Ref)
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	var found []Ref
	for _, iface := range candidates {
		s, ok := memo.ifaces[iface]
		if !ok {
			if s, err = h.shapeOf(iface, false); err != nil {
				return nil, err
			}
			memo.ifaces[iface] = s
		}
		if conforms(class, s) {
			found = append(found, iface)
		}
	}
	return found, nil
}

// conforms reports that every member of the interface shape iface is a
// member of the class shape class, a method no more demanding than the
// interface's.
func conforms(class, iface shape) bool {
	for name, want := range iface.members {
		have, ok := class.members[name]
		if !ok {
			return false
		}
		for _, w := range want {
			if w.Sig != shapeMethod || w.MaxArgs == 0 {
				continue
			}
			// A call through the interface passes fewer than w.MaxArgs
			// arguments; some member of the class must accept that many.
			if !slices.ContainsFunc(have, func(m entry) bool { return m.Sig != shapeMethod || m.MinArgs < w.MaxArgs }) {
				return false
			}
		}
	}
	return true
}

// structuralFacts emits the structural implements facts of the ECMAScript
// class at ref.
func (h *hierarchy) structuralFacts(ref Ref, memo *structuralMemo, emit func(Binding) error) error {
	return h.structural(ref, memo, func(iface Ref) error {
		return emit(Binding{Kind: FactImplements, Site: ref, Target: iface, Detail: DetailMethodSet})
	})
}

// structuralOverrides yields the methods of name the method of the
// ECMAScript class owner overrides by the interfaces owner structurally
// implements: each such interface's nearest declaration of the name.
func (h *hierarchy) structuralOverrides(owner Ref, name string, memo *structuralMemo, yield func(entry) error) error {
	return h.structural(owner, memo, func(iface Ref) error {
		return h.declared(iface, name, yield)
	})
}
