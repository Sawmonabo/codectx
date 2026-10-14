package link

import (
	"github.com/Sawmonabo/codectx/internal/model"
	"github.com/Sawmonabo/codectx/internal/pagination"
	"github.com/Sawmonabo/codectx/internal/provider/treesitter/wire"
)

// A Rust unit struct or unit enum variant names, in the value namespace, a
// value of its type, and a tuple struct or tuple variant names a
// constructor function whose call returns one (the Rust Reference's
// "Struct expressions", tuple struct and unit-like struct expressions, and
// "Enumerations"): `let v = S;`, `S(1)`, `E::A` and `E::B(1)` are each a
// value of exactly S or E, and a call of `S(..)` or `E::B(..)` calls the
// struct or variant itself. A tuple struct's path not called (`let f =
// S;`) is the constructor, a function, never a value of S. A struct
// expression of a variant (`E::C { .. }`) is a value of E. The worker
// marks each struct and variant with its shape (Decl.Constructor), and the
// linker reads the path through the unit's types: the path itself names a
// struct, or its last segment names a variant of the enum the rest names.

// keyRustConstructor + refKey(struct or variant): its Decl.Constructor in
// Sig. keyRustVariant + refKey(enum) + name: the enum's variant of that
// name, its Decl.Constructor in Sig.
const (
	keyRustConstructor = "rk\x00"
	keyRustVariant     = "rv\x00"
)

// Constructor shapes (Decl.Constructor).
const (
	rustUnit  = "unit"
	rustTuple = "tuple"
)

// addRustConstructor adds the struct or enum variant d at ref of the Rust
// file t to the declarations index; any other declaration adds nothing.
func addRustConstructor(s *pagination.ExternalSort[entry], fc fileCtx, ref Ref, t *Table, d *Decl) error {
	if fc.family != "rust" {
		return nil
	}
	if p := d.Parent; p >= 0 && int(p) < len(t.Decls) && t.Decls[p].Kind == model.NodeEnum {
		enum := Ref{File: ref.File, Index: p}
		return s.Add(entry{Key: keyRustVariant + refKey(enum) + d.Name, Ref: ref, Sig: d.Constructor})
	}
	if d.Constructor == "" {
		return nil
	}
	return s.Add(entry{Key: keyRustConstructor + refKey(ref), Ref: ref, Sig: d.Constructor})
}

// rustValue yields, for the Rust value path written in caller (a callee,
// or what a function-valued local holds), each type of the unit whose
// value it denotes and the struct or variant declaration that makes it,
// when that declaration's shape is one accept takes: a struct the path
// names, or a variant its last segment names of an enum the rest names.
// `Self` is the caller's impl type, as the qualifier `Self::` is.
func (r *staticFile) rustValue(caller int32, written string, accept func(shape string, variant bool) bool, yield func(typ, made entry) error) error {
	structs := func(typ entry) error {
		return r.h.decls.lookup(keyRustConstructor+refKey(typ.Ref), func(e entry) error {
			if !accept(e.Sig, false) {
				return nil
			}
			return yield(typ, e)
		})
	}
	variants := func(enum entry, name string) error {
		return r.h.decls.lookup(keyRustVariant+refKey(enum.Ref)+name, func(e entry) error {
			if !accept(e.Sig, true) {
				return nil
			}
			return yield(enum, e)
		})
	}
	q, name, qualified := cutLast(written, "::")
	switch {
	case written == "Self":
		return r.innermostTypes(caller, func(typ Ref) error { return structs(entry{Ref: typ}) })
	case q == "Self":
		return r.innermostTypes(caller, func(typ Ref) error { return variants(entry{Ref: typ}, name) })
	}
	if err := r.h.typesIn(r.fc, written, structs); err != nil || !qualified {
		return err
	}
	return r.h.typesIn(r.fc, q, func(enum entry) error { return variants(enum, name) })
}

// rustCalled accepts the constructor a call calls: a tuple struct's or
// tuple variant's.
func rustCalled(shape string, _ bool) bool { return shape == rustTuple }

// rustUnitValue accepts the unit struct or variant a path names as a
// value.
func rustUnitValue(shape string, _ bool) bool { return shape == rustUnit }

// rustVariant accepts a variant of any shape: a struct expression of a
// variant path (`E::C { .. }`) builds each, while a struct's path is a type
// the hierarchy reads itself.
func rustVariant(_ string, variant bool) bool { return variant }

// rustConstruct reports to out the call of written in caller when it calls
// a tuple struct or tuple variant: that declaration, exactly. found
// reports one.
func (r *staticFile) rustConstruct(caller int32, written string, out siteOut) (found bool, err error) {
	err = r.rustValue(caller, written, rustCalled, func(_, made entry) error {
		found = true
		return out.call(made, DetailDirect, model.DispatchExact)
	})
	return found, err
}

// rustMember reports to out the call of method on a Rust receiver of the
// type the value path written in caller makes, accepted by accept, the
// receiver exactly that type. found reports one.
func (r *staticFile) rustMember(caller int32, written string, accept func(string, bool) bool, method string, out siteOut) (found bool, err error) {
	err = r.rustValue(caller, written, accept, func(typ, _ entry) error {
		found = true
		return r.onType(typ, method, true, out)
	})
	return found, err
}

// rustReceiver reports to out the call of method on a Rust receiver that a
// struct or variant makes, exactly its type: a unit struct's or unit
// variant's path held as a value (`let v = S;`), the result of a tuple
// constructor's call (`S(1)`, `E::B(1)`), or a struct expression of a
// variant (`E::C { .. }`). found is false for any other receiver.
func (r *staticFile) rustReceiver(caller int32, recv wire.Type, method string, out siteOut) (found bool, err error) {
	switch recv.Kind {
	case wire.TypeFunction:
		return r.rustMember(caller, recv.Name, rustUnitValue, method, out)
	case wire.TypeNamed:
		if _, _, qualified := cutLast(recv.Name, "::"); qualified {
			return r.rustMember(caller, recv.Name, rustVariant, method, out)
		}
	case wire.TypeResult:
		j := int(recv.Index)
		if j >= len(r.t.Sites) {
			return false, nil
		}
		s := &r.t.Sites[j]
		switch s.Shape {
		case wire.ShapeBare:
			return r.rustMember(s.Caller, s.Callee, rustCalled, method, out)
		case wire.ShapeStatic:
			return r.rustMember(s.Caller, s.Qualifier+"::"+s.Callee, rustCalled, method, out)
		}
	}
	return false, nil
}
