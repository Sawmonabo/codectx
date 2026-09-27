package worker

/*
#include <stdbool.h>
#include <stdint.h>

// The parse-tree library's public types this file passes across its API,
// declared here as its api.h declares them: the header ships inside the
// binding's module, which no build of this package can name. The size
// assertions below check each against the binding's wrapper of the same
// struct, which is compiled against that header.
typedef struct TSTree TSTree;
typedef struct TSNode {
	uint32_t context[4];
	const void *id;
	const TSTree *tree;
} TSNode;
typedef struct TSTreeCursor {
	const void *tree;
	const void *id;
	uint32_t context[3];
} TSTreeCursor;

// The library's public functions the walk calls. They are defined in the
// binding's compiled library, which every binary that links this package
// also links.
TSNode ts_tree_root_node(const TSTree *self);
uint32_t ts_node_descendant_count(TSNode self);
uint16_t ts_node_symbol(TSNode self);
uint32_t ts_node_start_byte(TSNode self);
uint32_t ts_node_end_byte(TSNode self);
bool ts_node_is_named(TSNode self);
bool ts_node_is_missing(TSNode self);
bool ts_node_is_extra(TSNode self);
bool ts_node_has_error(TSNode self);
TSTreeCursor ts_tree_cursor_new(TSNode node);
void ts_tree_cursor_delete(TSTreeCursor *self);
TSNode ts_tree_cursor_current_node(const TSTreeCursor *self);
uint16_t ts_tree_cursor_current_field_id(const TSTreeCursor *self);
bool ts_tree_cursor_goto_first_child(TSTreeCursor *self);
bool ts_tree_cursor_goto_next_sibling(TSTreeCursor *self);
bool ts_tree_cursor_goto_parent(TSTreeCursor *self);

enum {
	flat_named = 1,
	flat_missing = 2,
	flat_error = 4,
	flat_extra = 8,
};

// flat_node is one node of the flat array. parent is the distance back to
// the parent, first and next the distances forward to the first child and
// the next sibling, all counted in nodes; 0 is none, since no node is its
// own parent, child or sibling. The array is in preorder, so a parent lies
// before its node and a first child and a next sibling after it, and each
// distance is unsigned in its own direction: it spans any tree the
// library's own uint32_t descendant count can describe.
typedef struct flat_node {
	uint32_t start, end;
	uint32_t parent, first, next;
	uint16_t kind, field;
	uint8_t flags;
} flat_node;

// flat_size is the number of nodes the flat array of tree holds: the root
// and its visible descendants, the nodes a tree cursor visits.
static uint32_t flat_size(const TSTree *tree) {
	return ts_node_descendant_count(ts_tree_root_node(tree));
}

static void flat_record(flat_node *n, const TSTreeCursor *c) {
	TSNode x = ts_tree_cursor_current_node(c);
	n->start = ts_node_start_byte(x);
	n->end = ts_node_end_byte(x);
	n->kind = ts_node_symbol(x);
	n->field = ts_tree_cursor_current_field_id(c);
	n->flags = (ts_node_is_named(x) ? flat_named : 0) | (ts_node_is_missing(x) ? flat_missing : 0) |
		(ts_node_has_error(x) ? flat_error : 0) | (ts_node_is_extra(x) ? flat_extra : 0);
}

// flat_fill walks tree in preorder with one tree cursor and records every
// node it visits into out, which holds cap zeroed nodes. It writes nothing
// past cap: it returns the number of nodes the walk visited, and stops at
// cap + 1 when the walk would visit more, so a count that differs from cap is
// reported rather than written.
static uint64_t flat_fill(const TSTree *tree, flat_node *out, uint32_t cap) {
	if (cap == 0) {
		return 1;
	}
	TSTreeCursor c = ts_tree_cursor_new(ts_tree_root_node(tree));
	flat_record(&out[0], &c);
	uint32_t n = 1, cur = 0;
	for (;;) {
		bool child = ts_tree_cursor_goto_first_child(&c);
		if (!child) {
			while (!ts_tree_cursor_goto_next_sibling(&c)) {
				if (!ts_tree_cursor_goto_parent(&c)) {
					ts_tree_cursor_delete(&c);
					return n;
				}
				cur -= out[cur].parent;
			}
		}
		if (n == cap) {
			ts_tree_cursor_delete(&c);
			return (uint64_t)cap + 1;
		}
		uint32_t parent = child ? cur : cur - out[cur].parent;
		if (child) {
			out[cur].first = n - cur;
		} else {
			out[cur].next = n - cur;
		}
		flat_record(&out[n], &c);
		out[n].parent = n - parent;
		cur = n++;
	}
}
*/
import "C"

import (
	"fmt"
	"unsafe"

	ts "github.com/tree-sitter/go-tree-sitter"
)

// flatNode is the walk's flat_node, one element of a Flat's array.
type flatNode = C.flat_node

// The record's flags, as the walk sets them.
const (
	flatNamed   = C.flat_named
	flatMissing = C.flat_missing
	flatError   = C.flat_error
	flatExtra   = C.flat_extra
)

// The layout checks. Each fails the build, rather than letting the walk read
// wrong memory, when the binding changes a wrapper or the library changes a
// struct this file declares:
//
//   - the tree wrapper is exactly one pointer wide, so its only field is the
//     native tree handle Flatten reads (a root-node test in flatten_test.go
//     checks that the pointer is that handle);
//   - the node and tree-cursor wrappers are each exactly one native struct
//     wide, so the declarations above, which the walk passes to and receives
//     from the library by value, have the library's size.
var (
	_ = [1]struct{}{}[unsafe.Sizeof(ts.Tree{})-unsafe.Sizeof(unsafe.Pointer(nil))]
	_ = [1]struct{}{}[unsafe.Sizeof(unsafe.Pointer(nil))-unsafe.Sizeof(ts.Tree{})]
	_ = [1]struct{}{}[unsafe.Sizeof(ts.Node{})-C.sizeof_TSNode]
	_ = [1]struct{}{}[C.sizeof_TSNode-unsafe.Sizeof(ts.Node{})]
	_ = [1]struct{}{}[unsafe.Sizeof(ts.TreeCursor{})-C.sizeof_TSTreeCursor]
	_ = [1]struct{}{}[C.sizeof_TSTreeCursor-unsafe.Sizeof(ts.TreeCursor{})]
)

// Flatten copies tree into a Go-side node array in preorder: every node's
// kind, field, flags, byte range, parent, first child and next sibling. It
// makes two native calls per tree, one to size the array and one to fill it,
// and none per node; the walk runs in the library,
// over its public tree-cursor API. It only reads tree, so it may run at any
// point before tree is closed: a header's parse is flattened to weigh its
// errors before the structural queries run on the parse that is kept. The
// caller may close tree as soon as it returns and the queries are done with
// it: the array holds no native pointer.
//
// A node's kind is the library's public symbol, the id KindId returns, which
// the library already gives to every symbol of one name and one visibility
// (the id Language.IdForNodeKind resolves), so each lowering's once-resolved
// kind ids compare with it unchanged. Its field is the field the tree cursor
// reports for it under its parent.
//
// The native tree handle is read from the binding's tree wrapper, whose only
// field it is (see the layout checks above).
func Flatten(tree *ts.Tree) (*Flat, error) {
	h := *(**C.TSTree)(unsafe.Pointer(tree))
	n := C.flat_size(h)
	if n == 0 {
		return nil, fmt.Errorf("flatten: the tree counts no root node")
	}
	nodes := make([]flatNode, n)
	if visited := C.flat_fill(h, &nodes[0], n); uint64(visited) != uint64(n) {
		return nil, fmt.Errorf("flatten: the tree cursor visited %d nodes where the tree counts %d", uint64(visited), uint64(n))
	}
	return &Flat{nodes: nodes}, nil
}

// Bytes is the array's size in bytes, the figure the parse benchmark reports
// per source byte beside the tree's own.
func (f *Flat) Bytes() int { return len(f.nodes) * C.sizeof_flat_node }
