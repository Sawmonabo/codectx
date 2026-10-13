package link

import (
	"path"
	"strings"
)

// A glob of a tsconfig.json `files`/`include`/`exclude`, a package.json
// `workspaces`, a pnpm-workspace.yaml or a lerna.json `packages` list, as
// TypeScript and the package managers read one: `*` matches any run of
// characters within one path component, `?` one character within one, and a
// `**` component any number of whole components (TypeScript's
// getFileMatcherPatterns; the package managers' globstar). A pattern is matched
// against a root-relative path, component by component, so a match costs
// time linear in the two lengths times the number of `**` components and
// allocates nothing beyond the split.

// glob is one compiled pattern: its components, rooted at a directory.
type glob struct {
	parts []string
	// files reports a tsconfig include or files pattern, under whose
	// wildcards a component starting with "." or naming a package folder
	// never matches (TypeScript's matchFiles); a workspace pattern matches
	// every directory.
	files bool
	// literal reports a pattern with no wildcard at all.
	literal bool
}

// compileGlob compiles pattern, written relative to the directory dir, into
// a glob over root-relative paths. A tsconfig include pattern (implicitDir)
// whose last component has no ".", "*" or "?" names a directory and matches
// everything under it ("dir/**/*").
func compileGlob(dir, pattern string, files, implicitDir bool) glob {
	p := path.Join(dir, strings.TrimPrefix(pattern, "./"))
	if p == "." {
		p = ""
	}
	var parts []string
	if p != "" {
		parts = strings.Split(p, "/")
	}
	if implicitDir {
		last := ""
		if len(parts) > 0 {
			last = parts[len(parts)-1]
		}
		if !strings.ContainsAny(last, ".*?") {
			parts = append(parts, "**", "*")
		}
	}
	return glob{parts: parts, files: files, literal: !strings.ContainsAny(p, "*?")}
}

// packageFolders are the directories a tsconfig wildcard never enters unless
// the pattern names them (TypeScript's commonPackageFolders).
var packageFolders = map[string]bool{"node_modules": true, "bower_components": true, "jspm_packages": true}

// match reports whether the root-relative path p matches g.
func (g glob) match(p string) bool {
	var segs []string
	if p != "" {
		segs = strings.Split(p, "/")
	}
	return g.matchFrom(0, segs)
}

func (g glob) matchFrom(i int, segs []string) bool {
	for ; i < len(g.parts); i++ {
		part := g.parts[i]
		if part == "**" {
			// Zero or more whole components, none hidden or a package
			// folder for a file pattern.
			for k := 0; ; k++ {
				if g.matchFrom(i+1, segs[k:]) {
					return true
				}
				if k == len(segs) || g.files && (strings.HasPrefix(segs[k], ".") || packageFolders[segs[k]]) {
					return false
				}
			}
		}
		if len(segs) == 0 {
			return false
		}
		seg := segs[0]
		if g.files && part != seg && (strings.HasPrefix(seg, ".") && !strings.HasPrefix(part, ".") || packageFolders[seg]) {
			return false
		}
		if !matchComponent(part, seg) {
			return false
		}
		segs = segs[1:]
	}
	return len(segs) == 0
}

// matchComponent matches one path component against one pattern component
// of `*` and `?` wildcards, by the usual two-pointer backtracking over the
// last star, linear in practice.
func matchComponent(pattern, s string) bool {
	pi, si := 0, 0
	star, mark := -1, 0
	for si < len(s) {
		switch {
		case pi < len(pattern) && (pattern[pi] == '?' || pattern[pi] == s[si]):
			pi++
			si++
		case pi < len(pattern) && pattern[pi] == '*':
			star, mark = pi, si
			pi++
		case star >= 0:
			pi = star + 1
			mark++
			si = mark
		default:
			return false
		}
	}
	for pi < len(pattern) && pattern[pi] == '*' {
		pi++
	}
	return pi == len(pattern)
}

// globSet is an ordered list of patterns, each possibly negated with a
// leading "!" (pnpm-workspace.yaml): a path matches when the last pattern
// that matches it is not negated.
type globSet struct {
	globs   []glob
	negated []bool
}

func (s *globSet) add(dir, pattern string) {
	neg := strings.HasPrefix(pattern, "!")
	pattern = strings.TrimPrefix(pattern, "!")
	s.globs = append(s.globs, compileGlob(dir, pattern, false, false))
	s.negated = append(s.negated, neg)
}

func (s *globSet) match(p string) bool {
	in := false
	for i, g := range s.globs {
		if g.match(p) {
			in = !s.negated[i]
		}
	}
	return in
}
