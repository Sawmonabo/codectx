package link

import (
	"context"
	"io"
	"path"
	"slices"
	"strings"

	"golang.org/x/mod/modfile"
	"golang.org/x/mod/module"

	"github.com/Sawmonabo/codectx/internal/model"
)

// goModuleInfo is what a Go unit's go.mod, the go.work governing it and its
// vendor manifest say about where each import path's source is (The Go
// Modules Reference, "go.mod files", "Workspaces", "Vendoring"): the module
// path, the go and toolchain lines, every requirement at its version, the
// replacements, the workspace's modules and the vendor manifest's digest.
// Every path in it is repository-relative.
type goModuleInfo struct {
	module, goVersion, toolchain string
	// requires are the go.mod's requirements, sorted by path.
	requires []module.Version
	// replaces are the replacements in force: the go.work's, then the
	// go.mod's, the first that matches deciding (a workspace's replacement
	// overrides a module's).
	replaces []goReplace
	// work is the governing go.work's path, empty when none governs the
	// unit; uses its workspace modules, by module path, each at its
	// repository-relative directory.
	work string
	uses []goWorkModule
	// vendorDigest is the content hash of vendor/modules.txt, empty when
	// the unit vendors nothing.
	vendorDigest string
}

// goReplace is one replacement: Old (a version-less Old replaces every
// version) by New, a module at a version or, when Dir is set, the
// repository-relative directory a local path names. A local path that
// leaves the repository names no directory the snapshot holds: Outside.
type goReplace struct {
	old, new module.Version
	dir      string
	outside  bool
}

// goWorkModule is one module a go.work `use` directive adds.
type goWorkModule struct{ path, dir string }

// readGoModuleInfo reads the unit's go.mod, the nearest go.work at or above
// its root whose `use` directives name the unit's root, and the vendor
// manifest, all from the snapshot. A go.mod the parser refuses keeps its
// module path (modfile.ModulePath) and nothing else.
func readGoModuleInfo(ctx context.Context, u *unit) (*goModuleInfo, error) {
	info := &goModuleInfo{}
	if u.content == nil {
		return info, nil
	}
	root := path.Join(".", u.root)
	data, ok, err := readSnapshotFile(ctx, u.content, path.Join(root, "go.mod"))
	if err != nil || !ok {
		return info, err
	}
	keep := func(_, v string) (string, error) { return v, nil }
	if f, err := modfile.Parse("go.mod", data, keep); err == nil {
		if f.Module != nil {
			info.module = f.Module.Mod.Path
		}
		if f.Go != nil {
			info.goVersion = f.Go.Version
		}
		if f.Toolchain != nil {
			info.toolchain = f.Toolchain.Name
		}
		for _, r := range f.Require {
			info.requires = append(info.requires, r.Mod)
		}
		slices.SortFunc(info.requires, func(a, b module.Version) int { return strings.Compare(a.Path, b.Path) })
		for _, r := range f.Replace {
			info.replaces = append(info.replaces, goReplacement(root, r.Old, r.New))
		}
	} else {
		info.module = modfile.ModulePath(data)
	}
	if err := info.readWork(ctx, u.content, root); err != nil {
		return nil, err
	}
	if v, ok, err := readSnapshotFile(ctx, u.content, path.Join(root, "vendor", "modules.txt")); err != nil {
		return nil, err
	} else if ok {
		info.vendorDigest = model.H("link.go_vendor", string(v))
	}
	return info, nil
}

// readWork finds the go.work governing the module at root: the nearest one
// in root or a directory above it, which governs root only when one of its
// `use` directives names root ("Workspaces": a module outside the
// workspace is built in module mode). Its replacements come before the
// go.mod's.
func (info *goModuleInfo) readWork(ctx context.Context, content model.SnapshotView, root string) error {
	for dir := root; ; dir = path.Dir(dir) {
		p := path.Join(dir, "go.work")
		data, ok, err := readSnapshotFile(ctx, content, p)
		if err != nil {
			return err
		}
		if ok {
			w, err := modfile.ParseWork("go.work", data, func(_, v string) (string, error) { return v, nil })
			if err != nil {
				return nil
			}
			var uses []goWorkModule
			governs := false
			for _, use := range w.Use {
				d, inside := repoPath(dir, use.Path)
				if !inside {
					continue
				}
				if d == root {
					governs = true
				}
				uses = append(uses, goWorkModule{dir: d})
			}
			if !governs {
				return nil
			}
			info.work = p
			for i := range uses {
				if mod, ok, err := readSnapshotFile(ctx, content, path.Join(uses[i].dir, "go.mod")); err != nil {
					return err
				} else if ok {
					uses[i].path = modfile.ModulePath(mod)
				}
			}
			info.uses = slices.DeleteFunc(uses, func(m goWorkModule) bool { return m.path == "" })
			var replaces []goReplace
			for _, r := range w.Replace {
				replaces = append(replaces, goReplacement(dir, r.Old, r.New))
			}
			info.replaces = append(replaces, info.replaces...)
			return nil
		}
		if dir == "." {
			return nil
		}
	}
}

// goReplacement is the replacement of old by new written in the go.mod or
// go.work in the repository-relative directory dir.
func goReplacement(dir string, old, new module.Version) goReplace {
	r := goReplace{old: old, new: new}
	if new.Version == "" && modfile.IsDirectoryPath(new.Path) {
		r.dir, r.outside = repoPath(dir, new.Path)
		r.outside = !r.outside
	}
	return r
}

// repoPath is the repository-relative path a directory path written in a
// file of the repository-relative directory dir names, and whether it lies
// inside the repository.
func repoPath(dir, p string) (string, bool) {
	if path.IsAbs(p) || strings.HasPrefix(p, `\`) || len(p) > 1 && p[1] == ':' {
		return "", false
	}
	j := path.Join(dir, p)
	if j == ".." || strings.HasPrefix(j, "../") {
		return "", false
	}
	return j, true
}

// readSnapshotFile is the content of the snapshot's file at p; ok is false
// when the snapshot holds none (or a deleted one).
func readSnapshotFile(ctx context.Context, content model.SnapshotView, p string) (data []byte, ok bool, err error) {
	err = content.EachFile(ctx, model.FileSelection{Paths: []string{p}}, func(fv model.FileVersion) error {
		if fv.Status == model.FileDeleted || fv.Path != p {
			return nil
		}
		rc, fv, err := content.Open(ctx, fv.ID)
		if err != nil {
			return err
		}
		defer rc.Close()
		data, err = io.ReadAll(io.LimitReader(rc, fv.Size))
		ok = err == nil
		return err
	})
	return data, ok, err
}

// replacement is the replacement in force for module m, if any: the first
// whose Old names m's path at m's version or at every version.
func (info *goModuleInfo) replacement(m module.Version) (goReplace, bool) {
	for _, r := range info.replaces {
		if r.old.Path == m.Path && (r.old.Version == "" || r.old.Version == m.Version) {
			return r, true
		}
	}
	return goReplace{}, false
}

// goSource is where the source of a Go package outside the unit is: in the
// snapshot under Dir (a workspace module or a local replacement, another
// unit's module), in the module cache as Module at its exact version
// (cache, its import path below the module's in Sub), or in the standard
// library. Missing is set for a package no read-only source can hold: a
// module the go.mod does not require, a local replacement outside the
// repository, a vendored unit's package absent from vendor/.
type goSource struct {
	std     bool
	dir     string
	module  module.Version
	sub     string
	missing bool
}

// source is where the package at import path p is, for a package that is no
// package of the unit: the standard library when p's first element has no
// dot ("Package path"), else the module providing it by the longest module
// path prefix among the workspace's modules, then the requirements with
// their replacements applied. A vendoring unit outside a workspace reads its
// dependencies from vendor/ only (-mod=vendor), which are the unit's own
// members, so every other package is missing.
func (info *goModuleInfo) source(p string) goSource {
	first, _, _ := strings.Cut(p, "/")
	if !strings.Contains(first, ".") {
		return goSource{std: true, sub: p}
	}
	var best goWorkModule
	for _, m := range info.uses {
		if under(p, m.path) && len(m.path) > len(best.path) {
			best = m
		}
	}
	if best.path != "" {
		return goSource{dir: path.Join(best.dir, strings.TrimPrefix(strings.TrimPrefix(p, best.path), "/"))}
	}
	if info.vendorDigest != "" && info.work == "" {
		return goSource{missing: true}
	}
	var req module.Version
	for _, r := range info.requires {
		if under(p, r.Path) && len(r.Path) > len(req.Path) {
			req = r
		}
	}
	if req.Path == "" {
		return goSource{missing: true}
	}
	sub := strings.TrimPrefix(strings.TrimPrefix(p, req.Path), "/")
	if r, ok := info.replacement(req); ok {
		switch {
		case r.outside:
			return goSource{missing: true}
		case r.dir != "":
			return goSource{dir: path.Join(r.dir, sub)}
		}
		return goSource{module: r.new, sub: sub}
	}
	return goSource{module: req, sub: sub}
}

// under reports that import path p is module path m or a path below it.
func under(p, m string) bool {
	return p == m || strings.HasPrefix(p, m+"/")
}
