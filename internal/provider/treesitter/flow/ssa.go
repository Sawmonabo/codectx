package flow

import (
	"math/bits"
	"slices"
)

// DefUse returns the def-use pairs of g as (defining node, using node): for
// every node u and every variable v in Uses(u), one pair per definition of v
// that reaches u along some CFG path without an intervening definition of v.
//
// Construction is sparse SSA by Braun et al. over the complete graph: every
// block is sealed before any lookup, so no incomplete φ remains. Trivial φs
// (all operands the φ itself or one other value) are removed, and every
// remaining φ is resolved transitively to the definitions it merges, so no
// pair names a φ. A use that no definition reaches (a free variable, or a
// parameter the lowering gives no defining node) yields no pair. Uses
// read the value before the node's own definition, so a node can be both
// ends of a pair only through a loop.
//
// The result is arena-backed, sorted and deduplicated as Edges promises, and
// valid until the next Arena.Begin. There is no definition, variable or
// iteration limit.
func DefUse(g *Graph, a *Arena) Edges {
	uses := int(g.useOff[g.Len()])
	s := &ssa{g: g, a: a, n: int32(g.Len()), chain: a.Int32s(g.Len())}
	s.memo.init(a, uses+g.Len())
	// Phase 1: the value live on entry to every use, and every φ it needs,
	// operands complete. Resolution waits until no φ is pending.
	vals := a.Int32s(uses)
	for u := range int32(g.Len()) {
		for i := g.useOff[u]; i < g.useOff[u+1]; i++ {
			vals[i] = s.entry(u, g.uses[i])
			s.complete()
		}
	}
	// Phase 2: resolve each use through φ operands to its defining nodes.
	r := resolver{
		ssa:      s,
		phiStamp: a.Int32s(len(s.phiNode.s)),
		defStamp: a.Int32s(g.Len()),
		stack:    a.Int32s(len(s.phiNode.s)),
	}
	r.pairs.s = a.Uint64s(uses)[:0]
	for u := range int32(g.Len()) {
		for i := g.useOff[u]; i < g.useOff[u+1]; i++ {
			r.resolve(u, vals[i], i+1)
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
	// onChain marks, in the memo, a node whose entry value the current
	// single-predecessor walk is still computing.
	onChain int32 = -2
)

// ssa is one DefUse construction, every structure in the arena.
//
// Arena, with J the φ count and P the operands they hold: the memo table
// 12 bytes per slot at a load of at most one half (every chain node and φ
// site memoized once per variable read through it), the chain 4·N, the φ
// tables 16·J and operands 4·P (each grown by need), and for resolution 4·U
// entry values, 8·J + 4·N stamps and stack, and 8 bytes per pair.
type ssa struct {
	g *Graph
	a *Arena
	n int32
	// memo maps (node, variable) to the value live on entry to the node.
	memo memo
	// chain is the single-predecessor walk in progress.
	chain []int32
	// φ k sits at node phiNode[k] for variable phiVar[k]; its operands are
	// ops[phiStart[k]:], one per predecessor of its node, in Pred order.
	// replaced[k] is the value φ k was replaced by, or φ k itself.
	phiNode, phiVar, phiStart, replaced, ops list32
	// pending holds the φs whose operands are not yet read.
	pending list32
}

// exit is the value of v live on exit from node p: p's own definition if p
// defines v, else the value on entry.
func (s *ssa) exit(p, v int32) int32 {
	if s.g.Def(p) == v {
		return p
	}
	return s.entry(p, v)
}

// entry is the value of v live on entry to node n. A run of single-
// predecessor nodes is walked in a loop, not by recursion, and every node on
// it is memoized with the value found; a join creates a φ, memoized before its
// operands are read, which breaks every cycle. A walk that returns to a node
// of its own chain has gone around a cycle no definition enters.
func (s *ssa) entry(n, v int32) int32 {
	depth := 0
	val := undefined
	for cur := n; ; {
		if m, ok := s.memo.get(cur, v); ok {
			if m != onChain {
				val = m
			}
			break
		}
		preds := s.g.Pred(cur)
		if cur == EntryNode || len(preds) == 0 {
			break
		}
		if len(preds) > 1 {
			val = s.phi(cur, v)
			break
		}
		s.memo.set(s.a, cur, v, onChain)
		s.chain[depth] = cur
		depth++
		p := preds[0]
		if s.g.Def(p) == v {
			val = p
			break
		}
		cur = p
	}
	for _, c := range s.chain[:depth] {
		s.memo.set(s.a, c, v, val)
	}
	return val
}

// phi creates the φ for v at join node n with its operand slots, memoizes it
// as n's entry value and queues its operands.
func (s *ssa) phi(n, v int32) int32 {
	k := int32(len(s.phiNode.s))
	val := s.n + k
	s.phiNode.push(s.a, n)
	s.phiVar.push(s.a, v)
	s.phiStart.push(s.a, int32(len(s.ops.s)))
	s.replaced.push(s.a, val)
	for range s.g.Pred(n) {
		s.ops.push(s.a, undefined)
	}
	s.memo.set(s.a, n, v, val)
	s.pending.push(s.a, k)
	return val
}

// complete reads the operands of every queued φ (which may queue more), and
// removes each φ that is trivial once its operands are read.
func (s *ssa) complete() {
	for len(s.pending.s) > 0 {
		k := s.pending.s[len(s.pending.s)-1]
		s.pending.s = s.pending.s[:len(s.pending.s)-1]
		v, start := s.phiVar.s[k], s.phiStart.s[k]
		for j, p := range s.g.Pred(s.phiNode.s[k]) {
			// exit may grow ops, so the slot is indexed after it returns.
			val := s.exit(p, v)
			s.ops.s[start+int32(j)] = val
		}
		s.removeTrivial(k)
	}
}

// removeTrivial replaces φ k by its one other operand when every operand is
// φ k itself or that one value (by undefined when every operand is φ k).
// Users are not rewritten: every read goes through find, so a memo entry or
// an operand naming φ k sees the replacement. A φ that becomes trivial only
// through a later replacement is left in place; DefUse resolves through it to
// the same definitions its replacement would reach, so the pairs are the same.
func (s *ssa) removeTrivial(k int32) {
	self := s.n + k
	same, seen := undefined, false
	start := s.phiStart.s[k]
	for j := range int32(len(s.g.Pred(s.phiNode.s[k]))) {
		o := s.find(s.ops.s[start+j])
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
		k := r.stack[top]
		start := r.phiStart.s[k]
		for j := range int32(len(r.g.Pred(r.phiNode.s[k]))) {
			if kk, ok := r.reach(u, r.ops.s[start+j], stamp); ok {
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

// memo is an open-addressing table from (node, variable) to an int32 value,
// in arena slabs, never a Go map, so Bytes measures it. A slot's key is
// pack(node, variable) + 1, so a zeroed slot is empty. It doubles when an
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

func (m *memo) get(n, v int32) (int32, bool) {
	i := m.slot(pack(n, v) + 1)
	return m.vals[i], m.keys[i] != 0
}

func (m *memo) set(a *Arena, n, v, val int32) {
	key := pack(n, v) + 1
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
