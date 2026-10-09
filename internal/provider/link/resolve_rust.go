package link

import (
	"path"
	"strings"
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
			return r.h.decls.lookup(keyFuncModule+rustPath(r.t.Path, module)+"\x00"+last, yield)
		}
		return nil
	})
	if err != nil || imported {
		return err
	}
	return r.h.imports(r.fc, ImportAll, func(p string) error {
		return r.h.decls.lookup(keyFuncModule+rustPath(r.t.Path, p)+"\x00"+callee, yield)
	})
}

// qualifiedRust resolves a Rust `module::f()` path: a path whose first
// component a `use` binds is read from that use's path (`u::f()` after `use
// crate::util as u`).
func (r *staticFile) qualifiedRust(q, name string, yield func(entry) error) error {
	found := false
	hit := func(e entry) error {
		found = true
		return yield(e)
	}
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
	module := rustPath(r.t.Path, q)
	if err := r.h.decls.lookup(keyFuncModule+module+"\x00"+name, hit); err != nil || found {
		return err
	}
	return r.h.decls.lookup(keyFuncQualified+r.fc.family+"\x00"+q+r.fc.sep+name, yield)
}

// rustPath is the module a Rust path q written in the file at p names: its
// last component, with `self` the file's own module, `super` its parent's
// and `crate` the crate root.
func rustPath(p, q string) string {
	module := lastComponent(q, "::")
	switch module {
	case "self":
		return rustModule(p)
	case "super":
		dir := path.Dir(p)
		if path.Base(p) == "mod.rs" {
			dir = path.Dir(dir)
		}
		return path.Base(dir)
	}
	return module
}

// rustModule is the module name a Rust file's path gives it: its stem, its
// directory for a mod.rs, and "crate" for a crate root.
func rustModule(p string) string {
	switch stem := strings.TrimSuffix(path.Base(p), ".rs"); stem {
	case "lib", "main":
		return "crate"
	case "mod":
		return path.Base(path.Dir(p))
	default:
		return stem
	}
}
