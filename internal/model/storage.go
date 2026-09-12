package model

import (
	"crypto/rand"
	"encoding/hex"
	"sort"
	"time"
)

// Storage-facing records: the inputs the store needs that no public request or
// result carries. They live here so internal/snapshot, internal/index and
// internal/pagination can hand them to internal/storage without any of those
// packages importing another (Section 7.1).

// BlobBlockBytes is the fixed CAS block size of Section 10.3. Block digests are
// indexed metadata bound to the whole-file digest; range serving verifies every
// block it exposes without rehashing the file.
const BlobBlockBytes = 65536

// MaxDependenciesPerUnit bounds a unit's declared dependency list. It reuses
// the generic bounded-list ceiling rather than inventing a new one.
const MaxDependenciesPerUnit = MaxRecordsPerResult

// Hash domains for the aggregate digests that feed UnitSpec.
const (
	domainUnitInputs = "unit-inputs-v1"
	domainUnitDeps   = "unit-deps-v1"
)

// LineCheckpoint is one sparse line index entry for a blob: the line that
// contains ByteOffset starts at LineStartByte and is LineNumber (one-based).
type LineCheckpoint struct {
	ByteOffset    uint64 `json:"byte_offset"`
	LineNumber    uint32 `json:"line_number"`
	LineStartByte uint64 `json:"line_start_byte"`
}

// BlobRecord is the metadata the CAS persists for one retained object: its
// whole-file SHA-256, size, one digest per BlobBlockBytes block and sparse line
// checkpoints (Section 10.3).
type BlobRecord struct {
	Hash            string           `json:"hash"`
	Size            int64            `json:"size"`
	BlockDigests    []string         `json:"block_digests"`
	LineCheckpoints []LineCheckpoint `json:"line_checkpoints,omitempty"`
}

// Validate enforces the blobs, blob_blocks and line_checkpoints constraints and
// the arithmetic tying block count to size.
func (b BlobRecord) Validate() error {
	if err := requireID("blob.hash", b.Hash); err != nil {
		return err
	}
	if err := requireNonNegative("blob.size", b.Size); err != nil {
		return err
	}
	wantBlocks := int((b.Size + BlobBlockBytes - 1) / BlobBlockBytes)
	if len(b.BlockDigests) != wantBlocks {
		return invalid("blob.block_digests has %d entries for a %d-byte blob, want %d", len(b.BlockDigests), b.Size, wantBlocks)
	}
	for i, d := range b.BlockDigests {
		if err := requireID(indexed("blob.block_digests", i), d); err != nil {
			return err
		}
	}
	var prev uint64
	for i, c := range b.LineCheckpoints {
		field := indexed("blob.line_checkpoints", i)
		if err := boundSigned64(field+".byte_offset", c.ByteOffset); err != nil {
			return err
		}
		if c.ByteOffset > uint64(b.Size) {
			return invalid("%s.byte_offset %d is past the %d-byte blob", field, c.ByteOffset, b.Size)
		}
		if i > 0 && c.ByteOffset <= prev {
			return invalid("%s.byte_offset %d is not after the previous checkpoint %d", field, c.ByteOffset, prev)
		}
		prev = c.ByteOffset
		if c.LineNumber < 1 {
			return invalid("%s.line_number is %d; line numbers are one-based", field, c.LineNumber)
		}
		if c.LineStartByte > c.ByteOffset {
			return invalid("%s.line_start_byte %d is after byte_offset %d", field, c.LineStartByte, c.ByteOffset)
		}
	}
	return nil
}

// UnitInput is one source file a unit read: the unit_inputs row. Executable is
// part of the input because a mode change can alter analysis (Section 9.4).
type UnitInput struct {
	FileID      FileID `json:"file_id"`
	ContentHash string `json:"content_hash"`
	Executable  bool   `json:"executable"`
}

// Validate enforces the unit_inputs constraints.
func (i UnitInput) Validate() error {
	if err := requireID("unit_input.file_id", string(i.FileID)); err != nil {
		return err
	}
	if err := requireID("unit_input.content_hash", i.ContentHash); err != nil {
		return err
	}
	return nil
}

// UnitInputHasher folds a unit's inputs into UnitSpec.InputHash one at a time,
// so a workspace-scoped unit never materializes its input list. Inputs must be
// added in strictly ascending FileID order, which is what makes the digest
// canonical regardless of discovery order.
type UnitInputHasher struct {
	h    *Hasher
	last FileID
	n    int
}

// NewUnitInputHasher starts an input digest.
func NewUnitInputHasher() *UnitInputHasher {
	return &UnitInputHasher{h: NewHasher(domainUnitInputs)}
}

// Add folds one input. It rejects an out-of-order or repeated FileID.
func (u *UnitInputHasher) Add(in UnitInput) error {
	if err := in.Validate(); err != nil {
		return err
	}
	if u.n > 0 && in.FileID <= u.last {
		return invalid("unit input %q is not after %q; inputs must be added in ascending file_id order", in.FileID, u.last)
	}
	u.last = in.FileID
	u.n++
	u.h.AddString(string(in.FileID))
	u.h.AddString(in.ContentHash)
	if in.Executable {
		u.h.AddString("1")
	} else {
		u.h.AddString("0")
	}
	return nil
}

// Sum returns the input digest folded so far.
func (u *UnitInputHasher) Sum() string { return u.h.Sum() }

// DependencyHash is UnitSpec.DependencyHash: the digest of the canonically
// sorted dependency unit keys. An empty list has a well-defined digest.
func DependencyHash(deps []UnitID) string {
	keys := make([]string, 0, len(deps))
	for _, d := range deps {
		keys = append(keys, string(d))
	}
	sort.Strings(keys)
	h := NewHasher(domainUnitDeps)
	for _, k := range keys {
		h.AddString(k)
	}
	return h.Sum()
}

// UnitBuild is everything the store needs to open one immutable unit: the
// spec, the analysis configuration digest that completes its identity, the run
// producing it, its source binding and its declared dependencies. Inputs are
// streamed separately so a large unit never holds its file list in memory.
type UnitBuild struct {
	Spec               UnitSpec      `json:"spec"`
	AnalysisConfigHash string        `json:"analysis_config_hash"`
	OriginRunID        ProviderRunID `json:"origin_run_id"`
	SourceBinding      SourceBinding `json:"source_binding"`
	Dependencies       []UnitID      `json:"dependencies,omitempty"`
}

// Validate enforces the units table constraints and recomputes identity: the
// spec's ID must be NewUnitID over these fields and its DependencyHash must be
// the digest of the declared dependencies, so a producer cannot present a unit
// under a key its inputs do not justify (Section 13.1).
func (b UnitBuild) Validate() error {
	if err := b.Spec.Validate(); err != nil {
		return err
	}
	if err := requireField("unit_build.analysis_config_hash", b.AnalysisConfigHash, MaxIdentifierBytes); err != nil {
		return err
	}
	if err := requireID("unit_build.origin_run_id", string(b.OriginRunID)); err != nil {
		return err
	}
	if !b.SourceBinding.Valid() {
		return invalid("unit_build.source_binding %q is not a known source binding", truncateForMessage(string(b.SourceBinding)))
	}
	if err := boundCount("unit_build.dependencies", len(b.Dependencies), MaxDependenciesPerUnit); err != nil {
		return err
	}
	seen := make(map[UnitID]bool, len(b.Dependencies))
	for i, d := range b.Dependencies {
		if err := requireID(indexed("unit_build.dependencies", i), string(d)); err != nil {
			return err
		}
		if d == b.Spec.ID {
			return invalid("unit %q declares itself as a dependency", b.Spec.ID)
		}
		if seen[d] {
			return invalid("unit_build.dependencies repeats %q", d)
		}
		seen[d] = true
	}
	if want := DependencyHash(b.Dependencies); b.Spec.DependencyHash != want {
		return invalid("unit_spec.dependency_hash %q does not match the declared dependencies (%q)", b.Spec.DependencyHash, want)
	}
	if want := NewUnitID(b.Spec, b.AnalysisConfigHash); b.Spec.ID != want {
		return invalid("unit_spec.id %q does not match the identity derived from its fields (%q)", b.Spec.ID, want)
	}
	return nil
}

// Lease is one retention_leases row: an owner's promise that the generation
// and/or snapshot it names must survive collection until ExpiresAt.
type Lease struct {
	ID           string         `json:"id"`
	GenerationID GenerationID   `json:"generation_id,omitempty"`
	SnapshotID   SnapshotID     `json:"snapshot_id,omitempty"`
	OwnerKind    LeaseOwnerKind `json:"owner_kind"`
	ExpiresAt    time.Time      `json:"expires_at"`
}

// Validate enforces the retention_leases constraints.
func (l Lease) Validate() error {
	if err := requireID("lease.id", l.ID); err != nil {
		return err
	}
	if err := requireNonNegative("lease.generation_id", int64(l.GenerationID)); err != nil {
		return err
	}
	if err := optionalID("lease.snapshot_id", string(l.SnapshotID)); err != nil {
		return err
	}
	if l.GenerationID == 0 && l.SnapshotID == "" {
		return invalid("lease names neither a generation nor a snapshot")
	}
	if !l.OwnerKind.Valid() {
		return invalid("lease.owner_kind %q is not a known lease owner", truncateForMessage(string(l.OwnerKind)))
	}
	if l.ExpiresAt.IsZero() {
		return invalid("lease.expires_at is required")
	}
	return nil
}

// ProviderRun is one provider_runs row as read back for status and doctor.
// GenerationID is zero once the creating generation has been deleted; the run
// itself outlives it while a retained unit still names it as origin.
type ProviderRun struct {
	ID              ProviderRunID `json:"id"`
	GenerationID    GenerationID  `json:"generation_id,omitempty"`
	ProviderID      string        `json:"provider_id"`
	ProviderVersion string        `json:"provider_version"`
	State           RunState      `json:"state"`
	RecordsEmitted  uint64        `json:"records_emitted"`
	BytesProcessed  uint64        `json:"bytes_processed"`
	DiagnosticCode  string        `json:"diagnostic_code,omitempty"`
	StartedAt       time.Time     `json:"started_at"`
	CompletedAt     *time.Time    `json:"completed_at,omitempty"`
}

// SessionOpen is the read_sessions row the workflow service creates. The
// session's generation, snapshot and initial state come from its manifest.
type SessionOpen struct {
	ID              SessionID  `json:"id"`
	ActorID         string     `json:"actor_id"`
	IdempotencyKey  string     `json:"idempotency_key,omitempty"`
	OpenRequestHash string     `json:"open_request_hash"`
	ManifestID      ManifestID `json:"manifest_id"`
	ExpiresAt       time.Time  `json:"expires_at"`
}

// Validate enforces the read_sessions constraints.
func (s SessionOpen) Validate() error {
	if err := requireID("session_open.id", string(s.ID)); err != nil {
		return err
	}
	if err := requireTrimmed("session_open.actor_id", s.ActorID, MaxIdentifierBytes); err != nil {
		return err
	}
	if s.IdempotencyKey != "" {
		if err := requireTrimmed("session_open.idempotency_key", s.IdempotencyKey, MaxIdentifierBytes); err != nil {
			return err
		}
	}
	if err := requireID("session_open.open_request_hash", s.OpenRequestHash); err != nil {
		return err
	}
	if err := requireID("session_open.manifest_id", string(s.ManifestID)); err != nil {
		return err
	}
	if s.ExpiresAt.IsZero() {
		return invalid("session_open.expires_at is required")
	}
	return nil
}

// IssuedChunk is one issued_chunks row: a source chunk handed to a client whose
// receipt has not yet been echoed. Coverage is credited only on confirmation.
type IssuedChunk struct {
	ID          string    `json:"id"`
	SessionID   SessionID `json:"session_id"`
	ActorID     string    `json:"actor_id"`
	FileID      FileID    `json:"file_id"`
	ContentHash string    `json:"content_hash"`
	Bytes       ByteRange `json:"bytes"`
	ExpiresAt   time.Time `json:"expires_at"`
}

// Validate enforces the issued_chunks constraints; a zero-length EOF chunk is
// legal (Section 16.3).
func (c IssuedChunk) Validate() error {
	if err := requireID("issued_chunk.id", c.ID); err != nil {
		return err
	}
	if err := requireID("issued_chunk.session_id", string(c.SessionID)); err != nil {
		return err
	}
	if err := requireField("issued_chunk.actor_id", c.ActorID, MaxIdentifierBytes); err != nil {
		return err
	}
	if err := requireID("issued_chunk.file_id", string(c.FileID)); err != nil {
		return err
	}
	if err := requireID("issued_chunk.content_hash", c.ContentHash); err != nil {
		return err
	}
	if err := c.Bytes.Validate("issued_chunk.bytes"); err != nil {
		return err
	}
	if c.ExpiresAt.IsZero() {
		return invalid("issued_chunk.expires_at is required")
	}
	return nil
}

// NewRandomID returns a fresh operational identifier: 32 cryptographically
// random bytes as lowercase hex, the same wire shape as a content-derived ID
// but never a semantic hash. Session, run, lease and chunk IDs use it.
func NewRandomID() (string, error) {
	var raw [32]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", &Error{Code: CodeInternal, Message: "random identifier: " + err.Error()}
	}
	return hex.EncodeToString(raw[:]), nil
}
