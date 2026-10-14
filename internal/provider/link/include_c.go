package link

import (
	"path"
	"strings"

	"github.com/Sawmonabo/codectx/internal/pagination"
)

// keyCInclude + refKey(file): one `#include` of a C or C++ member, the path
// as the directive writes it (`<h>` keeps its angle brackets) in Sig and the
// member's own path in Aux. A member's include closure is read through it,
// one member at a time (cClosure).
const keyCInclude = "ci\x00"

// addCIncludes adds the `#include` directives of the C or C++ member t at
// ordinal file (keyCInclude). A directive that names a macro the worker
// could not fold to a path is added with no path: the closure through it is
// not known (cClosure.open).
func addCIncludes(s *pagination.ExternalSort[entry], file int32, t *Table, fc fileCtx) error {
	if fc.family != "c" {
		return nil
	}
	key := keyCInclude + refKey(Ref{File: file})
	for _, imp := range t.Imports {
		if imp.Name != "" {
			continue
		}
		if err := s.Add(entry{Key: key, Ref: Ref{File: file, Index: -1}, Aux: t.Path, Sig: imp.Path}); err != nil {
			return err
		}
	}
	return nil
}

// cClosure is the include closure of one C or C++ member: the member and
// every member file the preprocessor reads for it, through its own
// `#include` directives and those of each file they include, each resolved
// as includes resolves the member's own (the file beside the including one
// for a quoted path, and every file whose path ends in it at a component
// boundary). The closure is read breadth first and only as far as a
// question needs: sees reads on until the file it asks about is reached or
// no file is left to read. A file is read once, which is the cycle guard,
// as an include guard or `#pragma once` makes a second inclusion of a
// header add nothing; what a file sees is the same either way. Memory is the
// members reached and those still to read, never the unit. One closure
// serves every question a member's resolution asks (staticFile.closure):
// the macros a call is replaced by, the functions of internal linkage it
// may call, and the visibility macros of its declarations.
type cClosure struct {
	r *staticFile
	// reached holds every member found in the closure so far.
	reached map[int32]bool
	// pending holds the members reached whose directives are not read yet.
	pending []int32
	// started is set once the member's own directives are resolved.
	started bool
	// open is set once a directive read names what the unit cannot follow:
	// a macro the worker could not fold to a path, or a quoted path no
	// member file answers (a generated header, say). Files beyond it may
	// be in the translation unit without being in the closure.
	open bool
}

// includeClosure is the include closure of r's member, made on first use
// and shared by every question of the member's resolution.
func (r *staticFile) includeClosure() *cClosure {
	if r.closure == nil {
		r.closure = newCClosure(r)
	}
	return r.closure
}

// newCClosure is the closure of r's member, nothing read yet.
func newCClosure(r *staticFile) *cClosure {
	return &cClosure{r: r, reached: map[int32]bool{r.file: true}}
}

// sees reports whether the definition e, an entry whose Ref is its
// declaration and whose Aux is its file's path, is in a file of the
// closure. The member itself and a file it includes directly are told
// without reading the index.
func (c *cClosure) sees(e entry) (bool, error) {
	return c.seesFile(e.Ref.File, e.Aux)
}

// seesFile reports whether the member file at path p is in the closure.
func (c *cClosure) seesFile(file int32, p string) (bool, error) {
	if file == c.r.file || c.r.includes(p) {
		return true, nil
	}
	return c.reaches(file)
}

// complete reads the whole closure and reports whether it is open.
func (c *cClosure) complete() (open bool, err error) {
	_, err = c.reaches(-1)
	return c.open, err
}

// reaches reports whether file is in the closure, reading the directives
// of the members still pending until it is reached or none is left.
func (c *cClosure) reaches(file int32) (bool, error) {
	defer c.r.h.memberWide()()
	if !c.started {
		// The member's own directives, from its table.
		c.started = true
		for _, imp := range c.r.t.Imports {
			if imp.Name != "" {
				continue
			}
			if err := c.resolve(c.r.fc.path, imp.Path); err != nil {
				return false, err
			}
		}
	}
	for !c.reached[file] && len(c.pending) > 0 {
		next := c.pending[0]
		c.pending = c.pending[1:]
		var from string
		var written []string
		err := c.r.h.decls.lookup(keyCInclude+refKey(Ref{File: next}), func(e entry) error {
			from = e.Aux
			written = append(written, e.Sig)
			return nil
		})
		if err != nil {
			return false, err
		}
		for _, w := range written {
			if err := c.resolve(from, w); err != nil {
				return false, err
			}
		}
	}
	return c.reached[file], nil
}

// resolve adds to the closure every member the directive written in the
// file at path from names: for a quoted path the file beside from, and for
// either form every file whose path ends in it at a component boundary
// unless it climbs out of the directory (`../`), as includes reads it. A
// directive with no path, or a quoted one no member file answers, opens
// the closure; an angled one no member answers is a header of the
// toolchain's, which includes no file of the unit.
func (c *cClosure) resolve(from, written string) error {
	if written == "" {
		c.open = true
		return nil
	}
	h, angled := strings.CutPrefix(written, "<")
	h = path.Clean(strings.TrimSuffix(h, ">"))
	named := false
	add := func(file int32) {
		named = true
		if !c.reached[file] {
			c.reached[file] = true
			c.pending = append(c.pending, file)
		}
	}
	if !angled {
		beside := path.Join(path.Dir(from), h)
		err := c.r.h.decls.lookup(keyPath+beside, func(e entry) error {
			fc, ok, err := c.r.h.fileContext(e.Ref.File)
			if err == nil && ok && fc.path == beside {
				add(e.Ref.File)
			}
			return err
		})
		if err != nil {
			return err
		}
	}
	if !strings.HasPrefix(h, "../") {
		err := c.r.h.decls.lookup(keyPath+h, func(e entry) error {
			add(e.Ref.File)
			return nil
		})
		if err != nil {
			return err
		}
	}
	if !named && !angled {
		c.open = true
	}
	return nil
}
