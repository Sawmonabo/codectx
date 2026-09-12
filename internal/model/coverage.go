package model

import (
	"time"
	"unicode/utf8"
)

// CoverageState is how much of one file this actor's session has confirmed.
// full_served requires the union [0,size) for that exact session/file/hash
// (Section 16.3). Coverage proves delivery, never attention or comprehension.
type CoverageState string

const (
	CoverageUnserved      CoverageState = "unserved"
	CoveragePartialServed CoverageState = "partial_served"
	CoverageFullServed    CoverageState = "full_served"
)

// Valid reports whether s is a known wire spelling.
func (s CoverageState) Valid() bool {
	switch s {
	case CoverageUnserved, CoveragePartialServed, CoverageFullServed:
		return true
	}
	return false
}

// ChunkEncoding is how a source chunk is carried. Invalid UTF-8 is returned
// losslessly as base64 with byte-based ranges, never coerced into replacement
// characters (Section 16.2).
type ChunkEncoding string

const (
	EncodingUTF8   ChunkEncoding = "utf8"
	EncodingBase64 ChunkEncoding = "base64"
)

// Valid reports whether e is a known wire spelling.
func (e ChunkEncoding) Valid() bool {
	return e == EncodingUTF8 || e == EncodingBase64
}

// AcknowledgeKind distinguishes the two operations behind the acknowledgment
// surface. They are not interchangeable: a receipt confirms delivered bytes,
// while a file acknowledgment is a separately labeled client statement that
// requires full confirmed coverage first (Section 16.2).
type AcknowledgeKind string

const (
	AcknowledgeReceipt AcknowledgeKind = "receipt"
	AcknowledgeFile    AcknowledgeKind = "file"
)

// Valid reports whether k is a known wire spelling.
func (k AcknowledgeKind) Valid() bool {
	return k == AcknowledgeReceipt || k == AcknowledgeFile
}

// ReadChunkRequest asks for one bounded source chunk from the pinned snapshot.
// ConfirmReceipts echoes previously issued tokens so the client avoids an extra
// round trip; the final chunk still needs explicit confirmation.
//
// MaxBytes follows the PageRequest.Limit convention: zero means "the endpoint's
// configured default", so the caller need not know the deployment's policy. A
// nonzero value must be large enough to carry one UTF-8 code point, because
// Section 16.2 forbids a chunk that can never make progress, and must not
// exceed MaxRawChunkBytes.
type ReadChunkRequest struct {
	SessionID       SessionID `json:"session_id"`
	ActorID         string    `json:"actor_id"`
	FileID          FileID    `json:"file_id"`
	Offset          uint64    `json:"offset"`
	MaxBytes        uint32    `json:"max_bytes"`
	ConfirmReceipts []string  `json:"confirm_receipts,omitempty"`
}

// Validate enforces the request shape and the Section 16.2 receipt batch cap.
func (r ReadChunkRequest) Validate() error {
	if err := requireID("read_chunk.session_id", string(r.SessionID)); err != nil {
		return err
	}
	if err := requireTrimmed("read_chunk.actor_id", r.ActorID, MaxIdentifierBytes); err != nil {
		return err
	}
	if err := requireID("read_chunk.file_id", string(r.FileID)); err != nil {
		return err
	}
	if err := boundSigned64("read_chunk.offset", r.Offset); err != nil {
		return err
	}
	if r.MaxBytes != 0 {
		if r.MaxBytes < utf8.UTFMax {
			return invalid("read_chunk.max_bytes is %d, want 0 for the endpoint default or at least %d", r.MaxBytes, utf8.UTFMax)
		}
		if r.MaxBytes > MaxRawChunkBytes {
			return invalid("read_chunk.max_bytes is %d, want at most %d", r.MaxBytes, MaxRawChunkBytes)
		}
	}
	if err := boundStrings("read_chunk.confirm_receipts", r.ConfirmReceipts, MaxReceiptsPerConfirmation, MaxTokenBytes); err != nil {
		return err
	}
	return nil
}

// ReadChunkResponse carries the exact bytes plus the receipt that proves they
// were issued. Only this response and its capsule export carry source bodies.
type ReadChunkResponse struct {
	Binding     Binding       `json:"binding"`
	FileID      FileID        `json:"file_id"`
	ContentHash string        `json:"content_hash"`
	ByteRange   ByteRange     `json:"byte_range"`
	LineRange   SourceRange   `json:"line_range"`
	Encoding    ChunkEncoding `json:"encoding"`
	Content     string        `json:"content"`
	PartialLine bool          `json:"partial_line"`
	NextOffset  *uint64       `json:"next_offset,omitempty"`
	Receipt     string        `json:"receipt"`
	Coverage    CoverageState `json:"coverage"`
}

// Validate enforces the response shape. The byte range may be zero length: an
// empty file is served as a valid EOF response whose receipt must still be
// confirmed (Section 16.3).
func (r ReadChunkResponse) Validate() error {
	if err := r.Binding.Validate(); err != nil {
		return err
	}
	if err := requireID("read_chunk_response.file_id", string(r.FileID)); err != nil {
		return err
	}
	if err := requireID("read_chunk_response.content_hash", r.ContentHash); err != nil {
		return err
	}
	if err := r.ByteRange.Validate("read_chunk_response.byte_range"); err != nil {
		return err
	}
	if err := r.LineRange.Validate("read_chunk_response.line_range"); err != nil {
		return err
	}
	if !r.Encoding.Valid() {
		return invalid("read_chunk_response.encoding %q is not a known encoding", truncateForMessage(string(r.Encoding)))
	}
	// Section 20.2 requires the raw chunk plus its worst-case wire encoding to
	// fit the source response budget; without this bound a single response could
	// exceed the wire ceiling Section 16.2 fixes.
	if err := boundField("read_chunk_response.content", r.Content, MaxChunkContentBytes); err != nil {
		return err
	}
	if !r.Coverage.Valid() {
		return invalid("read_chunk_response.coverage %q is not a known coverage state", truncateForMessage(string(r.Coverage)))
	}
	if err := requireField("read_chunk_response.receipt", r.Receipt, MaxTokenBytes); err != nil {
		return err
	}
	if r.NextOffset != nil {
		if err := boundSigned64("read_chunk_response.next_offset", *r.NextOffset); err != nil {
			return err
		}
	}
	return nil
}

// AcknowledgeRequest confirms issued receipts or records a full-file client
// acknowledgment.
type AcknowledgeRequest struct {
	SessionID SessionID       `json:"session_id"`
	ActorID   string          `json:"actor_id"`
	Kind      AcknowledgeKind `json:"kind"`
	Receipts  []string        `json:"receipts,omitempty"`
	FileID    FileID          `json:"file_id,omitempty"`
}

// Validate enforces the kind-specific required fields and the receipt cap.
func (r AcknowledgeRequest) Validate() error {
	if err := requireID("acknowledge.session_id", string(r.SessionID)); err != nil {
		return err
	}
	if err := requireTrimmed("acknowledge.actor_id", r.ActorID, MaxIdentifierBytes); err != nil {
		return err
	}
	switch r.Kind {
	case AcknowledgeReceipt:
		if len(r.Receipts) == 0 {
			return invalid("acknowledge kind %q requires at least one receipt", AcknowledgeReceipt)
		}
		if err := boundStrings("acknowledge.receipts", r.Receipts, MaxReceiptsPerConfirmation, MaxTokenBytes); err != nil {
			return err
		}
		if r.FileID != "" {
			return invalid("acknowledge kind %q must not name a file_id; use kind %q for a file acknowledgment",
				AcknowledgeReceipt, AcknowledgeFile)
		}
	case AcknowledgeFile:
		if err := requireID("acknowledge.file_id", string(r.FileID)); err != nil {
			return err
		}
		if len(r.Receipts) != 0 {
			return invalid("acknowledge kind %q must not carry receipts; confirm them with kind %q first",
				AcknowledgeFile, AcknowledgeReceipt)
		}
	default:
		return invalid("acknowledge.kind %q is not a known acknowledgment kind", truncateForMessage(string(r.Kind)))
	}
	return nil
}

// FileCoverage is one file's honest coverage record: the pinned hash, its size,
// the confirmed served bytes and whether a waiver applies (Section 17.3).
// Waived is reported separately because a waiver never fabricates coverage.
//
// There is no path: it is derivable from FileID through the snapshot, and
// carrying a second spelling of the same identity would put a value in the
// capsule's canonical hash that the hash cannot verify. Requirement is kept
// beyond the Section 17.3 list because a capsule reader judging readiness needs
// to know whether an unserved file was required or merely offered.
type FileCoverage struct {
	FileID         FileID        `json:"file_id"`
	ContentHash    string        `json:"content_hash"`
	Size           int64         `json:"size"`
	ConfirmedBytes int64         `json:"confirmed_bytes"`
	Requirement    Requirement   `json:"requirement"`
	State          CoverageState `json:"state"`
	Waived         bool          `json:"waived"`
}

// Validate enforces the coverage record's shape and the rule that confirmed
// bytes can never exceed the stored source size (Section 16.3).
func (c FileCoverage) Validate() error {
	if err := requireID("file_coverage.file_id", string(c.FileID)); err != nil {
		return err
	}
	if err := requireID("file_coverage.content_hash", c.ContentHash); err != nil {
		return err
	}
	if err := requireNonNegative("file_coverage.size", c.Size); err != nil {
		return err
	}
	if err := requireNonNegative("file_coverage.confirmed_bytes", c.ConfirmedBytes); err != nil {
		return err
	}
	if c.ConfirmedBytes > c.Size {
		return invalid("file_coverage %q confirms %d bytes of a %d-byte file", c.FileID, c.ConfirmedBytes, c.Size)
	}
	if !c.Requirement.Valid() {
		return invalid("file_coverage.requirement %q is not a known requirement", truncateForMessage(string(c.Requirement)))
	}
	if !c.State.Valid() {
		return invalid("file_coverage.state %q is not a known coverage state", truncateForMessage(string(c.State)))
	}
	if c.State == CoverageFullServed && c.ConfirmedBytes != c.Size {
		return invalid("file_coverage %q claims full_served with %d of %d bytes confirmed",
			c.FileID, c.ConfirmedBytes, c.Size)
	}
	return nil
}

// WaiverRequest records an auditable exception for one required file. It never
// updates coverage and never makes ready_for_implementation true.
type WaiverRequest struct {
	SessionID SessionID `json:"session_id"`
	ActorID   string    `json:"actor_id"`
	FileID    FileID    `json:"file_id"`
	Reason    string    `json:"reason"`
}

// Validate enforces the coverage_waivers constraint that a reason is nonempty.
func (r WaiverRequest) Validate() error {
	if err := requireID("waiver.session_id", string(r.SessionID)); err != nil {
		return err
	}
	if err := requireTrimmed("waiver.actor_id", r.ActorID, MaxIdentifierBytes); err != nil {
		return err
	}
	if err := requireID("waiver.file_id", string(r.FileID)); err != nil {
		return err
	}
	if err := requireTrimmed("waiver.reason", r.Reason, MaxReasonBytes); err != nil {
		return err
	}
	return nil
}

// WaiverRecord is the stored waiver: "session/actor, required file, pinned
// content hash, nonempty reason and UTC timestamp" (Section 17.3). SessionID is
// implied by the record's owner and carried here so an exported capsule entry
// stands alone.
type WaiverRecord struct {
	SessionID   SessionID `json:"session_id"`
	ActorID     string    `json:"actor_id"`
	FileID      FileID    `json:"file_id"`
	ContentHash string    `json:"content_hash"`
	Reason      string    `json:"reason"`
	CreatedAt   time.Time `json:"created_at"`
}

// Validate enforces the stored waiver shape.
func (w WaiverRecord) Validate() error {
	if err := requireID("waiver_record.session_id", string(w.SessionID)); err != nil {
		return err
	}
	if err := requireTrimmed("waiver_record.actor_id", w.ActorID, MaxIdentifierBytes); err != nil {
		return err
	}
	if err := requireID("waiver_record.file_id", string(w.FileID)); err != nil {
		return err
	}
	if err := requireID("waiver_record.content_hash", w.ContentHash); err != nil {
		return err
	}
	if err := requireTrimmed("waiver_record.reason", w.Reason, MaxReasonBytes); err != nil {
		return err
	}
	return nil
}
