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

// fieldGo yields the types a Go field receiver written "Owner.f1.f2…"
// holds. Owner is the first component, or the first two when the first is a
// name the file's imports bind (`pkg.T`). Each field is read on each type
// the step before it holds: the field a selector of that name denotes on the
// type, its own or promoted through embedding (keyGoField), whose declared
// type is read in the file that declares the field. A step that reaches no
// field or no declared type binds nothing; so does a field name alone, which
// names no owner.
func (r *staticFile) fieldGo(written string, yield func(Ref) error) error {
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
		return r.fieldPathGo(typ.Ref, parts[n:], yield)
	})
}

// fieldPathGo yields the types the field path fields, read from a value of
// the type typ, holds.
func (r *staticFile) fieldPathGo(typ Ref, fields []string, yield func(Ref) error) error {
	return r.h.links.lookup(keyGoField+refKey(typ)+fields[0], func(f entry) error {
		if f.Sig == "" {
			return nil
		}
		return r.h.types(r.ctx, f.Ref.File, f.Sig, func(next Ref) error {
			if len(fields) == 1 {
				return yield(next)
			}
			return r.fieldPathGo(next, fields[1:], yield)
		})
	})
}
