package flow

// Dominators returns the immediate dominator of every node on the forward
// CFG rooted at Entry, indexed by node id, computed by the Cooper-Harvey-
// Kennedy iterative algorithm over reverse post-order until no entry changes.
// Entry's immediate dominator is Entry itself; a node unreachable from Entry
// has -1. The slice is arena-backed (Arena.Bytes) and valid until the next
// Arena.Begin.
//
// Neither ControlDependence nor DefUse needs it; it is exposed so the
// benchmarks measure the routine on the forward graph, and for consumers that
// need forward dominance.
func Dominators(g *Graph, a *Arena) []int32 {
	panic("flow: Dominators is not implemented")
}
