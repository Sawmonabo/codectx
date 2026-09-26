package flow

// PostDom is the post-dominator tree of one Graph after exit augmentation,
// valid until the next Arena.Begin.
//
// Exit augmentation, applied to a view of the successor relation (the Graph
// itself is not changed): an edge to Exit from every node other than Exit
// with no successor, and, for every non-trivial strongly connected component
// from which Exit is not reachable, one edge to Exit from its member with the
// smallest reverse-post-order number. After it every node reaches Exit, so the
// relation is a tree rooted at Exit and IPDom is total. Without it an
// infinite loop's nodes would have no post-dominator and their control
// dependences would be silently lost.
type PostDom struct{}

// PostDominators computes the post-dominator tree by the same iterative pass
// as Dominators, run on the reversed exit-augmented CFG rooted at Exit. Its
// arrays are arena-backed (Arena.Bytes).
func PostDominators(g *Graph, a *Arena) PostDom {
	panic("flow: PostDominators is not implemented")
}

// IPDom is node n's immediate post-dominator; Exit's is Exit. It is never -1
// for a node of the Graph, reachable from Entry or not.
func (p PostDom) IPDom(n int32) int32 {
	panic("flow: PostDom.IPDom is not implemented")
}

// Augmented is the number of edges exit augmentation added: one per
// successor-less node plus one per non-trivial strongly connected component
// that could not reach Exit.
func (p PostDom) Augmented() int {
	panic("flow: PostDom.Augmented is not implemented")
}
