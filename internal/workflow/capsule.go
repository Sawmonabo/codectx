package workflow

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"strconv"

	"github.com/Sawmonabo/codectx/internal/config"
	"github.com/Sawmonabo/codectx/internal/model"
	"github.com/Sawmonabo/codectx/internal/storage/sqlite"
)

// Capsule pages one projection of a session's sealed capsule (Section 17.3).
//
// It reads the stored record and never seals one: a capsule exists only because
// a completion produced it, so paging a session that sealed none reports the
// store's own "no capsule" rejection rather than manufacturing an empty page
// that reads as "this session recorded nothing".
//
// The capsule blob carries counts, not records, so the page comes from the
// store's keyset-paged rows and only one page's records exist in this process
// at a time. The continuation is decided from the last row's ordinal against
// the sealed count: no second query, and no empty trailing page.
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
	list := req.View.List()
	rows, err := s.sessions.CapsuleRows(ctx, req.SessionID, req.ActorID, list,
		req.Page.Cursor, s.capsulePageLimit(req.Page.Limit))
	if err != nil {
		return model.CapsulePage{}, err
	}
	switch req.View {
	case model.CapsuleViewAcceptedFacts:
		page.AcceptedFacts, err = decodeCapsuleRows[model.FactReference](rows)
	case model.CapsuleViewRejectedFacts:
		page.RejectedFacts, err = decodeCapsuleRows[model.FactReference](rows)
	case model.CapsuleViewContradictions:
		page.Contradictions, err = decodeCapsuleRows[model.ObservationReference](rows)
	case model.CapsuleViewUnresolved:
		page.Unresolved, err = decodeCapsuleRows[model.ObservationReference](rows)
	case model.CapsuleViewCoverage:
		page.Coverage, err = decodeCapsuleRows[model.FileCoverage](rows)
	case model.CapsuleViewWaivers:
		page.Waivers, err = decodeCapsuleRows[model.WaiverRecord](rows)
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
	if len(rows) > 0 {
		// The sealed count is the length of the whole list, so the last row of
		// the last page is the one whose ordinal is count-1 and it ends the
		// projection. Any earlier row names the next page.
		if last := rows[len(rows)-1]; last.Ordinal+1 < c.Counts.Of(list) {
			page.Meta.NextCursor = last.Key
		}
	}
	if err := page.Validate(); err != nil {
		return model.CapsulePage{}, err
	}
	return page, nil
}

// decodeCapsuleRows turns one page of stored rows back into the view's record
// type. A row that does not decode is a corrupted seal, not a caller error: the
// bytes were written by NewCapsuleRow from a validated record.
func decodeCapsuleRows[T any](rows []model.CapsuleRow) ([]T, error) {
	out := make([]T, 0, len(rows))
	for _, row := range rows {
		var v T
		if err := json.Unmarshal(row.JSON, &v); err != nil {
			return nil, typedErrf(model.CodeInternal,
				"the sealed capsule's %s record %d is not readable", row.List, row.Ordinal)
		}
		out = append(out, v)
	}
	return out, nil
}

// Export returns the sealed capsule's identity and record counts for one
// session.
//
// It no longer carries the records: they are rows, and a whole-capsule render
// streams them per list through Capsule. The byte budget below therefore
// measures a small fixed record and stays as the caller's own ceiling.
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

// buildCapsule seals the Section 17.3 completion record.
//
// It assembles nothing in memory. capsuleSource streams the session's stored
// observations, scope, coverage and waivers -- never a live recomputation,
// which is what makes the identity reproducible -- and the seal makes three
// bounded passes over it: pass 1 counts each list, pass 2 derives the canonical
// identity with model.CapsuleCanonicalHash, and pass 3 is the row write
// PutCapsule performs inside its own write transaction. Every pass re-walks the
// session store, so no list is ever held whole; the counting pass exists
// because a list's count is hashed AHEAD of its records, which is what makes the
// streamed digest byte-identical to the assembled one.
//
// It returns the STORED capsule, so a second completion answers with the first
// capsule and its original timestamp. Sealing before the advance to complete is
// deliberate and is not atomic with it (ruling Q4): the write is idempotent and
// write-once, so a crash between the two is retried, never duplicated.
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
	src := &capsuleSource{svc: s, rec: rec}

	// Pass 1: count. It is also where a list that exceeds an operator-set
	// ceiling refuses the seal, before the digest or the write spends work.
	var counts model.CapsuleCounts
	for _, list := range model.CapsuleListOrder {
		var n int64
		if err := src.Rows(ctx, list, func(model.CapsuleRow) error { n++; return nil }); err != nil {
			return model.Capsule{}, err
		}
		counts.Set(list, n)
	}

	c := model.Capsule{
		SessionID:    rec.ID,
		ActorID:      rec.ActorID,
		Binding:      rec.Binding,
		ManifestHash: manifest.CanonicalHash,
		ScopeVersion: rec.ScopeVersion,
		Counts:       counts,
		Completeness: manifest.Completeness,
		// Straight from the one readiness evaluation. Capsule.Validate refuses
		// a satisfied strict gate beside recorded waivers, so a wrong gate
		// fails loudly here instead of being defensively corrected.
		StrictGateSatisfied: g.Strict,
		CreatedAt:           s.now().UTC(),
	}
	// Pass 2: the identity, over the same source and the counts pass 1 produced.
	if c.CanonicalHash, err = model.CapsuleCanonicalHash(ctx, c, src); err != nil {
		return model.Capsule{}, err
	}
	if err := s.checkCapsuleBytes(c); err != nil {
		return model.Capsule{}, err
	}
	if err := c.Validate(); err != nil {
		return model.Capsule{}, err
	}
	// Pass 3: the write. PutCapsule streams the rows itself, in its own
	// transaction, after its write-once check.
	stored, err := s.sessions.PutCapsule(ctx, c, src)
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

// capsuleSource is the seal's streaming view of one session's records. It
// satisfies model.CapsuleListSource: each call to Rows re-walks the session
// store for that list from its first record, so the counting pass, the digest
// pass and the row write see the same sequence without any of them retaining it.
//
// It caches nothing on purpose. A source that memoised pass 1 would turn the
// three bounded walks into one whole-list buffer, which is exactly the heap the
// row redesign exists to remove.
type capsuleSource struct {
	svc *Service
	rec sqlite.SessionRecord
}

var _ model.CapsuleListSource = (*capsuleSource)(nil)

// Rows streams one list in its canonical order.
func (src *capsuleSource) Rows(ctx context.Context, list model.CapsuleList,
	yield func(model.CapsuleRow) error) error {
	e := &capsuleEmitter{list: list, yield: yield, limit: src.svc.limits.MaxCapsuleRecordsPerList,
		limitKey: "context.max_capsule_records_per_list"}
	switch list {
	case model.CapsuleListScope:
		return src.scope(ctx, e)
	case model.CapsuleListAcceptedFacts:
		return src.facts(ctx, e, model.ObservationAcceptFact)
	case model.CapsuleListRejectedFacts:
		return src.facts(ctx, e, model.ObservationRejectFact)
	case model.CapsuleListContradictions:
		return src.observations(ctx, e, model.ObservationContradiction)
	case model.CapsuleListUnresolved:
		return src.observations(ctx, e, model.ObservationUnresolved)
	case model.CapsuleListScopeReviewIDs:
		return src.reviews(ctx, e)
	case model.CapsuleListCoverage:
		// Coverage grows with the session's pinned file set rather than with
		// what the actor observed, so it has a ceiling of its own.
		e.limit, e.limitKey = src.svc.limits.MaxCapsuleCoverageFiles, "context.max_capsule_coverage_files"
		return src.coverage(ctx, e)
	case model.CapsuleListWaivers:
		return src.waivers(ctx, e)
	}
	// model.CapsuleListOrder is closed; a list added without a collector must
	// fail rather than seal an empty list into the capsule's identity.
	return typedErrf(model.CodeInternal, "capsule list %q has no collector", list)
}

// capsuleEmitter numbers one list's records and hands each to the caller's
// yield. It is the single place a capsule record becomes a row, so every list
// shares one ordinal discipline and one ceiling check.
type capsuleEmitter struct {
	list     model.CapsuleList
	yield    func(model.CapsuleRow) error
	limit    config.Limit
	limitKey string
	ordinal  int64
}

// emit refuses before encoding: a list that has already reached the operator's
// ceiling is not truncated, it stops the seal.
func (e *capsuleEmitter) emit(record any) error {
	if e.limit.Exceeded(e.ordinal + 1) {
		return capsuleListTooLarge(e.list, e.limitKey, e.limit, e.ordinal+1)
	}
	row, err := model.NewCapsuleRow(e.list, e.ordinal, record)
	if err != nil {
		return err
	}
	e.ordinal++
	return e.yield(row)
}

// scope streams the pinned manifest's DISTINCT node ids in node-id order. The
// store answers the dedup and the ordering in SQL (Store.ManifestScopeNodes),
// so the seal holds one page of ids and not a manifest-sized set.
//
// Scope is the PINNED MANIFEST's scope and Coverage is the SESSION's union, and
// after an Include the two deliberately differ: a recompiled manifest covers the
// included seeds only, while session_files accumulates across every include
// (Store.IncludeManifest inserts, never replaces). So a sealed capsule can list
// coverage for a file whose nodes are absent from scope; that is the accepted
// reading of Section 17.3, not a drift between two spellings of one set.
func (src *capsuleSource) scope(ctx context.Context, e *capsuleEmitter) error {
	var after model.NodeID
	for {
		ids, err := src.svc.sessions.ManifestScopeNodes(ctx, src.rec.ManifestID, after, src.svc.limits.MaxPageItems)
		if err != nil {
			return err
		}
		if len(ids) == 0 {
			return nil
		}
		for _, id := range ids {
			if err := e.emit(model.CapsuleScopeNode{NodeID: id}); err != nil {
				return err
			}
		}
		after = ids[len(ids)-1]
	}
}

// coverage streams the session's whole coverage set in file-id order, which is
// the store's own keyset order and therefore already the capsule's canonical
// order.
func (src *capsuleSource) coverage(ctx context.Context, e *capsuleEmitter) error {
	var after model.FileID
	for {
		rows, err := src.svc.sessions.Coverage(ctx, src.rec.ID, src.rec.ActorID, after, src.svc.limits.MaxPageItems)
		if err != nil {
			return err
		}
		if len(rows) == 0 {
			return nil
		}
		for _, f := range rows {
			if err := e.emit(f); err != nil {
				return err
			}
		}
		after = rows[len(rows)-1].FileID
	}
}

// waivers streams the session's recorded exceptions. A capsule that listed no
// waivers while its coverage shows waived files would be a dishonest durable
// artifact, so the reasons come from the store's own append-only
// coverage_waivers rows -- never from the Waive request that echoed them -- and
// arrive in the store's file-id order, which is the capsule's canonical order.
func (src *capsuleSource) waivers(ctx context.Context, e *capsuleEmitter) error {
	out, err := src.svc.sessions.Waivers(ctx, src.rec.ID, src.rec.ActorID)
	if err != nil {
		return err
	}
	for _, w := range out {
		if err := e.emit(w); err != nil {
			return err
		}
	}
	return nil
}

// facts streams one fact-bearing observation kind. Section 17.3 records a fact
// as its relation and the observation that accepted or rejected it, so one
// observation naming several relations contributes one record each.
//
// The order is the store's observation-id order, and within one observation the
// relation ids sorted -- so the sequence is independent of how that observation
// listed its references without any cross-observation sort, which could not be
// done without holding the whole list. A relation named twice by one
// observation is one fact: the pair is the record's key, and a repeated key
// would be an unaddressable row.
func (src *capsuleSource) facts(ctx context.Context, e *capsuleEmitter, kind model.ObservationKind) error {
	return src.eachObservation(ctx, kind, func(o model.Observation) error {
		rels := make([]model.RelationID, 0, len(o.References))
		for _, ref := range o.References {
			if ref.RelationID != "" {
				rels = append(rels, ref.RelationID)
			}
		}
		sort.Slice(rels, func(i, j int) bool { return rels[i] < rels[j] })
		var previous model.RelationID
		for i, r := range rels {
			if i > 0 && r == previous {
				continue
			}
			previous = r
			if err := e.emit(model.FactReference{RelationID: r, ObservationID: o.ID}); err != nil {
				return err
			}
		}
		return nil
	})
}

// observations streams one observation kind without re-embedding the whole
// record: the id, its canonical references and the actor's own note.
func (src *capsuleSource) observations(ctx context.Context, e *capsuleEmitter, kind model.ObservationKind) error {
	return src.eachObservation(ctx, kind, func(o model.Observation) error {
		return e.emit(model.ObservationReference{
			ObservationID: o.ID, Kind: o.Kind, References: o.References, Note: o.Note,
		})
	})
}

// reviews names the current-scope scope reviews. The review rows stay in
// session_observations for audit; the capsule records which ones bound this
// scope version rather than copying the attestation.
func (src *capsuleSource) reviews(ctx context.Context, e *capsuleEmitter) error {
	return src.eachObservation(ctx, model.ObservationScopeReview, func(o model.Observation) error {
		return e.emit(model.CapsuleScopeReview{ObservationID: string(o.ID)})
	})
}

// eachObservation pages one kind at the session's current scope version by
// keyset on the observation id, which is the order the store returns and the
// order the capsule stores, and hands each observation to fn without retaining
// it. An include bumps the scope version, so observations bound to a superseded
// scope are not in the capsule -- they stay stored.
func (src *capsuleSource) eachObservation(ctx context.Context, kind model.ObservationKind,
	fn func(model.Observation) error) error {
	var after model.ObservationID
	for {
		page, err := src.svc.sessions.Observations(ctx, src.rec.ID, src.rec.ActorID, kind,
			src.rec.ScopeVersion, after, src.svc.limits.MaxPageItems)
		if err != nil {
			return err
		}
		if len(page) == 0 {
			return nil
		}
		for _, o := range page {
			if err := fn(o); err != nil {
				return err
			}
		}
		after = page[len(page)-1].ID
	}
}

// checkCapsuleBytes applies context.max_capsule_bytes as a CALLER BUDGET
// against the serialized record, so an over-budget capsule is reported before
// the write instead of at it.
//
// The key is unlimited by default: a capsule is never refused for being the
// capsule of a large repository. A caller that set a ceiling -- because it must
// fit the record in a window -- is told the ceiling it set and the size the
// capsule reached, so raising it is a decision with both numbers in hand rather
// than a retry against an unnamed bound.
func (s *Service) checkCapsuleBytes(c model.Capsule) error {
	payload, err := json.Marshal(c)
	if err != nil {
		return typedErrf(model.CodeInternal, "the capsule could not be serialized: %v", err)
	}
	if s.limits.MaxCapsuleBytes.Exceeded(int64(len(payload))) {
		return (&model.Error{Code: model.CodeResourceLimit,
			Message: "capsule exceeds the caller's context.max_capsule_bytes budget",
			Details: map[string]string{
				"limit":         "context.max_capsule_bytes",
				"limit_value":   s.limits.MaxCapsuleBytes.String(),
				"capsule_bytes": strconv.Itoa(len(payload)),
			}}).
			WithRemediation("page the capsule through its six views, or raise context.max_capsule_bytes (0 or \"unlimited\" removes the ceiling)")
	}
	return nil
}

// capsuleListTooLarge is the explicit refusal of a capsule list that exceeded a
// ceiling THE OPERATOR SET. There is no such refusal on stock configuration:
// both keys default to unlimited, and a completion is never refused for the
// size of the session it records.
//
// Section 17.3 forbids silent omission, so this stops the seal rather than
// truncating the list, and it names both numbers -- the key's value and the
// count the list reached -- so raising the ceiling is a decision, not a guess.
func capsuleListTooLarge(list model.CapsuleList, key string, limit config.Limit, reached int64) error {
	return (&model.Error{Code: model.CodeResourceLimit,
		Message: "the capsule's " + string(list) + " list exceeds the configured " + key,
		Details: map[string]string{
			"limit":         key,
			"limit_value":   limit.String(),
			"list":          string(list),
			"records_found": strconv.FormatInt(reached, 10),
		}}).
		WithRemediation("raise " + key + " (0 or \"unlimited\" removes the ceiling) or narrow the session's scope; a capsule is never sealed with a truncated list")
}

// capsulePageLimit resolves a requested page size against the configured ceiling. Zero
// means the endpoint default, never unlimited.
func (s *Service) capsulePageLimit(requested int) int {
	if requested <= 0 || requested > s.limits.MaxPageItems {
		return s.limits.MaxPageItems
	}
	return requested
}

// CapsuleRows reads one keyset page of one sealed capsule list, with the
// continuation the caller uses to read the next.
//
// It exists for the whole-capsule render `codectx context export` performs.
// Export answers identity and counts only -- a sealed capsule's records are
// rows -- so a caller that must write every record streams each list through
// this call one page at a time, and the records resident in either process are
// one page's worth whatever the session recorded.
//
// The continuation rule is Capsule's, for the same reason: the sealed count is
// the length of the whole list, so the last row of the last page is the one
// whose ordinal is count-1. A reader is therefore never handed a cursor that
// would fetch an empty page.
func (s *Service) CapsuleRows(ctx context.Context, req model.SessionRequest,
	list model.CapsuleList, after string, limit int) ([]model.CapsuleRow, string, error) {
	if err := req.Validate(); err != nil {
		return nil, "", err
	}
	if !list.Valid() {
		return nil, "", typedErrf(model.CodeArgumentInvalid,
			"%q is not a capsule list", list)
	}
	c, err := s.sessions.Capsule(ctx, req.SessionID, req.ActorID)
	if err != nil {
		return nil, "", err
	}
	rows, err := s.sessions.CapsuleRows(ctx, req.SessionID, req.ActorID, list,
		after, s.capsulePageLimit(limit))
	if err != nil {
		return nil, "", err
	}
	var next string
	if len(rows) > 0 {
		if last := rows[len(rows)-1]; last.Ordinal+1 < c.Counts.Of(list) {
			next = last.Key
		}
	}
	return rows, next, nil
}
