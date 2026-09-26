package flow

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
	panic("flow: ControlDependence is not implemented")
}
