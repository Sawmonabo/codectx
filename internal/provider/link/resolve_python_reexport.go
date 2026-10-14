package link

import (
	"cmp"
	"slices"
	"strings"

	"github.com/Sawmonabo/codectx/internal/pagination"
)

// lookPythonFollowing answers the lookup r from the module index as
// lookPython does and, when the module it names declares nothing under the
// name, follows the import the module binds the name's first component by
// (pyReexport): `from pkg import User` where `pkg/__init__.py` binds User
// by `from .user import User` reaches the class pkg.user declares, as
// Python's from-import reads the package's attribute (Python Language
// Reference, The import statement). A from-import's name is looked up as a
// declaration of its module, else as its submodule; a whole-module import
// names the module itself. The hits it finds are near when the module file
// that re-exports them is, so the lookup keeps the module it named. A
// module file already on the way ends the walk; a probe, which asks only
// whether a module is the unit's, follows nothing. Each step is one point
// lookup, made only after a lookup met nothing.
func lookPythonFollowing(h *hierarchy, mods *index, r pyRecord, hits *pagination.ExternalSort[pyHit]) error {
	before := hits.Len()
	if err := lookPython(mods, r, hits); err != nil || hits.Len() > before || r.Probe {
		return err
	}
	_, err := followPython(h, mods, r, r.Key, nil, nil, hits)
	return err
}

// lookPythonKey answers r's lookup under key, its hits near as forced says,
// and reports how many it found, following a re-export when it finds none.
func lookPythonKey(h *hierarchy, mods *index, r pyRecord, key string, forced bool, way []Ref,
	hits *pagination.ExternalSort[pyHit]) (int, error) {
	group := hitSite
	if r.Import {
		group = hitImport
	}
	n := 0
	err := mods.lookup(keyPyModule+key, func(e entry) error {
		d := pyModuleRecord(e)
		n++
		return hits.Add(pyHit{Group: group, Site: r.Ref, Target: d.Ref, Init: r.Init, Class: d.Class, Near: forced,
			Exact: r.Exact, Unbound: d.Unbound, Probe: r.Probe, Rank: r.Rank})
	})
	if err != nil || n > 0 {
		return n, err
	}
	return followPython(h, mods, r, key, &forced, way, hits)
}

// followPython follows the import the module key names binds its name's
// first component by, the hits near as forced says when it is set, else as
// the re-exporting module file is to r's file; it reports how many hits it
// found.
func followPython(h *hierarchy, mods *index, r pyRecord, key string, forced *bool, way []Ref,
	hits *pagination.ExternalSort[pyHit]) (int, error) {
	nearOf := func(root string) bool {
		if forced != nil {
			return *forced
		}
		if r.Exact {
			return root == ""
		}
		return strings.HasPrefix(r.Root, root)
	}
	n := 0
	mod, qualified, _ := strings.Cut(key, "\x00")
	if qualified == "" {
		return 0, nil
	}
	head, rest, _ := strings.Cut(qualified, ".")
	var files []pyRecord
	if err := mods.lookup(keyPyModule+mod+"\x00", func(e entry) error {
		files = append(files, pyModuleRecord(e))
		return nil
	}); err != nil {
		return 0, err
	}
	for _, f := range files {
		if slices.Contains(way, f.Ref) {
			continue
		}
		b, ok, err := pyReexport(h, mods, f.Ref.File, mod, head)
		if err != nil {
			return n, err
		}
		if !ok {
			continue
		}
		target, ok := pyAbsolute(b.at, b.module)
		if !ok {
			continue
		}
		near := nearOf(f.Root)
		next := append(slices.Clone(way), f.Ref)
		if b.whole {
			m, err := lookPythonKey(h, mods, r, target+"\x00"+rest, near, next, hits)
			if err != nil {
				return n, err
			}
			n += m
			continue
		}
		member := b.member
		if rest != "" {
			member += "." + rest
		}
		m, err := lookPythonKey(h, mods, r, target+"\x00"+member, near, next, hits)
		if err != nil {
			return n, err
		}
		if m == 0 {
			// `from . import views` binds the submodule when the package
			// declares no `views`.
			sub := target + "." + b.member
			if target == "" {
				sub = b.member
			}
			if m, err = lookPythonKey(h, mods, r, sub+"\x00"+rest, near, next, hits); err != nil {
				return n, err
			}
		}
		n += m
	}
	return n, nil
}

// addPythonReexports adds, under each module path bases names a Python
// member by, what the module binds by its module-level from-imports, as an
// ECMAScript module's re-exports are indexed (keyReexport), so a type
// lookup through the module (moduleType, resolveExport) reaches the class
// `from .user import User` binds in a package's `__init__.py`: a name one
// from-import binds, which the module declares no type or callable of, is a
// re-export of that name of the imported module; each `from m import *` is
// a star re-export of m, which resolveExport follows when the star imports
// agree. A name several imports bind, or a whole-module import
// (`import a.b as c`), is not indexed: which binding holds is not decided
// by the index.
func addPythonReexports(s *pagination.ExternalSort[entry], t *Table, bases []string) error {
	add := func(name string, e entry) error {
		for _, b := range bases {
			e.Key = keyReexport + b + "\x00" + name
			if err := s.Add(e); err != nil {
				return err
			}
		}
		return nil
	}
	declared := map[string]bool{}
	for i := range t.Decls {
		if d := &t.Decls[i]; d.Parent < 0 && (isType(d.Kind) || isCallable(d.Kind)) {
			declared[d.Name] = true
		}
	}
	bound := map[string]int{}
	for _, imp := range t.Imports {
		if imp.Scope < 0 && imp.Name != "" {
			bound[imp.Name]++
		}
	}
	for _, imp := range t.Imports {
		if imp.Scope >= 0 || imp.Name == "" || imp.Original == "*" {
			continue
		}
		target, ok := moduleBase(t.Path, imp.Path, true)
		if !ok {
			continue
		}
		if imp.Name == ImportAll {
			if err := add("*", entry{Aux: target, Sig: "*"}); err != nil {
				return err
			}
			continue
		}
		if declared[imp.Name] || bound[imp.Name] != 1 {
			continue
		}
		if err := add(imp.Name, entry{Aux: target, Sig: cmp.Or(imp.Original, imp.Name)}); err != nil {
			return err
		}
	}
	return nil
}
