package sqlite

import (
	"container/list"
	"context"
	"database/sql"

	"github.com/Sawmonabo/codectx/internal/model"
)

// Storage-internal surrogate identities (scale-posture-plan.md S-1..S-3).
//
// A reference site holds an INTEGER surrogate, never the 32-byte canonical
// BLOB it names. A BLOB record field costs 2N+12 as its serial type plus the
// varint that encodes it -- ~33 B for a 32-byte id -- and, because node_facts,
// relation_facts and native_aliases are WITHOUT ROWID tables, every secondary
// index re-stores the whole primary key, so each of those bytes is paid once
// per index. An INTEGER PRIMARY KEY is the b-tree key itself and costs zero
// payload bytes in its own table; as a reference it is a 1-8 byte varint.
//
// THE SURROGATE NEVER LEAVES THE STORE. It is meaningful only inside one
// database file and only until the next rebuild. model.NodeID / model.RelationID
// on the wire, in every canonical hash, and in every signed pagination cursor
// stay the canonical 32-byte id, which lives exactly once in node_ids.canonical
// and relation_ids.canonical. A surrogate that reaches a cursor payload, an MCP
// response, a CLI argument or a provider is a defect, not an optimisation.
type (
	// nodeRef is node_ids.id.
	nodeRef int64
	// relRef is relation_ids.id.
	relRef int64
	// scopeRef is scope_keys.id; nativeRef is native_keys.id.
	scopeRef  int64
	nativeRef int64
)

// noRef is the zero value of every ref type and is never a valid row id: the
// schema pins CHECK(id > 0) on node_ids, relation_ids, scope_keys and
// native_keys precisely so that zero can be read as "absent" without a
// sentinel column. fact_keys relies on NULL rather than on this value.
const noRef = 0

func (r nodeRef) valid() bool { return r > 0 }
func (r relRef) valid() bool  { return r > 0 }

// nullRef renders an optional ref for a nullable INTEGER column: fact_keys and
// evidence each carry exactly one of node_id / relation_id, the other NULL.
func nullNode(r nodeRef) any {
	if !r.valid() {
		return nil
	}
	return int64(r)
}

func nullRelation(r relRef) any {
	if !r.valid() {
		return nil
	}
	return int64(r)
}

// interner resolves a canonical identity or an interned string to its surrogate
// inside one write transaction, creating the dictionary row on first sight.
//
// Contract, binding on every implementation and every caller:
//
//   - Every method is called ONLY inside the caller's write transaction and is
//     passed that transaction. The interner holds no *sql.DB and opens nothing.
//   - Resolution is upsert-and-read: INSERT ... ON CONFLICT DO NOTHING followed
//     by a SELECT of the surviving row, so two writers converge on one id.
//   - The returned id is stable for the life of the database and meaningless
//     outside it (see the type comment above).
//   - Memory is bounded by the cache size, never by repository size. The cache
//     is an LRU whose capacity is a batch-sized constant; a miss costs one
//     indexed lookup, never a re-scan. reset() is called at each batch boundary
//     by the writer so that peak RSS is a function of batch size alone.
//   - A canonical id of the wrong length, or an empty scope key, is a
//     programming error the implementation reports as an error, never a panic.
//     An empty NATIVE key is valid: model.Evidence.NativeKey is optional and
//     evidence.native_key_id is NOT NULL, so unlocated evidence interns "".
//
// Implemented by lane S4. Consumed by S1 (writer: units.go, delta.go, carry.go)
// and S3 (sweep: gc.go, retention.go, reconcile.go, snapshots.go). S2 (reader:
// query.go, search.go, adjacency.go, context.go) does NOT use the interner --
// it joins outward through node_ids/relation_ids/scope_keys/native_keys and
// hydrates canonical ids at the response boundary only.
type interner interface {
	// node resolves model.NodeID -> node_ids.id, inserting (canonical, kind,
	// canonical_key) when absent. canonicalKey is the raw 32 bytes, not hex:
	// node_ids.canonical_key is BLOB(32) after S-4.
	node(ctx context.Context, tx *sql.Tx, id model.NodeID, kind string, canonicalKey []byte) (nodeRef, error)
	// relation resolves model.RelationID -> relation_ids.id. from and to must
	// already be resolved by the caller; the interner does not recurse.
	relation(ctx context.Context, tx *sql.Tx, id model.RelationID, from nodeRef, kind string, to nodeRef) (relRef, error)
	// scopeKey and nativeKey resolve into the S-3 string dictionaries.
	scopeKey(ctx context.Context, tx *sql.Tx, key string) (scopeRef, error)
	nativeKey(ctx context.Context, tx *sql.Tx, key string) (nativeRef, error)
	// reset drops every cached entry. The writer calls it at each batch
	// boundary so that the interner's footprint never accumulates across a
	// repository-sized index run.
	reset()
}

// refCache is the bounded LRU behind every interner implementation: a map for
// O(1) lookup plus an intrusive recency list, evicting the least recently used
// entry once capacity is reached. It is deliberately NOT a whole-repository
// dictionary -- that is the heap-resident structure the scale posture forbids.
// Capacity is set by the writer from its batch sizing, so peak memory is a
// function of the batch, not of the number of distinct strings in the repo.
type refCache struct {
	capacity int
	order    *list.List // front = most recently used; values are *refEntry
	byKey    map[string]*list.Element
}

type refEntry struct {
	key string
	ref int64
}

func newRefCache(capacity int) *refCache {
	if capacity < 1 {
		capacity = 1
	}
	return &refCache{capacity: capacity, order: list.New(), byKey: make(map[string]*list.Element, capacity)}
}

func (c *refCache) get(key string) (int64, bool) {
	el, ok := c.byKey[key]
	if !ok {
		return 0, false
	}
	c.order.MoveToFront(el)
	return el.Value.(*refEntry).ref, true
}

func (c *refCache) put(key string, ref int64) {
	if el, ok := c.byKey[key]; ok {
		el.Value.(*refEntry).ref = ref
		c.order.MoveToFront(el)
		return
	}
	c.byKey[key] = c.order.PushFront(&refEntry{key: key, ref: ref})
	for c.order.Len() > c.capacity {
		oldest := c.order.Back()
		c.order.Remove(oldest)
		delete(c.byKey, oldest.Value.(*refEntry).key)
	}
}

// reset empties the cache without releasing its capacity.
func (c *refCache) reset() {
	c.order.Init()
	clear(c.byKey)
}

func (c *refCache) len() int { return c.order.Len() }
