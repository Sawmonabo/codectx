package workflow

import (
	"context"
	"errors"
	"strings"

	"github.com/Sawmonabo/codectx/internal/model"
	"github.com/Sawmonabo/codectx/internal/storage/sqlite"
)

// guaranteeLimit is the Section 16.3 point-in-time warning that rides on every
// open gate (ruling Q10). Readiness is a statement about the instant it was
// evaluated and nothing more, so an orchestrator that caches it is authorizing
// writes against a snapshot that may already have moved. A silent true is the
// defect this string exists to prevent.
const guaranteeLimit = "this readiness answer is point-in-time: re-check the gate immediately " +
	"before the write phase and pair it with expected-content-hash validation at each write; " +
	"codectx claims no atomic multi-file write transaction it does not control"

// Status is the honest readiness answer for one session (Section 16.3). It is
// revalidated per request, immediately before answering, and never cached.
// Owned by L4.
func (s *Service) Status(ctx context.Context, req model.SessionRequest) (model.SessionStatus, error) {
	if err := req.Validate(); err != nil {
		return model.SessionStatus{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, s.limits.QueryTimeout)
	defer cancel()
	// Session returns a partially populated record beside CTX_SESSION_EXPIRED
	// rather than swallowing it, and describing an expired session honestly --
	// read complete for its historical snapshot, never ready -- is the whole
	// point of this call. Only a record that was never loaded is refused.
	rec, err := s.sessions.Session(ctx, req.SessionID, req.ActorID)
	if err != nil && !(expiredSession(err) && rec.ID != "") {
		return model.SessionStatus{}, err
	}
	return s.status(ctx, rec)
}

// readiness evaluates the full Section 16.3 precondition set exactly once per
// request: one CoverageSummary call, the manifest's ScopeComplete, the verify
// phase, a current-scope review, a blocking-unresolved scan, waived == 0 and
// per-request Validator revalidation of every cited source. When the gate is
// open the result carries the point-in-time guarantee limit (ruling Q10): a
// silent true is a defect. Owned by L4.
//
// This is the gate Advance and buildCapsule read; Status reads the wider
// evaluation, because the frozen gate carries no Superseded field.
func (s *Service) readiness(ctx context.Context, rec sqlite.SessionRecord, m model.ContextManifest) (gate, error) {
	e, err := s.evaluate(ctx, rec, m)
	return e.gate, err
}

// evaluation is one readiness evaluation plus the one SessionStatus field the
// frozen gate cannot carry. Superseded has no source on the Sessions surface --
// no method there reports the active generation -- so it is derived here from
// Validator.Current: a required file whose pinned content hash is no longer the
// current one means this session is reading a superseded snapshot. That is also
// precondition 7, so the two share a single walk rather than validating twice.
//
// gate is frozen in workflow.go with four booleans against SessionStatus' five,
// and a fill-in lane does not change a frozen type; this local wrapper keeps
// Superseded honest without a second Validator pass. Recorded in the L4 report
// as the one field the frozen gate should probably carry.
type evaluation struct {
	gate
	Superseded bool
}

// evaluate is the single place the Section 16.3 preconditions are decided.
//
// The order is deliberate. The counts, the manifest's own scope answer, the
// phase and state, the waiver count and the current-source walk are all
// evaluated unconditionally, because every one of them is also a reported
// SessionStatus field or operator-facing diagnostic. The current-scope review
// is asked for last and only when every other precondition already holds: it is
// a pure predicate with nothing to report but pass or fail, so querying storage
// for it while the gate is already shut buys the operator nothing.
func (s *Service) evaluate(ctx context.Context, rec sqlite.SessionRecord, m model.ContextManifest) (evaluation, error) {
	// Precondition 3's inputs, in one call. Paging Coverage for counts would
	// cost a round trip per page on a large session, which is exactly why
	// CoverageSummary exists; this package defines no second aggregate.
	required, served, waived, err := s.sessions.CoverageSummary(ctx, rec.ID, rec.ActorID)
	if err != nil && !expiredSession(err) {
		return evaluation{}, err
	}

	e := evaluation{gate: gate{
		Required:      required,
		Served:        served,
		Waived:        waived,
		ScopeComplete: m.ScopeComplete,
	}}
	// Task 16's honest weaker answer, unchanged: a waiver is counted and
	// reported but never folded in, and an incomplete manifest scope
	// disqualifies the session outright even when the counts agree.
	e.ReadComplete = m.ScopeComplete && served == required

	var shut []string

	// Precondition 1 -- the verify phase. The session must have reached verify
	// and must still be open: sweep_open has not reviewed anything, and
	// complete and closed have no write phase left to gate. consolidate_open
	// counts because the capsule records StrictGateSatisfied at seal time, and
	// a gate that only ever answered in verify_open would stamp every capsule
	// false regardless of what the session actually achieved.
	inVerify := (rec.Phase == model.PhaseVerify || rec.Phase == model.PhaseConsolidate) &&
		(rec.State == model.StateVerifyOpen || rec.State == model.StateConsolidateOpen)
	if !inVerify {
		shut = append(shut, "the session is not in an open verify or consolidate state")
	}

	// Precondition 1, second half -- the session lease is still live. Expiry is
	// lazy: the store closes nothing until something reads, so a lapsed session
	// still carries State: verify_open, and the CTX_SESSION_EXPIRED that comes
	// back beside the record is deliberately swallowed here so status stays
	// honest. Nothing above would notice, and SessionStatus.Validate has no
	// expiry clause, so without this an expired lease would open the gate.
	if !rec.ExpiresAt.IsZero() && !rec.ExpiresAt.After(s.now()) {
		shut = append(shut, "the session lease has expired")
	}

	// Precondition 2 -- resolved and complete scope. Task 15 compiles an
	// ambiguous or empty scope into a manifest with ScopeComplete=false and no
	// required_full entries, so this is what stops a session that resolved
	// nothing from reading as fully read.
	if !m.ScopeComplete {
		shut = append(shut, "the manifest scope is incomplete or unresolved")
	}

	// Precondition 3 -- read completeness for this actor, this session, these
	// pinned hashes.
	if !e.ReadComplete {
		shut = append(shut, "required files are not all fully served to this actor")
	}

	// Precondition 6 -- no required-file waiver. A waiver is an honest
	// admission that a required file was not read; SessionStatus.Validate
	// refuses the combination outright, so a bug here fails loudly.
	if waived > 0 {
		shut = append(shut, "the session carries a required-file waiver")
	}

	// Precondition 7 -- current-source validation, per request, immediately
	// before answering. The same walk answers Superseded.
	current, err := s.sourcesCurrent(ctx, rec, required)
	if err != nil {
		return evaluation{}, err
	}
	e.Superseded = !current
	if !current {
		shut = append(shut, "a required file's pinned content hash is no longer current")
	}

	// Precondition 5, first half -- no unresolved dependency at the current
	// scope version. The list is bounded by one page; the gate only needs to
	// know whether any exists, and the ids are there so an operator can see
	// which ones to resolve.
	blocking, err := s.sessions.Observations(ctx, rec.ID, rec.ActorID, model.ObservationUnresolved,
		rec.ScopeVersion, "", s.limits.MaxPageItems)
	if err != nil && !expiredSession(err) {
		return evaluation{}, err
	}
	for _, o := range blocking {
		e.Blocking = append(e.Blocking, o.ID)
	}
	if len(e.Blocking) > 0 {
		shut = append(shut, "unresolved dependencies are recorded at the current scope version")
	}

	if len(shut) > 0 {
		e.Reason = strings.Join(shut, "; ")
		return e, nil
	}

	// Precondition 4 -- a current-scope actor review, and precondition 5's
	// second half, a review that blocks nothing. An include bumps ScopeVersion
	// and invalidates the old review, which stays for audit.
	review, err := s.currentReview(ctx, rec, m.CanonicalHash)
	if err != nil {
		return evaluation{}, err
	}
	if review == nil || review.Review == nil {
		e.Reason = "no scope review covers this manifest at the current scope version"
		return e, nil
	}
	for _, entry := range review.Review.Entries {
		if entry.Blocking {
			e.Reason = "the current scope review records blocking uncertainty in " + string(entry.Category)
			return e, nil
		}
	}

	// All seven hold. Superseded is false here by construction -- it is the
	// negation of precondition 7 -- so ReadyForImplementation and
	// StrictGateSatisfied agree, and both carry the guarantee limit rather than
	// a silent true.
	e.Strict = true
	e.Ready = e.Strict && !e.Superseded
	e.Reason = guaranteeLimit
	s.log.Warn("strict readiness gate is open",
		"session_id", string(rec.ID), "scope_version", rec.ScopeVersion, "guarantee", guaranteeLimit)
	return e, nil
}

// sourcesCurrent revalidates every required_full file's pinned content hash
// against the current snapshot, per request. A boolean cached earlier is not
// authorization, so nothing here is memoised.
//
// It walks Coverage by keyset for the pinned hashes -- the counts already came
// from CoverageSummary -- with each page bounded by the configured page limit,
// and stops at the first stale file, because one stale file already decides
// both this answer and Superseded.
func (s *Service) sourcesCurrent(ctx context.Context, rec sqlite.SessionRecord, required int64) (bool, error) {
	limit := s.limits.MaxPageItems
	var seen int64
	after := model.FileID("")
	for {
		page, err := s.sessions.Coverage(ctx, rec.ID, rec.ActorID, after, limit)
		if err != nil && !expiredSession(err) {
			return false, err
		}
		if len(page) == 0 {
			return true, nil
		}
		for _, fc := range page {
			if fc.Requirement != model.RequirementFull {
				continue
			}
			seen++
			ok, err := s.validate.Current(ctx, fc.FileID, fc.ContentHash)
			if err != nil {
				return false, err
			}
			if !ok {
				return false, nil
			}
		}
		// The keyset strictly advances, so this terminates on any finite
		// session; the two early exits only save round trips.
		after = page[len(page)-1].FileID
		if len(page) < limit || seen >= required {
			return true, nil
		}
	}
}

// status projects one readiness evaluation onto all nineteen SessionStatus
// fields, Superseded included. SessionStatus.Validate refuses the dishonest
// combinations, so a wrong evaluator fails loudly rather than silently.
// Owned by L4.
func (s *Service) status(ctx context.Context, rec sqlite.SessionRecord) (model.SessionStatus, error) {
	if rec.ID == "" {
		return model.SessionStatus{}, typedErrf(model.CodeInternal,
			"workflow status was asked to describe a session that was never loaded")
	}
	m, err := s.sessions.Manifest(ctx, rec.ManifestID)
	if err != nil {
		return model.SessionStatus{}, err
	}
	e, err := s.evaluate(ctx, rec, m)
	if err != nil {
		return model.SessionStatus{}, err
	}
	status := model.SessionStatus{
		SessionID:    rec.ID,
		ActorID:      rec.ActorID,
		Binding:      rec.Binding,
		ManifestID:   rec.ManifestID,
		Phase:        rec.Phase,
		State:        rec.State,
		StateVersion: rec.StateVersion,
		ScopeVersion: rec.ScopeVersion,
		// The manifest's own per-capability completeness is the only verified
		// source for this field; nothing on the Sessions surface reports a
		// second one, and inventing one would be a claim nothing checked.
		Completeness:            m.Completeness,
		ScopeComplete:           e.ScopeComplete,
		ReadCompleteForSnapshot: e.ReadComplete,
		ReadyForImplementation:  e.Ready,
		StrictGateSatisfied:     e.Strict,
		Superseded:              e.Superseded,
		RequiredFiles:           e.Required,
		FullyServedFiles:        e.Served,
		WaivedFiles:             e.Waived,
		CreatedAt:               rec.CreatedAt,
		ExpiresAt:               rec.ExpiresAt,
	}
	if err := status.Validate(); err != nil {
		return model.SessionStatus{}, err
	}
	return status, nil
}

// expiredSession reports whether err is the CTX_SESSION_EXPIRED that Session,
// Coverage, Observations, Capsule and CoverageSummary return beside a partially
// populated value. It is errors.As rather than a type assertion because
// model.Canceled returns an errors.Join, which no assertion unwraps.
func expiredSession(err error) bool {
	var typed *model.Error
	return errors.As(err, &typed) && typed.Code == model.CodeSessionExpired
}
