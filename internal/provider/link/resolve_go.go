package link

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
