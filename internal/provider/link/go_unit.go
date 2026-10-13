package link

import (
	"context"
	"go/token"
	"slices"
	"strings"
)

// The keys of the Go identity and external-interface entries.
const (
	// keyGoAlias + scope + "\x00" + name: a Go alias declaration of that
	// name in that package scope, the form of the type it denotes in Sig
	// (wire.Export.Identity past its "=").
	keyGoAlias = "ga\x00"
	// keyGoConst + scope + "\x00" + name: a package-level Go constant of
	// that name in that package scope, the form of its integer value in Sig
	// (wire.Export.Identity past its "#").
	keyGoConst = "gk\x00"
	// keyGoExternalImpl + refKey(type): an interface outside the unit the
	// type implements, its qualified name in Aux and the evidence detail in
	// Sig: DetailMethodSet when its method set satisfies the interface's
	// (goExternal), DetailSupertypeInterface when a conversion proves it
	// (goConversions).
	keyGoExternalImpl = "gx\x00"
	// keyGoExternalOverride + refKey(type) + name: the interface outside the
	// unit, qualified in Aux, whose method of that name the type's method of
	// that name implements.
	keyGoExternalOverride = "gy\x00"
	// keyGoCoverage: the unit's count of packages outside it read
	// (MinArgs) and of those with no readable source (MaxArgs), Tag 0; and
	// of its methods whose signature names a type by its unresolved
	// spelling (MinArgs) and of those whose signature could not be spelled
	// (MaxArgs), Tag 1.
	keyGoCoverage = "gc\x00"
	// goImportList, past keyImport + refKey(file), lists every named import
	// of a Go file, its path in Aux and its name in Sig; no import's name
	// is spelled with it.
	goImportList = "\x01"
)

// goTypes is goPackageTypes over the unit's packages and the packages
// outside it the build read.
func (h *hierarchy) goTypes() goPackageTypes { return goUnitTypes{h} }

type goUnitTypes struct{ h *hierarchy }

func (t goUnitTypes) lookup(ctx context.Context, p, name string) (goTypeAnswer, bool, error) {
	h := t.h
	if !h.goMemoOn() {
		return t.read(p, name)
	}
	key := p + "\x00" + name
	if m, ok := h.goMemo.types.get(key); ok {
		return m.answer, m.known, nil
	}
	a, known, err := t.read(p, name)
	if err == nil {
		h.goMemo.types.put(key, goTypeMemo{a, known})
	}
	return a, known, err
}

// read is lookup's answer read from the indexes.
func (t goUnitTypes) read(p, name string) (goTypeAnswer, bool, error) {
	h := t.h
	var scopes []string
	if err := h.goPackages(p, func(scope string) error {
		scopes = append(scopes, scope)
		return nil
	}); err != nil {
		return goTypeAnswer{}, false, err
	}
	if len(scopes) == 0 && !(h.goModule != "" && under(p, h.goModule)) {
		if h.goExt == nil {
			return goTypeAnswer{}, false, nil
		}
		a, known := h.goExt.lookup(p, name)
		return a, known, nil
	}
	var answer goTypeAnswer
	for _, scope := range scopes {
		// A package's external test package (`package p_test`) shares its
		// directory and import path prefix but is a package of its own.
		if strings.HasSuffix(scope, "_test") != strings.HasSuffix(p, "_test") {
			continue
		}
		// An alias declared once per platform or build tag is each of its
		// distinctly written declarations (goTypeAnswer.others).
		var aliases []entry
		err := h.decls.lookup(keyGoAlias+scope+"\x00"+name, func(e entry) error {
			if !slices.ContainsFunc(aliases, func(a entry) bool { return a.Sig == e.Sig }) {
				aliases = append(aliases, e)
			}
			return nil
		})
		if err != nil {
			return goTypeAnswer{}, false, err
		}
		if len(aliases) > 0 {
			decls, err := t.decls(aliases)
			if err != nil {
				return goTypeAnswer{}, false, err
			}
			return goTypeAnswer{found: true, alias: true, form: decls[0].form, names: decls[0].names, others: decls[1:]}, true, nil
		}
		err = h.decls.lookup(keyType+"go\x00"+scope+"\x00"+name, func(entry) error {
			answer.found = true
			return nil
		})
		if err != nil {
			return goTypeAnswer{}, false, err
		}
	}
	return answer, true, nil
}

func (t goUnitTypes) constant(ctx context.Context, p, name string) ([]goDecl, error) {
	h := t.h
	if !h.goMemoOn() {
		return t.readConstant(p, name)
	}
	key := p + "\x00" + name
	if m, ok := h.goMemo.consts.get(key); ok {
		return m, nil
	}
	decls, err := t.readConstant(p, name)
	if err == nil {
		h.goMemo.consts.put(key, decls)
	}
	return decls, err
}

// readConstant is constant's answer read from the indexes.
func (t goUnitTypes) readConstant(p, name string) ([]goDecl, error) {
	h := t.h
	var scopes []string
	if err := h.goPackages(p, func(scope string) error {
		scopes = append(scopes, scope)
		return nil
	}); err != nil {
		return nil, err
	}
	if len(scopes) == 0 && !(h.goModule != "" && under(p, h.goModule)) {
		if h.goExt == nil {
			return nil, nil
		}
		return h.goExt.constant(p, name), nil
	}
	var cs []entry
	for _, scope := range scopes {
		if strings.HasSuffix(scope, "_test") != strings.HasSuffix(p, "_test") {
			continue
		}
		err := h.decls.lookup(keyGoConst+scope+"\x00"+name, func(e entry) error {
			if !slices.ContainsFunc(cs, func(c entry) bool { return c.Sig == e.Sig }) {
				cs = append(cs, e)
			}
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	return t.decls(cs)
}

// decls are the forms (Sig) of the declarations es, each written in its
// own file; a form whose file cannot be read is unknown.
func (t goUnitTypes) decls(es []entry) ([]goDecl, error) {
	var out []goDecl
	for _, e := range es {
		fc, found, err := t.h.fileContext(e.Ref.File)
		if err != nil {
			return nil, err
		}
		d := goDecl{form: e.Sig, names: goUnitNames{h: t.h, fc: fc}}
		if !found {
			d.form = goIdentityUnknown
		}
		out = append(out, d)
	}
	return out, nil
}

// goUnitNames is goNames of a member file of the unit.
type goUnitNames struct {
	h  *hierarchy
	fc fileCtx
}

func (n goUnitNames) pkg() string {
	dir, pkg, _ := strings.Cut(n.fc.scopes[0], "\x00")
	return goImportPath(n.h.goModule, n.h.goRoot, dir, pkg)
}

// imported is the import path the file's import of package name name binds.
// A table names an import written without a name by its path's last element
// (the worker's convention), while the language names it by the imported
// package's clause: an import whose clause is read (a package of the unit,
// or one outside it the build read) binds that clause's name instead, when
// its table name is only the convention's.
func (n goUnitNames) imported(name string) (string, bool, error) {
	var paths []string
	add := func(p string) {
		if !slices.Contains(paths, p) {
			paths = append(paths, p)
		}
	}
	err := n.h.imports(n.fc, name, func(p string) error {
		if clause, ok, err := n.h.goClause(p); err != nil {
			return err
		} else if ok && clause != name && name == goPackageName(p) {
			return nil
		}
		add(p)
		return nil
	})
	if err != nil {
		return "", false, err
	}
	imports, err := n.imports()
	if err != nil {
		return "", false, err
	}
	for _, imp := range imports {
		if imp.Name != goPackageName(imp.Path) || imp.Name == name {
			continue
		}
		if clause, ok, err := n.h.goClause(imp.Path); err != nil {
			return "", false, err
		} else if ok && clause == name {
			add(imp.Path)
		}
	}
	if len(paths) != 1 || paths[0] == "C" {
		return "", false, nil
	}
	return paths[0], true, nil
}

// imports are the file's imports: its context's, or, for a context read
// back from the index (an alias's file), the ones the index lists for the
// file (keyImport + goImportList).
func (n goUnitNames) imports() ([]Import, error) {
	if n.fc.imports != nil {
		return n.fc.imports, nil
	}
	var out []Import
	err := n.h.decls.lookup(keyImport+refKey(Ref{File: n.fc.file})+goImportList, func(e entry) error {
		out = append(out, Import{Name: e.Sig, Path: e.Aux})
		return nil
	})
	return out, err
}

func (n goUnitNames) unresolved() ([]string, error) {
	imports, err := n.imports()
	if err != nil {
		return nil, err
	}
	var out []string
	for _, imp := range imports {
		if imp.Path == "C" || imp.Name != goPackageName(imp.Path) || slices.Contains(out, imp.Path) {
			continue
		}
		if _, ok, err := n.h.goClause(imp.Path); err != nil {
			return nil, err
		} else if !ok {
			out = append(out, imp.Path)
		}
	}
	slices.Sort(out)
	return out, nil
}

func (n goUnitNames) dots() ([]string, error) {
	var out []string
	err := n.h.imports(n.fc, ImportAll, func(p string) error {
		if !slices.Contains(out, p) {
			out = append(out, p)
		}
		return nil
	})
	return out, err
}

// goClause is the package clause of the package at import path p, when the
// unit holds it or the build read it.
func (h *hierarchy) goClause(p string) (string, bool, error) {
	if !h.goMemoOn() {
		return h.readClause(p)
	}
	if m, ok := h.goMemo.clause.get(p); ok {
		return m.clause, m.ok, nil
	}
	clause, ok, err := h.readClause(p)
	if err == nil {
		h.goMemo.clause.put(p, goClauseMemo{clause, ok})
	}
	return clause, ok, err
}

// readClause is goClause's answer read from the indexes.
func (h *hierarchy) readClause(p string) (string, bool, error) {
	var names []string
	err := h.goPackages(p, func(scope string) error {
		if _, pkg, _ := strings.Cut(scope, "\x00"); !strings.HasSuffix(pkg, "_test") {
			names = append(names, pkg)
		}
		return nil
	})
	if err != nil {
		return "", false, err
	}
	if len(names) == 1 {
		return names[0], true, nil
	}
	if len(names) == 0 && h.goExt != nil {
		if pkg := h.goExt.pkgs[p]; pkg != nil {
			return pkg.name, true, nil
		}
	}
	return "", false, nil
}

// inUnit reports an import path that names a package of the unit, or a
// directory below its module path that holds none.
func (h *hierarchy) inUnit(p string) (bool, error) {
	if h.goModule != "" && under(p, h.goModule) {
		return true, nil
	}
	found := false
	err := h.goPackages(p, func(string) error {
		found = true
		return nil
	})
	return found, err
}

// goMethodKey is the method-set key (satisfy) of the method named name of
// identity form form, declared at ref in the file n: its canonical
// signature (goIdentity), and for an unexported name its package's import
// path, since a non-exported name is unique to its package ("Uniqueness of
// identifiers"). spelling says how exact the signature is.
func goMethodKey(ctx context.Context, types goPackageTypes, n goNames, name, form string, ref Ref) (key string, spelling goSpelling, err error) {
	id, err := goIdentity(ctx, types, n, form)
	if err != nil {
		return "", 0, err
	}
	switch {
	case id == goIdentityUnknown:
		spelling = goSpellingUnknown
	case goUnresolvedIdentity(id):
		spelling = goSpellingUnresolved
	}
	if !token.IsExported(name) {
		name = n.pkg() + "." + name
	}
	return goIdentityKey(name, id, ref), spelling, nil
}

// goSpelling is how exactly a method's canonical signature names its types.
type goSpelling uint8

const (
	// goSpellingExact names every type by its package and name.
	goSpellingExact goSpelling = iota
	// goSpellingUnresolved names a type by its unresolved spelling
	// (goUnboundQualifier, goUnqualified).
	goSpellingUnresolved
	// goSpellingUnknown is a signature goIdentity could not spell, whose
	// key matches no other method's.
	goSpellingUnknown
)
