package gobuild

import (
	"go/build/constraint"
	"math"
	"slices"
	"strconv"
	"strings"
	"sync"
)

// Conditions as reduced ordered binary decision diagrams (Bryant, "Graph-based
// algorithms for boolean function manipulation", IEEE TC C-35(8), 1986),
// hash-consed so that two conditions of one function share one id, and
// decided against one diagram of the valid builds: the family-based reading
// of presence conditions variability-aware analyzers use (Gazzillo and
// Grimm, PLDI 2012, section 4; Thüm et al., ACM CSUR 47(1), 2014,
// Def. 4.1).
//
// A build fixes its operating system, architecture, compiler and
// architecture feature level, so the tags naming them are fixed by the
// build's settings, never free (go help buildconstraint; internal/buildcfg
// gogoarchTags): a feature tag such as `amd64.v3` holds only on its
// architecture, and a level holds every lower level of its chain. The -tags
// flag adds only tags of no fixed meaning: a model that let it set `linux`
// on windows would make `linux && windows` hold and erase every per-port
// distinction, so a tag of fixed meaning is never a custom tag, and a tag
// shaped `<arch>.<feature>` the toolchain does not define holds in no build.
// `cgo` holds on any port (CGO_ENABLED is respected even where the port has
// no default cgo, cmd/go/internal/cfg). A release tag `go1.N` at or below the
// module's go line holds in every build, and above it each holds only where
// every lower one does. Every other tag is free.

// Cond is a condition's id in its Conds: True is every build, False none.
type Cond = uint32

const (
	True  Cond = 0
	False Cond = 1
)

// Conds is a table of build conditions. Each table decides with the release
// floor it was made with; ids of one table mean nothing in another.
type Conds struct {
	// nodes are the diagram's nodes by id, the two terminals first.
	nodes []node
	// unique is the open-addressing table of nodes by (v, lo, hi): each
	// slot a node id, 0 empty.
	unique []uint32
	used   int
	// cache is the computed table of and, or, not and meets results,
	// lossy: a collision overwrites, which costs only the recomputation.
	cache []computed
	// met is each node's satisfiability against domain once decided: 0
	// not decided, 1 met by some build, 2 by none.
	met []uint8
	// vars are the variables by tag, the order of the diagram, and fixed
	// the count of the base table's (variable).
	vars  map[string]int32
	fixed int
	// releases are the release minors above minor with a variable, sorted.
	releases []int
	// domain is the valid builds and the release chain.
	domain Cond
	minor  int
}

// node is a decision on variable v: lo when the variable is false, hi when
// it is true. The terminals decide on no variable.
type node struct {
	v      int32
	lo, hi uint32
}

const terminal = math.MaxInt32

type computed struct {
	op, a, b, r uint32
}

const (
	opAnd uint32 = iota + 1
	opOr
	opNot
	opMeets
)

// NewConds is an empty table whose builds use release go1.minor or later.
func NewConds(minor int) *Conds {
	once.Do(buildBase)
	c := &Conds{
		nodes:  slices.Clone(base.nodes),
		unique: slices.Clone(base.unique),
		used:   base.used,
		cache:  make([]computed, len(base.cache)),
		met:    slices.Clone(base.met),
		vars:   make(map[string]int32, len(base.vars)+16),
		domain: base.domain,
		fixed:  len(base.vars),
		minor:  minor,
	}
	for k, v := range base.vars {
		c.vars[k] = v
	}
	return c
}

// Satisfiable reports whether some build satisfies x: some port, compiler
// (gc or gccgo), architecture feature level, cgo setting, release of at
// least go1.minor (release tags go1.1 through the toolchain's, which is at
// least the module's), and assignment of every free tag x names.
func Satisfiable(x constraint.Expr, minor int) bool {
	c := NewConds(minor)
	return c.Holds(c.Expr(x))
}

// Expr is x's condition.
func (c *Conds) Expr(x constraint.Expr) Cond {
	switch x := x.(type) {
	case *constraint.TagExpr:
		return c.tag(x.Tag)
	case *constraint.NotExpr:
		return c.Not(c.Expr(x.X))
	case *constraint.AndExpr:
		return c.And(c.Expr(x.X), c.Expr(x.Y))
	case *constraint.OrExpr:
		return c.Or(c.Expr(x.X), c.Expr(x.Y))
	}
	return True
}

// tag is the condition of one tag holding.
func (c *Conds) tag(tag string) Cond {
	if tag == "boringcrypto" {
		tag = "goexperiment.boringcrypto"
	}
	if v, ok := c.vars[tag]; ok {
		return c.mk(v, False, True)
	}
	if r, ok := release(tag); ok {
		if r <= c.minor {
			return True
		}
		return c.mk(c.release(r), False, True)
	}
	if arch, _, ok := strings.Cut(tag, "."); ok && knownArch[arch] {
		// A feature the toolchain does not define for its architecture.
		return False
	}
	return c.mk(c.variable(tag), False, True)
}

// variable allocates tag's variable. The fixed-meaning tags and cgo, the
// base table's, come first in the order, in their own order, so a walk
// against the valid builds leaves their support early. Every later
// variable is placed above the later ones before it: extending a running
// disjunction or conjunction with a tag first seen then is one new node, not
// a copy of the whole diagram beneath it.
func (c *Conds) variable(tag string) int32 {
	v := int32(len(c.vars))
	if c.fixed > 0 {
		v = terminal - 1 - int32(len(c.vars)-c.fixed)
	}
	c.vars[tag] = v
	return v
}

// release allocates go1.r's variable, r above the floor, and adds to the
// domain its implications with its allocated neighbours: go1.r implies the
// next lower, the next higher implies go1.r. The domain's answers for
// conditions that do not name go1.r stay what they were: the chain already
// implies the next higher's implication of the next lower.
func (c *Conds) release(r int) int32 {
	v := c.variable("go1." + strconv.Itoa(r))
	i, _ := slices.BinarySearch(c.releases, r)
	if i > 0 {
		lo := c.mk(c.vars["go1."+strconv.Itoa(c.releases[i-1])], False, True)
		c.domain = c.And(c.domain, c.Or(c.mk(v, True, False), lo))
	}
	if i < len(c.releases) {
		hi := c.mk(c.vars["go1."+strconv.Itoa(c.releases[i])], False, True)
		c.domain = c.And(c.domain, c.Or(c.Not(hi), c.mk(v, False, True)))
	}
	c.releases = slices.Insert(c.releases, i, r)
	return v
}

// Not is the builds a fails in.
func (c *Conds) Not(a Cond) Cond {
	switch a {
	case True:
		return False
	case False:
		return True
	}
	if r, ok := c.lookup(opNot, a, 0); ok {
		return r
	}
	n := c.nodes[a]
	r := c.mk(n.v, c.Not(n.lo), c.Not(n.hi))
	c.store(opNot, a, 0, r)
	return r
}

// And is the builds both hold in.
func (c *Conds) And(a, b Cond) Cond {
	switch {
	case a == False || b == False:
		return False
	case a == True || a == b:
		return b
	case b == True:
		return a
	}
	return c.apply(opAnd, a, b)
}

// Or is the builds either holds in.
func (c *Conds) Or(a, b Cond) Cond {
	switch {
	case a == True || b == True:
		return True
	case a == False || a == b:
		return b
	case b == False:
		return a
	}
	return c.apply(opOr, a, b)
}

// AndNot is the builds a holds in and b does not.
func (c *Conds) AndNot(a, b Cond) Cond { return c.And(a, c.Not(b)) }

func (c *Conds) apply(op uint32, a, b Cond) Cond {
	if a > b {
		a, b = b, a
	}
	if r, ok := c.lookup(op, a, b); ok {
		return r
	}
	v, al, ah, bl, bh := c.split(a, b)
	var lo, hi Cond
	if op == opAnd {
		lo, hi = c.And(al, bl), c.And(ah, bh)
	} else {
		lo, hi = c.Or(al, bl), c.Or(ah, bh)
	}
	r := c.mk(v, lo, hi)
	c.store(op, a, b, r)
	return r
}

// split is the top variable of a and b and each one's cofactors on it.
func (c *Conds) split(a, b Cond) (v int32, al, ah, bl, bh Cond) {
	na, nb := c.nodes[a], c.nodes[b]
	v = min(na.v, nb.v)
	al, ah, bl, bh = a, a, b, b
	if na.v == v {
		al, ah = na.lo, na.hi
	}
	if nb.v == v {
		bl, bh = nb.lo, nb.hi
	}
	return v, al, ah, bl, bh
}

// Holds reports whether some valid build meets a.
func (c *Conds) Holds(a Cond) bool {
	switch a {
	case True:
		return true
	case False:
		return false
	}
	switch c.met[a] {
	case 1:
		return true
	case 2:
		return false
	}
	ok := c.meets(a, c.domain)
	c.met[a] = 2
	if ok {
		c.met[a] = 1
	}
	return ok
}

// meets reports that a and b share a satisfying assignment, without
// building their conjunction.
func (c *Conds) meets(a, b Cond) bool {
	switch {
	case a == False || b == False:
		return false
	case a == True || b == True || a == b:
		return true
	}
	if a > b {
		a, b = b, a
	}
	if r, ok := c.lookup(opMeets, a, b); ok {
		return r == True
	}
	_, al, ah, bl, bh := c.split(a, b)
	ok := c.meets(al, bl) || c.meets(ah, bh)
	r := False
	if ok {
		r = True
	}
	c.store(opMeets, a, b, r)
	return ok
}

// mk is the node deciding v between lo and hi, made once.
func (c *Conds) mk(v int32, lo, hi Cond) Cond {
	if lo == hi {
		return lo
	}
	mask := uint32(len(c.unique) - 1)
	for i := hashNode(v, lo, hi) & mask; ; i = (i + 1) & mask {
		id := c.unique[i]
		if id == 0 {
			break
		}
		if n := c.nodes[id]; n.v == v && n.lo == lo && n.hi == hi {
			return id
		}
	}
	id := uint32(len(c.nodes))
	c.nodes = append(c.nodes, node{v, lo, hi})
	c.met = append(c.met, 0)
	c.insert(id)
	if c.used*2 > len(c.unique) {
		c.grow()
	}
	return id
}

func (c *Conds) insert(id uint32) {
	n := c.nodes[id]
	mask := uint32(len(c.unique) - 1)
	i := hashNode(n.v, n.lo, n.hi) & mask
	for c.unique[i] != 0 {
		i = (i + 1) & mask
	}
	c.unique[i] = id
	c.used++
}

// grow doubles the unique table and keeps the computed table as large as
// the node count.
func (c *Conds) grow() {
	c.unique, c.used = make([]uint32, 2*len(c.unique)), 0
	for id := uint32(2); id < uint32(len(c.nodes)); id++ {
		c.insert(id)
	}
	if len(c.cache) < len(c.nodes) && len(c.cache) < maxCache {
		c.cache = make([]computed, 2*len(c.cache))
	}
}

// maxCache bounds the computed table: 2^20 entries of 16 bytes.
const maxCache = 1 << 20

func hashNode(v int32, lo, hi uint32) uint32 {
	h := uint64(uint32(v))*0x9E3779B97F4A7C15 ^ uint64(lo)*0xC2B2AE3D27D4EB4F ^ uint64(hi)*0x165667B19E3779F9
	return uint32(h>>32) ^ uint32(h)
}

func (c *Conds) slot(op, a, b uint32) *computed {
	h := uint64(op)*0x9E3779B97F4A7C15 ^ uint64(a)*0xC2B2AE3D27D4EB4F ^ uint64(b)*0x165667B19E3779F9
	return &c.cache[(uint32(h>>32)^uint32(h))&uint32(len(c.cache)-1)]
}

func (c *Conds) lookup(op, a, b uint32) (Cond, bool) {
	if e := c.slot(op, a, b); e.op == op && e.a == a && e.b == b {
		return e.r, true
	}
	return 0, false
}

func (c *Conds) store(op, a, b, r uint32) {
	*c.slot(op, a, b) = computed{op, a, b, r}
}

// base is the table every NewConds copies: the fixed-meaning variables and
// the diagram of the valid builds, its only nodes.
var (
	once sync.Once
	base *Conds
)

func buildBase() {
	order := slices.Concat(sortedKeys(knownOS), []string{"unix"}, sortedKeys(knownArch), []string{"gc", "gccgo"})
	var features []string
	for _, arch := range sortedKeys(knownArch) {
		for _, s := range featureSettings(arch) {
			for _, t := range s {
				if !slices.Contains(features, t) {
					features = append(features, t)
				}
			}
		}
	}
	slices.Sort(features)
	order = append(order, features...)
	// The valid builds, built in a scratch table, then copied alone.
	scratch := &Conds{nodes: []node{{v: terminal}, {v: terminal}}, unique: make([]uint32, 1<<12),
		cache: make([]computed, 1<<12), met: []uint8{0, 0}, vars: map[string]int32{}}
	for _, t := range order {
		scratch.variable(t)
	}
	valid := False
	for _, p := range ports {
		for _, compiler := range []string{"gc", "gccgo"} {
			for _, s := range featureSettings(p[1]) {
				on := map[string]bool{p[0]: true, p[1]: true, compiler: true, "unix": unixOS[p[0]]}
				switch p[0] {
				case "android":
					on["linux"] = true
				case "illumos":
					on["solaris"] = true
				case "ios":
					on["darwin"] = true
				}
				for _, t := range s {
					on[t] = true
				}
				cube := True
				for i := len(order) - 1; i >= 0; i-- {
					if on[order[i]] {
						cube = scratch.mk(int32(i), False, cube)
					} else {
						cube = scratch.mk(int32(i), cube, False)
					}
				}
				valid = scratch.Or(valid, cube)
			}
		}
	}
	b := &Conds{nodes: []node{{v: terminal}, {v: terminal}}, unique: make([]uint32, 1<<12),
		cache: make([]computed, 1<<10), met: []uint8{0, 0}, vars: scratch.vars}
	copied := map[Cond]Cond{True: True, False: False}
	var copyNode func(Cond) Cond
	copyNode = func(id Cond) Cond {
		if r, ok := copied[id]; ok {
			return r
		}
		n := scratch.nodes[id]
		r := b.mk(n.v, copyNode(n.lo), copyNode(n.hi))
		copied[id] = r
		return r
	}
	b.domain = copyNode(valid)
	b.variable("cgo")
	base = b
}

// featureSettings are the feature tag sets internal/buildcfg gogoarchTags
// gives architecture arch, one per valid setting of its GO386, GOAMD64,
// GOARM, GOARM64, GOMIPS, GOMIPS64, GOPPC64, GORISCV64 or GOWASM (Go
// 1.27.2): a level sets every lower level of its chain, and arm64 v9.x also
// sets v8.0 through v8.(x+5).
func featureSettings(arch string) [][]string {
	chain := func(prefix string, from, to int) [][]string {
		var out [][]string
		for k := from; k <= to; k++ {
			var l []string
			for i := from; i <= k; i++ {
				l = append(l, prefix+strconv.Itoa(i))
			}
			out = append(out, l)
		}
		return out
	}
	switch arch {
	case "386":
		return [][]string{{"386.sse2"}, {"386.softfloat"}}
	case "amd64":
		return chain("amd64.v", 1, 4)
	case "arm":
		return chain("arm.", 5, 7)
	case "arm64":
		out := chain("arm64.v8.", 0, 9)
		for minor, v9 := range chain("arm64.v9.", 0, 5) {
			out = append(out, append(v9, chain("arm64.v8.", 0, min(minor+5, 9))[min(minor+5, 9)]...))
		}
		return out
	case "mips", "mipsle", "mips64", "mips64le":
		return [][]string{{arch + ".hardfloat"}, {arch + ".softfloat"}}
	case "ppc64", "ppc64le":
		return chain(arch+".power", 8, 10)
	case "riscv64":
		return [][]string{{"riscv64.rva20u64"}, {"riscv64.rva20u64", "riscv64.rva22u64"},
			{"riscv64.rva20u64", "riscv64.rva22u64", "riscv64.rva23u64"}}
	case "wasm":
		return [][]string{{"wasm.satconv", "wasm.signext"}}
	}
	return [][]string{nil}
}

func sortedKeys(m map[string]bool) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}
