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

// aliasC yields what a call of name in caller binds through the C or C++
// object-like macros of that name whose replacement is one identifier
// (Decl.Alias) and that the caller's file sees: its own, and those of a
// header it includes (includes). The preprocessor replaces the name before
// the call is parsed, so a visible macro decides alone (aliased), and the
// call binds what each definition's identifier names in the caller, as a
// bare call of it would, one binding per definition (a macro defined in
// several `#if` arms binds each arm's). seen holds the macro names already
// replaced along the expansion, which the preprocessor never replaces again
// ([cpp.rescan]).
func (r *staticFile) aliasC(caller int32, name string, yield func(entry) error, seen map[string]bool) (aliased bool, err error) {
	var replacements []string
	err = r.h.decls.lookup(keyMacro+name, func(e entry) error {
		if e.Ref.File == r.file || r.includes(e.Aux) {
			aliased = true
			if !slices.Contains(replacements, e.Sig) {
				replacements = append(replacements, e.Sig)
			}
		}
		return nil
	})
	if err != nil || !aliased {
		return aliased, err
	}
	if seen == nil {
		seen = map[string]bool{}
	}
	seen[name] = true
	for _, id := range replacements {
		if seen[id] {
			continue
		}
		again, err := r.aliasC(caller, id, yield, seen)
		if err != nil {
			return true, err
		}
		if !again {
			if err := r.free(caller, id, yield); err != nil {
				return true, err
			}
		}
	}
	return true, nil
}

// includeFacts emits a FactImport for each quoted `#include` of a C or C++
// member to every member file it may name, as the include lookup reads it
// (includes): the file beside the including one, and every file whose path
// ends in the header's at a component boundary, each once. An angled
// include names a header an include directory supplies, which the unit
// does not decide, and one that names no member file publishes nothing.
func (r *staticFile) includeFacts() error {
	if r.fc.family != "c" {
		return nil
	}
	for i, imp := range r.t.Imports {
		if imp.Name != "" || strings.HasPrefix(imp.Path, "<") || imp.Path == "" {
			continue
		}
		h := path.Clean(imp.Path)
		var files []int32
		add := func(file int32) {
			if file != r.file && !slices.Contains(files, file) {
				files = append(files, file)
			}
		}
		beside := path.Join(path.Dir(r.fc.path), h)
		err := r.h.decls.lookup(keyPath+beside, func(e entry) error {
			fc, ok, err := r.h.fileContext(e.Ref.File)
			if err == nil && ok && fc.path == beside {
				add(e.Ref.File)
			}
			return err
		})
		if err != nil {
			return err
		}
		if !strings.HasPrefix(h, "../") {
			if err := r.h.decls.lookup(keyPath+h, func(e entry) error {
				add(e.Ref.File)
				return nil
			}); err != nil {
				return err
			}
		}
		for _, file := range files {
			err := r.emit(Binding{Kind: FactImport, Site: Ref{File: r.file, Index: int32(i)}, Target: Ref{File: file, Index: -1},
				Detail: DetailImport})
			if err != nil {
				return err
			}
		}
	}
	return nil
}
