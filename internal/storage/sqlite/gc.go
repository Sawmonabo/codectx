package sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/Sawmonabo/codectx/internal/model"
)

// gcBatchUnits bounds one collection transaction.
const gcBatchUnits = 200

// DeleteGeneration removes a failed or superseded generation's membership and
// metadata, then collects units no retained generation or dependent unit
// reaches, in reverse dependency order through the FTS-aware deletion
// procedure, then orphan identities and origin runs (Section 12.4). A live
// lease or a retained session referencing the generation blocks deletion with
// a retryable CTX_WORKSPACE_BUSY. The tail is the shared sweep; blob
// collection is a separate grace-protocol step and is not performed here.
func (s *Store) DeleteGeneration(ctx context.Context, gen model.GenerationID) error {
	var snapshot []byte
	err := s.write(ctx, func(tx *sql.Tx) error {
		g, err := s.generationRow(ctx, tx, gen, "")
		if err != nil {
			return err
		}
		if g.status != model.GenerationFailed && g.status != model.GenerationSuperseded {
			return conflict("generation %d is %s; only failed or superseded generations are deleted", gen, g.status)
		}
		snapshot = g.snapshot
		var leases, sessions int64
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM retention_leases WHERE generation_id = ? AND expires_at > ?`, g.id, formatTime(time.Now())).Scan(&leases); err != nil {
			return wrap("retention_leases", err)
		}
		if leases != 0 {
			return &model.Error{Code: model.CodeWorkspaceBusy, Retryable: true,
				Message: fmt.Sprintf("generation %d is retained by %d live leases", gen, leases)}
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM context_manifests WHERE generation_id = ?
			AND NOT EXISTS (SELECT 1 FROM read_sessions rs WHERE rs.manifest_id = context_manifests.id)
			AND NOT EXISTS (SELECT 1 FROM session_manifests sm WHERE sm.manifest_id = context_manifests.id)`, g.id); err != nil {
			return wrap("context_manifests", err)
		}
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM read_sessions WHERE generation_id = ?`, g.id).Scan(&sessions); err != nil {
			return wrap("read_sessions", err)
		}
		if sessions != 0 {
			return &model.Error{Code: model.CodeWorkspaceBusy, Retryable: true,
				Message:     fmt.Sprintf("generation %d is referenced by %d retained sessions", gen, sessions),
				Remediation: "sessions are pruned under the closed-session retention policy before their generation can be collected"}
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM retention_leases WHERE generation_id = ?`, g.id); err != nil {
			return wrap("retention_leases", err)
		}
		_, err = tx.ExecContext(ctx, `DELETE FROM generations WHERE id = ?`, g.id)
		return wrap("generations", err)
	})
	if err != nil {
		return err
	}
	return s.sweep(ctx, time.Now(), snapshot)
}

// sweep is the collection tail shared by DeleteGeneration and Recover: it
// collects unreachable units, then in one transaction removes provider runs
// that no generation or unit references, expired leases, the snapshot(s) that
// nothing references any more, and files rows no snapshot, unit, manifest or
// session names. snapshot limits the snapshot step to one row; nil considers
// every snapshot, which only the indexing owner under its lock may do, since
// a snapshot imported for a generation not yet begun is otherwise referenced
// by nothing. Blob rows are left for the Section 10.4 grace protocol.
func (s *Store) sweep(ctx context.Context, now time.Time, snapshot []byte) error {
	if err := s.collectUnreachableUnits(ctx); err != nil {
		return err
	}
	return s.write(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `DELETE FROM provider_runs WHERE generation_id IS NULL
			AND NOT EXISTS (SELECT 1 FROM units u WHERE u.origin_run_id = provider_runs.id)`); err != nil {
			return wrap("provider_runs", err)
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM retention_leases WHERE expires_at <= ?`, formatTime(now)); err != nil {
			return wrap("retention_leases", err)
		}
		unreferenced := `NOT EXISTS (SELECT 1 FROM generations g WHERE g.snapshot_id = snapshots.id)
			AND NOT EXISTS (SELECT 1 FROM retention_leases l WHERE l.snapshot_id = snapshots.id)
			AND NOT EXISTS (SELECT 1 FROM read_sessions rs WHERE rs.snapshot_id = snapshots.id)`
		var err error
		if snapshot != nil {
			_, err = tx.ExecContext(ctx, `DELETE FROM snapshots WHERE id = ? AND `+unreferenced, snapshot)
		} else {
			_, err = tx.ExecContext(ctx, `DELETE FROM snapshots WHERE `+unreferenced)
		}
		if err != nil {
			return wrap("snapshots", err)
		}
		_, err = tx.ExecContext(ctx, `DELETE FROM files WHERE
			NOT EXISTS (SELECT 1 FROM snapshot_files sf WHERE sf.file_id = files.id)
			AND NOT EXISTS (SELECT 1 FROM unit_inputs ui WHERE ui.file_id = files.id)
			AND NOT EXISTS (SELECT 1 FROM node_facts nf WHERE nf.file_id = files.id)
			AND NOT EXISTS (SELECT 1 FROM evidence e WHERE e.file_id = files.id)
			AND NOT EXISTS (SELECT 1 FROM search_units su WHERE su.file_id = files.id)
			AND NOT EXISTS (SELECT 1 FROM context_entries ce WHERE ce.file_id = files.id)
			AND NOT EXISTS (SELECT 1 FROM session_files s WHERE s.file_id = files.id)`)
		return wrap("files", err)
	})
}

// collectUnreachableUnits deletes, in bounded batches, every non-building unit
// that no generation selects and no remaining unit depends on. Each pass
// removes leaves only, so dependencies are deleted after their dependents.
func (s *Store) collectUnreachableUnits(ctx context.Context) error {
	return s.collectUnits(ctx, `SELECT u.id FROM units u WHERE u.state <> 'building'
		AND NOT EXISTS (SELECT 1 FROM generation_units gu WHERE gu.unit_id = u.id)
		AND NOT EXISTS (SELECT 1 FROM unit_dependencies ud WHERE ud.dependency_id = u.id) LIMIT ?2`, 0)
}

// collectUnits repeatedly selects up to gcBatchUnits unit rows with query
// (bound ?1 = arg, ?2 = batch size) and deletes them through deleteUnit, one
// transaction per batch, until the query returns nothing.
func (s *Store) collectUnits(ctx context.Context, query string, arg int64) error {
	for {
		var deleted int
		err := s.write(ctx, func(tx *sql.Tx) error {
			rows, err := tx.QueryContext(ctx, query, arg, gcBatchUnits)
			if err != nil {
				return wrap("units", err)
			}
			ids := make([]int64, 0, gcBatchUnits)
			for rows.Next() {
				var id int64
				if err := rows.Scan(&id); err != nil {
					rows.Close()
					return wrap("units", err)
				}
				ids = append(ids, id)
			}
			rows.Close()
			if err := rows.Err(); err != nil {
				return wrap("units", err)
			}
			for _, id := range ids {
				if err := s.deleteUnit(ctx, tx, id); err != nil {
					return err
				}
			}
			deleted = len(ids)
			return nil
		})
		if err != nil || deleted == 0 {
			return err
		}
	}
}

// deleteUnit is the one unit deletion procedure (Section 12.4). It issues the
// FTS delete with each document's indexed values before the content rows go,
// removes facts, then the identities only this unit referenced, then the unit.
// Callers never `DELETE FROM units` directly. Membership rows are deliberately
// not touched: every caller has already established the unit is unreachable,
// and if one ever were not, the generation_units foreign key aborts the
// transaction rather than silently shrinking a retained generation.
func (s *Store) deleteUnit(ctx context.Context, tx *sql.Tx, unitRow int64) error {
	steps := []string{
		`CREATE TEMP TABLE IF NOT EXISTS gc_nodes(id BLOB PRIMARY KEY) WITHOUT ROWID`,
		`CREATE TEMP TABLE IF NOT EXISTS gc_relations(id BLOB PRIMARY KEY) WITHOUT ROWID`,
		`INSERT OR IGNORE INTO gc_nodes SELECT node_id FROM node_facts WHERE unit_id = ?1`,
		`INSERT OR IGNORE INTO gc_nodes SELECT node_id FROM native_aliases WHERE unit_id = ?1`,
		`INSERT OR IGNORE INTO gc_relations SELECT relation_id FROM relation_facts WHERE unit_id = ?1`,
		`INSERT INTO search_fts(search_fts, rowid, name, qualified_name, signature, path, body)
			SELECT 'delete', rowid, name, qualified_name, signature, path, body FROM search_units WHERE unit_id = ?1`,
		`DELETE FROM search_units WHERE unit_id = ?1`,
		`DELETE FROM evidence WHERE unit_id = ?1`,
		`DELETE FROM fact_keys WHERE unit_id = ?1`,
		`DELETE FROM native_aliases WHERE unit_id = ?1`,
		`DELETE FROM relation_facts WHERE unit_id = ?1`,
		`DELETE FROM node_facts WHERE unit_id = ?1`,
		`DELETE FROM relation_ids WHERE id IN (SELECT id FROM gc_relations)
			AND NOT EXISTS (SELECT 1 FROM relation_facts rf WHERE rf.relation_id = relation_ids.id)`,
		`DELETE FROM node_ids WHERE id IN (SELECT id FROM gc_nodes)
			AND NOT EXISTS (SELECT 1 FROM node_facts nf WHERE nf.node_id = node_ids.id)
			AND NOT EXISTS (SELECT 1 FROM native_aliases na WHERE na.node_id = node_ids.id)
			AND NOT EXISTS (SELECT 1 FROM relation_ids ri WHERE ri.from_node_id = node_ids.id OR ri.to_node_id = node_ids.id)
			AND NOT EXISTS (SELECT 1 FROM context_entries ce WHERE ce.node_id = node_ids.id)`,
		`DELETE FROM gc_nodes`,
		`DELETE FROM gc_relations`,
		`DELETE FROM unit_delta_state WHERE unit_id = ?1`,
		`DELETE FROM unit_inputs WHERE unit_id = ?1`,
		`DELETE FROM unit_dependencies WHERE unit_id = ?1`,
		`DELETE FROM units WHERE id = ?1`,
	}
	for _, q := range steps {
		if _, err := tx.ExecContext(ctx, q, unitRow); err != nil {
			return wrap("delete unit", err)
		}
	}
	return nil
}
