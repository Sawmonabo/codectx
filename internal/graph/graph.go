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
	// Spools holds the frontier and visited set a continuation resumes from.
	// It is NOT an overflow area for Limits.FrontierBytes: a level that reaches
	// that ceiling stops reading and the answer says so. The
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
	// FrontierBytes caps the edges one frontier level may hold at once,
	// estimated by edgeRowBytes. A level that reaches it stops reading and the
	// answer is truncated with reasonFrontierBytes.
	FrontierBytes int64
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

// Neighbors, Callers and Callees live in traverse.go.

// References is implemented in references.go (lane L7).

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
	// now is the engine clock the deadline was measured on. A budget whose
	// deadline comes from e.now() must be compared against e.now(): mixing in
	// time.Now would make a test clock's deadline either unreachable or
	// already past. Nil means time.Now, for a budget built without a clock.
	now func() time.Time
	// frontierHit records that levelEdges stopped accumulating because the
	// frontier byte budget was spent. The walk itself is a clean finish; the
	// caller turns this into the truncation reason, because only it owns the
	// answer's meta.
	frontierHit bool
}

// clock is the budget's own clock, defaulting to time.Now.
func (b *budget) clock() time.Time {
	if b.now == nil {
		return time.Now()
	}
	return b.now()
}

// expandOptions configures one batched walk.
type expandOptions struct {
	Direction model.Direction
	Kinds     []model.RelationKind
	MaxDepth  int
	Budget    *budget
	BatchSize int
	// FrontierBytes is Limits.FrontierBytes: the ceiling on the edges one
	// frontier level may hold in memory at once. A level that reaches it stops
	// reading and the walk is reported as truncated for that reason, rather
	// than accumulating a whole hub's fan-out with no bound in front of it.
	// Zero leaves the accumulation unbounded and is only reachable from a test
	// that builds expandOptions directly.
	FrontierBytes int64
}

// expand, the ONE batched BFS every operation walks with, lives in traverse.go.

// errStopExpansion is the sentinel an expand visitor returns to end the walk
// deliberately. expand reports it as a clean finish, so the visitor owns
// whatever truncation it recorded before stopping.
var errStopExpansion = errors.New("expansion stopped by visitor")

// EvidenceRowReader is the OPTIONAL hydration seam an Adjacency may also
// implement. The frozen Adjacency.EvidenceFor returns evidence IDENTITIES,
// which is all a traversal needs to call a path evidence-backed; a reference
// occurrence additionally needs the precision class, the file and the byte
// range, and all three live on the evidence row rather than on the relation.
// Without this seam those fields would have to be left empty, and an empty
// precision is not a neutral default -- ReferenceOccurrence.Validate rejects
// it, because a fact whose derivation class is unknown cannot be trusted at
// the precision the caller assumes.
//
// It is a separate optional interface rather than a widening of Adjacency so
// the frozen port stays frozen and a test fake that has no evidence rows keeps
// compiling; References type-asserts it and falls back to the identity-only
// read, so the seam's absence costs the occurrence fields and nothing else.
type EvidenceRowReader interface {
	// EvidenceRows hydrates the evidence backing a page of relations in one
	// round trip. limit is the PER-RELATION cap, exactly as on
	// Adjacency.EvidenceFor.
	EvidenceRows(ctx context.Context, relations []model.RelationID,
		limit int) (map[model.RelationID][]model.Evidence, error)
}

// LeaseHolder is the OPTIONAL seam exposing the retention lease that pins the
// generation an answer was read from. A continuation token must name that
// lease: the token is only valid while the facts it will resume over are still
// retained, so a cursor minted without one would outlive the generation it
// pins. An Adjacency that does not hold a lease simply offers no continuation.
type LeaseHolder interface {
	LeaseID() string
}

// typedContextError is the ONE place a bare context failure becomes a Section 8
// code. Every Engine entry point routes its error through it, because a raw
// context.DeadlineExceeded reaching internal/cli is rendered as the exit-2
// invalid-argument class -- a query that ran out of time would be reported as a
// command line the operator typed wrong.
//
// An error that already carries a model.Error is returned untouched: the gate's
// CTX_RESOURCE_LIMIT and every validator rejection are more specific than the
// context state that accompanies them, and rewriting them here would erase the
// one detail the operator needs.
func typedContextError(ctx context.Context, err error) error {
	if err == nil {
		return nil
	}
	var typed *model.Error
	if errors.As(err, &typed) {
		return err
	}
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		// The deadline is the engine's own query timeout, not the caller's:
		// Options.Limits.QueryTimeout is what the entry points wrap the walk in.
		return errors.Join(&model.Error{Code: model.CodeQueryDeadline, Retryable: true,
			Message:     "the query did not finish within the configured query timeout",
			Remediation: "narrow the request bounds or raise resources.query_timeout"}, err)
	case errors.Is(err, context.Canceled):
		return model.Canceled(ctx.Err())
	}
	return err
}
