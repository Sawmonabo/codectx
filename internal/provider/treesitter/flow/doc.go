// Package flow is the per-function dependence core: a language-neutral
// control-flow graph, its dominance relations, control dependence and SSA
// def-use. It is pure Go — no cgo and no parse-tree import — so a lowering
// (one per language, in the parser worker) is the only code that sees syntax.
//
// # Pipeline, per function
//
//	lower     a language's lowering drives a Builder obtained from Arena.Begin
//	          and calls Builder.Finish, yielding a Graph: dense int32 node ids,
//	          node 0 the Entry, node 1 the synthetic Exit, successor and
//	          predecessor adjacency, and per node the variables it defines,
//	          each killing (Builder.Def), the variables it may define
//	          without killing (Builder.MayDef) and the distinct variables it
//	          reads; the
//	          exceptional edges of a try land on a builder-made Handler node
//	CFG → post-dominators
//	          PostDominators runs the iterative dominator pass over reverse
//	          post-order on the REVERSED graph after exit augmentation: an edge
//	          to Exit from every node with no successor, and from the
//	          smallest-reverse-post-order member of every non-trivial strongly
//	          connected component that cannot reach Exit. After augmentation
//	          every node reaches Exit, so the relation is a tree and total
//	post-dominators → control dependence
//	          ControlDependence computes the textbook control-dependence set
//	          through post-dominance frontiers over the augmented successor
//	          relation, WITHOUT the entry-to-exit edge of the original
//	          formulation; each pair is (controller, dependent)
//	CFG → def-use
//	          DefUse builds sparse SSA over the complete, sealed graph,
//	          memoized per basic block, with a non-killing may-definition as a
//	          two-operand merge and a Handler taking each predecessor's entry
//	          values; it resolves every φ transitively, so each pair is
//	          (defining node, using node); no dominance input
//
// Dominators (the forward relation rooted at Entry) is not an input of either
// result; it is exposed so the benchmarks measure the routine on the forward
// graph and for consumers that need it. PostDominators runs two depth-first
// searches (forward, for exit augmentation, and reverse, for the tree) over
// one shared scratch; Dominators, when run, allocates a third.
//
// # Lifetime
//
// One Arena per worker, used by one goroutine. Everything a Graph, an Edges,
// a PostDom or a returned slice exposes is backed by the arena and is valid
// only until the next Arena.Begin. A consumer copies out what it keeps before
// starting the next function; a stale view is not detected, it silently reads
// the next function's data.
//
// # No caps
//
// Nothing here limits a function: there is no node, edge, definition,
// variable, iteration or time limit, and no preallocation constant. The
// killing definitions and may-definitions are bounded by the lowering's
// records, like uses: a node may define several variables, so D is not
// bounded by N. Every fixed point runs to
// convergence. Each pass documents the arena it uses in terms of N, the
// edges, the variables, the definitions and uses, and, for DefUse, the
// (block, variable) pairs its lookups visit and the pairs it emits; the
// total is not linear in N alone. The design's per-function figure of 96·N + 64
// bytes is what the benchmarks compare Arena.Bytes against, not a bound the
// code guarantees, assumes or enforces.
package flow
