package worker

import "unsafe"

// Flat is one file's parse tree as a Go-side node array, filled by Flatten.
// It holds no native pointer and no Go pointer, so it outlives the tree it
// was flattened from and the collector never scans it. Nothing writes it
// after Flatten, so any number of readers may share it.
type Flat struct {
	nodes []Node
}

// Root is the tree's root node.
func (f *Flat) Root() *Node { return &f.nodes[0] }

// Bytes is the array's size in bytes, the figure the parse benchmark reports
// per source byte beside the tree's own.
func (f *Flat) Bytes() int { return len(f.nodes) * int(unsafe.Sizeof(Node{})) }

// Node is one node of a Flat. A *Node answers the methods the lowerings and
// the callable walk call on a native node, with the same signatures and the
// same answers, and with no native call (Walk returns a Cursor, which answers
// the native cursor's methods the same way): it navigates by the distances its
// record holds to its parent, first child and next sibling within the one
// array. Every *Node points into that array, so a Node is never copied: a
// copy is outside the array and navigates to memory that is not a node. go
// vet reports every copy (noCopy); code keeps *Node, as it kept *ts.Node.
type Node struct {
	_ noCopy
	n flatNode
}

// noCopy makes go vet's copylocks check report a copied Node. It is zero
// size and first, so Node keeps flat_node's layout.
type noCopy struct{}

func (*noCopy) Lock()   {}
func (*noCopy) Unlock() {}

// step is the node dist nodes after n (forward) or before it (back), or nil
// for dist 0.
func (n *Node) step(dist uint32, forward bool) *Node {
	if dist == 0 {
		return nil
	}
	off := int(dist) * int(unsafe.Sizeof(Node{}))
	if !forward {
		off = -off
	}
	return (*Node)(unsafe.Add(unsafe.Pointer(n), off))
}

// Id is unique among the nodes of one Flat, as the native node's id is
// among the nodes of one tree.
func (n *Node) Id() uintptr { return uintptr(unsafe.Pointer(n)) }

// KindId is the node's kind: the library's public symbol.
func (n *Node) KindId() uint16 { return uint16(n.n.kind) }

// IsNamed reports a named node.
func (n *Node) IsNamed() bool { return n.n.flags&flatNamed != 0 }

// IsExtra reports an extra node (a comment).
func (n *Node) IsExtra() bool { return n.n.flags&flatExtra != 0 }

// HasError reports that the node is, or contains, an error or a missing node.
func (n *Node) HasError() bool { return n.n.flags&flatError != 0 }

// StartByte is the node's first byte offset.
func (n *Node) StartByte() uint { return uint(n.n.start) }

// EndByte is the node's end byte offset, exclusive.
func (n *Node) EndByte() uint { return uint(n.n.end) }

// Parent is the node's parent, or nil for the root.
func (n *Node) Parent() *Node { return n.step(uint32(n.n.parent), false) }

// first is the node's first child, or nil.
func (n *Node) first() *Node { return n.step(uint32(n.n.first), true) }

// NextSibling is the node's next sibling, named or not, or nil.
func (n *Node) NextSibling() *Node { return n.step(uint32(n.n.next), true) }

// PrevSibling is the node's previous sibling, named or not, or nil. It walks
// the parent's children from the first, as the native call does.
func (n *Node) PrevSibling() *Node {
	p := n.Parent()
	if p == nil {
		return nil
	}
	var prev *Node
	for c := p.first(); c != n; c = c.NextSibling() {
		prev = c
	}
	return prev
}

// NextNamedSibling is the node's next named sibling, or nil.
func (n *Node) NextNamedSibling() *Node {
	c := n.NextSibling()
	for c != nil && !c.IsNamed() {
		c = c.NextSibling()
	}
	return c
}

// ChildCount is the number of the node's children, named or not.
func (n *Node) ChildCount() uint {
	var k uint
	for c := n.first(); c != nil; c = c.NextSibling() {
		k++
	}
	return k
}

// Child is the node's i-th child, named or not, or nil.
func (n *Node) Child(i uint) *Node {
	c := n.first()
	for ; c != nil && i > 0; i-- {
		c = c.NextSibling()
	}
	return c
}

// NamedChildCount is the number of the node's named children, extras
// included.
func (n *Node) NamedChildCount() uint {
	var k uint
	for c := n.first(); c != nil; c = c.NextSibling() {
		if c.IsNamed() {
			k++
		}
	}
	return k
}

// NamedChild is the node's i-th named child, extras included, or nil.
func (n *Node) NamedChild(i uint) *Node {
	for c := n.first(); c != nil; c = c.NextSibling() {
		if c.IsNamed() {
			if i == 0 {
				return c
			}
			i--
		}
	}
	return nil
}

// ChildByFieldId is the node's first child under field id, or nil; id 0
// names no field.
func (n *Node) ChildByFieldId(id uint16) *Node {
	if id == 0 {
		return nil
	}
	for c := n.first(); c != nil; c = c.NextSibling() {
		if uint16(c.n.field) == id {
			return c
		}
	}
	return nil
}

// Walk is a cursor rooted at n.
func (n *Node) Walk() *Cursor { return &Cursor{root: n, cur: n} }

// Cursor walks the nodes under its root, as a native tree cursor does: it
// never moves above or beside the node it was created or reset at.
type Cursor struct {
	root, cur *Node
}

// Reset roots c at n. It takes the node by pointer where the native cursor
// takes it by value, since a Node is never copied.
func (c *Cursor) Reset(n *Node) { c.root, c.cur = n, n }

// Node is the cursor's node.
func (c *Cursor) Node() *Node { return c.cur }

// FieldId is the field of the cursor's node under its parent, or 0 at the
// cursor's root, as the native cursor reports it.
func (c *Cursor) FieldId() uint16 {
	if c.cur == c.root {
		return 0
	}
	return uint16(c.cur.n.field)
}

// GotoFirstChild moves to the first child and reports whether there is one.
func (c *Cursor) GotoFirstChild() bool {
	f := c.cur.first()
	if f == nil {
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
	if s == nil {
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
