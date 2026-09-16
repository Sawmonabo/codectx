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
	"strings"
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
		return s.checkFingerprint(ctx, tx)
	})
}

// verifySchema is initSchema's check for a process that opened the store
// read-only: the same comparison, run on the reader pool, so a report never
// begins a write transaction to learn whether it may read. It creates nothing.
// A cache that holds no tables is one no run has ever built here -- a read-only
// open of a missing database leaves a zero-byte file behind -- and is reported
// as the absent workspace it is, not as a fingerprint that failed to match.
func (s *Store) verifySchema(ctx context.Context) error {
	return s.read(ctx, func(tx *sql.Tx) error {
		var tables int
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM sqlite_master WHERE type = 'table'`).Scan(&tables); err != nil {
			return wrap("sqlite_master", err)
		}
		if tables == 0 {
			return &model.Error{Code: model.CodeWorkspaceNotFound,
				Message:     "no index cache has been built at " + s.path,
				Remediation: "run `codectx index` to build one"}
		}
		return s.checkFingerprint(ctx, tx)
	})
}

// checkFingerprint compares the stored schema identity with this binary's. It
// is the one place the comparison lives, so the writing and the read-only opens
// cannot come to different conclusions about the same database.
func (s *Store) checkFingerprint(ctx context.Context, tx *sql.Tx) error {
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
// doctor and recovery.
//
// It is two different checks, because the expensive one is O(database bytes)
// and an ordinary `doctor` may not pay it: on a 1.9 GB index `quick_check`
// alone costs 30 s cold and ~4.5 s of CPU warm, which made the one command an
// operator runs on a sick workspace the slowest command in the product.
//
//   - shallow (deep == false): the constant-cost header reads -- page size and
//     page count, the schema fingerprint row, and that the journal is still the
//     write-ahead log this store requires. These catch a database that is not
//     this schema, was truncated below its own header, or lost its WAL mode;
//     they do NOT walk a single page of content, and the caller must report the
//     content walk as unverified rather than as passed.
//   - deep (deep == true): everything shallow checks, then `quick_check`, the
//     foreign key check and the full-text index walk -- the complete pass.
//
// Both run on the reader pool (`s.read`), never `s.write`: an integrity walk
// reads, and taking the writer for it serialised `doctor` against any running
// indexer for the whole walk. The one exception is the FTS5 `integrity-check`
// command, which is spelled as an INSERT and is therefore refused by the
// readers' `query_only=ON`; it takes the writer for that statement alone, and
// only under deep.
//
// search_fts is contentless (ADR-0003 §2.1), so its integrity-check proves the
// index is internally consistent, not that it agrees with a stored copy of the
// text -- there is no stored copy, and the text's own authority is the content
// store, which verifies every block digest on read. Document membership is
// still checked both ways: a posting is named by search_units.doc_id -- which a
// delta carry-over shares along a carry chain, so several rows may name one --
// and the vocabulary scan in checkContent reports any indexed document no row
// names.
func (s *Store) Check(ctx context.Context, deep bool) error {
	if err := s.checkHeader(ctx); err != nil {
		return err
	}
	if !deep {
		return nil
	}
	return s.checkContent(ctx)
}

// checkHeader is the constant-cost half of Check: header pragmas, the schema
// fingerprint and the journal mode. Nothing here is a function of how much the
// database holds.
func (s *Store) checkHeader(ctx context.Context) error {
	return s.read(ctx, func(tx *sql.Tx) error {
		var pageSize, pageCount int64
		if err := tx.QueryRowContext(ctx, `PRAGMA page_size`).Scan(&pageSize); err != nil {
			return wrap("page_size", err)
		}
		if err := tx.QueryRowContext(ctx, `PRAGMA page_count`).Scan(&pageCount); err != nil {
			return wrap("page_count", err)
		}
		if pageSize <= 0 || pageCount <= 0 {
			return corrupt("database header reports %d pages of %d bytes", pageCount, pageSize)
		}
		var version int
		var fingerprint string
		if err := tx.QueryRowContext(ctx, `SELECT version, fingerprint FROM schema_meta WHERE singleton = 1`).Scan(&version, &fingerprint); err != nil {
			return &model.Error{Code: model.CodeSchemaMismatch,
				Message:     "this database carries no readable schema_meta row",
				Details:     map[string]string{"path": s.path},
				Remediation: "run `codectx index --rebuild` to create a new cache; the existing database is left untouched"}
		}
		if version != schemaVersion || fingerprint != Fingerprint {
			return &model.Error{Code: model.CodeSchemaMismatch,
				Message:     "database schema fingerprint " + fingerprint + " does not match this binary's schema " + Fingerprint,
				Details:     map[string]string{"path": s.path},
				Remediation: "run `codectx index --rebuild` to create a new cache; the existing database is left untouched"}
		}
		var journal string
		if err := tx.QueryRowContext(ctx, `PRAGMA journal_mode`).Scan(&journal); err != nil {
			return wrap("journal_mode", err)
		}
		if !strings.EqualFold(journal, "wal") {
			return corrupt("database journal mode is %s, not the write-ahead log this store requires", journal)
		}
		return nil
	})
}

// checkContent is the O(database bytes) half: quick_check, the foreign key
// check and the full-text index walk. Only --deep reaches it.
func (s *Store) checkContent(ctx context.Context) error {
	err := s.read(ctx, func(tx *sql.Tx) error {
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
		// integrity-check inspects only the index's own structures; it cannot
		// report index entries no search_units row names. The vocabulary table
		// enumerates every indexed instance, so a stale document shows up here.
		// idx_search_doc serves the probe.
		var stale int64
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM search_vocab v WHERE NOT EXISTS (SELECT 1 FROM search_units su WHERE su.doc_id = v.doc)`).Scan(&stale); err != nil {
			return wrap("search_vocab", err)
		}
		if stale != 0 {
			return &model.Error{Code: model.CodeStorageCorrupt, Message: fmt.Sprintf("search index holds %d term instances for documents that no longer exist", stale),
				Remediation: "rebuild the cache with index --rebuild"}
		}
		return nil
	})
	if err != nil {
		return err
	}
	// FTS5's integrity-check is spelled as an INSERT into the virtual table, so
	// the readers' query_only=ON refuses it. It is a read in every other sense
	// and writes nothing; it is the only statement of this check that needs the
	// writer, and it holds it only for its own duration.
	return s.write(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `INSERT INTO search_fts(search_fts) VALUES('integrity-check')`); err != nil {
			return &model.Error{Code: model.CodeStorageCorrupt, Message: "search index failed its internal integrity check: " + err.Error(),
				Remediation: "rebuild the cache with index --rebuild"}
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

// isNoRows reports a missing row without leaking database/sql to callers.
func isNoRows(err error) bool { return errors.Is(err, sql.ErrNoRows) }

// StoreSizes reports the on-disk database and write-ahead-log sizes without
// running a query. It is what a shallow `doctor` reads instead of Stats, whose
// row counts are O(rows) per table.
func (s *Store) StoreSizes(ctx context.Context) (databaseBytes, walBytes int64, err error) {
	if err = ctx.Err(); err != nil {
		return 0, 0, wrap("store sizes", err)
	}
	if databaseBytes, err = fileSize(s.path, false); err != nil {
		return 0, 0, err
	}
	if walBytes, err = s.walBytes(); err != nil {
		return 0, 0, err
	}
	return databaseBytes, walBytes, nil
}
