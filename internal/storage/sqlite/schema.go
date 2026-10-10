package sqlite

import (
	"context"
	"crypto/sha256"
	"database/sql"
	_ "embed"
	"encoding/hex"
	"errors"
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
			if _, err := tx.ExecContext(ctx, `INSERT INTO search_sequence(singleton, next_doc_id) VALUES(1, 1)`); err != nil {
				return wrap("search_sequence", err)
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
// A cache that holds no tables is one a writing open created and no run has
// yet published into, and it is reported as the same state Open reports when
// there is no database to open at all: the workspace that has published
// nothing. A read-only open creates no database, so it never makes that state
// itself.
func (s *Store) verifySchema(ctx context.Context) error {
	return s.read(ctx, func(tx *sql.Tx) error {
		var tables int
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM sqlite_master WHERE type = 'table'`).Scan(&tables); err != nil {
			return wrap("sqlite_master", err)
		}
		if tables == 0 {
			return notPublishedYet()
		}
		return s.checkFingerprint(ctx, tx)
	})
}

// notPublishedYet is the answer to a read-only open of a workspace nothing has
// built: the database is not there at all, or it is there and holds no tables.
// One answer for both, because they are one state to an operator and one
// remedy. Reporting it as a fingerprint that failed to match, or as a workspace
// that was never discovered, would send them to the wrong one.
func notPublishedYet() error {
	return &model.Error{Code: model.CodeNoActiveGeneration,
		Message:     "nothing has been published in this workspace yet",
		Remediation: "run `codectx index`"}
}

// adoptSchema is the writer-deferred open's schema check (Options.LazyWriter):
// the same comparison verifySchema runs, on the reader pool, with one
// difference -- a cache that holds no tables is CREATED here rather than
// reported as a workspace that has published nothing.
//
// That difference is safe and is the whole reason the check can be a read. A
// cache with no tables is one no writing open has ever completed against, so
// no run can be holding a transaction on it; every cache a run IS writing has
// its schema already, and there this open writes nothing and waits for
// nothing. The empty case is the one write an empty cache needs, and the
// alternative -- deferring it too -- would leave the read-only handle opened
// beside this one with no schema to verify.
func (s *Store) adoptSchema(ctx context.Context) error {
	empty := false
	err := s.read(ctx, func(tx *sql.Tx) error {
		var tables int
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM sqlite_master WHERE type = 'table'`).Scan(&tables); err != nil {
			return wrap("sqlite_master", err)
		}
		if tables == 0 {
			empty = true
			return nil
		}
		return s.checkFingerprint(ctx, tx)
	})
	if err != nil || !empty {
		return err
	}
	return s.initSchema(ctx)
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
	// The search index is reconciled first, while the crashed run's staging
	// is still as the crash left it (searchindex.go).
	if err := s.reconcileSearch(ctx); err != nil {
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
//     foreign key check, that every search document id lies below the
//     sequence that allocates them, and that the search index's last commit
//     holds the active generation -- the complete pass.
//
// Both run on the reader pool (`s.read`), never `s.write`: an integrity walk
// reads, and taking the writer for it serialised `doctor` against any running
// indexer for the whole walk.
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
// check and the search document checks. Only --deep reaches it.
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
		// A document id at or past the sequence was never allocated by a
		// committed transaction; Recover removes such documents from the
		// search index, and no row may name one. idx_search_doc serves it.
		var unallocated int64
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM search_units
			WHERE doc_id >= (SELECT next_doc_id FROM search_sequence WHERE singleton = 1)`).Scan(&unallocated); err != nil {
			return wrap("search_units", err)
		}
		if unallocated != 0 {
			return corrupt("%d search documents name a doc id the sequence never allocated", unallocated)
		}
		return nil
	})
	if err != nil {
		return err
	}
	active, err := s.activeGenerationID(ctx)
	if err != nil {
		return err
	}
	return s.searchHolds(active)
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
