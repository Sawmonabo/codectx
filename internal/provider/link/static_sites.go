package link

import (
	"cmp"
	"slices"

	"github.com/Sawmonabo/codectx/internal/model"
	"github.com/Sawmonabo/codectx/internal/pagination"
	"github.com/Sawmonabo/codectx/internal/provider/treesitter/wire"
)

// Site-granular resolution of the static family (Go, Java, Rust, C, C++;
// ADR-0014 decisions 3 and 4). A member is resolved as nodes, as the flow
// family's are (refresh.go): each site is a node, and the member's own facts
// (its supertypes, overrides, routes, inputs, includes and visibility) and
// the functions it takes as values are its file node (fileNode). Each read
// is recorded with the node that made it, so a refresh re-runs, of a member
// whose table held, only the nodes whose reads changed, and carries every
// other node's facts, reads and taken functions.
//
// Two things a node uses are not its own reads:
//
//   - A site whose receiver is another site's result (`f().m()`, wire's
//     TypeResult) reads the types that site's targets return. A site that
//     re-runs therefore re-runs every site whose receiver is its result,
//     transitively (selection.closedUnder), so the dependence is taken from
//     the table and never stored.
//   - What a member resolves once and shares among its nodes (whether its own
//     imports all name the unit, closedImports; a C or C++ member's include
//     closure) is filled by whichever node asks first. Its reads are recorded
//     for the whole member (wholeFile), so a change to one of them re-runs
//     every node of the member, whichever filled it.

// nodeReads is one member's reads by node while it resolves: each key's
// hash once, by first read, and each (key, node) pair the first time the
// node reads the key after another node did, so a site's repeated lookups
// of one key cost one map probe and nothing else.
type nodeReads struct {
	cur   int32
	index map[string]int32
	keys  []string
	last  []int32
	pairs []nodeRead
}

// nodeRead is one key, by its index in nodeReads.keys, read by one node.
type nodeRead struct{ key, node int32 }

func newNodeReads() *nodeReads { return &nodeReads{cur: fileNode, index: map[string]int32{}} }

// note records key as read by the current node.
func (r *nodeReads) note(key TraceKey) error {
	k := key.hash()
	i, ok := r.index[k]
	if !ok {
		i = int32(len(r.keys))
		r.index[k] = i
		r.keys, r.last = append(r.keys, k), append(r.last, r.cur)
		r.pairs = append(r.pairs, nodeRead{key: i, node: r.cur})
		return nil
	}
	if r.last[i] != r.cur {
		r.last[i] = r.cur
		r.pairs = append(r.pairs, nodeRead{key: i, node: r.cur})
	}
	return nil
}

// whole has every read until the returned function is called recorded for
// the whole member (hierarchy.memberWide).
func (r *nodeReads) whole() func() {
	prev := r.cur
	r.cur = wholeFile
	return func() { r.cur = prev }
}

// flush adds the member's reads to out, one record per key with its nodes
// ascending, each once.
func (r *nodeReads) flush(file model.FileID, out *pagination.ExternalSort[read]) error {
	slices.SortFunc(r.pairs, func(a, b nodeRead) int { return cmp.Or(cmp.Compare(a.key, b.key), cmp.Compare(a.node, b.node)) })
	for i := 0; i < len(r.pairs); {
		j := i
		nodes := make([]int32, 0, 1)
		for ; j < len(r.pairs) && r.pairs[j].key == r.pairs[i].key; j++ {
			if n := len(nodes); n == 0 || nodes[n-1] != r.pairs[j].node {
				nodes = append(nodes, r.pairs[j].node)
			}
		}
		if err := out.Add(read{Key: r.keys[r.pairs[i].key], File: file, Nodes: nodes}); err != nil {
			return err
		}
		i = j
	}
	return nil
}

// memberWide has the reads until the returned function is called recorded
// for the whole member being resolved, when a static-family member's reads
// are recorded by node (static_sites.go); it does nothing otherwise.
func (h *hierarchy) memberWide() func() {
	if h.wide == nil {
		return func() {}
	}
	return h.wide()
}

// closedUnder is s with every site of t whose receiver is the result of a
// selected site selected too, transitively.
func (s selection) closedUnder(t *Table) selection {
	if s.whole || len(s.sites) == 0 {
		return s
	}
	for grown := true; grown; {
		grown = false
		for i := range t.Sites {
			recv := t.Sites[i].Receiver
			if recv.Kind != wire.TypeResult || s.sites[int32(i)] || !s.sites[int32(recv.Index)] {
				continue
			}
			s.sites[int32(i)], grown = true, true
		}
	}
	return s
}

// empty reports that s selects no node.
func (s selection) empty() bool { return !s.whole && !s.file && len(s.sites) == 0 }

// ran adds to out the nodes s selects of the member file: the member, when s
// is whole, else each selected site and its file node when selected.
func (s selection) ran(file model.FileID, out *pagination.ExternalSort[flowNode]) error {
	if s.whole {
		return out.Add(flowNode{File: file, Index: wholeFile})
	}
	if s.file {
		if err := out.Add(flowNode{File: file, Index: fileNode}); err != nil {
			return err
		}
	}
	for i := range s.sites {
		if err := out.Add(flowNode{File: file, Index: i}); err != nil {
			return err
		}
	}
	return nil
}

// readersOf yields every node of reads (by key) that read a key keys holds.
func readersOf(keys *pagination.ExternalSort[string], reads *pagination.SortedRun[read], yield func(flowNode) error) error {
	run, err := keys.Sorted()
	if err != nil {
		return err
	}
	defer run.Close()
	next, stop := pull(run)
	defer stop()
	key, ok, err := next()
	if err != nil {
		return err
	}
	return reads.Each(func(r read) error {
		for ok && key < r.Key {
			if key, ok, err = next(); err != nil {
				return err
			}
		}
		if !ok || key != r.Key {
			return nil
		}
		for _, n := range r.Nodes {
			if err := yield(flowNode{File: r.File, Index: n}); err != nil {
				return err
			}
		}
		return nil
	})
}

// keepReads adds to out every read of reads (by member) by a node that
// superseded does not hold, so a member's reads by the nodes that ran again
// are dropped and those of the nodes that did not are kept.
func keepReads(reads *pagination.SortedRun[read], superseded *pagination.SortedRun[flowNode], out *pagination.ExternalSort[read]) error {
	sel, err := newSelections(superseded, splitNode)
	if err != nil {
		return err
	}
	defer sel.stop()
	return reads.Each(func(r read) error {
		s, err := sel.of(r.File)
		if err != nil {
			return err
		}
		if s.whole {
			return nil
		}
		if !s.empty() {
			r.Nodes = slices.DeleteFunc(slices.Clone(r.Nodes), s.has)
		}
		if len(r.Nodes) == 0 {
			return nil
		}
		return out.Add(r)
	})
}

// fileNodesOf is the members whose file node nodes holds (an Index of
// wholeFile or fileNode), with the members of gone: those whose taken
// functions are found again.
func fileNodesOf(dir string, nodes *pagination.SortedRun[flowNode], gone *pagination.SortedRun[model.FileID]) (*pagination.SortedRun[model.FileID], error) {
	out, err := newFileIDSort(dir)
	if err != nil {
		return nil, err
	}
	defer out.Close()
	err = nodes.Each(func(n flowNode) error {
		if n.Index == wholeFile || n.Index == fileNode {
			return out.Add(n.File)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if err := gone.Each(out.Add); err != nil {
		return nil, err
	}
	return out.Sorted()
}
