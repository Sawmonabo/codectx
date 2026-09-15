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

	"github.com/Sawmonabo/codectx/internal/config"
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
	// Leases mints the CURSOR-SCOPED retention lease every continuation token
	// names. It is not the pinned reader's query lease: that one is released
	// when the request returns, so a token naming it is refused by the spool
	// store on the very next invocation. Nil means continuations are not
	// offered, exactly like a nil Signer or Spools.
	Leases *pagination.Leases
	Gate   Gate // process-scoped; never per-Engine. Nil means the caller gates elsewhere.
	// Limits is resolved from config.Context/config.Resources by the caller.
	// The engine never reads config itself, so a cost or bound can never differ
	// between two callers holding the same Limits.
	Limits Limits
	// Now is the clock used for deadlines and cursor expiry; nil means time.Now.
	Now func() time.Time
}

// Limits is the resolved budget list for one request.
//
// MaxDepth, MaxVisited, MaxEdges and MaxReasonPaths carry the config.Limit
// convention verbatim: 0 is UNLIMITED, and only a negative value is a wiring
// defect. FrontierBytes does NOT: it is a memory ceiling and must be positive. They are stored as plain integers because that is what
// the composition root hands over (config.Limit.Int()), and every comparison
// below goes through config.Limit's own accessors -- Depth/Visited/Edges --
// rather than re-inventing a zero test.
//
// MaxVisited and MaxEdges are PER-PAGE work budgets, not cumulative walk
// ceilings: a page that spends one stops with a continuation cursor, and the
// walk resumes on the next page. The cumulative counts are still carried by the
// cursor and reported, so a caller sees the total the walk has spent.
//
// MaxPageItems, QueryTimeout, CursorTTL and FrontierBytes stay strictly
// positive: a page with no item ceiling is a wire-security bound (class B), not
// a scale bound, and a frontier with no byte ceiling has no heap bound at all.
type Limits struct {
	MaxDepth, MaxVisited, MaxEdges, MaxPageItems, MaxReasonPaths int
	QueryTimeout, CursorTTL                                      time.Duration
	// FrontierBytes caps the edges one frontier level may hold at once,
	// estimated by edgeRowBytes. A level that reaches it SPILLS: the walk stops
	// that level, the frontier goes to the continuation spool and the next page
	// carries on from the keyset position, so peak heap is bounded by this
	// number and never by the graph.
	FrontierBytes int64
}

// Depth, Visited, Edges and ReasonPaths are the unlimited-capable count bounds
// read through config.Limit, which owns the 0-means-unlimited semantics. Every
// consumer must go through them: reading the int field directly turns an
// unlimited bound into a zero-sized budget that stops the walk immediately.
func (l Limits) Depth() config.Limit       { return config.Limit(l.MaxDepth) }
func (l Limits) Visited() config.Limit     { return config.Limit(l.MaxVisited) }
func (l Limits) Edges() config.Limit       { return config.Limit(l.MaxEdges) }
func (l Limits) ReasonPaths() config.Limit { return config.Limit(l.MaxReasonPaths) }

// Engine answers graph queries against one pinned generation. One Engine is
// built per request, which is why the concurrency Gate is passed in rather than
// owned here.
type Engine struct {
	adjacency Adjacency
	promoter  Promoter
	signer    *pagination.Signer
	spools    *pagination.Spools
	leases    *pagination.Leases
	gate      Gate
	limits    Limits
	now       func() time.Time
	// probe is TEST-only memory instrumentation (heapProbe in impactrank.go):
	// nil in production, and every ranking pass's observation is a nil check.
	probe *heapProbe
	// rankStopAfter is TEST-only: when positive, each ranking pass reports the
	// query deadline once this request has added that many records to it, which
	// is the only way to reach ruling P7's mid-rank branch deterministically --
	// a fixture's ranking is far too fast to be caught by a clock that advances
	// on adjacency round trips.
	rankStopAfter int
	// pairStopAfter is the same TEST-only hook for the package-pair ranking,
	// which is the phase that runs AFTER the impact ranking has produced its
	// answer. It is separate from rankStopAfter so a fixture can cut one phase
	// without cutting the other, which is what reaches the one window in which
	// a request completes the impact rank and still mints a rank continuation.
	pairStopAfter int
}

// New builds an Engine. Adjacency is required; Promoter, Signer, Spools, Leases
// and Gate are optional as documented on Options, and a nil Now defaults to
// time.Now.
//
// The scale bounds accept 0 = unlimited (the config.Limit convention); only a
// negative value is a wiring defect. The four that are not scale bounds --
// the page item ceiling, the two durations and the frontier memory ceiling --
// stay strictly positive,
// because an absent page ceiling or an absent deadline is a missing mechanism
// rather than a generous one.
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
		{"max_reason_paths", int64(o.Limits.MaxReasonPaths)},
	} {
		if b.value < 0 {
			return nil, (&model.Error{Code: model.CodeArgumentInvalid,
				Message: "graph engine limit must not be negative"}).WithDetail("limit", b.field)
		}
	}
	for _, b := range []struct {
		field string
		value int64
	}{
		{"max_page_items", int64(o.Limits.MaxPageItems)},
		// frontier_bytes is a MEMORY ceiling, not a scale cap: zero would not
		// mean "read as much as you like", it would remove the only bound on
		// how much of one frontier level is held in heap at once, which is the
		// OOM the scale posture forbids rather than the generosity it asks
		// for. resources.query_memory_bytes -- the key it is resolved from --
		// already refuses 0 at config load as a reservation, so this is the
		// engine-side half of the same rule.
		{"frontier_bytes", o.Limits.FrontierBytes},
		{"query_timeout", int64(o.Limits.QueryTimeout)},
		{"cursor_ttl", int64(o.Limits.CursorTTL)},
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
		leases:    o.Leases,
		gate:      o.Gate,
		limits:    o.Limits,
		now:       now,
	}, nil
}

// Neighbors lives in traverse.go.

// References is implemented in references.go (lane L7).

// frontierState is one admitted node's position in a walk: how far it sits from
// the nearest seed, what it cost to reach, and the edge that reached it.
type frontierState struct {
	Depth int
	Cost  int64
	Node  model.NodeID
	Via   model.RelationID
	// Route is the chain of relation ids from a seed to this node, Via last.
	//
	// It travels with the frontier because ruling P2's walk runs to completion
	// and streams its records into a sort: there is no page-local byNode map
	// left to walk a parent chain through, and model.ImpactEntry.Paths is not
	// optional (Section 14.3 rejects an entry whose reason nothing backs). It
	// is capped at model.MaxRelationsPerPath+1 by appendRoute -- one past the
	// bound, so a consumer can tell "too long to report" from "exactly at the
	// bound" -- which is what keeps the frontier's per-node cost bounded.
	Route []model.RelationID
}

// appendRoute extends a parent's route with the edge that left it, copying
// rather than sharing the backing array: two neighbours of one frontier node
// would otherwise append over each other's last element.
//
// It stops one past model.MaxRelationsPerPath. A route at that length is
// already longer than a servable path, so the elements beyond it would be
// carried through the whole walk to be discarded at hydration.
func appendRoute(parent []model.RelationID, via model.RelationID) []model.RelationID {
	if len(parent) > model.MaxRelationsPerPath {
		return parent
	}
	out := make([]model.RelationID, len(parent), len(parent)+1)
	copy(out, parent)
	return append(out, via)
}

// queryDeadline is the instant one graph query runs under.
//
// resources.query_timeout is a DEFAULT, never a ceiling. When the INCOMING
// context already carries a deadline -- the CLI facade sets one from --timeout
// or from config, and the MCP server likewise -- that deadline is the
// request's, whether it is shorter or longer than the configured one. Every
// entry point used to compute now+QueryTimeout unconditionally, so `--timeout
// 120s` expired after the configured 10s and reported the answer as out of
// time at a twelfth of the time the operator had asked for. A --timeout of 0
// mints no deadline at all (internal/cli queryContext), so it falls through to
// the configured default exactly as before.
//
// A context with no deadline gets now+QueryTimeout on the ENGINE clock, the
// same clock the walk budget compares against. A caller-set deadline is a real
// instant, so a fixture that drives the engine clock must derive the deadline
// it passes in from that clock or the two are not comparable.
func (e *Engine) queryDeadline(ctx context.Context) time.Time {
	if deadline, ok := ctx.Deadline(); ok {
		return deadline
	}
	return e.now().Add(e.limits.QueryTimeout)
}

// budget is the cumulative, cursor-carried work allowance of one traversal. It
// does NOT reset per page: a resumed page that is already at a cap answers
// truncated with no continuation rather than spending the budget again.
type budget struct {
	visited, edges int64
	// pageVisited and pageEdges are THIS page's own spend. The visited and edge
	// bounds are per-page work budgets, so they are compared against these;
	// visited and edges above stay cumulative because the cursor carries them
	// and the answer reports them.
	pageVisited, pageEdges int64
	deadline               time.Time
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
	// deadlineHit records that the walk stopped because the request's
	// query_timeout ran out with edges already admitted. Ruling Q4 makes the
	// deadline end a PAGE, not an answer, so -- like frontierHit -- it is a
	// clean finish here and the caller turns it into the truncation reason and
	// the continuation cursor. It is set only by a walk that opted in
	// (expandOptions.DeadlineStops). For a PAGED traversal it is set only once
	// the page holds a row -- a deadline before any edge has nothing partial to
	// return there, and the caller still holds the cursor it arrived with, so
	// it stays the CTX_QUERY_DEADLINE error it always was. For a walk whose
	// progress is persisted independently of what this page served
	// (expandOptions.DeadlineResumesEmptyPage) an empty page is not a lost one,
	// and every deadline sets this.
	deadlineHit bool
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
	// MaxDepth is the effective depth bound, unlimited when zero. A walk that
	// runs out of depth with a frontier still standing reports reasonDepth;
	// it is the one bound that is a property of the WALK rather than of the
	// page, so it is not a per-page budget.
	MaxDepth  config.Limit
	Budget    *budget
	BatchSize int
	// FrontierBytes is Limits.FrontierBytes: the ceiling on the edges one
	// frontier level may hold in memory at once. A level that reaches it stops
	// reading; the frontier it had built is spilled to the continuation spool
	// and the next page resumes from the keyset position, rather than
	// accumulating a whole hub's fan-out with no bound in front of it.
	// It is strictly positive (New refuses 0): it is a memory ceiling, so a
	// zero would remove the level's only heap bound rather than lift a scale
	// cap.
	FrontierBytes int64
	// DeadlineStops makes the query deadline END THIS PAGE instead of failing
	// the walk: expand returns the partial walkState and the caller mints a
	// continuation from its frontier. The paged traversal sets it on its own;
	// the operations that aggregate a whole walk into one answer (impact, the
	// package rollup) set it together with DeadlineResumesEmptyPage, because
	// they serve nothing until the walk is exhausted.
	DeadlineStops bool
	// DeadlineResumesEmptyPage says the walk's progress survives this request
	// whatever the page served: every record the walk admitted is in the
	// retained input and the standing frontier becomes the continuation, so a
	// page that admitted no edge at all still carries the walk forward. Ruling
	// P3 then applies to EVERY deadline -- it ends the page, never the answer.
	// Without it, a deadline that lands before the page's first edge fails the
	// walk, and for an aggregating operation that failure is the whole retained
	// walk thrown away with no continuation to reach its remainder.
	DeadlineResumesEmptyPage bool
	// Resume, when non-nil, is the state a continuation restored: the walk
	// starts from the spooled frontier at the cursor's depth instead of from
	// seeds, and skips the rows the issuing page already emitted.
	Resume *resumeState
	// Visited is the walk's PERSISTENT cumulative admitted-node set
	// (visitedstore.go), set by the operations that chain several expand calls
	// inside one request. runWalkToCompletion appends each internal link's own
	// admissions to it directly, so the next link -- and the next REQUEST --
	// reads them as part of the one cumulative set. Without it the earlier
	// links' admissions reached no continuation at all and the next page
	// re-admitted every one of them, reporting the same entity twice.
	//
	// It is nil on the paged traversal, which runs one expand per request and
	// carries its cumulative set forward in the continuation spool.
	Visited *visitedStore
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

// isDeadline is the ONE test for "the query ran out of time", in every shape a
// deadline reaches the engine in. The engine's own checks (checkWalk) raise a
// typed CTX_QUERY_DEADLINE, but two other shapes exist and both were treated as
// fatal failures before: the RAW context.DeadlineExceeded a storage read
// returns when the deadline falls inside the query -- sqlite's wrap passes it
// through deliberately, so a caller can tell it from a crash -- and the same
// raw error from any other I/O the request performs past the deadline,
// including the lease write that MINTS the continuation.
//
// Treating those as fatal is what made a timed-out impact answer on a real
// repository return ok:false, data:null and a bare CTX_QUERY_DEADLINE with no
// cursor: the page, the frontier and the continuation were all thrown away.
// Every site that decides whether a deadline ends a page or an answer routes
// through here, so the storage layer may keep returning raw context errors and
// the engine still classifies them once.
func isDeadline(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	var typed *model.Error
	return errors.As(err, &typed) && typed.Code == model.CodeQueryDeadline
}
