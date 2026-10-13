package link

import (
	"path"
	"strings"
)

// TypeScript's `classic` module resolution (moduleNameResolver.ts,
// classicNameResolver; identical in TypeScript 5.9 and 6.0, removed in 7.0,
// so a project that selects it compiles under a version that runs it). It
// reads no package.json: no `exports`, `imports`, self name or workspace
// package. A specifier resolves in two passes, TypeScript and declaration
// files first, then JavaScript files, each pass in this order:
//
//  1. the config's settings: `paths` for a bare specifier (a key that
//     matches decides alone, found or not), else its baseUrl; rootDirs for
//     a relative one;
//  2. a relative or absolute specifier as a file next to the importing
//     file; a bare one as a file under the importing file's directory or
//     any directory above it, nearest first.
//
// Every lookup is of a file, never of a directory's index file. A bare
// specifier found nowhere is a package outside the repository (TypeScript
// then reads node_modules/@types, which the snapshot does not hold).

// The two passes of a classic resolution.
const (
	classicTS = iota // TypeScript and declaration files
	classicJS        // JavaScript files
)

// classicExtensions is what TypeScript's tryAddingExtensions appends to a
// specifier with the extension ext ("" for none) in the given pass.
func classicExtensions(ext string, pass int) []string {
	switch ext {
	case ".mjs", ".mts", ".d.mts":
		if pass == classicTS {
			return []string{".mts", ".d.mts"}
		}
		return []string{".mjs"}
	case ".cjs", ".cts", ".d.cts":
		if pass == classicTS {
			return []string{".cts", ".d.cts"}
		}
		return []string{".cjs"}
	case ".json":
		// A JSON module is an error under classic; only its declaration
		// file is read.
		if pass == classicTS {
			return []string{".d.json.ts"}
		}
		return nil
	case ".tsx", ".jsx":
		if pass == classicTS {
			return []string{".tsx", ".ts", ".d.ts"}
		}
		return []string{".jsx", ".js"}
	case ".ts", ".d.ts", ".js", "":
		if pass == classicTS {
			return []string{".ts", ".tsx", ".d.ts"}
		}
		return []string{".js", ".jsx"}
	}
	if pass == classicTS && !declarationFile(ext) {
		return []string{".d" + ext + ".ts"}
	}
	return nil
}

// tsExtensions are the extensions TypeScript's removeFileExtension strips,
// in its order.
var tsExtensions = []string{".d.ts", ".d.mts", ".d.cts", ".mjs", ".mts", ".cjs", ".cts", ".ts", ".js", ".tsx", ".jsx", ".json"}

// declarationFile reports a name TypeScript reads as a declaration file.
func declarationFile(name string) bool {
	if strings.HasSuffix(name, ".d.ts") || strings.HasSuffix(name, ".d.mts") || strings.HasSuffix(name, ".d.cts") {
		return true
	}
	// An arbitrary extension's declaration file: name.d.<ext>.ts.
	rest, ok := strings.CutSuffix(name, ".ts")
	if !ok {
		return false
	}
	i := strings.LastIndex(rest, ".d.")
	return i >= 0 && !strings.Contains(rest[i+3:], ".") && !strings.Contains(rest[i+3:], "/")
}

// classicFiles is the files, in order, TypeScript's loadModuleFromFile
// tries for the path p in one pass: p with its extension replaced (`./a.js`
// is read as a.ts), then p with an extension added (`./a` as a.ts, `./a.b`
// as a.b.ts).
func classicFiles(p string, pass int) []string {
	var out []string
	if base := path.Base(p); strings.Contains(base, ".") {
		bare := p
		for _, ext := range tsExtensions {
			if s, ok := strings.CutSuffix(p, ext); ok {
				bare = s
				break
			}
		}
		if bare == p {
			bare = p[:strings.LastIndex(p, ".")]
		}
		for _, ext := range classicExtensions(p[len(bare):], pass) {
			out = append(out, bare+ext)
		}
	}
	for _, ext := range classicExtensions("", pass) {
		out = append(out, p+ext)
	}
	return out
}

// classicFile is the member module path of the first of classicFiles(p,
// pass) that is a member file, "" when none is.
func (r *esResolver) classicFile(p string, pass int) (string, error) {
	for _, f := range classicFiles(p, pass) {
		got, err := r.exactFile(f)
		if err != nil || got != "" {
			return got, err
		}
	}
	return "", nil
}

// hasTSExtension reports a name ending in an extension TypeScript's
// tryGetExtensionFromPath reads.
func hasTSExtension(name string) bool {
	for _, ext := range tsExtensions {
		if strings.HasSuffix(name, ext) {
			return true
		}
	}
	return false
}

// exactFile is the member module path of the member file f, "" when f is
// no member.
func (r *esResolver) exactFile(f string) (string, error) {
	base := stripExtension(f)
	found := false
	if err := r.bases.lookup(keyESBase+base, func(e entry) error {
		found = found || e.Aux == f
		return nil
	}); err != nil || !found {
		return "", err
	}
	return base, nil
}

// resolveClassic is one classic resolution of spec, written in the member
// at from, under config c (nil for none).
func (r *esResolver) resolveClassic(from, spec string, c *tsConfig) (esResult, error) {
	relative := esRelative(spec)
	for pass := classicTS; pass <= classicJS; pass++ {
		got, err := r.classicSettings(from, spec, relative, c, pass)
		if err != nil || got != "" {
			return one(got), err
		}
		if relative {
			got, err := r.classicFile(esJoin(from, spec), pass)
			if err != nil || got != "" {
				return one(got), err
			}
			continue
		}
		for d := path.Dir(from); ; d = path.Dir(d) {
			if d == "." {
				d = ""
			}
			got, err := r.classicFile(path.Join(d, spec), pass)
			if err != nil || got != "" {
				return one(got), err
			}
			if d == "" || d == r.in.root {
				break
			}
		}
	}
	if relative {
		return esResult{}, nil
	}
	return esResult{extn: true}, nil
}

// classicSettings is TypeScript's tryLoadModuleUsingOptionalResolutionSettings
// with classic's file loader: a bare specifier through `paths`, else the
// baseUrl; a relative one through rootDirs.
func (r *esResolver) classicSettings(from, spec string, relative bool, c *tsConfig, pass int) (string, error) {
	if c == nil {
		return "", nil
	}
	if relative {
		for _, p := range rootDirCandidates(c, esJoin(from, spec)) {
			got, err := r.classicFile(p, pass)
			if err != nil || got != "" {
				return got, err
			}
		}
		return "", nil
	}
	if c.paths != nil {
		if targets, star, ok := matchPaths(*c.paths, spec); ok {
			for _, t := range targets {
				p := path.Join(c.pathsRoot(), strings.Replace(t, "*", star, 1))
				if hasTSExtension(t) {
					// A substitution with an extension names its file
					// first.
					got, err := r.exactFile(p)
					if err != nil || got != "" {
						return got, err
					}
				}
				got, err := r.classicFile(p, pass)
				if err != nil || got != "" {
					return got, err
				}
			}
			// A matching key decides: the baseUrl is not read.
			return "", nil
		}
	}
	if c.baseURL != "" {
		return r.classicFile(path.Join(c.baseURL, spec), pass)
	}
	return "", nil
}
