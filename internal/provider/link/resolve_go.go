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
// import's path; imported reports that one does, and inUnit that its path
// names a package of the unit.
func (r *staticFile) qualifiedGo(q, name string, yield func(entry) error) (imported, inUnit bool, err error) {
	err = r.h.imports(r.fc, q, func(p string) error {
		imported = true
		return r.h.goPackages(p, func(scope string) error {
			inUnit = true
			return r.h.decls.lookup(keyFunc+r.fc.family+"\x00"+scope+"\x00"+name, yield)
		})
	})
	return imported, inUnit, err
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

// fieldGo calls each with the type a Go field receiver written
// "Owner.f1.f2…" holds, as the file declaring its last field writes it, in
// that file's context. Owner is the first component, or the first two when
// the first is a name the file's imports bind (`pkg.T`). Each field is read
// on each type the step before it holds (fieldPath). A step that reaches no
// field or no declared type binds nothing; so does a field name alone, which
// names no owner.
func (r *staticFile) fieldGo(written string, each func(fc fileCtx, written string) error) error {
	parts := strings.Split(written, ".")
	n := 1
	if len(parts) > 2 {
		err := r.h.imports(r.fc, parts[0], func(string) error {
			n = 2
			return nil
		})
		if err != nil {
			return err
		}
	}
	if len(parts) <= n {
		return nil
	}
	return r.h.typesIn(r.ctx, r.fc, strings.Join(parts[:n], "."), func(typ entry) error {
		return r.fieldPath(typ.Ref, parts[n:], each)
	})
}
