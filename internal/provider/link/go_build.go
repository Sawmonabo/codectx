package link

import (
	"context"
	"go/build/constraint"
	"path"
	"slices"
	"strconv"
	"strings"

	"github.com/Sawmonabo/codectx/internal/gobuild"
)

// Go build conditions. A Go file belongs to the builds its build constraint
// and its name's _GOOS/_GOARCH suffix admit (go/build, "Build
// Constraints"), so a method declared in one file and a type declared in
// another meet only in the builds both admit, and a type satisfies an
// interface only when one build holds the interface's methods and all of
// the type's that match them. Each record satisfy reads carries the
// condition under which it is part of a build (goCond, in entry.MaxArgs of
// the method-set records, satisfy's sets and pairs): the conjunction of the
// conditions of the files it depends on. A record whose condition no build
// meets is never written, and a pair is satisfied when some build meets the
// conjunction, over the interface's method names, of the disjunction over
// the keys of each name of the key's interface and type conditions
// (pairSatisfied). The id 0 is the condition every build meets, so a unit
// with no constrained file writes and decides exactly what it did without
// them.

// goCond is a build condition's id in a hierarchy's goConds
// (gobuild.Conds), 0 the condition every build meets and 1 the one none
// does; one function of the build's tags has one id however it is written.
type goCond = gobuild.Cond

// goConds holds the build conditions of one hierarchy build.
type goConds struct {
	*gobuild.Conds
	// files are the unit's files' conditions, by file ordinal.
	files map[int32]goCond
}

func newGoConds(goVersion string) *goConds {
	minor := 0
	if _, rest, ok := strings.Cut(goVersion, "."); ok {
		rest, _, _ = strings.Cut(rest, ".")
		minor, _ = strconv.Atoi(rest)
	}
	return &goConds{Conds: gobuild.NewConds(minor), files: map[int32]goCond{}}
}

// parse is the conjunction of the constraint expressions texts, each empty
// or one gobuild.Header, gobuild.FileName or gobuild.File returns.
func (c *goConds) parse(texts ...string) goCond {
	all := gobuild.True
	for _, t := range texts {
		if t == "" {
			continue
		}
		x, err := constraint.Parse("//go:build " + t)
		if err != nil {
			continue
		}
		all = c.And(all, c.Expr(x))
	}
	return all
}

// file is the condition of a Go file at path p whose leading comments state
// the constraint header.
func (c *goConds) file(p, header string) goCond {
	return c.parse(header, gobuild.FileName(path.Base(p)))
}

func (c *goConds) and(a, b goCond) goCond    { return c.And(a, b) }
func (c *goConds) or(a, b goCond) goCond     { return c.Or(a, b) }
func (c *goConds) andNot(a, b goCond) goCond { return c.AndNot(a, b) }

// holds reports whether some build meets id.
func (c *goConds) holds(id goCond) bool { return c.Holds(id) }

// goFileCond is the condition of the unit's file of ordinal file, read from
// its keyFile entry (the table's Constraint in Returns).
func (h *hierarchy) goFileCond(file int32) (goCond, error) {
	c := h.goConds
	if id, ok := c.files[file]; ok {
		return id, nil
	}
	var id goCond
	err := h.decls.lookup(keyFile+refKey(Ref{File: file}), func(e entry) error {
		_, p, _ := strings.Cut(e.Aux, "\x00")
		id = c.file(p, e.Returns)
		return nil
	})
	if err != nil {
		return 0, err
	}
	c.files[file] = id
	return id, nil
}

// goForkLeaves bounds the builds-dependent spellings one method signature
// is enumerated into (goMethodKeys); past it the signature is unknown.
const goForkLeaves = 64

// goMethodKeys yields the method-set keys (goMethodKey) of the method name
// of signature form written in n, each with the build condition under which
// it is the method's. A signature that names no alias or constant declared
// more than once is one key under condition 0. One that does is one key per
// choice of declaration for each such name, under the conjunction of the
// chosen declarations' conditions, a choice no build makes dropped; when
// every choice spells one key, it is that key under the disjunction of
// their conditions. Past goForkLeaves choices the signature is its key read
// over every declaration at once (unknown when they differ), under
// condition 0.
func (h *hierarchy) goMethodKeys(ctx context.Context, types goPackageTypes, n goNames, name, form string, ref Ref,
	yield func(key string, spelling goSpelling, cond goCond) error) error {
	if h.goConds == nil {
		key, spelling, err := goMethodKey(ctx, types, n, name, form, ref)
		if err != nil {
			return err
		}
		return yield(key, spelling, 0)
	}
	w := &goChosen{inner: types}
	key, spelling, err := goMethodKey(ctx, w, n, name, form, ref)
	if err != nil {
		return err
	}
	if len(w.forks) == 0 {
		return yield(key, spelling, 0)
	}
	w.choice = map[string]int{}
	type leaf struct {
		key      string
		spelling goSpelling
		cond     goCond
	}
	var leaves []leaf
	exceeded := false
	var walk func(cond goCond) error
	walk = func(cond goCond) error {
		if exceeded {
			return nil
		}
		w.forks = w.forks[:0]
		k, sp, err := goMethodKey(ctx, w, n, name, form, ref)
		if err != nil {
			return err
		}
		if len(w.forks) == 0 {
			if len(leaves) == goForkLeaves {
				exceeded = true
				return nil
			}
			leaves = append(leaves, leaf{k, sp, cond})
			return nil
		}
		f := w.forks[0]
		for i, c := range f.conds {
			next := h.goConds.and(cond, c)
			if !h.goConds.holds(next) {
				continue
			}
			w.choice[f.key] = i
			if err := walk(next); err != nil {
				return err
			}
		}
		delete(w.choice, f.key)
		return nil
	}
	if err := walk(0); err != nil {
		return err
	}
	if exceeded {
		return yield(key, spelling, 0)
	}
	if len(leaves) > 0 && !slices.ContainsFunc(leaves, func(l leaf) bool { return l.key != leaves[0].key }) {
		// Every choice spells one key: the builds of any of them.
		cond := leaves[0].cond
		for _, l := range leaves[1:] {
			cond = h.goConds.or(cond, l.cond)
		}
		return yield(leaves[0].key, leaves[0].spelling, cond)
	}
	for _, l := range leaves {
		if err := yield(l.key, l.spelling, l.cond); err != nil {
			return err
		}
	}
	return nil
}

// goChosen reads its inner goPackageTypes with one declaration chosen for
// each alias and constant named in choice (by import path, NUL, name), and
// lists in forks each one declared more than once that choice does not
// name, as goMethodKeys enumerates them. It sits outside the lookup memo,
// so a memoized answer holds every declaration whatever a choice picks.
type goChosen struct {
	inner  goPackageTypes
	choice map[string]int
	forks  []goFork
}

// goFork is a name declared more than once and the conditions of its
// declarations.
type goFork struct {
	key   string
	conds []goCond
}

func (w *goChosen) fork(key string, conds []goCond) {
	if !slices.ContainsFunc(w.forks, func(f goFork) bool { return f.key == key }) {
		w.forks = append(w.forks, goFork{key, conds})
	}
}

func (w *goChosen) lookup(ctx context.Context, p, name string) (goTypeAnswer, bool, error) {
	a, known, err := w.inner.lookup(ctx, p, name)
	if err != nil || !a.alias || len(a.others) == 0 {
		return a, known, err
	}
	key := "t\x00" + p + "\x00" + name
	i, chosen := w.choice[key]
	if !chosen {
		conds := []goCond{a.cond}
		for _, d := range a.others {
			conds = append(conds, d.cond)
		}
		w.fork(key, conds)
		return a, known, nil
	}
	if i > 0 {
		d := a.others[i-1]
		a.form, a.names, a.cond = d.form, d.names, d.cond
	}
	a.others = nil
	return a, known, nil
}

func (w *goChosen) constant(ctx context.Context, p, name string) ([]goDecl, error) {
	decls, err := w.inner.constant(ctx, p, name)
	if err != nil || len(decls) < 2 {
		return decls, err
	}
	key := "k\x00" + p + "\x00" + name
	i, chosen := w.choice[key]
	if !chosen {
		conds := make([]goCond, len(decls))
		for j, d := range decls {
			conds[j] = d.cond
		}
		w.fork(key, conds)
		return decls, nil
	}
	return decls[i : i+1], nil
}

// goMethodKeysAux is the Aux of a Go method's keyMethod entry that records
// its method-set keys: a mark, then the keys, each closed by a separator no
// key holds. An entry without the mark records none: the method is in
// every build with one key.
func goMethodKeysAux(keys []string) string {
	var b strings.Builder
	b.WriteByte('\x02')
	for _, k := range keys {
		b.WriteString(k)
		b.WriteByte('\x03')
	}
	return b.String()
}

// goKeysMeet reports that a method of recorded keys own may implement the
// interface method of recorded keys other: either records none (a method
// of one key in every build, matched by name, as satisfy matched it), or
// they share a key.
func goKeysMeet(own, other string) bool {
	if !strings.HasPrefix(own, "\x02") || !strings.HasPrefix(other, "\x02") {
		return true
	}
	for k := range strings.SplitSeq(own[1:], "\x03") {
		if k == "" {
			continue
		}
		for o := range strings.SplitSeq(other[1:], "\x03") {
			if o == k {
				return true
			}
		}
	}
	return false
}

// goOwnKeys is the recorded keys (goMethodKeysAux) of the Go method at ref
// declared on owner under name, "" when it records none.
func (h *hierarchy) goOwnKeys(owner, ref Ref, name string) (string, error) {
	own := ""
	err := h.links.lookup(keyMethod+refKey(owner)+name, func(e entry) error {
		if e.Ref == ref {
			own = e.Aux
		}
		return nil
	})
	return own, err
}
