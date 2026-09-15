package graph

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"strconv"
	"time"

	"github.com/Sawmonabo/codectx/internal/model"
	"github.com/Sawmonabo/codectx/internal/pagination"
)

// This file is the traversal continuation. Three rules shape it, and every
// exported behaviour below exists to keep one of them:
//
//  1. The token is small and carries no traversal state. Section 14.3 forbids
//     serializing a frontier into a cursor, so the token holds only the
//     binding, the keyset position and the CUMULATIVE budget; the frontier and
//     the visited set live in a spool.
//  2. Hard budgets do not reset per page. The token carries what earlier pages
//     already spent, and a resume ASSIGNS those counters into the new page's
//     budget rather than starting from zero or adding to a running total, so
//     replaying one page's cursor twice can neither reset nor double the
//     allowance.
//  3. One fresh spool per page (the L0 freeze). pagination.Spools.Open streams
//     from the start and cannot seek, so a resume opens the PREVIOUS page's
//     spool, replays it to rebuild the frontier, and writes a NEW spool for the
//     page after it. Replay-and-skip over one growing spool would cost
//     O(pages x records).
//
// Every rejection -- a tampered tag, an expired token, a cursor issued for
// another endpoint, generation, analysis key or query -- is CTX_CURSOR_INVALID,
// and no message ever echoes the token.

// traversalCursorVersion is the payload version. A payload without it -- for
// instance a pagination.Cursor token, whose JSON has no such field -- decodes
// to zero and is rejected, so the two token shapes can never be interchanged
// even though both are signed with pagination.PurposeCursor.
const traversalCursorVersion = 1

// queryHashDomain is the Section 9.1 hash domain for the normalized
// query/filter/ordering hash a traversal cursor is bound to.
const queryHashDomain = "graph.cursor.query"

// traversalCursor is the signed continuation payload.
//
// It is NOT a pagination.Cursor: that type rejects a LastKey and a SpoolID
// together and has no field for a cumulative budget, and both are required
// here -- a spooled page still needs its keyset position and its spent
// counters. The payload is signed with the shared pagination.Signer under
// PurposeCursor, so it inherits the installation key, the bounded token size
// and the expiry check, and spoolCursor projects it onto a pagination.Cursor
// for the spool calls.
type traversalCursor struct {
	Version      int                `json:"version"`
	Endpoint     string             `json:"endpoint"`
	GenerationID model.GenerationID `json:"generation_id"`
	AnalysisKey  model.AnalysisKey  `json:"analysis_key"`
	QueryHash    string             `json:"query_hash"`
	LeaseID      string             `json:"lease_id"`
	// SpoolID names the spool written by the page that issued this cursor; it
	// is empty when the page left no frontier behind (a pure keyset page).
	SpoolID string `json:"spool_id,omitempty"`
	// LastOwner and LastKey are the keyset position: the frontier node that
	// owned the last row the issuing page admitted, and that row's RelationID.
	// A level is emitted in the frozen (owner.Node asc, rel.ID asc) order
	// across independently keyset-paged node chunks, so a relation id ALONE
	// cannot name a mid-level stop -- the chunk after the stopping one restarts
	// its own relation-id keyset from the beginning. The pair does name it: a
	// resumed page re-reads the level and skips every row <= (LastOwner,
	// LastKey). Unlike pagination.Cursor either may accompany a SpoolID.
	LastOwner model.NodeID     `json:"last_owner,omitempty"`
	LastKey   model.RelationID `json:"last_key,omitempty"`
	// Depth is the hop count the issuing page stopped at, so a resumed walk
	// measures MaxDepth from the original seeds rather than from its frontier.
	Depth int `json:"depth"`
	// Visited and Edges are CUMULATIVE across every page of this traversal.
	Visited   int64     `json:"visited"`
	Edges     int64     `json:"edges"`
	ExpiresAt time.Time `json:"expires_at"`
}

// validate enforces the payload's shape. It is applied to what comes back from
// the signer as well as to what goes in: a tag verifies only that we issued the
// bytes, not that a later change to this package still agrees with them.
func (c traversalCursor) validate() error {
	if c.Version != traversalCursorVersion {
		return cursorInvalid("cursor version is not supported")
	}
	if c.Endpoint == "" || len(c.Endpoint) > model.MaxIdentifierBytes {
		return cursorInvalid("cursor endpoint is missing or too long")
	}
	if c.GenerationID <= 0 {
		return cursorInvalid("cursor does not pin a generation")
	}
	if !model.ValidHexID(c.QueryHash) || !model.ValidHexID(c.LeaseID) {
		return cursorInvalid("cursor query hash and lease id must be well-formed identifiers")
	}
	// The analysis key is only bounded here, not required to be canonical hex:
	// resumeTraversal compares it for equality with the pinned binding's key,
	// which is the invariant that matters, and pagination.Cursor.Validate
	// enforces the canonical spelling on the spool binding itself.
	if c.AnalysisKey == "" || len(c.AnalysisKey) > model.MaxIdentifierBytes {
		return cursorInvalid("cursor does not name an analysis key")
	}
	if c.SpoolID != "" && !model.ValidHexID(c.SpoolID) {
		return cursorInvalid("cursor spool id is malformed")
	}
	if len(c.LastKey) > model.MaxIdentifierBytes || len(c.LastOwner) > model.MaxIdentifierBytes {
		return cursorInvalid("cursor sort key exceeds its bound")
	}
	if (c.LastOwner == "") != (c.LastKey == "") {
		return cursorInvalid("cursor sort key is half-formed")
	}
	if c.Depth < 0 || c.Visited < 0 || c.Edges < 0 {
		return cursorInvalid("cursor carries a negative depth or budget")
	}
	if c.ExpiresAt.IsZero() {
		return cursorInvalid("cursor has no expiry")
	}
	return nil
}

// spoolCursor projects the payload onto the pagination.Cursor the spool API
// takes. LastKey is deliberately dropped: pagination.Cursor.Validate rejects a
// sort key and a spool together, and the spool binding is the other five
// fields, which Spools.Open compares against the spool header.
func (c traversalCursor) spoolCursor() pagination.Cursor {
	return pagination.Cursor{
		Endpoint:     c.Endpoint,
		GenerationID: c.GenerationID,
		AnalysisKey:  c.AnalysisKey,
		QueryHash:    c.QueryHash,
		SpoolID:      c.SpoolID,
		LeaseID:      c.LeaseID,
		ExpiresAt:    c.ExpiresAt,
	}
}

// traversalQueryHash is the normalized query/filter/ordering hash of Section
// 14.4. Seeds and relation kinds are sorted copies, so two spellings of one
// query share a cursor and two different queries never do -- presenting a
// neighbours cursor to a differently filtered walk is CTX_CURSOR_INVALID
// rather than a silently repinned answer.
func traversalQueryHash(direction model.Direction, kinds []model.RelationKind,
	seeds []model.NodeID, maxDepth, pageLimit int) string {
	sortedKinds := make([]string, len(kinds))
	for i, k := range kinds {
		sortedKinds[i] = string(k)
	}
	sort.Strings(sortedKinds)
	sortedSeeds := make([]string, len(seeds))
	for i, s := range seeds {
		sortedSeeds[i] = string(s)
	}
	sort.Strings(sortedSeeds)

	h := model.NewHasher(queryHashDomain)
	h.AddString(string(direction))
	h.AddString(strconv.Itoa(maxDepth))
	h.AddString(strconv.Itoa(pageLimit))
	for _, k := range sortedKinds {
		h.AddString(k)
	}
	// The two lists are separated by a framed empty component so no
	// arrangement of kinds and seeds can alias another.
	h.AddString("")
	for _, s := range sortedSeeds {
		h.AddString(s)
	}
	return h.Sum()
}

// The kinds of record a page spills. The first two are a traversal's: the
// frontier it stopped at, and the nodes it already admitted (which a resumed
// page must not admit again). The last three are a RANKED page's: impact ranks
// its whole walk and cuts the ranked list, so its continuation replays the tail
// it already computed instead of resuming a frontier.
//
// The two vocabularies are disjoint and each replay accepts only its own, so a
// traversal cursor can never be resumed over a ranked spool, or the reverse.
const (
	spoolRecordFrontier = "f"
	spoolRecordVisited  = "v"
	// spoolRecordAnswer is the LEADING record of a ranked spool: the
	// answer-level facts every page must report identically.
	spoolRecordAnswer = "a"
	// spoolRecordEntry is one ranked impact entry, in rank order.
	spoolRecordEntry = "e"
	// spoolRecordPackage is one rollup pair. The pairs are individual records
	// rather than a field of the answer record because a rollup bounded only
	// by MaxRecordsPerResult can exceed the spool's per-record byte bound.
	spoolRecordPackage = "p"
)

// spoolRecord is one spilled record. The field names are short because the
// spool byte budget is shared across every live continuation in the process.
// A traversal record uses the node fields; a ranked record carries its own
// already-encoded value in Payload, so adding a ranked kind does not widen the
// struct every traversal record pays for.
type spoolRecord struct {
	Kind    string           `json:"k"`
	Node    model.NodeID     `json:"n,omitempty"`
	Depth   int              `json:"d,omitempty"`
	Cost    int64            `json:"c,omitempty"`
	Via     model.RelationID `json:"v,omitempty"`
	Payload json.RawMessage  `json:"p,omitempty"`
}

// encodeSpoolRecord builds one ranked record around its payload.
func encodeSpoolRecord(kind string, payload any) (spoolRecord, error) {
	encoded, err := json.Marshal(payload)
	if err != nil {
		return spoolRecord{}, &model.Error{Code: model.CodeInternal,
			Message: "continuation state encoding: " + err.Error()}
	}
	return spoolRecord{Kind: kind, Payload: encoded}, nil
}

// resumeState is what a continuation restores: the decoded cursor, a budget
// seeded with what the earlier pages already spent, the frontier they stopped
// at and the nodes they already admitted.
type resumeState struct {
	Cursor   traversalCursor
	Budget   *budget
	Frontier []frontierState
	Visited  map[model.NodeID]struct{}
}

// continuation is what the page just answered hands to nextTraversalCursor.
type continuation struct {
	Endpoint string
	// QueryHash binds the next cursor to this same normalized query.
	QueryHash string
	Depth     int
	LastOwner model.NodeID
	LastKey   model.RelationID
	// Frontier is where the walk stopped, and Visited the nodes it admitted.
	Frontier []frontierState
	Visited  []model.NodeID
	// Records is the already-built state of a RANKED page -- the answer-level
	// record, the ranked tail and the rollup pairs impact spills. A traversal
	// leaves a frontier instead and sets none of these; a ranked page leaves
	// records instead and has no frontier. Exactly one of the two is populated,
	// which is what keeps the two replay vocabularies disjoint.
	Records []spoolRecord
}

// verifyContinuation is the half of a resume every paging endpoint shares: it
// verifies the tag, re-checks the payload's own shape, refuses a token issued
// for another endpoint, query or generation, and rebuilds the budget.
//
// The budget carries the cursor's cumulative counters by ASSIGNMENT: presenting
// one page's cursor twice restores the same allowance both times -- it neither
// resets to zero nor accumulates. The deadline is THIS request's, measured by
// the caller before its gate wait (recomputing it here would let a resumed page
// outlive the request deadline by however long that wait took), and the engine
// clock is carried with it because checkWalk compares the two.
//
// Only the spool replay differs between endpoints, and each endpoint owns its
// own: the traversal vocabulary and the ranked vocabulary are disjoint, so
// neither replay can be fed the other's records.
func (e *Engine) verifyContinuation(token, endpoint, queryHash string,
	deadline time.Time) (traversalCursor, *budget, error) {
	if e.signer == nil {
		return traversalCursor{}, nil, cursorInvalid("continuations are not available in this workspace")
	}
	payload, err := e.signer.Verify(token, pagination.PurposeCursor, e.now())
	if err != nil {
		return traversalCursor{}, nil, err
	}
	var c traversalCursor
	if err := json.Unmarshal(payload, &c); err != nil {
		return traversalCursor{}, nil, cursorInvalid("cursor payload is malformed")
	}
	if err := c.validate(); err != nil {
		return traversalCursor{}, nil, err
	}
	if c.Endpoint != endpoint {
		return traversalCursor{}, nil, cursorInvalid("cursor was issued by a different endpoint")
	}
	if c.QueryHash != queryHash {
		return traversalCursor{}, nil, cursorInvalid(
			"cursor was issued for a different query: the direction, relation kinds, seeds, depth and page limit that minted it must be repeated on every page")
	}
	binding := e.adjacency.Binding()
	if c.GenerationID != binding.GenerationID || c.AnalysisKey != binding.AnalysisKey {
		return traversalCursor{}, nil, cursorInvalid("cursor pins a generation that is no longer the one being read")
	}
	return c, &budget{visited: c.Visited, edges: c.Edges, deadline: deadline, now: e.now}, nil
}

// resumeTraversal verifies token for endpoint, binds it to the engine's pinned
// generation and to queryHash, and rebuilds the state the issuing page left.
//
// The returned budget carries the cursor's cumulative counters by ASSIGNMENT:
// presenting one page's cursor twice restores the same allowance both times --
// it neither resets to zero nor accumulates. The deadline is fresh, because
// Section 3 scopes the query timeout to one request, not to a cursor chain.
func (e *Engine) resumeTraversal(ctx context.Context, token, endpoint, queryHash string,
	deadline time.Time) (*resumeState, error) {
	c, b, err := e.verifyContinuation(token, endpoint, queryHash, deadline)
	if err != nil {
		return nil, err
	}
	now := e.now()
	s := &resumeState{Cursor: c, Budget: b, Visited: map[model.NodeID]struct{}{}}
	if c.SpoolID == "" {
		// A pure keyset continuation carries a lease and no spool; it is
		// consumed here for the same reason a spooled one is below.
		e.releaseConsumed(ctx, c.SpoolID, c.LeaseID)
		return s, nil
	}
	if e.spools == nil {
		return nil, cursorInvalid("continuation state has expired or was released")
	}
	// One fresh spool per page: this replays the PREVIOUS page's spool. The
	// replay is bounded by MaxVisited, which is also what bounded the page
	// that wrote it, so a tampered or corrupt spool cannot make a resume
	// allocate without limit.
	var records int64
	err = e.spools.Open(ctx, c.spoolCursor(), now, func(record []byte) error {
		records++
		if records > int64(e.limits.MaxVisited) {
			return (&model.Error{Code: model.CodeResourceLimit,
				Message:     "continuation state exceeds the visited bound",
				Remediation: "restart the query with a narrower scope"}).WithDetail("limit", "max_visited")
		}
		var r spoolRecord
		if err := json.Unmarshal(record, &r); err != nil {
			return cursorInvalid("continuation state is not readable")
		}
		switch r.Kind {
		case spoolRecordFrontier:
			s.Frontier = append(s.Frontier, frontierState{Depth: r.Depth, Cost: r.Cost, Node: r.Node, Via: r.Via})
			s.Visited[r.Node] = struct{}{}
		case spoolRecordVisited:
			s.Visited[r.Node] = struct{}{}
		default:
			return cursorInvalid("continuation state is not readable")
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	// The continuation is consumed: its frontier and visited set are in memory
	// now, and the page being built will spill a FRESH spool under a FRESH
	// lease. Holding the replayed pair until the cursor TTL would pin a
	// generation against retention -- one lease and one spool per page of every
	// walk -- for state nothing will read again. Presenting the same token
	// twice is therefore CTX_CURSOR_INVALID rather than a replayed page, which
	// is the deliberate trade: the caller's remedy is the continuation this
	// page hands it.
	e.releaseConsumed(ctx, c.SpoolID, c.LeaseID)
	return s, nil
}

// releaseConsumed ends a replayed continuation's spool and its cursor-owned
// lease. It takes the two identifiers rather than a cursor so EVERY paged
// endpoint can end its own continuation whatever payload shape carries it --
// a traversal cursor, a ranked one, or a page that spills no spool at all and
// passes an empty spoolID.
//
// The engine has no logger, and neither release can fail a page that is still
// being built, so a failure is left to the lease's own TTL and the spool sweep
// rather than raised: this is reclamation, not an invariant the answer depends
// on.
func (e *Engine) releaseConsumed(ctx context.Context, spoolID, leaseID string) {
	if spoolID != "" && e.spools != nil {
		_ = e.spools.Release(spoolID)
	}
	if leaseID != "" && e.leases != nil {
		_ = e.leases.Release(context.WithoutCancel(ctx), leaseID)
	}
}

// nextTraversalCursor writes a FRESH spool for c's frontier and visited set and
// signs the continuation for the page after it.
//
// It returns an empty token, and no error, whenever the answer must stop rather
// than continue: a walk whose cumulative budget is exhausted, an engine with no
// signer or lease store (continuations are not offered), or state with no spool
// to spill into. In every one of those cases the caller reports Truncated with
// no NextCursor, which is the same contract as running out of page items.
func (e *Engine) nextTraversalCursor(ctx context.Context, b *budget, c continuation) (string, error) {
	if e.signer == nil || e.leases == nil {
		// No signer, or no lease store to retain the generation the resumed
		// page will read: a token minted here would resume over facts nothing
		// is holding.
		return "", nil
	}
	// This is the ONE place the cumulative caps decide whether a WALK may
	// continue. A resumed page that arrives already at cap therefore answers
	// Truncated with no NextCursor without spending the budget a second time.
	// The test is gated on the continuation actually resuming a walk, which
	// every traversal mint does (traverse.go mints only with a live frontier),
	// so traversal behaviour is unchanged. A ranked continuation replays
	// records the first page already computed and spends no traversal budget at
	// all; refusing it here would silently drop entries the walk did find.
	resumesWalk := len(c.Frontier) > 0 || c.LastKey != ""
	if resumesWalk && (b.visited >= int64(e.limits.MaxVisited) || b.edges >= int64(e.limits.MaxEdges)) {
		return "", nil
	}
	if !resumesWalk && len(c.Records) == 0 {
		// Nothing left to resume from: the answer is complete.
		return "", nil
	}
	// A frontier, an already-admitted visited set or a ranked tail has to
	// survive the response, and all three live in the spool; with spilling
	// disabled none can, so the answer stops here rather than resuming without
	// them.
	needsSpool := len(c.Frontier) > 0 || len(c.Visited) > 0 || len(c.Records) > 0
	if needsSpool && e.spools == nil {
		return "", nil
	}
	// A NEW cursor-owned retention lease, never the pinned reader's query
	// lease: that one is released when this request returns, so the very next
	// invocation's spool read would be refused and the continuation would be
	// unusable. The lease expires with the cursor, so nothing minted here pins
	// a generation for longer than the token lives.
	binding := e.adjacency.Binding()
	lease, err := e.leases.Acquire(ctx, binding.GenerationID, binding.SnapshotID, model.LeaseCursor)
	if err != nil {
		return "", err
	}
	next := traversalCursor{
		Version:      traversalCursorVersion,
		Endpoint:     c.Endpoint,
		GenerationID: binding.GenerationID,
		AnalysisKey:  binding.AnalysisKey,
		QueryHash:    c.QueryHash,
		LeaseID:      lease.ID,
		LastOwner:    c.LastOwner,
		LastKey:      c.LastKey,
		Depth:        c.Depth,
		Visited:      b.visited,
		Edges:        b.edges,
		ExpiresAt:    e.now().Add(e.limits.CursorTTL).UTC().Truncate(time.Second),
	}
	if needsSpool {
		id, err := e.spill(next, c)
		if err != nil {
			return "", e.releaseLease(ctx, lease.ID, err)
		}
		next.SpoolID = id
	}
	if err := next.validate(); err != nil {
		return "", e.releaseLease(ctx, lease.ID, err)
	}
	payload, err := json.Marshal(next)
	if err != nil {
		return "", e.releaseLease(ctx, lease.ID,
			&model.Error{Code: model.CodeInternal, Message: "cursor encoding: " + err.Error()})
	}
	token, err := e.signer.Sign(pagination.PurposeCursor, payload, next.ExpiresAt)
	if err != nil {
		return "", e.releaseLease(ctx, lease.ID, err)
	}
	return token, nil
}

// releaseLease returns a lease whose cursor never reached the caller and
// reports cause. The request's own context may already be done, so the release
// runs on an uncancelled one: leaking the lease would pin a generation against
// retention for the full cursor TTL for a page nobody can ask for.
//
// A release that itself fails is joined onto cause rather than dropped -- the
// engine has no logger, and a retention lease that will now expire only with
// its TTL is something the operator is told about. errors.As still finds the
// typed cause, so the joined error keeps its Section 8 code.
func (e *Engine) releaseLease(ctx context.Context, id string, cause error) error {
	if err := e.leases.Release(context.WithoutCancel(ctx), id); err != nil {
		return errors.Join(cause, &model.Error{Code: model.CodeInternal,
			Message: "a continuation lease could not be released: " + err.Error()})
	}
	return cause
}

// spill writes one fresh spool holding c's records, frontier and visited set
// and returns its id. The spool is bound to next's lease, generation, analysis key and
// query hash, which is what Spools.Open checks before it replays a record.
func (e *Engine) spill(next traversalCursor, c continuation) (string, error) {
	sp, err := e.spools.Create(next.spoolCursor())
	if err != nil {
		return "", err
	}
	appendRecord := func(r spoolRecord) error {
		encoded, err := json.Marshal(r)
		if err != nil {
			return &model.Error{Code: model.CodeInternal, Message: "continuation state encoding: " + err.Error()}
		}
		return sp.Append(encoded)
	}
	for _, r := range c.Records {
		if err := appendRecord(r); err != nil {
			return "", e.releaseSpool(sp, err)
		}
	}
	frontierNodes := make(map[model.NodeID]struct{}, len(c.Frontier))
	for _, fs := range c.Frontier {
		if err := appendRecord(spoolRecord{Kind: spoolRecordFrontier, Node: fs.Node,
			Depth: fs.Depth, Cost: fs.Cost, Via: fs.Via}); err != nil {
			return "", e.releaseSpool(sp, err)
		}
		frontierNodes[fs.Node] = struct{}{}
	}
	for _, n := range c.Visited {
		// A frontier record already marks its node visited; writing it twice
		// would spend the shared spool budget for nothing.
		if _, ok := frontierNodes[n]; ok {
			continue
		}
		if err := appendRecord(spoolRecord{Kind: spoolRecordVisited, Node: n}); err != nil {
			return "", e.releaseSpool(sp, err)
		}
	}
	if err := sp.Close(); err != nil {
		return "", e.releaseSpool(sp, err)
	}
	return sp.ID(), nil
}

// releaseSpool closes and removes a half-written spool so its bytes return to
// the shared budget, and reports the failure that caused it. A release failure
// never masks that original error.
func (e *Engine) releaseSpool(sp *pagination.Spool, cause error) error {
	_ = sp.Close()
	_ = e.spools.Release(sp.ID())
	return cause
}

// cursorInvalid is the one rejection this file produces. Every tampered,
// expired, foreign or stale continuation is CTX_CURSOR_INVALID, and the
// remediation tells the caller the state is not resurrected.
func cursorInvalid(msg string) *model.Error {
	return &model.Error{Code: model.CodeCursorInvalid, Message: msg,
		Remediation: "restart the query; continuation state is not resurrected"}
}
