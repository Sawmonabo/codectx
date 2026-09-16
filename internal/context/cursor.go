package context

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/Sawmonabo/codectx/internal/model"
	"github.com/Sawmonabo/codectx/internal/paced"
	"github.com/Sawmonabo/codectx/internal/pagination"
)

// Ruling C7's continuation token. resume.go persists WHAT a completed pass
// produced; this file is WHO may read it back.
//
// The token carries no plan state, only identity: the leased state directory's
// id, the pass the next call resumes at, and the compile's normalised request
// identity. That is the same shape internal/graph/pathcursor.go uses for the
// shortest-path search's retained scratch, and it is signed under the same
// shared pagination.PurposeCursor, so a token minted by any other surface
// decodes here into a payload whose discriminator refuses it.

// contextCursorVersion fences this payload's shape. A payload without it -- a
// pagination.Cursor token or a graph cursor, both signed under the same purpose
// -- decodes to zero and is refused.
const contextCursorVersion = 1

// contextCursorKind is the vocabulary discriminator, checked before anything
// else is trusted.
const contextCursorKind = "ctx-plan"

// contextEndpoint is what a compile continuation is bound to; a token presented
// to any other endpoint is CTX_CURSOR_INVALID.
const contextEndpoint = "context.plan"

// contextCursor is the signed continuation payload of an interrupted compile.
type contextCursor struct {
	Kind         string             `json:"k"`
	Version      int                `json:"version"`
	Endpoint     string             `json:"endpoint"`
	GenerationID model.GenerationID `json:"generation_id"`
	AnalysisKey  model.AnalysisKey  `json:"analysis_key"`
	// RequestHash is the compile's manifest identity: it folds the binding, the
	// request and the configured bounds, so a cursor minted for one request can
	// never resume another and a configuration change between two calls ends
	// the continuation rather than splicing two different compiles.
	RequestHash string `json:"request_hash"`
	LeaseID     string `json:"lease_id"`
	// StateID names the leased state directory holding the checkpoint.
	StateID string `json:"state_id"`
	// Pass is the index of the first unfinished pass, mirrored from the state
	// file so a cursor that no longer agrees with the directory it names is
	// refused instead of resumed at the wrong boundary.
	Pass      int       `json:"pass"`
	ExpiresAt time.Time `json:"expires_at"`
}

// validate enforces the payload's shape on the way in and on the way out: a
// signature proves only that we issued the bytes, not that this build still
// agrees with them.
func (c contextCursor) validate() error {
	switch {
	case c.Kind != contextCursorKind:
		return cursorInvalid("cursor was not issued for a context compile")
	case c.Version != contextCursorVersion:
		return cursorInvalid("cursor version is not supported")
	case c.Endpoint != contextEndpoint:
		return cursorInvalid("cursor was issued by a different endpoint")
	case c.GenerationID <= 0:
		return cursorInvalid("cursor does not pin a generation")
	case !model.ValidHexID(c.RequestHash) || !model.ValidHexID(c.LeaseID) || !model.ValidHexID(c.StateID):
		return cursorInvalid("cursor request hash, lease id and state id must be well-formed identifiers")
	case c.AnalysisKey == "" || len(c.AnalysisKey) > model.MaxIdentifierBytes:
		return cursorInvalid("cursor does not name an analysis key")
	case c.Pass <= 0:
		return cursorInvalid("cursor names no unfinished pass")
	case c.ExpiresAt.IsZero():
		return cursorInvalid("cursor has no expiry")
	}
	return nil
}

// spoolCursor projects the payload onto the binding pagination.Spools checks
// before it hands back the leased state directory.
func (c contextCursor) spoolCursor() pagination.Cursor {
	return pagination.Cursor{
		Endpoint:     c.Endpoint,
		GenerationID: c.GenerationID,
		AnalysisKey:  c.AnalysisKey,
		QueryHash:    c.RequestHash,
		SpoolID:      c.StateID,
		LeaseID:      c.LeaseID,
		ExpiresAt:    c.ExpiresAt,
	}
}

// cursorInvalid is the typed refusal of a token this compile may not resume.
func cursorInvalid(msg string) error {
	return &model.Error{Code: model.CodeCursorInvalid, Message: msg}
}

// continuationsAvailable reports whether this workspace can mint and resume a
// compile continuation at all. A workspace composed without the spool store,
// the signer or the lease store offers none, and a deadline then ends the
// answer exactly as it did before ruling C7.
func (c *Compiler) continuationsAvailable() bool {
	return c.spools != nil && c.signer != nil && c.leases != nil
}

// resumeState verifies token, binds it to this compile's binding and request
// identity, and reopens the state directory the interrupted call retained.
//
// The deadline is THIS request's: Section 3 scopes the query timeout to one
// request, not to a cursor chain.
func (c *Compiler) resumeState(ctx context.Context, token string, b model.Binding, requestHash string) (contextCursor, string, error) {
	if !c.continuationsAvailable() {
		return contextCursor{}, "", cursorInvalid("continuations are not available in this workspace")
	}
	payload, err := c.signer.Verify(token, pagination.PurposeCursor, c.now())
	if err != nil {
		return contextCursor{}, "", err
	}
	var cur contextCursor
	if err := json.Unmarshal(payload, &cur); err != nil {
		return contextCursor{}, "", cursorInvalid("cursor payload is malformed")
	}
	if err := cur.validate(); err != nil {
		return contextCursor{}, "", err
	}
	if cur.RequestHash != requestHash {
		return contextCursor{}, "", cursorInvalid(
			"cursor was issued for a different context request: the same request and the same configured bounds must be repeated on every call")
	}
	if cur.GenerationID != b.GenerationID || cur.AnalysisKey != b.AnalysisKey {
		return contextCursor{}, "", cursorInvalid("cursor pins a generation that is no longer the one being read")
	}
	dir, err := c.spools.OpenDir(ctx, cur.spoolCursor(), c.now())
	if err != nil {
		return contextCursor{}, "", err
	}
	return cur, dir, nil
}

// releaseState ends a consumed continuation's retention. It runs on an
// uncancelled context: the call that consumed the state may itself have ended
// on its deadline, and leaving the lease live would pin a generation against
// retention for the full cursor TTL for a continuation nobody can ask for.
//
// A failure is left to the lease's own TTL and the spool sweep rather than
// raised -- this is reclamation, not an invariant the answer depends on -- but
// the operator hears about it, because it is real retained disk.
func (c *Compiler) releaseState(ctx context.Context, stateID, leaseID string) {
	if stateID != "" && c.spools != nil {
		if err := c.spools.Release(stateID); err != nil {
			c.logger().Warn("a consumed context continuation could not be released",
				"component", "context", "error", err)
		}
	}
	if leaseID != "" && c.leases != nil {
		if err := c.leases.Release(context.WithoutCancel(ctx), leaseID); err != nil {
			c.logger().Warn("a consumed context continuation's lease could not be released",
				"component", "context", "error", err)
		}
	}
}

// nextStateCursor adopts dir as this compile's leased continuation state and
// signs the token the next call resumes from.
//
// It answers an empty token, and no error, whenever the compile must stop
// rather than continue: a workspace that offers no continuations, and a shared
// spool budget that cannot hold the state. In both cases the caller reports the
// answer truncated with no cursor, which is the contract a page stop already
// has. dir is this call's to remove until the store takes it, and is removed on
// every path that does not hand it over.
func (c *Compiler) nextStateCursor(ctx context.Context, b model.Binding, requestHash string, pass int, dir string) (string, error) {
	if !c.continuationsAvailable() {
		_ = paced.RemoveAll(dir)
		return "", nil
	}
	// The call's own context is already past its deadline on the path that
	// mints this token, so the lease is acquired on an uncancelled one: the
	// checkpoint is written and a token that cannot be minted wastes it.
	mintCtx := context.WithoutCancel(ctx)
	// A NEW cursor-owned retention lease, never the compile's pinned reader
	// lease: that one ends with this call, so the next call's state would be
	// swept out from under it. The lease expires with the cursor, so state
	// nobody resumes is reclaimed by the ordinary sweep.
	lease, err := c.leases.Acquire(mintCtx, b.GenerationID, b.SnapshotID, model.LeaseCursor)
	if err != nil {
		_ = paced.RemoveAll(dir)
		return "", err
	}
	next := contextCursor{
		Kind:         contextCursorKind,
		Version:      contextCursorVersion,
		Endpoint:     contextEndpoint,
		GenerationID: b.GenerationID,
		AnalysisKey:  b.AnalysisKey,
		RequestHash:  requestHash,
		LeaseID:      lease.ID,
		Pass:         pass,
		ExpiresAt:    c.now().Add(c.cfg.Storage.QueryCursorTTL.Std()).UTC().Truncate(time.Second),
	}
	id, err := c.spools.AdoptDir(next.spoolCursor(), dir)
	if err != nil {
		// The state is this call's to clean up until the store takes it.
		_ = paced.RemoveAll(dir)
		if pagination.IsBudgetExhausted(err) {
			// The shared continuation budget is full: the answer ends here,
			// truncated and without a token, exactly as a spooled page does.
			return "", c.releaseMintLease(mintCtx, lease.ID, nil)
		}
		return "", c.releaseMintLease(mintCtx, lease.ID, err)
	}
	next.StateID = id
	if err := next.validate(); err != nil {
		return "", c.releaseMintLease(mintCtx, lease.ID, err)
	}
	payload, err := json.Marshal(next)
	if err != nil {
		return "", c.releaseMintLease(mintCtx, lease.ID,
			&model.Error{Code: model.CodeInternal, Message: "context cursor encoding: " + err.Error()})
	}
	return c.signer.Sign(pagination.PurposeCursor, payload, next.ExpiresAt)
}

// releaseMintLease returns a lease whose cursor never reached the caller and
// reports cause. A release that itself fails is joined onto cause rather than
// dropped: a retention lease that will now expire only with its TTL is
// something the operator is told about.
func (c *Compiler) releaseMintLease(ctx context.Context, id string, cause error) error {
	if err := c.leases.Release(ctx, id); err != nil {
		return errors.Join(cause, &model.Error{Code: model.CodeInternal,
			Message: "a context continuation lease could not be released: " + err.Error()})
	}
	return cause
}
