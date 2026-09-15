// This file is C-STREAM pass P-C (relation attributes) and pass P-E
// (walk-local centrality) of .superpowers/sdd/implementation-plan/C-STREAM-plan.md,
// plus the two whole-set functions they replace, moved here so that the
// streaming form and the form it must reproduce sit side by side.
//
// Both passes exist to delete a candidate-sized map. relationsOnPaths holds
// `wanted` (every relation id on every retained route) and `out` (the relation
// row for each), resolvePrecision holds one multiplier per relation, and rank
// holds `centrality map[pkg]map[RelationID]struct{}`. None of the three has a
// bound: they are functions of the walk, and the walk is a function of the
// repository. Here each becomes a sort-merge join or a streaming aggregation,
// so the heap a compile holds is the sort run buffer and one page of reads.
//
// The parity contract of this file is the READ LOG. The streamed passes must
// issue exactly the EdgesBatch and EvidenceBatch calls today's two functions
// issue, in the same order, with the same batch contents -- same node batches
// in the same order, same keyset cursor, same `scanned` budget accounting, same
// early exit, and the same ascending de-duplicated relation-id batches for
// evidence. A divergence there is a divergence in what the store is asked, and
// the store's answer is what the ranking is built from.
package context

import (
	"context"

	"github.com/Sawmonabo/codectx/internal/model"
	"github.com/Sawmonabo/codectx/internal/pagination"
	"github.com/Sawmonabo/codectx/internal/storage/sqlite"
)

// relationReader is the slice of sqlite.PinnedReader these passes read through:
// the batched edge scan and the batched evidence read, and nothing else.
//
// It exists so the read log is observable. The parity proof of P-C is that the
// streamed pass and the whole-set functions below issue the identical sequence
// of calls on one fixture, and a concrete *sqlite.PinnedReader cannot be asked
// what it was called with. *sqlite.PinnedReader satisfies this interface, so no
// caller changes and no behaviour is routed through a second implementation in
// production.
type relationReader interface {
	EdgesBatch(ctx context.Context, nodes []model.NodeID, direction model.Direction,
		kinds []model.RelationKind, after model.RelationID, limit int) ([]model.Relation, error)
	EvidenceBatch(ctx context.Context, relations []model.RelationID,
		perRelation int) (map[model.RelationID][]sqlite.StoredEvidence, error)
}

// ---------------------------------------------------------------------------
// The whole-set forms, moved
// ---------------------------------------------------------------------------

// relationsOnPathsWholeSet is today's relationsOnPaths (compiler.go), moved
// verbatim except that it reads through relationReader. compiler.go keeps a
// one-line wrapper so every existing caller and test is unchanged; lane L5
// deletes the wrapper when Compile stops calling it.
//
// It is kept, and not merely copied into a test, because it is the definition
// the streamed pass is proved against: a copy in a test file could drift from
// the code that shipped, and then the proof would compare the stream to a
// fiction.
// edgeScanBudget answers the edge scan's stop condition: how many rows the
// scan may read, and whether that bound exists at all.
//
// context.max_graph_edges is Unlimited by default, and reading an absent bound
// as model.MaxPageItems (which is what ValueOr did here) made the default
// stricter than any value a user could type: a scope wanting more relations
// than one page carries stopped after 200 rows, left most of its routes
// untyped and reported scope_complete=false. An unlimited bound now means what
// it says -- the scan pages the keyset to exhaustion. The page WIDTH is
// unchanged and is not a ceiling: it is the lossless keyset page the reader is
// asked for, and the scan keeps asking for the next one.
func (c *Compiler) edgeScanBudget() (int, bool) {
	l := c.cfg.Context.MaxGraphEdges
	if l.IsUnlimited() {
		return 0, false
	}
	return l.Int(), true
}

// ---------------------------------------------------------------------------
// The records and comparators these two passes own
// ---------------------------------------------------------------------------

// nodeRec is one candidate's node identity with the ingest sequence that found
// it: the streaming form of relationsOnPaths' `nodes []NodeID` + `seenNode`
// map. It is a projection and not a candRec because the node pass is sorted
// twice -- once by identity to deduplicate, once by sequence to restore the
// order the edge batches are cut in -- and carrying a whole candidate through
// both would size the run buffer by the candidate payload instead of by the
// two fields the pass reads.
type nodeRec struct {
	NodeID model.NodeID `json:"n"`
	Seq    int64        `json:"q"`
}

// lessNodeID groups node records by identity for the dedupe fold. A join
// comparator: identity alone, so foldMinNodeSeq sees every repeat of one node.
func lessNodeID(a, b nodeRec) int { return cmpString(string(a.NodeID), string(b.NodeID)) }

// lessNodeSeq restores the deduplicated node stream to ingest order, which is
// the order relationsOnPaths appends `nodes` in and therefore the order the
// EdgesBatch batches are cut in. Total: one node survives the dedupe with one
// sequence.
func lessNodeSeq(a, b nodeRec) int { return cmpInt(a.Seq, b.Seq) }

// foldMinNodeSeq keeps the earliest arrival of a repeated node, which is the
// one `seenNode` admits and every later one it ignores. Order-independent.
func foldMinNodeSeq(a, b nodeRec) (nodeRec, error) {
	if b.Seq < a.Seq {
		return b, nil
	}
	return a, nil
}

// sizeOfNode charges a buffered node record: its identity bytes plus a fixed
// allowance for the struct and its string header, on the same convention as
// stream.go's sizeOf family.
func sizeOfNode(r nodeRec) int64 { return int64(len(r.NodeID)) + recordOverheadBytes }

// ---------------------------------------------------------------------------
// P-C -- relation attributes
// ---------------------------------------------------------------------------

// relationAttributes is the result of P-C: the hop stream with every hop's kind
// and precision multiplier resolved, and the completeness verdict the manifest
// folds into scope_complete.
//
// Complete is today's `len(out) == len(wanted)`: whether the edge scan typed
// every relation the retained routes name. A route with an untyped edge is
// scored as inadmissible (rank.go:316-319), so an incomplete scan changes the
// ranking and must be disclosed rather than left to differ in silence between
// two compiles of one generation.
type relationAttributes struct {
	Hops     *pagination.SortedRun[hopRec]
	Complete bool
}

// passCRelationAttributes resolves every hop's relation kind and precision
// multiplier without holding a relation-keyed map, and reports whether the edge
// scan saw every relation the routes name.
//
// hops is every hop of every retained route, and cands is the candidate spool
// in ingest order -- BOTH including excluded candidates, because
// relationsOnPaths deliberately does not filter on Excluded: an excluded
// candidate's routes still contribute relation ids to `wanted` and its node id
// still contributes to the scanned node set, so diverting those records would
// shrink the node batches and change the read log.
//
// The passes, in the order the read log requires. Compile calls
// relationsOnPaths before rank, so the edge scan's reads precede the evidence
// reads, and the two are not interleaved:
//
//  1. Sort the hops by relation id. The run is the join side for step 5, and
//     its de-duplicated key stream IS the ascending distinct relation-id
//     sequence resolvePrecision builds by sorting its collected ids.
//  2. Deduplicate the candidate node ids by identity keeping the earliest
//     sequence, then restore ingest order: the node list relationsOnPaths cuts
//     its EdgesBatch batches from.
//  3. Scan edges exactly as relationsOnPaths does, merge-matching each ascending
//     keyset page against the wanted stream instead of probing a map.
//  4. Resolve precision exactly as resolvePrecision does, over the same
//     ascending de-duplicated ids in the same pageLimit batches.
//  5. Merge the two attribute streams by relation id (foldRelAttr is
//     mostPrecise plus "whichever side carries a kind") and join the result onto
//     the hop run, then restore route order so P-D can rebuild one candidate's
//     routes from a contiguous run.
func (c *Compiler) passCRelationAttributes(ctx context.Context, s *compileSorts, reader relationReader,
	hops *pagination.SortedRun[hopRec], cands *pagination.SortedRun[candRec]) (relationAttributes, error) {
	if reader == nil {
		return relationAttributes{}, argumentInvalid("resolving relation attributes requires a pinned reader")
	}

	byRelation, err := newSort[hopRec](s, "hop-relid", lessRelID, sizeOfHop)
	if err != nil {
		return relationAttributes{}, err
	}
	if err := hops.Each(func(h hopRec) error { return byRelation.Add(h) }); err != nil {
		return relationAttributes{}, err
	}
	hopsByRelation, err := byRelation.Sorted()
	if err != nil {
		return relationAttributes{}, err
	}
	hopsByRelation = trackRun(s, hopsByRelation)

	// wanted is counted, not collected: the count is every comparison today's
	// `len(wanted)` is used in, and the ids themselves are re-read from the
	// sorted run each time a pass needs them.
	wanted, err := distinctRelationCount(hopsByRelation)
	if err != nil {
		return relationAttributes{}, err
	}
	nodes, err := c.scanNodes(s, cands)
	if err != nil {
		return relationAttributes{}, err
	}

	// The zero-read path, reproduced deliberately: with no wanted relation or
	// no node, relationsOnPaths returns before its first EdgesBatch and
	// resolvePrecision returns before its first EvidenceBatch, and complete is
	// "there was nothing to find".
	if wanted == 0 || nodes.Len() == 0 {
		return relationAttributes{Hops: hops, Complete: wanted == 0}, nil
	}

	attrs, err := newSort[relAttrRec](s, "rel-attr", lessRelAttr, sizeOfRelAttr)
	if err != nil {
		return relationAttributes{}, err
	}
	attrs = attrs.WithFold(foldRelAttr)

	matched, err := c.scanEdgeKinds(ctx, reader, hopsByRelation, nodes, wanted, attrs)
	if err != nil {
		return relationAttributes{}, err
	}
	if err := c.resolveEvidencePrecision(ctx, reader, hopsByRelation, attrs); err != nil {
		return relationAttributes{}, err
	}

	attributed, err := joinHopAttributes(s, hopsByRelation, attrs)
	if err != nil {
		return relationAttributes{}, err
	}
	return relationAttributes{Hops: attributed, Complete: matched == wanted}, nil
}

// distinctRelationCount counts the distinct relation ids in a run ordered by
// relation id: today's len(wanted). The empty id is counted, because
// relationsOnPaths puts every id a route names into `wanted` with no filter --
// the asymmetry with resolvePrecision, which skips it, is load-bearing and is
// preserved on both sides.
func distinctRelationCount(run *pagination.SortedRun[hopRec]) (int64, error) {
	var count int64
	var prev model.RelationID
	first := true
	err := run.Each(func(h hopRec) error {
		if first || h.RelationID != prev {
			count++
			prev, first = h.RelationID, false
		}
		return nil
	})
	return count, err
}

// scanNodes is the streaming form of relationsOnPaths' `nodes`/`seenNode` pair:
// the candidate node ids, each appearing once, in the order of their first
// arrival. Two sorts and no map.
func (c *Compiler) scanNodes(s *compileSorts, cands *pagination.SortedRun[candRec]) (*pagination.SortedRun[nodeRec], error) {
	distinct, err := newSort[nodeRec](s, "node-id", lessNodeID, sizeOfNode)
	if err != nil {
		return nil, err
	}
	distinct = distinct.WithFold(foldMinNodeSeq)
	if err := cands.Each(func(r candRec) error {
		if r.NodeID == "" {
			return nil
		}
		return distinct.Add(nodeRec{NodeID: r.NodeID, Seq: r.Seq})
	}); err != nil {
		return nil, err
	}
	deduped, err := distinct.Sorted()
	if err != nil {
		return nil, err
	}
	deduped = trackRun(s, deduped)

	ordered, err := newSort[nodeRec](s, "node-seq", lessNodeSeq, sizeOfNode)
	if err != nil {
		return nil, err
	}
	if err := deduped.Each(func(r nodeRec) error { return ordered.Add(r) }); err != nil {
		return nil, err
	}
	run, err := ordered.Sorted()
	if err != nil {
		return nil, err
	}
	return trackRun(s, run), nil
}

// scanEdgeKinds re-reads the edges incident to the candidate nodes and writes
// the kind of every one the routes name, returning how many distinct wanted
// relations it typed -- today's len(out).
//
// Every loop bound is relationsOnPaths': pageLimit node batches in ingest
// order, a keyset cursor per batch, `scanned` counting every returned row and
// not the matches, the same MaxGraphEdges budget, and the same early exit once
// every wanted relation is typed. The one difference is the membership test: a
// keyset page is ascending in relation id and the wanted stream is ascending in
// relation id, so a merge cursor answers it in one forward walk per node batch.
//
// The matched set is a bit per distinct wanted relation, indexed by the
// relation's ordinal in the wanted stream. It is the one structure here that is
// a function of the walk rather than of a page, and it is what preserves the
// early exit: without an exact running count of distinct typed relations the
// scan cannot know it is done, and it would page every node batch to exhaustion
// -- more reads than today, on exactly the repositories this wave serves.
func (c *Compiler) scanEdgeKinds(ctx context.Context, reader relationReader,
	wantedRun *pagination.SortedRun[hopRec], nodes *pagination.SortedRun[nodeRec],
	wanted int64, attrs *pagination.ExternalSort[relAttrRec]) (int64, error) {
	limit := c.pageLimit()
	budget, bounded := c.edgeScanBudget()
	seen := newBitset(wanted)
	scanned := 0

	batch := make([]model.NodeID, 0, limit)
	stopped := false
	flush := func() error {
		if len(batch) == 0 || stopped || seen.count == wanted {
			return nil
		}
		// One cursor per node batch: the keyset restarts at the batch, so the
		// merge restarts with it. The cursor is closed on every exit path of
		// this batch, including the error ones.
		cursor := newRunCursor(wantedRun)
		defer cursor.Close()
		var after model.RelationID
		for seen.count < wanted {
			page, err := reader.EdgesBatch(ctx, batch, model.DirectionBoth, scopeRelations, after, limit)
			if err != nil {
				return err
			}
			for _, rel := range page {
				if pos, ok := cursor.seek(hopRec{RelationID: rel.ID}, lessRelID); ok && seen.set(pos) {
					if err := attrs.Add(relAttrRec{RelationID: rel.ID, Kind: rel.Kind}); err != nil {
						return err
					}
				}
				after = rel.ID
			}
			scanned += len(page)
			if len(page) < limit || (bounded && scanned >= budget) {
				break
			}
		}
		if err := cursor.Close(); err != nil {
			return err
		}
		if bounded && scanned >= budget {
			stopped = true
		}
		batch = batch[:0]
		return nil
	}

	if err := nodes.Each(func(r nodeRec) error {
		if stopped || seen.count == wanted {
			return nil
		}
		batch = append(batch, r.NodeID)
		if len(batch) < limit {
			return nil
		}
		return flush()
	}); err != nil {
		return 0, err
	}
	if err := flush(); err != nil {
		return 0, err
	}
	return seen.count, nil
}

// resolveEvidencePrecision is resolvePrecision over the sorted stream: the same
// ascending, de-duplicated relation ids, cut into the same pageLimit batches,
// read with the same per-relation cap, and reduced by the same mostPrecise.
//
// The empty relation id is dropped BEFORE the batches are cut, exactly as
// resolvePrecision drops it before sorting: dropping it afterwards would shift
// every later batch boundary and change the read log.
func (c *Compiler) resolveEvidencePrecision(ctx context.Context, reader relationReader,
	wantedRun *pagination.SortedRun[hopRec], attrs *pagination.ExternalSort[relAttrRec]) error {
	limit := c.pageLimit()
	ids := make([]model.RelationID, 0, limit)
	flush := func() error {
		if len(ids) == 0 {
			return nil
		}
		rows, err := reader.EvidenceBatch(ctx, ids, evidencePerRelation)
		if err != nil {
			return contextErr(ctx, err)
		}
		// Emitted in the batch's own ascending order rather than the map's
		// range order, so the run this sort writes is a function of the read
		// and not of Go's map iteration.
		for _, id := range ids {
			evidence, ok := rows[id]
			if !ok {
				continue
			}
			if err := attrs.Add(relAttrRec{RelationID: id, Multiplier: mostPrecise(evidence)}); err != nil {
				return err
			}
		}
		ids = ids[:0]
		return nil
	}
	var prev model.RelationID
	first := true
	if err := wantedRun.Each(func(h hopRec) error {
		if !first && h.RelationID == prev {
			return nil
		}
		prev, first = h.RelationID, false
		if h.RelationID == "" {
			return nil
		}
		ids = append(ids, h.RelationID)
		if len(ids) < limit {
			return nil
		}
		return flush()
	}); err != nil {
		return err
	}
	return flush()
}

// joinHopAttributes merge-joins the resolved attributes onto the hop stream and
// restores route order.
//
// Both sides are ordered by relation id, so the join is one forward walk of
// each. A hop whose relation the scan could not type keeps an empty kind, which
// is the "not known" that makes scorePath report its route inadmissible rather
// than free -- the same answer a miss on today's `relations` map gives.
func joinHopAttributes(s *compileSorts, hopsByRelation *pagination.SortedRun[hopRec],
	attrs *pagination.ExternalSort[relAttrRec]) (*pagination.SortedRun[hopRec], error) {
	resolved, err := attrs.Sorted()
	if err != nil {
		return nil, err
	}
	resolved = trackRun(s, resolved)

	ordered, err := newSort[hopRec](s, "hop-seq", lessHopSeq, sizeOfHop)
	if err != nil {
		return nil, err
	}
	cursor := newRunCursor(resolved)
	err = hopsByRelation.Each(func(h hopRec) error {
		if _, ok := cursor.seek(relAttrRec{RelationID: h.RelationID}, lessRelAttr); ok {
			h.Kind, h.Multiplier = cursor.cur.Kind, cursor.cur.Multiplier
		}
		return ordered.Add(h)
	})
	if cerr := cursor.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return nil, err
	}
	run, err := ordered.Sorted()
	if err != nil {
		return nil, err
	}
	return trackRun(s, run), nil
}

// ---------------------------------------------------------------------------
// P-E -- walk-local centrality
// ---------------------------------------------------------------------------

// passECentrality counts, per package, the distinct admitted edges this compile
// walked: today's `centrality map[string]map[RelationID]struct{}` reduced to one
// row per package.
//
// edges is the sort P-D fed one (package, admitted edge) pair into, built with
// foldPkgEdgeDistinct, so a repeated pair has already collapsed by the time the
// run is read and each package's edges are contiguous. Counting the contiguous
// run therefore counts exactly what len(centrality[pkg]) counts.
//
// The result is a sorted run and not a map because P-F merge-joins it against
// the ranked stream sorted by package; it is complete before the first boost
// applies, which is the property rank's two-pass shape exists to guarantee (a
// candidate ranked first must not see fewer edges in its package than one
// ranked last).
func (c *Compiler) passECentrality(ctx context.Context, s *compileSorts,
	edges *pagination.ExternalSort[pkgEdgeRec]) (*pagination.SortedRun[pkgCountRec], error) {
	if err := ctx.Err(); err != nil {
		return nil, contextErr(ctx, err)
	}
	distinct, err := edges.Sorted()
	if err != nil {
		return nil, err
	}
	distinct = trackRun(s, distinct)

	counts, err := newSort[pkgCountRec](s, "pkg-count", lessPkg, sizeOfPkgCount)
	if err != nil {
		return nil, err
	}
	counts = counts.WithFold(foldPkgCount)

	var pkg string
	var edgeCount int64
	open := false
	if err := distinct.Each(func(e pkgEdgeRec) error {
		if open && e.Pkg != pkg {
			if err := counts.Add(pkgCountRec{Pkg: pkg, Edges: edgeCount}); err != nil {
				return err
			}
			edgeCount = 0
		}
		pkg, open = e.Pkg, true
		edgeCount++
		return nil
	}); err != nil {
		return nil, err
	}
	if open {
		if err := counts.Add(pkgCountRec{Pkg: pkg, Edges: edgeCount}); err != nil {
			return nil, err
		}
	}
	run, err := counts.Sorted()
	if err != nil {
		return nil, err
	}
	return trackRun(s, run), nil
}

// ---------------------------------------------------------------------------
// bitset
// ---------------------------------------------------------------------------

// bitset is the matched set of the edge scan: one bit per distinct wanted
// relation, indexed by the relation's ordinal in the ascending wanted stream,
// with the population count maintained so the scan's early exit is an integer
// comparison rather than a scan of the bits.
//
// It replaces relationsOnPaths' `out map[RelationID]model.Relation`, which
// holds an id string, a whole model.Relation and the map's own bucket per
// entry. One bit is not zero, and it is still a function of the walk; it is
// what the exact early exit costs, and the alternative -- paging every node
// batch to exhaustion because the scan cannot tell it is finished -- costs
// store reads instead, which is the more expensive resource.
//
// Ruling C11 accepts that trade with its bound named: the words are
// distinct-retained-relations / 8 bytes, so 12.5 MB at 10^8 distinct retained
// relations and 125 MB at 10^9. It is contiguous and pointer-free, which is
// why it is the one f(walk) structure this wave keeps.
type bitset struct {
	words []uint64
	count int64
}

func newBitset(n int64) *bitset {
	if n <= 0 {
		return &bitset{}
	}
	return &bitset{words: make([]uint64, (n+63)/64)}
}

// set marks the bit and reports whether this call is what marked it, so a
// caller can both count distinct hits and act only on the first one.
func (b *bitset) set(i int64) bool {
	if i < 0 || i/64 >= int64(len(b.words)) {
		return false
	}
	mask := uint64(1) << uint(i%64)
	if b.words[i/64]&mask != 0 {
		return false
	}
	b.words[i/64] |= mask
	b.count++
	return true
}
