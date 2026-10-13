package link

import (
	"cmp"
	"context"
	"slices"
	"strings"

	"github.com/Sawmonabo/codectx/internal/model"
	"github.com/Sawmonabo/codectx/internal/provider/treesitter/wire"
)

// sigSink receives what a site joins the cross-product with (the signature
// sink of staticRun.member): an indirect site (tag 1) or a Java call of an
// abstract method (tag 2, the method's Ref in Aux, the detail in Sig), each
// under its declared signature in Key and the site's Ref in Ref.
type sigSink func(entry) error

// staticFile resolves the sites and values of one member table.
type staticFile struct {
	h    *hierarchy
	ctx  context.Context
	file int32
	t    *Table
	fc   fileCtx
	emit func(Binding) error
	// returns holds, for each site resolved so far, the types its targets
	// return, which a receiver of kind TypeResult naming it reads. A site
	// is present while it is being resolved, so a cycle reads nothing.
	returns map[int32][]returned
}

// returned is a type a target returns, as the target's own file writes it.
type returned struct {
	file int32
	name string
	// exact reports a result that is an object of exactly the type name
	// (flagExactReturn).
	exact bool
}

// siteOut receives what the resolution of one site finds: each declaration
// of the unit it calls, with the technique and the occurrence's dispatch,
// and each proven external call, to the external method key names
// (externalMethod), or, with key empty, to nothing a node names
// (FactExternalSite). A nil external drops the external calls (a function
// taken as a value names no call).
type siteOut struct {
	call     func(e entry, detail string, d model.Dispatch) error
	external func(key string, d model.Dispatch) error
}

// ext reports an external call to out.
func (out siteOut) ext(key string, d model.Dispatch) error {
	if out.external == nil {
		return nil
	}
	return out.external(key, d)
}

// with is out with every call of the unit reported through call.
func (out siteOut) with(call func(e entry, detail string, d model.Dispatch) error) siteOut {
	return siteOut{call: call, external: out.external}
}

// valueOut is the siteOut of a function taken as a value: each declaration
// it denotes, and no external call.
func valueOut(yield func(entry) error) siteOut {
	return siteOut{call: func(e entry, _ string, _ model.Dispatch) error { return yield(e) }}
}

// site resolves site i and emits its facts: a FactCall per declaration it
// calls, with its FactReturn (returnFact) and the FactFlow of each argument
// into the parameter it binds and its FactSummary (argumentFlows,
// flowFacts), and a FactCall to an external method or a
// FactExternalSite for a proven external call. An indirect site with a
// declared function type also joins bySignature.
//
// A Java call of an abstract method binds that declaration only when no
// method reference of its signature does (crossProduct): a call runs a
// body, and the abstract method is the target only when no body is known.
func (r *staticFile) site(i int32, bySignature sigSink) error {
	ref := Ref{File: r.file, Index: i}
	s := &r.t.Sites[i]
	joins := r.fc.language == "java" && s.Shape == wire.ShapeMember && s.Receiver.Kind == wire.TypeNamed
	out := siteOut{
		call: func(e entry, detail string, d model.Dispatch) error {
			if joins && e.Flags&flagAbstract != 0 && e.Sig != "" {
				return bySignature(entry{Key: e.Sig, Tag: 2, Ref: ref, Aux: refKey(e.Ref), Sig: detail})
			}
			if err := r.emit(Binding{Site: ref, Target: e.Ref, Detail: detail, Dispatch: d}); err != nil {
				return err
			}
			if err := returnFact(s, ref, e.Ref, r.emit); err != nil {
				return err
			}
			return r.flows(ref, s, e)
		},
		external: func(key string, d model.Dispatch) error {
			if key == "" {
				return r.emit(Binding{Kind: FactExternalSite, Site: ref})
			}
			return r.emit(Binding{Site: ref, External: key, Detail: DetailDirect, Dispatch: d})
		},
	}
	return r.resolve(i, bySignature, out)
}

// flows emits the FactFlow of each argument of site s at ref into the
// parameter of e, the declaration the site calls, it binds, and its
// FactSummary when that parameter returns (flowFacts). A Go method
// expression or a Rust path naming a method (`T.M(t, x)`, `T::m(&t, x)`)
// passes the receiver as its first argument.
func (r *staticFile) flows(ref Ref, s *Site, e entry) error {
	if len(s.Arguments) == 0 {
		return nil
	}
	var params []paramInfo
	if err := r.h.parameters(e.Ref, func(p paramInfo) error {
		params = append(params, p)
		return nil
	}); err != nil {
		return err
	}
	return argumentFlows(s, params, r.receiverFirst(s, e), func(arg, pos int32) error {
		return flowFacts(s, ref, e.Ref, params, arg, pos, r.emit)
	})
}

// javaUnbound reports a Java method reference written `T::m` that resolved
// to e, an instance method, through its qualifier T naming a type (function):
// an unbound reference, whose functional interface method takes the receiver
// as its first parameter and m's parameters after it (JLS §15.13.1, the
// ReferenceType :: Identifier form whose compile-time declaration is not
// static). recv is T as written. A reference through `this` or `super` is
// bound, its receiver captured, and a static method or a constructor (`T::new`,
// static in the table) takes the arguments as its own parameters.
func javaUnbound(written string, e entry) (recv string, ok bool) {
	q, _, found := cutLast(strings.TrimSpace(written), "::")
	if !found || q == "this" || q == "super" || e.Flags&flagStatic != 0 {
		return "", false
	}
	return q, true
}

// receiverSignature is the signature of a function value that takes the
// receiver of the method of signature sig, a receiver of type recv, as its
// first parameter: sig with recv put first in its parameter list, so the
// value joins the indirect sites, or the functional interface methods, of
// that type.
func receiverSignature(recv, sig string) string {
	switch {
	case !strings.HasPrefix(sig, "("):
		return sig
	case strings.HasPrefix(sig, "()"):
		return "(" + recv + sig[1:]
	}
	return "(" + recv + "," + sig[1:]
}

// receiverFirst reports that site s passes the receiver of e, the method it
// calls, as its first argument: a Go method expression or a Rust path
// naming a method.
func (r *staticFile) receiverFirst(s *Site, e entry) bool {
	return s.Shape == wire.ShapeStatic && e.Flags&flagStatic == 0 && (r.fc.family == "go" || r.fc.family == "rust")
}

// resolve reports to out what site i binds by its shape, and records the
// types its targets return. Only a target a call passing the site's
// arguments can call is reported (fits). With bySignature, an indirect site
// with a declared function type, and a Java call of an abstract method, also
// join it.
func (r *staticFile) resolve(i int32, bySignature sigSink, out siteOut) error {
	s := &r.t.Sites[i]
	_, known := r.returns[i]
	if !known {
		r.returns[i] = nil
	}
	var rets []returned
	err := r.shape(i, s, bySignature, out.with(func(e entry, detail string, d model.Dispatch) error {
		if !r.fits(s, e) {
			return nil
		}
		if !known && e.Returns != "" {
			ret := returned{file: e.Ref.File, name: e.Returns, exact: e.Flags&flagExactReturn != 0}
			if !slices.Contains(rets, ret) {
				rets = append(rets, ret)
			}
		}
		return out.call(e, detail, d)
	}))
	if !known {
		r.returns[i] = rets
	}
	return err
}

// exactly reports each declaration to out as an exact call of detail.
func exactly(out siteOut, detail string) func(entry) error {
	return func(e entry) error { return out.call(e, detail, model.DispatchExact) }
}

// shape resolves site s, at index i, by its shape.
//
// A call binds the one method its receiver's static type declares or
// inherits (declared), `virtual` when an override of it can run
// (dispatchOf), never the overrides themselves, which a query reaches
// through `overrides`. A free function, a qualified or `super` call, a
// constructor and a method of a constructed object are `exact`.
func (r *staticFile) shape(i int32, s *Site, bySignature sigSink, out siteOut) error {
	switch s.Shape {
	case wire.ShapeBare:
		return r.bare(s.Caller, s.Callee, out)
	case wire.ShapeStatic:
		return r.qualified(s.Caller, s.Qualifier, s.Callee, out)
	case wire.ShapeConstructor:
		name := s.Qualifier
		if name == "" {
			name = s.Callee
		}
		return r.construct(name, out)
	case wire.ShapeMember:
		err := r.member(s.Caller, s.Receiver, s.Callee, out)
		if err != nil || bySignature == nil || r.fc.language != "java" || s.Receiver.Kind != wire.TypeNamed {
			return err
		}
		// A Java call of an abstract method (a functional interface's, on a
		// field or local holding a method reference) is an indirect call
		// of that method's signature: it joins every method reference of
		// that signature, as an indirect site joins the address-taken
		// functions of its declared type.
		return r.h.typesIn(r.ctx, r.fc, s.Receiver.Name, func(typ entry) error {
			return r.h.declared(typ.Ref, s.Callee, func(e entry) error {
				if e.Flags&flagAbstract == 0 || e.Sig == "" {
					return nil
				}
				return bySignature(entry{Key: e.Sig, Tag: 1, Ref: Ref{File: r.file, Index: i}})
			})
		})
	case wire.ShapeIndirect:
		if s.Receiver.Kind == wire.TypeFunction {
			if err := r.function(s.Caller, s.Receiver.Name, exactly(out, DetailDirect)); err != nil {
				return err
			}
		}
		if s.Signature == "" || bySignature == nil {
			return nil
		}
		return bySignature(entry{Key: s.Signature, Tag: 1, Ref: Ref{File: r.file, Index: i}})
	}
	return nil
}

// fits reports that site s passes as many arguments as target e accepts
// (wire.Export.Params). A Go method expression or a Rust path naming a
// method (`T.M(t, x)`, `T::m(t, x)`) passes the receiver as its first
// argument. A site the worker sent no wire.Site for (not Described) is not
// filtered: it carries no argument list.
func (r *staticFile) fits(s *Site, e entry) bool {
	if !s.Described {
		return true
	}
	n := len(s.Args)
	if r.receiverFirst(s, e) {
		n--
	}
	return e.fits(n)
}

// dispatchOf is the dispatch of a call of method on a receiver of static
// type typ (iface when it is an interface or trait) that binds found, the
// nearest declarations of it typ declares or inherits: `virtual` when an
// override of them can run. A Rust call is virtual on a trait object or a
// generic bounded by a trait (whose receiver names the trait) and exact on a
// concrete type, which no impl overrides; every other family's rule is
// overridable's.
func (r *staticFile) dispatchOf(typ Ref, iface bool, method string, found []entry) (model.Dispatch, error) {
	if r.fc.family == "rust" {
		if iface {
			return model.DispatchVirtual, nil
		}
		return model.DispatchExact, nil
	}
	v, err := r.h.overridable(r.fc.family, typ, method, found)
	if err != nil || !v {
		return model.DispatchExact, err
	}
	return model.DispatchVirtual, nil
}

// onType reports to out the call of method on a receiver of static type
// typ: the declarations typ declares or inherits, with their dispatch
// (exact when the receiver is known to be exactly typ, a constructed
// object), else the method of typ's one supertype outside the unit
// (inherited).
func (r *staticFile) onType(typ entry, method string, exact bool, out siteOut) error {
	var found []entry
	if err := r.h.declared(typ.Ref, method, func(e entry) error {
		found = append(found, e)
		return nil
	}); err != nil {
		return err
	}
	if len(found) == 0 {
		d := model.DispatchVirtual
		if exact {
			d = model.DispatchExact
		}
		return r.inherited(typ.Ref, method, d, out)
	}
	d := model.DispatchExact
	if !exact {
		var err error
		if d, err = r.dispatchOf(typ.Ref, typ.Flags&flagInterface != 0, method, found); err != nil {
			return err
		}
	}
	for _, e := range found {
		if err := out.call(e, DetailHierarchy, d); err != nil {
			return err
		}
	}
	return nil
}

// inherited reports the call of method on typ, a type of the unit that
// neither declares nor inherits it from the unit: with exactly one
// supertype outside the unit (externalSupers), a call of that type's
// external method; with several, a proven external call no node names;
// with none, nothing.
func (r *staticFile) inherited(typ Ref, method string, d model.Dispatch, out siteOut) error {
	external, err := r.h.externalSupers(r.fc.family, typ, method)
	if err != nil {
		return err
	}
	switch len(external) {
	case 0:
		return nil
	case 1:
		return out.ext(externalMethod(r.fc.family, external[0].Aux, method), d)
	}
	return out.ext("", d)
}

// named reports the call of method on a receiver of the type written as
// written in a file of fc: through each type of the unit it names (onType),
// else, when it names a type outside the project, that type's external
// method, `virtual`.
func (r *staticFile) named(fc fileCtx, written, method string, exact bool, out siteOut) error {
	found := false
	err := r.h.typesIn(r.ctx, fc, written, func(typ entry) error {
		found = true
		return r.onType(typ, method, exact, out)
	})
	if err != nil || found {
		return err
	}
	q, ok, err := r.h.externalName(r.ctx, fc, written)
	if err != nil || !ok {
		return err
	}
	d := model.DispatchVirtual
	if exact {
		d = model.DispatchExact
	}
	return out.ext(externalMethod(fc.family, q, method), d)
}

// bare resolves a call of a simple name: in Java, and in C++ inside a
// class, through the innermost enclosing type that has a method of that
// name, declared or inherited (JLS §15.12.1, [class.member.lookup]), walking
// out from an inner, local or anonymous class to the classes enclosing it
// (an implicit `this` of the class that declares or inherits it), with that
// type's dispatch; otherwise the free functions the language's scope makes
// visible to the caller, and in C and C++ first what an object-like macro of
// the name stands for (aliasC). In C++ a member of an enclosing class is
// called without an object of it, so only its static members are bound
// there.
func (r *staticFile) bare(caller int32, callee string, out siteOut) error {
	if r.fc.language == "java" || r.fc.language == "cpp" {
		found, outer := false, false
		err := r.enclosingTypes(caller, func(types []Ref) (bool, error) {
			for _, typ := range types {
				// A level counts when its type has the method as a member,
				// declared or inherited, even one it cannot call there.
				var members []entry
				member := false
				err := r.h.declared(typ, callee, func(e entry) error {
					member = true
					if !outer || r.fc.language != "cpp" || e.Flags&flagStatic != 0 {
						members = append(members, e)
					}
					return nil
				})
				if err != nil {
					return false, err
				}
				if !member {
					continue
				}
				found = true
				d, err := r.dispatchOf(typ, false, callee, members)
				if err != nil {
					return false, err
				}
				for _, e := range members {
					if err := out.call(e, DetailHierarchy, d); err != nil {
						return false, err
					}
				}
			}
			outer = true
			return found, nil
		})
		if err != nil || found {
			return err
		}
		if r.fc.language == "java" {
			return r.staticImport(caller, callee, out)
		}
	}
	if r.fc.family == "c" {
		if aliased, err := r.aliasC(caller, callee, exactly(out, DetailDirect), nil); err != nil || aliased {
			return err
		}
	}
	found := false
	err := r.free(caller, callee, func(e entry) error {
		found = true
		return out.call(e, DetailDirect, model.DispatchExact)
	})
	if err != nil || found || r.fc.language != "cpp" {
		return err
	}
	// A C++ call of a type's name constructs it ([expr.type.conv]).
	typed, err := r.isType(callee)
	if err != nil || !typed {
		return err
	}
	return r.construct(callee, out)
}

// isType reports that written names a type of the unit in the member.
func (r *staticFile) isType(written string) (bool, error) {
	typed := false
	err := r.h.typesIn(r.ctx, r.fc, written, func(entry) error {
		typed = true
		return nil
	})
	return typed, err
}

// construct reports the construction of the type written as written: each
// constructor of each type of the unit it names (the type's methods of its
// own name), else the type itself, whose implicit constructor runs (JLS
// §8.8.9, [class.default.ctor]), each exact. A type outside the project is a
// proven external call no node names.
func (r *staticFile) construct(written string, out siteOut) error {
	ctor := lastComponent(typeName(written), r.fc.sep)
	typed := false
	err := r.h.typesIn(r.ctx, r.fc, written, func(typ entry) error {
		typed = true
		found := false
		err := r.h.methods(typ.Ref, ctor, func(e entry) error {
			found = true
			return out.call(e, DetailDirect, model.DispatchExact)
		})
		if err != nil || found {
			return err
		}
		return out.call(typ, DetailDirect, model.DispatchExact)
	})
	if err != nil || typed {
		return err
	}
	if _, ok, err := r.h.externalName(r.ctx, r.fc, written); err != nil || !ok {
		return err
	}
	return out.ext("", model.DispatchExact)
}

// constructed is the type site j constructs, as written: a `new` or a
// constructor call (ShapeConstructor), or a C++ call of a type's name; ok is
// false for any other site.
func (r *staticFile) constructed(j int32) (string, bool, error) {
	if j < 0 || int(j) >= len(r.t.Sites) {
		return "", false, nil
	}
	s := &r.t.Sites[j]
	switch {
	case s.Shape == wire.ShapeConstructor:
		return cmp.Or(s.Qualifier, s.Callee), true, nil
	case s.Shape == wire.ShapeBare && r.fc.language == "cpp":
		typed, err := r.isType(s.Callee)
		return s.Callee, typed, err
	}
	return "", false, nil
}

// staticImport reports the static methods named callee a Java file's
// static imports bring into scope: `import static a.b.C.m;` names C's m,
// and `import static a.b.C.*;` every member of C. A single-static import of
// the name decides alone, and one of a type outside the unit is a call of
// its external method. Otherwise a call inside a class that inherits the
// method from outside the unit calls it there (inherited).
func (r *staticFile) staticImport(caller int32, callee string, out siteOut) error {
	found := false
	member := func(typePath, method string) error {
		return r.h.decls.lookup(keyTypeQualified+r.fc.family+"\x00"+typePath, func(typ entry) error {
			found = true
			return r.h.declared(typ.Ref, method, exactly(out, DetailDirect))
		})
	}
	var single []string
	err := r.h.imports(r.fc, callee, func(p string) error {
		single = append(single, p)
		if typePath, method, ok := cutLast(p, "."); ok {
			return member(typePath, method)
		}
		return nil
	})
	if err != nil || found {
		return err
	}
	if len(single) > 0 {
		if typePath, method, ok := cutLast(single[0], "."); ok && len(single) == 1 {
			return out.ext(externalMethod(r.fc.family, typePath, method), model.DispatchExact)
		}
		return nil
	}
	if err := r.h.imports(r.fc, ImportAll, func(p string) error { return member(p, callee) }); err != nil || found {
		return err
	}
	return r.innermostTypes(caller, func(typ Ref) error { return r.inherited(typ, callee, model.DispatchVirtual, out) })
}

// free yields the free functions named callee visible to code in caller:
// those its scope declares and, when it declares none, those its imports
// bring into scope (a Go dot import, a C++ using-declaration or
// using-directive, a Rust `use`).
func (r *staticFile) free(caller int32, callee string, yield func(entry) error) error {
	switch r.fc.family {
	case "c":
		return r.freeC(caller, callee, yield)
	case "go":
		return r.freeGo(callee, yield)
	case "rust":
		return r.freeRust(caller, callee, yield)
	}
	return r.h.decls.lookup(keyFunc+r.fc.family+"\x00"+declScope(r.t, r.fc)+"\x00"+callee, yield)
}

// qualified resolves a call of name through qualifier q, each binding
// exact: a type's static method or associated function (Rust's `Self` is the
// caller's impl type), a package's function when q is a Go import (by the
// import's path), a namespace's function (C++), or a Rust module's function
// (through the `use` that binds q's first component, when one does).
//
// A qualifier that names nothing of the unit is a proven external call: a Go
// import of a package outside the unit, a Java type outside the project (its
// external method), a C++ namespace or type, or a Rust path, that names no
// type or module of the unit. A type of the unit that neither declares nor
// inherits name calls it on its one supertype outside the unit (inherited).
func (r *staticFile) qualified(caller int32, q, name string, out siteOut) error {
	yield := exactly(out, DetailDirect)
	if r.fc.language == "rust" && q == "Self" {
		return r.innermostTypes(caller, func(typ Ref) error { return r.h.declared(typ, name, yield) })
	}
	if r.fc.family == "go" {
		imported, inUnit, err := r.qualifiedGo(q, name, yield)
		if err != nil || imported {
			if err == nil && !inUnit {
				err = out.ext("", model.DispatchExact)
			}
			return err
		}
	}
	found, typed := false, false
	err := r.h.typesIn(r.ctx, r.fc, q, func(typ entry) error {
		typed = true
		var members []entry
		if err := r.h.declared(typ.Ref, name, func(e entry) error {
			members = append(members, e)
			return nil
		}); err != nil {
			return err
		}
		if len(members) == 0 {
			return r.inherited(typ.Ref, name, model.DispatchExact, out)
		}
		found = true
		for _, e := range members {
			if err := yield(e); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil || found || typed {
		return err
	}
	switch r.fc.family {
	case "c":
		bound := false
		if err := r.qualifiedC(q, name, func(e entry) error {
			bound = true
			return yield(e)
		}); err != nil || bound {
			return err
		}
		return out.ext("", model.DispatchExact)
	case "rust":
		inUnit, err := r.qualifiedRust(caller, q, name, yield)
		if err != nil || inUnit {
			return err
		}
		return out.ext("", model.DispatchExact)
	case "java":
		external, ok, err := r.h.externalName(r.ctx, r.fc, q)
		if err != nil || !ok {
			return err
		}
		return out.ext(externalMethod(r.fc.family, external, name), model.DispatchExact)
	}
	return nil
}

// member resolves a call of method on a receiver: over its declared type
// (exact for a C++ object declared by value, wire.ExactObject), or over the
// types the targets of the call it is the result of return (exact when that
// call constructs it, or when the target returns its type by value,
// flagExactReturn); along the enclosing class's
// supertypes for `super`, exact; as a qualified call through a module; or,
// for Go and Java, over the declared type of the field it reads (fieldGo,
// fieldJava).
func (r *staticFile) member(caller int32, recv wire.Type, method string, out siteOut) error {
	switch recv.Kind {
	case wire.TypeNamed:
		return r.named(r.fc, recv.Name, method, recv.Index == wire.ExactObject, out)
	case wire.TypeResult:
		// A constructed object is exactly the type it constructs.
		if typ, ok, err := r.constructed(int32(recv.Index)); err != nil || ok {
			if err != nil {
				return err
			}
			return r.named(r.fc, typ, method, true, out)
		}
		return r.results(int32(recv.Index), method, func(fc fileCtx, written string, exact bool) error {
			return r.named(fc, written, method, exact, out)
		})
	case wire.TypeSuper:
		return r.h.typesIn(r.ctx, r.fc, recv.Name, func(typ entry) error {
			found := false
			err := r.h.links.lookup(keySuper+refKey(typ.Ref), func(super entry) error {
				return r.h.declared(super.Ref, method, func(e entry) error {
					found = true
					return out.call(e, DetailDirect, model.DispatchExact)
				})
			})
			if err != nil || found {
				return err
			}
			return r.inherited(typ.Ref, method, model.DispatchExact, out)
		})
	case wire.TypeModule:
		return r.qualified(caller, recv.Name, method, out)
	case wire.TypeClass:
		return r.h.typesIn(r.ctx, r.fc, recv.Name, func(typ entry) error {
			return r.onType(typ, method, true, out)
		})
	case wire.TypeField:
		switch r.fc.family {
		case "go":
			return r.fieldGo(recv.Name, func(fc fileCtx, written string) error {
				return r.named(fc, written, method, false, out)
			})
		case "java":
			return r.fieldJava(recv.Name, method, out)
		}
	}
	return nil
}

// results calls each with the context of the file of each target of site j
// and the type it returns, as that file writes it, each once, and whether
// the result is an object of exactly that type (a C++ class returned by
// value, flagExactReturn): the static type of a call's result
// (`NewServer().Start()`, a fluent chain, `Config::new().load()`). A Rust
// result held in a smart pointer (`-> Rc<Foo>`) reaches the owned type,
// except by a method the pointer answers itself (rustReceiver). A site not yet resolved is resolved first,
// without publishing anything.
func (r *staticFile) results(j int32, method string, each func(fc fileCtx, written string, exact bool) error) error {
	if j < 0 || int(j) >= len(r.t.Sites) {
		return nil
	}
	if _, ok := r.returns[j]; !ok {
		noop := siteOut{call: func(entry, string, model.Dispatch) error { return nil }}
		if err := r.resolve(j, nil, noop); err != nil {
			return err
		}
	}
	for _, ret := range r.returns[j] {
		name := ret.name
		if r.fc.family == "rust" {
			var ok bool
			if name, ok = rustReceiver(name, method); !ok {
				continue
			}
		}
		fc, found, err := r.h.fileContext(ret.file)
		if err != nil {
			return err
		}
		if !found {
			continue
		}
		if err := each(fc, name, ret.exact); err != nil {
			return err
		}
	}
	return nil
}

// function yields the declarations a function or method name taken as a
// value in caller denotes: `f`, `pkg.F`, `T.M`, `T::m`, `&f`, and the
// method `this`, `self` or `Self` declares or inherits (`this::m`).
func (r *staticFile) function(caller int32, written string, yield func(entry) error) error {
	name := strings.TrimSpace(strings.TrimLeft(strings.TrimSpace(written), "&*"))
	sep := r.fc.sep
	if r.fc.language == "java" {
		sep = "::"
	}
	q, last, ok := cutLast(name, sep)
	if !ok {
		if r.fc.language == "java" {
			return nil
		}
		return r.free(caller, name, yield)
	}
	if q == "this" || q == "self" {
		return r.innermostTypes(caller, func(typ Ref) error { return r.h.declared(typ, last, yield) })
	}
	return r.qualified(caller, q, last, valueOut(yield))
}

// enclosingTypes calls level with the types whose bodies enclose code in
// caller, one enclosing level at a time, innermost first, until level
// reports it is done: each type declaration enclosing caller, and the types
// the receiver of an enclosing callable declared outside any type names (a
// Go, Rust or C++ method defined outside its type) or a method of a Java
// anonymous class body names (its synthetic type, inside the enclosing
// class even when an initializer block, which declares nothing, holds it).
func (r *staticFile) enclosingTypes(caller int32, level func([]Ref) (bool, error)) error {
	for i := caller; i >= 0 && int(i) < len(r.t.Decls); i = r.t.Decls[i].Parent {
		d := &r.t.Decls[i]
		var types []Ref
		switch {
		case isType(d.Kind):
			types = []Ref{{File: r.file, Index: i}}
		case d.Receiver != "" && (d.Parent < 0 || int(d.Parent) >= len(r.t.Decls) || !isType(r.t.Decls[d.Parent].Kind) ||
			anonymousReceiver(r.fc, d)):
			err := r.h.typesIn(r.ctx, r.fc, d.Receiver, func(e entry) error {
				types = append(types, e.Ref)
				return nil
			})
			if err != nil {
				return err
			}
		}
		if len(types) == 0 {
			continue
		}
		if done, err := level(types); err != nil || done {
			return err
		}
	}
	return nil
}

// innermostTypes yields the types of the innermost level enclosingTypes
// finds: those `this`, `self` and `Self` denote.
func (r *staticFile) innermostTypes(caller int32, yield func(Ref) error) error {
	return r.enclosingTypes(caller, func(types []Ref) (bool, error) {
		for _, typ := range types {
			if err := yield(typ); err != nil {
				return true, err
			}
		}
		return true, nil
	})
}

func cutLast(s, sep string) (before, after string, ok bool) {
	i := strings.LastIndex(s, sep)
	if i < 0 {
		return "", s, false
	}
	return s[:i], s[i+len(sep):], true
}

func lastComponent(s, sep string) string {
	_, last, _ := cutLast(s, sep)
	return last
}

// fieldPath calls each with the type the field path fields, read from a
// value of the type typ, holds, as the file declaring the last field writes
// it, in that file's context: each step the field a selector of its name
// denotes on the type the step before it holds (keyGoField: a Go type's own
// or promoted, a Java class's own, else, in Java, the nearest along its
// supertypes, JLS §8.3, else the class enclosing it in the member's file),
// whose declared type is read in the file that declares the field. A field
// with no declared type ends the path.
func (r *staticFile) fieldPath(typ Ref, fields []string, each func(fc fileCtx, written string) error) error {
	var found []entry
	seen := map[Ref]bool{typ: true}
	queue := []Ref{typ}
	for len(queue) > 0 && len(found) == 0 {
		t := queue[0]
		queue = queue[1:]
		if err := r.h.links.lookup(keyGoField+refKey(t)+fields[0], func(f entry) error {
			found = append(found, f)
			return nil
		}); err != nil {
			return err
		}
		if len(found) > 0 || r.fc.family != "java" {
			break
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
	if len(found) == 0 && r.fc.family == "java" && typ.File == r.file && typ.Index >= 0 && int(typ.Index) < len(r.t.Decls) {
		// A field read inside an inner class may be the enclosing class's
		// (JLS §6.5.6.1, §8.1.3), named by the inner class it is read in.
		for p := r.t.Decls[typ.Index].Parent; p >= 0 && int(p) < len(r.t.Decls); p = r.t.Decls[p].Parent {
			if isType(r.t.Decls[p].Kind) {
				return r.fieldPath(Ref{File: r.file, Index: p}, fields, each)
			}
		}
	}
	for _, f := range found {
		if f.Sig == "" {
			continue
		}
		if len(fields) == 1 {
			fc, ok, err := r.h.fileContext(f.Ref.File)
			if err != nil {
				return err
			}
			if ok {
				if err := each(fc, f.Sig); err != nil {
					return err
				}
			}
			continue
		}
		err := r.h.types(r.ctx, f.Ref.File, f.Sig, func(next Ref) error { return r.fieldPath(next, fields[1:], each) })
		if err != nil {
			return err
		}
	}
	return nil
}

// fieldJava resolves a call of method on a Java field receiver written
// "Owner.f1.f2…" (wire.TypeField), Owner as written, a qualified name when
// written qualified: the longest leading part that names a type of the unit
// is the owner, and the rest is read on it (fieldPath); the call binds on the
// type the last field declares (named), of the unit or outside it. An owner
// outside the project (`System.out`) makes it a proven external call no node
// names.
func (r *staticFile) fieldJava(written, method string, out siteOut) error {
	parts := strings.Split(written, ".")
	for k := len(parts) - 1; k >= 1; k-- {
		found := false
		err := r.h.typesIn(r.ctx, r.fc, strings.Join(parts[:k], "."), func(typ entry) error {
			found = true
			return r.fieldPath(typ.Ref, parts[k:], func(fc fileCtx, written string) error {
				return r.named(fc, written, method, false, out)
			})
		})
		if err != nil || found {
			return err
		}
	}
	if len(parts) < 2 {
		return nil
	}
	if _, ok, err := r.h.externalName(r.ctx, r.fc, strings.Join(parts[:len(parts)-1], ".")); err != nil || !ok {
		return err
	}
	return out.ext("", model.DispatchVirtual)
}
