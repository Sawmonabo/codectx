package worker

import "github.com/Sawmonabo/codectx/internal/provider/treesitter/lang"

// Flat is one file's parse tree as a Go-side node array, filled by Flatten.
// It holds no native pointer and no Go pointer, so it outlives the tree it
// was flattened from and the collector never scans it. Nothing writes it
// after Flatten, so any number of readers may share it.
type Flat struct {
	nodes []flatNode
}

// Root is the tree's root node.
func (f *Flat) Root() Node { return Node{f: f} }

// parseErrors is what the tree f was flattened from reports about its errors:
// whether it has an error or a missing node, and the bytes its outermost
// ERROR nodes (errorKind) cover, so a byte inside nested error nodes counts
// once. It reads the array alone and descends only into nodes that contain an
// error.
func (f *Flat) parseErrors(errorKind uint16) lang.ParseErrors {
	root := f.Root()
	pe := lang.ParseErrors{Any: root.HasError()}
	if !pe.Any {
		return pe
	}
	c := root.Walk()
	for {
		n := c.Node()
		if n.KindId() == errorKind {
			pe.Bytes += uint64(n.EndByte() - n.StartByte())
		} else if n.HasError() && c.GotoFirstChild() {
			continue
		}
		for !c.GotoNextSibling() {
			if !c.GotoParent() {
				return pe
			}
		}
	}
}

// Node is a handle on one node of a Flat: the array and the node's index in
// it. It is a value: a copy names the same node. The zero Node is null, the
// answer where the native API answers nil. A Node answers the methods the
// lowerings and the callable walk call on a native node, with the same
// answers and no native call (Walk returns a Cursor, which answers the
// native cursor's methods the same way). Calling any method but IsNull on a
// null Node panics, as calling one on a nil native node does.
type Node struct {
	f *Flat
	i uint32
}

// IsNull reports the null Node.
func (n Node) IsNull() bool { return n.f == nil }

// rec is n's record.
func (n Node) rec() *flatNode { return &n.f.nodes[n.i] }

// Id is unique among the nodes of one Flat, as the native node's id is
// among the nodes of one tree.
func (n Node) Id() uintptr { return uintptr(n.i) }

// KindId is the node's kind: the library's public symbol.
func (n Node) KindId() uint16 { return uint16(n.rec().kind) }

// IsNamed reports a named node.
func (n Node) IsNamed() bool { return n.rec().flags&flatNamed != 0 }

// IsExtra reports an extra node (a comment).
func (n Node) IsExtra() bool { return n.rec().flags&flatExtra != 0 }

// HasError reports that the node is, or contains, an error or a missing node.
func (n Node) HasError() bool { return n.rec().flags&flatError != 0 }

// StartByte is the node's first byte offset.
func (n Node) StartByte() uint { return uint(n.rec().start) }

// EndByte is the node's end byte offset, exclusive.
func (n Node) EndByte() uint { return uint(n.rec().end) }

// Parent is the node's parent, or null for the root.
func (n Node) Parent() Node {
	d := uint32(n.rec().parent)
	if d == 0 {
		return Node{}
	}
	return Node{f: n.f, i: n.i - d}
}

// first is the node's first child, or null.
func (n Node) first() Node {
	d := uint32(n.rec().first)
	if d == 0 {
		return Node{}
	}
	return Node{f: n.f, i: n.i + d}
}

// NextSibling is the node's next sibling, named or not, or null.
func (n Node) NextSibling() Node {
	d := uint32(n.rec().next)
	if d == 0 {
		return Node{}
	}
	return Node{f: n.f, i: n.i + d}
}

// PrevSibling is the node's previous sibling, named or not, or null. It
// walks the parent's children from the first, as the native call does.
func (n Node) PrevSibling() Node {
	p := n.Parent()
	if p.IsNull() {
		return Node{}
	}
	var prev Node
	for c := p.first(); c != n; c = c.NextSibling() {
		prev = c
	}
	return prev
}

// NextNamedSibling is the node's next named sibling, or null.
func (n Node) NextNamedSibling() Node {
	c := n.NextSibling()
	for !c.IsNull() && !c.IsNamed() {
		c = c.NextSibling()
	}
	return c
}

// ChildCount is the number of the node's children, named or not.
func (n Node) ChildCount() uint {
	var k uint
	for c := n.first(); !c.IsNull(); c = c.NextSibling() {
		k++
	}
	return k
}

// Child is the node's i-th child, named or not, or null.
func (n Node) Child(i uint) Node {
	c := n.first()
	for ; !c.IsNull() && i > 0; i-- {
		c = c.NextSibling()
	}
	return c
}

// NamedChildCount is the number of the node's named children, extras
// included.
func (n Node) NamedChildCount() uint {
	var k uint
	for c := n.first(); !c.IsNull(); c = c.NextSibling() {
		if c.IsNamed() {
			k++
		}
	}
	return k
}

// NamedChild is the node's i-th named child, extras included, or null.
func (n Node) NamedChild(i uint) Node {
	for c := n.first(); !c.IsNull(); c = c.NextSibling() {
		if c.IsNamed() {
			if i == 0 {
				return c
			}
			i--
		}
	}
	return Node{}
}

// ChildByFieldId is the node's first child under field id, or null; id 0
// names no field.
func (n Node) ChildByFieldId(id uint16) Node {
	if id == 0 {
		return Node{}
	}
	for c := n.first(); !c.IsNull(); c = c.NextSibling() {
		if uint16(c.rec().field) == id {
			return c
		}
	}
	return Node{}
}

// Walk is a cursor rooted at n.
func (n Node) Walk() *Cursor { return &Cursor{root: n, cur: n} }

// Cursor walks the nodes under its root, as a native tree cursor does: it
// never moves above or beside the node it was created or reset at. It holds
// two handles and no native state.
type Cursor struct {
	root, cur Node
}

// Reset roots c at n.
func (c *Cursor) Reset(n Node) { c.root, c.cur = n, n }

// Node is the cursor's node.
func (c *Cursor) Node() Node { return c.cur }

// FieldId is the field of the cursor's node under its parent, or 0 at the
// cursor's root, as the native cursor reports it.
func (c *Cursor) FieldId() uint16 {
	if c.cur == c.root {
		return 0
	}
	return uint16(c.cur.rec().field)
}

// GotoFirstChild moves to the first child and reports whether there is one.
func (c *Cursor) GotoFirstChild() bool {
	f := c.cur.first()
	if f.IsNull() {
		return false
	}
	c.cur = f
	return true
}

// GotoNextSibling moves to the next sibling and reports whether there is one;
// the root has none.
func (c *Cursor) GotoNextSibling() bool {
	if c.cur == c.root {
		return false
	}
	s := c.cur.NextSibling()
	if s.IsNull() {
		return false
	}
	c.cur = s
	return true
}

// GotoParent moves to the parent and reports whether there is one; the root
// has none.
func (c *Cursor) GotoParent() bool {
	if c.cur == c.root {
		return false
	}
	c.cur = c.cur.Parent()
	return true
}

// Close has nothing to release: the cursor holds no native state. It is
// kept so a cursor is closed exactly where a native one is.
func (c *Cursor) Close() {}
