package flow

import "slices"

// ControlDependence returns the control-dependence pairs of g as
// (controller, dependent): n is control dependent on b iff b is in n's
// post-dominance frontier. The frontier is computed by the Cooper-Harvey-
// Kennedy recurrence over the exit-augmented successor relation pd was built
// on: for each node b with at least two augmented successors, walk from each
// successor up the post-dominator tree to IPDom(b), adding b to the frontier
// of every node passed. pd must come from PostDominators(g, a) in the same
// Begin.
//
// There is no entry-to-exit edge, so nothing is control dependent on Entry
// unless Entry itself branches. The result is arena-backed, sorted and
// deduplicated as Edges promises, and valid until the next Arena.Begin.
func ControlDependence(g *Graph, pd PostDom, a *Arena) Edges {
	pairs := a.Uint64s(frontierWalk(g, pd, nil))
	frontierWalk(g, pd, pairs)
	slices.Sort(pairs)
	return Edges{pairs: slices.Compact(pairs)}
}

// frontierWalk walks the frontier of every node with at least two augmented
// successors: from each successor up the post-dominator tree to the node's
// own immediate post-dominator, writing (node, passed) into out when out is
// not nil. It returns the pair count, so ControlDependence sizes its one
// arena array exactly and then fills it (Arena: 8 bytes per pair, duplicates
// included). A self-loop makes a loop head control dependent on itself. Exit
// is the tree's root and controls nothing.
//
// Every walk ends: for an edge b→s, IPDom(b) is s or a proper post-dominator
// of s, so it lies on s's path to the root.
func frontierWalk(g *Graph, pd PostDom, out []uint64) int {
	k := 0
	for b := range int32(g.Len()) {
		succ := pd.succ.of(b)
		if b == ExitNode || len(succ) < 2 {
			continue
		}
		stop := pd.ipdom[b]
		for _, r := range succ {
			for ; r != stop; r = pd.ipdom[r] {
				if out != nil {
					out[k] = pack(b, r)
				}
				k++
			}
		}
	}
	return k
}
