package link

import (
	"path"
	"strings"
)

// ---- language rules ----

// ecmaScriptExtensions are the extensions an ECMAScript import may leave
// out, so a module's path is compared without them.
var ecmaScriptExtensions = []string{".d.ts", ".d.mts", ".d.cts", ".js", ".mjs", ".cjs", ".jsx", ".ts", ".mts", ".cts", ".tsx"}

// moduleBases are the module paths, without extension, an import may name
// the member file p by: ECMAScript's file path (a directory's index file is
// reached through the directory only where the importing file's module mode
// allows it, which the resolution decides, esResolver.probe); Python's
// dotted module path as a path, from every directory that may be a source
// root, and for a package's `__init__.py` the package.
func moduleBases(p string, python bool) []string {
	var base string
	if python {
		base = strings.TrimSuffix(strings.TrimSuffix(p, ".py"), ".pyi")
		if path.Base(base) == "__init__" {
			base = path.Dir(base)
		}
		var out []string
		for s := base; ; {
			out = append(out, s)
			i := strings.IndexByte(s, '/')
			if i < 0 {
				return out
			}
			s = s[i+1:]
		}
	}
	return []string{stripExtension(p)}
}

// moduleBase is the module path, without extension, the import spec written
// in the file at p denotes as a path alone, and false for a spec no member
// file can be by its path: an ECMAScript bare specifier, which the
// configuration-aware resolution answers instead (esResolver).
func moduleBase(p, spec string, python bool) (string, bool) {
	dir := path.Dir(p)
	if python {
		dots := len(spec) - len(strings.TrimLeft(spec, "."))
		rest := strings.ReplaceAll(spec[dots:], ".", "/")
		if dots == 0 {
			return rest, rest != ""
		}
		for range dots - 1 {
			dir = path.Dir(dir)
		}
		return path.Join(dir, rest), true
	}
	if !strings.HasPrefix(spec, "./") && !strings.HasPrefix(spec, "../") {
		return "", false
	}
	return stripExtension(path.Join(dir, spec)), true
}

// importedName splits the module receiver of a bare call of an imported
// name into the module as written and the name the module declares: an
// ECMAScript `spec#name` (`import {name as local} from "spec"`), a Python
// `module.name` split at its last dot (`from module import name as local`,
// a relative module keeping its dots).
func importedName(written string, python bool) (spec, name string, ok bool) {
	sep := "#"
	if python {
		sep = "."
	}
	i := strings.LastIndex(written, sep)
	if i < 0 || i == len(written)-1 {
		return "", "", false
	}
	spec, name = written[:i], written[i+1:]
	if python && strings.Trim(spec, ".") == "" {
		// `from . import name`: the module is the package the dots name.
		spec = written[:i+1]
	}
	return spec, name, spec != ""
}

func stripExtension(p string) string {
	for _, ext := range ecmaScriptExtensions {
		if strings.HasSuffix(p, ext) {
			return strings.TrimSuffix(p, ext)
		}
	}
	return p
}
