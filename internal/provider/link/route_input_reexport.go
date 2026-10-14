package link

import (
	"cmp"
	"slices"
	"strings"
)

// reexportedClass is the kind of the class a module binds by an import of
// its own: `from app.schemas import User` reads the attribute `User` of the
// package app.schemas, which `app/schemas/__init__.py` binds by `from .user
// import User` (Python Language Reference, "The import statement": a
// from-import binds each name to the attribute the module has). mod is the
// module's absolute name, read from the file at from as an import there
// reads it (exact for a relative import's). The module's nearest files are
// the ones an import's lookup keeps; in each, the name is followed through
// the one module-level import that binds it, or, when none does, through
// the one `from m import *` whose module declares it. A module that
// declares the name itself (a class not recorded, a function, an
// assignment), binds it by several imports, by a whole-module import, or
// by several or no star imports, decides nothing: the kind is unknown. way
// holds the classes and modules the walk is inside, so a cycle of packages
// re-exporting each other ends.
func reexportedClass(h *hierarchy, mods *index, from, mod, name string, exact bool, way []Ref) (uint8, error) {
	if h == nil {
		return classUnknown, nil
	}
	var files []pyRecord
	near := false
	err := mods.lookup(keyPyModule+mod+"\x00", func(e entry) error {
		d := pyModuleRecord(e)
		files = append(files, d)
		near = near || hitNear(from, d.Root, exact)
		return nil
	})
	if err != nil {
		return classUnknown, err
	}
	out, found := classUnknown, false
	for _, f := range files {
		if !(hitNear(from, f.Root, exact) || !near && !exact) {
			continue
		}
		if slices.Contains(way, f.Ref) {
			return classUnknown, nil
		}
		kind, err := moduleBinding(h, mods, f.Ref.File, mod, name, append(slices.Clone(way), f.Ref))
		if err != nil || kind == classUnknown {
			return classUnknown, err
		}
		if found && kind != out {
			return classUnknown, nil
		}
		out, found = kind, true
	}
	return out, nil
}

// moduleBinding is the kind of the class the member at ordinal file, module
// mod, binds to name by an import (reexportedClass, pyReexport).
func moduleBinding(h *hierarchy, mods *index, file int32, mod, name string, way []Ref) (uint8, error) {
	r, ok, err := pyReexport(h, mods, file, mod, name)
	if err != nil || !ok || r.whole {
		return classUnknown, err
	}
	return classOf(h, mods, r.at, r.module+"\x00"+r.member, way)
}

// pyBinding is what a module binds a name to by an import of its own: the
// module as the import writes it, read from the module's file at, and the
// name in it; whole for a whole-module import (`import a.b as name`),
// which binds the module itself.
type pyBinding struct {
	at, module, member string
	whole              bool
}

// pyReexport is what the member at ordinal file, module mod, binds name to
// by an import (Python Language Reference, The import statement: a
// from-import binds each name to the attribute the module has), and false
// when no import decides it: the module declares the name itself, binds it
// by several module-level imports, or binds it by none and its `from m
// import *` statements supply it from several modules or none. A star
// import supplies the name when its module declares it.
func pyReexport(h *hierarchy, mods *index, file int32, mod, name string) (pyBinding, bool, error) {
	declared := false
	err := mods.lookup(keyPyModule+mod+"\x00"+name, func(e entry) error {
		declared = declared || e.Ref.File == file
		return nil
	})
	if err != nil || declared {
		return pyBinding{}, false, err
	}
	at := ""
	if err := h.decls.lookup(keyFile+refKey(Ref{File: file}), func(e entry) error {
		_, at, _ = strings.Cut(e.Aux, "\x00")
		return nil
	}); err != nil || at == "" {
		return pyBinding{}, false, err
	}
	var named []entry
	err = h.decls.lookup(keyImport+refKey(Ref{File: file})+name, func(e entry) error {
		if e.Ref.Index < 0 {
			named = append(named, e)
		}
		return nil
	})
	if err != nil {
		return pyBinding{}, false, err
	}
	switch {
	case len(named) == 1 && named[0].Sig == "*":
		return pyBinding{at: at, module: named[0].Aux, whole: true}, true, nil
	case len(named) == 1:
		return pyBinding{at: at, module: named[0].Aux, member: cmp.Or(named[0].Sig, name)}, true, nil
	case len(named) > 0:
		return pyBinding{}, false, nil
	}
	var supplying []string
	err = h.decls.lookup(keyImport+refKey(Ref{File: file})+ImportAll, func(e entry) error {
		if e.Ref.Index >= 0 {
			return nil
		}
		star, ok := pyAbsolute(at, e.Aux)
		if !ok {
			return nil
		}
		hit := false
		if err := mods.lookup(keyPyModule+star+"\x00"+name, func(entry) error {
			hit = true
			return nil
		}); err != nil {
			return err
		}
		if hit && !slices.Contains(supplying, e.Aux) {
			supplying = append(supplying, e.Aux)
		}
		return nil
	})
	if err != nil || len(supplying) != 1 {
		return pyBinding{}, false, err
	}
	return pyBinding{at: at, module: supplying[0], member: name}, true, nil
}

// pyAbsolute is module as an import in the file at writes it, named from
// the repository root when it is relative, and false when it climbs above
// the root or names nothing.
func pyAbsolute(at, module string) (string, bool) {
	if strings.HasPrefix(module, ".") {
		if !pythonRelativeFits(at, module) {
			return "", false
		}
		module = absolutePythonModule(at, module)
	}
	return module, module != ""
}
