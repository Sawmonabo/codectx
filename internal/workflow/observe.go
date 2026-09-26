package workflow

import (
	"context"
	"sort"
	"strconv"

	"github.com/Sawmonabo/codectx/internal/model"
	"github.com/Sawmonabo/codectx/internal/storage/sqlite"
)

// Record persists one immutable actor observation. It checks the request, the
// actor, the expected scope version and the per-kind reference rules, proves
// every citation was already confirmed served to this actor, validates a scope
// review against the eight required categories, derives the id through
// model.NewObservationID -- the one Section 17.2 preimage -- and writes through
// PutObservation. Owned by L3.
//
// The per-kind reference rules (at least one relation for accept/reject, at
// least two distinct claims for a contradiction, a specific node, relation or
// source plus a reason for an unresolved item, and all eight review categories
// answered exactly once) are model.ObservationRequest.Validate's, and
// distinctness is ClaimReference.canonical()'s. Restating either here would be
// a second spelling of a rule that already has one.
//
// A mutation is refused outright on any Session error, expiry included: the
// honest partial record exists so a *read* can stay truthful about an expired
// session, and it is not licence to write to one.
func (s *Service) Record(ctx context.Context, req model.ObservationRequest) (model.Observation, model.SessionStatus, error) {
	if err := req.Validate(); err != nil {
		return model.Observation{}, model.SessionStatus{}, err
	}
	// The model carries no count ceiling of its own: this is the only ceiling
	// on an observation's references, it is the operator's, and it is unlimited
	// by default.
	if err := s.checkReferenceCount(len(req.References), "observation"); err != nil {
		return model.Observation{}, model.SessionStatus{}, err
	}
	rec, err := s.sessions.Session(ctx, req.SessionID, req.ActorID)
	if err != nil {
		return model.Observation{}, model.SessionStatus{}, err
	}
	// From complete and closed there is no edge and no new mutation (Section
	// 17.1); the store refuses a closed session and says nothing about a
	// completed one, so the completed case is answered here.
	if rec.State == model.StateComplete || rec.State == model.StateClosed {
		return model.Observation{}, model.SessionStatus{}, typedErrf(model.CodeVersionConflict,
			"session %s is %s and records no further observation", rec.ID, rec.State)
	}
	// PutObservation re-checks this inside its own statement and answers the
	// same code; checking here is what lets the citation and review guards
	// below run against a scope version the request still agrees with, and it
	// deliberately does not invent a second code for the same condition.
	if req.ExpectedScope != rec.ScopeVersion {
		return model.Observation{}, model.SessionStatus{}, typedErrf(model.CodeScopeChanged,
			"the observation expects scope version %d; this session is at %d",
			req.ExpectedScope, rec.ScopeVersion)
	}
	if err := s.citationsServed(ctx, rec, req.References); err != nil {
		return model.Observation{}, model.SessionStatus{}, err
	}
	if req.Kind == model.ObservationScopeReview {
		if err := s.reviewSupported(ctx, rec, req.Review); err != nil {
			return model.Observation{}, model.SessionStatus{}, err
		}
	}

	obs := model.Observation{
		// ScopeVersion is the request's expected version, not the record's:
		// that is the field NewObservationID hashed and Observation.Validate
		// reconstructs, so taking it from anywhere else forges an identity.
		ID:           model.NewObservationID(req),
		SessionID:    req.SessionID,
		ActorID:      req.ActorID,
		ScopeVersion: req.ExpectedScope,
		Kind:         req.Kind,
		References:   req.References,
		Review:       req.Review,
		Note:         req.Note,
		CreatedAt:    s.now().UTC(),
	}
	if err := obs.Validate(); err != nil {
		return model.Observation{}, model.SessionStatus{}, err
	}
	if err := s.sessions.PutObservation(ctx, obs); err != nil {
		return model.Observation{}, model.SessionStatus{}, err
	}
	// The write landed; only the status projection can still fail, so the
	// stored observation is returned beside that failure rather than discarded.
	status, err := s.status(ctx, rec)
	if err != nil {
		return obs, model.SessionStatus{}, err
	}
	return obs, status, nil
}

// Waive records one required-file waiver and reports the resulting status. A
// waiver is an honest admission that a required file was not read: it never
// grants strict readiness, and SessionStatus.Validate refuses the combination
// outright. Owned by L3 (ownership unassigned in the lane plan -- see report).
//
// context.allow_exploratory_waiver_consolidation is not consulted here: it
// gates the verify_open -> consolidate_open transition, not the recording of
// the exception, and a waiver that cannot be recorded cannot be audited.
func (s *Service) Waive(ctx context.Context, req model.WaiverRequest) (model.WaiverRecord, model.SessionStatus, error) {
	if err := req.Validate(); err != nil {
		return model.WaiverRecord{}, model.SessionStatus{}, err
	}
	rec, err := s.sessions.Session(ctx, req.SessionID, req.ActorID)
	if err != nil {
		return model.WaiverRecord{}, model.SessionStatus{}, err
	}
	if rec.State == model.StateComplete || rec.State == model.StateClosed {
		return model.WaiverRecord{}, model.SessionStatus{}, typedErrf(model.CodeVersionConflict,
			"session %s is %s and records no further waiver", rec.ID, rec.State)
	}
	waiver, err := s.sessions.Waive(ctx, req)
	if err != nil {
		return model.WaiverRecord{}, model.SessionStatus{}, err
	}
	status, err := s.status(ctx, rec)
	if err != nil {
		return waiver, model.SessionStatus{}, err
	}
	return waiver, status, nil
}

// currentReview returns this actor's scope review for the session's current
// scope version and manifest hash, or nil when none is current. A review from a
// superseded scope version is not current and does not satisfy the gate.
// Owned by L3.
//
// Its consumer is L4's readiness (digest Section 7, precondition 4), which is
// the only place a current review is an input; Record does not call it, because
// a resubmitted observation is already idempotent on the content-derived id and
// a second review at the same scope version is a new record, not a conflict.
func (s *Service) currentReview(ctx context.Context, rec sqlite.SessionRecord, manifestHash string) (*model.Observation, error) {
	var newest *model.Observation
	var after model.ObservationID
	for {
		page, err := s.sessions.Observations(ctx, rec.ID, rec.ActorID,
			model.ObservationScopeReview, rec.ScopeVersion, after, s.limits.MaxPageItems)
		if err != nil {
			return nil, err
		}
		for i := range page {
			o := page[i]
			if o.Review == nil || o.Review.ManifestHash != manifestHash ||
				o.Review.ScopeVersion != rec.ScopeVersion {
				continue
			}
			// Several reviews may stand at one scope version; the latest is the
			// current one, and the content-derived id breaks a timestamp tie so
			// the answer does not depend on page order.
			if newest == nil || o.CreatedAt.After(newest.CreatedAt) ||
				(o.CreatedAt.Equal(newest.CreatedAt) && o.ID > newest.ID) {
				current := o
				newest = &current
			}
		}
		if len(page) < s.limits.MaxPageItems {
			return newest, nil
		}
		after = page[len(page)-1].ID
	}
}

// citationsServed proves every source citation on the given references names an
// interval already confirmed served to this actor at that content hash, through
// Sessions.RangeConfirmed. This is what stops a review from marking a file read
// without the coverage to back it. Owned by L3.
//
// Containment is answered in SQL over served_ranges: no interval list crosses
// into Go and this package merges nothing. An interval spanning the gap between
// two confirmed intervals is confirmed by neither.
func (s *Service) citationsServed(ctx context.Context, rec sqlite.SessionRecord, refs []model.ClaimReference) error {
	for _, ref := range refs {
		if ref.Source == nil {
			continue
		}
		src := *ref.Source
		ok, err := s.sessions.RangeConfirmed(ctx, rec.ID, rec.ActorID, src.FileID, src.ContentHash, src.Bytes)
		if err != nil {
			return err
		}
		if !ok {
			return typedErrf(model.CodeCoverageIncomplete,
				"the citation of bytes [%d,%d) of file %s was never confirmed served to this actor at that content hash",
				src.Bytes.Start, src.Bytes.End, src.FileID)
		}
	}
	return nil
}

// reviewSupported validates the structured attestation beyond its shape, which
// model.ScopeReview.Validate already enforces: the review must bind this
// session's current scope version and the canonical hash of the manifest that
// scope came from, every citation it makes must be confirmed served, and the
// complete_files_read attestation must be backed by full coverage rather than
// by its own note.
func (s *Service) reviewSupported(ctx context.Context, rec sqlite.SessionRecord, review *model.ScopeReview) error {
	if review == nil {
		return typedErrf(model.CodeInternal, "a scope review reached validation without an attestation")
	}
	manifest, err := s.sessions.Manifest(ctx, rec.ManifestID)
	if err != nil {
		return err
	}
	// The only legitimate way to hold a review bound to another manifest hash
	// or scope version is that the scope moved under it -- an include bumps
	// both -- so the scope-changed code is the honest answer, not an argument
	// error about a field the actor copied correctly a moment earlier.
	if review.ScopeVersion != rec.ScopeVersion {
		return typedErrf(model.CodeScopeChanged,
			"the scope review binds scope version %d; this session is at %d",
			review.ScopeVersion, rec.ScopeVersion)
	}
	if review.ManifestHash != manifest.CanonicalHash {
		return typedErrf(model.CodeScopeChanged,
			"the scope review binds a manifest this session's scope no longer comes from")
	}
	// A review carries its references inside its entries rather than on the
	// request, so the aggregate across the eight categories is what the
	// configured ceiling has to hold. Neither the entries nor this total are
	// bounded by the model; unlimited -- the default -- admits all eight
	// categories however much of a large scope they cite.
	total := 0
	for _, entry := range review.Entries {
		total += len(entry.References)
	}
	if err := s.checkReferenceCount(total, "scope review"); err != nil {
		return err
	}

	var read []model.FileID
	for _, entry := range review.Entries {
		if err := s.citationsServed(ctx, rec, entry.References); err != nil {
			return err
		}
		if entry.Category != model.ReviewCompleteFilesRead {
			continue
		}
		for _, ref := range entry.References {
			if ref.Source != nil {
				read = append(read, ref.Source.FileID)
			}
		}
	}
	return s.filesFullyRead(ctx, rec, read)
}

// filesFullyRead refuses a complete_files_read attestation over any file this
// actor has not fully read. A confirmed citation proves the cited bytes were
// served; it does not prove the file was read end to end, and the claim being
// made here is the stronger one.
//
// Per-file coverage state has no other source on the frozen Sessions surface --
// CoverageSummary answers counts and cannot say which file is short -- so this
// pages Coverage, bounded by the configured page size and stopped as soon as
// every cited file is resolved or the keyset has run past the last of them. It
// is not paging for counts, which is what CoverageSummary exists for and what
// this package never does.
func (s *Service) filesFullyRead(ctx context.Context, rec sqlite.SessionRecord, ids []model.FileID) error {
	if len(ids) == 0 {
		return nil
	}
	wanted := make(map[model.FileID]bool, len(ids))
	for _, id := range ids {
		wanted[id] = true
	}
	distinct := make([]model.FileID, 0, len(wanted))
	for id := range wanted {
		distinct = append(distinct, id)
	}
	sort.Slice(distinct, func(i, j int) bool { return distinct[i] < distinct[j] })
	last := distinct[len(distinct)-1]

	found := make(map[model.FileID]model.FileCoverage, len(distinct))
	var after model.FileID
	for len(found) < len(distinct) {
		page, err := s.sessions.Coverage(ctx, rec.ID, rec.ActorID, after, s.limits.MaxPageItems)
		if err != nil {
			return err
		}
		if len(page) == 0 {
			break
		}
		for _, c := range page {
			if wanted[c.FileID] {
				found[c.FileID] = c
			}
		}
		after = page[len(page)-1].FileID
		// Coverage is keyset-ordered by file id, so once the page has run past
		// the largest cited id there is nothing left to find.
		if len(page) < s.limits.MaxPageItems || after >= last {
			break
		}
	}

	for _, id := range distinct {
		c, ok := found[id]
		if !ok {
			return typedErrf(model.CodeArgumentInvalid,
				"the review attests to reading file %s, which this session does not pin", id)
		}
		if c.State != model.CoverageFullServed {
			return typedErrf(model.CodeCoverageIncomplete,
				"the review attests to reading file %s, which is %s with %d of %d bytes confirmed",
				id, c.State, c.ConfirmedBytes, c.Size)
		}
	}
	return nil
}

// checkReferenceCount applies workflow.max_observation_references as a CALLER
// CEILING to one attestation's reference count: the observation's own list, or
// a scope review's total across its eight categories.
//
// The key is unlimited by default, and this never refuses a count for being
// large: a session over a big scope cites what it read, and the model's own
// per-list ceiling is the only structural bound. A caller that did set a
// ceiling is told the ceiling it set and the count the attestation reached, so
// raising it is a decision with both numbers in hand rather than a retry
// against an unnamed bound. Nothing is ever trimmed to fit: dropping a citation
// would falsify the attestation it belongs to.
func (s *Service) checkReferenceCount(n int, what string) error {
	if !s.limits.MaxObservationReferences.Exceeded(int64(n)) {
		return nil
	}
	return (&model.Error{Code: model.CodeResourceLimit,
		Message: "the " + what + " carries more references than the caller's workflow.max_observation_references ceiling",
		Details: map[string]string{
			"limit":       "workflow.max_observation_references",
			"limit_value": s.limits.MaxObservationReferences.String(),
			"references":  strconv.Itoa(n),
		}}).
		WithRemediation("record the attestation in smaller observations, or raise workflow.max_observation_references (0 or \"unlimited\" removes the ceiling)")
}
