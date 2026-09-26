package flow

// PostDom is the post-dominator tree of one Graph after exit augmentation,
// valid until the next Arena.Begin.
//
// Exit augmentation, applied to a view of the successor relation (the Graph
// itself is not changed): an edge to Exit from every node other than Exit
// with no successor, and, for every non-trivial strongly connected component
// from which Exit is not reachable, one edge to Exit from its member with the
// smallest reverse-post-order number. After it every node reaches Exit, so the
// relation is a tree rooted at Exit and every node has an immediate
// post-dominator. Without it an infinite loop's nodes would have no
// post-dominator and their control dependences would be silently lost.
type PostDom struct {
	// ipdom is the immediate post-dominator of every node, Exit's being Exit;
	// never -1, reachable from Entry or not. ControlDependence walks it.
	ipdom []int32
	// succ is the exit-augmented successor relation the tree was built on;
	// ControlDependence walks its frontier over it.
	succ csr
	// augmented is the number of edges the augmentation added.
	augmented int
}

// PostDominators computes the post-dominator tree by the same iterative pass
// as Dominators, run on the reversed exit-augmented CFG rooted at Exit. Its
// arrays are arena-backed (Arena.Bytes).
func PostDominators(g *Graph, a *Arena) PostDom {
	toExit, augmented := exitAugmentation(g, a)
	succ, pred := augment(g, toExit, augmented, a)
	return PostDom{
		ipdom:     dominatorTree(pred, succ, ExitNode, a),
		succ:      succ,
		augmented: augmented,
	}
}

// Augmented is the number of edges exit augmentation added: one per
// successor-less node plus one per non-trivial strongly connected component
// that could not reach Exit.
func (p PostDom) Augmented() int { return p.augmented }

// exitAugmentation returns the set of nodes that receive an edge to Exit, as
// a bitset over node ids, and its size.
//
// A node other than Exit with no successor gets one. Then Tarjan's strongly
// connected components, iterative, over every node in id order: components
// are emitted sinks first, so when one is emitted every component it has an
// edge into has already been decided. A component reaches Exit when it holds
// Exit or a successor-less node, or a member has an edge to Exit or into an
// emitted component that reaches it. A non-trivial component (two or more
// members, or one member with a self-loop) that does not gets one edge from
// its header and then reaches Exit. By induction every component then
// reaches Exit, so only sink components are ever augmented.
//
// The header is the member first in the forward reverse post-order from
// Entry; members Entry does not reach order after every reachable node, by
// node id.
//
// Arena: forward order 16·N, Tarjan's index, lowlink, component stack, call
// stack and edge cursor 20·N, and two bitsets of ⌈N/64⌉ words.
func exitAugmentation(g *Graph, a *Arena) (toExit []uint64, count int) {
	n := g.Len()
	fwd := g.successors()
	_, rpo := reversePostOrder(fwd, EntryNode, a)
	rank := func(v int32) int {
		if rpo[v] >= 0 {
			return int(rpo[v])
		}
		return n + int(v)
	}

	words := (n + 63) / 64
	toExit = a.Uint64s(words)
	reaches := a.Uint64s(words)
	// index[v]: 0 unvisited; > 0 the depth-first index while v's component
	// is open; -(c+1) once v was emitted in component c.
	index := a.Int32s(n)
	low := a.Int32s(n)
	members := a.Int32s(n)
	calls := a.Int32s(n)
	cursor := a.Int32s(n)
	next, comp := int32(0), int32(0)
	top := 0
	for root := range int32(n) {
		if index[root] != 0 {
			continue
		}
		next++
		index[root], low[root], cursor[root] = next, next, fwd.off[root]
		members[top], calls[0] = root, root
		top++
		depth := 1
		for depth > 0 {
			v := calls[depth-1]
			if cursor[v] < fwd.off[v+1] {
				w := fwd.adj[cursor[v]]
				cursor[v]++
				switch {
				case index[w] == 0:
					next++
					index[w], low[w], cursor[w] = next, next, fwd.off[w]
					members[top], calls[depth] = w, w
					top++
					depth++
				case index[w] > 0:
					low[v] = min(low[v], index[w])
				}
				continue
			}
			depth--
			if depth > 0 {
				u := calls[depth-1]
				low[u] = min(low[u], low[v])
			}
			if low[v] != index[v] {
				continue
			}
			// v is the root of a component: members[bottom:top].
			bottom := top - 1
			for members[bottom] != v {
				bottom--
			}
			scc := members[bottom:top]
			done := -(comp + 1)
			for _, m := range scc {
				index[m] = done
			}
			reached, cyclic := false, len(scc) > 1
			header := scc[0]
			for _, m := range scc {
				succ := fwd.of(m)
				switch {
				case m == ExitNode:
					reached = true
				case len(succ) == 0:
					setBit(toExit, m)
					count++
					reached = true
				}
				for _, w := range succ {
					switch {
					case w == m:
						cyclic = true
					case index[w] != done && bit(reaches, w):
						reached = true
					}
				}
				if rank(m) < rank(header) {
					header = m
				}
			}
			if !reached && cyclic {
				setBit(toExit, header)
				count++
				reached = true
			}
			if reached {
				for _, m := range scc {
					setBit(reaches, m)
				}
			}
			top = bottom
			comp++
		}
	}
	return toExit, count
}

// augment builds the exit-augmented successor relation and its reverse: g's
// successors plus an edge to Exit from every node in toExit, each list kept
// ascending. Only Exit's predecessor list changes. Arena: 8·(N+1) + 8·(E+A).
func augment(g *Graph, toExit []uint64, count int, a *Arena) (succ, pred csr) {
	n := g.Len()
	succ = csr{a.Int32s(n + 1), a.Int32s(int(g.succOff[n]) + count)}
	pred = csr{a.Int32s(n + 1), a.Int32s(int(g.predOff[n]) + count)}
	k := int32(0)
	for v := range int32(n) {
		succ.off[v] = k
		out := g.Succ(v)
		if bit(toExit, v) {
			// Exit is node 1: only Entry sorts before it.
			for len(out) > 0 && out[0] < ExitNode {
				succ.adj[k] = out[0]
				k++
				out = out[1:]
			}
			succ.adj[k] = ExitNode
			k++
		}
		k += int32(copy(succ.adj[k:], out))
	}
	succ.off[n] = k
	k = 0
	for v := range int32(n) {
		pred.off[v] = k
		in := g.Pred(v)
		if v != ExitNode {
			k += int32(copy(pred.adj[k:], in))
			continue
		}
		// Merge Exit's own predecessors with the augmented sources; the two
		// are disjoint, since a node with an edge to Exit is never augmented.
		for u := range int32(n) {
			if !bit(toExit, u) {
				continue
			}
			for len(in) > 0 && in[0] < u {
				pred.adj[k] = in[0]
				k++
				in = in[1:]
			}
			pred.adj[k] = u
			k++
		}
		k += int32(copy(pred.adj[k:], in))
	}
	pred.off[n] = k
	return succ, pred
}

func bit(s []uint64, i int32) bool { return s[i>>6]&(1<<(uint(i)&63)) != 0 }
func setBit(s []uint64, i int32)   { s[i>>6] |= 1 << (uint(i) & 63) }
