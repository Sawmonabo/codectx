package workflow

import (
	"context"

	"github.com/Sawmonabo/codectx/internal/model"
	"github.com/Sawmonabo/codectx/internal/storage/sqlite"
)

// Advance applies one guarded transition. The store owns reachability and the
// state_version compare-and-swap; this owns the Section 17.1 service guards the
// store deliberately does not check -- a resolved seed for a direct verify,
// required-full coverage plus a current-scope review for verify -> consolidate
// (or recorded waivers with allow_exploratory_waiver_consolidation), and
// capsule-before-complete ordering. Owned by L1.
func (s *Service) Advance(ctx context.Context, req model.AdvanceRequest) (_ model.WorkflowStatus, _ model.SessionStatus, err error) {
	var (
		noTransition model.WorkflowStatus
		noStatus     model.SessionStatus
	)
	if err := req.Validate(); err != nil {
		return noTransition, noStatus, err
	}

	ctx, cancel := model.QueryDeadline(ctx, s.limits.QueryTimeout)
	defer cancel()
	// This surface's answer is one bounded read (or one mutation): between two
	// pages it accumulates nothing, and its continuation cursor -- where it has
	// one -- is minted from the last item served, never from a deadline. There
	// is therefore nothing for a continuation to resume PAST, so an expired
	// deadline is answered as itself, naming the setting that installed it.
	defer func() { err = model.ClassifyQueryDeadline(ctx, "workflow advance", err) }()

	rec, applied, err := s.transition(ctx, req)
	if err != nil {
		return noTransition, noStatus, err
	}

	// The store's answer is authoritative for what just landed, so the status
	// projection reads the transition's own result rather than taking a second
	// round trip that a concurrent client could have moved again.
	rec.State = applied.State
	rec.StateVersion = applied.StateVersion
	rec.ScopeVersion = applied.ScopeVersion
	rec.ManifestID = applied.ManifestID
	rec.Phase = applied.Phase

	status, err := s.status(ctx, rec)
	if err != nil {
		// The transition is committed. Reporting it beside the projection's
		// failure keeps the caller's next ExpectedVersion honest instead of
		// hiding a state change behind an empty value.
		return applied, noStatus, err
	}
	return applied, status, nil
}

// Close closes a session under the caller's expected version. Closing is
// expressed as AdvanceRequest{Target: StateClosed}: there is no CloseSession on
// the store and no edge out of complete, so closing a completed session is
// CTX_VERSION_CONFLICT. Owned by L1.
func (s *Service) Close(ctx context.Context, req model.SessionRequest, expectedVersion int) (_ model.WorkflowStatus, err error) {
	if err := req.Validate(); err != nil {
		return model.WorkflowStatus{}, err
	}
	advance := model.AdvanceRequest{
		SessionID:       req.SessionID,
		ActorID:         req.ActorID,
		Target:          model.StateClosed,
		ExpectedVersion: expectedVersion,
	}
	// Validate the composed request too, for the bound SessionRequest does not
	// carry: a version below 1 is a caller defect, not a conflict to discover
	// in the store.
	if err := advance.Validate(); err != nil {
		return model.WorkflowStatus{}, err
	}

	ctx, cancel := model.QueryDeadline(ctx, s.limits.QueryTimeout)
	defer cancel()
	// This surface's answer is one bounded read (or one mutation): between two
	// pages it accumulates nothing, and its continuation cursor -- where it has
	// one -- is minted from the last item served, never from a deadline. There
	// is therefore nothing for a continuation to resume PAST, so an expired
	// deadline is answered as itself, naming the setting that installed it.
	defer func() { err = model.ClassifyQueryDeadline(ctx, "workflow close", err) }()

	_, applied, err := s.transition(ctx, advance)
	if err != nil {
		return model.WorkflowStatus{}, err
	}
	return applied, nil
}

// transition loads the session under the actor check, applies the one service
// guard its target carries and then presents the caller's ExpectedVersion to
// the store. It returns the record as it was read, so a caller that needs a
// status projects it from the store's answer.
//
// The record is read to evaluate the guards, never to supply a version: the
// caller's ExpectedVersion travels to AdvanceSession untouched, so the store's
// compare-and-swap is the only arbiter and a conflict surfaces rather than
// being retried under a version this service picked. ExpireSessions bumps
// state_version with no compare-and-swap, so a cached ExpectedVersion can go
// stale under a caller; that is exactly the CTX_VERSION_CONFLICT to report.
func (s *Service) transition(ctx context.Context, req model.AdvanceRequest) (sqlite.SessionRecord, model.WorkflowStatus, error) {
	var (
		noRecord     sqlite.SessionRecord
		noTransition model.WorkflowStatus
	)
	// A transition mutates, so an expired or wrong-actor session is fatal here
	// rather than a partial answer to report beside: only the read paths keep
	// the record beside CTX_SESSION_EXPIRED.
	rec, err := s.sessions.Session(ctx, req.SessionID, req.ActorID)
	if err != nil {
		return noRecord, noTransition, err
	}

	// Reachability is the store's allowedTransitions table and is never
	// re-checked here; each case is only the guard that table does not carry.
	switch req.Target {
	case model.StateVerifyOpen:
		err = s.requireResolvedScope(ctx, rec)
	case model.StateConsolidateOpen:
		err = s.requireConsolidationReady(ctx, rec)
	case model.StateComplete:
		err = s.sealCapsule(ctx, rec)
	case model.StateClosed:
		// Closing carries no service guard: it is always available from an open
		// state and never available from complete or closed, both of which the
		// store's table already answers.
	}
	if err != nil {
		return noRecord, noTransition, err
	}

	applied, err := s.sessions.AdvanceSession(ctx, req)
	if err != nil {
		return noRecord, noTransition, err
	}
	return rec, applied, nil
}

// requireResolvedScope is the sweep_open -> verify_open guard: the session's
// manifest must have resolved at least one seed, because a verify phase over an
// empty scope has nothing to read and could never be complete.
//
// An incomplete discovery may still enter verify -- reading is allowed on a
// manifest whose ScopeComplete is false; it simply can never grant readiness,
// which is the readiness gate's answer and not this guard's.
func (s *Service) requireResolvedScope(ctx context.Context, rec sqlite.SessionRecord) error {
	m, err := s.sessions.Manifest(ctx, rec.ManifestID)
	if err != nil {
		return err
	}
	if m.EntryCount <= 0 {
		return typedErrf(model.CodeScopeIncomplete,
			"this session's manifest resolved no seed, so there is nothing to verify")
	}
	return nil
}

// requireConsolidationReady is the verify_open -> consolidate_open guard: every
// required file fully served for this actor at these pinned hashes, plus a
// current-scope review -- or recorded waivers together with the user-level
// context.allow_exploratory_waiver_consolidation.
//
// The waiver route is the whole alternative, review included: it is the
// exploratory path, and what keeps it honest is not a review but the readiness
// gate, which holds StrictGateSatisfied false for as long as any waiver exists
// and leaves every unresolved item visible.
func (s *Service) requireConsolidationReady(ctx context.Context, rec sqlite.SessionRecord) error {
	m, err := s.sessions.Manifest(ctx, rec.ManifestID)
	if err != nil {
		return err
	}
	g, err := s.readiness(ctx, rec, m)
	if err != nil {
		return err
	}
	// FullyRead, not Served: Section 17.1's coverage completeness asks whether
	// every required file was fully read, waived or not, and Served is the
	// reported column that deliberately drops a waived file. A session whose
	// waived files were all read is complete and needs no flag, so comparing
	// Served would send the operator back to read files they had already read.
	//
	// Adding the waiver count back instead -- g.Served+g.Waived -- is the
	// mirror-image mistake and the worse one: a waived and UNREAD required file
	// contributes to that sum, so a session whose entire shortfall is waived
	// never trips the guard, the branch below is never reached, and the
	// default-false user flag ruling Q12 puts in front of the exploratory route
	// is bypassed. The shortfall is a read fact; the flag gates the exception.
	if g.FullyRead < g.Required {
		if g.Waived > 0 && s.limits.AllowExploratoryWaiverConsolidation {
			return nil
		}
		return typedErrf(model.CodeCoverageIncomplete,
			"%d of %d required files are fully read for this actor; read the rest, or waive them and enable context.allow_exploratory_waiver_consolidation",
			g.FullyRead, g.Required)
	}
	review, err := s.currentReview(ctx, rec, m.CanonicalHash)
	if err != nil {
		return err
	}
	if review == nil {
		return typedErrf(model.CodeScopeIncomplete,
			"this session has no scope review at scope version %d; record one before consolidating", rec.ScopeVersion)
	}
	return nil
}

// sealCapsule is the consolidate_open -> complete guard: the deterministic
// capsule is persisted and its identity verified before the state moves, so a
// completed session always has the capsule that names what it read.
//
// The two writes are deliberately not one transaction (the store exposes no
// transaction helper and this package adds none): PutCapsule is write-once on
// session_id, so a crash between them is retried and never duplicated.
//
// buildCapsule does the whole seal -- hash, write-once PutCapsule and the
// CTX_VERSION_CONFLICT comparison of the returned CanonicalHash (ruling Q11).
// This guard therefore calls it and does nothing else with the result: INT
// removed a second hash-write-compare sequence here, which was idempotent but
// was a second spelling of the one sealing path.
func (s *Service) sealCapsule(ctx context.Context, rec sqlite.SessionRecord) error {
	m, err := s.sessions.Manifest(ctx, rec.ManifestID)
	if err != nil {
		return err
	}
	g, err := s.readiness(ctx, rec, m)
	if err != nil {
		return err
	}
	_, err = s.buildCapsule(ctx, rec, g)
	return err
}
