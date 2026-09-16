package graph

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"strconv"
	"time"

	"github.com/Sawmonabo/codectx/internal/model"
	"github.com/Sawmonabo/codectx/internal/paced"
	"github.com/Sawmonabo/codectx/internal/pagination"
)

// This file is the shortest-path continuation. It is a SEPARATE vocabulary from
// the traversal one in cursor.go, and deliberately so: a traversal page resumes
// a frontier replayed out of a record spool, while a path page resumes an
// external-memory search whose whole state -- settled set, parent DAG, cost
// buckets, the work still owed -- stays exactly where the previous page left
// it, in a retained state directory. Nothing about the two shapes is
// interchangeable, and the `p` discriminator below is what refuses to feed
// either one the other's state.
//
// What the token carries is therefore small and fixed: the retention lease, the
// state directory's id, the position inside the search (the cost bucket and the
// last node finished in it) and the CUMULATIVE visited and edge counters the
// answer reports. The search state itself never enters a token (Section 14.3).

// pathCursorVersion is this payload's version. A payload without it -- a
// pagination.Cursor token or a traversalCursor, both signed under the same
// purpose -- decodes to zero and is refused, so the three token shapes can
// never be interchanged.
const pathCursorVersion = 1

// pathEndpoint is what a path continuation is bound to; a token presented to
// any other endpoint is CTX_CURSOR_INVALID.
const pathEndpoint = "path"

// pathCursor is the signed continuation payload of a shortest-path search.
type pathCursor struct {
	// Kind is the vocabulary discriminator, spoolRecordPath. It is checked
	// before anything else is trusted, so a traversal payload that happens to
	// decode into these fields is refused rather than resumed.
	Kind         string             `json:"k"`
	Version      int                `json:"version"`
	Endpoint     string             `json:"endpoint"`
	GenerationID model.GenerationID `json:"generation_id"`
	AnalysisKey  model.AnalysisKey  `json:"analysis_key"`
	QueryHash    string             `json:"query_hash"`
	LeaseID      string             `json:"lease_id"`
	// ScratchID names the retained state directory holding the search.
	ScratchID string `json:"scratch_id"`
	// BucketCost and LastNode are the resume position: the cost bucket the
	// issuing page stopped in, and the last node it finished inside that
	// bucket. The page after it re-reads the bucket from after that node, so
	// the groups the stop never reached are still settled.
	BucketCost int64        `json:"bucket_cost"`
	LastNode   model.NodeID `json:"last_node,omitempty"`
	// Visited and Edges are CUMULATIVE across every page of this search: the
	// per-page budgets are refilled on each page, and these are what the answer
	// discloses as the work the whole search has spent.
	Visited   int64     `json:"visited"`
	Edges     int64     `json:"edges"`
	ExpiresAt time.Time `json:"expires_at"`
}

// validate enforces the payload's shape, on the way in and on the way out: a
// signature proves only that we issued the bytes, not that this build still
// agrees with them.
func (c pathCursor) validate() error {
	if c.Kind != spoolRecordPath {
		return cursorInvalid("cursor was not issued for a path search")
	}
	if c.Version != pathCursorVersion {
		return cursorInvalid("cursor version is not supported")
	}
	if c.Endpoint != pathEndpoint {
		return cursorInvalid("cursor was issued by a different endpoint")
	}
	if c.GenerationID <= 0 {
		return cursorInvalid("cursor does not pin a generation")
	}
	if !model.ValidHexID(c.QueryHash) || !model.ValidHexID(c.LeaseID) || !model.ValidHexID(c.ScratchID) {
		return cursorInvalid("cursor query hash, lease id and scratch id must be well-formed identifiers")
	}
	if c.AnalysisKey == "" || len(c.AnalysisKey) > model.MaxIdentifierBytes {
		return cursorInvalid("cursor does not name an analysis key")
	}
	if len(c.LastNode) > model.MaxIdentifierBytes {
		return cursorInvalid("cursor resume position exceeds its bound")
	}
	if c.BucketCost < 0 || c.Visited < 0 || c.Edges < 0 {
		return cursorInvalid("cursor carries a negative position or budget")
	}
	if c.ExpiresAt.IsZero() {
		return cursorInvalid("cursor has no expiry")
	}
	return nil
}

// spoolCursor projects the payload onto the binding pagination.Spools checks
// before it hands back the retained state directory.
func (c pathCursor) spoolCursor() pagination.Cursor {
	return pagination.Cursor{
		Endpoint:     c.Endpoint,
		GenerationID: c.GenerationID,
		AnalysisKey:  c.AnalysisKey,
		QueryHash:    c.QueryHash,
		SpoolID:      c.ScratchID,
		LeaseID:      c.LeaseID,
		ExpiresAt:    c.ExpiresAt,
	}
}

// pathQueryHash is the normalized question a path continuation is bound to.
// Relation kinds are a sorted copy, so two spellings of one query share a
// cursor and two different queries never do: presenting a path cursor to a
// differently filtered or differently depth-bounded search is
// CTX_CURSOR_INVALID rather than a silently repinned answer.
//
// The visited and edge bounds are deliberately NOT part of it. They are
// per-page work budgets, exactly as they are for a traversal, and a caller that
// asks the next page for less work must not be refused for it.
func pathQueryHash(kinds []model.RelationKind, direction model.Direction,
	from, to model.NodeID, maxDepth int) string {
	sorted := make([]string, len(kinds))
	for i, k := range kinds {
		sorted[i] = string(k)
	}
	sort.Strings(sorted)
	h := model.NewHasher(queryHashDomain)
	h.AddString(pathEndpoint)
	h.AddString(string(direction))
	h.AddString(string(from))
	h.AddString(string(to))
	h.AddString(strconv.Itoa(maxDepth))
	for _, k := range sorted {
		h.AddString(k)
	}
	return h.Sum()
}

// pathResume is what a path continuation restores: the decoded cursor and the
// retained state directory to reopen the search from.
type pathResume struct {
	Cursor pathCursor
	Dir    string
	// Release ends the consumed continuation's retention. The caller defers it:
	// the state directory is READ for the whole page, and -- when that page
	// also ends early -- is adopted under a fresh lease before this runs, so
	// the release then finds nothing left to remove and costs only the lease.
	Release func()
}

// resumePath verifies token, binds it to the engine's pinned generation and to
// queryHash, and reopens the state the issuing page retained.
//
// The deadline is THIS request's: Section 3 scopes the query timeout to one
// request, not to a cursor chain.
func (e *Engine) resumePath(ctx context.Context, token, queryHash string) (*pathResume, error) {
	if e.signer == nil || e.spools == nil {
		return nil, cursorInvalid("continuations are not available in this workspace")
	}
	payload, err := e.signer.Verify(token, pagination.PurposeCursor, e.now())
	if err != nil {
		return nil, err
	}
	var c pathCursor
	if err := json.Unmarshal(payload, &c); err != nil {
		return nil, cursorInvalid("cursor payload is malformed")
	}
	if err := c.validate(); err != nil {
		return nil, err
	}
	if c.QueryHash != queryHash {
		return nil, cursorInvalid(
			"cursor was issued for a different query: the endpoints, relation kinds and depth that minted it must be repeated on every page")
	}
	binding := e.adjacency.Binding()
	if c.GenerationID != binding.GenerationID || c.AnalysisKey != binding.AnalysisKey {
		return nil, cursorInvalid("cursor pins a generation that is no longer the one being read")
	}
	dir, err := e.spools.OpenDir(ctx, c.spoolCursor(), e.now())
	if err != nil {
		return nil, err
	}
	return &pathResume{Cursor: c, Dir: dir, Release: func() {
		e.releaseConsumed(context.WithoutCancel(ctx), c.ScratchID, c.LeaseID)
	}}, nil
}

// terminalRetention makes a failure raised AFTER the page's state was committed
// and handed to the spool store TERMINAL, whatever its origin.
//
// Past sc.detach() the search state is committed and the directory is no longer
// this request's: the cursor the caller still holds names a position the state
// on disk has moved past, and the directory itself is removed on the way out of
// the branches below. A failure spelled RETRYABLE there would invite the one
// thing that cannot work -- presenting that cursor again -- and the caller
// would meet CTX_CURSOR_INVALID instead of the error it was told to retry.
// errRetentionBudget is already deliberately non-retryable for this reason;
// this is the same rule for the failures that do not choose their own spelling,
// such as a raw filesystem error out of AdoptDir.
func terminalRetention(err error) error {
	if err == nil {
		return nil
	}
	var typed *model.Error
	if errors.As(err, &typed) && !typed.Retryable {
		return err
	}
	return internalErr("path continuation: the search state was committed and handed over, " +
		"so this failure cannot be retried on the same cursor: " + err.Error())
}

// nextPathCursor retains the search's state directory under a fresh
// cursor-owned lease and signs the continuation for the page after it.
//
// It returns an empty token, and no error, whenever the answer must stop rather
// than continue: a workspace that offers no continuations, a process that
// retains nothing, and a shared spool budget that cannot hold the state. In
// every case the caller reports Truncated with no NextCursor, which is the
// contract a page stop already has.
func (e *Engine) nextPathCursor(ctx context.Context, sc *pathScratch, queryHash string, w *pathWalk) (string, error) {
	// A process that writes nothing takes the third stop: it can record no
	// cursor lease, and the search state it would hand to the spool store is
	// reclaimed on that lease's expiry, so adopting the directory without one
	// would leak it. The return is BEFORE sc.detach(), so the directory is
	// never handed over and the request's own deferred close removes it.
	if e.signer == nil || e.spools == nil || !e.leases.Retains() {
		return "", nil
	}
	binding := e.adjacency.Binding()
	// A NEW cursor-owned retention lease, never the pinned reader's query
	// lease: that one ends with this request, so the next page's state would be
	// swept out from under it. The lease expires with the cursor, so the state
	// directory is reclaimed by the ordinary sweep even if no one ever asks for
	// the next page.
	lease, err := e.leases.Acquire(ctx, binding.GenerationID, binding.SnapshotID, model.LeaseCursor)
	if err != nil {
		return "", err
	}
	next := pathCursor{
		Kind:         spoolRecordPath,
		Version:      pathCursorVersion,
		Endpoint:     pathEndpoint,
		GenerationID: binding.GenerationID,
		AnalysisKey:  binding.AnalysisKey,
		QueryHash:    queryHash,
		LeaseID:      lease.ID,
		BucketCost:   w.bucketCost,
		LastNode:     w.lastSettled,
		Visited:      w.visited,
		Edges:        w.spent,
		ExpiresAt:    e.now().Add(e.limits.CursorTTL).UTC().Truncate(time.Second),
	}
	// The page's writes are committed and the directory handed over BEFORE the
	// token is signed: a token naming state that was never committed would
	// resume a search that has forgotten half its work.
	dir, err := sc.detach()
	if err != nil {
		// detach() commits before it can fail, so a failure here leaves the
		// page's writes committed but no directory a continuation could name:
		// not this request's to resume from either way, so terminal too.
		return "", terminalRetention(e.releaseLease(ctx, lease.ID, err))
	}
	id, err := e.spools.AdoptDir(next.spoolCursor(), dir)
	if err != nil {
		// The state is this request's to clean up until the store takes it.
		_ = paced.RemoveAllFor(paced.LeaseReclamation, dir)
		if pagination.IsBudgetExhausted(err) {
			// The shared continuation budget cannot hold this search's state.
			// Reported, never silent: ending the answer here with no token
			// under whichever WORK budget happened to be set told the caller to
			// raise a bound that was not the one that stopped it.
			return "", terminalRetention(e.releaseLease(ctx, lease.ID, errRetentionBudget("search")))
		}
		return "", terminalRetention(e.releaseLease(ctx, lease.ID, err))
	}
	next.ScratchID = id
	if err := next.validate(); err != nil {
		return "", terminalRetention(e.releaseLease(ctx, lease.ID, err))
	}
	payload, err := json.Marshal(next)
	if err != nil {
		return "", terminalRetention(e.releaseLease(ctx, lease.ID,
			&model.Error{Code: model.CodeInternal, Message: "cursor encoding: " + err.Error()}))
	}
	token, err := e.signer.Sign(pagination.PurposeCursor, payload, next.ExpiresAt)
	if err != nil {
		return "", terminalRetention(e.releaseLease(ctx, lease.ID, err))
	}
	return token, nil
}
