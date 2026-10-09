package link

import (
	"context"
	"slices"
	"strings"

	"github.com/Sawmonabo/codectx/internal/model"
	"github.com/Sawmonabo/codectx/internal/pagination"
	"github.com/Sawmonabo/codectx/internal/provider/treesitter/wire"
)

// resolveStatic resolves the sites of a Java, Go, Rust, C or C++ unit: direct
// binding, the signature-keyed address-taken × indirect-site cross-product
// without a reachability fixed point, and class-hierarchy analysis, for Java
// over classes and interfaces and for the others over interfaces and traits.
//
// Every binding is justified by a scope, an import, a type or a signature:
// a bare name by the scope the language reads it in (Java's and C++'s
// enclosing class, Go's package, C's and C++'s linkage scope and enclosing
// namespaces, Rust's file), a qualified name by the type, package, namespace
// or module it names, a member by its receiver's declared type, and an
// indirect call by its declared function type. The cross-product is one
// ordered merge: the address-taken functions and the indirect sites sorted
// together by signature, each signature's functions read before its sites.
func resolveStatic(ctx context.Context, u *unit, emit func(Binding) error) error {
	h, err := buildHierarchy(ctx, u)
	if err != nil {
		return err
	}
	defer h.close()
	// bySignature holds the address-taken functions (tag 0), the indirect
	// sites (tag 1) and the abstract methods a Java call binds when no
	// function joins it (tag 2) under their declared signatures.
	bySignature, err := newEntrySort(u.dir)
	if err != nil {
		return err
	}
	defer bySignature.Close()
	err = u.Files(ctx, func(file int32, _ model.FileVersion, t *Table) error {
		r := staticFile{h: h, ctx: ctx, file: file, t: t, fc: contextOf(file, t), emit: emit, returns: map[int32][]returned{}}
		for i := range t.Sites {
			if err := r.site(int32(i), bySignature); err != nil {
				return err
			}
		}
		for i := range t.Values {
			v := &t.Values[i]
			if v.Type.Kind != wire.TypeFunction {
				continue
			}
			err := r.function(v.Caller, v.Type.Name, func(e entry) error {
				if e.Sig == "" {
					return nil
				}
				sig := e.Sig
				if r.fc.family == "go" && v.Type.Index > 0 {
					sig = goMethodExpression(v.Type.Name, sig)
				}
				return bySignature.Add(entry{Key: sig, Ref: e.Ref})
			})
			if err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	return crossProduct(bySignature, emit)
}

// crossProduct joins each indirect site to every address-taken function of
// its signature, and binds a Java call of an abstract method to that method
// (tag 2: the site, the method's Ref in Aux, the detail in Sig) only when
// no function of its signature is taken as a value. Only one signature's
// functions are held at a time.
func crossProduct(bySignature *pagination.ExternalSort[entry], emit func(Binding) error) error {
	run, err := bySignature.Sorted()
	if err != nil {
		return err
	}
	defer run.Close()
	var sig string
	var functions []Ref
	return run.Each(func(e entry) error {
		if e.Key != sig {
			sig, functions = e.Key, functions[:0]
		}
		switch e.Tag {
		case 0:
			if n := len(functions); n == 0 || functions[n-1] != e.Ref {
				functions = append(functions, e.Ref)
			}
			return nil
		case 2:
			if len(functions) > 0 {
				return nil
			}
			return emit(Binding{Site: e.Ref, Target: refOf(e.Aux), Detail: e.Sig})
		}
		for _, f := range functions {
			if err := emit(Binding{Site: e.Ref, Target: f, Detail: DetailAddressTaken}); err != nil {
				return err
			}
		}
		return nil
	})
}

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
}

// site resolves site i and publishes its bindings; an indirect site with a
// declared function type also joins bySignature.
//
// A Java call of an abstract method binds that declaration only when no
// method reference of its signature does (crossProduct): a call runs a
// body, and the abstract method is the target only when no body is known.
func (r *staticFile) site(i int32, bySignature *pagination.ExternalSort[entry]) error {
	ref := Ref{File: r.file, Index: i}
	s := &r.t.Sites[i]
	joins := r.fc.language == "java" && s.Shape == wire.ShapeMember && s.Receiver.Kind == wire.TypeNamed
	return r.resolve(i, bySignature, func(e entry, detail string) error {
		if joins && e.Flags&flagAbstract != 0 && e.Sig != "" {
			return bySignature.Add(entry{Key: e.Sig, Tag: 2, Ref: ref, Aux: refKey(e.Ref), Sig: detail})
		}
		return r.emit(Binding{Site: ref, Target: e.Ref, Detail: detail})
	})
}

// resolve yields the declarations site i binds by its shape, with each
// binding's detail, and records the types they return. Only a target a call
// passing the site's arguments can call is yielded (fits). With
// bySignature, an indirect site with a declared function type, and a Java
// call of an abstract method, also join it.
func (r *staticFile) resolve(i int32, bySignature *pagination.ExternalSort[entry], yield func(entry, string) error) error {
	s := &r.t.Sites[i]
	_, known := r.returns[i]
	if !known {
		r.returns[i] = nil
	}
	var rets []returned
	bind := func(detail string) func(entry) error {
		return func(e entry) error {
			if !r.fits(s, e) {
				return nil
			}
			if !known && e.Returns != "" {
				ret := returned{file: e.Ref.File, name: e.Returns}
				if !slices.Contains(rets, ret) {
					rets = append(rets, ret)
				}
			}
			return yield(e, detail)
		}
	}
	err := r.shape(i, s, bySignature, bind)
	if !known {
		r.returns[i] = rets
	}
	return err
}

// shape resolves site s, at index i, by its shape.
func (r *staticFile) shape(i int32, s *Site, bySignature *pagination.ExternalSort[entry], bind func(string) func(entry) error) error {
	switch s.Shape {
	case wire.ShapeBare:
		return r.bare(s.Caller, s.Callee, bind)
	case wire.ShapeStatic:
		return r.qualified(s.Caller, s.Qualifier, s.Callee, bind(DetailDirect))
	case wire.ShapeConstructor:
		name := s.Qualifier
		if name == "" {
			name = s.Callee
		}
		return r.h.typesIn(r.ctx, r.fc, name, func(typ entry) error {
			return r.h.methods(typ.Ref, lastComponent(typeName(name), r.fc.sep), bind(DetailDirect))
		})
	case wire.ShapeMember:
		err := r.member(s.Caller, s.Receiver, s.Callee, bind)
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
				return bySignature.Add(entry{Key: e.Sig, Tag: 1, Ref: Ref{File: r.file, Index: i}})
			})
		})
	case wire.ShapeIndirect:
		if s.Receiver.Kind == wire.TypeFunction {
			if err := r.function(s.Caller, s.Receiver.Name, bind(DetailDirect)); err != nil {
				return err
			}
		}
		if s.Signature == "" || bySignature == nil {
			return nil
		}
		return bySignature.Add(entry{Key: s.Signature, Tag: 1, Ref: Ref{File: r.file, Index: i}})
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
	if s.Shape == wire.ShapeStatic && e.Flags&flagStatic == 0 && (r.fc.family == "go" || r.fc.family == "rust") {
		n--
	}
	return e.fits(n)
}

// dispatch binds the methods a call of method on a receiver of static type
// typ reaches (dispatchEntries): DetailHierarchy when the call reaches into
// typ's subtypes, DetailDirect when the language says it cannot.
func (r *staticFile) dispatch(typ Ref, method string, bind func(string) func(entry) error) error {
	return r.h.dispatchEntries(r.ctx, r.fc.family, typ, method, func(e entry, down bool) error {
		if down {
			return bind(DetailHierarchy)(e)
		}
		return bind(DetailDirect)(e)
	})
}

// bare resolves a call of a simple name: in Java, and in C++ inside a
// class, by class-hierarchy analysis over the innermost enclosing type that
// has a method of that name (JLS §15.12.1, [class.member.lookup]), walking
// out from an inner, local or anonymous class to the classes enclosing it
// (an implicit `this` of the class that declares or inherits it); otherwise
// the free functions the language's scope makes visible to the caller. In
// C++ a member of an enclosing class is called without an object of it, so
// only its static members are bound there.
func (r *staticFile) bare(caller int32, callee string, bind func(string) func(entry) error) error {
	if r.fc.language == "java" || r.fc.language == "cpp" {
		found, outer := false, false
		hit := func(detail string) func(entry) error {
			return func(e entry) error {
				found = true
				if outer && r.fc.language == "cpp" && e.Flags&flagStatic == 0 {
					return nil
				}
				return bind(detail)(e)
			}
		}
		err := r.enclosingTypes(caller, func(types []Ref) (bool, error) {
			for _, typ := range types {
				// A level counts only when its type has the method as a
				// member, declared or inherited: one it lacks is not
				// reached through that type's subtypes.
				member := false
				err := r.h.declared(typ, callee, func(entry) error {
					member = true
					return nil
				})
				if err == nil && member {
					err = r.dispatch(typ, callee, hit)
				}
				if err != nil {
					return false, err
				}
			}
			outer = true
			return found, nil
		})
		if err != nil || found {
			return err
		}
		if r.fc.language == "java" {
			return r.staticImport(callee, bind(DetailDirect))
		}
	}
	return r.free(caller, callee, bind(DetailDirect))
}

// staticImport yields the static methods named callee a Java file's static
// imports bring into scope: `import static a.b.C.m;` names C's m, and
// `import static a.b.C.*;` every member of C. A single-static import of the
// name decides alone.
func (r *staticFile) staticImport(callee string, yield func(entry) error) error {
	member := func(typePath, method string) error {
		return r.h.decls.lookup(keyTypeQualified+r.fc.family+"\x00"+typePath, func(typ entry) error {
			return r.h.declared(typ.Ref, method, yield)
		})
	}
	imported := false
	err := r.h.imports(r.fc, callee, func(p string) error {
		imported = true
		if typePath, method, ok := cutLast(p, "."); ok {
			return member(typePath, method)
		}
		return nil
	})
	if err != nil || imported {
		return err
	}
	return r.h.imports(r.fc, ImportAll, func(p string) error { return member(p, callee) })
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

// qualified resolves a call of name through qualifier q: a type's static
// method or associated function (Rust's `Self` is the caller's impl type), a
// package's function when q is a Go import (by the import's path), a
// namespace's function (C++), or a Rust module's function (through the
// `use` that binds q's first component, when one does).
func (r *staticFile) qualified(caller int32, q, name string, yield func(entry) error) error {
	found := false
	hit := func(e entry) error {
		found = true
		return yield(e)
	}
	if r.fc.language == "rust" && q == "Self" {
		return r.innermostTypes(caller, func(typ Ref) error { return r.h.declared(typ, name, yield) })
	}
	if r.fc.family == "go" {
		if imported, err := r.qualifiedGo(q, name, yield); err != nil || imported {
			return err
		}
	}
	err := r.h.typesIn(r.ctx, r.fc, q, func(typ entry) error { return r.h.declared(typ.Ref, name, hit) })
	if err != nil || found {
		return err
	}
	switch r.fc.family {
	case "c":
		return r.qualifiedC(q, name, yield)
	case "rust":
		return r.qualifiedRust(caller, q, name, yield)
	}
	return nil
}

// member resolves a call of method on a receiver: by class-hierarchy
// analysis over its declared type, or over the types the targets of the
// call it is the result of return; along the enclosing class's supertypes
// for `super`; as a qualified call through a module; or, for Go, over the
// declared type of the field it reads (fieldGo).
func (r *staticFile) member(caller int32, recv wire.Type, method string, bind func(string) func(entry) error) error {
	switch recv.Kind {
	case wire.TypeNamed:
		return r.h.typesIn(r.ctx, r.fc, recv.Name, func(typ entry) error { return r.dispatch(typ.Ref, method, bind) })
	case wire.TypeResult:
		return r.results(int32(recv.Index), method, func(typ Ref) error { return r.dispatch(typ, method, bind) })
	case wire.TypeSuper:
		return r.h.typesIn(r.ctx, r.fc, recv.Name, func(typ entry) error {
			return r.h.links.lookup(keySuper+refKey(typ.Ref), func(super entry) error {
				return r.h.declared(super.Ref, method, bind(DetailDirect))
			})
		})
	case wire.TypeModule:
		return r.qualified(caller, recv.Name, method, bind(DetailDirect))
	case wire.TypeField:
		if r.fc.family == "go" {
			return r.fieldGo(recv.Name, func(typ Ref) error { return r.dispatch(typ, method, bind) })
		}
	}
	return nil
}

// results yields, each once, the types the targets of site j return that a
// call of method on the result reaches, each read in the file of the target
// that returns it: the static type of a call's result
// (`NewServer().Start()`, a fluent chain, `Config::new().load()`). A Rust
// result held in a smart pointer (`-> Rc<Foo>`) reaches the owned type,
// except by a method the pointer answers itself (rustReceiver). A site not
// yet resolved is resolved first, without publishing anything.
func (r *staticFile) results(j int32, method string, yield func(Ref) error) error {
	if j < 0 || int(j) >= len(r.t.Sites) {
		return nil
	}
	if _, ok := r.returns[j]; !ok {
		if err := r.resolve(j, nil, func(entry, string) error { return nil }); err != nil {
			return err
		}
	}
	seen := map[Ref]bool{}
	for _, ret := range r.returns[j] {
		name := ret.name
		if r.fc.family == "rust" {
			var ok bool
			if name, ok = rustReceiver(name, method); !ok {
				continue
			}
		}
		err := r.h.types(r.ctx, ret.file, name, func(typ Ref) error {
			if seen[typ] {
				return nil
			}
			seen[typ] = true
			return yield(typ)
		})
		if err != nil {
			return err
		}
	}
	return nil
}

// function yields the declarations a function or method name taken as a
// value in caller denotes: `f`, `pkg.F`, `T.M`, `T::m`, `&f`.
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
		return r.innermostTypes(caller, func(typ Ref) error {
			return r.h.dispatchEntries(r.ctx, r.fc.family, typ, last, func(e entry, _ bool) error { return yield(e) })
		})
	}
	return r.qualified(caller, q, last, yield)
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
