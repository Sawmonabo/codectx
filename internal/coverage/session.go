package coverage

import (
	"context"
	"errors"
	"log/slog"
	"strconv"
	"time"

	"github.com/Sawmonabo/codectx/internal/model"
	"github.com/Sawmonabo/codectx/internal/pagination"
	"github.com/Sawmonabo/codectx/internal/storage/sqlite"
)

// endpointStatus is the cursor endpoint of the status page. A cursor signed for
// any other endpoint is refused by pagination.Signer.DecodeCursor, so a search
// or reference continuation can never be resumed as a coverage page.
const endpointStatus = "context.status"

// statusQueryHashDomain versions the status cursor preimage. Changing what the
// preimage folds without changing this label would let an old token resume a
// query it no longer describes.
const statusQueryHashDomain = "codectx.coverage.status.v1"

// openRequestHashDomain versions the open-request preimage. The store compares
// this hash against the one it stored for an actor's idempotency key: the same
// key with a different request is CTX_VERSION_CONFLICT, so the preimage must
// fold everything that makes two open requests different, and nothing else.
const openRequestHashDomain = "codectx.coverage.open.v1"

// New validates the options and builds the service.
//
// Sessions, OpenSource, Signer and every Limits bound are required: the
// composition root builds this eagerly when a workspace opens, so a missing one
// is a wiring defect that must fail there rather than per request. Leases and
// Logger are the two exceptions. A nil Logger is the discard logger's job, and a
// nil Leases leaves sessions unleased -- legible behaviour that New warns about
// once rather than a per-request failure, mirroring how internal/graph answers
// without a continuation when no lease holder is available.
func New(o Options) (*Service, error) {
	if o.Sessions == nil {
		return nil, typedErrf(model.CodeInternal, "coverage service was built without a session store")
	}
	if o.OpenSource == nil {
		return nil, typedErrf(model.CodeInternal, "coverage service was built without a source opener")
	}
	if o.Signer == nil {
		return nil, typedErrf(model.CodeInternal, "coverage service was built without a token signer")
	}
	if err := checkLimits(o.Limits); err != nil {
		return nil, err
	}
	now := o.Now
	if now == nil {
		now = time.Now
	}
	log := o.Logger
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	s := &Service{
		sessions: o.Sessions,
		open:     o.OpenSource,
		signer:   o.Signer,
		leases:   o.Leases,
		limits:   o.Limits,
		now:      now,
		log:      log,
	}
	if s.leases == nil {
		// Both consequences are real and neither is recoverable per request, so
		// the operator hears about it once at composition instead of never.
		log.Warn("coverage sessions will not retain their generation and status pages will not continue",
			"component", "coverage", "reason", "no retention leases were supplied")
	}
	return s, nil
}

// checkLimits rejects a Limits that cannot bound a request. Zero on a request
// field means the configured default, so a zero default means "unlimited",
// which Section 20.2 forbids.
func checkLimits(l Limits) error {
	for _, b := range []struct {
		name  string
		value int64
	}{
		{"chunk_bytes", l.ChunkBytes},
		{"max_chunk_bytes", l.MaxChunkBytes},
		{"max_source_response_bytes", l.MaxSourceResponseBytes},
		{"max_metadata_response_bytes", l.MaxMetadataResponseBytes},
		{"max_receipts_per_confirmation", int64(l.MaxReceiptsPerConfirmation)},
		{"max_unconfirmed_chunks_per_session", int64(l.MaxUnconfirmedChunksPerSession)},
		{"max_page_items", int64(l.MaxPageItems)},
		{"session_ttl", int64(l.SessionTTL)},
		{"query_timeout", int64(l.QueryTimeout)},
		{"receipt_ttl", int64(l.ReceiptTTL)},
	} {
		if b.value <= 0 {
			return typedErrf(model.CodeInternal,
				"coverage limit %s is %d; every bound must be resolved to a positive value before the service is built",
				b.name, b.value)
		}
	}
	if l.MaxReceiptsPerConfirmation > model.MaxReceiptsPerConfirmation {
		return typedErrf(model.CodeInternal,
			"coverage limit max_receipts_per_confirmation is %d, above the %d the store enforces",
			l.MaxReceiptsPerConfirmation, model.MaxReceiptsPerConfirmation)
	}
	if l.MaxPageItems > model.MaxPageItems {
		return typedErrf(model.CodeInternal,
			"coverage limit max_page_items is %d, above the %d a page may carry",
			l.MaxPageItems, model.MaxPageItems)
	}
	return nil
}

// OpenSession compiles nothing: it opens an actor-specific session over a
// manifest Task 15 has already persisted, acquires the model.LeaseSession
// retention lease that stops an expired session from resurrecting deleted
// source, and reports the resulting status. Owned by L3.
//
// The store owns idempotency: with a key, a retry by the same actor carrying
// the same open-request hash returns the existing session and the freshly
// generated id is discarded, while the same key with a different hash is
// CTX_VERSION_CONFLICT. The phase gate, the manifest's generation and snapshot
// and the session's file scope are the store's too, so none of it is repeated
// here.
func (s *Service) OpenSession(ctx context.Context, req model.PlanRequest, manifest model.ManifestID) (model.SessionStatus, error) {
	if err := req.Validate(); err != nil {
		return model.SessionStatus{}, err
	}
	if !model.ValidHexID(string(manifest)) {
		return model.SessionStatus{}, typedErrf(model.CodeArgumentInvalid,
			"context.manifest_id is not a well-formed identifier")
	}
	id, err := model.NewRandomID()
	if err != nil {
		return model.SessionStatus{}, err
	}

	ctx, cancel := context.WithTimeout(ctx, s.limits.QueryTimeout)
	defer cancel()

	session, err := s.sessions.OpenSession(ctx, model.SessionOpen{
		ID:              model.SessionID(id),
		ActorID:         req.ActorID,
		IdempotencyKey:  req.IdempotencyKey,
		OpenRequestHash: openRequestHash(req, manifest),
		ManifestID:      manifest,
		ExpiresAt:       s.now().Add(s.limits.SessionTTL).UTC(),
	})
	if err != nil {
		return model.SessionStatus{}, err
	}
	rec, err := s.sessions.Session(ctx, session, req.ActorID)
	if err != nil {
		return model.SessionStatus{}, err
	}
	if err := s.retain(ctx, rec); err != nil {
		return model.SessionStatus{}, err
	}
	s.log.Debug("opened a read session", "component", "coverage",
		"generation_id", int64(rec.Binding.GenerationID), "phase", string(rec.Phase))
	return s.status(ctx, rec)
}

// openRequestHash folds everything that makes two open requests different. It
// is fixed arity, so an absent idempotency key can never alias a present one,
// and it is a digest rather than the request itself because the store stores it
// beside the key and the task text is unbounded.
func openRequestHash(req model.PlanRequest, manifest model.ManifestID) string {
	h := model.NewHasher(openRequestHashDomain)
	h.AddString(req.ActorID)
	h.AddString(string(manifest))
	h.AddString(req.Context.Task)
	h.AddString(string(req.Context.Phase))
	h.AddString(strconv.FormatInt(int64(req.Context.GenerationID), 10))
	h.AddString(strconv.Itoa(len(req.Context.Seeds)))
	for _, seed := range req.Context.Seeds {
		h.AddString(seed)
	}
	h.AddString(strconv.FormatInt(req.Context.Budget.MaxEstimatedTokens, 10))
	h.AddString(strconv.FormatInt(req.Context.Budget.MaxBytes, 10))
	h.AddString(strconv.Itoa(req.Context.Budget.MaxFiles))
	h.AddString(strconv.Itoa(req.Context.Budget.MaxSlices))
	return h.Sum()
}

// retain acquires the session's retention lease, which is what stops an expired
// session from resurrecting source its generation no longer holds. The lease
// expires on the TTL the composition root gave pagination.Leases, which must be
// coverage.session_ttl for the lease and the session to die together.
//
// A session whose lease could not be acquired is reported as a failure rather
// than served: retention may collect the blobs it is about to hand out.
func (s *Service) retain(ctx context.Context, rec sqlite.SessionRecord) error {
	if s.leases == nil {
		return nil
	}
	_, err := s.leases.Acquire(ctx, rec.Binding.GenerationID, rec.Binding.SnapshotID, model.LeaseSession)
	return err
}

// Status reports this actor's coverage for this session: one page of per-file
// records keyed on file_id plus the honest session status. Owned by L3.
//
// The page is keyset paged on file_id, exactly as the store orders it, and the
// counts come from status's single CoverageSummary call rather than from
// walking the pages -- a 250k-file session is 1250 pages and one aggregate.
func (s *Service) Status(ctx context.Context, req model.SessionRequest, page model.PageRequest) (model.Page[model.FileCoverage], model.SessionStatus, error) {
	var (
		emptyPage   model.Page[model.FileCoverage]
		emptyStatus model.SessionStatus
	)
	if err := req.Validate(); err != nil {
		return emptyPage, emptyStatus, err
	}
	if err := page.Validate(); err != nil {
		return emptyPage, emptyStatus, err
	}

	ctx, cancel := context.WithTimeout(ctx, s.limits.QueryTimeout)
	defer cancel()

	rec, err := s.sessions.Session(ctx, req.SessionID, req.ActorID)
	if err != nil && !(expiredSession(err) && rec.ID != "") {
		return emptyPage, emptyStatus, err
	}

	limit := s.statusLimit(page.Limit)
	after, err := s.resumeStatus(page.Cursor, rec)
	if err != nil {
		return emptyPage, emptyStatus, err
	}

	// One extra record answers "is there another page" without a second query.
	items, err := s.sessions.Coverage(ctx, req.SessionID, req.ActorID, after, limit+1)
	if err != nil && !expiredSession(err) {
		return emptyPage, emptyStatus, err
	}
	more := len(items) > limit
	if more {
		items = items[:limit]
	}

	status, err := s.status(ctx, rec)
	if err != nil {
		return emptyPage, emptyStatus, err
	}

	meta := model.QueryMeta{Binding: rec.Binding}
	if more {
		token, why, err := s.statusCursor(ctx, rec, items[len(items)-1].FileID)
		if err != nil {
			return emptyPage, emptyStatus, err
		}
		if token == "" {
			// A page that cannot be continued says so rather than looking like
			// the last page, which would silently hide the remaining files.
			meta.Truncated, meta.TruncationReason = true, why
		} else {
			meta.NextCursor = token
		}
	}
	out := model.Page[model.FileCoverage]{Meta: meta, Items: items}
	if err := out.Validate(); err != nil {
		return emptyPage, emptyStatus, err
	}
	for i := range out.Items {
		if err := out.Items[i].Validate(); err != nil {
			return emptyPage, emptyStatus, err
		}
	}
	return out, status, nil
}

// statusLimit resolves the requested page size, leaving room for the one extra
// record Status probes with.
//
// Store.Coverage runs its limit through pageLimit (query.go:283), which SILENTLY
// CLAMPS anything above model.MaxPageItems instead of rejecting it. So asking
// for model.MaxPageItems+1 returns model.MaxPageItems and the probe can never
// see a further record: every page past the first 200 files would vanish
// without a cursor and without a truncation notice. The page is one record
// short of the ceiling so the probe stays inside it.
func (s *Service) statusLimit(requested int) int {
	limit := requested
	if limit <= 0 || limit > s.limits.MaxPageItems {
		limit = s.limits.MaxPageItems
	}
	if limit >= model.MaxPageItems {
		limit = model.MaxPageItems - 1
	}
	return limit
}

// resumeStatus turns a presented cursor into the file_id to resume after. A
// cursor is a claim about which page this is; honouring one that names another
// generation, another session or a spool this endpoint never writes would
// silently re-serve or skip files, so each is refused rather than trusted.
func (s *Service) resumeStatus(token string, rec sqlite.SessionRecord) (model.FileID, error) {
	if token == "" {
		return "", nil
	}
	c, err := s.signer.DecodeCursor(token, endpointStatus, s.now())
	if err != nil {
		return "", err
	}
	if c.GenerationID != rec.Binding.GenerationID || c.AnalysisKey != rec.Binding.AnalysisKey {
		return "", cursorInvalid("cursor was issued against a different generation")
	}
	if c.QueryHash != statusQueryHash(rec) {
		return "", cursorInvalid("cursor was issued for a different session or actor")
	}
	if c.SpoolID != "" {
		return "", cursorInvalid("cursor carries spooled traversal state this endpoint does not produce")
	}
	if c.LastKey != "" && !model.ValidHexID(c.LastKey) {
		return "", cursorInvalid("cursor sort key is not a file identifier")
	}
	return model.FileID(c.LastKey), nil
}

// statusCursor signs the continuation for the next status page, or reports why
// there cannot be one. Like every other continuation in the tree it carries a
// fresh cursor-owned retention lease, and it expires with the session: a status
// page resumed after the session is gone would describe coverage nobody holds.
func (s *Service) statusCursor(ctx context.Context, rec sqlite.SessionRecord, last model.FileID) (token, why string, err error) {
	if last == "" {
		return "", "", typedErrf(model.CodeInternal, "a status page ended without a keyset position")
	}
	if s.leases == nil {
		return "", "this workspace does not retain coverage continuations", nil
	}
	if !model.ValidHexID(string(rec.Binding.AnalysisKey)) {
		// The generation is still staging, so there is no analysis key to pin
		// the continuation to and a cursor cannot be well formed.
		return "", "this session's generation is not yet analysed, so its coverage cannot be continued", nil
	}
	lease, err := s.leases.Acquire(ctx, rec.Binding.GenerationID, rec.Binding.SnapshotID, model.LeaseCursor)
	if err != nil {
		return "", "", err
	}
	token, err = s.signer.EncodeCursor(pagination.Cursor{
		Endpoint:     endpointStatus,
		GenerationID: rec.Binding.GenerationID,
		AnalysisKey:  rec.Binding.AnalysisKey,
		QueryHash:    statusQueryHash(rec),
		LastKey:      string(last),
		LeaseID:      lease.ID,
		ExpiresAt:    rec.ExpiresAt,
	})
	if err != nil {
		s.releaseLease(ctx, lease.ID)
		return "", "", err
	}
	return token, "", nil
}

// releaseLease returns a lease whose cursor never reached the caller. The
// request's own context may already be done, so the release runs on a fresh
// one: leaking it would pin a generation against retention for a page nobody
// can ask for.
func (s *Service) releaseLease(ctx context.Context, id string) {
	if err := s.leases.Release(context.WithoutCancel(ctx), id); err != nil {
		s.log.Warn("a coverage continuation lease could not be released",
			"component", "coverage", "error", err.Error())
	}
}

// statusQueryHash pins a status cursor to the one session and actor it was
// issued for. Coverage belongs to one actor, one session and one source hash,
// so a continuation must never be replayable inside another.
func statusQueryHash(rec sqlite.SessionRecord) string {
	return model.H(statusQueryHashDomain, endpointStatus, string(rec.ID), rec.ActorID, string(rec.ManifestID))
}

// Close ends the session with the Section 17.1 compare-and-swap:
// AdvanceSession{Target: StateClosed, ExpectedVersion}. It releases the
// retention lease. Owned by L3.
//
// There is no CloseSession and no edge out of complete, so closing a completed
// session is the store's CTX_VERSION_CONFLICT rather than an invented code --
// the same answer a stale expected version gets, because both mean the caller's
// view of the session is not the stored one.
//
// The session's retention lease is NOT released here: pagination.Leases
// addresses a lease by the id it minted, and nothing persists that id against
// the session, so Close has nothing to name. The lease expires with
// coverage.session_ttl instead. See the lane report.
func (s *Service) Close(ctx context.Context, req model.SessionRequest, expectedVersion int) (model.SessionStatus, error) {
	if err := req.Validate(); err != nil {
		return model.SessionStatus{}, err
	}

	ctx, cancel := context.WithTimeout(ctx, s.limits.QueryTimeout)
	defer cancel()

	// A mutation never looks past CTX_SESSION_EXPIRED: an expired or already
	// closed session has no transition to make.
	if _, err := s.sessions.AdvanceSession(ctx, model.AdvanceRequest{
		SessionID:       req.SessionID,
		ActorID:         req.ActorID,
		Target:          model.StateClosed,
		ExpectedVersion: expectedVersion,
	}); err != nil {
		return model.SessionStatus{}, err
	}

	// Reporting the closed session is a read, and a just-closed session reads
	// as expired by definition, so the record is used rather than refused.
	rec, err := s.sessions.Session(ctx, req.SessionID, req.ActorID)
	if err != nil && !(expiredSession(err) && rec.ID != "") {
		return model.SessionStatus{}, err
	}
	return s.status(ctx, rec)
}

// status is the one place a model.SessionStatus is built from a session record.
// It spends exactly one CoverageSummary call for the counts -- paging Coverage
// for them would cost 1250 round trips on a 250k-file session -- and it leaves
// ReadyForImplementation and StrictGateSatisfied false, because Section 16.3's
// extra preconditions are Task 17 surface and a half-built readiness evaluator
// is a duplicate implementation. Owned by L3; called by L1, L4 and L5.
//
// ReadCompleteForSnapshot is the weaker, honest answer this task can give:
// every required_full file of this manifest is full_served for THIS actor's
// session at the pinned content hashes. A waiver is counted and reported but
// never folded into it -- a waiver never fabricates coverage.
//
// An incomplete manifest scope disqualifies it outright. Task 15 compiles an
// ambiguous or empty scope into a manifest with ScopeComplete=false and NO
// required_full entries, so the count test alone would report a session that
// resolved nothing as fully read.
func (s *Service) status(ctx context.Context, rec sqlite.SessionRecord) (model.SessionStatus, error) {
	if rec.ID == "" {
		return model.SessionStatus{}, typedErrf(model.CodeInternal,
			"coverage status was asked to describe a session that was never loaded")
	}
	required, fullyServed, waived, err := s.sessions.CoverageSummary(ctx, rec.ID, rec.ActorID)
	if err != nil && !expiredSession(err) {
		return model.SessionStatus{}, err
	}
	manifest, err := s.sessions.Manifest(ctx, rec.ManifestID)
	if err != nil {
		return model.SessionStatus{}, err
	}
	status := model.SessionStatus{
		SessionID:               rec.ID,
		ActorID:                 rec.ActorID,
		Binding:                 rec.Binding,
		ManifestID:              rec.ManifestID,
		Phase:                   rec.Phase,
		State:                   rec.State,
		StateVersion:            rec.StateVersion,
		ScopeVersion:            rec.ScopeVersion,
		ScopeComplete:           manifest.ScopeComplete,
		ReadCompleteForSnapshot: manifest.ScopeComplete && fullyServed == required,
		RequiredFiles:           required,
		FullyServedFiles:        fullyServed,
		WaivedFiles:             waived,
		CreatedAt:               rec.CreatedAt,
		ExpiresAt:               rec.ExpiresAt,
		// ReadyForImplementation and StrictGateSatisfied stay false: the verify
		// phase, a current-scope actor review, the absence of a blocking
		// unresolved dependency and current-source validation are Task 17's to
		// evaluate, and this task builds no readiness evaluator.
		//
		// Completeness and Superseded stay zero for the same reason of honesty:
		// no method on the Sessions surface reports per-capability completeness
		// or whether a newer generation has superseded this one, and inventing
		// either would be a claim nothing verified.
	}
	if err := status.Validate(); err != nil {
		return model.SessionStatus{}, err
	}
	return status, nil
}

// expiredSession reports whether err is the CTX_SESSION_EXPIRED that
// Store.Session and Store.Coverage return beside a partially populated record.
// It is errors.As rather than a type assertion because model.Canceled returns
// an errors.Join, which no assertion unwraps.
func expiredSession(err error) bool {
	var typed *model.Error
	return errors.As(err, &typed) && typed.Code == model.CodeSessionExpired
}

// cursorInvalid is the one rejection a bad continuation gets. Section 16.3 has
// no CTX_CURSOR_* family per endpoint, and the token is never echoed back.
func cursorInvalid(message string) *model.Error {
	return (&model.Error{Code: model.CodeCursorInvalid, Message: message}).WithDetail("endpoint", endpointStatus)
}
