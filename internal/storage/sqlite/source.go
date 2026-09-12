package sqlite

import (
	"context"
	"database/sql"
	"time"

	"github.com/Sawmonabo/codectx/internal/model"
)

// snapshotBatchRows bounds one manifest import transaction. A manifest is
// streamed row by row from the caller's on-disk staging; the store never holds
// more than this many FileVersions at once (Section 10.1).
const snapshotBatchRows = 500

// EnsureRepository records the installation-local repository identity and its
// current root path. The row is required by every snapshot and generation.
func (s *Store) EnsureRepository(ctx context.Context, id model.RepositoryID, rootPath string) error {
	raw, err := idBlob("repository_id", string(id))
	if err != nil {
		return err
	}
	if rootPath == "" || len(rootPath) > model.MaxPathBytes {
		return invalid("repository root path is empty or exceeds %d bytes", model.MaxPathBytes)
	}
	return s.write(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `INSERT INTO repositories(id, root_path, created_at) VALUES(?, ?, ?)
			ON CONFLICT(id) DO UPDATE SET root_path = excluded.root_path`, raw, rootPath, formatTime(time.Now()))
		return wrap("repositories", err)
	})
}

// PutBlob records one retained CAS object with its block digests and line
// checkpoints (Section 10.3). It is idempotent for the same hash and size; the
// same hash with a different size is a source integrity failure. The caller
// has already flushed and published the object file.
func (s *Store) PutBlob(ctx context.Context, b model.BlobRecord) error {
	if err := b.Validate(); err != nil {
		return err
	}
	hash, err := idBlob("blob.hash", b.Hash)
	if err != nil {
		return err
	}
	return s.write(ctx, func(tx *sql.Tx) error {
		var size int64
		err := tx.QueryRowContext(ctx, `SELECT size_bytes FROM blobs WHERE hash = ?`, hash).Scan(&size)
		switch {
		case err == nil:
			if size != b.Size {
				return &model.Error{Code: model.CodeSourceIntegrity,
					Message: "blob is already retained with a different size; the content hash does not match its bytes"}
			}
			return nil
		case !isNoRows(err):
			return wrap("blobs", err)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO blobs(hash, size_bytes, state, created_at) VALUES(?, ?, ?, ?)`,
			hash, b.Size, string(model.BlobReady), formatTime(time.Now())); err != nil {
			return wrap("blobs", err)
		}
		blocks, err := tx.PrepareContext(ctx, `INSERT INTO blob_blocks(blob_hash, block_index, digest) VALUES(?, ?, ?)`)
		if err != nil {
			return wrap("blob_blocks", err)
		}
		defer blocks.Close()
		for i, d := range b.BlockDigests {
			digest, err := idBlob("blob.block_digests", d)
			if err != nil {
				return err
			}
			if _, err := blocks.ExecContext(ctx, hash, i, digest); err != nil {
				return wrap("blob_blocks", err)
			}
		}
		lines, err := tx.PrepareContext(ctx, `INSERT INTO line_checkpoints(blob_hash, byte_offset, line_number, line_start_byte) VALUES(?, ?, ?, ?)`)
		if err != nil {
			return wrap("line_checkpoints", err)
		}
		defer lines.Close()
		for _, c := range b.LineCheckpoints {
			if _, err := lines.ExecContext(ctx, hash, int64(c.ByteOffset), int64(c.LineNumber), int64(c.LineStartByte)); err != nil {
				return wrap("line_checkpoints", err)
			}
		}
		return nil
	})
}

// PutSnapshot stores a snapshot header and its manifest rows. files streams the
// manifest; rows are written in bounded batches and never collected. Every
// nondeleted row must name a blob already retained with PutBlob. The header's
// file count and byte total are validated against the rows actually written.
//
// The call is idempotent for a complete existing snapshot. An incomplete one
// left by a crash is replaced only while no generation references it.
func (s *Store) PutSnapshot(ctx context.Context, snap model.Snapshot, files func(yield func(model.FileVersion) error) error) error {
	if err := snap.Validate(); err != nil {
		return err
	}
	snapID, err := idBlob("snapshot.id", string(snap.ID))
	if err != nil {
		return err
	}
	repo, err := idBlob("snapshot.repository_id", string(snap.RepositoryID))
	if err != nil {
		return err
	}
	complete := false
	err = s.write(ctx, func(tx *sql.Tx) error {
		var count, bytes int64
		err := tx.QueryRowContext(ctx, `SELECT count(*), coalesce(sum(size_bytes), 0) FROM snapshot_files WHERE snapshot_id = ?`, snapID).Scan(&count, &bytes)
		if err != nil {
			return wrap("snapshot_files", err)
		}
		var exists int
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM snapshots WHERE id = ?`, snapID).Scan(&exists); err != nil {
			return wrap("snapshots", err)
		}
		if exists == 1 {
			if uint64(count) == snap.FileCount && uint64(bytes) == snap.SourceBytes {
				complete = true
				return nil
			}
			var referenced int
			if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM generations WHERE snapshot_id = ?`, snapID).Scan(&referenced); err != nil {
				return wrap("generations", err)
			}
			if referenced != 0 {
				return corrupt("snapshot %s is referenced by a generation but its manifest is incomplete", snap.ID)
			}
			if _, err := tx.ExecContext(ctx, `DELETE FROM snapshots WHERE id = ?`, snapID); err != nil {
				return wrap("snapshots", err)
			}
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO snapshots(id, repository_id, head_object_id, source_policy_hash, manifest_hash,
			file_count, source_bytes, capture_consistency, created_at) VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			snapID, repo, snap.HeadObjectID, snap.SourcePolicyHash, snap.ManifestHash,
			int64(snap.FileCount), int64(snap.SourceBytes), string(snap.CaptureConsistency), formatTime(snap.CreatedAt))
		return wrap("snapshots", err)
	})
	if err != nil || complete {
		return err
	}

	batch := make([]model.FileVersion, 0, snapshotBatchRows)
	var count, bytes uint64
	flush := func() error {
		if len(batch) == 0 {
			return nil
		}
		err := s.write(ctx, func(tx *sql.Tx) error {
			return s.insertManifestRows(ctx, tx, snapID, repo, snap.RepositoryID, batch)
		})
		batch = batch[:0]
		return err
	}
	err = files(func(fv model.FileVersion) error {
		if err := fv.Validate(); err != nil {
			return err
		}
		count++
		bytes += uint64(fv.Size)
		batch = append(batch, fv)
		if len(batch) == snapshotBatchRows {
			return flush()
		}
		return nil
	})
	if err == nil {
		err = flush()
	}
	if err == nil && (count != snap.FileCount || bytes != snap.SourceBytes) {
		err = &model.Error{Code: model.CodeSourceIntegrity,
			Message: "snapshot header does not match its manifest rows: file count or byte total differs"}
	}
	if err != nil {
		// Leave no half-imported snapshot behind; the next attempt would
		// otherwise have to detect and replace it.
		s.write(ctx, func(tx *sql.Tx) error {
			_, derr := tx.ExecContext(ctx, `DELETE FROM snapshots WHERE id = ?`, snapID)
			return wrap("snapshots", derr)
		})
		return err
	}
	return nil
}

func (s *Store) insertManifestRows(ctx context.Context, tx *sql.Tx, snapID, repo []byte, repoID model.RepositoryID, rows []model.FileVersion) error {
	fileStmt, err := tx.PrepareContext(ctx, `INSERT OR IGNORE INTO files(id, repository_id, path) VALUES(?, ?, ?)`)
	if err != nil {
		return wrap("files", err)
	}
	defer fileStmt.Close()
	rowStmt, err := tx.PrepareContext(ctx, `INSERT INTO snapshot_files(snapshot_id, file_id, status, content_hash, size_bytes,
		git_object_id, language, executable) VALUES(?, ?, ?, ?, ?, ?, ?, ?)`)
	if err != nil {
		return wrap("snapshot_files", err)
	}
	defer rowStmt.Close()
	blobStmt, err := tx.PrepareContext(ctx, `SELECT size_bytes FROM blobs WHERE hash = ? AND state = 'ready'`)
	if err != nil {
		return wrap("blobs", err)
	}
	defer blobStmt.Close()
	for _, fv := range rows {
		if want := model.NewFileID(repoID, fv.Path); fv.ID != want {
			return &model.Error{Code: model.CodeSourceIntegrity,
				Message: "file id does not derive from its repository and path"}
		}
		fileID, err := idBlob("file_version.id", string(fv.ID))
		if err != nil {
			return err
		}
		if _, err := fileStmt.ExecContext(ctx, fileID, repo, fv.Path); err != nil {
			return wrap("files", err)
		}
		var hash any
		if fv.Status != model.FileDeleted {
			raw, err := idBlob("file_version.content_hash", fv.ContentHash)
			if err != nil {
				return err
			}
			var size int64
			if err := blobStmt.QueryRowContext(ctx, raw).Scan(&size); err != nil {
				if isNoRows(err) {
					return &model.Error{Code: model.CodeSourceIntegrity,
						Message: "manifest names a blob that is not retained; capture must publish the object before the snapshot",
						Details: map[string]string{"content_hash": fv.ContentHash}}
				}
				return wrap("blobs", err)
			}
			if size != fv.Size {
				return &model.Error{Code: model.CodeSourceIntegrity,
					Message: "manifest size disagrees with the retained blob size", Details: map[string]string{"content_hash": fv.ContentHash}}
			}
			hash = raw
		}
		if _, err := rowStmt.ExecContext(ctx, snapID, fileID, string(fv.Status), hash, fv.Size,
			fv.GitObjectID, fv.Language, boolInt(fv.Executable)); err != nil {
			return wrap("snapshot_files", err)
		}
	}
	return nil
}
