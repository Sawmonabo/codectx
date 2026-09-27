package worker

/*
#include <stdbool.h>
#include <stddef.h>
#include <stdint.h>
#include <stdlib.h>

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
uint32_t ts_node_child_count(TSNode self);
bool ts_node_is_named(TSNode self);
bool ts_node_is_missing(TSNode self);
bool ts_node_is_extra(TSNode self);
bool ts_node_has_error(TSNode self);
TSNode ts_node_child_by_field_id(TSNode self, uint16_t field_id);
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

// flat_field is one entry of the field table: the library's
// ts_node_child_by_field_id answers child, a node index, for node and field.
// Entries are in preorder of node, so the table is sorted by it.
typedef struct flat_field {
	uint32_t node, child;
	uint16_t field;
} flat_field;

// flat_answer is an entry's answer before it is resolved to an index: the
// answering node's native id and byte range.
typedef struct flat_answer {
	const void *id;
	uint32_t start, end;
} flat_answer;

enum {
	flat_ok = 0,
	flat_overrun = 1,
	flat_no_memory = 2,
	flat_unresolved = 3,
};

// flat_result is what flat_fill reports: a status, and on flat_ok the field
// table, allocated here, which the caller frees.
typedef struct flat_result {
	int status;
	flat_field *fields;
	size_t len;
} flat_result;

// flat_size is the number of nodes the flat array of tree holds: the root
// and its visible descendants, the nodes a tree cursor visits.
static uint32_t flat_size(const TSTree *tree) {
	return ts_node_descendant_count(ts_tree_root_node(tree));
}

// flat_table is the field table while the walk builds it.
typedef struct flat_table {
	flat_field *at;
	flat_answer *ans;
	size_t len, cap;
} flat_table;

// flat_add appends node's answer a for field, growing the table by
// doubling; it reports false when the memory is not there.
static bool flat_add(flat_table *t, uint32_t node, uint16_t field, TSNode a) {
	if (t->len == t->cap) {
		size_t cap = t->cap ? 2 * t->cap : 64;
		flat_field *at = realloc(t->at, cap * sizeof *at);
		if (!at) {
			return false;
		}
		t->at = at;
		flat_answer *ans = realloc(t->ans, cap * sizeof *ans);
		if (!ans) {
			return false;
		}
		t->ans = ans;
		t->cap = cap;
	}
	t->at[t->len] = (flat_field){.node = node, .field = field};
	t->ans[t->len] = (flat_answer){.id = a.id, .start = ts_node_start_byte(a), .end = ts_node_end_byte(a)};
	t->len++;
	return true;
}

// flat_record records the cursor's node as node i, its native id into
// ids[i], and, for an internal node, the library's answer for each of the
// nfields registered fields that has one into t.
static bool flat_record(flat_node *out, const void **ids, uint32_t i, const TSTreeCursor *c, flat_table *t,
		const uint16_t *fields, uint32_t nfields) {
	TSNode x = ts_tree_cursor_current_node(c);
	flat_node *n = &out[i];
	ids[i] = x.id;
	n->start = ts_node_start_byte(x);
	n->end = ts_node_end_byte(x);
	n->kind = ts_node_symbol(x);
	n->field = ts_tree_cursor_current_field_id(c);
	n->flags = (ts_node_is_named(x) ? flat_named : 0) | (ts_node_is_missing(x) ? flat_missing : 0) |
		(ts_node_has_error(x) ? flat_error : 0) | (ts_node_is_extra(x) ? flat_extra : 0);
	if (nfields == 0 || ts_node_child_count(x) == 0) {
		return true;
	}
	for (uint32_t k = 0; k < nfields; k++) {
		TSNode a = ts_node_child_by_field_id(x, fields[k]);
		if (a.id && !flat_add(t, i, fields[k], a)) {
			return false;
		}
	}
	return true;
}

// flat_find is the index of the node under owner whose native id is a's, or
// 0, which no descendant is. The answer is a descendant: a visible child, or
// a node inside a child the field map descends into (an inherited field
// through an aliased child). The search descends only into nodes whose
// byte range holds the answer's, so it passes over every other subtree.
static uint32_t flat_find(const flat_node *out, const void *const *ids, uint32_t owner, const flat_answer *a) {
	if (!out[owner].first) {
		return 0;
	}
	uint32_t c = owner + out[owner].first;
	for (;;) {
		if (ids[c] == a->id) {
			return c;
		}
		if (out[c].first && out[c].start <= a->start && a->end <= out[c].end) {
			c += out[c].first;
			continue;
		}
		while (!out[c].next) {
			c -= out[c].parent;
			if (c == owner) {
				return 0;
			}
		}
		c += out[c].next;
	}
}

// flat_fill walks tree in preorder with one tree cursor and records every
// node it visits into out, which holds cap zeroed nodes, and the field table
// of the nfields registered fields. It writes nothing past cap: a walk that
// would visit more nodes reports flat_overrun, as does one that visits fewer.
// Once the walk is done, every answer is resolved to its node's index,
// which the preorder walk had not reached when it asked.
static flat_result flat_fill(const TSTree *tree, flat_node *out, uint32_t cap, const uint16_t *fields,
		uint32_t nfields) {
	flat_result res = {.status = flat_overrun};
	if (cap == 0) {
		return res;
	}
	const void **ids = malloc((size_t)cap * sizeof *ids);
	if (!ids) {
		res.status = flat_no_memory;
		return res;
	}
	flat_table t = {0};
	TSTreeCursor c = ts_tree_cursor_new(ts_tree_root_node(tree));
	bool ok = flat_record(out, ids, 0, &c, &t, fields, nfields);
	uint32_t n = 1, cur = 0;
	while (ok) {
		bool child = ts_tree_cursor_goto_first_child(&c);
		if (!child) {
			bool done = false;
			while (!ts_tree_cursor_goto_next_sibling(&c)) {
				if (!ts_tree_cursor_goto_parent(&c)) {
					done = true;
					break;
				}
				cur -= out[cur].parent;
			}
			if (done) {
				res.status = n == cap ? flat_ok : flat_overrun;
				break;
			}
		}
		if (n == cap) {
			break;
		}
		uint32_t parent = child ? cur : cur - out[cur].parent;
		if (child) {
			out[cur].first = n - cur;
		} else {
			out[cur].next = n - cur;
		}
		ok = flat_record(out, ids, n, &c, &t, fields, nfields);
		out[n].parent = n - parent;
		cur = n++;
	}
	ts_tree_cursor_delete(&c);
	if (!ok) {
		res.status = flat_no_memory;
	}
	for (size_t k = 0; res.status == flat_ok && k < t.len; k++) {
		t.at[k].child = flat_find(out, ids, t.at[k].node, &t.ans[k]);
		if (t.at[k].child == 0) {
			res.status = flat_unresolved;
		}
	}
	free(ids);
	free(t.ans);
	if (res.status != flat_ok) {
		free(t.at);
		return res;
	}
	res.fields = t.at;
	res.len = t.len;
	return res;
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

// flatField is the walk's flat_field, one entry of a Flat's field table.
type flatField = C.flat_field

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

// Flatten copies tree, parsed with the grammar registered as language, into
// a Go-side node array in preorder: every node's kind, field, flags, byte
// range, parent, first child and next sibling, and the field table of the
// language's field registry (grammars.go). It makes three native calls per
// tree, one to size the array, one to fill it and one to free the fill's
// field table once it is copied, and none per node; the walk runs in the
// library, over its public tree-cursor and node API. It only reads tree, so
// it may run at any point before tree is closed. The caller may close tree
// as soon as it returns and the queries are done with it: the array holds no
// native pointer.
//
// A node's kind is the library's public symbol, the id KindId returns, which
// the library already gives to every symbol of one name and one visibility
// (the id Language.IdForNodeKind resolves), so each lowering's once-resolved
// kind ids compare with it unchanged. Its field is the field the tree cursor
// reports for it under its parent, the nearest one, which Cursor.FieldId
// answers.
//
// The field table is what ChildByFieldId answers from: for every internal
// node and every registered field, the fill asks the library's
// ts_node_child_by_field_id, which reads the production's field map, and
// keeps each answer that is not null as a (field, node index) pair. A field
// map says more than the cursor's nearest field: a field inherited through a
// hidden or an aliased child (the answer then lies below that child), an
// outer field on a node that also has a nearer one, and no field at all on an
// ERROR node, whose production has no map. The table costs one C-side lookup
// per internal node per registered field, and holds only the answers.
//
// The native tree handle is read from the binding's tree wrapper, whose only
// field it is (see the layout checks above).
func Flatten(tree *ts.Tree, language string) (*Flat, error) {
	g, ok := grammars[language]
	if !ok {
		return nil, fmt.Errorf("flatten: language %q has no linked grammar", language)
	}
	return flatten(tree, g.fieldRegistry())
}

// flatten is Flatten with the field registry reg. A header's parse weighed
// for its errors alone is flattened with an empty registry: no field is read
// from that array, so it asks the library for none.
func flatten(tree *ts.Tree, reg *fieldRegistry) (*Flat, error) {
	h := *(**C.TSTree)(unsafe.Pointer(tree))
	n := C.flat_size(h)
	if n == 0 {
		return nil, fmt.Errorf("flatten: the tree counts no root node")
	}
	nodes := make([]flatNode, n)
	var ids *C.uint16_t
	if len(reg.ids) > 0 {
		ids = (*C.uint16_t)(unsafe.Pointer(&reg.ids[0]))
	}
	res := C.flat_fill(h, &nodes[0], n, ids, C.uint32_t(len(reg.ids)))
	switch res.status {
	case C.flat_ok:
	case C.flat_overrun:
		return nil, fmt.Errorf("flatten: the tree cursor did not visit the %d nodes the tree counts", uint64(n))
	case C.flat_no_memory:
		return nil, fmt.Errorf("flatten: no memory for the field table of a tree of %d nodes", uint64(n))
	default:
		return nil, fmt.Errorf("flatten: a field's answer is no node under the node it was asked of")
	}
	var fields []flatField
	if res.len > 0 {
		fields = make([]flatField, res.len)
		copy(fields, unsafe.Slice((*flatField)(unsafe.Pointer(res.fields)), res.len))
	}
	C.free(unsafe.Pointer(res.fields))
	return &Flat{nodes: nodes, fields: fields, registered: reg.known}, nil
}

// Bytes is the size in bytes of the array and its field table, the figure
// the parse benchmark reports per source byte beside the tree's own.
func (f *Flat) Bytes() int {
	return len(f.nodes)*C.sizeof_flat_node + len(f.fields)*C.sizeof_flat_field
}
