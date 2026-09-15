package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/Sawmonabo/codectx/internal/model"
)

// Retention by distinct ref (Section 12.4). The store keeps the results of the
// last retain_refs refs the user actually indexed, not the last N generations:
// switching A -> B -> C -> A must find A's units still on disk and reuse them
// without a run. Each activated generation records the ref it was built from
// (generations.ref); a ref's most recent generation is retained while that ref
// is among the last retain_refs worked on, and every unit a retained
// generation selects is retained with it.

// maxRetainRefs bounds the configured ref window. Retention walks one query
// per retained ref, so the window is a finite loop bound, not an open number.
const maxRetainRefs = 1024

// maxSweptGenerations bounds one retention pass. A pass runs at activation, so
// a backlog drains over calls rather than turning one pass into an unbounded
// deletion loop.
const maxSweptGenerations = model.MaxRecordsPerResult

// RetentionPolicy is the user-set retention window of Section 20.1.
// MaxRetainedBytes is the second setting where 0 is meaningful: the retained
// store has no default size limit, so 0 leaves retention governed by
// RetainRefs alone. A nonzero value evicts least-recently-used refs first and
// never the active generation's ref.
type RetentionPolicy struct {
	RetainRefs       int
	MaxRetainedBytes int64
}

// bytesBounded and overRetained are the one place this package spells the
// zero-means-unlimited comparison for MaxRetainedBytes, mirroring
// config.Limit's own accessors (IsUnlimited and Exceeded -- strictly greater)
// exactly. The field arrives here as an int64 because this package takes its
// policy from a caller rather than from configuration, so the semantics travel
// as these two methods rather than as a hand-rolled `== 0` at each comparison.
func (p RetentionPolicy) bytesBounded() bool { return p.MaxRetainedBytes > 0 }

func (p RetentionPolicy) overRetained(total int64) bool {
	return p.bytesBounded() && total > p.MaxRetainedBytes
}

// RetentionReport is what one pass did and what a stricter limit could still
// reclaim. BytesReclaimable is measured even when MaxRetainedBytes is 0
// (unlimited), so `status` can warn before a disk fills: it is the accounted
// size of the retained non-active generations this pass kept, including any it
// skipped because a live lease still holds them.
//
// Both byte figures are this store's own record accounting — the fixed
// per-record overhead plus the stored text — over the fact, evidence, alias,
// search and delta-state rows of a generation's units, and both count only the
// units the generation holds EXCLUSIVELY, which are the ones deleting it would
// actually free; a unit another generation still selects reclaims nothing. The
// size max_retained_bytes is measured against is a different question and
// counts every unit a retained generation selects, shared or not (see
// keepByRef). Both exclude FTS index overhead, page rounding and the content
// blobs, which snapshots and the Section 10.4 grace protocol retain, not
// generations. The figures are an accounting estimate for a warning, not a
// page count.
type RetentionReport struct {
	RefsRetained     int
	GenerationsSwept int
	UnitsDeleted     int
	BytesReclaimed   int64
	BytesReclaimable int64
}

// RetainByRef applies the policy to repo and reports what it did. It keeps the
// most recent generation of each of the last p.RetainRefs distinct refs, and
// when p.MaxRetainedBytes is set evicts the least recently used of those refs
// until the accounted retained size fits, never evicting the active
// generation's ref and never touching a staging generation (Recover owns
// those). Failed generations are swept unconditionally: a generation that
// never published is not a result the user indexed.
//
// A generation a live lease or a retained session still holds is skipped
// rather than failing the pass; it is retained until its lease expires and
// counts toward BytesReclaimable.
func (s *Store) RetainByRef(ctx context.Context, repo model.RepositoryID, p RetentionPolicy, now time.Time) (RetentionReport, error) {
	repoRaw, err := idBlob("repository_id", string(repo))
	if err != nil {
		return RetentionReport{}, err
	}
	if p.RetainRefs < 1 || p.RetainRefs > maxRetainRefs {
		return RetentionReport{}, invalid("retain_refs must be between 1 and %d", maxRetainRefs)
	}
	if p.MaxRetainedBytes < 0 {
		return RetentionReport{}, invalid("max_retained_bytes must not be negative")
	}

	var active int64
	if err := s.read(ctx, func(tx *sql.Tx) error {
		err := activeGeneration(ctx, tx, repoRaw, &active)
		var typed *model.Error
		if errors.As(err, &typed) && typed.Code == model.CodeNoActiveGeneration {
			return nil
		}
		return err
	}); err != nil {
		return RetentionReport{}, err
	}

	kept, err := s.keepByRef(ctx, repoRaw, active, p)
	if err != nil {
		return RetentionReport{}, err
	}
	report := RetentionReport{RefsRetained: len(kept)}

	unitsBefore, err := s.countUnits(ctx, repoRaw)
	if err != nil {
		return RetentionReport{}, err
	}
	keptGens := make(map[int64]bool, len(kept)+1)
	for _, k := range kept {
		keptGens[k.generation] = true
	}
	keptGens[active] = true
	candidates, err := s.sweepCandidates(ctx, repoRaw, keptGens)
	if err != nil {
		return RetentionReport{}, err
	}
	for _, gen := range candidates {
		bytes, err := s.generationBytes(ctx, gen, true)
		if err != nil {
			return RetentionReport{}, err
		}
		err = s.DeleteGeneration(ctx, model.GenerationID(gen))
		var typed *model.Error
		if errors.As(err, &typed) && typed.Code == model.CodeWorkspaceBusy {
			// Held by a live lease or a retained session: retained for now,
			// and still reclaimable once that reference is gone.
			report.BytesReclaimable += bytes
			continue
		}
		if err != nil {
			return RetentionReport{}, err
		}
		report.GenerationsSwept++
		report.BytesReclaimed += bytes
	}
	// The collection tail: leases that expired by now, the snapshots nothing
	// references any more and their orphaned file rows. Running it with no
	// snapshot filter is the indexing owner's prerogative, and retention runs
	// under that lock, so a snapshot captured for a generation that was never
	// begun is collected here rather than never.
	if err := s.sweep(ctx, now, nil); err != nil {
		return RetentionReport{}, err
	}
	unitsAfter, err := s.countUnits(ctx, repoRaw)
	if err != nil {
		return RetentionReport{}, err
	}
	report.UnitsDeleted = int(unitsBefore - unitsAfter)

	// What a stricter max_retained_bytes could still free: everything retained
	// by ref that is not the published generation.
	for _, k := range kept {
		if k.generation == active {
			continue
		}
		bytes, err := s.generationBytes(ctx, k.generation, true)
		if err != nil {
			return RetentionReport{}, err
		}
		report.BytesReclaimable += bytes
	}
	return report, nil
}

// retainedRef is one kept ref and the generation retention keeps for it.
type retainedRef struct {
	ref        string
	generation int64
	bytes      int64
}

// keepByRef ranks the repository's refs by the most recent generation each was
// activated for, keeps the newest generation of the first p.RetainRefs of
// them, and then, when p.MaxRetainedBytes is set, drops the least recently
// used of those until the accounted retained size fits. That size is what the
// store is holding, so it counts every unit a retained generation selects,
// including units it shares with another retained generation. The active
// generation's ref is never dropped, so a limit no retention can satisfy
// leaves the published generation standing rather than emptying the store. Ranking ties on activated_at are broken
// by generation id, so two generations published in the same clock tick still
// order deterministically.
func (s *Store) keepByRef(ctx context.Context, repoRaw []byte, active int64, p RetentionPolicy) ([]retainedRef, error) {
	var kept []retainedRef
	err := s.read(ctx, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, `SELECT ref FROM generations
			WHERE repository_id = ? AND status IN ('active','superseded') AND activated_at IS NOT NULL
			GROUP BY ref ORDER BY max(activated_at) DESC, max(id) DESC LIMIT ?`, repoRaw, p.RetainRefs)
		if err != nil {
			return wrap("generations", err)
		}
		defer rows.Close()
		for rows.Next() {
			var ref string
			if err := rows.Scan(&ref); err != nil {
				return wrap("generations", err)
			}
			kept = append(kept, retainedRef{ref: ref})
		}
		return wrap("generations", rows.Err())
	})
	if err != nil {
		return nil, err
	}
	for i := range kept {
		err := s.read(ctx, func(tx *sql.Tx) error {
			return wrap("generations", tx.QueryRowContext(ctx, `SELECT id FROM generations
				WHERE repository_id = ? AND ref = ? AND status IN ('active','superseded') AND activated_at IS NOT NULL
				ORDER BY activated_at DESC, id DESC LIMIT 1`, repoRaw, kept[i].ref).Scan(&kept[i].generation))
		})
		if err != nil {
			return nil, err
		}
	}
	if !p.bytesBounded() {
		return kept, nil
	}
	var total int64
	for i := range kept {
		if kept[i].bytes, err = s.generationBytes(ctx, kept[i].generation, false); err != nil {
			return nil, err
		}
		total += kept[i].bytes
	}
	// kept is most-recent first, so eviction walks it from the back.
	for i := len(kept) - 1; i >= 0 && p.overRetained(total); i-- {
		if kept[i].generation == active {
			continue
		}
		total -= kept[i].bytes
		kept = append(kept[:i], kept[i+1:]...)
	}
	return kept, nil
}

// sweepCandidates lists the repository's failed and superseded generations
// that no retained ref keeps, oldest first, so a bounded pass removes the
// least recently used work first. Staging generations are never candidates.
func (s *Store) sweepCandidates(ctx context.Context, repoRaw []byte, kept map[int64]bool) ([]int64, error) {
	var out []int64
	err := s.read(ctx, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, `SELECT id FROM generations
			WHERE repository_id = ? AND status IN ('failed','superseded')
			ORDER BY coalesce(activated_at, created_at) ASC, id ASC LIMIT ?`, repoRaw, maxSweptGenerations)
		if err != nil {
			return wrap("generations", err)
		}
		defer rows.Close()
		for rows.Next() {
			var id int64
			if err := rows.Scan(&id); err != nil {
				return wrap("generations", err)
			}
			if !kept[id] {
				out = append(out, id)
			}
		}
		return wrap("generations", rows.Err())
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// countUnits counts the units some generation of repo selects, which is what
// RetentionReport.UnitsDeleted is about. The store admits several repositories
// (repositories is a table and generations.repository_id references it) while
// the collection tail retention brackets is store-wide, so an unscoped count
// would attribute another repository's collected units to this one.
func (s *Store) countUnits(ctx context.Context, repoRaw []byte) (int64, error) {
	var n int64
	err := s.read(ctx, func(tx *sql.Tx) error {
		return wrap("units", tx.QueryRowContext(ctx, `SELECT count(*) FROM units u WHERE EXISTS (
			SELECT 1 FROM generation_units gu JOIN generations g ON g.id = gu.generation_id
			WHERE gu.unit_id = u.id AND g.repository_id = ?)`, repoRaw).Scan(&n))
	})
	return n, err
}

// generationBytes accounts the rows of a generation's units with this store's
// own record accounting (recordOverhead per row plus the stored text, in
// bytes: length() counts characters, so text columns are cast to BLOB first).
//
// With exclusive set it counts only the units no other generation selects —
// exactly the units that become collectable when this generation is deleted,
// which is what "reclaimed" and "reclaimable" mean. Without it, it counts
// every unit the generation selects, which is what the generation is holding
// and therefore what a size limit is measured against. A unit kept alive only
// by a dependency edge from a surviving unit is counted as exclusive although
// collection will not take it, so neither figure under-counts what is
// retained.
func (s *Store) generationBytes(ctx context.Context, gen int64, exclusive bool) (int64, error) {
	const members = `WITH ex(id) AS (SELECT gu.unit_id FROM generation_units gu WHERE gu.generation_id = ?1)`
	const exclusiveMembers = `WITH ex(id) AS (
		SELECT gu.unit_id FROM generation_units gu WHERE gu.generation_id = ?1
		AND NOT EXISTS (SELECT 1 FROM generation_units g2 WHERE g2.unit_id = gu.unit_id AND g2.generation_id <> ?1))`
	const accounting = `
	SELECT
		(SELECT count(*) * ?2 + coalesce(sum(length(cast(language AS BLOB)) + length(cast(name AS BLOB))
			+ length(cast(qualified_name AS BLOB)) + length(cast(signature AS BLOB)) + length(cast(metadata_json AS BLOB))), 0)
			FROM node_facts WHERE unit_id IN (SELECT id FROM ex))
	  + (SELECT count(*) * ?2 FROM relation_facts WHERE unit_id IN (SELECT id FROM ex))
	  + (SELECT count(*) * ?2 + coalesce(sum(length(cast(native_key AS BLOB)) + length(cast(detail AS BLOB))), 0)
			FROM evidence WHERE unit_id IN (SELECT id FROM ex))
	  + (SELECT count(*) * ?2 + coalesce(sum(length(cast(fact_key AS BLOB))), 0)
			FROM fact_keys WHERE unit_id IN (SELECT id FROM ex))
	  + (SELECT count(*) * ?2 + coalesce(sum(length(cast(scope_key AS BLOB)) + length(cast(native_key AS BLOB))), 0)
			FROM native_aliases WHERE unit_id IN (SELECT id FROM ex))
	  + (SELECT count(*) * ?2 + coalesce(sum(length(cast(name AS BLOB)) + length(cast(qualified_name AS BLOB))
			+ length(cast(signature AS BLOB)) + length(cast(path AS BLOB)) + length(cast(body AS BLOB))), 0)
			FROM search_units WHERE unit_id IN (SELECT id FROM ex))
	  + (SELECT count(*) * ?2 + coalesce(sum(length(payload)), 0) FROM unit_delta_state WHERE unit_id IN (SELECT id FROM ex))`
	cte := members
	if exclusive {
		cte = exclusiveMembers
	}
	var bytes int64
	err := s.read(ctx, func(tx *sql.Tx) error {
		return wrap("retention accounting", tx.QueryRowContext(ctx, cte+accounting, gen, int64(recordOverhead)).Scan(&bytes))
	})
	return bytes, err
}
