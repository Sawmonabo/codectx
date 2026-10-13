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

// goCondNone is the condition no build meets.
const goCondNone = gobuild.False

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

// goMethodKeys yields the method-set keys (goMethodKey) of the method name
// of signature form written in n, each with the build condition under which
// it is the method's. A signature that names no alias or constant whose
// declarations spell different types or values is one key under condition
// 0. One that does is walked through each choice of spelling for each such
// name (goChosen), a choice no build makes cut as soon as it is made, and is
// each distinct key of the choices under the disjunction of the conditions
// of the choices that spell it: in every build, the key of the one choice
// that build makes.
func (h *hierarchy) goMethodKeys(ctx context.Context, types goPackageTypes, n goNames, name, form string, ref Ref,
	yield func(key string, spelling goSpelling, cond goCond) error) error {
	if h.goConds == nil {
		key, spelling, err := goMethodKey(ctx, types, n, name, form, ref)
		if err != nil {
			return err
		}
		return yield(key, spelling, 0)
	}
	w := &goChosen{inner: types, conds: h.goConds, groups: map[string][]goGroup{}}
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
	at := map[string]int{}
	var walk func(cond goCond) error
	walk = func(cond goCond) error {
		w.forks = w.forks[:0]
		k, sp, err := goMethodKey(ctx, w, n, name, form, ref)
		if err != nil {
			return err
		}
		if len(w.forks) == 0 {
			if i, ok := at[k]; ok {
				leaves[i].cond = h.goConds.or(leaves[i].cond, cond)
				leaves[i].spelling = max(leaves[i].spelling, sp)
				return nil
			}
			at[k] = len(leaves)
			leaves = append(leaves, leaf{k, sp, cond})
			return nil
		}
		f := w.forks[0]
		for i, g := range w.groups[f] {
			next := h.goConds.and(cond, g.cond)
			if !h.goConds.holds(next) {
				continue
			}
			w.choice[f] = i
			if err := walk(next); err != nil {
				return err
			}
		}
		delete(w.choice, f)
		return nil
	}
	if err := walk(0); err != nil {
		return err
	}
	for _, l := range leaves {
		if err := yield(l.key, l.spelling, l.cond); err != nil {
			return err
		}
	}
	return nil
}

// goChosen reads its inner goPackageTypes with one spelling chosen for each
// alias and constant named in choice (by import path, NUL, name), and lists
// in forks each one whose declarations spell more than one type or value
// that choice does not name, as goMethodKeys walks them. A name's
// declarations are grouped by what each spells (goGroup): declarations
// written differently that spell one type, or fold to one value, are one
// choice under the disjunction of their conditions, so they never multiply
// the walk. A declaration whose spelling depends on another name's choice
// is a group of its own, and choosing it forks on that name in turn. It
// sits outside the lookup memo, so a memoized answer holds every
// declaration whatever a choice picks.
type goChosen struct {
	inner  goPackageTypes
	conds  *goConds
	choice map[string]int
	forks  []string
	// groups are each name's groups, by its key, once computed.
	groups map[string][]goGroup
}

// goGroup is the declarations of one name that spell one type or value:
// the first of them, by its index among the name's declarations, and the
// disjunction of their conditions.
type goGroup struct {
	first int
	cond  goCond
}

func (w *goChosen) fork(key string) {
	if !slices.Contains(w.forks, key) {
		w.forks = append(w.forks, key)
	}
}

// group is the groups of the declarations decls of the name of key, each
// spelled by spell: "" for one whose spelling cannot be known apart from
// any choice, a group of its own.
func (w *goChosen) group(ctx context.Context, key string, decls []goDecl,
	spell func(d goDecl) (string, error)) ([]goGroup, error) {
	if g, ok := w.groups[key]; ok {
		return g, nil
	}
	var groups []goGroup
	var spellings []string
	for i, d := range decls {
		sp, err := spell(d)
		if err != nil {
			return nil, err
		}
		if j := slices.Index(spellings, sp); sp != "" && j >= 0 {
			groups[j].cond = w.conds.or(groups[j].cond, d.cond)
			continue
		}
		groups = append(groups, goGroup{first: i, cond: d.cond})
		spellings = append(spellings, sp)
	}
	w.groups[key] = groups
	return groups, nil
}

func (w *goChosen) lookup(ctx context.Context, p, name string) (goTypeAnswer, bool, error) {
	a, known, err := w.inner.lookup(ctx, p, name)
	if err != nil || !a.alias || len(a.others) == 0 {
		return a, known, err
	}
	key := "t\x00" + p + "\x00" + name
	decls := append([]goDecl{{form: a.form, names: a.names, cond: a.cond}}, a.others...)
	groups, err := w.group(ctx, key, decls, func(d goDecl) (string, error) {
		// A generic alias is spelled over its parameters, which stand for
		// any arguments alike.
		n, form, _ := goGenericAlias(d.form)
		subst := make([]string, n)
		for i := range subst {
			subst[i] = "$" + strconv.Itoa(i)
		}
		if n == 0 {
			subst = nil
		}
		var b strings.Builder
		if ok, err := goCanonical(ctx, w.inner, d.names, form, subst, 1, &b); err != nil || !ok {
			return "", err
		}
		return strconv.Itoa(n) + " " + b.String(), nil
	})
	if err != nil {
		return goTypeAnswer{}, false, err
	}
	i, chosen := w.choice[key]
	if !chosen && len(groups) > 1 {
		w.fork(key)
		return a, known, nil
	}
	g := groups[0]
	if chosen {
		g = groups[i]
	}
	d := decls[g.first]
	a.form, a.names, a.cond, a.others = d.form, d.names, g.cond, nil
	return a, known, nil
}

func (w *goChosen) constant(ctx context.Context, p, name string) ([]goDecl, error) {
	decls, err := w.inner.constant(ctx, p, name)
	if err != nil || len(decls) < 2 {
		return decls, err
	}
	key := "k\x00" + p + "\x00" + name
	groups, err := w.group(ctx, key, decls, func(d goDecl) (string, error) {
		if d.form == goIdentityUnknown {
			return "", nil
		}
		v, err := goFold(ctx, w.inner, d.names, strings.Split(d.form, " "), 1)
		if err != nil || v == nil {
			return "", err
		}
		return v.String(), nil
	})
	if err != nil {
		return nil, err
	}
	i, chosen := w.choice[key]
	if !chosen && len(groups) > 1 {
		w.fork(key)
		return decls, nil
	}
	g := groups[0]
	if chosen {
		g = groups[i]
	}
	d := decls[g.first]
	d.cond = g.cond
	return []goDecl{d}, nil
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
