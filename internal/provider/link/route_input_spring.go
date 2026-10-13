package link

import (
	"encoding/json"

	"github.com/Sawmonabo/codectx/internal/model"
)

// Spring MVC hands a handler parameter no binding annotation marks to its
// catch-all (RequestMappingHandlerAdapter.getDefaultArgumentResolvers) only
// after the project's own argument resolvers decline it; those are code
// whose answer no table holds. A project registers one through an API whose
// types name an argument resolver (WebMvcConfigurer.addArgumentResolvers,
// RequestMappingHandlerAdapter.setCustomArgumentResolvers), so a unit with
// no Java file naming one leaves the catch-all exact: a simple value type
// binds as `@RequestParam` keyed by the parameter's name, any other class
// as `@ModelAttribute`; an enum is simple. An interface or an abstract
// class, which the catch-all cannot instantiate, and a type no member
// declares (a library may resolve its own types) publish nothing.

// The hierarchy index's keys of Spring's default resolution.
const (
	// keyJavaResolvers: a Java member file that names an argument
	// resolver (Table.ArgumentResolvers).
	keyJavaResolvers = "jr\x00"
	// keyJavaKind + refKey(type): how Spring's catch-all binds a parameter
	// of a Java type the unit declares (Table.Classes), the kind in Flags.
	keyJavaKind = "jk\x00"
)

// addSpringLinks adds a Java member's entries for Spring's default
// resolution: the file, when it names an argument resolver, and the kind of
// each type it declares.
func addSpringLinks(links interface{ Add(entry) error }, file int32, t *Table) error {
	if t.Language != "java" {
		return nil
	}
	if t.ArgumentResolvers {
		if err := links.Add(entry{Key: keyJavaResolvers, Ref: Ref{File: file}}); err != nil {
			return err
		}
	}
	for _, c := range t.Classes {
		if c.Decl < 0 || int(c.Decl) >= len(t.Decls) {
			continue
		}
		ref := Ref{File: file, Index: c.Decl}
		if err := links.Add(entry{Key: keyJavaKind + refKey(ref), Ref: ref, Flags: c.Kind}); err != nil {
			return err
		}
	}
	return nil
}

// springInputFacts emits a FactInput for each Spring parameter of the
// member at ordinal file left to the catch-all, when no member names an
// argument resolver: a combined read keyed by the parameter's name for a
// simple type or an enum, a combined read with no key for a class the
// unit declares (a model attribute). Several declarations the type may
// denote must agree.
func (h *hierarchy) springInputFacts(file int32, t *Table, emit func(Binding) error) error {
	pending := false
	for i := range t.Inputs {
		pending = pending || t.Inputs[i].Rule == RuleSpring
	}
	if !pending {
		return nil
	}
	custom := false
	if err := h.links.lookup(keyJavaResolvers, func(entry) error { custom = true; return nil }); err != nil || custom {
		return err
	}
	for i := range t.Inputs {
		p := &t.Inputs[i]
		if p.Rule != RuleSpring {
			continue
		}
		kind := p.Kind
		if kind != ClassSimple {
			var err error
			if kind, err = h.springTypeKind(file, p.Class); err != nil {
				return err
			}
		}
		in := inputFact{Framework: p.Framework, Source: model.InputCombined}
		switch kind {
		case ClassSimple:
			in.Key = p.Param
		case ClassBody:
		default:
			continue
		}
		raw, err := json.Marshal(in)
		if err != nil {
			return internalErr("input fact: " + err.Error())
		}
		b := Binding{Kind: FactInput, Site: Ref{File: file, Index: int32(i)}, Target: Ref{File: -1, Index: -1},
			Detail: DetailHierarchy, External: string(raw)}
		if err := emit(b); err != nil {
			return err
		}
	}
	return nil
}

// springTypeKind is how Spring's catch-all binds a parameter of the type
// written in the member at ordinal file: ClassSimple for an enum, ClassBody
// (a model attribute) for a concrete class, classUnknown for an interface,
// an abstract class, a type no member declares or declarations that
// disagree.
func (h *hierarchy) springTypeKind(file int32, written string) (uint8, error) {
	kind, found := classUnknown, false
	err := h.typeEntries(file, written, func(e entry) error {
		k := classUnknown
		err := h.links.lookup(keyJavaKind+refKey(e.Ref), func(c entry) error {
			if c.Flags == ClassSimple || c.Flags == ClassBody {
				k = c.Flags
			}
			return nil
		})
		if err != nil {
			return err
		}
		if found && k != kind {
			kind = classUnknown
			return nil
		}
		kind, found = k, true
		return nil
	})
	return kind, err
}
