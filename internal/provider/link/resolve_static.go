package link

import (
	"context"
	"path"
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
	// bySignature holds the address-taken functions (tag 0) and the
	// indirect sites (tag 1) under their declared signatures.
	bySignature, err := newEntrySort(u.dir)
	if err != nil {
		return err
	}
	defer bySignature.Close()
	err = u.Files(ctx, func(file int32, _ model.FileVersion, t *Table) error {
		r := staticFile{h: h, ctx: ctx, file: file, t: t, fc: contextOf(t), emit: emit}
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
				return bySignature.Add(entry{Key: e.Sig, Ref: e.Ref})
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
// its signature. Only one signature's functions are held at a time.
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
		if e.Tag == 0 {
			if n := len(functions); n == 0 || functions[n-1] != e.Ref {
				functions = append(functions, e.Ref)
			}
			return nil
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
	// importNames is imports' answer, computed on first use.
	importNames map[string]bool
}

// site resolves site i by its shape, and adds an indirect site with a
// declared function type to bySignature.
func (r *staticFile) site(i int32, bySignature *pagination.ExternalSort[entry]) error {
	s := &r.t.Sites[i]
	ref := Ref{File: r.file, Index: i}
	bind := func(detail string) func(entry) error {
		return func(e entry) error {
			d := detail
			if d == DetailHierarchy && e.Flags&flagStatic != 0 {
				d = DetailDirect
			}
			return r.emit(Binding{Site: ref, Target: e.Ref, Detail: d})
		}
	}
	switch s.Shape {
	case wire.ShapeBare:
		return r.bare(s.Caller, s.Callee, bind)
	case wire.ShapeStatic:
		return r.qualified(s.Caller, s.Qualifier, s.Callee, s.QualifierIsImport, bind(DetailDirect))
	case wire.ShapeConstructor:
		name := s.Qualifier
		if name == "" {
			name = s.Callee
		}
		return r.h.typesIn(r.ctx, r.fc, name, func(typ entry) error {
			return r.h.methods(typ.Ref, lastComponent(typeName(name), r.fc.sep), bind(DetailDirect))
		})
	case wire.ShapeMember:
		return r.member(s.Caller, s.Receiver, s.Callee, bind)
	case wire.ShapeIndirect:
		if s.Receiver.Kind == wire.TypeFunction {
			if err := r.function(s.Caller, s.Receiver.Name, bind(DetailDirect)); err != nil {
				return err
			}
		}
		if s.Signature == "" {
			return nil
		}
		return bySignature.Add(entry{Key: s.Signature, Tag: 1, Ref: ref})
	}
	return nil
}

// bare resolves a call of a simple name: in Java, and in C++ inside a
// class, the enclosing class's methods by class-hierarchy analysis (an
// implicit `this`); otherwise the free functions the language's scope makes
// visible to the caller.
func (r *staticFile) bare(caller int32, callee string, bind func(string) func(entry) error) error {
	if r.fc.language == "java" || r.fc.language == "cpp" {
		found := false
		err := r.enclosingTypes(caller, func(typ Ref) error {
			return r.h.dispatchEntries(r.ctx, typ, callee, func(e entry) error {
				found = true
				return bind(DetailHierarchy)(e)
			})
		})
		if err != nil || found || r.fc.language == "java" {
			return err
		}
	}
	return r.free(caller, callee, bind(DetailDirect))
}

// free yields the free functions named callee visible to code in caller.
func (r *staticFile) free(caller int32, callee string, yield func(entry) error) error {
	if r.fc.family != "c" {
		return r.h.decls.lookup(keyFunc+r.fc.family+"\x00"+declScope(r.t, r.fc)+"\x00"+callee, yield)
	}
	// The caller's own scope: its namespaces and enclosing classes.
	from := ""
	if caller >= 0 {
		from = namespaceOf(&r.t.Decls[caller], r.fc.sep)
	}
	return preferDefinitions(func(f func(entry) error) error {
		return r.h.decls.lookup(keyFunc+r.fc.family+"\x00\x00"+callee, func(e entry) error {
			if visible(e.Aux, from) {
				return f(e)
			}
			return nil
		})
	}, yield)
}

// qualified resolves a call of name through qualifier q: a type's static
// method or associated function (Rust's `Self` is the caller's impl type), a
// package's function when q is an import (Go), a namespace's function (C++),
// or a Rust module's function.
func (r *staticFile) qualified(caller int32, q, name string, imported bool, yield func(entry) error) error {
	found := false
	hit := func(e entry) error {
		found = true
		return yield(e)
	}
	if r.fc.language == "rust" && q == "Self" {
		return r.enclosingTypes(caller, func(typ Ref) error { return r.h.declared(typ, name, yield) })
	}
	if r.fc.family == "go" && imported {
		return r.h.decls.lookup(keyFuncQualified+r.fc.family+"\x00"+q+"."+name, yield)
	}
	err := r.h.typesIn(r.ctx, r.fc, q, func(typ entry) error { return r.h.declared(typ.Ref, name, hit) })
	if err != nil || found {
		return err
	}
	switch r.fc.family {
	case "c":
		return preferDefinitions(func(f func(entry) error) error {
			return r.h.decls.lookup(keyFuncQualified+r.fc.family+"\x00"+q+r.fc.sep+name, f)
		}, yield)
	case "rust":
		module := rustPath(r.t.Path, q)
		if err := r.h.decls.lookup(keyFuncModule+module+"\x00"+name, hit); err != nil || found {
			return err
		}
		return r.h.decls.lookup(keyFuncQualified+r.fc.family+"\x00"+q+r.fc.sep+name, yield)
	}
	return nil
}

// member resolves a call of method on a receiver: by class-hierarchy
// analysis over its declared type, along the enclosing class's supertypes
// for `super`, or as a qualified call through a module.
func (r *staticFile) member(caller int32, recv wire.Type, method string, bind func(string) func(entry) error) error {
	switch recv.Kind {
	case wire.TypeNamed:
		return r.h.typesIn(r.ctx, r.fc, recv.Name, func(typ entry) error {
			return r.h.dispatchEntries(r.ctx, typ.Ref, method, bind(DetailHierarchy))
		})
	case wire.TypeSuper:
		return r.h.typesIn(r.ctx, r.fc, recv.Name, func(typ entry) error {
			return r.h.links.lookup(keySuper+refKey(typ.Ref), func(super entry) error {
				return r.h.declared(super.Ref, method, bind(DetailDirect))
			})
		})
	case wire.TypeModule:
		return r.qualified(caller, recv.Name, method, true, bind(DetailDirect))
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
		return r.enclosingTypes(caller, func(typ Ref) error { return r.h.dispatchEntries(r.ctx, typ, last, yield) })
	}
	return r.qualified(caller, q, last, r.fc.family == "go" && r.imports()[q], yield)
}

// imports is the import names this file's static sites record, the only
// import names a table carries.
func (r *staticFile) imports() map[string]bool {
	if r.importNames == nil {
		r.importNames = map[string]bool{}
		for i := range r.t.Sites {
			if s := &r.t.Sites[i]; s.QualifierIsImport {
				r.importNames[s.Qualifier] = true
			}
		}
	}
	return r.importNames
}

// rustPath is the module a Rust path q written in the file at p names: its
// last component, with `self` the file's own module, `super` its parent's
// and `crate` the crate root.
func rustPath(p, q string) string {
	module := lastComponent(q, "::")
	switch module {
	case "self":
		return rustModule(p)
	case "super":
		dir := path.Dir(p)
		if path.Base(p) == "mod.rs" {
			dir = path.Dir(dir)
		}
		return path.Base(dir)
	}
	return module
}

// enclosingTypes yields the types caller is a member of: the type
// declaration enclosing it, or the types its receiver names.
func (r *staticFile) enclosingTypes(caller int32, yield func(Ref) error) error {
	for i := caller; i >= 0; i = r.t.Decls[i].Parent {
		d := &r.t.Decls[i]
		if isType(d.Kind) {
			return yield(Ref{File: r.file, Index: i})
		}
		if d.Receiver != "" {
			return r.h.typesIn(r.ctx, r.fc, d.Receiver, func(e entry) error { return yield(e.Ref) })
		}
	}
	return nil
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
