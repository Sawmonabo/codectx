package sqlite

import (
	"context"
	"crypto/sha256"
	"database/sql"
	_ "embed"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/Sawmonabo/codectx/internal/model"
)

// schemaSQL is the complete Section 12.2 DDL, reproduced verbatim. There is no
// migration runner, history or compatibility facade: a change to this file is a
// new fingerprint, and an existing database with another fingerprint fails
// closed until the user explicitly rebuilds into a new cache.
//
//go:embed schema.sql
var schemaSQL string

// schemaVersion is the single value schema_meta.version admits.
const schemaVersion = 1

// Fingerprint is the SHA-256 of the embedded DDL text. It is stored in
// schema_meta and folded into every AnalysisKey (Section 9.1).
var Fingerprint = func() string {
	sum := sha256.Sum256([]byte(schemaSQL))
	return hex.EncodeToString(sum[:])
}()

// initSchema creates the schema in an empty database or verifies an existing
// one. A database with tables but no readable, matching schema_meta row is
// CTX_SCHEMA_MISMATCH; nothing is altered.
func (s *Store) initSchema(ctx context.Context) error {
	return s.write(ctx, func(tx *sql.Tx) error {
		var tables int
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM sqlite_master WHERE type = 'table'`).Scan(&tables); err != nil {
			return wrap("sqlite_master", err)
		}
		if tables == 0 {
			if _, err := tx.ExecContext(ctx, schemaSQL); err != nil {
				return wrap("create schema", err)
			}
			_, err := tx.ExecContext(ctx, `INSERT INTO schema_meta(singleton, version, fingerprint) VALUES(1, ?, ?)`, schemaVersion, Fingerprint)
			return wrap("schema_meta", err)
		}
		var version int
		var fingerprint string
		err := tx.QueryRowContext(ctx, `SELECT version, fingerprint FROM schema_meta WHERE singleton = 1`).Scan(&version, &fingerprint)
		if err != nil || version != schemaVersion || fingerprint != Fingerprint {
			found := fingerprint
			if err != nil {
				found = "unreadable"
			}
			return &model.Error{Code: model.CodeSchemaMismatch,
				Message:     "database schema fingerprint " + found + " does not match this binary's schema " + Fingerprint,
				Details:     map[string]string{"path": s.path},
				Remediation: "run `codectx index --rebuild` to create a new cache; the existing database is left untouched"}
		}
		return nil
	})
}

// Recover is startup recovery for the indexing owner (Section 12.3). The
// caller must hold the workspace indexing lock: it marks every staging
// generation failed, deletes the unsealed units and running runs those
// generations left behind, then runs the same collection tail as
// DeleteGeneration (unreachable units, orphan runs, expired leases,
// unreferenced snapshots and files), so a crash between a deletion's steps
// leaves nothing a second Recover would treat differently. The active pointer
// is never touched.
func (s *Store) Recover(ctx context.Context, now time.Time) error {
	var staging []int64
	err := s.read(ctx, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, `SELECT id FROM generations WHERE status = 'staging'`)
		if err != nil {
			return wrap("staging generations", err)
		}
		defer rows.Close()
		for rows.Next() {
			var id int64
			if err := rows.Scan(&id); err != nil {
				return wrap("scan", err)
			}
			staging = append(staging, id)
		}
		return wrap("staging generations", rows.Err())
	})
	if err != nil {
		return err
	}
	for _, id := range staging {
		if err := s.Abort(ctx, model.GenerationID(id)); err != nil {
			return err
		}
	}
	return s.sweep(ctx, now, nil)
}

// Check runs the integrity checks Section 12.1 reserves for initialization,
// doctor and recovery: quick_check, foreign_key_check and, when deep, the FTS5
// integrity check plus the vocabulary staleness count. It never runs per query.
//
// search_fts is contentless (ADR-0003 §2.1), so its integrity-check proves the
// index is internally consistent, not that it agrees with a stored copy of the
// text -- there is no stored copy, and the text's own authority is the content
// store, which verifies every block digest on read. Document membership is
// still checked both ways: the FTS rowid space is search_units.rowid, and the
// vocabulary scan below reports any indexed document whose row is gone.
func (s *Store) Check(ctx context.Context, deep bool) error {
	return s.write(ctx, func(tx *sql.Tx) error {
		var result string
		if err := tx.QueryRowContext(ctx, `PRAGMA quick_check(1)`).Scan(&result); err != nil {
			return wrap("quick_check", err)
		}
		if result != "ok" {
			return corrupt("quick_check: %s", result)
		}
		var violations int
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM pragma_foreign_key_check`).Scan(&violations); err != nil {
			return wrap("foreign_key_check", err)
		}
		if violations != 0 {
			return corrupt("%d foreign key violations", violations)
		}
		if deep {
			if _, err := tx.ExecContext(ctx, `INSERT INTO search_fts(search_fts) VALUES('integrity-check')`); err != nil {
				return &model.Error{Code: model.CodeStorageCorrupt, Message: "search index failed its internal integrity check: " + err.Error(),
					Remediation: "rebuild the cache with index --rebuild"}
			}
			// integrity-check inspects only the index's own structures; it cannot
			// report index entries whose search_units row is gone. The vocabulary table
			// enumerates every indexed instance, so a stale document shows up here.
			var stale int64
			if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM search_vocab v WHERE NOT EXISTS (SELECT 1 FROM search_units su WHERE su.rowid = v.doc)`).Scan(&stale); err != nil {
				return wrap("search_vocab", err)
			}
			if stale != 0 {
				return &model.Error{Code: model.CodeStorageCorrupt, Message: fmt.Sprintf("search index holds %d term instances for documents that no longer exist", stale),
					Remediation: "rebuild the cache with index --rebuild"}
			}
		}
		return nil
	})
}

// Stats is the storage accounting status and doctor report (Section 22):
// bounded row counts plus database and WAL bytes on disk.
type Stats struct {
	Generations   int64
	Snapshots     int64
	Files         int64
	Blobs         int64
	Units         int64
	NodeFacts     int64
	RelationFacts int64
	Evidence      int64
	SearchUnits   int64
	Leases        int64
	Sessions      int64
	DatabaseBytes int64
	WALBytes      int64
}

// Stats reads the row counts in one read transaction and sizes the files. A
// file size that cannot be read is an error, never reported as zero.
func (s *Store) Stats(ctx context.Context) (Stats, error) {
	var st Stats
	err := s.read(ctx, func(tx *sql.Tx) error {
		return tx.QueryRowContext(ctx, `SELECT
			(SELECT count(*) FROM generations), (SELECT count(*) FROM snapshots), (SELECT count(*) FROM files), (SELECT count(*) FROM blobs),
			(SELECT count(*) FROM units), (SELECT count(*) FROM node_facts), (SELECT count(*) FROM relation_facts),
			(SELECT count(*) FROM evidence), (SELECT count(*) FROM search_units),
			(SELECT count(*) FROM retention_leases), (SELECT count(*) FROM read_sessions)`).
			Scan(&st.Generations, &st.Snapshots, &st.Files, &st.Blobs, &st.Units, &st.NodeFacts, &st.RelationFacts, &st.Evidence, &st.SearchUnits, &st.Leases, &st.Sessions)
	})
	if err != nil {
		return Stats{}, wrap("stats", err)
	}
	if st.DatabaseBytes, err = fileSize(s.path, false); err != nil {
		return Stats{}, err
	}
	if st.WALBytes, err = s.walBytes(); err != nil {
		return Stats{}, err
	}
	return st, nil
}

// fileSize stats path. With absentIsZero a missing file is legitimately zero
// bytes (the WAL is removed by a checkpoint); any other failure is returned so
// a caller never mistakes "unreadable" for "empty".
func fileSize(path string, absentIsZero bool) (int64, error) {
	info, err := os.Stat(path)
	if err != nil {
		if absentIsZero && errors.Is(err, os.ErrNotExist) {
			return 0, nil
		}
		return 0, internal("stat " + path + ": " + err.Error())
	}
	return info.Size(), nil
}

func (s *Store) walBytes() (int64, error) { return fileSize(s.path+"-wal", true) }

// MaintainWAL is the writer-owned passive checkpoint of Section 12.1. It
// reports the WAL size and whether it still exceeds the configured high-water
// mark after a passive checkpoint, in which case the caller applies indexing
// backpressure. It runs between batches, never inside a query.
func (s *Store) MaintainWAL(ctx context.Context) (walBytes int64, backpressure bool, err error) {
	if walBytes, err = s.walBytes(); err != nil {
		return 0, false, err
	}
	if walBytes < s.opts.WALHighWaterBytes {
		return walBytes, false, nil
	}
	var busy, logFrames, checkpointed int64
	if err := s.writer.QueryRowContext(ctx, `PRAGMA wal_checkpoint(PASSIVE)`).Scan(&busy, &logFrames, &checkpointed); err != nil {
		return 0, false, wrap("wal_checkpoint", err)
	}
	if walBytes, err = s.walBytes(); err != nil {
		return 0, false, err
	}
	return walBytes, walBytes >= s.opts.WALHighWaterBytes || busy != 0, nil
}

// isNoRows reports a missing row without leaking database/sql to callers.
func isNoRows(err error) bool { return errors.Is(err, sql.ErrNoRows) }
