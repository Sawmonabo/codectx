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
	// (MinArgs) and of those with no readable source (MaxArgs).
	keyGoCoverage = "gc\x00"
)

// goTypes is goPackageTypes over the unit's packages and the packages
// outside it the build read.
func (h *hierarchy) goTypes() goPackageTypes { return goUnitTypes{h} }

type goUnitTypes struct{ h *hierarchy }

func (t goUnitTypes) lookup(ctx context.Context, p, name string) (goTypeAnswer, bool, error) {
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
		var alias *entry
		err := h.decls.lookup(keyGoAlias+scope+"\x00"+name, func(e entry) error {
			if alias == nil {
				alias = &e
			} else if alias.Sig != e.Sig {
				alias.Sig = goIdentityUnknown
			}
			return nil
		})
		if err != nil {
			return goTypeAnswer{}, false, err
		}
		if alias != nil {
			fc, found, err := h.fileContext(alias.Ref.File)
			if err != nil {
				return goTypeAnswer{}, false, err
			}
			if !found {
				return goTypeAnswer{found: true, alias: true, form: goIdentityUnknown, names: goUnitNames{h: h, fc: fc}}, true, nil
			}
			return goTypeAnswer{found: true, alias: true, form: alias.Sig, names: goUnitNames{h: h, fc: fc}}, true, nil
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
	for _, imp := range n.fc.imports {
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
// identifiers").
func goMethodKey(ctx context.Context, types goPackageTypes, n goNames, name, form string, ref Ref) (string, error) {
	id, err := goIdentity(ctx, types, n, form)
	if err != nil {
		return "", err
	}
	if !token.IsExported(name) {
		name = n.pkg() + "." + name
	}
	return goIdentityKey(name, id, ref), nil
}
