package flow

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
	panic("flow: DefUse is not implemented")
}
