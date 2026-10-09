package sqlite

// Benchmark harness only (throwaway worktree): an observer of the store's own
// write path. It is nil unless the exporter installs it, so the index arm runs
// the production path with one nil check per call.

import (
	"context"
	"database/sql"

	"github.com/Sawmonabo/codectx/internal/model"
)

// TBObserver sees what reached the store, after the store accepted it.
type TBObserver interface {
	UnitBegun(w *UnitWriter, inputs []model.UnitInput)
	NodesPut(w *UnitWriter, facts []model.NodeFact)
	// SearchPut carries the store-computed token count and the distinct terms
	// of each document, from the store's one tokenizer pass.
	SearchPut(w *UnitWriter, docs []model.SearchUnit, tokenCounts []int64, terms [][]string)
	UnitSealed(w *UnitWriter)
}

// TBObserve is the installed observer, or nil.
var TBObserve TBObserver

// TBBuild is the unit's build record.
func (w *UnitWriter) TBBuild() model.UnitBuild { return w.build }

// TBRepo is the repository the unit's generation belongs to.
func (w *UnitWriter) TBRepo() model.RepositoryID { return w.repo }

// TBGeneration is the generation the unit was begun for.
func (w *UnitWriter) TBGeneration() model.GenerationID { return w.gen }

// TBSnapshotOf is the snapshot a generation was built over.
func (s *Store) TBSnapshotOf(ctx context.Context, gen model.GenerationID) (model.SnapshotID, error) {
	var raw []byte
	if err := s.readers.QueryRowContext(ctx, `SELECT snapshot_id FROM generations WHERE id = ?`, int64(gen)).Scan(&raw); err != nil {
		return "", err
	}
	return model.SnapshotID(idHex(raw)), nil
}

// TBRef is the ref a generation was built from.
func (s *Store) TBRef(ctx context.Context, gen model.GenerationID) (string, error) {
	var ref string
	err := s.readers.QueryRowContext(ctx, `SELECT ref FROM generations WHERE id = ?`, int64(gen)).Scan(&ref)
	return ref, err
}

// TBTableBytes is dbstat's page bytes per table or index, or an error when the
// engine was built without the dbstat virtual table.
func (s *Store) TBTableBytes(ctx context.Context) (map[string]int64, error) {
	rows, err := s.readers.QueryContext(ctx, `SELECT name, sum(pgsize) FROM dbstat GROUP BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int64{}
	for rows.Next() {
		var name string
		var n sql.NullInt64
		if err := rows.Scan(&name, &n); err != nil {
			return nil, err
		}
		out[name] = n.Int64
	}
	return out, rows.Err()
}

// TBSnapshot is the snapshot of the generation the unit was begun for.
func (w *UnitWriter) TBSnapshot(ctx context.Context) (model.SnapshotID, error) {
	return w.s.TBSnapshotOf(ctx, w.gen)
}
