package link

import (
	"path"
	"slices"
	"strings"

	"github.com/Sawmonabo/codectx/internal/pagination"
	"github.com/Sawmonabo/codectx/internal/provider/treesitter/lang"
	"github.com/Sawmonabo/codectx/internal/provider/treesitter/wire"
)

// freeC is free for C and C++. A function of internal linkage that the
// caller's own file declares, else that a header in its include closure
// declares, decides alone: in the translation unit it denotes every use of
// its name, and no other file's function of internal linkage is ever named
// (ISO C 6.2.2, [basic.link]). Otherwise the functions of the one external
// linkage scope whose namespace encloses the caller or a namespace a
// using-directive of the file names are read, together with those of
// internal linkage any other header declares when the closure is open
// (internalC); else the function a using-declaration of the name names. A prototype yields to its definition throughout.
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
// a header in its include closure declares, else those of external linkage
// with those of internal linkage any other header declares when the
// closure is open.
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
// qualified name) that the caller's file declares, else those a header in
// its include closure declares (cClosure): either is in the translation
// unit, and no other file's function of internal linkage ever is. When
// neither declares one and the closure is open (a directive it cannot
// follow), headers are those every other header declares, for the caller to
// read with the functions of external linkage, since the file may reach
// their header through that directive; a closed closure leaves none.
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
	var candidates, included []entry
	err = r.h.decls.lookup(keyFuncInternal+"\x00"+name, func(e entry) error {
		if e.Ref.File != r.file && inScope(e) {
			candidates = append(candidates, e)
		}
		return nil
	})
	if err != nil || len(candidates) == 0 {
		return nil, false, err
	}
	closure := r.includeClosure()
	for _, e := range candidates {
		_, p, _ := strings.Cut(e.Aux, "\x00")
		ok, err := closure.seesFile(e.Ref.File, p)
		if err != nil {
			return nil, false, err
		}
		if ok {
			included = append(included, e)
		} else {
			headers = append(headers, e)
		}
	}
	if len(included) == 0 {
		// Every candidate was read for, so the whole closure is read.
		if open, err := closure.complete(); err != nil || !open {
			return nil, false, err
		}
		return headers, false, nil
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
// header in its include closure (cClosure). The preprocessor replaces the name before
// the call is parsed, so a visible macro decides alone (aliased), and the
// call binds what each definition's identifier names in the caller, as a
// bare call of it would, one binding per definition (a macro defined in
// several `#if` arms binds each arm's). seen holds the macro names already
// replaced along the expansion, which the preprocessor never replaces again
// ([cpp.rescan]).
func (r *staticFile) aliasC(caller int32, name string, yield func(entry) error, seen map[string]bool) (aliased bool, err error) {
	var defs []entry
	if err = r.h.decls.lookup(keyMacro+name, func(e entry) error {
		defs = append(defs, e)
		return nil
	}); err != nil {
		return false, err
	}
	var replacements []string
	for _, e := range defs {
		ok, err := r.includeClosure().sees(e)
		if err != nil {
			return false, err
		}
		if ok {
			aliased = true
			if !slices.Contains(replacements, e.Sig) {
				replacements = append(replacements, e.Sig)
			}
		}
	}
	if !aliased {
		return false, nil
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

// The evidence detail of a C or C++ visibility occurrence: "visibility_" and
// the visibility the macro definition states (wire.Export.Visibility).
const (
	DetailVisibilityDefault     = "visibility_" + wire.VisibilityDefault
	DetailVisibilityHidden      = "visibility_" + wire.VisibilityHidden
	DetailVisibilityProtected   = "visibility_" + wire.VisibilityProtected
	DetailVisibilityInternal    = "visibility_" + wire.VisibilityInternal
	DetailVisibilityDllexport   = "visibility_" + wire.VisibilityDllexport
	DetailVisibilityDllimport   = "visibility_" + wire.VisibilityDllimport
	DetailVisibilityUnspecified = "visibility_" + wire.VisibilityUnspecified
	DetailVisibilityUnresolved  = "visibility_" + wire.VisibilityUnresolved
)

// visibilityFacts emits, for each C or C++ declaration of the file written
// with macros among its specifiers (Decl.Macros), a FactVisibility to each
// definition of each macro that states a visibility and that the file sees:
// its own, and those of every header in its include closure (cClosure), the
// headers the preprocessor reads for it. A one-identifier replacement is
// followed to the definitions of the macro it names, never to one already
// replaced along the expansion ([cpp.rescan]). Every definition reached is a
// fact, so a macro defined in several `#if` arms publishes each arm's
// visibility; a macro the file sees no definition of publishes nothing.
// What a macro reaches is read once per file, and the closure is read only
// as far as the definitions it is asked about need.
func (r *staticFile) visibilityFacts() error {
	if r.fc.family != "c" {
		return nil
	}
	reached := map[string][]Binding{}
	for i := range r.t.Decls {
		d := &r.t.Decls[i]
		if d.Macros == "" {
			continue
		}
		closure := r.includeClosure()
		site := Ref{File: r.file, Index: int32(i)}
		for _, m := range strings.Fields(d.Macros) {
			facts, ok := reached[m]
			if !ok {
				var err error
				if facts, err = r.macroVisibility(m, closure, map[string]bool{}, nil); err != nil {
					return err
				}
				reached[m] = facts
			}
			for _, b := range facts {
				b.Site = site
				if err := r.emit(b); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

// macroVisibility appends to out the FactVisibility facts, with no site, of
// the definitions the macro name reaches (visibilityFacts) in the files of
// closure; seen holds the names already read along the expansion.
func (r *staticFile) macroVisibility(name string, closure *cClosure, seen map[string]bool, out []Binding) ([]Binding, error) {
	if seen[name] {
		return out, nil
	}
	seen[name] = true
	var defs, aliases []entry
	err := r.h.decls.lookup(keyMacroVisibility+name, func(e entry) error {
		if _, ok := visibilityDetail(e.Sig); ok {
			defs = append(defs, e)
		}
		return nil
	})
	if err != nil {
		return out, err
	}
	if err := r.h.decls.lookup(keyMacro+name, func(e entry) error {
		aliases = append(aliases, e)
		return nil
	}); err != nil {
		return out, err
	}
	for _, e := range defs {
		ok, err := closure.sees(e)
		if err != nil {
			return out, err
		}
		if ok {
			detail, _ := visibilityDetail(e.Sig)
			out = append(out, Binding{Kind: FactVisibility, Target: e.Ref, Detail: detail})
		}
	}
	var names []string
	for _, e := range aliases {
		ok, err := closure.sees(e)
		if err != nil {
			return out, err
		}
		if ok && !slices.Contains(names, e.Sig) {
			names = append(names, e.Sig)
		}
	}
	for _, n := range names {
		if out, err = r.macroVisibility(n, closure, seen, out); err != nil {
			return out, err
		}
	}
	return out, nil
}

// visibilityDetail is the evidence detail of the visibility v, or false when
// v is none of the values the worker sends.
func visibilityDetail(v string) (string, bool) {
	d := "visibility_" + v
	switch d {
	case DetailVisibilityDefault, DetailVisibilityHidden, DetailVisibilityProtected, DetailVisibilityInternal,
		DetailVisibilityDllexport, DetailVisibilityDllimport, DetailVisibilityUnspecified, DetailVisibilityUnresolved:
		return d, true
	}
	return "", false
}
