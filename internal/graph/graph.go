// Package graph answers bounded traversal, dependency-path and impact queries
// over the canonical fact store. It accepts already-resolved NodeIDs and never
// resolves names itself, so it does not depend on search; every fact it reads
// arrives through the Adjacency port, so it does not depend on storage either.
// Every queue, batch, traversal and response it produces carries an explicit
// finite bound, and a zero bound on a request means "the configured default",
// never "unlimited".
package graph

import (
	"context"
	"errors"
	"time"

	"github.com/Sawmonabo/codectx/internal/model"
	"github.com/Sawmonabo/codectx/internal/pagination"
)

// adjacencyBatch is the number of frontier NodeIDs sent to Adjacency.Edges in
// one round trip. It bounds both the SQL IN-list and the rows a single call can
// return, which is what keeps a high fan-out hub from turning one expansion
// step into an unbounded query.
const adjacencyBatch = 256

// Adjacency is the only way the engine reads facts. It is deliberately batched:
// a per-node reader would turn every expansion step into N+1 queries against
// the relation indexes.
type Adjacency interface {
	// Edges returns every visible edge touching any of nodes in direction, restricted to kinds,
	// keyset-ordered by relation id after `after`, at most limit rows. One round trip per call.
	//
	// An empty kinds slice means "no kind filter", not "no rows": callers resolve
	// DefaultRelations before calling, so an empty slice is a caller that wants every
	// kind. limit must be positive -- every batch carries an explicit finite bound --
	// and an implementation may reject a non-positive limit with CTX_ARGUMENT_INVALID.
	Edges(ctx context.Context, nodes []model.NodeID, direction model.Direction,
		kinds []model.RelationKind, after model.RelationID, limit int) ([]model.Relation, error)
	// NodesByID hydrates the nodes a traversal admitted, in one round trip.
	NodesByID(ctx context.Context, ids []model.NodeID) ([]model.Node, error)
	// EvidenceFor hydrates the evidence backing a page of relations, in one
	// round trip, so a returned path is evidence-backed without one query per edge.
	EvidenceFor(ctx context.Context, relations []model.RelationID, limit int) (map[model.RelationID][]model.EvidenceID, error)
	// Capabilities reports the pinned generation's capability states, including
	// the deferred dependence rows an incomplete answer must disclose.
	Capabilities(ctx context.Context) ([]model.CapabilityState, error)
	// Binding is the pinned generation the answer is bound to.
	Binding() model.Binding
}

// PendingUnits mirrors index.Pending's three fields locally so internal/graph never imports the
// coordinator package. app adapts index.Pending into it.
type PendingUnits struct {
	Units, Position int
	Estimate        time.Duration
}

// Promoter raises the priority of a deferred provider build so a query that
// needs those facts does not wait behind unrelated work.
type Promoter interface { // nil in report mode; a failed promotion never fails the query
	Promote(ctx context.Context, providerID, scopeKey string) (PendingUnits, error)
}

// Gate is the PROCESS-scoped max_concurrent_graph_queries semaphore. It is owned by the app stack
// and passed in, because Workspace.Query builds one Engine per request: a per-Engine semaphore
// would gate nothing. Acquire honours the request deadline and returns CTX_RESOURCE_LIMIT.
type Gate interface {
	Acquire(ctx context.Context) error
	Release()
}

// Options configures one Engine. Adjacency is the only required field; the
// rest are optional ports whose absence disables exactly the feature they
// serve, so a caller that does not page or gate need not supply a fake.
type Options struct {
	Adjacency Adjacency
	// Promoter is nil in report mode, where the coordinator is not writable.
	Promoter Promoter
	// Signer is nil when continuations are not offered; a request that asks to
	// page without one is answered as truncated with no NextCursor.
	Signer *pagination.Signer
	// Spools holds traversal state that overflows Limits.FrontierBytes. The
	// frozen strategy is ONE FRESH SPOOL PER PAGE: pagination.Spools.Open
	// replays every record from the start and cannot seek, so a replay-and-skip
	// continuation would cost O(pages x records) and re-read a growing spool on
	// every page. Nil disables spilling; an overflowing traversal then truncates.
	Spools *pagination.Spools
	Gate   Gate // process-scoped; never per-Engine. Nil means the caller gates elsewhere.
	// Limits is resolved from config.Context/config.Resources by the caller.
	// The engine never reads config itself, so a cost or bound can never differ
	// between two callers holding the same Limits.
	Limits Limits
	// Now is the clock used for deadlines and cursor expiry; nil means time.Now.
	Now func() time.Time
}

// Limits is the resolved budget list for one request. Every field is a hard
// bound, and the visited and edge budgets are cumulative across the pages of
// one traversal rather than reset per page.
type Limits struct {
	MaxDepth, MaxVisited, MaxEdges, MaxPageItems, MaxReasonPaths int
	QueryTimeout, CursorTTL                                      time.Duration
	FrontierBytes                                                int64
}

// Engine answers graph queries against one pinned generation. One Engine is
// built per request, which is why the concurrency Gate is passed in rather than
// owned here.
type Engine struct {
	adjacency Adjacency
	promoter  Promoter
	signer    *pagination.Signer
	spools    *pagination.Spools
	gate      Gate
	limits    Limits
	now       func() time.Time
}

// New builds an Engine. Adjacency is required; Promoter, Signer, Spools and
// Gate are optional as documented on Options, and a nil Now defaults to
// time.Now. Every Limits field must be positive: a zero bound is resolved to
// the configured default by the caller before it reaches here, so a zero
// arriving at New is a wiring defect, not a request asking for "unlimited".
func New(o Options) (*Engine, error) {
	if o.Adjacency == nil {
		return nil, &model.Error{Code: model.CodeArgumentInvalid,
			Message: "graph engine requires an adjacency reader"}
	}
	for _, b := range []struct {
		field string
		value int64
	}{
		{"max_depth", int64(o.Limits.MaxDepth)},
		{"max_visited", int64(o.Limits.MaxVisited)},
		{"max_edges", int64(o.Limits.MaxEdges)},
		{"max_page_items", int64(o.Limits.MaxPageItems)},
		{"max_reason_paths", int64(o.Limits.MaxReasonPaths)},
		{"query_timeout", int64(o.Limits.QueryTimeout)},
		{"cursor_ttl", int64(o.Limits.CursorTTL)},
		{"frontier_bytes", o.Limits.FrontierBytes},
	} {
		if b.value <= 0 {
			return nil, (&model.Error{Code: model.CodeArgumentInvalid,
				Message: "graph engine limit must be positive"}).WithDetail("limit", b.field)
		}
	}
	now := o.Now
	if now == nil {
		now = time.Now
	}
	return &Engine{
		adjacency: o.Adjacency,
		promoter:  o.Promoter,
		signer:    o.Signer,
		spools:    o.Spools,
		gate:      o.Gate,
		limits:    o.Limits,
		now:       now,
	}, nil
}

// notImplemented is the typed stub every operation returns until its owning
// lane lands its body. It is CTX_INTERNAL because reaching it is a defect in
// this package, never a caller error.
func notImplemented(op string) error {
	return (&model.Error{Code: model.CodeInternal,
		Message: "graph operation is not implemented"}).WithDetail("operation", op)
}

// Neighbors expands the request's seeds in the request's own direction over its
// relation allowlist, reporting the direction it walked and the visited and
// edge counts it spent.
func (e *Engine) Neighbors(ctx context.Context, req model.GraphRequest) (model.GraphResult, error) {
	return model.GraphResult{}, notImplemented("neighbors")
}

// Callers expands incoming `calls` edges. It pins both the direction and the
// relation itself; a request whose Direction contradicts that is rejected with
// CTX_ARGUMENT_INVALID rather than having the field silently ignored.
func (e *Engine) Callers(ctx context.Context, req model.GraphRequest) (model.GraphResult, error) {
	return model.GraphResult{}, notImplemented("callers")
}

// Callees expands outgoing `calls` edges, pinning direction and relation the
// same way Callers does.
func (e *Engine) Callees(ctx context.Context, req model.GraphRequest) (model.GraphResult, error) {
	return model.GraphResult{}, notImplemented("callees")
}

// References is implemented in references.go (lane L7).

// ShortestPath runs a nonnegative integer-cost Dijkstra over the request's
// relation allowlist. An exhausted depth, visited budget or deadline is
// reported as truncation together with the paths found so far -- never as "no
// path exists", which is reserved for a genuinely unreachable target.
func (e *Engine) ShortestPath(ctx context.Context, req model.PathRequest) (model.PathResult, error) {
	return model.PathResult{}, notImplemented("shortest_path")
}

// Impact ranks the entities a change to the seeds may affect. Every entry
// carries the direction that made it affected -- incoming means it may need
// modification, outgoing that it may need reading -- and at least one
// evidence-backed reason naming the relation kind in product vocabulary.
func (e *Engine) Impact(ctx context.Context, req model.ImpactRequest) (model.ImpactResult, error) {
	return model.ImpactResult{}, notImplemented("impact")
}

// PackageDependencies rolls symbol-level edges up to their package or module
// container nodes and reports distinct (from-package, to-package) pairs with
// their evidence counts. A rollup pair is never presented as a precise symbol
// call.
func (e *Engine) PackageDependencies(ctx context.Context, req model.GraphRequest) (model.Page[model.PackageEdge], error) {
	return model.Page[model.PackageEdge]{}, notImplemented("package_dependencies")
}

// frontierState is one admitted node's position in a walk: how far it sits from
// the nearest seed, what it cost to reach, and the edge that reached it.
type frontierState struct {
	Depth int
	Cost  int64
	Node  model.NodeID
	Via   model.RelationID
}

// budget is the cumulative, cursor-carried work allowance of one traversal. It
// does NOT reset per page: a resumed page that is already at a cap answers
// truncated with no continuation rather than spending the budget again.
type budget struct {
	visited, edges int64
	deadline       time.Time
}

// expandOptions configures one batched walk.
type expandOptions struct {
	Direction model.Direction
	Kinds     []model.RelationKind
	MaxDepth  int
	Budget    *budget
	BatchSize int
}

// expand is the ONE batched BFS. L1 owns its body in traverse.go; L0 ships a stub so L3 compiles
// and codes against this exact signature in parallel. visit is called once per admitted edge in
// the frozen (depth asc, NodeID asc) order and may return errStopExpansion to end the walk.
func expand(ctx context.Context, a Adjacency, seeds []model.NodeID, o expandOptions,
	visit func(frontierState, model.Relation) error) error {
	return notImplemented("expand")
}

var errStopExpansion = errors.New("expansion stopped by visitor")
