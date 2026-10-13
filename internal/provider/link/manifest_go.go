package link

import (
	"context"
	"os"
)

// The keys of a Go unit's manifest fields.
const (
	// fieldGoModule is a Go unit's module path.
	fieldGoModule = "go.mod\x00module"
	// fieldGoVersion and fieldGoToolchain are the go.mod's go and
	// toolchain lines, which choose the standard library (goRoot).
	fieldGoVersion   = "go.mod\x00go"
	fieldGoToolchain = "go.mod\x00toolchain"
	// fieldGoRequire + a module path is that requirement's version.
	fieldGoRequire = "go.mod\x00require\x00"
	// fieldGoReplace + a module path and "@" + a version (or none) is the
	// replacement in force for it, ordered as goModuleInfo.replaces is.
	fieldGoReplace = "go.mod\x00replace\x00"
	// fieldGoWork is the governing go.work's path; fieldGoWorkUse + a
	// directory is the module path of that workspace module.
	fieldGoWork    = "go.work\x00path"
	fieldGoWorkUse = "go.work\x00use\x00"
	// fieldGoVendor is the digest of vendor/modules.txt.
	fieldGoVendor = "vendor/modules.txt\x00digest"
	// fieldGoRoot is the standard library read (goRoot): its release and
	// directory; fieldGoSource + a module@version says whether the module
	// cache holds that module, which decides whether its packages are
	// read.
	fieldGoRoot   = "go.host\x00goroot"
	fieldGoSource = "go.host\x00module\x00"
)

// goManifestFields is every field a Go unit's resolution reads from its
// go.mod, the go.work governing it, its vendor manifest and the read-only
// host sources (goExternal): the module path decides which import paths are
// the unit's own, and the rest where each package outside the unit is read
// from, so a version bump, a replacement, a workspace change or a module
// cache that now holds a required version moves a field and the unit's
// hierarchy is built again.
func goManifestFields(ctx context.Context, u *unit) ([]manifestField, error) {
	info, err := readGoModuleInfo(ctx, u)
	if err != nil {
		return nil, err
	}
	out := []manifestField{{Key: fieldGoModule, Value: info.module}}
	add := func(k, v string) { out = append(out, manifestField{Key: k, Value: v}) }
	if info.goVersion != "" {
		add(fieldGoVersion, info.goVersion)
	}
	if info.toolchain != "" {
		add(fieldGoToolchain, info.toolchain)
	}
	for _, r := range info.requires {
		add(fieldGoRequire+r.Path, r.Version)
	}
	for i, r := range info.replaces {
		to := r.new.Path + "@" + r.new.Version
		if r.dir != "" || r.outside {
			to = "dir:" + r.dir
		}
		add(fieldGoReplace+r.old.Path+"@"+r.old.Version, itoaInt(i)+"\x00"+to)
	}
	if info.work != "" {
		add(fieldGoWork, info.work)
	}
	for _, m := range info.uses {
		add(fieldGoWorkUse+m.dir, m.path)
	}
	if info.vendorDigest != "" {
		add(fieldGoVendor, info.vendorDigest)
	}
	if u.family != FamilyGo || info.module == "" {
		return out, nil
	}
	cache := goModCache()
	dir, release := goRoot(cache, info.goVersion, info.toolchain)
	add(fieldGoRoot, release+"\x00"+dir)
	for _, r := range info.requires {
		src := info.source(r.Path)
		if src.missing || src.std || src.dir != "" {
			continue
		}
		present := "absent"
		if d := goModuleDir(cache, src.module); d != "" {
			if fi, err := os.Stat(d); err == nil && fi.IsDir() {
				present = "present"
			}
		}
		add(fieldGoSource+src.module.Path+"@"+src.module.Version, present)
	}
	return out, nil
}
