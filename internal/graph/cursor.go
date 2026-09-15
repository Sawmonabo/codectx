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
//
// Version 2 retires the ranked spool vocabulary: a version-1 token issued by an
// impact page that spilled a ranked tail names a spool this build cannot
// replay, and resuming it would serve an empty continuation instead of the rest
// of the answer. It is refused as CTX_CURSOR_INVALID, which a caller re-runs
// the query for, rather than answered short.
//
// Version 3 orders the spool's visited section by NodeID. Membership is a
// MERGE-JOIN over that order now (visited.go), and a merge-join over a
// version-2 spool -- whose visited records are in admission order -- would
// walk past a node the spool holds and report it absent. That is a
// cross-page RE-ADMISSION: the node would be emitted on two pages. The
// version is what refuses such a token instead of answering it wrong.
const traversalCursorVersion = 3

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

// The kinds of record a page spills, and the whole vocabulary: the frontier a
// page stopped at, and the nodes it already admitted (which a resumed page must
// not admit again). Every paged endpoint -- neighbours, impact and the package
// rollup alike -- resumes a WALK, so there is one vocabulary and one replay.
//
// A second, ranked vocabulary ("a", "e", "p", "c") existed while impact ranked
// its whole walk on the first page and replayed the cut tail from the spool.
// That shape is gone: impact answers one page of the walk at a time, so there
// is no pre-computed tail to replay. The payload version below is bumped with
// its removal, which is what stops a token minted by a build that wrote a
// ranked spool from resuming over a reader that no longer understands one.
const (
	spoolRecordFrontier = "f"
	spoolRecordVisited  = "v"
)

// spoolRecord is one spilled record. The field names are short because the
// spool byte budget is shared across every live continuation in the process.
type spoolRecord struct {
	Kind  string           `json:"k"`
	Node  model.NodeID     `json:"n,omitempty"`
	Depth int              `json:"d,omitempty"`
	Cost  int64            `json:"c,omitempty"`
	Via   model.RelationID `json:"v,omitempty"`
}

// resumeState is what a continuation restores: the decoded cursor, a budget
// seeded with what the earlier pages already spent, the frontier they stopped
// at and the nodes they already admitted.
type resumeState struct {
	Cursor   traversalCursor
	Budget   *budget
	Frontier []frontierState
	// Visited streams the nodes the earlier pages admitted, straight off their
	// spool. It is a STREAM and not a map because the cumulative set is sized
	// by the walk: materializing it here was the last repository-sized heap
	// structure on the traversal path (visited.go).
	Visited visitedStream
	// Filter summarizes the nodes Visited will replay. It is built during the
	// replay below -- which already decodes every record to rebuild the
	// frontier -- so it costs no extra I/O, and it lets a level whose
	// candidates are all freshly reached skip the stream entirely.
	Filter *visitedFilter
	// Release ends the replayed continuation's spool and lease. The caller
	// defers it: the spool must outlive the walk, because both the membership
	// probes and the next page's spill read from it.
	Release func()
}

// continuation is what the page just answered hands to nextTraversalCursor.
type continuation struct {
	Endpoint string
	// QueryHash binds the next cursor to this same normalized query.
	QueryHash string
	Depth     int
	LastOwner model.NodeID
	LastKey   model.RelationID
	// Frontier is where the walk stopped, and Visited the nodes this page must
	// contribute to the fresh spool's visited section, ascending: what it
	// admitted itself plus the frontier it resumed. Both are bounded by the
	// page and the frontier ceiling, never by the walk.
	Frontier []frontierState
	Visited  []model.NodeID
	// Carried streams the visited SECTION of the spool the earlier pages
	// wrote, ascending. spill merges it with Visited without materializing it,
	// so a walk of any size costs one spool block of heap here.
	Carried visitedStream
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
	s := &resumeState{Cursor: c, Budget: b, Release: func() {
		e.releaseConsumed(context.WithoutCancel(ctx), c.SpoolID, c.LeaseID)
	}}
	if c.SpoolID == "" {
		// A pure keyset continuation carries a lease and no spool; the caller
		// still releases it, for the same reason a spooled one is released.
		return s, nil
	}
	if e.spools == nil {
		return nil, cursorInvalid("continuation state has expired or was released")
	}
	// One fresh spool per page: this replays the PREVIOUS page's spool.
	//
	// The replay is bounded by the SPOOL's own byte budget
	// (resources.max_temp_bytes, which pagination.Spools reserves on every
	// Append -- Open itself re-checks the binding and the lease, not the
	// budget, so the bound is the one the WRITE already paid), never by
	// max_visited: that is a per-page work budget now, while the spooled
	// visited set is cumulative across the whole walk, so testing the one
	// against the other refused the third page of any walk whose budget it was
	// working correctly. A tampered or oversized spool is caught by the byte
	// budget, which is also what keeps peak heap a function of page size.
	// The membership summary is sized from the cumulative admitted count the
	// cursor already carries and clamped to a fraction of the walk's own memory
	// ceiling, so a walk of any size costs a bounded number of filter bytes; a
	// walk past the clamp gets a denser filter (more false positives, more
	// sweeps), never a refusal or a truncation.
	s.Filter = newVisitedFilter(c.Visited, e.limits.FrontierBytes/visitedFilterBudgetShare)
	err = e.spools.Open(ctx, c.spoolCursor(), now, func(record []byte) error {
		var r spoolRecord
		if err := json.Unmarshal(record, &r); err != nil {
			return cursorInvalid("continuation state is not readable")
		}
		switch r.Kind {
		case spoolRecordFrontier:
			s.Frontier = append(s.Frontier, frontierState{Depth: r.Depth, Cost: r.Cost, Node: r.Node, Via: r.Via})
		case spoolRecordVisited:
			// Deliberately not accumulated: the cumulative set stays on disk
			// and is streamed by s.Visited below.
		default:
			return cursorInvalid("continuation state is not readable")
		}
		// The summary describes exactly what s.Visited replays -- the visited
		// section -- so a miss is a proof the stream holds nothing. A frontier
		// record's node is answered from the front instead.
		if r.Kind == spoolRecordVisited {
			s.Filter.add(r.Node)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	// The continuation is consumed -- but only once the page that is resuming
	// it has finished, because the cumulative visited set it holds is read
	// twice more: once per level for the membership probes, and once by the
	// spill that copies it forward into the fresh spool. The caller therefore
	// defers Release. Holding the pair past that would pin a generation
	// against retention -- one lease and one spool per page of every walk --
	// for state nothing will read again; presenting the same token twice is
	// CTX_CURSOR_INVALID rather than a replayed page, which is the deliberate
	// trade: the caller's remedy is the continuation this page hands it.
	s.Visited = func(ctx context.Context, fn func(model.NodeID) error) error {
		return e.spools.Open(ctx, c.spoolCursor(), e.now(), func(record []byte) error {
			var r spoolRecord
			if err := json.Unmarshal(record, &r); err != nil {
				return cursorInvalid("continuation state is not readable")
			}
			switch r.Kind {
			case spoolRecordVisited:
				// The visited SECTION only, and it is written in ascending
				// NodeID order (spill below), which is what lets warm()
				// merge-join against it. The frontier records ahead of it are
				// deliberately skipped: their nodes are carried in heap by the
				// page that resumes them (visited.go carry), so replaying them
				// here would both break the order and answer what the front
				// already answers.
				return fn(r.Node)
			case spoolRecordFrontier:
				return nil
			}
			return cursorInvalid("continuation state is not readable")
		})
	}
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
	// The visited and edge bounds are PER-PAGE work budgets now, so they no
	// longer decide whether a walk may continue -- spending one is exactly the
	// condition that mints this cursor. A walk ends when its frontier is empty,
	// when the depth bound is reached (reported, not resumed) or when the
	// request deadline passes; nothing here silently withholds a continuation
	// from a walk that still has work.
	resumesWalk := len(c.Frontier) > 0 || c.LastKey != ""
	if !resumesWalk {
		// Nothing left to resume from: the answer is complete.
		return "", nil
	}
	// A frontier or an already-admitted visited set has to survive the
	// response, and both live in the spool; with spilling disabled neither can,
	// so the answer stops here rather than resuming without them.
	needsSpool := len(c.Frontier) > 0 || len(c.Visited) > 0
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
		id, err := e.spill(ctx, next, c)
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
func (e *Engine) spill(ctx context.Context, next traversalCursor, c continuation) (string, error) {
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
	frontierNodes := make(map[model.NodeID]struct{}, len(c.Frontier))
	for _, fs := range c.Frontier {
		if err := appendRecord(spoolRecord{Kind: spoolRecordFrontier, Node: fs.Node,
			Depth: fs.Depth, Cost: fs.Cost, Via: fs.Via}); err != nil {
			return "", e.releaseSpool(sp, err)
		}
		frontierNodes[fs.Node] = struct{}{}
	}
	// The visited section follows the frontier records and is written in
	// ASCENDING NodeID order, which is what makes membership a merge-join with
	// an early exit instead of a scan (visited.go). It is produced by a two-way
	// merge of the carried stream -- already ascending, because the page that
	// wrote it ran this same merge -- with this page's own ascending
	// contribution: one record in flight, never the whole set, so a walk of any
	// size costs one spool block of heap here.
	//
	// A node that is on THIS page's frontier is skipped: its frontier record
	// already marks it visited, and writing it twice would spend the shared
	// spool budget for nothing. Equal keys are emitted once for the same
	// reason -- the two sources are disjoint by construction, so this is a
	// guard, not a correction.
	var (
		pending  = c.Visited
		lastNode model.NodeID
		haveLast bool
	)
	writeVisited := func(n model.NodeID) error {
		if _, ok := frontierNodes[n]; ok {
			return nil
		}
		if haveLast && n == lastNode {
			return nil
		}
		lastNode, haveLast = n, true
		return appendRecord(spoolRecord{Kind: spoolRecordVisited, Node: n})
	}
	// drainBelow emits every pending node that sorts before n, which is what
	// keeps the merged output ascending.
	drainBelow := func(n model.NodeID) error {
		for len(pending) > 0 && pending[0] < n {
			if err := writeVisited(pending[0]); err != nil {
				return err
			}
			pending = pending[1:]
		}
		return nil
	}
	if c.Carried != nil {
		err := c.Carried(ctx, func(n model.NodeID) error {
			if err := drainBelow(n); err != nil {
				return err
			}
			return writeVisited(n)
		})
		if err != nil {
			return "", e.releaseSpool(sp, err)
		}
	}
	for _, n := range pending {
		if err := writeVisited(n); err != nil {
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
