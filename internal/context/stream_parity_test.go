package context

// stream_parity_test.go holds the whole-set pipeline the streamed passes
// replaced, kept verbatim as the REFERENCE the parity proofs compare against.
//
// A function arrives here when Compile stops calling it. Keeping the reference
// in a _test.go file rather than deleting it is deliberate: the streamed passes
// are only correct insofar as they reproduce this code's answer, and a
// reference that lives in production would be a second pipeline a caller could
// reach by accident. Nothing here may be called from a non-test file.
//
// Lane C-INT2 moved the two functions below, whose production callers Compile's
// wiring removed. `expandScope`, `rank` and `buildPlan` are still referenced by
// tests in four files of this package and have NOT been moved yet; that
// relocation, together with the helper sweep it needs, is the open half of
// C-STREAM item 5 and is recorded in C-INT2-report.md.

import (
	"context"

	"github.com/Sawmonabo/codectx/internal/model"
	"github.com/Sawmonabo/codectx/internal/storage/sqlite"
)

// hydrateFiles reads Size, Path and Status for every candidate's file in
// bounded batches and writes them onto the candidates in place.
//
// It runs ONCE, before ranking: the Section 15.3 active-change boost reads
// Status, and the Section 15.4 budget sizes an entry from Size, so a compile
// that hydrated per pass would issue the same read twice and could observe two
// different answers. The rows are returned as well as applied, so the budget
// pass consumes exactly what ranking saw.
//
// FilesByID omits an id the pinned snapshot does not hold and returns file_id
// order rather than input order, so the result is indexed by id here and a
// candidate whose file is invisible keeps a zero size, which buildPlan excludes
// with that reason rather than sizing as empty.
func (c *Compiler) hydrateFiles(ctx context.Context, reader *sqlite.PinnedReader,
	cands []candidate) ([]model.FileVersion, error) {
	seen := map[model.FileID]struct{}{}
	ids := make([]model.FileID, 0, len(cands))
	for _, cand := range cands {
		if cand.FileID == "" || cand.Excluded != "" {
			continue
		}
		if _, dup := seen[cand.FileID]; dup {
			continue
		}
		seen[cand.FileID] = struct{}{}
		ids = append(ids, cand.FileID)
	}
	limit := c.pageLimit()
	out := make([]model.FileVersion, 0, len(ids))
	for start := 0; start < len(ids); start += limit {
		batch := ids[start:min(start+limit, len(ids))]
		page, err := reader.FilesByID(ctx, batch)
		if err != nil {
			return nil, err
		}
		out = append(out, page...)
	}
	byID := make(map[model.FileID]model.FileVersion, len(out))
	for _, fv := range out {
		byID[fv.ID] = fv
	}
	for i := range cands {
		fv, ok := byID[cands[i].FileID]
		if !ok {
			continue
		}
		cands[i].SizeBytes, cands[i].Status = fv.Size, fv.Status
		if cands[i].Path == "" {
			cands[i].Path = fv.Path
		}
	}
	return out, nil
}

// relationsOnPaths reads the relation kind of every edge the expansion admitted
// onto a retained route, and reports whether it found all of them.
//
// A model.RelationPath stores relation ids only, graph.ImpactResult never
// returns the relations it walked, and the pinned reader exposes no by-id
// relation read -- adding one would be a second spelling of EdgesBatch. So the
// edges are re-read here from the candidate node set, keyset-paged by relation
// id and bounded by the same context.max_graph_edges the walk ran under, and
// filtered to the ids the routes actually name.
//
// The completeness flag matters: ranking treats an edge it cannot type as
// inadmissible, so stopping at the edge bound with ids still unfound changes
// scores. The caller discloses that as an incomplete scope rather than letting
// two compiles of one generation disagree in silence.
// It is a thin wrapper: the body moved to rankjoin.go, beside the streamed
// pass that must reproduce its read log, and reads through the narrow
// relationReader so that log is observable. Lane L5 removes this wrapper when
// Compile stops calling it.
func (c *Compiler) relationsOnPaths(ctx context.Context, reader *sqlite.PinnedReader,
	cands []candidate) (map[model.RelationID]model.Relation, bool, error) {
	return c.relationsOnPathsWholeSet(ctx, reader, cands)
}
