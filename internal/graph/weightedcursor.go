package graph

import (
	"context"
	"encoding/json"
	"time"

	"github.com/Sawmonabo/codectx/internal/model"
	"github.com/Sawmonabo/codectx/internal/paced"
	"github.com/Sawmonabo/codectx/internal/pagination"
)

// The weighted walk's continuation. Like the shortest-path search's
// (pathcursor.go) it names a retained state directory -- the walk's whole
// label state, its progress and its listing position live there -- and
// carries only the binding, the lease and the cumulative counters the answer
// reports. The `w` discriminator refuses every other continuation shape.

// weightedCursorVersion is this payload's version; a payload of another shape
// decodes to zero and is refused.
const weightedCursorVersion = 1

// weightedCursor is the signed continuation payload of a weighted walk.
type weightedCursor struct {
	Kind         string             `json:"k"`
	Version      int                `json:"version"`
	Endpoint     string             `json:"endpoint"`
	GenerationID model.GenerationID `json:"generation_id"`
	AnalysisKey  model.AnalysisKey  `json:"analysis_key"`
	QueryHash    string             `json:"query_hash"`
	LeaseID      string             `json:"lease_id"`
	ScratchID    string             `json:"scratch_id"`
	Visited      int64              `json:"visited"`
	Edges        int64              `json:"edges"`
	ExpiresAt    time.Time          `json:"expires_at"`
}

func (c weightedCursor) validate() error {
	switch {
	case c.Kind != spoolRecordWeighted:
		return cursorInvalid("cursor was not issued for a weighted walk")
	case c.Version != weightedCursorVersion:
		return cursorInvalid("cursor version is not supported")
	case c.Endpoint != weightedEndpoint:
		return cursorInvalid("cursor was issued by a different endpoint")
	case c.GenerationID <= 0:
		return cursorInvalid("cursor does not pin a generation")
	case !model.ValidHexID(c.QueryHash) || !model.ValidHexID(c.ScratchID):
		return cursorInvalid("cursor query hash and scratch id must be well-formed identifiers")
	case c.LeaseID != "" && !model.ValidHexID(c.LeaseID):
		return cursorInvalid("cursor lease id is malformed")
	case c.AnalysisKey == "" || len(c.AnalysisKey) > model.MaxIdentifierBytes:
		return cursorInvalid("cursor does not name an analysis key")
	case c.Visited < 0 || c.Edges < 0:
		return cursorInvalid("cursor carries a negative budget")
	case c.ExpiresAt.IsZero():
		return cursorInvalid("cursor has no expiry")
	}
	return nil
}

func (c weightedCursor) spoolCursor() pagination.Cursor {
	return pagination.Cursor{
		Endpoint: c.Endpoint, GenerationID: c.GenerationID, AnalysisKey: c.AnalysisKey,
		QueryHash: c.QueryHash, SpoolID: c.ScratchID, LeaseID: c.LeaseID, ExpiresAt: c.ExpiresAt,
	}
}

// weightedResume is what a continuation restores.
type weightedResume struct {
	Cursor  weightedCursor
	Dir     string
	Release func()
}

// resumeWeighted verifies token against this engine's generation and the
// request's query hash and reopens the state the issuing page retained.
func (e *Engine) resumeWeighted(ctx context.Context, token, queryHash string) (*weightedResume, error) {
	if e.signer == nil || e.spools == nil {
		return nil, cursorInvalid("continuations are not available in this workspace")
	}
	payload, err := e.signer.Verify(token, pagination.PurposeCursor, e.now())
	if err != nil {
		return nil, err
	}
	var c weightedCursor
	if err := json.Unmarshal(payload, &c); err != nil {
		return nil, cursorInvalid("cursor payload is malformed")
	}
	if err := c.validate(); err != nil {
		return nil, err
	}
	if c.QueryHash != queryHash {
		return nil, cursorInvalid(
			"cursor was issued for a different query: the roots, relation kinds, depth, page limit and rule that minted it must be repeated on every page")
	}
	binding := e.adjacency.Binding()
	if c.GenerationID != binding.GenerationID || c.AnalysisKey != binding.AnalysisKey {
		return nil, cursorInvalid("cursor pins a generation that is no longer the one being read")
	}
	dir, err := e.spools.OpenDir(ctx, c.spoolCursor(), e.now())
	if err != nil {
		return nil, err
	}
	return &weightedResume{Cursor: c, Dir: dir, Release: func() {
		e.releaseConsumed(context.WithoutCancel(ctx), c.ScratchID, c.LeaseID)
	}}, nil
}

// nextWeightedCursor commits the page's state, hands its directory to the
// spool store under a fresh cursor-owned lease and signs the continuation. It
// returns an empty token, and no error, in a workspace that offers no
// continuations; the caller discloses the stop.
func (e *Engine) nextWeightedCursor(ctx context.Context, sc *pathScratch, queryHash string,
	w *weightedWalk) (string, error) {
	if e.signer == nil || e.spools == nil {
		return "", nil
	}
	binding := e.adjacency.Binding()
	var leaseID string
	if e.leases.Retains() {
		lease, err := e.leases.Acquire(ctx, binding.GenerationID, binding.SnapshotID, model.LeaseCursor)
		if err != nil {
			return "", err
		}
		leaseID = lease.ID
	}
	next := weightedCursor{
		Kind: spoolRecordWeighted, Version: weightedCursorVersion, Endpoint: weightedEndpoint,
		GenerationID: binding.GenerationID, AnalysisKey: binding.AnalysisKey, QueryHash: queryHash,
		LeaseID: leaseID, Visited: w.visited, Edges: w.edges,
		ExpiresAt: e.now().Add(e.limits.CursorTTL).UTC().Truncate(time.Second),
	}
	dir, err := sc.detach()
	if err != nil {
		return "", terminalRetention(e.releaseLease(ctx, leaseID, err))
	}
	id, err := e.spools.AdoptDir(next.spoolCursor(), dir)
	if err != nil {
		_ = paced.RemoveAllFor(paced.LeaseReclamation, dir)
		if pagination.IsBudgetExhausted(err) {
			return "", terminalRetention(e.releaseLease(ctx, leaseID, errRetentionBudget("walk")))
		}
		return "", terminalRetention(e.releaseLease(ctx, leaseID, err))
	}
	next.ScratchID = id
	if err := next.validate(); err != nil {
		return "", terminalRetention(e.releaseLease(ctx, leaseID, err))
	}
	payload, err := json.Marshal(next)
	if err != nil {
		return "", terminalRetention(e.releaseLease(ctx, leaseID,
			&model.Error{Code: model.CodeInternal, Message: "cursor encoding: " + err.Error()}))
	}
	token, err := e.signer.Sign(pagination.PurposeCursor, payload, next.ExpiresAt)
	if err != nil {
		return "", terminalRetention(e.releaseLease(ctx, leaseID, err))
	}
	return token, nil
}
