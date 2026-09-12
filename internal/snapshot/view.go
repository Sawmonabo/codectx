package snapshot

import (
	"context"
	"errors"
	"io"
	"slices"

	"github.com/Sawmonabo/codectx/internal/model"
	"github.com/Sawmonabo/codectx/internal/source"
	"github.com/Sawmonabo/codectx/internal/vcs/git"
)

// Catalog is the read side of storage a view needs: the snapshot header, its
// manifest rows in canonical path order, and each blob's integrity metadata.
// *sqlite.Store satisfies it. Callers hold the workspace lock or a lease that
// names the snapshot, which is what keeps these rows from being collected.
type Catalog interface {
	Snapshot(ctx context.Context, id model.SnapshotID) (model.Snapshot, error)
	SnapshotFile(ctx context.Context, id model.SnapshotID, file model.FileID) (model.FileVersion, error)
	SnapshotFiles(ctx context.Context, id model.SnapshotID, afterPath string, limit int) ([]model.FileVersion, error)
	Blob(ctx context.Context, hash string) (model.BlobRecord, error)
}

// View is the model.SnapshotView over one stored snapshot. Every byte it
// returns comes from the CAS and is verified against the stored block digests
// on the way out; it never reads the live checkout or Git. A View is safe for
// concurrent use.
type View struct {
	header  model.Snapshot
	catalog Catalog
	cas     *CAS
}

var _ model.SnapshotView = (*View)(nil)

// OpenView loads the header of snapshot id.
func OpenView(ctx context.Context, catalog Catalog, cas *CAS, id model.SnapshotID) (*View, error) {
	if catalog == nil || cas == nil {
		return nil, invalid("a snapshot view needs a catalog and a CAS")
	}
	header, err := catalog.Snapshot(ctx, id)
	if err != nil {
		return nil, err
	}
	return &View{header: header, catalog: catalog, cas: cas}, nil
}

// Header returns the snapshot header.
func (v *View) Header() model.Snapshot { return v.header }

// EachFile streams manifest rows in canonical path order through bounded
// pages. An explicit path filter is answered by point lookups in sorted
// order; ChangedOnly drops rows whose bytes match HEAD. A filtered path that
// is not in the snapshot is skipped, not an error: the caller asked what the
// snapshot holds.
func (v *View) EachFile(ctx context.Context, sel model.FileSelection, fn func(model.FileVersion) error) error {
	if err := sel.Validate(); err != nil {
		return err
	}
	emit := func(fv model.FileVersion) error {
		if sel.ChangedOnly && fv.Status == model.FileTracked {
			return nil
		}
		return fn(fv)
	}
	if len(sel.Paths) > 0 {
		paths := slices.Clone(sel.Paths)
		slices.Sort(paths)
		paths = slices.Compact(paths)
		for _, p := range paths {
			fv, err := v.catalog.SnapshotFile(ctx, v.header.ID, model.NewFileID(v.header.RepositoryID, p))
			if err != nil {
				var typed *model.Error
				if errors.As(err, &typed) && typed.Code == model.CodeArgumentInvalid {
					continue
				}
				return err
			}
			if err := emit(fv); err != nil {
				return err
			}
		}
		return nil
	}
	after := ""
	for {
		page, err := v.catalog.SnapshotFiles(ctx, v.header.ID, after, model.MaxPageItems)
		if err != nil {
			return err
		}
		for _, fv := range page {
			if err := emit(fv); err != nil {
				return err
			}
		}
		if len(page) < model.MaxPageItems {
			return nil
		}
		after = page[len(page)-1].Path
	}
}

// file resolves a manifest row that carries content.
func (v *View) file(ctx context.Context, id model.FileID) (model.FileVersion, model.BlobRecord, error) {
	fv, err := v.catalog.SnapshotFile(ctx, v.header.ID, id)
	if err != nil {
		return model.FileVersion{}, model.BlobRecord{}, err
	}
	if fv.Status == model.FileDeleted {
		return fv, model.BlobRecord{}, invalid("file %s is a deletion tombstone in this snapshot and has no content", id)
	}
	rec, err := v.catalog.Blob(ctx, fv.ContentHash)
	if err != nil {
		return fv, model.BlobRecord{}, err
	}
	if rec.Size != fv.Size {
		return fv, rec, integrity("manifest size disagrees with the retained blob").WithDetail("content_hash", fv.ContentHash)
	}
	return fv, rec, nil
}

// Open streams the file's retained bytes, verifying every block as it passes
// and the whole-file digest at the end.
func (v *View) Open(ctx context.Context, id model.FileID) (io.ReadCloser, model.FileVersion, error) {
	fv, rec, err := v.file(ctx, id)
	if err != nil {
		return nil, fv, err
	}
	rc, err := v.cas.Open(ctx, rec)
	return rc, fv, err
}

// ReadRange returns the bytes of [r.Start, r.End), verifying only the blocks
// that cover the interval.
func (v *View) ReadRange(ctx context.Context, id model.FileID, r model.ByteRange) ([]byte, model.FileVersion, error) {
	fv, rec, err := v.file(ctx, id)
	if err != nil {
		return nil, fv, err
	}
	data, err := v.cas.ReadRange(ctx, rec, r)
	return data, fv, err
}

// Range is one verified range read with its whole-file line/column context.
type Range struct {
	Bytes []byte
	Start model.Position
	End   model.Position
}

// Read is ReadRange plus positions: the start and end of the interval in
// one-based lines and zero-based byte columns, derived from the nearest
// stored line checkpoint at or before the interval through source.NewCursorAt.
// The bytes between that checkpoint and the interval are read and verified
// along with the interval; nothing before the checkpoint is touched, so the
// cost is bounded by the checkpoint spacing plus the length of the line that
// crosses it, never by the offset into the file. A boundary inside a UTF-8
// sequence is rejected, as Section 16.2 requires of served offsets.
func (v *View) Read(ctx context.Context, id model.FileID, r model.ByteRange) (Range, model.FileVersion, error) {
	fv, rec, err := v.file(ctx, id)
	if err != nil {
		return Range{}, fv, err
	}
	if err := r.Validate("byte_range"); err != nil {
		return Range{}, fv, err
	}
	if r.End > uint64(rec.Size) {
		return Range{}, fv, invalid("byte range ends at %d, past the %d-byte file", r.End, rec.Size)
	}
	cp := indexOf(rec).CheckpointFor(r.Start)
	window, err := v.cas.ReadRange(ctx, rec, model.ByteRange{Start: cp.Byte, End: r.End})
	if err != nil {
		return Range{}, fv, err
	}
	cursor, err := source.NewCursorAt(window, cp.Byte, cp.Line)
	if err != nil {
		return Range{}, fv, err
	}
	start, err := cursor.PositionAt(r.Start)
	if err != nil {
		return Range{}, fv, err
	}
	end, err := cursor.PositionAt(r.End)
	if err != nil {
		return Range{}, fv, err
	}
	return Range{Bytes: window[r.Start-cp.Byte:], Start: start, End: end}, fv, nil
}

// RepairSource is the only fallback a read may ever use, and only through
// Repair: the repository whose recorded Git object IDs may reconstruct a
// missing blob.
type RepairSource struct {
	Git  *git.Git
	Root string
}

// Repair reconstructs the CAS object for id from its recorded Git object ID
// when the object file is missing (Section 10.4). The reconstruction is
// published only if its complete SHA-256 equals the manifest's content hash;
// anything else is CTX_SOURCE_INTEGRITY and leaves the store untouched. A blob
// that is present is verified, not replaced: a corrupt present object is
// reported, because silently overwriting it would hide the corruption's cause.
// Repair never changes the snapshot's identity.
func (v *View) Repair(ctx context.Context, id model.FileID, src RepairSource) error {
	fv, rec, err := v.file(ctx, id)
	if err != nil {
		return err
	}
	present, err := v.cas.Has(rec.Hash)
	if err != nil {
		return err
	}
	if present {
		rc, err := v.cas.Open(ctx, rec)
		if err != nil {
			return err
		}
		defer rc.Close()
		_, err = io.Copy(io.Discard, rc)
		return err
	}
	if fv.GitObjectID == "" {
		return integrity("blob is missing and the manifest records no Git provenance for it").WithDetail("content_hash", rec.Hash)
	}
	if src.Git == nil || src.Root == "" {
		return integrity("blob is missing; repair needs the repository that recorded its Git object").WithDetail("content_hash", rec.Hash)
	}
	pr, pw := io.Pipe()
	done := make(chan error, 1)
	go func() {
		_, err := v.cas.put(ctx, pr, rec.Hash)
		// Drain so the writer is never blocked behind a rejection.
		io.Copy(io.Discard, pr)
		done <- err
	}()
	catErr := src.Git.CatBlob(ctx, src.Root, fv.GitObjectID, pw, rec.Size+1)
	pw.Close()
	putErr := <-done
	var typed *model.Error
	if errors.As(catErr, &typed) && typed.Code == model.CodeResourceLimit {
		// The bound was the recorded size plus one: the object is not the
		// blob the manifest describes.
		return integrity("Git object is larger than the recorded blob").WithDetail("content_hash", rec.Hash)
	}
	if catErr != nil {
		return catErr
	}
	return putErr
}
