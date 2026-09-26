package flow

import (
	"math/bits"
	"slices"
)

// DefUse returns the def-use pairs of g as (defining node, using node): for
// every node u and every variable v in Uses(u), one pair per definition of v
// that reaches u along some CFG path.
//
// # Semantics
//
// A killing definition (Def) of v ends every path it lies on: a use it
// reaches pairs with it alone. A may-definition (MayDefs) of v at node p is
// non-killing: a use it reaches pairs with p AND with every definition of v
// reaching p's entry. Uses read the value before the node's own definitions,
// so a node can be both ends of a pair only through a loop.
//
// The value of v flowing along an edge p → n is:
//   - when n is a Handler, the value on ENTRY to p: p threw part-way, before
//     any definition it makes, so neither its Def nor its MayDefs hold there;
//   - otherwise p itself when Def(p) is v, the may-merge {p, entry(p, v)}
//     when v is one of MayDefs(p), and the value on entry to p when p does
//     not define v.
//
// This rule applies to a single-predecessor node and to every φ operand
// alike. A use that no definition reaches (a free variable, or a parameter
// the lowering gives no defining node) yields no pair.
//
// # Construction
//
// Sparse SSA by Braun et al. over the complete graph: every block is sealed
// before any lookup, so no incomplete φ remains. Values are memoized per
// basic block, not per node: a node continues its predecessor's block when it
// has exactly one predecessor, that predecessor has exactly one successor, and
// it is not a Handler; Entry and every Handler lead a block, and a cycle made
// only of such continuations is led by its lowest node id. Inside a block,
// the nearest definition of v before a node is found by binary search in v's
// definition sites, which are listed in block layout order, so no block is
// scanned.
//
// A may-merge is a two-operand φ-like value whose first operand is p itself
// and whose second is the value on entry to p. Each φ is checked for
// triviality once, when its operands are first read: a φ whose operands are
// all the φ itself or one other value is replaced by that value (by undefined
// when every operand is the φ itself). A φ that becomes trivial only through
// a later replacement is left in place. Neither case changes a pair:
// resolution walks every remaining φ transitively to the definitions it
// merges, a replacement is always one of the φ's own operands, and so a
// may-merge never loses p. No pair names a φ.
//
// The result is arena-backed, sorted and deduplicated as Edges promises, and
// valid until the next Arena.Begin. There is no definition, variable,
// iteration or depth limit, and no walk recurses.
func DefUse(g *Graph, a *Arena) Edges {
	s := &ssa{g: g, a: a, n: int32(g.Len())}
	s.layout()
	s.definitionSites()
	s.chain = a.Int32s(int(s.blocks))
	s.memo.init(a, int(s.blocks)+len(g.mayDefs))
	// Phase 1: the value live on entry to every use, and every φ it needs,
	// operands complete. Resolution waits until no φ is pending.
	vals := a.Int32s(len(g.uses))
	i := 0
	for u := range s.n {
		for _, v := range g.Uses(u) {
			vals[i] = s.walk(u, v, false)
			s.complete()
			i++
		}
	}
	// Phase 2: resolve each use through φ operands to its defining nodes.
	r := resolver{
		ssa:      s,
		phiStamp: a.Int32s(len(s.phiSite.s)),
		defStamp: a.Int32s(g.Len()),
		stack:    a.Int32s(len(s.phiSite.s)),
	}
	r.pairs.s = a.Uint64s(len(g.uses))[:0]
	i = 0
	for u := range s.n {
		for range g.Uses(u) {
			r.resolve(u, vals[i], int32(i+1))
			i++
		}
	}
	slices.Sort(r.pairs.s)
	return Edges{pairs: r.pairs.s}
}

// Value ids during construction: a definition is its node id in [0, n); φ k
// is n + k; undefined is the value on entry to Entry, to a node with no
// predecessor, and around a cycle no definition enters.
const (
	undefined int32 = -1
	// onChain marks, in the memo, a block whose entry value the current
	// walk is still computing.
	onChain int32 = -2
)

// ssa is one DefUse construction, every structure in the arena.
//
// Arena, with B blocks, V variables, D killing and M may-definitions, U uses,
// L the (block, variable) pairs the walks memoize (at most B·V), K the
// may-merges created (at most M), J the φs (joins and may-merges), P their
// operands (a join's predecessor count, two per may-merge) and R the pairs
// emitted:
//   - block layout 12·N (leader, rank, at) and the walk's chain 4·B;
//   - definition sites 4·(V+1) + 4·(D+M);
//   - the memo, 12 bytes per slot, sized for B+M entries and doubled past
//     half full, so under 96·max(B+M, L+K) bytes counting every table it
//     outgrew;
//   - the φ tables (site, variable, start, replacement, pending), under 80·J,
//     and operands, under 16·P, each a list grown by doubling that counts
//     under four times its final length;
//   - for resolution, 4·U entry values, 8·J stamps and stack, 4·N definition
//     stamps and at most 8·max(U, 4·R) bytes of pairs.
//
// No term is fixed per N: L can reach B·V, and R can exceed N.
type ssa struct {
	g      *Graph
	a      *Arena
	n      int32
	blocks int32
	// leader[n] is the first node of n's block; rank[n] is n's index in the
	// block layout, where every block's nodes are contiguous and in path
	// order; at[r] is the node of rank r.
	leader, rank, at []int32
	// The definition sites of v, killing and may, are the ranks
	// sites[siteOff[v]:siteOff[v+1]], ascending.
	siteOff, sites []int32
	// memo maps (site, variable) to a value. A site in [0, n) is a block's
	// leader, and its value is the one live on entry to the block; a site
	// n + p is node p's may-definition, and its value is p's may-merge. The
	// two ranges are disjoint, so no entry value and may-merge collide.
	memo memo
	// chain holds the blocks the walk in progress has marked onChain.
	chain []int32
	// φ k sits at site phiSite[k] (a join's block leader, or n + p for the
	// may-merge at p) for variable phiVar[k]. Its operands are
	// ops[phiStart[k]:end], end being phiStart[k+1] or, for the last φ,
	// len(ops): every φ reserves its operand slots when it is created, so the
	// count is fixed then. A join has one operand per predecessor in Pred
	// order; a may-merge has two, p and the value on entry to p.
	// replaced[k] is the value φ k was replaced by, or φ k itself.
	phiSite, phiVar, phiStart, replaced, ops list32
	// pending holds the φs whose operands are not yet read.
	pending list32
}

// continues reports whether node n continues its predecessor's block.
func (s *ssa) continues(n int32) bool {
	if n == EntryNode || s.g.Kind(n) == Handler {
		return false
	}
	preds := s.g.Pred(n)
	return len(preds) == 1 && len(s.g.Succ(preds[0])) == 1
}

// layout partitions the nodes into blocks, filling leader, rank and at: every
// node that does not continue leads a block, then every node still unplaced
// (a cycle of continuations no leader enters) leads one in id order.
func (s *ssa) layout() {
	s.leader, s.rank, s.at = s.a.Int32s(int(s.n)), s.a.Int32s(int(s.n)), s.a.Int32s(int(s.n))
	fill(s.leader, -1)
	next := int32(0)
	for n := range s.n {
		if !s.continues(n) {
			next = s.lay(n, next)
		}
	}
	for n := range s.n {
		if s.leader[n] == -1 {
			next = s.lay(n, next)
		}
	}
}

// lay places the block led by l from rank next on, following the one
// successor while it continues and is unplaced, and returns the next free
// rank.
func (s *ssa) lay(l, next int32) int32 {
	s.blocks++
	for cur := l; ; {
		s.leader[cur], s.rank[cur], s.at[next] = l, next, cur
		next++
		succ := s.g.Succ(cur)
		if len(succ) != 1 || s.leader[succ[0]] != -1 || !s.continues(succ[0]) {
			return next
		}
		cur = succ[0]
	}
}

// definitionSites lists every variable's definition sites, killing and may,
// by rank: one pass counts, one fills in rank order, so each list is sorted
// without a sort.
func (s *ssa) definitionSites() {
	vars := s.g.Vars()
	s.siteOff = s.a.Int32s(vars + 1)
	for n := range s.n {
		if d := s.g.Def(n); d >= 0 {
			s.siteOff[d+1]++
		}
		for _, v := range s.g.MayDefs(n) {
			s.siteOff[v+1]++
		}
	}
	prefix(s.siteOff)
	s.sites = s.a.Int32s(int(s.siteOff[vars]))
	// siteOff[v] serves as v's cursor and is shifted back afterwards.
	for r, n := range s.at {
		if d := s.g.Def(n); d >= 0 {
			s.sites[s.siteOff[d]] = int32(r)
			s.siteOff[d]++
		}
		for _, v := range s.g.MayDefs(n) {
			s.sites[s.siteOff[v]] = int32(r)
			s.siteOff[v]++
		}
	}
	copy(s.siteOff[1:], s.siteOff[:vars])
	s.siteOff[0] = 0
}

// lastDef is the nearest node defining v (killing or may) in n's block
// before n, or at n too when inclusive, or -1.
func (s *ssa) lastDef(n, v int32, inclusive bool) int32 {
	r := s.rank[n]
	if inclusive {
		r++
	}
	sites := window(s.sites, s.siteOff, v)
	i, _ := slices.BinarySearch(sites, r)
	if i == 0 || sites[i-1] < s.rank[s.leader[n]] {
		return -1
	}
	return s.at[sites[i-1]]
}

// walk is the value of v live on entry to node n, or just after n's own
// definitions when inclusive. It looks for a definition earlier in the block;
// failing that, the block's entry value is memoized per (block, v). A block
// with one predecessor p takes the value flowing along p → leader (see
// DefUse), found by continuing the walk at p, in a loop, not by recursion;
// every block the walk passes is marked onChain and then memoized with the
// value found. A join creates a φ, memoized before its operands are read,
// which breaks every cycle. A walk that returns to a block of its own chain
// has gone around a cycle no definition enters.
func (s *ssa) walk(n, v int32, inclusive bool) int32 {
	depth := 0
	val := undefined
	for {
		if d := s.lastDef(n, v, inclusive); d >= 0 {
			val = s.out(d, v)
			break
		}
		b := s.leader[n]
		if m, ok := s.memo.get(b, v); ok {
			if m != onChain {
				val = m
			}
			break
		}
		preds := s.g.Pred(b)
		if b == EntryNode || len(preds) == 0 {
			break
		}
		if len(preds) > 1 {
			val = s.phi(b, v)
			break
		}
		s.memo.set(s.a, b, v, onChain)
		s.chain[depth] = b
		depth++
		n, inclusive = preds[0], s.g.Kind(b) != Handler
	}
	for _, c := range s.chain[:depth] {
		s.memo.set(s.a, c, v, val)
	}
	return val
}

// out is the value of v just after node d, which defines v: d itself for a
// killing definition, d's may-merge for a may-definition.
func (s *ssa) out(d, v int32) int32 {
	if s.g.Def(d) == v {
		return d
	}
	return s.merge(d, v)
}

// phi creates the φ for v at the join led by b, one operand slot per
// predecessor, memoized as b's entry value.
func (s *ssa) phi(b, v int32) int32 {
	return s.newPhi(b, v, len(s.g.Pred(b)))
}

// merge is p's may-merge for v: memoized under site n + p, created with p as
// its first operand and the value on entry to p, read when it is completed,
// as its second.
func (s *ssa) merge(p, v int32) int32 {
	site := s.n + p
	if m, ok := s.memo.get(site, v); ok {
		return m
	}
	val := s.newPhi(site, v, 2)
	s.ops.s[s.phiStart.s[val-s.n]] = p
	return val
}

// newPhi creates φ k for v at site with count operand slots, memoizes it as
// the site's value and queues its operands.
func (s *ssa) newPhi(site, v int32, count int) int32 {
	k := int32(len(s.phiSite.s))
	val := s.n + k
	s.phiSite.push(s.a, site)
	s.phiVar.push(s.a, v)
	s.phiStart.push(s.a, int32(len(s.ops.s)))
	s.replaced.push(s.a, val)
	for range count {
		s.ops.push(s.a, undefined)
	}
	s.memo.set(s.a, site, v, val)
	s.pending.push(s.a, k)
	return val
}

// operands are φ k's operand slots.
func (s *ssa) operands(k int32) []int32 {
	end := int32(len(s.ops.s))
	if int(k)+1 < len(s.phiSite.s) {
		end = s.phiStart.s[k+1]
	}
	return s.ops.s[s.phiStart.s[k]:end]
}

// complete reads the operands of every queued φ (which may queue more), and
// removes each φ that is trivial once its operands are read.
func (s *ssa) complete() {
	for len(s.pending.s) > 0 {
		k := s.pending.s[len(s.pending.s)-1]
		s.pending.s = s.pending.s[:len(s.pending.s)-1]
		site, v, start := s.phiSite.s[k], s.phiVar.s[k], s.phiStart.s[k]
		// walk may grow ops, so each slot is indexed after it returns.
		if site >= s.n {
			val := s.walk(site-s.n, v, false)
			s.ops.s[start+1] = val
		} else {
			inclusive := s.g.Kind(site) != Handler
			for j, p := range s.g.Pred(site) {
				val := s.walk(p, v, inclusive)
				s.ops.s[start+int32(j)] = val
			}
		}
		s.removeTrivial(k)
	}
}

// removeTrivial replaces φ k by its one other operand when every operand is
// φ k itself or that one value (by undefined when every operand is φ k).
// Users are not rewritten: every read goes through find, so a memo entry or
// an operand naming φ k sees the replacement.
func (s *ssa) removeTrivial(k int32) {
	self := s.n + k
	same, seen := undefined, false
	for _, o := range s.operands(k) {
		o = s.find(o)
		if o == self || (seen && o == same) {
			continue
		}
		if seen {
			return
		}
		same, seen = o, true
	}
	s.replaced.s[k] = same
}

// find is the value v stands for after every replacement, compressing the
// replacement path it walks.
func (s *ssa) find(v int32) int32 {
	r := v
	for r >= s.n && s.replaced.s[r-s.n] != r {
		r = s.replaced.s[r-s.n]
	}
	for v >= s.n && s.replaced.s[v-s.n] != v {
		next := s.replaced.s[v-s.n]
		s.replaced.s[v-s.n] = r
		v = next
	}
	return r
}

// resolver pairs each use with the definitions its entry value stands for.
// A stamp is the use's index + 1, so no per-use clearing is needed, a φ is
// expanded once per use (the stack holds each at most once) and a
// definition met on two φ paths is paired once.
type resolver struct {
	*ssa
	phiStamp, defStamp, stack []int32
	pairs                     list64
}

// resolve emits (definition, u) for every definition value val reaches
// through φ operands.
func (r *resolver) resolve(u, val, stamp int32) {
	top := 0
	if k, ok := r.reach(u, val, stamp); ok {
		r.stack[top] = k
		top++
	}
	for top > 0 {
		top--
		for _, o := range r.operands(r.stack[top]) {
			if kk, ok := r.reach(u, o, stamp); ok {
				r.stack[top] = kk
				top++
			}
		}
	}
}

// reach pairs a definition value with u, once per stamp, and reports a φ not
// yet expanded for this stamp.
func (r *resolver) reach(u, val, stamp int32) (phi int32, ok bool) {
	val = r.find(val)
	switch {
	case val == undefined:
	case val < r.n:
		if r.defStamp[val] != stamp {
			r.defStamp[val] = stamp
			r.pairs.push(r.a, pack(val, u))
		}
	case r.phiStamp[val-r.n] != stamp:
		r.phiStamp[val-r.n] = stamp
		return val - r.n, true
	}
	return 0, false
}

// memo is an open-addressing table from (site, variable) to an int32 value,
// in arena slabs, never a Go map, so Bytes measures it. A slot's key is
// pack(site, variable) + 1, so a zeroed slot is empty. It doubles when an
// insert would take it past half full; there is no size limit.
type memo struct {
	keys  []uint64
	vals  []int32
	used  int
	shift uint
}

// init sizes the table for hint entries: the smallest power of two at least
// twice hint.
func (m *memo) init(a *Arena, hint int) {
	m.alloc(a, 2*max(hint, 1))
}

func (m *memo) alloc(a *Arena, want int) {
	b := bits.Len(uint(want - 1))
	m.keys = a.Uint64s(1 << b)
	m.vals = a.Int32s(1 << b)
	m.shift = uint(64 - b)
	m.used = 0
}

// slot is the index holding key, or the empty slot where it belongs.
func (m *memo) slot(key uint64) int {
	mask := len(m.keys) - 1
	i := int((key * 0x9e3779b97f4a7c15) >> m.shift)
	for m.keys[i] != 0 && m.keys[i] != key {
		i = (i + 1) & mask
	}
	return i
}

func (m *memo) get(site, v int32) (int32, bool) {
	i := m.slot(pack(site, v) + 1)
	return m.vals[i], m.keys[i] != 0
}

func (m *memo) set(a *Arena, site, v, val int32) {
	key := pack(site, v) + 1
	i := m.slot(key)
	if m.keys[i] == 0 {
		if 2*(m.used+1) > len(m.keys) {
			keys, vals := m.keys, m.vals
			m.alloc(a, 2*len(keys))
			for j, k := range keys {
				if k != 0 {
					at := m.slot(k)
					m.keys[at], m.vals[at] = k, vals[j]
					m.used++
				}
			}
			i = m.slot(key)
		}
		m.keys[i] = key
		m.used++
	}
	m.vals[i] = val
}
