// Package codectx holds the module's own Go sources, so a provider's version
// can be the digest of the code that produces its output: a stored result is
// reused only when that code is unchanged, and no version is ever written by
// hand.
package codectx

import (
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"go/parser"
	"go/token"
	"io/fs"
	"maps"
	"path"
	"runtime"
	"runtime/debug"
	"slices"
	"strconv"
	"strings"
)

// Module is the module path of go.mod.
const Module = "github.com/Sawmonabo/codectx"

// sources is every Go file of this package and of the module's packages
// under internal, test files included (an embed pattern cannot leave them
// out; Digest reads none). This package is ".".
//
//go:embed *.go internal/*/*.go internal/*/*/*.go internal/*/*/*/*.go
var sources embed.FS

// Digest is the digest of the code the packages roots (import paths relative
// to the module, `internal/provider/link`) are built from: every package of
// the module they import, transitively, each by its non-test Go files'
// names and bytes; every module outside it those imports name, by its
// version and sum as the binary was built with them; and the Go toolchain's
// version for the standard library. A change to any of it changes the
// digest; a change to a test file, or to a package none of the roots reach,
// does not.
func Digest(roots ...string) (string, error) {
	var deps []*debug.Module
	if info, ok := debug.ReadBuildInfo(); ok {
		deps = info.Deps
	}
	sum, _, err := digest(sources, deps, runtime.Version(), roots...)
	return sum, err
}

// digest is Digest over the module files fsys, the build's module
// dependencies deps and the toolchain version toolchain, with the packages
// of the module it read, in order.
func digest(fsys fs.FS, deps []*debug.Module, toolchain string, roots ...string) (string, []string, error) {
	h := sha256.New()
	add := func(parts ...string) {
		for _, p := range parts {
			h.Write([]byte(strconv.Itoa(len(p))))
			h.Write([]byte{':'})
			h.Write([]byte(p))
		}
	}
	add("codectx-sources-v1", toolchain)
	seen := map[string]bool{}
	external := map[string]bool{}
	queue := slices.Clone(roots)
	slices.Sort(queue)
	var packages []string
	// files is each file read, by path, with its bytes' digest.
	files := map[string][sha256.Size]byte{}
	fset := token.NewFileSet()
	for len(queue) > 0 {
		dir := queue[0]
		queue = queue[1:]
		if seen[dir] {
			continue
		}
		seen[dir] = true
		packages = append(packages, dir)
		entries, err := fs.ReadDir(fsys, dir)
		if err != nil {
			return "", nil, &sourceError{dir: dir, err: err}
		}
		found := false
		for _, e := range entries {
			name := e.Name()
			if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
				continue
			}
			found = true
			src, err := fs.ReadFile(fsys, path.Join(dir, name))
			if err != nil {
				return "", nil, &sourceError{dir: dir, err: err}
			}
			files[path.Join(dir, name)] = sha256.Sum256(src)
			f, err := parser.ParseFile(fset, name, src, parser.ImportsOnly)
			if err != nil {
				return "", nil, &sourceError{dir: dir, err: err}
			}
			for _, imp := range f.Imports {
				p, _ := strconv.Unquote(imp.Path.Value)
				switch {
				case p == Module:
					queue = append(queue, ".")
				case strings.HasPrefix(p, Module+"/"):
					queue = append(queue, strings.TrimPrefix(p, Module+"/"))
				case p != "C" && strings.Contains(strings.SplitN(p, "/", 2)[0], "."):
					external[p] = true
				}
			}
		}
		if !found {
			return "", nil, &sourceError{dir: dir, err: fs.ErrNotExist}
		}
	}
	slices.Sort(packages)
	for _, name := range slices.Sorted(maps.Keys(files)) {
		sum := files[name]
		add(name, string(sum[:]))
	}
	// Each module outside this one that an import names, by the longest
	// module path that prefixes the import, as the build resolved it.
	var mods []string
	for imp := range external {
		var best *debug.Module
		for _, m := range deps {
			if (imp == m.Path || strings.HasPrefix(imp, m.Path+"/")) && (best == nil || len(m.Path) > len(best.Path)) {
				best = m
			}
		}
		if best == nil {
			mods = append(mods, imp+" unresolved")
			continue
		}
		if best.Replace != nil {
			best = best.Replace
		}
		mods = append(mods, best.Path+" "+best.Version+" "+best.Sum)
	}
	slices.Sort(mods)
	add(slices.Compact(mods)...)
	return hex.EncodeToString(h.Sum(nil)), packages, nil
}

// sourceError is a root or an import whose package's sources are not
// embedded.
type sourceError struct {
	dir string
	err error
}

func (e *sourceError) Error() string {
	return "the sources of package " + Module + "/" + e.dir + " are not embedded: " + e.err.Error()
}

func (e *sourceError) Unwrap() error { return e.err }
