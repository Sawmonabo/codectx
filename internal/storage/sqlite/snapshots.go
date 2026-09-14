package sqlite

import (
	"context"
	"database/sql"

	"github.com/Sawmonabo/codectx/internal/model"
)

// Snapshot-scoped reads for the capture and source-serving path (Section 10).
// They are addressed by snapshot identity rather than by a pinned generation
// because their callers already hold the workspace indexing lock (a capture in
// progress) or a retention lease naming the snapshot (a source read), and
// because a snapshot exists before any generation references it.

// ReasonNotFound is the Details["reason"] value that marks a lookup which
// found no row, so a caller can tell an absent snapshot or file from a
// malformed argument under the shared CTX_ARGUMENT_INVALID code.
const ReasonNotFound = "not_found"

func notFound(format string, args ...any) *model.Error {
	return invalid(format, args...).WithDetail("reason", ReasonNotFound)
}

// Snapshot reads one stored header exactly as PutSnapshot wrote it.
func (s *Store) Snapshot(ctx context.Context, id model.SnapshotID) (model.Snapshot, error) {
	raw, err := idBlob("snapshot.id", string(id))
	if err != nil {
		return model.Snapshot{}, err
	}
	var snap model.Snapshot
	err = s.read(ctx, func(tx *sql.Tx) error {
		var repo []byte
		var count, bytes int64
		var created string
		err := tx.QueryRowContext(ctx, `SELECT repository_id, head_object_id, source_policy_hash, manifest_hash, file_count, source_bytes,
			capture_consistency, created_at FROM snapshots WHERE id = ?`, raw).
			Scan(&repo, &snap.HeadObjectID, &snap.SourcePolicyHash, &snap.ManifestHash, &count, &bytes, &snap.CaptureConsistency, &created)
		if isNoRows(err) {
			return notFound("snapshot %s does not exist", id)
		}
		if err != nil {
			return wrap("snapshots", err)
		}
		snap.ID, snap.RepositoryID = id, model.RepositoryID(idHex(repo))
		snap.FileCount, snap.SourceBytes = uint64(count), uint64(bytes)
		snap.CreatedAt, err = parseTime(created)
		return err
	})
	return snap, err
}

const snapshotFileQuery = `SELECT sf.file_id, f.path, sf.status, sf.size_bytes, sf.content_hash, sf.git_object_id, sf.language, sf.executable
	FROM snapshot_files sf JOIN files f ON f.id = sf.file_id WHERE sf.snapshot_id = ?1`

// SnapshotFile reads one manifest row of a snapshot.
func (s *Store) SnapshotFile(ctx context.Context, id model.SnapshotID, file model.FileID) (model.FileVersion, error) {
	snapRaw, err := idBlob("snapshot.id", string(id))
	if err != nil {
		return model.FileVersion{}, err
	}
	fileRaw, err := idBlob("file_id", string(file))
	if err != nil {
		return model.FileVersion{}, err
	}
	var fv model.FileVersion
	err = s.read(ctx, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, snapshotFileQuery+` AND sf.file_id = ?2 LIMIT 1`, snapRaw, fileRaw)
		if err != nil {
			return wrap("snapshot_files", err)
		}
		defer rows.Close()
		if !rows.Next() {
			if err := rows.Err(); err != nil {
				return wrap("snapshot_files", err)
			}
			return notFound("file %s is not in snapshot %s", file, id)
		}
		fv, err = scanFile(rows)
		return err
	})
	return fv, err
}

// FilePath is one pinned file's path and nothing else: a point read on the
// snapshot_files primary key joined to files, for callers that hold a file
// identifier and must name the file to an operator. It is deliberately not
// SnapshotFile: a caller that needs only the path must not be handed -- or come
// to depend on -- the rest of the pinned row.
func (s *Store) FilePath(ctx context.Context, snapshot model.SnapshotID, id model.FileID) (string, error) {
	snapRaw, err := idBlob("snapshot.id", string(snapshot))
	if err != nil {
		return "", err
	}
	fileRaw, err := idBlob("file_id", string(id))
	if err != nil {
		return "", err
	}
	var path string
	err = s.read(ctx, func(tx *sql.Tx) error {
		err := tx.QueryRowContext(ctx, `SELECT f.path FROM snapshot_files sf JOIN files f ON f.id = sf.file_id
			WHERE sf.snapshot_id = ?1 AND sf.file_id = ?2`, snapRaw, fileRaw).Scan(&path)
		if isNoRows(err) {
			return notFound("file %s is not in snapshot %s", id, snapshot)
		}
		if err != nil {
			return wrap("snapshot_files", err)
		}
		return nil
	})
	return path, err
}

// SnapshotFiles pages a snapshot's manifest in canonical path order, keyset on
// the path itself (the empty string starts from the beginning). Paths compare
// bytewise under the column's BINARY collation, which is the order the capture
// walk emits and the manifest hash folds, so a page boundary never reorders or
// repeats a row. limit is capped at model.MaxPageItems.
func (s *Store) SnapshotFiles(ctx context.Context, id model.SnapshotID, afterPath string, limit int) ([]model.FileVersion, error) {
	limit = pageLimit(limit)
	snapRaw, err := idBlob("snapshot.id", string(id))
	if err != nil {
		return nil, err
	}
	if len(afterPath) > model.MaxPathBytes {
		return nil, invalid("after_path is %d bytes, limit %d", len(afterPath), model.MaxPathBytes)
	}
	var out []model.FileVersion
	err = s.read(ctx, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, snapshotFileQuery+` AND f.path > ?2 ORDER BY f.path LIMIT ?3`, snapRaw, afterPath, limit)
		if err != nil {
			return wrap("snapshot_files", err)
		}
		defer rows.Close()
		for rows.Next() {
			fv, err := scanFile(rows)
			if err != nil {
				return err
			}
			out = append(out, fv)
		}
		return wrap("snapshot_files", rows.Err())
	})
	return out, err
}

// Blob reads a ready blob's integrity metadata: the record PutBlob persisted,
// with block digests in block order and line checkpoints in byte order. A
// blob that is absent, quarantined or in trash is CTX_SOURCE_INTEGRITY: no
// caller may serve bytes it cannot verify against stored digests.
func (s *Store) Blob(ctx context.Context, hash string) (model.BlobRecord, error) {
	raw, err := idBlob("blob.hash", hash)
	if err != nil {
		return model.BlobRecord{}, err
	}
	rec := model.BlobRecord{Hash: hash}
	err = s.read(ctx, func(tx *sql.Tx) error {
		var state model.BlobState
		err := tx.QueryRowContext(ctx, `SELECT size_bytes, state FROM blobs WHERE hash = ?`, raw).Scan(&rec.Size, &state)
		if isNoRows(err) {
			return &model.Error{Code: model.CodeSourceIntegrity, Message: "blob is not retained",
				Details: map[string]string{"content_hash": hash}}
		}
		if err != nil {
			return wrap("blobs", err)
		}
		if state != model.BlobReady {
			return &model.Error{Code: model.CodeSourceIntegrity, Message: "blob is " + string(state) + ", not ready",
				Details: map[string]string{"content_hash": hash}}
		}
		blocks, err := tx.QueryContext(ctx, `SELECT digest FROM blob_blocks WHERE blob_hash = ? ORDER BY block_index`, raw)
		if err != nil {
			return wrap("blob_blocks", err)
		}
		defer blocks.Close()
		for blocks.Next() {
			var digest []byte
			if err := blocks.Scan(&digest); err != nil {
				return wrap("blob_blocks", err)
			}
			rec.BlockDigests = append(rec.BlockDigests, idHex(digest))
		}
		if err := blocks.Err(); err != nil {
			return wrap("blob_blocks", err)
		}
		lines, err := tx.QueryContext(ctx, `SELECT byte_offset, line_number, line_start_byte FROM line_checkpoints WHERE blob_hash = ? ORDER BY byte_offset`, raw)
		if err != nil {
			return wrap("line_checkpoints", err)
		}
		defer lines.Close()
		for lines.Next() {
			var offset, line, start int64
			if err := lines.Scan(&offset, &line, &start); err != nil {
				return wrap("line_checkpoints", err)
			}
			rec.LineCheckpoints = append(rec.LineCheckpoints, model.LineCheckpoint{ByteOffset: uint64(offset), LineNumber: uint32(line), LineStartByte: uint64(start)})
		}
		return wrap("line_checkpoints", lines.Err())
	})
	if err != nil {
		return model.BlobRecord{}, err
	}
	if err := rec.Validate(); err != nil {
		// Rows that do not describe their own size are not usable metadata.
		return model.BlobRecord{}, corrupt("blob %s metadata is inconsistent: %v", hash, err)
	}
	return rec, nil
}
