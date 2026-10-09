package link

import "slices"

// freeC is free for C and C++: the functions of the one linkage scope whose
// namespace encloses the caller or a namespace a using-directive of the
// file names, a prototype yielding to its definition; else the function a
// using-declaration of the name names.
func (r *staticFile) freeC(caller int32, callee string, yield func(entry) error) error {
	from := []string{""}
	if caller >= 0 {
		from[0] = namespaceOf(&r.t.Decls[caller], r.fc.sep)
	}
	for _, imp := range r.t.Imports {
		if imp.Name == ImportAll {
			from = append(from, imp.Path)
		}
	}
	found := false
	err := preferDefinitions(func(f func(entry) error) error {
		return r.h.decls.lookup(keyFunc+r.fc.family+"\x00\x00"+callee, func(e entry) error {
			if slices.ContainsFunc(from, func(ns string) bool { return visible(e.Aux, ns) }) {
				return f(e)
			}
			return nil
		})
	}, func(e entry) error {
		found = true
		return yield(e)
	})
	if err != nil || found {
		return err
	}
	return r.h.imports(r.fc, callee, func(p string) error {
		return preferDefinitions(func(f func(entry) error) error {
			return r.h.decls.lookup(keyFuncQualified+r.fc.family+"\x00"+p, f)
		}, yield)
	})
}

// qualifiedC resolves `ns::f()` in C++ through the namespace's qualified
// name, a prototype yielding to its definition.
func (r *staticFile) qualifiedC(q, name string, yield func(entry) error) error {
	return preferDefinitions(func(f func(entry) error) error {
		return r.h.decls.lookup(keyFuncQualified+r.fc.family+"\x00"+q+r.fc.sep+name, f)
	}, yield)
}
