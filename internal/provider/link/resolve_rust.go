package link

import (
	"path"
	"slices"
	"strings"

	"github.com/Sawmonabo/codectx/internal/model"
)

// freeRust is free for Rust: the file's own functions, else the function a
// `use` of the name binds, else those a glob `use` brings into scope.
func (r *staticFile) freeRust(callee string, yield func(entry) error) error {
	found := false
	hit := func(e entry) error {
		found = true
		return yield(e)
	}
	if err := r.h.decls.lookup(keyFunc+r.fc.family+"\x00"+declScope(r.t, r.fc)+"\x00"+callee, hit); err != nil || found {
		return err
	}
	imported := false
	err := r.h.imports(r.fc, callee, func(p string) error {
		imported = true
		if module, last, ok := cutLast(p, "::"); ok {
			return r.moduleFunc(rustPath(r.t.Path, module), last, yield)
		}
		return nil
	})
	if err != nil || imported {
		return err
	}
	return r.h.imports(r.fc, ImportAll, func(p string) error {
		return r.moduleFunc(rustPath(r.t.Path, p), callee, yield)
	})
}

// qualifiedRust resolves a Rust `module::f()` path: a path whose first
// component a `use` binds is read from that use's path (`u::f()` after `use
// crate::util as u`), any other through the module it names from this file
// (rustPath). A path that names no module of the crate (another crate's)
// binds nothing.
func (r *staticFile) qualifiedRust(q, name string, yield func(entry) error) error {
	head, rest, ok := strings.Cut(q, "::")
	err := r.h.imports(r.fc, head, func(p string) error {
		q = p
		if ok {
			q += "::" + rest
		}
		return nil
	})
	if err != nil {
		return err
	}
	return r.moduleFunc(rustPath(r.t.Path, q), name, yield)
}

// moduleFunc yields the free functions named name that module declares at
// its top level. A module several crate roots share (rustFile) declares, as
// read from one of them, that file's own function when it has one.
func (r *staticFile) moduleFunc(module, name string, yield func(entry) error) error {
	if module == "" {
		return nil
	}
	var hits []entry
	err := r.h.decls.lookup(keyFuncModule+module+"\x00"+name, func(e entry) error {
		hits = append(hits, e)
		return nil
	})
	if err != nil {
		return err
	}
	if own := slices.DeleteFunc(slices.Clone(hits), func(e entry) bool { return e.Ref.File != r.file }); len(own) > 0 {
		hits = own
	}
	for _, e := range hits {
		if err := yield(e); err != nil {
			return err
		}
	}
	return nil
}

// rustPath is the module a Rust path q written in the file at p names, as
// rustModuleKey spells it, or "" when q names no module of p's crate: `crate`
// is the crate root, `self` the file's module, each `super` one module up,
// and any other first component a module the file's module declares, since
// the callers have already read a first component a `use` binds. A path
// starting with `::`, or with more `super`s than the module is deep, names
// another crate.
func rustPath(p, q string) string {
	crate, module := rustFile(p)
	segs := strings.Split(q, "::")
	switch segs[0] {
	case "":
		return ""
	case "crate":
		module, segs = nil, segs[1:]
	case "self":
		segs = segs[1:]
	}
	for len(segs) > 0 && segs[0] == "super" {
		if len(module) == 0 {
			return ""
		}
		module, segs = module[:len(module)-1], segs[1:]
	}
	return rustModuleKey(crate, append(slices.Clone(module), segs...))
}

// rustDeclModule is the module the declaration d of the Rust table t is an
// item of: its file's module followed by the inline `mod` items enclosing
// it. It is false for a declaration nested in anything but a module (a
// function body, an impl, a trait), which no path names.
func rustDeclModule(t *Table, d *Decl) (string, bool) {
	crate, module := rustFile(t.Path)
	var inline []string
	for p := d.Parent; p >= 0; p = t.Decls[p].Parent {
		if int(p) >= len(t.Decls) || t.Decls[p].Kind != model.NodeModule {
			return "", false
		}
		inline = append(inline, t.Decls[p].Name)
	}
	slices.Reverse(inline)
	return rustModuleKey(crate, append(module, inline...)), true
}

// rustModuleKey spells a Rust module: its crate (rustFile) followed by `::`
// and each component of its module path.
func rustModuleKey(crate string, module []string) string {
	if len(module) == 0 {
		return crate
	}
	return crate + "::" + strings.Join(module, "::")
}

// rustTargetDirs are the directories each of whose files is a crate root of
// its own (Cargo's target auto-discovery): binaries under src/bin, and
// integration tests, examples and benchmarks.
var rustTargetDirs = []string{"tests", "examples", "benches"}

// rustFile is the crate the Rust file at p belongs to, named by its
// directory, and the file's module path inside it, by the layout Cargo
// discovers targets in (the Cargo Book's "Package Layout" and "Target
// auto-discovery") and the module-file rules of the Rust Reference's
// "Modules": in the nearest src directory, lib.rs and main.rs are the crate
// root, a/b.rs is module a::b and a/mod.rs is module a; a file under no src
// directory is read against the nearest tests, examples or benches
// directory. The files directly in src/bin or such a directory are each a
// crate root; they share
// their directory as one crate, whose modules (a tests/common/mod.rs) every
// one of them may declare, and a main.rs below them is read as a mod.rs. A
// file under none of these is a module of the crate its directory is, whose
// root is a lib.rs, main.rs or build.rs there. A crate is never the empty
// string: the repository root's directory is ".".
func rustFile(p string) (crate string, module []string) {
	dir, stem := path.Dir(p), strings.TrimSuffix(path.Base(p), ".rs")
	var dirs []string
	if dir != "." {
		dirs = strings.Split(dir, "/")
	}
	root, targets := -1, false
	for i, d := range dirs {
		if d == "src" {
			root = i
		}
	}
	switch {
	case root >= 0 && root+1 < len(dirs) && dirs[root+1] == "bin":
		root, targets = root+1, true
	case root < 0:
		for i := len(dirs) - 1; i >= 0; i-- {
			if slices.Contains(rustTargetDirs, dirs[i]) {
				root, targets = i, true
				break
			}
		}
	}
	if root < 0 {
		if stem == "lib" || stem == "main" || stem == "build" {
			return dir, nil
		}
		return dir, []string{stem}
	}
	crate = strings.Join(dirs[:root+1], "/")
	module = slices.Clone(dirs[root+1:])
	switch {
	case len(module) == 0 && (targets || stem == "lib" || stem == "main"):
	case stem == "mod" || stem == "main" && targets:
	default:
		module = append(module, stem)
	}
	return crate, module
}
