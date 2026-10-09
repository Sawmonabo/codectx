package link

import (
	"path"
	"slices"
	"strings"

	"github.com/Sawmonabo/codectx/internal/pagination"
	"github.com/Sawmonabo/codectx/internal/provider/treesitter/lang"
)

// freeC is free for C and C++. A function of internal linkage that the
// caller's own file declares, else that a header the file includes
// declares, decides alone: in the translation unit it denotes every use of
// its name, and no other file's function of internal linkage is ever named
// (ISO C 6.2.2, [basic.link]). Otherwise the functions of the one external
// linkage scope whose namespace encloses the caller or a namespace a
// using-directive of the file names are read together with those of
// internal linkage any other header declares, which the file may include
// through a header it includes; else the function a using-declaration of
// the name names. A prototype yields to its definition throughout.
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
	seen := func(ns string) bool {
		return slices.ContainsFunc(from, func(f string) bool { return visible(ns, f) })
	}
	headers, decided, err := r.internalC(callee, seen, yield)
	if err != nil || decided {
		return err
	}
	found := false
	err = preferDefinitions(func(f func(entry) error) error {
		for _, e := range headers {
			if err := f(e); err != nil {
				return err
			}
		}
		return r.h.decls.lookup(keyFunc+r.fc.family+"\x00\x00"+callee, func(e entry) error {
			if seen(e.Aux) {
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
	return r.h.imports(r.fc, callee, func(p string) error { return r.qualifiedName(p, yield) })
}

// qualifiedC resolves `ns::f()` in C++ through the namespace's qualified
// name, as freeC reads a simple name.
func (r *staticFile) qualifiedC(q, name string, yield func(entry) error) error {
	return r.qualifiedName(q+r.fc.sep+name, yield)
}

// qualifiedName yields the functions the qualified name q denotes, read as
// freeC reads a simple name: those of internal linkage the caller's file or
// a header it includes declares, else those of external linkage with those
// of internal linkage any other header declares.
func (r *staticFile) qualifiedName(q string, yield func(entry) error) error {
	headers, decided, err := r.internalC(q, func(string) bool { return true }, yield)
	if err != nil || decided {
		return err
	}
	return preferDefinitions(func(f func(entry) error) error {
		for _, e := range headers {
			if err := f(e); err != nil {
				return err
			}
		}
		return r.h.decls.lookup(keyFuncQualified+r.fc.family+"\x00"+q, f)
	}, yield)
}

// internalC yields, and reports decided for, the functions of internal
// linkage named name (a simple name in a namespace keep accepts, or a
// qualified name) that the caller's file declares, else those a header it
// includes declares. When neither declares one, headers are those every
// other header declares, for the caller to read with the functions of
// external linkage: the file may include their header through another, and
// no table says which.
func (r *staticFile) internalC(name string, keep func(ns string) bool, yield func(entry) error) (headers []entry, decided bool, err error) {
	inScope := func(e entry) bool {
		ns, _, _ := strings.Cut(e.Aux, "\x00")
		return keep(ns)
	}
	err = preferDefinitions(func(f func(entry) error) error {
		return r.h.decls.lookup(keyFuncInternal+r.fc.path+"\x00"+name, func(e entry) error {
			if inScope(e) {
				return f(e)
			}
			return nil
		})
	}, func(e entry) error {
		decided = true
		return yield(e)
	})
	if err != nil || decided {
		return nil, decided, err
	}
	var included []entry
	err = r.h.decls.lookup(keyFuncInternal+"\x00"+name, func(e entry) error {
		if e.Ref.File == r.file || !inScope(e) {
			return nil
		}
		if _, p, _ := strings.Cut(e.Aux, "\x00"); r.includes(p) {
			included = append(included, e)
		} else {
			headers = append(headers, e)
		}
		return nil
	})
	if err != nil || len(included) == 0 {
		return headers, false, err
	}
	err = preferDefinitions(func(f func(entry) error) error {
		for _, e := range included {
			if err := f(e); err != nil {
				return err
			}
		}
		return nil
	}, yield)
	return nil, true, err
}

// includes reports whether an `#include` of the caller's file names the
// file at path p: `"h"` the file beside it or any file whose path ends in h
// at a component boundary, `<h>` the latter, as an include directory would
// supply it.
func (r *staticFile) includes(p string) bool {
	for _, imp := range r.t.Imports {
		if imp.Name != "" {
			continue
		}
		h, angled := strings.CutPrefix(imp.Path, "<")
		h = path.Clean(strings.TrimSuffix(h, ">"))
		if !angled && path.Join(path.Dir(r.fc.path), h) == p {
			return true
		}
		if !strings.HasPrefix(h, "../") && (p == h || strings.HasSuffix(p, "/"+h)) {
			return true
		}
	}
	return false
}

// addInternal adds the C or C++ function d of t, which has internal
// linkage, under its own file's scope and, when t is a header, under the
// scope of the files including it (keyFuncInternal).
func addInternal(s *pagination.ExternalSort[entry], t *Table, d *Decl, e entry) error {
	scopes := []string{t.Path}
	if l, ok := lang.Lookup(t.Language); ok && slices.Contains(l.Headers, path.Ext(t.Path)) {
		scopes = append(scopes, "")
	}
	for _, scope := range scopes {
		prefix := keyFuncInternal + scope + "\x00"
		e.Key = prefix + d.Name
		if err := s.Add(e); err != nil {
			return err
		}
		var err error
		qualifiedSuffixes(d.Qualified, separatorOf(t.Language), func(q string) {
			if err == nil {
				e.Key = prefix + q
				err = s.Add(e)
			}
		})
		if err != nil {
			return err
		}
	}
	return nil
}
