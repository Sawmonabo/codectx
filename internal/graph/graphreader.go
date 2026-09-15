package graph

import (
	"context"
	"errors"

	"github.com/Sawmonabo/codectx/internal/model"
)

// NodeRef and RelRef are the generation-local integer surrogates of a node and
// a relation (node_ids.id and relation_ids.id, ADR-0002). Zero is never a node
// or a relation. A surrogate is meaningful only inside the generation the
// reader is pinned to: a cursor that carries one is fenced by that generation
// id, and a reader for another generation must never be handed it.
type NodeRef uint64

// RelRef is the relation counterpart of NodeRef.
type RelRef uint64

// KindCode indexes the generation's relation-kind dictionary. Codes are dense
// from 1; 0 is unused so a zero value is never mistaken for a kind.
type KindCode uint8

// KindTable is one generation's relation-kind dictionary, both ways.
type KindTable interface {
	Code(kind model.RelationKind) (KindCode, bool)
	Kind(code KindCode) (model.RelationKind, bool)
	Len() int
}

// Edge is one packed adjacency entry as the reader streams it.
type Edge struct {
	// Owner is the node whose list this entry belongs to.
	Owner NodeRef
	// Neighbour is the other endpoint.
	Neighbour NodeRef
	Rel       RelRef
	Kind      KindCode
	// Outgoing is true when Owner is the relation's from-node.
	Outgoing bool
}

// EdgePos is a resumable position inside one Neighbours scan: the owner node
// and the index of the next stored entry in that node's list. Index counts
// stored entries, before any kind filtering, so a position is stable whatever
// kinds a later scan asks for.
type EdgePos struct {
	Node  NodeRef
	Index uint32
}

// IsZero reports the beginning-of-scan position.
func (p EdgePos) IsZero() bool { return p.Node == 0 && p.Index == 0 }

// ErrStopScan, returned from a Neighbours callback, ends the scan without an
// error. Neighbours then reports the position of the entry that was NOT
// delivered, so a scan resumed at that position delivers it first.
var ErrStopScan = errors.New("graph: stop scan")

// GraphReader is the only way a traversal reads structure (ADR-0005). It is
// pinned to one generation, reads the packed per-generation adjacency and its
// side arrays, and never hands out a surrogate from another generation.
type GraphReader interface {
	// Binding is the pinned generation the answer is bound to.
	Binding() model.Binding
	// MaxNode is the largest node surrogate present in the generation; a
	// visited bitset is sized from it.
	MaxNode() NodeRef
	// MaxRelation is the largest relation surrogate present in the
	// generation. It bounds the walk's cumulative emitted-relation set the
	// same way MaxNode bounds the visited set, so a surrogate from another
	// generation is refused by RANGE and not only by the cursor's fence.
	MaxRelation() RelRef
	// Kinds is the generation's relation-kind dictionary.
	Kinds() KindTable
	// Resolve maps canonical ids to surrogates, 0 for an id that is not
	// visible in this generation. Results align with ids.
	Resolve(ctx context.Context, ids []model.NodeID) ([]NodeRef, error)
	// NodeIDs maps surrogates back to canonical ids in one batched
	// primary-key read. Results align with refs.
	NodeIDs(ctx context.Context, refs []NodeRef) ([]model.NodeID, error)
	// RelationIDs is the relation counterpart of NodeIDs.
	RelationIDs(ctx context.Context, refs []RelRef) ([]model.RelationID, error)
	// Neighbours streams the packed lists of refs, which MUST be ascending
	// and duplicate-free, in direction, visiting entries in (owner, list
	// index) order and starting AFTER from (the zero EdgePos means the
	// beginning). A nil kinds means every kind. DirectionBoth visits the
	// outgoing list of each owner and then its incoming list. It returns the
	// position of the last entry delivered, or, when fn returned ErrStopScan,
	// the position of the entry it did not deliver, so a caller that stops
	// early on a deadline or a budget resumes exactly there. Context errors
	// surface raw; the walk classifies them.
	Neighbours(ctx context.Context, refs []NodeRef, direction model.Direction, kinds []KindCode,
		from EdgePos, fn func(Edge) error) (EdgePos, error)
	// NodeKinds reads the node-kind side array; results align with refs.
	NodeKinds(ctx context.Context, refs []NodeRef) ([]model.NodeKind, error)
	// Containers reads each node's container surrogate, 0 when it has none.
	Containers(ctx context.Context, refs []NodeRef) ([]NodeRef, error)
	// SourceBytes reads the source size of file nodes, 0 for any other node.
	SourceBytes(ctx context.Context, refs []NodeRef) ([]int64, error)
	// EvidenceCounts reads the evidence count of each relation.
	EvidenceCounts(ctx context.Context, rels []RelRef) ([]int64, error)
	// NodesByID hydrates a page of nodes for delivery, in one round trip.
	NodesByID(ctx context.Context, ids []model.NodeID) ([]model.Node, error)
	// EvidenceFor hydrates the evidence backing a page of relations, in one
	// round trip, so a returned path is evidence-backed.
	EvidenceFor(ctx context.Context, relations []model.RelationID, limit int) (map[model.RelationID][]model.EvidenceID, error)
	// Capabilities reports the pinned generation's capability states.
	Capabilities(ctx context.Context) ([]model.CapabilityState, error)
}
