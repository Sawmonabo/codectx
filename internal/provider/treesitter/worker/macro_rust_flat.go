package worker

import (
	"cmp"
	"slices"
)

// flatSpliced marks, in a flat node's flags, a token tree whose children are
// a kept macro re-parse's top-level items rather than its tokens
// (spliceMacros); the walk's own flags use the bits below it.
const flatSpliced = flatExtra << 1

// spliced reports a macro token tree whose children are its re-parsed
// items.
func (n Node) spliced() bool { return n.rec().flags&flatSpliced != 0 }

// spliceMacros is the file's flat array main with every kept macro body's
// re-parse in place of the tokens of the token tree it re-parses: that token
// tree's children are the re-parse's top-level nodes, each with its subtree,
// and it is marked spliced. The dependence pass and the link tables then
// read the items a macro's input writes as they read every other item, in
// one array whose nodes keep the file's own offsets. Each re-parse is
// flattened once; the array is rebuilt in one preorder walk over the trees
// and the field tables are carried over by each node's new index, so the
// cost is linear in the nodes of the file and its re-parses. A file with no
// kept body answers main itself. The trees may be closed once it returns.
func (m *macroBodies) spliceMacros(main *Flat) (*Flat, error) {
	if m == nil || len(m.at) == 0 {
		return main, nil
	}
	subs := map[span]*Flat{}
	total := len(main.nodes)
	for _, bodies := range m.at {
		for _, b := range bodies {
			f, err := Flatten(b.tree, "rust")
			if err != nil {
				return nil, err
			}
			subs[b.tt] = f
			total += len(f.nodes)
		}
	}
	s := &splicer{subs: subs, tt: rsMacroSyntaxOf().tokenTree, out: make([]flatNode, 0, total),
		pos: map[*Flat][]uint32{}}
	s.emit(main, 0, 0)
	fields := make([]flatField, 0, len(main.fields))
	for f, pos := range s.pos {
		for _, e := range f.fields {
			n, c := pos[uint32(e.node)], pos[uint32(e.child)]
			if n == unspliced || c == unspliced {
				continue
			}
			setU32(&e.node, n)
			setU32(&e.child, c)
			fields = append(fields, e)
		}
	}
	// One node's entries come from one array, in that array's order.
	slices.SortStableFunc(fields, func(a, b flatField) int { return cmp.Compare(uint32(a.node), uint32(b.node)) })
	return &Flat{nodes: s.out, fields: fields, registered: main.registered}, nil
}

// unspliced is a node's index in the spliced array before it is emitted, and
// for good when it is a token the splice replaced.
const unspliced = ^uint32(0)

// splicer builds a spliced flat array.
type splicer struct {
	subs map[span]*Flat
	tt   uint16
	out  []flatNode
	// pos maps each source array's node index to its index in out.
	pos map[*Flat][]uint32
}

// emit appends node i of f and its subtree to out under the node at index
// parent (ignored for the first node, the root) and answers its index.
func (s *splicer) emit(f *Flat, i, parent uint32) uint32 {
	pos := s.pos[f]
	if pos == nil {
		pos = make([]uint32, len(f.nodes))
		for k := range pos {
			pos[k] = unspliced
		}
		s.pos[f] = pos
	}
	o := uint32(len(s.out))
	rec := f.nodes[i]
	setU32(&rec.first, 0)
	setU32(&rec.next, 0)
	setU32(&rec.parent, 0)
	if o > 0 {
		setU32(&rec.parent, o-parent)
	}
	from, at := f, i
	if uint16(rec.kind) == s.tt {
		if sub := s.subs[span{uint(rec.start), uint(rec.end)}]; sub != nil {
			from, at = sub, 0
			setU8(&rec.flags, uint8(rec.flags)|flatSpliced)
		}
	}
	s.out = append(s.out, rec)
	pos[i] = o
	prev := unspliced
	for c := uint32(from.nodes[at].first); c != 0; {
		child := at + c
		k := s.emit(from, child, o)
		if prev == unspliced {
			setU32(&s.out[o].first, k-o)
		} else {
			setU32(&s.out[prev].next, k-prev)
		}
		prev = k
		next := uint32(from.nodes[child].next)
		if next == 0 {
			break
		}
		at, c = child, next
	}
	return o
}

// setU32 and setU8 store v into a field of the walk's records, whose
// integer types are the C library's.
func setU32[T ~uint32](p *T, v uint32) { *p = T(v) }
func setU8[T ~uint8](p *T, v uint8)    { *p = T(v) }

// lastChild is n's last child, or null when it has none.
func lastChild(n Node) Node {
	var last Node
	for c := n.first(); !c.IsNull(); c = c.next() {
		last = c
	}
	return last
}
