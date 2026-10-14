package link

import "github.com/Sawmonabo/codectx/internal/provider/treesitter/wire"

// javaTaken yields the join-key prefix of each type a Java method reference
// of target type target is an object of (JLS §15.13.3: it evaluates to an
// instance of a class implementing its target type, so of that type and
// every supertype of it): javaTargetKey of the target and of each of its
// supertypes of the unit, each once. A target type outside the unit yields
// nothing: a call can join a reference only through an abstract method of
// the unit, which no supertype of a type outside it declares. A reference
// whose target type the worker could not read yields the empty prefix, the
// signature alone, which every call of an abstract method of that signature
// joins.
func (r *staticFile) javaTaken(target wire.Type, yield func(key string) error) error {
	if target.Kind != wire.TypeNamed {
		return yield("")
	}
	seen := map[Ref]bool{}
	var queue []Ref
	if err := r.h.typesIn(r.fc, target.Name, func(typ entry) error {
		if !seen[typ.Ref] {
			seen[typ.Ref] = true
			queue = append(queue, typ.Ref)
		}
		return nil
	}); err != nil {
		return err
	}
	for len(queue) > 0 {
		t := queue[0]
		queue = queue[1:]
		key, err := r.javaTargetKey(t)
		if err != nil {
			return err
		}
		if err := yield(key); err != nil {
			return err
		}
		if err := r.h.links.lookup(keySuper+refKey(t), func(e entry) error {
			if !seen[e.Ref] {
				seen[e.Ref] = true
				queue = append(queue, e.Ref)
			}
			return nil
		}); err != nil {
			return err
		}
	}
	return nil
}
