package workflow

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"strconv"

	"github.com/Sawmonabo/codectx/internal/model"
	"github.com/Sawmonabo/codectx/internal/storage/sqlite"
)

// Capsule pages one projection of a session's sealed capsule (Section 17.3).
//
// It reads the stored record and never seals one: a capsule exists only because
// a completion produced it, so paging a session that sealed none reports the
// store's own "no capsule" rejection rather than manufacturing an empty page
// that reads as "this session recorded nothing".
func (s *Service) Capsule(ctx context.Context, req model.CapsuleRequest) (model.CapsulePage, error) {
	if err := req.Validate(); err != nil {
		return model.CapsulePage{}, err
	}
	c, err := s.sessions.Capsule(ctx, req.SessionID, req.ActorID)
	if err != nil {
		return model.CapsulePage{}, err
	}
	page := model.CapsulePage{
		Meta:          model.QueryMeta{Binding: c.Binding, Completeness: c.Completeness},
		SessionID:     c.SessionID,
		ManifestHash:  c.ManifestHash,
		CanonicalHash: c.CanonicalHash,
		View:          req.View,
	}
	limit := s.capsulePageLimit(req.Page.Limit)
	var next string
	switch req.View {
	case model.CapsuleViewAcceptedFacts:
		page.AcceptedFacts, next, err = pageCapsuleList(c.AcceptedFacts, capsuleFactKey, req.Page.Cursor, limit)
	case model.CapsuleViewRejectedFacts:
		page.RejectedFacts, next, err = pageCapsuleList(c.RejectedFacts, capsuleFactKey, req.Page.Cursor, limit)
	case model.CapsuleViewContradictions:
		page.Contradictions, next, err = pageCapsuleList(c.Contradictions, capsuleObservationKey, req.Page.Cursor, limit)
	case model.CapsuleViewUnresolved:
		page.Unresolved, next, err = pageCapsuleList(c.Unresolved, capsuleObservationKey, req.Page.Cursor, limit)
	case model.CapsuleViewCoverage:
		page.Coverage, next, err = pageCapsuleList(c.Coverage, capsuleCoverageKey, req.Page.Cursor, limit)
	case model.CapsuleViewWaivers:
		page.Waivers, next, err = pageCapsuleList(c.Waivers, capsuleWaiverKey, req.Page.Cursor, limit)
	default:
		// CapsuleRequest.Validate already closed the view set; reaching here
		// would mean a new view was added to the model without a projection,
		// which must fail rather than page an empty list.
		return model.CapsulePage{}, typedErrf(model.CodeInternal,
			"capsule view %q has no projection", req.View)
	}
	if err != nil {
		return model.CapsulePage{}, err
	}
	page.Meta.NextCursor = next
	if err := page.Validate(); err != nil {
		return model.CapsulePage{}, err
	}
	return page, nil
}

// Export returns the whole sealed capsule for one session.
//
// The bound it checks is the same one the capsule was written under, so an
// export that cannot fit says so explicitly and names the paged read instead of
// returning a truncated record that would look complete.
func (s *Service) Export(ctx context.Context, req model.SessionRequest) (model.Capsule, error) {
	if err := req.Validate(); err != nil {
		return model.Capsule{}, err
	}
	c, err := s.sessions.Capsule(ctx, req.SessionID, req.ActorID)
	if err != nil {
		return model.Capsule{}, err
	}
	if err := s.checkCapsuleBytes(c); err != nil {
		return model.Capsule{}, err
	}
	return c, nil
}

// buildCapsule seals the Section 17.3 completion record: it assembles the
// capsule from stored observations, coverage and the manifest only -- never
// from a live recomputation, which is what makes the identity reproducible --
// then hashes it, writes it through the write-once PutCapsule and compares the
// returned CanonicalHash with the computed one, reporting CTX_VERSION_CONFLICT
// on a mismatch rather than recomputing and overwriting.
//
// It returns the STORED capsule, so a second completion answers with the first
// capsule and its original timestamp. Sealing before the advance to complete is
// deliberate and is not atomic with it (ruling Q4): the write is idempotent and
// write-once, so a crash between the two is retried, never duplicated.
//
// L0's declaration comment left the hash, the write and the comparison to the
// caller. Doing them here instead is what makes CTX_VERSION_CONFLICT on a
// determinism mismatch exist exactly once; a caller that repeats the sequence
// gets the same stored capsule back and the same matching hash.
func (s *Service) buildCapsule(ctx context.Context, rec sqlite.SessionRecord, g gate) (model.Capsule, error) {
	if rec.State != model.StateConsolidateOpen {
		return model.Capsule{}, errors.Join(
			typedErrf(model.CodeVersionConflict,
				"this session is %s; a capsule is produced only while consolidate is open", rec.State),
			errNotConsolidating)
	}
	manifest, err := s.sessions.Manifest(ctx, rec.ManifestID)
	if err != nil {
		return model.Capsule{}, err
	}
	scope, err := s.capsuleScope(ctx, rec)
	if err != nil {
		return model.Capsule{}, err
	}
	coverage, err := s.capsuleCoverage(ctx, rec)
	if err != nil {
		return model.Capsule{}, err
	}
	waivers, err := s.capsuleWaivers(ctx, rec)
	if err != nil {
		return model.Capsule{}, err
	}
	accepted, err := s.capsuleFacts(ctx, rec, model.ObservationAcceptFact)
	if err != nil {
		return model.Capsule{}, err
	}
	rejected, err := s.capsuleFacts(ctx, rec, model.ObservationRejectFact)
	if err != nil {
		return model.Capsule{}, err
	}
	contradictions, err := s.capsuleObservations(ctx, rec, model.ObservationContradiction)
	if err != nil {
		return model.Capsule{}, err
	}
	unresolved, err := s.capsuleObservations(ctx, rec, model.ObservationUnresolved)
	if err != nil {
		return model.Capsule{}, err
	}
	reviews, err := s.capsuleReviewIDs(ctx, rec)
	if err != nil {
		return model.Capsule{}, err
	}

	c := model.Capsule{
		SessionID:      rec.ID,
		ActorID:        rec.ActorID,
		Binding:        rec.Binding,
		ManifestHash:   manifest.CanonicalHash,
		ScopeVersion:   rec.ScopeVersion,
		Scope:          scope,
		AcceptedFacts:  accepted,
		RejectedFacts:  rejected,
		Contradictions: contradictions,
		Unresolved:     unresolved,
		ScopeReviewIDs: reviews,
		Coverage:       coverage,
		Waivers:        waivers,
		Completeness:   manifest.Completeness,
		// Straight from the one readiness evaluation. Capsule.Validate refuses
		// a satisfied strict gate beside recorded waivers, so a wrong gate
		// fails loudly here instead of being defensively corrected.
		StrictGateSatisfied: g.Strict,
		CreatedAt:           s.now().UTC(),
	}
	c.CanonicalHash = canonicalCapsuleHash(c)
	if err := s.checkCapsuleBytes(c); err != nil {
		return model.Capsule{}, err
	}
	if err := c.Validate(); err != nil {
		return model.Capsule{}, err
	}
	stored, err := s.sessions.PutCapsule(ctx, c)
	if err != nil {
		return model.Capsule{}, err
	}
	if stored.CanonicalHash != c.CanonicalHash {
		// PutCapsule does no hash comparison, so this is the determinism alarm
		// PutManifest raises and it does not. The stored capsule stands: a
		// completion that would change a sealed identity is refused, never
		// written over.
		return model.Capsule{}, typedErrf(model.CodeVersionConflict,
			"this session already sealed a capsule with a different canonical identity").
			WithRemediation("export the sealed capsule; a sealed identity is never rewritten")
	}
	return stored, nil
}

// capsuleScope collects the scoped node ids from the pinned manifest, sorted and
// deduplicated so two builds of the same session produce one ordering. Manifest
// entries that name no node (a file pulled in whole) contribute nothing.
//
// Scope is the PINNED MANIFEST's scope and Coverage is the SESSION's union, and
// after an Include the two deliberately differ: a recompiled manifest covers the
// included seeds only, while session_files accumulates across every include
// (Store.IncludeManifest inserts, never replaces). So a sealed capsule can list
// coverage for a file whose nodes are absent from Scope; that is the accepted
// reading of Section 17.3, not a drift between two spellings of one set.
func (s *Service) capsuleScope(ctx context.Context, rec sqlite.SessionRecord) ([]model.NodeID, error) {
	seen := make(map[model.NodeID]bool)
	var out []model.NodeID
	after := -1
	for {
		entries, err := s.sessions.ManifestEntries(ctx, rec.ManifestID, after, s.limits.MaxPageItems)
		if err != nil {
			return nil, err
		}
		if len(entries) == 0 {
			break
		}
		for _, e := range entries {
			after = e.Ordinal
			if e.NodeID == "" || seen[e.NodeID] {
				continue
			}
			if len(out) >= model.MaxRecordsPerResult {
				return nil, capsuleTooLarge("scope", model.MaxRecordsPerResult)
			}
			seen[e.NodeID] = true
			out = append(out, e.NodeID)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out, nil
}

// capsuleCoverage pages the session's whole coverage set in file-id order, which
// is the store's own keyset order, so the assembled list is already canonical.
func (s *Service) capsuleCoverage(ctx context.Context, rec sqlite.SessionRecord) ([]model.FileCoverage, error) {
	var out []model.FileCoverage
	var after model.FileID
	for {
		rows, err := s.sessions.Coverage(ctx, rec.ID, rec.ActorID, after, s.limits.MaxPageItems)
		if err != nil {
			return nil, err
		}
		if len(rows) == 0 {
			break
		}
		if len(out)+len(rows) > model.MaxCoverageFilesPerCapsule {
			return nil, capsuleTooLarge("coverage", model.MaxCoverageFilesPerCapsule)
		}
		out = append(out, rows...)
		after = rows[len(rows)-1].FileID
	}
	return out, nil
}

// capsuleWaivers reads the session's recorded exceptions. A capsule that
// listed no waivers while its coverage shows waived files would be a dishonest
// durable artifact, so the reasons come from the store's own append-only
// coverage_waivers rows -- never from the Waive request that echoed them -- and
// arrive in the store's file-id order, which is the capsule's canonical order.
//
// Store.Waivers pages internally and returns the whole list, so the bound is
// applied on consumption: a waiver exists per required file at most, the same
// population capsuleCoverage caps, and a capsule too large to carry is the
// CTX_RESOURCE_LIMIT refusal every other projection here already answers rather
// than an unbounded response Section 6 forbids.
func (s *Service) capsuleWaivers(ctx context.Context, rec sqlite.SessionRecord) ([]model.WaiverRecord, error) {
	out, err := s.sessions.Waivers(ctx, rec.ID, rec.ActorID)
	if err != nil {
		return nil, err
	}
	if len(out) > model.MaxCoverageFilesPerCapsule {
		return nil, capsuleTooLarge("waivers", model.MaxCoverageFilesPerCapsule)
	}
	return out, nil
}

// capsuleFacts projects one fact-bearing observation kind. Section 17.3 records
// a fact as its relation, the evidence behind it and the observation that
// accepted or rejected it, so one observation naming several relations
// contributes one reference each.
func (s *Service) capsuleFacts(ctx context.Context, rec sqlite.SessionRecord,
	kind model.ObservationKind) ([]model.FactReference, error) {
	obs, err := s.currentObservations(ctx, rec, kind)
	if err != nil {
		return nil, err
	}
	var out []model.FactReference
	for _, o := range obs {
		for _, ref := range o.References {
			if ref.RelationID == "" {
				continue
			}
			out = append(out, model.FactReference{RelationID: ref.RelationID, ObservationID: o.ID})
		}
	}
	// Observations already arrive in observation-id order; sorting on the pair
	// makes the order independent of how one observation listed its relations.
	sort.Slice(out, func(i, j int) bool { return capsuleFactKey(out[i]) < capsuleFactKey(out[j]) })
	return out, nil
}

// capsuleObservations projects one observation kind without re-embedding the
// whole record: the id, its canonical references and the actor's own note.
func (s *Service) capsuleObservations(ctx context.Context, rec sqlite.SessionRecord,
	kind model.ObservationKind) ([]model.ObservationReference, error) {
	obs, err := s.currentObservations(ctx, rec, kind)
	if err != nil {
		return nil, err
	}
	out := make([]model.ObservationReference, 0, len(obs))
	for _, o := range obs {
		out = append(out, model.ObservationReference{
			ObservationID: o.ID, Kind: o.Kind, References: o.References, Note: o.Note,
		})
	}
	return out, nil
}

// capsuleReviewIDs names the current-scope scope reviews. The review rows stay
// in session_observations for audit; the capsule records which ones bound this
// scope version rather than copying the attestation.
func (s *Service) capsuleReviewIDs(ctx context.Context, rec sqlite.SessionRecord) ([]string, error) {
	obs, err := s.currentObservations(ctx, rec, model.ObservationScopeReview)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(obs))
	for _, o := range obs {
		out = append(out, string(o.ID))
	}
	return out, nil
}

// currentObservations pages one kind at the session's current scope version by
// keyset on the observation id, which is the order the store returns and the
// order the capsule stores. An include bumps the scope version, so observations
// bound to a superseded scope are not in the capsule -- they stay stored.
func (s *Service) currentObservations(ctx context.Context, rec sqlite.SessionRecord,
	kind model.ObservationKind) ([]model.Observation, error) {
	var out []model.Observation
	var after model.ObservationID
	for {
		page, err := s.sessions.Observations(ctx, rec.ID, rec.ActorID, kind, rec.ScopeVersion, after, s.limits.MaxPageItems)
		if err != nil {
			return nil, err
		}
		if len(page) == 0 {
			break
		}
		if len(out)+len(page) > model.MaxRecordsPerResult {
			return nil, capsuleTooLarge(string(kind), model.MaxRecordsPerResult)
		}
		out = append(out, page...)
		after = page[len(page)-1].ID
	}
	return out, nil
}

// checkCapsuleBytes enforces context.max_capsule_bytes against the serialized
// record, the same measure and the same rejection the store applies, so an
// over-budget capsule fails before the write instead of at it.
func (s *Service) checkCapsuleBytes(c model.Capsule) error {
	payload, err := json.Marshal(c)
	if err != nil {
		return typedErrf(model.CodeInternal, "the capsule could not be serialized: %v", err)
	}
	if int64(len(payload)) > s.limits.MaxCapsuleBytes {
		return (&model.Error{Code: model.CodeResourceLimit, Message: "capsule exceeds the stored JSON bound"}).
			WithRemediation("export the capsule paginated; raise context.max_capsule_bytes only with sufficient reservations")
	}
	return nil
}

// capsuleTooLarge is the explicit refusal of a capsule list that would exceed a
// structural bound. Section 17.3 forbids silent omission: a capsule is a durable
// artifact a later session replays, so a truncated list is not an answer.
func capsuleTooLarge(list string, bound int) error {
	return (&model.Error{Code: model.CodeResourceLimit,
		Message: "the capsule's " + list + " list exceeds " + strconv.Itoa(bound) + " records"}).
		WithRemediation("narrow the session's scope; a capsule is never sealed with a truncated list")
}

// capsulePageLimit resolves a requested page size against the configured ceiling. Zero
// means the endpoint default, never unlimited.
func (s *Service) capsulePageLimit(requested int) int {
	if requested <= 0 || requested > s.limits.MaxPageItems {
		return s.limits.MaxPageItems
	}
	return requested
}

// pageCapsuleList returns one keyset page of an already-canonically-ordered capsule
// list and the continuation for the next.
//
// The cursor is the last item's own key rather than a signed token, and that is
// not a missing signature: the frozen Options carries no signer and no lease
// store, and it needs neither, because a capsule is immutable and write-once.
// A continuation can only re-read the same sealed record -- there is no
// generation to repin, no spool to expire and nothing to re-serve or skip. A
// key naming no record is refused rather than silently restarting the list.
func pageCapsuleList[T any](items []T, key func(T) string, cursor string, limit int) ([]T, string, error) {
	start := 0
	if cursor != "" {
		start = -1
		for i, it := range items {
			if key(it) == cursor {
				start = i + 1
				break
			}
		}
		if start < 0 {
			return nil, "", (&model.Error{Code: model.CodeCursorInvalid,
				Message: "cursor names no record in this capsule projection"}).
				WithDetail("endpoint", "context_capsule")
		}
	}
	if start >= len(items) {
		return nil, "", nil
	}
	end := start + limit
	if end > len(items) {
		end = len(items)
	}
	page := items[start:end]
	if end == len(items) {
		return page, "", nil
	}
	return page, key(page[len(page)-1]), nil
}

// The four keyset keys. A fact reference needs both halves: one relation can be
// accepted by more than one observation, so the relation alone is not unique and
// a page keyed on it would drop records.
func capsuleFactKey(f model.FactReference) string {
	return string(f.RelationID) + "|" + string(f.ObservationID)
}
func capsuleObservationKey(o model.ObservationReference) string { return string(o.ObservationID) }
func capsuleCoverageKey(f model.FileCoverage) string            { return string(f.FileID) }
func capsuleWaiverKey(w model.WaiverRecord) string              { return string(w.FileID) }

// canonicalCapsuleHash derives the Section 17.3 capsule identity over
// canonicalCapsuleDomain, mirroring context.canonicalManifestHash: every
// component is length-framed by model.Hasher and every list is preceded by its
// own count, so no rearrangement of records can forge a boundary.
//
// It hashes the capsule in STORED order -- buildCapsule is what canonicalizes
// the ordering, so the digest and the paged projection can never disagree.
//
// It includes exactly what Section 17.3 lists: the manifest hash, the semantic
// analysis binding, the scope version and scope, the accepted and rejected fact
// semantics, the contradiction and unresolved observation semantics, the scope
// review ids, the coverage and waiver semantics and the strict gate. It excludes
// CreatedAt and every audit timestamp, the LOCAL generation id, session and run
// ids and cursor tokens -- the same rule manifest.go:69-73 states -- so the same
// inputs hash identically across two builds and across a store reopened later.
func canonicalCapsuleHash(c model.Capsule) string {
	h := model.NewHasher(canonicalCapsuleDomain)
	h.AddString(c.ManifestHash)
	h.AddString(string(c.Binding.RepositoryID))
	h.AddString(string(c.Binding.SnapshotID))
	h.AddString(string(c.Binding.AnalysisKey))
	h.AddString(capsuleInt(int64(c.ScopeVersion)))

	h.AddString(capsuleInt(int64(len(c.Scope))))
	for _, id := range c.Scope {
		h.AddString(string(id))
	}
	for _, facts := range [][]model.FactReference{c.AcceptedFacts, c.RejectedFacts} {
		h.AddString(capsuleInt(int64(len(facts))))
		for _, f := range facts {
			h.AddString(string(f.RelationID))
			h.AddString(capsuleInt(int64(len(f.EvidenceIDs))))
			for _, e := range f.EvidenceIDs {
				h.AddString(string(e))
			}
			h.AddString(string(f.ObservationID))
		}
	}
	for _, refs := range [][]model.ObservationReference{c.Contradictions, c.Unresolved} {
		h.AddString(capsuleInt(int64(len(refs))))
		for _, o := range refs {
			h.AddString(string(o.ObservationID))
			h.AddString(string(o.Kind))
			h.AddString(capsuleInt(int64(len(o.References))))
			for _, r := range o.References {
				hashClaimReference(h, r)
			}
			h.AddString(o.Note)
		}
	}
	h.AddString(capsuleInt(int64(len(c.ScopeReviewIDs))))
	for _, id := range c.ScopeReviewIDs {
		h.AddString(id)
	}
	h.AddString(capsuleInt(int64(len(c.Coverage))))
	for _, f := range c.Coverage {
		h.AddString(string(f.FileID))
		h.AddString(f.ContentHash)
		h.AddString(capsuleInt(f.Size))
		h.AddString(string(f.State))
		h.AddString(strconv.FormatBool(f.Waived))
	}
	h.AddString(capsuleInt(int64(len(c.Waivers))))
	for _, w := range c.Waivers {
		h.AddString(string(w.FileID))
		h.AddString(w.ContentHash)
		h.AddString(w.ActorID)
		h.AddString(w.Reason)
	}
	h.AddString(strconv.FormatBool(c.StrictGateSatisfied))
	return h.Sum()
}

// hashClaimReference absorbs one claim reference at fixed arity: an absent
// source is four empty components rather than none, so an omission can never
// alias a present value. It mirrors model.ClaimReference.canonical, which is
// unexported in model and therefore unreachable from here.
func hashClaimReference(h *model.Hasher, r model.ClaimReference) {
	h.AddString(string(r.NodeID))
	h.AddString(string(r.RelationID))
	if r.Source == nil {
		h.AddString("")
		h.AddString("")
		h.AddString("")
		h.AddString("")
		return
	}
	h.AddString(string(r.Source.FileID))
	h.AddString(r.Source.ContentHash)
	h.AddString(strconv.FormatUint(r.Source.Bytes.Start, 10))
	h.AddString(strconv.FormatUint(r.Source.Bytes.End, 10))
}

// capsuleInt renders an integer component, matching context/manifest.go:23. The
// name is prefixed because this package is filled in by several files at once
// and a bare hashInt would collide with one of them.
func capsuleInt(n int64) string { return strconv.FormatInt(n, 10) }
