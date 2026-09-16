// This file holds pass P-C (relation attributes) and pass P-E (walk-local
// centrality) of the context compiler.
//
// Both passes exist to delete a candidate-sized map: the relation row of every
// relation on every retained route, one precision multiplier per relation, and
// `centrality map[pkg]map[RelationID]struct{}`. None of the three has a bound
// -- they are functions of the walk, and the walk is a function of the
// repository. Here each is a sort-merge join or a streaming aggregation, so the
// heap a compile holds is the sort run buffer and one page of reads.
//
// P-C reads structure through the packed adjacency port and nothing else
// (ADR-0005 Decision 1); only relation PRECISION, which the packed form does
// not carry, is still a b-tree read.
package context

import (
	"context"
	"slices"

	"github.com/Sawmonabo/codectx/internal/graph"
	"github.com/Sawmonabo/codectx/internal/model"
	"github.com/Sawmonabo/codectx/internal/pagination"
	"github.com/Sawmonabo/codectx/internal/storage/sqlite"
)

// evidenceReader is the slice of sqlite.PinnedReader P-C reads through: the
// batched evidence read, and nothing else.
//
// Precision is a property of the evidence rows a relation carries, and the
// packed adjacency publishes an evidence COUNT and not the rows, so this one
// read stays on the fact tables while the edge scan reads the packed form. The
// narrow interface is what lets the pass be exercised against a fixture graph
// with no store behind it.
type evidenceReader interface {
	EvidenceBatch(ctx context.Context, relations []model.RelationID,
		perRelation int) (map[model.RelationID][]sqlite.StoredEvidence, error)
}

// edgeScanBudget answers the edge scan's stop condition: how many rows the
// scan may read, and whether that bound exists at all.
//
// context.max_graph_edges is Unlimited by default, and an unlimited bound means
// what it says: the scan reads every incident entry of every candidate node.
// The page WIDTH -- how many entries are buffered between two joins against the
// wanted stream -- is not a ceiling and is bounded separately.
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
// it: the streaming form of the candidate node set the edge scan reads from. It is a projection and not a candRec because the node pass is sorted
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
// the order the edge scan's node batches are cut in. Total: one node survives
// the dedupe with one sequence.
func lessNodeSeq(a, b nodeRec) int { return cmpInt(a.Seq, b.Seq) }

// foldMinNodeSeq keeps the earliest arrival of a repeated node and drops every
// later one. Order-independent.
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
// in ingest order -- BOTH including excluded candidates, because an excluded
// candidate's routes still name relations that must be typed and its node id is
// still part of the set the edge scan reads from.
//
// The passes:
//
//  1. Sort the hops by relation id. The run is the join side for step 5, and
//     its de-duplicated key stream IS the ascending distinct relation-id
//     sequence the evidence read walks.
//  2. Deduplicate the candidate node ids by identity keeping the earliest
//     sequence, then restore ingest order: the node list the edge scan cuts its
//     batches from.
//  3. Scan the packed adjacency incident to those nodes, merge-matching each
//     bounded batch of delivered entries against the wanted stream instead of
//     probing a map.
//  4. Resolve precision over the same ascending de-duplicated ids in pageLimit
//     batches.
//  5. Merge the two attribute streams by relation id (foldRelAttr is
//     mostPrecise plus "whichever side carries a kind") and join the result onto
//     the hop run, then restore route order so P-D can rebuild one candidate's
//     routes from a contiguous run.
func (c *Compiler) passCRelationAttributes(ctx context.Context, s *compileSorts, reader evidenceReader,
	g graph.GraphReader, hops *pagination.SortedRun[hopRec], cands *pagination.SortedRun[candRec]) (relationAttributes, error) {
	if reader == nil {
		return relationAttributes{}, argumentInvalid("resolving relation attributes requires a pinned reader")
	}
	if g == nil {
		return relationAttributes{}, argumentInvalid("resolving relation attributes requires a packed adjacency reader")
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

	// wanted is counted, not collected: the count is every comparison a
	// whole-set `len(wanted)` is used in, and the ids themselves are re-read
	// from the sorted run each time a pass needs them.
	wanted, err := distinctRelationCount(hopsByRelation)
	if err != nil {
		return relationAttributes{}, err
	}
	nodes, err := c.scanNodes(s, cands)
	if err != nil {
		return relationAttributes{}, err
	}

	// The zero-read path: with no wanted relation or no node there is nothing
	// to scan and nothing to read evidence for, and complete is "there was
	// nothing to find".
	if wanted == 0 || nodes.Len() == 0 {
		return relationAttributes{Hops: hops, Complete: wanted == 0}, nil
	}

	attrs, err := newSort[relAttrRec](s, "rel-attr", lessRelAttr, sizeOfRelAttr)
	if err != nil {
		return relationAttributes{}, err
	}
	attrs = attrs.WithFold(foldRelAttr)

	matched, err := c.scanEdgeKinds(ctx, g, hopsByRelation, nodes, wanted, attrs)
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
// every id a route names enters the wanted stream with no filter, while the
// evidence read skips it -- an asymmetry the whole-set reference shares.
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

// scanNodes is the candidate node set the edge scan reads from: each node id
// once, in the order of its first arrival. Two sorts and no map.
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

// scopeKindCodes translates the Section 15.2 boundary allowlist into the pinned
// generation's kind dictionary.
//
// A kind this generation seals none of carries no code and is dropped. When
// none of them does, the scan has no rows to read at all, and that is reported
// as `absent` rather than as an empty code slice: the port reads an empty slice
// as "every kind", which would widen a boundary-filtered scan into the whole
// neighbourhood under a name that promises otherwise.
func scopeKindCodes(g graph.GraphReader) (codes []graph.KindCode, absent bool) {
	kinds := g.Kinds()
	codes = make([]graph.KindCode, 0, len(scopeRelations))
	for _, k := range scopeRelations {
		if code, ok := kinds.Code(k); ok {
			codes = append(codes, code)
		}
	}
	return codes, len(codes) == 0
}

// scanEdgeKinds reads the packed adjacency incident to the candidate nodes and
// writes the kind of every entry the routes name, returning how many distinct
// wanted relations it typed.
//
// The node list is cut into pageLimit batches in ingest order and each batch is
// resolved to surrogates, which the port requires ascending and duplicate-free.
// Delivered entries are buffered pageLimit at a time; a buffer is translated
// back to canonical relation ids in one batched read, SORTED, and merge-joined
// against the wanted stream. The sort is what makes the join legal: the port
// delivers in (owner, list index) order, which is a surrogate order and says
// nothing about canonical ids, so a forward cursor over the canonically-ordered
// wanted stream would walk past most of the batch without it. A fresh cursor
// opens per buffer for the same reason -- two consecutive buffers are not
// jointly ascending.
//
// `scanned` counts entries the port DELIVERED, which is what context.max_graph_edges
// bounds, and the scan ends early through ErrStopScan once every wanted
// relation is typed or that budget is spent.
//
// The matched set is a bit per distinct wanted relation, indexed by the
// relation's ordinal in the wanted stream. It is the one structure here that is
// a function of the walk rather than of a batch, and it is what preserves the
// early exit: without an exact running count of distinct typed relations the
// scan cannot know it is done, and it would read every node's whole list.
func (c *Compiler) scanEdgeKinds(ctx context.Context, g graph.GraphReader,
	wantedRun *pagination.SortedRun[hopRec], nodes *pagination.SortedRun[nodeRec],
	wanted int64, attrs *pagination.ExternalSort[relAttrRec]) (int64, error) {
	seen := newBitset(wanted)
	codes, absent := scopeKindCodes(g)
	if absent {
		return 0, nil
	}
	limit := c.pageLimit()
	budget, bounded := c.edgeScanBudget()
	scanned := 0
	stopped := false

	buf := make([]graph.Edge, 0, limit)
	joinBuffer := func() error {
		if len(buf) == 0 {
			return nil
		}
		rels := make([]graph.RelRef, len(buf))
		for i, e := range buf {
			rels[i] = e.Rel
		}
		ids, err := g.RelationIDs(ctx, rels)
		if err != nil {
			return err
		}
		typed := make([]relAttrRec, 0, len(buf))
		for i, e := range buf {
			kind, ok := g.Kinds().Kind(e.Kind)
			// The generation does not publish one of the two facts the entry
			// is made of. It cannot type a wanted relation, and counting it
			// would report a complete scan over a relation nothing named.
			if !ok || ids[i] == "" {
				continue
			}
			typed = append(typed, relAttrRec{RelationID: ids[i], Kind: kind})
		}
		buf = buf[:0]
		slices.SortFunc(typed, lessRelAttr)
		cursor := newRunCursor(wantedRun)
		for _, t := range typed {
			pos, ok := cursor.seek(hopRec{RelationID: t.RelationID}, lessRelID)
			if !ok || !seen.set(pos) {
				continue
			}
			if err := attrs.Add(t); err != nil {
				cursor.Close()
				return err
			}
		}
		return cursor.Close()
	}

	scan := func(refs []graph.NodeRef) error {
		_, err := g.Neighbours(ctx, refs, model.DirectionBoth, codes, graph.EdgePos{}, func(e graph.Edge) error {
			buf = append(buf, e)
			scanned++
			spent := bounded && scanned >= budget
			if len(buf) < limit && !spent {
				return nil
			}
			if err := joinBuffer(); err != nil {
				return err
			}
			if spent || seen.count == wanted {
				return graph.ErrStopScan
			}
			return nil
		})
		if err != nil {
			return err
		}
		return joinBuffer()
	}

	batch := make([]model.NodeID, 0, limit)
	flush := func() error {
		if len(batch) == 0 || stopped || seen.count == wanted {
			batch = batch[:0]
			return nil
		}
		refs, err := g.Resolve(ctx, batch)
		if err != nil {
			return err
		}
		batch = batch[:0]
		refs = ascendingDistinctRefs(refs)
		if len(refs) == 0 {
			return nil
		}
		if err := scan(refs); err != nil {
			return err
		}
		if bounded && scanned >= budget {
			stopped = true
		}
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

// ascendingDistinctRefs is the port's precondition on a batch of node
// surrogates: ascending, duplicate-free, and without the zero that Resolve
// returns for an identity this generation does not carry.
func ascendingDistinctRefs(refs []graph.NodeRef) []graph.NodeRef {
	slices.Sort(refs)
	out := refs[:0]
	var prev graph.NodeRef
	for _, ref := range refs {
		if ref == 0 || ref == prev {
			continue
		}
		out = append(out, ref)
		prev = ref
	}
	return out
}

// resolveEvidencePrecision reads one precision multiplier per wanted relation
// off the sorted stream: ascending, de-duplicated relation ids, cut into
// pageLimit batches, read with one per-relation cap, and reduced by mostPrecise.
//
// The empty relation id is dropped before the batches are cut: it names no
// relation, so reading evidence for it would spend a slot of every batch on a
// row that cannot exist.
func (c *Compiler) resolveEvidencePrecision(ctx context.Context, reader evidenceReader,
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
// than free.
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
// walked: a `centrality map[string]map[RelationID]struct{}` reduced to one row
// per package.
//
// edges is the sort P-D fed one (package, admitted edge) pair into, built with
// foldPkgEdgeDistinct, so a repeated pair has already collapsed by the time the
// run is read and each package's edges are contiguous. Counting the contiguous
// run therefore counts exactly what one such inner map's length counts.
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
// It replaces a `map[RelationID]model.Relation` of every typed relation, which
// holds an id string, a whole model.Relation and the map's own bucket per
// entry. One bit is not zero, and it is still a function of the walk; it is
// what the exact early exit costs, and the alternative -- reading every node's
// whole list because the scan cannot tell it is finished -- costs store reads
// instead, which is the more expensive resource.
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
