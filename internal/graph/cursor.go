package graph

import (
	"context"
	"encoding/json"
	"errors"
	"os"
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
// Version 3 also reintroduces a ranked spool vocabulary -- a different one: ruling P2's ranked tail, where the walk
// has already run to completion and the spool holds the globally RANKED
// remainder of the answer rather than a per-page chunk.
//
// The version is bumped even though the ranked fields below are purely
// additive -- a version-2 token would decode with Ranked false and route to the
// walk replay perfectly well -- because what an impact or rollup continuation
// MEANS changed, and nothing in a token's shape distinguishes the two
// contracts. A version-2 impact cursor was minted by a page that ranked its own
// chunk and left the walk unfinished; resuming it here would finish the walk
// and rank only what is left, so the caller would receive one answer ordered
// two different ways and never be told. The version is therefore the only
// honest refusal. It ends in-flight neighbours and traversal cursors too, which
// is accepted: a cursor is short-lived, and CTX_CURSOR_INVALID's remedy is to
// re-run the query, which returns the whole answer under one contract.
//
// Version 3 additionally orders the spool's visited section by NodeID. Membership is a
// MERGE-JOIN over that order now (visited.go), and a merge-join over a
// version-2 spool -- whose visited records are in admission order -- would
// walk past a node the spool holds and report it absent. That is a
// cross-page RE-ADMISSION: the node would be emitted on two pages. The
// version is what refuses such a token instead of answering it wrong.
//
// Version 4 adds the RETAINED pass-1 input a walk continuation names
// (walkretain.go). A version-3 `f` token names a frontier and a cumulative
// visited set and nothing else, so a build that resumed it would carry the walk
// on, rank what THIS leg admitted, and serve that as the whole blast radius --
// the earlier legs' entities are unreachable, because the carried visited set
// guarantees they are never admitted again. There is no shape in a version-3
// token that says so, so the version is the only honest refusal; a caller
// re-runs the query and receives the whole answer under one contract.
//
// Version 5 moves the cumulative visited set OUT of the spool and into the
// retained state directory, as append-only ascending runs beside a persisted
// membership filter of frozen geometry (visitedstore.go). A version-4 `f` token
// names a spool whose visited SECTION is the whole cumulative set and a
// directory with no run store in it; resuming it here would find an empty run
// store, believe the walk had admitted nothing, and re-admit every node every
// earlier page already reported -- the same entity on page after page. There is
// no shape in a version-4 token that says so, so the version is the only honest
// refusal.
const traversalCursorVersion = 5

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
	// Ranked marks the ranked-tail continuation of ruling P2: the walk is over,
	// the answer is globally ordered, and SpoolID names the records ranked
	// after the page that minted this token. It is an explicit discriminator
	// rather than an inference from the other fields, because the two
	// vocabularies -- a walk to resume and an order to continue -- are read by
	// different code and must never be fed each other's spool.
	Ranked bool `json:"ranked,omitempty"`
	// RetainID names the retained state directory holding the pass-1 INPUT of
	// this walk -- every impactRecord and every pairRecord every leg so far has
	// admitted (walkretain.go). It travels on the WALK vocabulary, never on the
	// ranked one: once the order is settled the input is dead and released. A
	// walk continuation without it is a traversal (neighbours), which ranks
	// nothing and retains nothing.
	RetainID string `json:"retain_id,omitempty"`
	// WalkDone marks ruling P7's third shape: the walk is EXHAUSTED and the
	// ranking is what the deadline cut short. The next request walks nothing --
	// there is no frontier to walk -- and re-runs both passes over the retained
	// input RetainID names. It is an explicit discriminator because a walk
	// continuation with an empty frontier is otherwise indistinguishable from a
	// finished answer, which mints no cursor at all.
	WalkDone bool `json:"walk_done,omitempty"`
	// RankOffset is how many records of that spool the next page skips, and
	// RankServed/RankTotal the cumulative answer facts the page discloses.
	// An offset is carried instead of a sort key because Spools.Open streams
	// from the start and cannot seek; see rankedTail (impactrank.go).
	RankOffset int   `json:"rank_offset,omitempty"`
	RankServed int64 `json:"rank_served,omitempty"`
	RankTotal  int64 `json:"rank_total,omitempty"`
	// PairServed and PairTotal are the same two facts for the SECOND ranked
	// list an impact answer carries, its package rollup. One result has room
	// for one cursor, so the two lists are paged together behind it: a
	// continuation is minted while EITHER list has records left, and each page
	// takes at most the page limit from each. They are zero on the endpoints
	// that rank a single list.
	PairServed int64 `json:"pair_served,omitempty"`
	PairTotal  int64 `json:"pair_total,omitempty"`
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
	if c.RetainID != "" && !model.ValidHexID(c.RetainID) {
		return cursorInvalid("cursor retained-input id is malformed")
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
	if err := c.validateRanked(); err != nil {
		return err
	}
	if c.ExpiresAt.IsZero() {
		return cursorInvalid("cursor has no expiry")
	}
	return nil
}

// validateRanked enforces the ruling P2 half of the payload: the ranked fields
// are meaningful only on a ranked continuation and meaningless -- and so
// refused -- on a walk one, which is what keeps a tampered token from steering
// a walk resume into a ranked spool or the reverse.
func (c traversalCursor) validateRanked() error {
	if !c.Ranked {
		if c.RankOffset != 0 || c.RankServed != 0 || c.RankTotal != 0 ||
			c.PairServed != 0 || c.PairTotal != 0 {
			return cursorInvalid("a walk continuation carries a ranked position")
		}
		if c.WalkDone {
			// Ruling P7's shape: nothing left to walk, the whole answer still
			// to rank. A token that claimed it while naming a frontier spool or
			// a keyset position would resume a walk this build believes is
			// over, and lose whatever that frontier still held.
			if c.RetainID == "" {
				return cursorInvalid("a completed walk names no retained input")
			}
			if c.SpoolID != "" || c.LastOwner != "" || c.LastKey != "" {
				return cursorInvalid("a completed walk carries a frontier to resume")
			}
		}
		return nil
	}
	if c.RetainID != "" || c.WalkDone {
		// The order is settled and the pass-1 input is released: a ranked
		// continuation that named one would resume a sort nothing will run.
		return cursorInvalid("a ranked continuation carries a walk's retained input")
	}
	if c.SpoolID == "" {
		return cursorInvalid("a ranked continuation names no result spool")
	}
	if c.LastOwner != "" || c.LastKey != "" {
		return cursorInvalid("a ranked continuation carries a traversal keyset position")
	}
	if c.RankOffset < 0 || c.RankServed < 0 || c.RankTotal < 0 {
		return cursorInvalid("cursor carries a negative ranked position")
	}
	if c.PairServed < 0 || c.PairTotal < 0 {
		return cursorInvalid("cursor carries a negative ranked position")
	}
	if c.RankServed > c.RankTotal || int64(c.RankOffset) > c.RankTotal ||
		c.PairServed > c.PairTotal {
		return cursorInvalid("cursor continues past the end of the ranked answer")
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

// retainCursor is the same projection for the RETAINED pass-1 input: the spool
// store checks one binding for every entry it holds, and the retained state
// directory is bound to this cursor exactly as the frontier spool is. Only the
// entry's id differs, which is why the two projections are separate functions
// rather than one with a flag.
func (c traversalCursor) retainCursor() pagination.Cursor {
	rc := c.spoolCursor()
	rc.SpoolID = c.RetainID
	return rc
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
// Ruling P2 adds a THIRD kind, and one spool shape that is not a walk at all.
// An impact or rollup request runs its walk to completion, ranks the whole
// answer, serves the first page and spools the globally ranked remainder; that
// spool's leading record is `r`, and every record after it is a bare ranked
// record (impactrank.go's codec), not one of these envelopes. The marker is
// what makes the two spool shapes self-identifying: the traversal replay below
// refuses an `r` record through its default branch, so a ranked spool can never
// be replayed as a frontier, and readRankedHeader refuses an `f` or `v` one.
//
// "p" is the ShortestPath continuation's discriminator. It names no spool
// RECORD -- a path search retains its whole external-memory state as a
// directory rather than as a record stream (pathcursor.go) -- so it appears
// here, in the one vocabulary block, purely so no two continuation shapes can
// ever claim the same marker. pathCursor.validate is what reads it.
//
// The older ranked vocabulary of payload version 1 ("a", "e", "p", "c") is
// gone and is not revived: it spooled a per-page chunk, which is the shape
// ruling P2 replaces.
const (
	spoolRecordFrontier = "f"
	spoolRecordVisited  = "v"
	spoolRecordRanked   = "r"
	spoolRecordPath     = "p"
)

// rankedHeader is the leading record of a ranked-tail spool: the marker above,
// the size of the whole ranked answer -- so a continuation can disclose the
// total without counting the spool it is about to stream -- and how many
// records of each SECTION this particular spool holds.
//
// A spool has one section for the endpoints that serve a single ranked list
// (the package rollup's pairs), and two for impact, whose one answer carries a
// ranked entity list AND a ranked package list and must page both under the
// one cursor a result has room for. The layout is
// [header][Count entity records][PairCount pair records], so Count is the only
// thing a reader needs in order to tell one section's records from the other's;
// nothing is inferred from a record's own bytes.
type rankedHeader struct {
	Kind  string `json:"k"`
	Total int64  `json:"total"`
	// Count is how many records of the FIRST section this spool holds, and
	// PairCount how many of the second. Both are spool facts; Total and
	// PairTotal are answer facts.
	Count     int   `json:"count,omitempty"`
	PairTotal int64 `json:"pair_total,omitempty"`
	PairCount int   `json:"pair_count,omitempty"`
}

// encodeRankedHeader renders that record. The marker is set here rather than
// taken from the caller, so no caller can write a header this build's reader
// would then refuse.
func encodeRankedHeader(h rankedHeader) ([]byte, error) {
	h.Kind = spoolRecordRanked
	b, err := json.Marshal(h)
	if err != nil {
		return nil, &model.Error{Code: model.CodeInternal,
			Message: "continuation state encoding: " + err.Error()}
	}
	return b, nil
}

// decodeRankedHeader reads it back. A record that is not this build's ranked
// header is CTX_CURSOR_INVALID rather than a page served from a spool whose
// shape this build guessed at.
func decodeRankedHeader(record []byte) (rankedHeader, error) {
	var h rankedHeader
	if err := json.Unmarshal(record, &h); err != nil || h.Kind != spoolRecordRanked ||
		h.Total < 0 || h.Count < 0 || h.PairTotal < 0 || h.PairCount < 0 {
		return rankedHeader{}, cursorInvalid("the cursor names a result page this build cannot replay")
	}
	return h, nil
}

// spoolRecord is one spilled record. The field names are short because the
// spool byte budget is shared across every live continuation in the process.
type spoolRecord struct {
	Kind  string           `json:"k"`
	Node  model.NodeID     `json:"n,omitempty"`
	Depth int              `json:"d,omitempty"`
	Cost  int64            `json:"c,omitempty"`
	Via   model.RelationID `json:"v,omitempty"`
	// Route is the frontier record's parent chain (graph.go). It is spilled
	// because an impact answer resumed from this spool must still be able to
	// report the path that reached each affected node, and the chain cannot be
	// rebuilt from a frontier alone.
	Route []model.RelationID `json:"rt,omitempty"`
}

// resumeState is what a continuation restores: the decoded cursor, a budget
// seeded with what the earlier pages already spent, the frontier they stopped
// at and the nodes they already admitted.
type resumeState struct {
	Cursor   traversalCursor
	Budget   *budget
	Frontier []frontierState
	// Visited streams the VISITED SECTION of the spool the earlier pages
	// wrote, ascending by NodeID: every node they admitted except the frontier
	// they stopped at, which Frontier above already carries into this page's
	// front. It is a STREAM and not a map because the cumulative set is sized
	// by the walk: materializing it here was the last repository-sized heap
	// structure on the traversal path (visited.go).
	Visited visitedStream
	// Filter summarizes the nodes Visited will replay. It is built during the
	// replay below -- which already decodes every record to rebuild the
	// frontier -- so it costs no extra I/O, and it lets a level whose
	// candidates are all freshly reached skip the stream entirely.
	Filter *visitedFilter
	// Retain is the retained pass-1 input the earlier legs appended to, reopened
	// for append. It is nil on a continuation that retained none (a neighbours
	// traversal), and the caller discards it.
	Retain *retainedWalk
	// Release ends the replayed continuation's spool, its retained input and
	// its lease. The caller defers it: the spool must outlive the walk, because
	// both the membership probes and the next page's spill read from it, and
	// the retained input is appended to for the whole page.
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
	// Frontier is where the walk stopped, and Visited the nodes this page
	// contributes to the cumulative admitted set, ascending. Both are bounded
	// by the page and the frontier ceiling, never by the walk.
	//
	// Where Visited GOES depends on which state this endpoint keeps. A walk
	// with a retained run store (Retain below) appends it there as one more
	// ascending run and spills no visited section at all. The paged traversal,
	// which retains nothing, still writes it into the fresh spool's visited
	// section merged with Carried, and its Visited therefore carries the
	// resumed frontier too -- the previous spool's section excluded those
	// nodes, so nothing else would carry them forward.
	Frontier []frontierState
	Visited  []model.NodeID
	// Carried streams the visited SECTION of the spool the earlier pages
	// wrote, ascending. spill merges it with Visited without materializing it,
	// so a walk of any size costs one spool block of heap here. It is IGNORED
	// -- and left nil by its callers -- whenever Retain holds a run store,
	// because nothing is copied forward on that path.
	Carried visitedStream
	// Retain is the pass-1 input this leg appended to, handed over for the
	// store to adopt (walkretain.go). It is nil for the endpoints that rank
	// nothing -- a neighbours page has no pass-1 input to retain.
	Retain *retainedWalk
	// WalkDone says the frontier is empty because the walk FINISHED, not
	// because there is nothing to continue: ruling P7's mid-rank deadline. It
	// is what lets nextTraversalCursor mint a token for a walk with no
	// frontier, which it otherwise refuses.
	WalkDone bool
}

// retainedVisited is c's append-only cumulative admitted-node set, or nil on an
// endpoint that keeps its set in the continuation spool instead.
func (c continuation) retainedVisited() *visitedStore {
	if c.Retain == nil {
		return nil
	}
	return c.Retain.visited
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
		// The retained input is released with the same lease: by the time this
		// runs, either the ranking has consumed it or the page that continued
		// the walk has renamed it out from under this id (nextTraversalCursor),
		// so the release then costs only the accounting.
		e.releaseConsumed(context.WithoutCancel(ctx), c.RetainID, "")
		e.releaseConsumed(context.WithoutCancel(ctx), c.SpoolID, c.LeaseID)
	}}
	if c.RetainID != "" {
		if e.spools == nil {
			return nil, cursorInvalid("continuation state has expired or was released")
		}
		dir, err := e.spools.OpenDir(ctx, c.retainCursor(), now)
		if err != nil {
			return nil, err
		}
		if s.Retain, err = reopenRetainedWalk(dir, c.RetainID); err != nil {
			return nil, err
		}
		// The cumulative admitted set and its membership summary come from the
		// retained runs, which cost the manifest and the filter to load and
		// nothing per admitted node. The spool replay below reads the FRONTIER
		// and nothing else.
		s.Visited, s.Filter = s.Retain.visited.stream, s.Retain.visited.filter
	}
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
	// The paged traversal still carries its cumulative set in the spool, so its
	// membership summary is still built here, inside the replay that is already
	// decoding every record to rebuild the frontier. A walk with a retained run
	// store has both already and adds nothing per record.
	var spoolFilter *visitedFilter
	if s.Visited == nil {
		spoolFilter = newFrozenVisitedFilter(spoolFilterBits(c.Visited,
			e.limits.FrontierBytes/visitedFilterBudgetShare), visitedFilterProbes)
		s.Filter = spoolFilter
	}
	err = e.spools.Open(ctx, c.spoolCursor(), now, func(record []byte) error {
		var r spoolRecord
		if err := json.Unmarshal(record, &r); err != nil {
			return cursorInvalid("continuation state is not readable")
		}
		switch r.Kind {
		case spoolRecordFrontier:
			s.Frontier = append(s.Frontier, frontierState{Depth: r.Depth, Cost: r.Cost, Node: r.Node, Via: r.Via, Route: r.Route})
		case spoolRecordVisited:
			// Deliberately not accumulated: the cumulative set stays on disk
			// and is streamed by s.Visited. The summary describes exactly what
			// that stream replays, so a miss is a proof the stream holds
			// nothing; a frontier record's node is answered from the front
			// instead.
			spoolFilter.addWords(r.Node, nil)
		default:
			return cursorInvalid("continuation state is not readable")
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
	if s.Visited != nil {
		// The retained run store is the authoritative set: the walk endpoints
		// spill no visited section at all, so there is nothing here to stream.
		return s, nil
	}
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

// errRetentionBudget is the refusal a page raises when the shared continuation
// byte budget cannot hold the state its next page would resume from.
//
// It is a REFUSAL, not a truncation. Both of these endpoints used to return an
// empty token and a nil error here, so the answer ended mid-walk marked with
// whichever reason had already been recorded -- "query deadline reached" for
// impact, a work budget for path -- and a caller reading that reason was told
// something false: raising the named budget was the only thing that could let
// the walk continue, and the reason never named it. An answer that stops
// because a user-set bound was reached must say so and name the key.
//
// Not retryable: repeating the request frees no bytes. That also keeps it
// terminal for terminalOutcome, so the consumed continuation is released.
func errRetentionBudget(what string) error {
	return (&model.Error{
		Code:        model.CodeResourceLimit,
		Message:     "graph: the continuation state this " + what + " must keep to resume does not fit the shared continuation budget",
		Remediation: "raise resources.max_temp_bytes, or narrow the query with --depth, --visited or --edges so the walk finishes within one page",
	}).WithDetail("limit", "resources.max_temp_bytes")
}

// terminalOutcome reports whether a page's outcome ENDS the continuation the
// page replayed. Only a terminal outcome may release the consumed state.
//
// Terminal: the page was served (err nil -- either the answer completed or a
// fresh continuation was minted from it), or the request was refused for a
// reason that repeating it cannot change (CTX_CURSOR_INVALID, an argument
// rejection, an internal defect). Releasing then is what keeps a spool and a
// retention lease from outliving the walk they belong to.
//
// NOT terminal: a RETRYABLE failure -- CTX_WORKSPACE_BUSY from a contended
// store, a transient read error, the query deadline. Those tell the caller to
// present the SAME cursor again, so the state that cursor names has to still be
// adoptable; releasing it turned a busy page into CTX_CURSOR_INVALID and made
// the whole walk behind it unrecoverable. The state is still bounded: it
// expires with the cursor's own lease TTL.
//
// A budget-exhaustion refusal is deliberately NOT retryable (see
// errRetentionBudget), so it releases like any other terminal refusal.
func terminalOutcome(err error) bool {
	if err == nil {
		return true
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return false
	}
	var typed *model.Error
	if errors.As(err, &typed) {
		return !typed.Retryable
	}
	// An untyped error is not a refusal this engine classified; keeping the
	// state costs one TTL and losing it costs the walk.
	return false
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
	resumesWalk := len(c.Frontier) > 0 || c.LastKey != "" || c.WalkDone
	if !resumesWalk {
		// Nothing left to resume from: the answer is complete.
		return "", nil
	}
	// A frontier or an already-admitted visited set has to survive the
	// response, and both live in the spool; with spilling disabled neither can,
	// so the answer stops here rather than resuming without them.
	// A run store takes this page's admissions itself, so a page with nothing
	// but admissions to record needs no spool of its own.
	store := c.retainedVisited()
	needsSpool := len(c.Frontier) > 0 || (store == nil && len(c.Visited) > 0)
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
		WalkDone:     c.WalkDone,
		Visited:      b.visited,
		Edges:        b.edges,
		ExpiresAt:    e.now().Add(e.limits.CursorTTL).UTC().Truncate(time.Second),
	}
	if store != nil {
		// This page's OWN admissions, appended as one more ascending run before
		// anything is handed over: a token minted over a store that had not yet
		// taken them would resume a walk that believes it never admitted them
		// and report every one of them a second time.
		if err := store.appendRun(c.Visited); err != nil {
			return "", e.releaseLease(ctx, lease.ID, err)
		}
	}
	if c.Retain != nil {
		// The pass-1 input is handed over BEFORE the frontier is spilled and
		// before the token is signed: a token that named a frontier whose
		// admitted records were never retained is exactly the silent loss this
		// retention exists to remove.
		id, err := e.retain(next, c.Retain)
		if err != nil {
			return "", e.releaseLease(ctx, lease.ID, err)
		}
		next.RetainID = id
	}
	if needsSpool {
		id, err := e.spill(ctx, next, c)
		if err != nil {
			e.releaseConsumed(ctx, next.RetainID, "")
			return "", e.releaseLease(ctx, lease.ID, err)
		}
		next.SpoolID = id
	}
	if err := next.validate(); err != nil {
		e.releaseConsumed(ctx, next.RetainID, "")
		return "", e.releaseLease(ctx, lease.ID, err)
	}
	payload, err := json.Marshal(next)
	if err != nil {
		e.releaseConsumed(ctx, next.RetainID, "")
		return "", e.releaseLease(ctx, lease.ID,
			&model.Error{Code: model.CodeInternal, Message: "cursor encoding: " + err.Error()})
	}
	token, err := e.signer.Sign(pagination.PurposeCursor, payload, next.ExpiresAt)
	if err != nil {
		e.releaseConsumed(ctx, next.RetainID, "")
		return "", e.releaseLease(ctx, lease.ID, err)
	}
	return token, nil
}

// retain hands the pass-1 input this leg appended to the spool store, bound to
// next exactly as a spool file is, and returns the id the cursor names it by.
//
// An empty id and a nil error mean the shared continuation byte budget cannot
// hold it: the caller ends the answer without a token, the contract a spooled
// page that cannot spill already has. The directory is this request's to clean
// up until the store takes it.
func (e *Engine) retain(next traversalCursor, w *retainedWalk) (string, error) {
	if e.spools == nil {
		return "", nil
	}
	dir, err := w.detach()
	if err != nil {
		return "", err
	}
	id, err := e.spools.AdoptDir(next.retainCursor(), dir)
	if err != nil {
		_ = os.RemoveAll(dir)
		if pagination.IsBudgetExhausted(err) {
			return "", errRetentionBudget("walk")
		}
		return "", err
	}
	return id, nil
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
			Depth: fs.Depth, Cost: fs.Cost, Via: fs.Via, Route: fs.Route}); err != nil {
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
	if c.retainedVisited() != nil {
		// The cumulative set is append-only state in the retained directory
		// (visitedstore.go): this page already added its own run there, every
		// earlier page's run is still where that page wrote it, and c.Carried
		// is nil. Copying anything forward here is exactly the O(visited)
		// per-page write that made paging a walk to completion quadratic.
		if err := sp.Close(); err != nil {
			return "", e.releaseSpool(sp, err)
		}
		return sp.ID(), nil
	}
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
