package sqlite

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/Sawmonabo/codectx/internal/model"
)

// Stale-with-distance carry (Section 13.3). While a semantic unit is being
// rebuilt after an edit, the scope's previous sealed unit is carried into the
// new generation so the capability keeps answering — as `stale` with a
// provenance distance, never as `fresh` and never as `pending`. The carried
// unit is replaced by the fresh one at the next activation.
//
// A carried unit is exactly the unit AttachUnit refuses: its inputs differ
// from the generation's snapshot, which is what "stale" means. AttachCarried
// is therefore the one path that admits a member without input equality. It
// does not weaken the two guarantees that still apply: the unit is sealed, and
// its inputs still exist in the snapshot — a unit whose files were deleted is
// never carried, because carrying it would answer questions about source that
// is gone.
//
// The marker and the distance are stored on the membership row and folded into
// the AnalysisKey (see Activate), so a generation carrying a stale unit is
// never byte-identical to one that rebuilt it.

// Carry is how far behind a carried unit is: the number of generations, and
// the number of changed files, between the unit's snapshot and the one the
// generation is being built over. Both surface as the provenance distance on
// the capability's state (Section 13.3).
type Carry struct{ DistanceGenerations, DistanceFiles int }

func (c Carry) validate() error {
	if c.DistanceGenerations < 0 || c.DistanceFiles < 0 {
		return invalid("carry distance must not be negative")
	}
	return nil
}

// AttachCarried makes a sealed unit a stale member of gen, recording how far
// behind it is. Unlike AttachUnit it does not require the unit's inputs to be
// the snapshot's version of those files — a carried unit differs from the
// snapshot by definition — but every input must still exist in the snapshot
// (CTX_SNAPSHOT_CHANGED otherwise), because Section 13.3 does not carry a unit
// whose files were deleted.
func (s *Store) AttachCarried(ctx context.Context, gen model.GenerationID, unit model.UnitID, c Carry) error {
	key, err := idBlob("unit_id", string(unit))
	if err != nil {
		return err
	}
	if err := c.validate(); err != nil {
		return err
	}
	return s.write(ctx, func(tx *sql.Tx) error {
		g, err := s.generationRow(ctx, tx, gen, model.GenerationStaging)
		if err != nil {
			return err
		}
		var row int64
		var state model.UnitState
		var providerID, scopeKey string
		err = tx.QueryRowContext(ctx, `SELECT id, state, provider_id, scope_key FROM units WHERE unit_key = ?`, key).Scan(&row, &state, &providerID, &scopeKey)
		if isNoRows(err) {
			return invalid("unit %s does not exist", unit)
		}
		if err != nil {
			return wrap("units", err)
		}
		if state != model.UnitSealed {
			return conflict("unit %s is %s; only sealed units can be carried", unit, state)
		}
		var missing int64
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM unit_inputs ui WHERE ui.unit_id = ? AND NOT EXISTS (
			SELECT 1 FROM snapshot_files sf WHERE sf.snapshot_id = ? AND sf.file_id = ui.file_id AND sf.status <> 'deleted')`, row, g.snapshot).Scan(&missing); err != nil {
			return wrap("unit_inputs", err)
		}
		if missing != 0 {
			return &model.Error{Code: model.CodeSnapshotChanged,
				Message:     fmt.Sprintf("unit cannot be carried: %d of its inputs no longer exist in the selected snapshot", missing),
				Details:     map[string]string{"unit_id": string(unit)},
				Remediation: "rebuild the scope; a unit whose source was deleted is not carried"}
		}
		return s.attach(ctx, tx, gen, row, providerID, scopeKey, true, c)
	})
}

// CarriedUnit is one stale member of a generation with its provenance
// distance, which is what the coordinator projects into the capability report.
type CarriedUnit struct {
	Unit                 model.UnitID
	ProviderID, ScopeKey string
	Carry                Carry
}

// CarriedUnits pages gen's stale members in provider and scope order, keyset
// on that pair (two empty strings start from the beginning); limit is capped
// at model.MaxPageItems. A generation with none returns an empty slice, not an
// error: carrying nothing is the ordinary case.
//
// It is a page and not a listing because a caller must not assume it returns
// everything: a generation may carry more stale units than one response may
// hold, and refusing that with CTX_RESOURCE_LIMIT would take the capability
// report down with it. A short page — fewer rows than limit — is the end.
//
// The fan-out is real and not hypothetical. The planner (internal/index/plan,
// which storage must not import) admits up to 513 dependence units per
// language family that subdivides — 512 nested project units plus the root —
// across five such families, plus one C/C++ unit: 2566 carriable units in one
// generation, well past any single page. Page until a short page arrives.
func (s *Store) CarriedUnits(ctx context.Context, gen model.GenerationID,
	afterProviderID, afterScopeKey string, limit int) ([]CarriedUnit, error) {
	limit = pageLimit(limit)
	if len(afterProviderID) > model.MaxIdentifierBytes {
		return nil, invalid("after_provider_id is %d bytes, limit %d", len(afterProviderID), model.MaxIdentifierBytes)
	}
	if len(afterScopeKey) > model.MaxScopeKeyBytes {
		return nil, invalid("after_scope_key is %d bytes, limit %d", len(afterScopeKey), model.MaxScopeKeyBytes)
	}
	var out []CarriedUnit
	err := s.read(ctx, func(tx *sql.Tx) error {
		out = out[:0]
		rows, err := tx.QueryContext(ctx, `SELECT lower(hex(u.unit_key)), gu.provider_id, gu.scope_key, gu.distance_generations, gu.distance_files
			FROM generation_units gu JOIN units u ON u.id = gu.unit_id
			WHERE gu.generation_id = ?1 AND gu.carried = 1
				AND (gu.provider_id > ?2 OR (gu.provider_id = ?2 AND gu.scope_key > ?3))
			ORDER BY gu.provider_id, gu.scope_key LIMIT ?4`, int64(gen), afterProviderID, afterScopeKey, limit)
		if err != nil {
			return wrap("generation_units", err)
		}
		defer rows.Close()
		for rows.Next() {
			var cu CarriedUnit
			var unit string
			if err := rows.Scan(&unit, &cu.ProviderID, &cu.ScopeKey, &cu.Carry.DistanceGenerations, &cu.Carry.DistanceFiles); err != nil {
				return wrap("generation_units", err)
			}
			cu.Unit = model.UnitID(unit)
			out = append(out, cu)
		}
		return wrap("generation_units", rows.Err())
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}
