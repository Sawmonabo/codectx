package link

import "strings"

// freeGo is free for Go: the package's own functions, else those of a
// package a dot import brings into scope.
func (r *staticFile) freeGo(callee string, yield func(entry) error) error {
	found := false
	hit := func(e entry) error {
		found = true
		return yield(e)
	}
	if err := r.h.decls.lookup(keyFunc+r.fc.family+"\x00"+declScope(r.t, r.fc)+"\x00"+callee, hit); err != nil || found {
		return err
	}
	return r.h.imports(r.fc, ImportAll, func(p string) error {
		return r.h.goPackages(p, func(scope string) error {
			return r.h.decls.lookup(keyFunc+r.fc.family+"\x00"+scope+"\x00"+callee, yield)
		})
	})
}

// qualifiedGo resolves `pkg.F()` when q is a name a Go import binds, by the
// import's path; imported reports that one does.
func (r *staticFile) qualifiedGo(q, name string, yield func(entry) error) (imported bool, err error) {
	err = r.h.imports(r.fc, q, func(p string) error {
		imported = true
		return r.h.goPackages(p, func(scope string) error {
			return r.h.decls.lookup(keyFunc+r.fc.family+"\x00"+scope+"\x00"+name, yield)
		})
	})
	return imported, err
}

// goMethodExpression is the signature of a Go method expression taken as a
// value, written "T.M" (or "*T.M" for `(*T).M`), whose method's signature
// is sig: the method's, with the receiver type as its first parameter
// (The Go Programming Language Specification, "Method expressions"), so it
// joins the indirect sites of type `func(T, …)`.
func goMethodExpression(written, sig string) string {
	recv, _, ok := cutLast(written, ".")
	if !ok || !strings.HasPrefix(sig, "(") {
		return sig
	}
	if strings.HasPrefix(sig, "()") {
		return "(" + recv + sig[1:]
	}
	return "(" + recv + "," + sig[1:]
}

// fieldGo yields the types a Go field receiver written "Owner.field" holds:
// for each type Owner denotes in the file, the field a selector of that name
// on it denotes, its own or promoted through embedding (keyGoField), and the
// declarations its declared type names, read in the file that declares the
// field. A field receiver with no owner binds nothing: a field name alone
// names no type.
func (r *staticFile) fieldGo(written string, yield func(Ref) error) error {
	owner, field, ok := cutLast(written, ".")
	if !ok {
		return nil
	}
	return r.h.typesIn(r.ctx, r.fc, owner, func(typ entry) error {
		return r.h.links.lookup(keyGoField+refKey(typ.Ref)+field, func(f entry) error {
			if f.Sig == "" {
				return nil
			}
			return r.h.types(r.ctx, f.Ref.File, f.Sig, yield)
		})
	})
}
