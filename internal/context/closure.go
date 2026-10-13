// This file holds the closure of a context plan's kept files (ADR-0016
// decision 5): the kept-file read, which admits every lexical match inside the
// files the previous round kept, and the closure walk, which roots a walk at
// the matches inside the components of those files' unreached nodes. Both run
// only in a round whose lexical read stopped at its floor and whose inputs name
// kept files.
package context

import (
	"context"
	"errors"
	"fmt"

	"github.com/Sawmonabo/codectx/internal/graph"
	"github.com/Sawmonabo/codectx/internal/model"
	"github.com/Sawmonabo/codectx/internal/pagination"
	"github.com/Sawmonabo/codectx/internal/storage/sqlite"
)

// idRec is one identity on its way through a closure sort: a node id or a file
// id, which the closure only ever compares, deduplicates and joins.
type idRec struct {
	ID string `json:"i"`
}

func lessID(a, b idRec) int { return cmpString(a.ID, b.ID) }

// foldSameID keeps one record of an identity seen twice.
func foldSameID(a, _ idRec) (idRec, error) { return a, nil }

func sizeOfID(r idRec) int64 { return int64(len(r.ID)) + recordOverheadBytes }

// newIDSort opens a deduplicating identity sort in the compile's sort area.
func newIDSort(s *compileSorts, name string) (*pagination.ExternalSort[idRec], error) {
	sorter, err := newSort(s, name, lessID, sizeOfID)
	if err != nil {
		return nil, err
	}
	return sorter.WithFold(foldSameID), nil
}

// keptClosure is one round's closure carry on the seed sink: the kept files,
// the stop the round's read ended at, and the reached set the main walks build
// for the closure walk to subtract.
//
// The kept set is at most the plan's file budget, so it is held. reached is a
// sort, not a set in heap, and it is PRE-fold while the walks run: a halt
// between two walk pages checkpoints it with the sink's other pre-fold sorts
// and the continuation restores it with foldSameID re-attached.
type keptClosure struct {
	kept    map[model.FileID]bool
	keptIDs []model.FileID
	stop    seedStop
	reached *pagination.ExternalSort[idRec]
	// reader and query are this call's pinned reader and the clipped task
	// text, set by P-A on every leg; neither is carried by a checkpoint.
	reader *sqlite.PinnedReader
	query  string
}

// keptClosureOf answers the closure carry of a round, or nil when the round
// runs no closure: its read met every match, or it names no kept files. A read
// that met every match admitted every match inside every file already, and a
// first round has kept nothing yet.
func keptClosureOf(inputs roundInputs, stop seedStop) *keptClosure {
	if !stop.Stopped || len(inputs.Kept) == 0 {
		return nil
	}
	kept := make(map[model.FileID]bool, len(inputs.Kept))
	for _, id := range inputs.Kept {
		kept[id] = true
	}
	return &keptClosure{kept: kept, keptIDs: inputs.Kept, stop: stop}
}

// markReached records a main walk's entry when it lies in a kept file, so the
// closure walk starts only from the kept files' nodes no walk reached.
func (in *seedIngest) markReached(e model.ImpactEntry) error {
	cl := in.closure
	if cl == nil || e.FileID == "" || !cl.kept[e.FileID] {
		return nil
	}
	return cl.reached.Add(idRec{ID: string(e.NodeID)})
}

// keptFileSeeds is the kept-file read (ADR-0016 decision 5): every lexical
// match of the task text inside the files the previous round kept, read through
// the search's path filter in batches of its filter limit and paged to
// exhaustion with no stop. The filter matches a path and what lies under it,
// so a file's own path admits that file alone. Each match is admitted as a lexical seed, after the
// main read's, so a match the main read already admitted folds onto that
// earlier seed in the seed fold.
//
// Every match it admits is one the main read did not reach, so its
// contribution is at most the stop's NextContribution; it is clamped there so
// that holds by construction even for a match whose score the path filter
// changed. Such a seed is never walked: lexicalDepthCap asks whether
// mulScaled(w, strongestHop) plus every boost scaled by w exceeds Tau, and with
// w <= NextContribution and strongestHop below the full scale the left side is
// at most NextContribution plus its boosts, which IS Tau -- so its group's
// derived depth is zero. foldSeeds therefore never makes it a root
// (keptFileSeed), and its node, unreached, is one the closure walk starts
// from.
func (c *Compiler) keptFileSeeds(ctx context.Context, reader *sqlite.PinnedReader, gen model.GenerationID,
	task string, round roundInputs, out *seedSet,
) error {
	stop := out.Stop
	if !stop.Stopped || len(round.Kept) == 0 {
		return nil
	}
	const step = "the lexical matches inside the kept files"
	out.sink.BeginStep(originLexical, step)
	query := clipUTF8(task, model.MaxQueryTextBytes)
	paths, err := c.filePaths(ctx, reader, round.Kept)
	if err != nil {
		return err
	}
	for lo := 0; lo < len(paths); lo += model.MaxFilterValues {
		hi := min(lo+model.MaxFilterValues, len(paths))
		res, err := c.searchPaths(ctx, gen, query, paths[lo:hi], func(hit model.SearchHit) error {
			weight, err := lexicalContribution(hit.ScoreMicros, stop.TopScore, hit.Tier)
			if err != nil {
				return err
			}
			weight = min(weight, stop.NextContribution)
			cnd := candidate{
				NodeID:      hit.NodeID,
				FileID:      hit.FileID,
				Path:        hit.Path,
				Requirement: model.RequirementRecommended,
				Origin:      originLexical,
				Reasons: []string{boundReason(fmt.Sprintf(
					"the task text matches %q lexically inside a file the plan keeps (tier %s, score %d)",
					hit.Path, hit.Tier, hit.ScoreMicros))},
				RootWeight: weight,
			}
			if hit.Range != nil {
				cnd.StartByte = int64(hit.Range.Start.Byte)
			}
			return add(out, cnd)
		})
		if err != nil {
			return err
		}
		if res.limited {
			// As the main read: a bound the search could not serve within
			// leaves the scope incomplete rather than failing the compile.
			out.Unresolved = true
			return nil
		}
		if res.stuck != "" {
			out.notePageEnd(originLexical, step, res.stuck)
		}
	}
	return nil
}

// keptFileSeed reports whether a folded seed is one the kept-file read
// admitted: a lexical seed of a stopped read whose weight is at most the
// stop's NextContribution. Every main-read match weighs more -- it was read
// because lexicalTau of its contribution reached the floor, which
// NextContribution's does not -- so the weight alone tells the two apart.
// Such a seed is never walked (keptFileSeeds), so foldSeeds does not make it a
// root: it takes no place under context.max_start_nodes and does not decide
// whether any symbol resolved. Its node still starts the closure walk when no
// walk reaches it.
func keptFileSeed(r candRec, stop seedStop) bool {
	return stop.Stopped && r.Origin == originLexical && r.RootWeight <= stop.NextContribution
}

// pathRead is how one path-filtered search ended short of exhaustion: limited
// when the search answered CTX_RESOURCE_LIMIT, stuck with the cursor a page
// handed back unchanged.
type pathRead struct {
	limited bool
	stuck   string
}

// searchPaths reads every match of query inside paths (at most the search's
// filter limit) to exhaustion, handing each hit to each in read order. The
// generation is named on the first page only: a cursor already pins it.
func (c *Compiler) searchPaths(ctx context.Context, gen model.GenerationID, query string, paths []string,
	each func(model.SearchHit) error,
) (pathRead, error) {
	cursor := ""
	for {
		read := model.SearchRequest{
			Query: query,
			Paths: paths,
			Page:  model.PageRequest{Limit: c.pageLimit(), Cursor: cursor},
		}
		if cursor == "" {
			read.GenerationID = gen
		}
		page, err := c.search.Search(ctx, read)
		if err != nil {
			var typed *model.Error
			if errors.As(err, &typed) && typed.Code == model.CodeResourceLimit {
				return pathRead{limited: true}, nil
			}
			return pathRead{}, contextErr(ctx, err)
		}
		for _, hit := range page.Items {
			if err := each(hit); err != nil {
				return pathRead{}, err
			}
		}
		next := page.Meta.NextCursor
		if next == "" {
			return pathRead{}, nil
		}
		if next == cursor {
			// A cursor that does not advance would read the same page forever.
			return pathRead{stuck: next}, nil
		}
		cursor = next
	}
}

// filePaths answers the snapshot paths of ids, in the reader's file-id order,
// one FilesByID batch per page. A file the pinned snapshot lacks has no path
// and is not searched.
func (c *Compiler) filePaths(ctx context.Context, reader *sqlite.PinnedReader, ids []model.FileID) ([]string, error) {
	limit := c.pageLimit()
	out := make([]string, 0, len(ids))
	for lo := 0; lo < len(ids); lo += limit {
		hi := min(lo+limit, len(ids))
		files, err := reader.FilesByID(ctx, ids[lo:hi])
		if err != nil {
			return nil, contextErr(ctx, err)
		}
		for _, fv := range files {
			out = append(out, fv.Path)
		}
	}
	return out, nil
}

// closeKeptFiles is the closure walk (ADR-0016 decision 5), run once after the
// round's walk:
//
//  1. U is the kept files' nodes that are neither a walked root nor a main
//     walk's entry. A lexical root that was not walked (its derived depth is
//     zero) is in U: nothing explored its neighbourhood.
//  2. One walk from U over the scope relations, both ways, under the user's
//     bounds, gives U's components C.
//  3. The task text is searched inside C's files; the hits whose node is in C
//     or U are the closure roots R.
//  4. R is walked as one lexical group of weight Stop.NextContribution (not
//     named, requirement capped at recommended, no derived depth bound), and
//     only its entries inside a kept file are admitted, with their open call
//     sites' name-only candidates.
//
// Every closure record's key is at most (recommended, Tau), so the closure can
// add and raise entries only below the stop, inside the kept files.
//
// It never halts mid-way: P-A asks its halt question before the closure starts
// and not inside it, because what the closure has admitted cannot be taken back
// out of the candidate spool and so cannot be replayed on a continuation. A
// deadline inside it shows up as a truncated walk or search, which leaves the
// scope incomplete, or as a failed read, which fails the compile, as it does in
// the seed reads. That is the cost of not halting here: on a paged compile a
// deadline that lands inside the closure gives a plan whose scope reads
// incomplete where the uninterrupted compile's would not, which the main walk
// avoids by halting on a deadline-cut page instead of narrowing on it.
//
// Memory: the kept files' nodes, C's nodes, C's files and the hits go through
// external sorts. U and R are held, as every walk's start list is, because
// graph.Impact takes its roots as one list; U is bounded by the kept files'
// nodes. A start list wider than the walk request admits is walked in several
// walks: their union is the same component set, and for R an entity several of
// them reach keeps its best record after ranking, as for any two walks.
func (c *Compiler) closeKeptFiles(ctx context.Context, in *seedIngest, eng *graph.Engine, gen model.GenerationID) error {
	cl := in.closure
	if cl == nil {
		return nil
	}
	s := in.sorts
	for _, r := range in.roots {
		if err := cl.reached.Add(idRec{ID: string(r.Node)}); err != nil {
			return err
		}
	}
	reached, err := sortedRun(s, cl.reached)
	if err != nil {
		return err
	}
	keptNodes, err := c.keptFileNodes(ctx, s, cl)
	if err != nil {
		return err
	}
	unreached, err := joinIDs(keptNodes, reached, false)
	if err != nil {
		return err
	}
	if len(unreached) == 0 {
		return nil
	}

	compNodes, err := newIDSort(s, "closure-component-node")
	if err != nil {
		return err
	}
	compFiles, err := newIDSort(s, "closure-component-file")
	if err != nil {
		return err
	}
	for _, n := range unreached {
		if err := compNodes.Add(idRec{ID: n}); err != nil {
			return err
		}
	}
	// U's own files are kept files, so every kept file is searched; a hit is
	// kept only when its node is in C or U, so a kept file U has no node in
	// adds no root.
	for _, f := range cl.keptIDs {
		if err := compFiles.Add(idRec{ID: string(f)}); err != nil {
			return err
		}
	}
	if err := c.closureWalk(ctx, in, eng, gen, unreached, func(entries []model.ImpactEntry) error {
		for _, e := range entries {
			if err := compNodes.Add(idRec{ID: string(e.NodeID)}); err != nil {
				return err
			}
			if e.FileID != "" {
				if err := compFiles.Add(idRec{ID: string(e.FileID)}); err != nil {
					return err
				}
			}
		}
		return nil
	}); err != nil {
		return err
	}
	nodes, err := sortedRun(s, compNodes)
	if err != nil {
		return err
	}
	files, err := sortedRun(s, compFiles)
	if err != nil {
		return err
	}
	hits, err := c.searchComponents(ctx, s, cl, gen, files)
	if err != nil {
		return err
	}
	if hits.partial {
		in.scope.ScopeComplete = false
	}
	roots, err := joinIDs(hits.nodes, nodes, true)
	if err != nil {
		return err
	}
	if len(roots) == 0 {
		return nil
	}

	weight := cl.stop.NextContribution
	return c.closureWalk(ctx, in, eng, gen, roots, func(entries []model.ImpactEntry) error {
		admitted := make([]model.ImpactEntry, 0, len(entries))
		for _, e := range entries {
			if e.FileID == "" || !cl.kept[e.FileID] {
				continue
			}
			if err := in.admitEntry(e, weight, false, false); err != nil {
				return err
			}
			admitted = append(admitted, e)
		}
		// Only the open call sites the closure admits have their candidates
		// read: a site outside the kept files is not part of the plan.
		return c.admitCandidates(ctx, eng, gen, admitted, in)
	})
}

// keptFileNodes streams every node of the kept files into a deduplicating
// sort, paging PinnedReader.NodesInFile on its (start byte, node id) keyset to
// an empty page, and answers the sorted run.
func (c *Compiler) keptFileNodes(ctx context.Context, s *compileSorts, cl *keptClosure) (*pagination.SortedRun[idRec], error) {
	sorter, err := newIDSort(s, "closure-kept-node")
	if err != nil {
		return nil, err
	}
	limit := c.pageLimit()
	for _, file := range cl.keptIDs {
		var afterStart int64
		var after model.NodeID
		for {
			nodes, err := cl.reader.NodesInFile(ctx, file, afterStart, after, limit)
			if err != nil {
				return nil, contextErr(ctx, err)
			}
			if len(nodes) == 0 {
				// Only an empty page ends the file: a short page may still have
				// rows behind it, and the keyset advances on every non-empty one.
				break
			}
			for _, n := range nodes {
				afterStart, after = storedStartByte(n), n.Node.ID
				if err := sorter.Add(idRec{ID: string(n.Node.ID)}); err != nil {
					return nil, err
				}
			}
		}
	}
	return sortedRun(s, sorter)
}

// storedStartByte is the offset NodesInFile pages on: a node with no byte range
// sorts at offset zero there, so the keyset carries zero for it too.
func storedStartByte(n sqlite.StoredNode) int64 {
	if n.Bytes == nil {
		return 0
	}
	return int64(n.Bytes.Start)
}

// componentHits is what the search inside C's files found: the hit nodes as a
// sorted run, and whether a search ended short of exhaustion -- it answered
// CTX_RESOURCE_LIMIT or its cursor stopped advancing.
type componentHits struct {
	nodes   *pagination.SortedRun[idRec]
	partial bool
}

// searchComponents searches the task text inside the component files, their paths
// read in FilesByID batches and searched in batches of the filter limit, every
// search paged to exhaustion, and answers the distinct hit nodes.
func (c *Compiler) searchComponents(ctx context.Context, s *compileSorts, cl *keptClosure, gen model.GenerationID,
	files *pagination.SortedRun[idRec],
) (componentHits, error) {
	hitSort, err := newIDSort(s, "closure-hit-node")
	if err != nil {
		return componentHits{}, err
	}
	var out componentHits
	var paths []string
	search := func() error {
		if len(paths) == 0 {
			return nil
		}
		res, err := c.searchPaths(ctx, gen, cl.query, paths, func(hit model.SearchHit) error {
			if hit.NodeID == "" {
				return nil
			}
			return hitSort.Add(idRec{ID: string(hit.NodeID)})
		})
		if err != nil {
			return err
		}
		if res.limited || res.stuck != "" {
			out.partial = true
		}
		paths = paths[:0]
		return nil
	}
	limit := c.pageLimit()
	batch := make([]model.FileID, 0, limit)
	resolve := func() error {
		if len(batch) == 0 {
			return nil
		}
		got, err := c.filePaths(ctx, cl.reader, batch)
		if err != nil {
			return err
		}
		batch = batch[:0]
		for _, p := range got {
			paths = append(paths, p)
			if len(paths) == model.MaxFilterValues {
				if err := search(); err != nil {
					return err
				}
			}
		}
		return nil
	}
	if err := files.Each(func(r idRec) error {
		batch = append(batch, model.FileID(r.ID))
		if len(batch) < limit {
			return nil
		}
		return resolve()
	}); err != nil {
		return componentHits{}, err
	}
	if err := resolve(); err != nil {
		return componentHits{}, err
	}
	if err := search(); err != nil {
		return componentHits{}, err
	}
	if out.nodes, err = sortedRun(s, hitSort); err != nil {
		return componentHits{}, err
	}
	return out, nil
}

// closureWalk walks from start over the scope relations, both ways, under the
// user's bounds, every walk read to exhaustion, handing each page's entries to
// each. A start list wider than model.MaxStartNodes is walked in several walks.
//
// No page halts here (closeKeptFiles says why), so a truncated page, the
// deadline's own included, leaves the scope incomplete, and a page that stalls
// without advancing ends its walk incomplete rather than being re-issued. The
// capability rows each page discloses fold into the scope verdict as the main
// walk's do.
func (c *Compiler) closureWalk(ctx context.Context, in *seedIngest, eng *graph.Engine, gen model.GenerationID,
	start []string, each func([]model.ImpactEntry) error,
) error {
	cfg := in.cfg
	depth := cfg.MaxGraphDepth.Int()
	for lo := 0; lo < len(start); lo += model.MaxStartNodes {
		hi := min(lo+model.MaxStartNodes, len(start))
		roots := make([]model.NodeID, 0, hi-lo)
		for _, id := range start[lo:hi] {
			roots = append(roots, model.NodeID(id))
		}
		cursor := ""
		for {
			impact, err := eng.Impact(ctx, model.ImpactRequest{
				GenerationID: pinnedGeneration(gen, cursor),
				Start:        roots,
				Relations:    scopeRelations,
				Direction:    model.DirectionBoth,
				MaxDepth:     depth,
				MaxVisited:   cfg.MaxVisitedNodes.Int(),
				MaxEdges:     cfg.MaxGraphEdges.Int(),
				Page:         model.PageRequest{Limit: c.pageLimit(), Cursor: cursor},
			})
			if err != nil {
				return contextErr(ctx, err)
			}
			if impact.Meta.Truncated {
				in.scope.ScopeComplete = false
			}
			if rows := impact.Meta.Completeness; len(rows) > 0 {
				in.scope.Completeness = degradedCapabilities(nil, append(in.scope.Completeness, rows...))
				if len(in.scope.Completeness) > 0 {
					in.scope.ScopeComplete = false
				}
			}
			if err := each(impact.Entries); err != nil {
				return err
			}
			next := impact.Meta.NextCursor
			if next == "" {
				break
			}
			if next == cursor {
				// A continuation that does not advance would read the same
				// page forever; the walk ends short and says so.
				in.scope.ScopeComplete = false
				break
			}
			cursor = next
		}
	}
	return nil
}

// joinIDs merge-joins two ascending identity runs and answers the ids of left
// that are in right (keep true) or not in right (keep false), ascending. Both
// runs are deduplicated, so each side's cursor only moves forward.
func joinIDs(left, right *pagination.SortedRun[idRec], keep bool) ([]string, error) {
	next, stop := pullRun(right)
	defer stop()
	cur, have, err := next()
	if err != nil {
		return nil, err
	}
	var out []string
	if err := left.Each(func(r idRec) error {
		for have && cur.ID < r.ID {
			if cur, have, err = next(); err != nil {
				return err
			}
		}
		if (have && cur.ID == r.ID) == keep {
			out = append(out, r.ID)
		}
		return nil
	}); err != nil {
		return nil, err
	}
	return out, nil
}
