package flow

import "slices"

// Dominators returns the immediate dominator of every node on the forward
// CFG rooted at Entry, indexed by node id, computed by the Cooper-Harvey-
// Kennedy iterative algorithm over reverse post-order until no entry changes.
// Entry's immediate dominator is Entry itself; a node unreachable from Entry
// has -1. The slice is arena-backed (Arena.Bytes) and valid until the next
// Arena.Begin. Arena: 20·N, a depth-first scratch of its own and the result.
//
// Neither ControlDependence nor DefUse needs it; it is exposed so the
// benchmarks measure the routine on the forward graph, and for consumers that
// need forward dominance.
func Dominators(g *Graph, a *Arena) []int32 {
	d := newDFS(g.Len(), a)
	return dominatorTree(g.successors(), g.predecessors(), EntryNode, &d, a)
}

// csr is one direction of an adjacency in compressed sparse row form: node
// n's neighbours are adj[off[n]:off[n+1]].
type csr struct{ off, adj []int32 }

func (c csr) of(n int32) []int32 { return window(c.adj, c.off, n) }

func (g *Graph) successors() csr   { return csr{g.succOff, g.succ} }
func (g *Graph) predecessors() csr { return csr{g.predOff, g.pred} }

// dominatorTree is the one dominator routine, used for the forward graph and
// for the reversed exit-augmented graph: the immediate dominator of every
// node on the graph whose successors are out and whose predecessors are in,
// rooted at root. The root's entry is itself; a node the root does not reach
// over out has -1.
//
// Cooper-Harvey-Kennedy: sweep the reachable nodes in reverse post-order,
// setting each node's dominator to the intersection of its processed
// predecessors', until a sweep changes nothing. The sweep count is not
// bounded by a constant; it runs to convergence. The order is computed in
// search, which it overwrites; the arena holds only the 4·N result.
func dominatorTree(out, in csr, root int32, search *dfs, a *Arena) []int32 {
	order, num := search.reversePostOrder(out, root)
	idom := a.Int32s(len(num))
	fill(idom, -1)
	idom[root] = root
	for changed := true; changed; {
		changed = false
		for _, b := range order[1:] {
			d := int32(-1)
			for _, p := range in.of(b) {
				if idom[p] == -1 {
					// Unreachable from root, or not yet reached by this
					// sweep: it contributes nothing yet.
					continue
				}
				if d == -1 {
					d = p
				} else {
					d = intersect(idom, num, p, d)
				}
			}
			if idom[b] != d {
				idom[b] = d
				changed = true
			}
		}
	}
	return idom
}

// intersect is the two-finger walk: the nearest common dominator of x and y
// in the tree built so far. num is the reverse-post-order number, so the
// finger deeper in the order moves up.
func intersect(idom, num []int32, x, y int32) int32 {
	for x != y {
		for num[x] > num[y] {
			x = idom[x]
		}
		for num[y] > num[x] {
			y = idom[y]
		}
	}
	return x
}

// dfs is the scratch of one iterative depth-first search over N nodes: num,
// order, stack and cursor, 16·N in the arena. Every array is written before
// it is read on each use, so one dfs serves several searches in sequence, and
// its arrays serve as scratch between them.
type dfs struct{ num, order, stack, cursor []int32 }

func newDFS(n int, a *Arena) dfs {
	return dfs{num: a.Int32s(n), order: a.Int32s(n), stack: a.Int32s(n), cursor: a.Int32s(n)}
}

// reversePostOrder is an iterative depth-first search from root over out,
// with an explicit stack and a per-node edge cursor, so the depth is not the
// goroutine stack's. order lists the nodes root reaches in reverse
// post-order (order[0] is root); num[n] is n's index in order, or -1 when
// root does not reach n. Both are views of d, valid until d's next use.
func (d *dfs) reversePostOrder(out csr, root int32) (order, num []int32) {
	num, order, stack, cursor := d.num, d.order, d.stack, d.cursor
	fill(num, -1)
	// num[v] = 0 marks v visited until its final number is written below.
	num[root] = 0
	stack[0], cursor[root] = root, out.off[root]
	sp, done := 1, 0
	for sp > 0 {
		v := stack[sp-1]
		if cursor[v] < out.off[v+1] {
			w := out.adj[cursor[v]]
			cursor[v]++
			if num[w] == -1 {
				num[w] = 0
				cursor[w] = out.off[w]
				stack[sp] = w
				sp++
			}
			continue
		}
		sp--
		order[done] = v
		done++
	}
	order = order[:done:done]
	slices.Reverse(order)
	for i, v := range order {
		num[v] = int32(i)
	}
	return order, num
}

// fill sets every element of s to v.
func fill(s []int32, v int32) {
	for i := range s {
		s[i] = v
	}
}
