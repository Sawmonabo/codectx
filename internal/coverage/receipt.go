package coverage

import (
	"context"
	"encoding/json"
	"time"

	"github.com/Sawmonabo/codectx/internal/model"
	"github.com/Sawmonabo/codectx/internal/pagination"
	"github.com/Sawmonabo/codectx/internal/storage/sqlite"
)

// check reports why the payload is not a usable receipt, or "" when it is. It
// is deliberately one function shared by both directions: a payload the encoder
// would refuse to sign is a payload the decoder must refuse to honour, and two
// copies of the rule could disagree about what a receipt binds.
//
// The caller chooses the code, because the same defect means two different
// things: on the way out an incomplete payload is a wiring defect, on the way
// in it is a malformed token.
func (p receiptPayload) check() string {
	switch {
	case !model.ValidHexID(string(p.SessionID)):
		return "session id is malformed"
	case p.ActorID == "" || len(p.ActorID) > model.MaxIdentifierBytes:
		return "actor id is missing or too long"
	case !model.ValidHexID(string(p.SnapshotID)):
		return "snapshot id is malformed"
	case !model.ValidHexID(string(p.FileID)):
		return "file id is malformed"
	case !model.ValidHexID(p.ContentHash):
		return "content hash is malformed"
	case !model.ValidHexID(p.ChunkID):
		return "chunk id is malformed"
	}
	// The landed half-open range validator carries the signed-64 bound the
	// issued_chunks row is stored under; a zero-length EOF receipt is legal and
	// is exactly what an empty file's coverage depends on, so Validate, not
	// ValidateNonEmpty.
	if err := (model.ByteRange{Start: p.Start, End: p.End}).Validate("receipt.bytes"); err != nil {
		return err.Message
	}
	return ""
}

// encodeReceipt signs a payload with pagination.PurposeReceipt, mirroring
// Signer.EncodeCursor: the same JSON framing, the same second-truncated UTC
// expiry, and no new method on Signer. Signer.Sign enforces the token size
// bound, so this never re-checks it.
func (s *Service) encodeReceipt(p receiptPayload, expires time.Time) (string, error) {
	if why := p.check(); why != "" {
		return "", typedErrf(model.CodeInternal, "receipt payload is incomplete: %s", why)
	}
	if expires.IsZero() {
		return "", typedErrf(model.CodeInternal, "receipt payload has no expiry")
	}
	// Truncating to the second matches the signer's header, which stores unix
	// seconds: an untruncated expiry would round down and expire the token
	// marginally earlier than the issued chunk it names.
	expires = expires.UTC().Truncate(time.Second)
	payload, err := json.Marshal(p)
	if err != nil {
		return "", typedErrf(model.CodeInternal, "receipt encoding: %s", err.Error())
	}
	return s.signer.Sign(pagination.PurposeReceipt, payload, expires)
}

// decodeReceipt verifies a token with pagination.PurposeReceipt, never
// PurposeCursor: Section 16.3's rule that cursor tokens and source receipts are
// never accepted interchangeably is enforced by that argument alone, since
// Signer.Verify rejects a mismatched purpose. Every rejection is
// CTX_CURSOR_INVALID -- there is no CTX_RECEIPT_* family -- and the token is
// never echoed back in the message.
func (s *Service) decodeReceipt(token string, now time.Time) (receiptPayload, error) {
	payload, err := s.signer.Verify(token, pagination.PurposeReceipt, now)
	if err != nil {
		return receiptPayload{}, err
	}
	var p receiptPayload
	if err := json.Unmarshal(payload, &p); err != nil {
		return receiptPayload{}, typedErrf(model.CodeCursorInvalid, "receipt payload is malformed")
	}
	if why := p.check(); why != "" {
		return receiptPayload{}, typedErrf(model.CodeCursorInvalid, "receipt payload is malformed: %s", why)
	}
	return p, nil
}

// confirmReceipts is the single confirmation path, shared by Acknowledge with
// kind "receipt" and by Read's ConfirmReceipts echo. It decodes every token,
// cross-checks each payload against the session record the caller has already
// gated, and then makes exactly ONE ConfirmChunks call with the raw 64-hex
// chunk ids the payloads name.
//
// Everything that happens to those ids afterwards -- marking them confirmed,
// merging intervals with overlap and adjacency, the [0,size) union test and the
// separate zero-length EOF bit -- belongs to ConfirmChunks and fileCoverage.
// Reproducing any of it here would be a second implementation of coverage.
//
// An empty token list confirms nothing and is not an error: Read echoes
// whatever the client sent, which is usually nothing, and the store rejects an
// empty batch.
func (s *Service) confirmReceipts(ctx context.Context, rec sqlite.SessionRecord, tokens []string) error {
	if len(tokens) == 0 {
		return nil
	}
	// Zero means the configured default, never unlimited.
	batch := s.limits.MaxReceiptsPerConfirmation
	if batch <= 0 {
		batch = model.MaxReceiptsPerConfirmation
	}
	if len(tokens) > batch {
		return typedErrf(model.CodeResourceLimit,
			"a confirmation carries at most %d receipts, got %d", batch, len(tokens))
	}

	now := s.now()
	ids := make([]string, 0, len(tokens))
	for _, token := range tokens {
		p, err := s.decodeReceipt(token, now)
		if err != nil {
			return err
		}
		// Every field the session record actually carries is checked before a
		// single chunk is confirmed, so a receipt issued to another actor,
		// another session or another snapshot is refused rather than shared.
		// The file and the content hash are not re-checked against the record,
		// which carries neither: the signature binds them to this chunk id, and
		// IssueChunk already proved the pair was in the pinned scope. All three
		// rejections carry CTX_CURSOR_INVALID; reporting an actor mismatch
		// separately would tell the caller that another actor's token exists.
		if p.SessionID != rec.ID || p.ActorID != rec.ActorID || p.SnapshotID != rec.Binding.SnapshotID {
			return typedErrf(model.CodeCursorInvalid, "receipt was not issued for this session")
		}
		ids = append(ids, p.ChunkID)
	}
	// One call: an unknown, expired or foreign chunk fails the whole batch
	// there, as CTX_CURSOR_INVALID, and a chunk already confirmed is idempotent.
	return s.sessions.ConfirmChunks(ctx, rec.ID, rec.ActorID, ids)
}

// Acknowledge confirms echoed receipts or records a full-file client
// assertion. The two kinds are never interchangeable: a receipt confirms
// delivered bytes, while a file acknowledgment asserts a human-visible review
// of a file that is already fully served and creates no coverage of its own --
// AcknowledgeFile refuses with CTX_COVERAGE_INCOMPLETE otherwise.
//
// The session is loaded through the actor-checked Session first, so neither
// branch touches session state the request's actor does not own. An expired or
// closed session fails here: only the reporting paths carry on beside
// CTX_SESSION_EXPIRED, and a confirmation is a mutation.
func (s *Service) Acknowledge(ctx context.Context, req model.AcknowledgeRequest) (model.SessionStatus, error) {
	if err := req.Validate(); err != nil {
		return model.SessionStatus{}, err
	}

	ctx, cancel := context.WithTimeout(ctx, s.limits.QueryTimeout)
	defer cancel()

	rec, err := s.sessions.Session(ctx, req.SessionID, req.ActorID)
	if err != nil {
		return model.SessionStatus{}, err
	}
	// Validate admits exactly the two kinds, so there is no third branch to
	// write and no unreachable default to leave behind.
	if req.Kind == model.AcknowledgeReceipt {
		err = s.confirmReceipts(ctx, rec, req.Receipts)
	} else {
		err = s.sessions.AcknowledgeFile(ctx, req.SessionID, req.ActorID, req.FileID)
	}
	if err != nil {
		return model.SessionStatus{}, err
	}
	// The status is read after the confirmation, so it reports the coverage this
	// call just granted rather than the state before it.
	return s.status(ctx, rec)
}
