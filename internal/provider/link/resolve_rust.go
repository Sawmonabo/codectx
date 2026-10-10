package link

import (
	"cmp"
	"context"
	"io"
	"path"
	"slices"
	"strings"

	"github.com/BurntSushi/toml"

	"github.com/Sawmonabo/codectx/internal/model"
	"github.com/Sawmonabo/codectx/internal/pagination"
	"github.com/Sawmonabo/codectx/internal/provider/treesitter/lang"
)

// freeRust is free for Rust, by the scoping of the Rust Reference's "Names"
// and "Use declarations": the functions declared in caller's module or in a
// block enclosing caller, else the function a `use` in that scope binds the
// name to, else those its glob `use`s bring into scope. A module's items
// and uses are not visible in the modules it declares.
func (r *staticFile) freeRust(caller int32, callee string, yield func(entry) error) error {
	var own []entry
	err := r.h.decls.lookup(keyFunc+r.fc.family+"\x00"+declScope(r.t, r.fc)+"\x00"+callee, func(e entry) error {
		if e.Ref.File == r.file && int(e.Ref.Index) < len(r.t.Decls) && rustSees(r.t.Decls, r.t.Decls[e.Ref.Index].Parent, caller) {
			own = append(own, e)
		}
		return nil
	})
	if err != nil {
		return err
	}
	if len(own) > 0 {
		for _, e := range own {
			if err := yield(e); err != nil {
				return err
			}
		}
		return nil
	}
	imported := false
	err = r.h.importsAt(r.fc, callee, func(p string, scope int32) error {
		if !rustSees(r.t.Decls, scope, caller) {
			return nil
		}
		imported = true
		if module, last, ok := cutLast(p, "::"); ok {
			return r.h.rustModuleDecls(keyFuncModule, r.fc, scope, module, last, yield)
		}
		return nil
	})
	if err != nil || imported {
		return err
	}
	return r.h.importsAt(r.fc, ImportAll, func(p string, scope int32) error {
		if !rustSees(r.t.Decls, scope, caller) {
			return nil
		}
		return r.h.rustModuleDecls(keyFuncModule, r.fc, scope, p, callee, yield)
	})
}

// qualifiedRust resolves a Rust `module::f()` path written in caller: a
// path whose first component a `use` in caller's scope binds is read from
// that use's path, from the module the use is written in (`u::f()` after
// `use crate::util as u`), any other from caller's module (rustPath).
func (r *staticFile) qualifiedRust(caller int32, q, name string, yield func(entry) error) error {
	head, rest, ok := strings.Cut(q, "::")
	scope := caller
	err := r.h.importsAt(r.fc, head, func(p string, at int32) error {
		if !rustSees(r.t.Decls, at, caller) {
			return nil
		}
		q, scope = p, at
		if ok {
			q += "::" + rest
		}
		return nil
	})
	if err != nil {
		return err
	}
	return r.h.rustModuleDecls(keyFuncModule, r.fc, scope, q, name, yield)
}

// rustSees reports that a declaration or `use` written in scope (an index
// of decls, -1 the file's module) is visible by its simple name to code in
// caller: scope is caller, or encloses it with no module between them,
// since a module's items and uses are its own.
func rustSees(decls []Decl, scope, caller int32) bool {
	for i := caller; ; i = decls[i].Parent {
		if i == scope {
			return true
		}
		if i < 0 || int(i) >= len(decls) || decls[i].Kind == model.NodeModule {
			return false
		}
	}
}

// rustScope is the crate and module path of code written in scope (an index
// of fc's declarations, -1 the file's module): the file's module (rustFile)
// followed by the inline `mod` items enclosing scope, scope included.
func rustScope(fc fileCtx, scope int32) (string, []string) {
	crate, module := rustFile(fc.path)
	var inline []string
	for i := scope; i >= 0 && int(i) < len(fc.decls); i = fc.decls[i].Parent {
		if fc.decls[i].Kind == model.NodeModule {
			inline = append(inline, fc.decls[i].Name)
		}
	}
	slices.Reverse(inline)
	return crate, append(module, inline...)
}

// rustModuleDecls yields the declarations under key (keyFuncModule or
// keyTypeModule) named name that the module the Rust path q, written in
// scope of fc's file, names. A module several crate roots share (rustFile)
// yields, as read from one of them, that file's own declaration when it has
// one.
func (h *hierarchy) rustModuleDecls(key string, fc fileCtx, scope int32, q, name string, yield func(entry) error) error {
	crate, module := rustScope(fc, scope)
	m, err := h.rustPath(crate, module, q)
	if err != nil || m == "" {
		return err
	}
	var hits []entry
	err = h.decls.lookup(key+m+"\x00"+name, func(e entry) error {
		hits = append(hits, e)
		return nil
	})
	if err != nil {
		return err
	}
	if own := slices.DeleteFunc(slices.Clone(hits), func(e entry) bool { return e.Ref.File != fc.file }); len(own) > 0 {
		hits = own
	}
	for _, e := range hits {
		if err := yield(e); err != nil {
			return err
		}
	}
	return nil
}

// rustPath is the module the Rust path q, written in module of crate, names
// (rustModuleKey), or "" when it names none of the unit's, by the Rust
// Reference's "Paths": `crate` is the crate root, `self` the module, each
// `super` one module up; `::name`, and a first component that names no
// module the current one declares, name a crate of the unit (keyRustCrate);
// any other first component is a module the current one declares. The
// callers have already read a first component a `use` binds.
func (h *hierarchy) rustPath(crate string, module []string, q string) (string, error) {
	segs := strings.Split(q, "::")
	external := func(name string, rest []string) (string, error) {
		root := ""
		err := h.decls.lookup(keyRustCrate+name, func(e entry) error {
			root = e.Aux
			return nil
		})
		if err != nil || root == "" {
			return "", err
		}
		return rustModuleKey(root, rest), nil
	}
	switch segs[0] {
	case "":
		if len(segs) < 2 {
			return "", nil
		}
		return external(segs[1], segs[2:])
	case "crate":
		module, segs = nil, segs[1:]
	case "self":
		segs = segs[1:]
	case "super":
	default:
		child := false
		err := h.decls.lookup(keyRustModule+rustModuleKey(crate, append(slices.Clone(module), segs[0])), func(entry) error {
			child = true
			return nil
		})
		if err != nil {
			return "", err
		}
		if !child {
			if m, err := external(segs[0], segs[1:]); err != nil || m != "" {
				return m, err
			}
		}
	}
	for len(segs) > 0 && segs[0] == "super" {
		if len(module) == 0 {
			return "", nil
		}
		module, segs = module[:len(module)-1], segs[1:]
	}
	return rustModuleKey(crate, append(slices.Clone(module), segs...)), nil
}

// rustCrates adds, for each Cargo.toml of the unit, the crate its library
// is, under the name other crates `use` it by (keyRustCrate): `[lib] name`,
// else `[package] name` with `-` written `_` (the Cargo Book's "Cargo
// Targets"), its root the `[lib] path`, else src/lib.rs. A manifest without
// a package (a virtual workspace) names no crate; neither does one that
// does not parse, which Cargo itself refuses.
func rustCrates(ctx context.Context, u *unit, s *pagination.ExternalSort[entry]) error {
	if u.content == nil {
		return nil
	}
	return u.content.EachFile(ctx, model.FileSelection{}, func(fv model.FileVersion) error {
		if fv.Status == model.FileDeleted || path.Base(fv.Path) != "Cargo.toml" ||
			u.root != "" && !strings.HasPrefix(fv.Path, u.root+"/") {
			return nil
		}
		rc, fv, err := u.content.Open(ctx, fv.ID)
		if err != nil {
			return err
		}
		data, err := io.ReadAll(io.LimitReader(rc, fv.Size))
		rc.Close()
		if err != nil {
			return err
		}
		var doc struct {
			Package struct {
				Name string `toml:"name"`
			} `toml:"package"`
			Lib struct {
				Name string `toml:"name"`
				Path string `toml:"path"`
			} `toml:"lib"`
		}
		if toml.Unmarshal(data, &doc) != nil {
			return nil
		}
		name := doc.Lib.Name
		if name == "" {
			name = strings.ReplaceAll(doc.Package.Name, "-", "_")
		}
		if name == "" {
			return nil
		}
		lib := cmp.Or(doc.Lib.Path, "src/lib.rs")
		return s.Add(entry{Key: keyRustCrate + name, Aux: rustModuleKey(rustFile(path.Join(path.Dir(fv.Path), lib)))})
	})
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

// rustReceiver is the type a call of method on a value of the Rust type
// written reaches, by the method-call lookup of the Rust Reference's
// "Method-call expressions": a smart pointer's own type for a method it
// answers itself (lang.RustPointerMethod), which no table declares, so
// false; otherwise, through each smart pointer, the type it owns.
func rustReceiver(written, method string) (string, bool) {
	for {
		inner, ok := lang.RustPointee(written)
		if !ok {
			return written, true
		}
		if lang.RustPointerMethod(method) {
			return "", false
		}
		written = inner
	}
}
