// Package coverage serves snapshot-pinned source in bounded chunks and keeps
// actor-scoped, receipt-confirmed coverage of what was actually delivered
// (Sections 16.1-16.3). It is a service and wiring layer over work that has
// already landed: internal/source plans every chunk boundary, internal/snapshot
// verifies and positions every range read, and internal/storage/sqlite owns
// session state, issued chunks, interval merging and the coverage state switch.
//
// Nothing here re-implements any of that. There is no Go-side interval merge,
// no second coverage-state switch and no second chunk planner; a chunk earns
// credit only when its signed receipt is echoed back and ConfirmChunks accepts
// it, so a broken pipe or a failed serialization can never grant coverage.
//
// This package imports neither internal/context nor internal/config nor
// *sqlite.Store's wider surface: internal/app supplies the frozen Sessions and
// Source interfaces and a Limits resolved from configuration.
package coverage

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"time"

	"github.com/Sawmonabo/codectx/internal/model"
	"github.com/Sawmonabo/codectx/internal/pagination"
	"github.com/Sawmonabo/codectx/internal/snapshot"
	"github.com/Sawmonabo/codectx/internal/storage/sqlite"
)

// Sessions is the only storage surface this package may use. *sqlite.Store
// satisfies it; the manifest readers are Task 15's and are consumed read-only,
// so no method here is defined or redefined by this package except
// CoverageSummary, which is a coverage aggregate owned by this task.
type Sessions interface {
	// Manifest reads belong to Task 15's MANIFEST lane. Entries persist in
	// Section 15.3 tie-break order, ordinals 0..n-1 are the canonical reading
	// order, and required_full entries occupy a prefix: Next walks ascending
	// from the first unserved required entry and stops at the end of that
	// prefix. It never sorts and never re-ranks.
	Manifest(ctx context.Context, id model.ManifestID) (model.ContextManifest, error)
	ManifestEntries(ctx context.Context, id model.ManifestID, afterOrdinal int, limit int) ([]model.ContextEntry, error)
	ManifestSlices(ctx context.Context, id model.ManifestID, afterIndex int, limit int) ([]model.ContextSlice, error)
	ManifestExcluded(ctx context.Context, id model.ManifestID, afterOrdinal int, limit int) ([]model.ExcludedContextEntry, error)

	// OpenSession already resolves the manifest's generation, snapshot and
	// phase, rejects a phase that cannot open a session, enforces the
	// idempotency key per actor and populates session_files from the manifest.
	OpenSession(ctx context.Context, open model.SessionOpen) (model.SessionID, error)
	// Session checks exact actor identity and rejects a wrong actor, an expired
	// session and a closed session on every mutation -- but it skips the actor
	// check when actor is empty, so every caller Validate()s its request first.
	// It deliberately returns a partially populated record beside
	// CTX_SESSION_EXPIRED rather than swallowing the record.
	Session(ctx context.Context, id model.SessionID, actor string) (sqlite.SessionRecord, error)
	// IssueChunk verifies that file+hash is in the pinned scope and refuses a
	// chunk past the stored size. A zero-length EOF chunk is legal and required.
	IssueChunk(ctx context.Context, c model.IssuedChunk) error
	// UnconfirmedChunks takes no actor and loads no session, so it is only ever
	// called after Session has gated the actor; calling it first would leak a
	// cross-actor count.
	UnconfirmedChunks(ctx context.Context, session model.SessionID) (int64, error)
	// ConfirmChunks takes raw 64-hex chunk ids, not receipt tokens. It marks
	// them confirmed and merges intervals with overlap and adjacency into
	// served_ranges in one transaction, is idempotent on an already-confirmed
	// chunk, and fails the whole batch on an unknown, expired or foreign chunk.
	ConfirmChunks(ctx context.Context, session model.SessionID, actor string, ids []string) error
	// Coverage pages one actor's per-file coverage by keyset on file_id. Like
	// Session it reports honestly beside CTX_SESSION_EXPIRED.
	Coverage(ctx context.Context, session model.SessionID, actor string, after model.FileID, limit int) ([]model.FileCoverage, error)
	// CoverageSummary is the one storage addition of this task (L6): the counts
	// Status needs in a single round trip instead of paging Coverage. Task 17
	// consumes the same method and must not define a second one.
	CoverageSummary(ctx context.Context, session model.SessionID, actor string) (required, fullyServed, waived int64, err error)
	// AcknowledgeFile refuses unless the file is already full_served; it never
	// creates coverage.
	AcknowledgeFile(ctx context.Context, session model.SessionID, actor string, file model.FileID) error
	// AdvanceSession is how Close is expressed: AdvanceRequest{Target:
	// StateClosed, ExpectedVersion: n}. There is no CloseSession and no edge out
	// of complete, so closing a completed session is CTX_VERSION_CONFLICT.
	AdvanceSession(ctx context.Context, req model.AdvanceRequest) (model.WorkflowStatus, error)
}

// Source is the per-snapshot verified read surface. *snapshot.View satisfies
// it. Read seeks to the nearest stored checkpoint, verifies exactly the CAS
// blocks it touches and returns bytes with line/column positions, rejecting a
// boundary inside a UTF-8 sequence and a range past the file.
//
// Range.Bytes aliases the checkpoint-prefixed window the view read, so a caller
// that retains the slice must copy it first.
type Source interface {
	Read(ctx context.Context, id model.FileID, r model.ByteRange) (snapshot.Range, model.FileVersion, error)
	// Checkpoint reports where Read will actually begin: the byte of the nearest
	// stored line checkpoint at or before offset. maxRawForWire needs the prefix
	// offset - Checkpoint(offset) before it can choose a raw size, and the prefix
	// cannot be assumed from the checkpoint spacing -- a checkpoint starts a
	// line, so on a minified file the nearest one may be byte zero.
	Checkpoint(ctx context.Context, id model.FileID, offset uint64) (uint64, error)
}

var _ Source = (*snapshot.View)(nil)

// SourceOpener yields the Source for one pinned snapshot. internal/app owns the
// *snapshot.View behind it and memoises per SnapshotID, which is what keeps
// *sqlite.Store, *snapshot.CAS and config.Config out of this package.
// The store is the production Sessions; the assertion keeps the interface
// honest against it.
var _ Sessions = (*sqlite.Store)(nil)

type SourceOpener func(ctx context.Context, snap model.SnapshotID) (Source, error)

// Options composes the service. Every dependency is required: the composition
// root builds this eagerly when a workspace opens, so a missing one is a wiring
// defect that must fail there rather than per request.
type Options struct {
	Sessions   Sessions
	OpenSource SourceOpener
	Signer     *pagination.Signer
	Leases     *pagination.Leases
	Limits     Limits
	Now        func() time.Time
	Logger     *slog.Logger
}

// Limits are the Section 20.1 bounds resolved from configuration by the caller.
// This package never reads config.Config, so every bound it enforces is here.
// Zero on a request field means the configured default, never unlimited.
type Limits struct {
	// ChunkBytes is the default raw chunk size (coverage.chunk_bytes, 64 KiB)
	// and MaxChunkBytes its ceiling (coverage.max_chunk_bytes, 1 MiB). A request
	// above the ceiling is clamped, not rejected.
	ChunkBytes, MaxChunkBytes int64
	// MaxSourceResponseBytes bounds a read response on the wire; only the read
	// path may spend it. MaxMetadataResponseBytes bounds Next, Status and
	// Acknowledge, which are generic tools and carry no source bytes.
	MaxSourceResponseBytes   int64
	MaxMetadataResponseBytes int64

	MaxReceiptsPerConfirmation     int
	MaxUnconfirmedChunksPerSession int
	MaxPageItems                   int

	// SessionTTL is coverage.session_ttl and also the retention lease duration.
	// ReceiptTTL bounds how long an issued receipt may be echoed back.
	SessionTTL, QueryTimeout, ReceiptTTL time.Duration
}

// Service answers the six Section 16 operations. It is safe for concurrent use:
// every field is read-only after New and all per-request state lives on the
// stack of the call that made it.
type Service struct {
	sessions Sessions
	open     SourceOpener
	signer   *pagination.Signer
	leases   *pagination.Leases
	limits   Limits
	now      func() time.Time
	log      *slog.Logger
}

// Next names the next required_full file this actor has not fully served, in
// manifest ordinal order, with its pinned hash, size and resume offset. It is
// metadata only and never carries source bytes. Implemented in next.go (L4).

// receiptPayload is what a source receipt binds. Every field is checked against
// the live session before ConfirmChunks runs, so a payload naming another
// actor, session, file or content hash is rejected rather than shared.
type receiptPayload struct {
	SessionID   model.SessionID
	ActorID     string
	SnapshotID  model.SnapshotID
	FileID      model.FileID
	ContentHash string
	Start, End  uint64
	ChunkID     string
}

// sourceEnvelopeBytes is the headroom reserved for everything in a read
// response that is not the encoded chunk: the JSON envelope, the binding, both
// ranges, the receipt token (up to model.MaxTokenBytes) and the field names.
// It is deliberately generous, because under-reserving it would let a response
// exceed resources.max_source_response_bytes, which Section 16.2 forbids.
const sourceEnvelopeBytes = 4096

// maxRawForWire is the single chunk-size arithmetic in this package. Section
// 16.2 requires the raw size to be reduced *before* bytes are emitted, so the
// slicer and the serializer must agree; recomputing any part of this elsewhere
// is a defect. It is L0's one implemented function.
//
// The result is the largest raw byte count that satisfies all four bounds:
//
//   - the request: requested, or Limits.ChunkBytes when requested is zero
//     (zero means the configured default, never unlimited);
//   - the policy ceiling: Limits.MaxChunkBytes and model.MaxRawChunkBytes. A
//     request above the ceiling is clamped, not rejected;
//   - the checkpoint prefix: snapshot.View.Read asks CAS.ReadRange for
//     [checkpoint, end), and CAS.ReadRange rejects a span over
//     model.MaxRawChunkBytes. Checkpoint spacing is 64 KiB, so a maximum-size
//     chunk that does not start on a checkpoint fails unless the prefix is
//     subtracted here. checkpointPrefix is offset minus Source.Checkpoint at
//     that offset; it is never assumed from the checkpoint spacing, which
//     bounds nothing on a file without line breaks;
//   - the wire budget: the worst-case base64 encoding 4*((raw+2)/3) plus
//     sourceEnvelopeBytes must fit Limits.MaxSourceResponseBytes. With
//     budget = MaxSourceResponseBytes - sourceEnvelopeBytes and k = budget/4
//     rounded down, 4*((raw+2)/3) <= budget holds exactly when raw <= 3*k:
//     (3k+2)/3 is k, while (3k+1+2)/3 is k+1 and overshoots.
//
// offset bounds the result too: a chunk may not run past the signed 64-bit
// range SQLite stores, which model.ByteRange.Validate enforces on the issued
// row. It is the read coordinate, not a diagnostic; callers still clamp the
// range to the file size, which this function does not know.
//
// It returns a cap only. It never errors on a small result and never inspects
// EOF: a zero-length chunk at EOF is legal and required, and L1 issues it
// without branching on an error here. The error return is reserved for a
// misconfigured Limits or an impossible prefix, both of which are wiring
// defects rather than user-correctable input, so they are CTX_INTERNAL.
func maxRawForWire(requested uint32, offset, checkpointPrefix uint64, l Limits) (uint32, error) {
	raw := int64(requested)
	if raw <= 0 {
		raw = l.ChunkBytes
	}
	if l.ChunkBytes <= 0 || l.MaxChunkBytes <= 0 || l.MaxSourceResponseBytes <= 0 {
		return 0, notImplementedf(model.CodeInternal,
			"coverage limits are not configured: chunk_bytes=%d max_chunk_bytes=%d max_source_response_bytes=%d",
			l.ChunkBytes, l.MaxChunkBytes, l.MaxSourceResponseBytes)
	}
	if raw > l.MaxChunkBytes {
		raw = l.MaxChunkBytes
	}
	if raw > model.MaxRawChunkBytes {
		raw = model.MaxRawChunkBytes
	}

	// The checkpoint prefix is read and verified along with the chunk, so it
	// spends the same CAS.ReadRange budget.
	if checkpointPrefix >= model.MaxRawChunkBytes {
		return 0, notImplementedf(model.CodeInternal,
			"checkpoint prefix %d at offset %d is not smaller than the %d-byte range ceiling",
			checkpointPrefix, offset, model.MaxRawChunkBytes)
	}
	if room := int64(model.MaxRawChunkBytes) - int64(checkpointPrefix); raw > room {
		raw = room
	}

	// Worst-case wire encoding: 4*((raw+2)/3) + sourceEnvelopeBytes.
	budget := l.MaxSourceResponseBytes - sourceEnvelopeBytes
	if budget <= 0 {
		return 0, notImplementedf(model.CodeInternal,
			"max_source_response_bytes %d leaves no room for the %d-byte response envelope",
			l.MaxSourceResponseBytes, sourceEnvelopeBytes)
	}
	if wire := 3 * (budget / 4); raw > wire {
		raw = wire
	}

	// A chunk may not run past the signed 64-bit byte range SQLite stores.
	if offset > uint64(math.MaxInt64) {
		return 0, notImplementedf(model.CodeInternal, "read offset %d is past the storable byte range", offset)
	}
	if room := math.MaxInt64 - int64(offset); raw > room {
		raw = room
	}

	if raw < 0 {
		raw = 0
	}
	return uint32(raw), nil
}

// The model.NextContextItem.Action vocabulary. internal/model/context.go
// assigns this set to Task 16 and deliberately leaves the model field a bounded
// free-form string, so Task 17 can add workflow actions without editing a
// frozen file. Owned by L4.
const (
	// actionReadSource: a required file still has unserved bytes; read them.
	actionReadSource = "read_source"
	// actionAcknowledgeReceipt: every required byte has been issued but a
	// receipt is still outstanding, so no credit has been granted yet.
	actionAcknowledgeReceipt = "acknowledge_receipt"
	// actionReviewScope: nothing is readable because the manifest's scope is
	// incomplete; the actor must review it rather than read.
	actionReviewScope = "review_scope"
	// actionComplete: no required file remains unserved for this actor.
	actionComplete = "complete"
)

// errNoSource reports that the manifest's required_full prefix is exhausted for
// this actor. It is a control signal inside Next, never a failure returned to a
// caller: Next answers actionComplete instead.
var errNoSource = errors.New("no unserved required file remains")

// notImplemented is the typed stub every unimplemented body returns. It is
// CTX_INTERNAL because reaching one is a wiring defect, not user-correctable
// input, and it is an error rather than a panic so a partially wired build
// fails honestly instead of crashing the process.
func notImplemented(op string) *model.Error {
	return notImplementedf(model.CodeInternal, "%s is not implemented", op)
}

// notImplementedf builds a typed model.Error without leaking source bytes,
// receipt tokens or the dependence engine's name into the message.
func notImplementedf(code, format string, args ...any) *model.Error {
	return &model.Error{Code: code, Message: fmt.Sprintf(format, args...)}
}

// --- Also frozen by L0; implemented elsewhere -------------------------------
//
// These signatures live in files this package does not own. They are recorded
// here so no fill-in lane has to invent a shared name, and no lane may change
// one without asking the controller.
//
//	// internal/app/compose.go -- openQueries-style construction. Deliberately
//	// not a raw Store() accessor, which would leak the whole storage surface.
//	func (s *stack) openCoverage() error
//
//	// internal/app/workspace.go -- the SourceOpener closes over the stack's
//	// store (as snapshot.Catalog) and CAS via snapshot.OpenView, memoised per
//	// snapshot. Added by INT.
//	func (w *Workspace) Coverage() *coverage.Service
//
//	// internal/storage/sqlite/state.go -- appended by L6, one aggregate query
//	// reproducing both branches of the fileCoverage state switch (union ==
//	// size, and the separate zero-length confirmed-EOF branch). Task 15 must
//	// not define it; Task 17 consumes it and must not define a second one.
//	func (s *Store) CoverageSummary(ctx context.Context, session model.SessionID, actor string) (required, fullyServed, waived int64, err error)
//
//	// internal/cli/context.go -- written by L5 with the Section 18.1 spellings
//	// context read/acknowledge/status/next/close. INT adds one loop line to
//	// root.go that registers them.
//	func newContextCommands(build model.BuildInfo) []*cobra.Command
