package graph

import (
	"context"
	"encoding/json"
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
	// LastKey is the keyset position: the last RelationID the issuing page
	// admitted. Unlike pagination.Cursor it may accompany a SpoolID.
	LastKey model.RelationID `json:"last_key,omitempty"`
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
	if len(c.LastKey) > model.MaxIdentifierBytes {
		return cursorInvalid("cursor sort key exceeds its bound")
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

// spoolRecordFrontier and spoolRecordVisited discriminate the two kinds of
// record a page spills: the frontier it stopped at, and the nodes it already
// admitted (which a resumed page must not admit again).
const (
	spoolRecordFrontier = "f"
	spoolRecordVisited  = "v"
)

// spoolRecord is one spilled record. The field names are short because the
// spool byte budget is shared across every live continuation in the process.
type spoolRecord struct {
	Kind  string           `json:"k"`
	Node  model.NodeID     `json:"n"`
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
	Visited  map[model.NodeID]struct{}
}

// continuation is what the page just answered hands to nextTraversalCursor.
type continuation struct {
	Endpoint string
	// QueryHash binds the next cursor to this same normalized query.
	QueryHash string
	// LeaseID is the retention lease holding the pinned generation; the cursor
	// and its spool both live exactly as long as it does.
	LeaseID string
	Depth   int
	LastKey model.RelationID
	// Frontier is where the walk stopped, and Visited the nodes it admitted.
	Frontier []frontierState
	Visited  []model.NodeID
}

// resumeTraversal verifies token for endpoint, binds it to the engine's pinned
// generation and to queryHash, and rebuilds the state the issuing page left.
//
// The returned budget carries the cursor's cumulative counters by ASSIGNMENT:
// presenting one page's cursor twice restores the same allowance both times --
// it neither resets to zero nor accumulates. The deadline is fresh, because
// Section 3 scopes the query timeout to one request, not to a cursor chain.
func (e *Engine) resumeTraversal(ctx context.Context, token, endpoint, queryHash string) (*resumeState, error) {
	if e.signer == nil {
		return nil, cursorInvalid("continuations are not available in this workspace")
	}
	now := e.now()
	payload, err := e.signer.Verify(token, pagination.PurposeCursor, now)
	if err != nil {
		return nil, err
	}
	var c traversalCursor
	if err := json.Unmarshal(payload, &c); err != nil {
		return nil, cursorInvalid("cursor payload is malformed")
	}
	if err := c.validate(); err != nil {
		return nil, err
	}
	if c.Endpoint != endpoint {
		return nil, cursorInvalid("cursor was issued by a different endpoint")
	}
	if c.QueryHash != queryHash {
		return nil, cursorInvalid("cursor was issued for a different query")
	}
	binding := e.adjacency.Binding()
	if c.GenerationID != binding.GenerationID || c.AnalysisKey != binding.AnalysisKey {
		return nil, cursorInvalid("cursor pins a generation that is no longer the one being read")
	}
	s := &resumeState{
		Cursor: c,
		// now is carried with the deadline: checkWalk compares them, and a
		// deadline measured on the engine clock against time.Now is the
		// two-clock defect this budget would otherwise reintroduce.
		Budget: &budget{visited: c.Visited, edges: c.Edges,
			deadline: now.Add(e.limits.QueryTimeout), now: e.now},
		Visited: map[model.NodeID]struct{}{},
	}
	if c.SpoolID == "" {
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
	return s, nil
}

// nextTraversalCursor writes a FRESH spool for c's frontier and visited set and
// signs the continuation for the page after it.
//
// It returns an empty token, and no error, whenever the traversal must stop
// rather than continue: an exhausted cumulative budget, an engine with no
// signer (continuations are not offered), or a frontier with no spool to spill
// into. In every one of those cases the caller reports Truncated with no
// NextCursor, which is the same contract as running out of page items.
func (e *Engine) nextTraversalCursor(b *budget, c continuation) (string, error) {
	if e.signer == nil {
		return "", nil
	}
	// This is the ONE place the cumulative caps decide whether a traversal may
	// continue. A resumed page that arrives already at cap therefore answers
	// Truncated with no NextCursor without spending the budget a second time.
	if b.visited >= int64(e.limits.MaxVisited) || b.edges >= int64(e.limits.MaxEdges) {
		return "", nil
	}
	if len(c.Frontier) == 0 && c.LastKey == "" {
		// Nothing left to resume from: the walk is complete.
		return "", nil
	}
	// A frontier OR an already-admitted visited set has to survive the
	// response, and both live in the spool; with spilling disabled neither can,
	// so the walk stops here rather than resuming without them.
	needsSpool := len(c.Frontier) > 0 || len(c.Visited) > 0
	if needsSpool && e.spools == nil {
		return "", nil
	}
	next := traversalCursor{
		Version:      traversalCursorVersion,
		Endpoint:     c.Endpoint,
		GenerationID: e.adjacency.Binding().GenerationID,
		AnalysisKey:  e.adjacency.Binding().AnalysisKey,
		QueryHash:    c.QueryHash,
		LeaseID:      c.LeaseID,
		LastKey:      c.LastKey,
		Depth:        c.Depth,
		Visited:      b.visited,
		Edges:        b.edges,
		ExpiresAt:    e.now().Add(e.limits.CursorTTL).UTC().Truncate(time.Second),
	}
	if needsSpool {
		id, err := e.spill(next, c)
		if err != nil {
			return "", err
		}
		next.SpoolID = id
	}
	if err := next.validate(); err != nil {
		return "", err
	}
	payload, err := json.Marshal(next)
	if err != nil {
		return "", &model.Error{Code: model.CodeInternal, Message: "cursor encoding: " + err.Error()}
	}
	return e.signer.Sign(pagination.PurposeCursor, payload, next.ExpiresAt)
}

// spill writes one fresh spool holding c's frontier and visited set and returns
// its id. The spool is bound to next's lease, generation, analysis key and
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
