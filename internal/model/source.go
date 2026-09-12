package model

import (
	"context"
	"io"
	"time"
)

// Position is a byte-authoritative source coordinate (Section 9.3). Byte is
// zero-based, Line is one-based and Column is a zero-based UTF-8 byte column.
// An EOF position is valid. CRLF is two bytes and one logical line break.
type Position struct {
	Byte   uint64 `json:"byte"`
	Line   uint32 `json:"line"`
	Column uint32 `json:"column"`
}

// SourceRange is a half-open [Start,End) range with line/column context. A
// range is absent, not zero-filled, for entities without source locations.
type SourceRange struct {
	Start Position `json:"start"`
	End   Position `json:"end"`
}

// ByteRange is a half-open [Start,End) byte interval with no line context.
type ByteRange struct {
	Start uint64 `json:"start"`
	End   uint64 `json:"end"`
}

// Validate enforces the Section 9.3 bounds: a one-based line and a value that
// still fits the signed 64-bit integer SQLite stores.
func (p Position) Validate(field string) *Error {
	if err := boundSigned64(field+".byte", p.Byte); err != nil {
		return err
	}
	if p.Line < 1 {
		return invalid("%s.line is %d; public line numbers are one-based", field, p.Line)
	}
	return nil
}

// Validate enforces a well-ordered half-open range. A zero-length range is
// legal: node_facts, evidence, issued_chunks and search_units all check
// end_byte >= start_byte, and Section 16.3 requires a zero-length EOF receipt
// for an empty file.
func (r SourceRange) Validate(field string) *Error {
	if err := r.Start.Validate(field + ".start"); err != nil {
		return err
	}
	if err := r.End.Validate(field + ".end"); err != nil {
		return err
	}
	if r.End.Byte < r.Start.Byte {
		return invalid("%s is [%d,%d); a half-open range must not end before it starts", field, r.Start.Byte, r.End.Byte)
	}
	return nil
}

// Validate enforces a well-ordered half-open byte interval, allowing the
// zero-length EOF interval.
func (r ByteRange) Validate(field string) *Error {
	if err := boundSigned64(field+".start", r.Start); err != nil {
		return err
	}
	if err := boundSigned64(field+".end", r.End); err != nil {
		return err
	}
	if r.End < r.Start {
		return invalid("%s is [%d,%d); a half-open range must not end before it starts", field, r.Start, r.End)
	}
	return nil
}

// ValidateNonEmpty additionally rejects a zero-length interval, matching the
// served_ranges CHECK(end_byte > start_byte): confirmed coverage of nothing is
// not coverage.
func (r ByteRange) ValidateNonEmpty(field string) *Error {
	if err := r.Validate(field); err != nil {
		return err
	}
	if r.End == r.Start {
		return invalid("%s is the empty interval [%d,%d); a served range must cover at least one byte", field, r.Start, r.End)
	}
	return nil
}

// validateLocatedRange enforces the mixed-range CHECK shared by node_facts and
// evidence: a range is either wholly absent or accompanied by the file it
// belongs to.
func validateLocatedRange(field string, fileID FileID, r *SourceRange) *Error {
	if r == nil {
		return nil
	}
	if fileID == "" {
		return invalid("%s is present without %s.file_id; a located range must name its file", field, field)
	}
	if err := r.Validate(field); err != nil {
		return err
	}
	return nil
}

// FileStatus is the snapshot_files vocabulary. A deleted entry is a tombstone
// with no content hash and zero size.
type FileStatus string

const (
	FileTracked   FileStatus = "tracked"
	FileModified  FileStatus = "modified"
	FileAdded     FileStatus = "added"
	FileDeleted   FileStatus = "deleted"
	FileUntracked FileStatus = "untracked"
)

// Valid reports whether s is a known wire spelling.
func (s FileStatus) Valid() bool {
	switch s {
	case FileTracked, FileModified, FileAdded, FileDeleted, FileUntracked:
		return true
	}
	return false
}

// CaptureConsistency records how a snapshot's bytes were obtained (Section
// 10.2). It is never upgraded by inference: validated_capture means an exact
// immutable manifest plus detected-change validation, and operator_frozen means
// the operator supplied a quiescent source or an OS snapshot.
type CaptureConsistency string

const (
	CaptureValidated      CaptureConsistency = "validated_capture"
	CaptureOperatorFrozen CaptureConsistency = "operator_frozen"
)

// Valid reports whether c is a known wire spelling.
func (c CaptureConsistency) Valid() bool {
	return c == CaptureValidated || c == CaptureOperatorFrozen
}

// BlobState is the CAS lifecycle vocabulary. Only ready blobs are servable;
// quarantined and trash objects are retained for diagnosis and collection.
type BlobState string

const (
	BlobReady       BlobState = "ready"
	BlobQuarantined BlobState = "quarantined"
	BlobTrash       BlobState = "trash"
)

// Valid reports whether b is a known wire spelling.
func (b BlobState) Valid() bool {
	switch b {
	case BlobReady, BlobQuarantined, BlobTrash:
		return true
	}
	return false
}

// FileVersion is one row of a snapshot manifest: the exact bytes captured for
// one path, not the current worktree file.
type FileVersion struct {
	ID          FileID     `json:"id"`
	Path        string     `json:"path"`
	Status      FileStatus `json:"status"`
	Size        int64      `json:"size"`
	ContentHash string     `json:"content_hash,omitempty"`
	GitObjectID string     `json:"git_object_id,omitempty"`
	Language    string     `json:"language,omitempty"`
	Executable  bool       `json:"executable"`
}

// Validate enforces the snapshot_files constraints, including the tombstone
// rule that a deleted entry carries no content hash and zero size.
func (f FileVersion) Validate() error {
	if err := requireID("file_version.id", string(f.ID)); err != nil {
		return err
	}
	if err := requireField("file_version.path", f.Path, MaxPathBytes); err != nil {
		return err
	}
	if !f.Status.Valid() {
		return invalid("file_version.status %q is not a known snapshot file status", truncateForMessage(string(f.Status)))
	}
	if err := requireNonNegative("file_version.size", f.Size); err != nil {
		return err
	}
	if err := boundField("file_version.language", f.Language, MaxLanguageBytes); err != nil {
		return err
	}
	if err := boundField("file_version.git_object_id", f.GitObjectID, MaxIdentifierBytes); err != nil {
		return err
	}
	if f.Status == FileDeleted {
		if f.ContentHash != "" || f.Size != 0 {
			return invalid("file_version %q is deleted but carries content; a tombstone has no hash and zero size", truncateForMessage(f.Path))
		}
		return nil
	}
	if err := requireID("file_version.content_hash", f.ContentHash); err != nil {
		return err
	}
	return nil
}

// Snapshot is the immutable header of one capture. The manifest itself is never
// embedded: it is streamed from indexed on-disk staging and reached through
// SnapshotView, so no whole-repository file list is retained in the Go heap.
type Snapshot struct {
	ID           SnapshotID   `json:"id"`
	RepositoryID RepositoryID `json:"repository_id"`
	HeadObjectID string       `json:"head_object_id,omitempty"`
	// CaptureConsistency is not in the Section 10.1 sketch, but Section 10.2
	// requires it to be exposed and never inferred, and snapshots.capture_
	// consistency is NOT NULL with no default. Carrying it on the header is the
	// only way the capturer can hand it to storage.
	CaptureConsistency CaptureConsistency `json:"capture_consistency"`
	SourcePolicyHash   string             `json:"source_policy_hash"`
	FileCount          uint64             `json:"file_count"`
	SourceBytes        uint64             `json:"source_bytes"`
	ManifestHash       string             `json:"manifest_hash"`
	CreatedAt          time.Time          `json:"created_at"`
}

// Validate enforces the snapshots table constraints.
func (s Snapshot) Validate() error {
	if err := requireID("snapshot.id", string(s.ID)); err != nil {
		return err
	}
	if err := requireID("snapshot.repository_id", string(s.RepositoryID)); err != nil {
		return err
	}
	if !s.CaptureConsistency.Valid() {
		return invalid("snapshot.capture_consistency %q is not a known capture consistency",
			truncateForMessage(string(s.CaptureConsistency)))
	}
	if err := requireID("snapshot.source_policy_hash", s.SourcePolicyHash); err != nil {
		return err
	}
	if err := requireID("snapshot.manifest_hash", s.ManifestHash); err != nil {
		return err
	}
	if err := boundSigned64("snapshot.file_count", s.FileCount); err != nil {
		return err
	}
	if err := boundSigned64("snapshot.source_bytes", s.SourceBytes); err != nil {
		return err
	}
	if err := boundField("snapshot.head_object_id", s.HeadObjectID, MaxIdentifierBytes); err != nil {
		return err
	}
	return nil
}

// FileSelection narrows a manifest walk. Paths is a bounded explicit filter
// supplied by the caller, never a materialized repository file list.
type FileSelection struct {
	ChangedOnly bool     `json:"changed_only"`
	Paths       []string `json:"paths,omitempty"`
}

// Validate bounds the explicit filter.
func (s FileSelection) Validate() error {
	if err := boundCount("file_selection.paths", len(s.Paths), MaxFilterValues); err != nil {
		return err
	}
	for i, p := range s.Paths {
		if err := requireField(indexed("file_selection.paths", i), p, MaxPathBytes); err != nil {
			return err
		}
	}
	return nil
}

// SnapshotView is the only way to reach captured bytes. Every read is served
// from the CAS at the pinned content hash; nothing reads the live checkout.
type SnapshotView interface {
	Header() Snapshot
	EachFile(context.Context, FileSelection, func(FileVersion) error) error
	Open(context.Context, FileID) (io.ReadCloser, FileVersion, error)
	ReadRange(context.Context, FileID, ByteRange) ([]byte, FileVersion, error)
}
