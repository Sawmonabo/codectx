package graph

import (
	"cmp"
	"context"
	"encoding/json"
	"os"
	"slices"

	"github.com/Sawmonabo/codectx/internal/model"
	"github.com/Sawmonabo/codectx/internal/pagination"
)

// The frozen record shapes, folds and comparators the global impact/deps rank
// is built from (ruling P2/P4). Nothing here walks, sorts or serves: this file
// is the contract the walk lane writes into and the rollup and paging lanes
// read out of, so the two halves cannot disagree about what a record means.
//
// The shape is the one internal/search already uses for its distinct set: two
// passes over one pagination.ExternalSort. Pass 1 is keyed by IDENTITY (the
// node, or the package pair) and folds; pass 2 is keyed by the SERVED ORDER
// and does not fold. Two passes are necessary rather than a convenience -- the
// fold moves a record's rank, because it can lower a node's cost and so raise
// its score, and folding equal-RANKED neighbours in one pass would merge
// records that are not the same node at all.
//
// Every fold below is a total, commutative, associative function of the
// records it merges, and never of the order they arrive in. That is the whole
// point of this file: under P2 the walk streams into a disk-backed sort, so
// arrival order is decided by where the run and page boundaries happened to
// fall. Any field taken from "whichever side arrived first" would make the
// served answer depend on those boundaries -- the exact defect this programme
// exists to remove. impactAccumulator.Visit's literal "first admission wins"
// for cost/depth/via therefore becomes "the cheapest route wins, tie-broken
// deterministically"; see foldImpact.

// impactRecord is one affected entity as the completed walk found it: the
// route that reached it, the direction that route walked, and the reasons of
// every edge that touched it. It is everything impactAccumulator.Visit
// accumulates per node (impact.go), flattened so it can live on disk.
//
// Two departures from the in-heap impactNode it replaces, both forced:
//
//   - there is no `seen` map. A map cannot be encoded deterministically and
//     would be re-allocated per decoded record; the reasons list is deduped by
//     the same slice scan boundReasons uses in internal/search.
//   - the parent CHAIN travels with the record, as Route. The served page no
//     longer has a page-local byNode map to walk parents through -- the page
//     comes out of a ranked spool -- and ImpactEntry.Paths is not optional:
//     Section 14.3 rejects an entry whose reason nothing backs, and the single
//     route is that backing. Route is bounded by model.MaxRelationsPerPath,
//     the same bound impactAccumulator.path applies.
//
// ScoreMicros is deliberately NOT a field: it is 1_000_000 / (1 + Cost), and a
// stored copy could disagree with the Cost a fold just lowered. scoreMicros()
// derives it, and lessByRank ranks on that derived value rather than on Cost --
// the division is integer, so distinct costs collapse to equal scores and
// ranking on cost would be a strictly finer order than ruling P1's key.
type impactRecord struct {
	NodeID    model.NodeID       `json:"n"`
	Depth     int                `json:"d"`
	Cost      int64              `json:"c"`
	Direction model.Direction    `json:"dir"`
	Via       model.RelationID   `json:"v,omitempty"`
	Parent    model.NodeID       `json:"p,omitempty"`
	Route     []model.RelationID `json:"rt,omitempty"`
	Reasons   []string           `json:"rs,omitempty"`
}

// scoreMicros is the frozen integer score: a cheaper, more direct route always
// outranks a longer one and no float ever enters the ordering. It is the same
// arithmetic impactAccumulator.Entries applies, kept in one place so the two
// cannot drift.
func (r impactRecord) scoreMicros() int64 { return 1_000_000 / (1 + r.Cost) }

// pairRecord is one (from-package, to-package) pair of the rollup, with the
// number of symbol edges and the number of evidence records behind it. It is
// model.PackageEdge's six fields in the sort's record form; ruling P4's
// "per-pair lists" has no counterpart on PackageEdge and none is invented here,
// because a field no served type can carry would have no reader.
type pairRecord struct {
	FromNodeID    model.NodeID `json:"f"`
	ToNodeID      model.NodeID `json:"t"`
	FromPath      string       `json:"fp,omitempty"`
	ToPath        string       `json:"tp,omitempty"`
	PairCount     int64        `json:"pc"`
	EvidenceCount int64        `json:"ec"`
}

// edge projects the record onto the served type. The counts are exact sums of
// every edge the whole walk admitted for this pair, which is what ruling P4
// requires of the rollup.
func (r pairRecord) edge() model.PackageEdge {
	return model.PackageEdge{
		FromNodeID: r.FromNodeID, ToNodeID: r.ToNodeID,
		FromPath: r.FromPath, ToPath: r.ToPath,
		PairCount: r.PairCount, EvidenceCount: r.EvidenceCount,
	}
}

// The codec is JSON, for the reason internal/search states for its own sort
// records: these bytes never leave the process, and a codec that cannot drift
// from the struct beats the bytes a packed one would save. Go's encoder emits
// fields in declaration order, so one record always encodes to one byte
// string -- the determinism the comparators are entitled to assume.

func encodeImpactRecord(v impactRecord) ([]byte, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, &model.Error{Code: model.CodeInternal,
			Message: "graph: encoding an impact record: " + err.Error()}
	}
	return b, nil
}

func decodeImpactRecord(b []byte) (impactRecord, error) {
	var v impactRecord
	if err := json.Unmarshal(b, &v); err != nil {
		return impactRecord{}, &model.Error{Code: model.CodeStorageCorrupt,
			Message: "graph: a spilled impact record is not readable"}
	}
	return v, nil
}

func encodePairRecord(v pairRecord) ([]byte, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, &model.Error{Code: model.CodeInternal,
			Message: "graph: encoding a package pair record: " + err.Error()}
	}
	return b, nil
}

func decodePairRecord(b []byte) (pairRecord, error) {
	var v pairRecord
	if err := json.Unmarshal(b, &v); err != nil {
		return pairRecord{}, &model.Error{Code: model.CodeStorageCorrupt,
			Message: "graph: a spilled package pair record is not readable"}
	}
	return v, nil
}

// sizeOfImpactRecord charges one buffered record against the sort's run budget
// (ExternalSort.WithRunBytes). It is an estimate by construction -- the record
// is encoded only when a run spills -- and it errs high. The route makes the
// charge depth-dependent, which is why it is counted rather than folded into
// the fixed allowance.
func sizeOfImpactRecord(v impactRecord) int64 {
	n := len(v.NodeID) + len(v.Via) + len(v.Parent) + len(v.Direction)
	for _, rel := range v.Route {
		n += len(rel) + 16
	}
	for _, reason := range v.Reasons {
		n += len(reason) + 16
	}
	return int64(n) + 192
}

// sizeOfPairRecord is the same charge for a pair.
func sizeOfPairRecord(v pairRecord) int64 {
	return int64(len(v.FromNodeID)+len(v.ToNodeID)+len(v.FromPath)+len(v.ToPath)) + 128
}

// foldImpact merges two admissions of ONE node into the entry that survives
// them. It reproduces what impactAccumulator.Visit does per node, with every
// order-dependence removed:
//
//   - the CHEAPEST route wins, and Depth, Via, Parent and Route all come from
//     that same winner, so the served path never describes a route whose cost
//     contradicts the reported score. Visit kept the first admission's cost
//     because the page-local walk admitted a node once; across a completed
//     walk streamed through a sort, "first" is a boundary artefact.
//   - DirectionIncoming is sticky: a node reachable both ways under
//     DirectionBoth is reported as incoming, because "may need modification"
//     is the stronger claim. Taking it from either side makes it commutative.
//   - the reasons of both sides survive, deduplicated and ordered, up to
//     model.MaxReasonsPerEntry. They are ordered BEFORE the bound is applied:
//     truncating an arrival-ordered list would make the surviving SET depend
//     on where the page boundaries fell.
//
// It cannot fail; the error is the signature ExternalSort.WithFold takes.
func foldImpact(a, b impactRecord) (impactRecord, error) {
	keep := a
	if compareImpactRoute(b, a) < 0 {
		keep = b
	}
	if a.Direction == model.DirectionIncoming || b.Direction == model.DirectionIncoming {
		keep.Direction = model.DirectionIncoming
	}
	keep.Reasons = mergeReasons(a.Reasons, b.Reasons)
	return keep, nil
}

// compareImpactRoute is the tie-break that picks the surviving route: cheapest
// cost, then shallowest, then the smallest admitting edge, then the
// lexicographically smallest route, then the smallest parent. It is a total
// order over the routes that can reach one node, so foldImpact's winner is
// unique however the records are grouped.
func compareImpactRoute(x, y impactRecord) int {
	if c := cmp.Compare(x.Cost, y.Cost); c != 0 {
		return c
	}
	if c := cmp.Compare(x.Depth, y.Depth); c != 0 {
		return c
	}
	if c := cmp.Compare(x.Via, y.Via); c != 0 {
		return c
	}
	if c := slices.Compare(x.Route, y.Route); c != 0 {
		return c
	}
	return cmp.Compare(x.Parent, y.Parent)
}

// mergeReasons is the deterministic reason merge: the union of both sides,
// deduplicated, each clipped to model.MaxReasonBytes, sorted, and only then
// cut to model.MaxReasonsPerEntry -- so the surviving set is a function of the
// reasons alone. Empty reasons are dropped: model.ImpactEntry.Validate rejects
// them, and an entry must never fail validation because of a fold.
func mergeReasons(a, b []string) []string {
	out := make([]string, 0, len(a)+len(b))
	for _, side := range [2][]string{a, b} {
		for _, r := range side {
			if len(r) > model.MaxReasonBytes {
				r = r[:model.MaxReasonBytes]
			}
			if r == "" || slices.Contains(out, r) {
				continue
			}
			out = append(out, r)
		}
	}
	slices.Sort(out)
	if len(out) > model.MaxReasonsPerEntry {
		out = out[:model.MaxReasonsPerEntry]
	}
	return out
}

// foldPair merges two observations of ONE package pair. The counts are summed
// exactly -- ruling P4 -- and the labels are chosen by a rule that depends on
// the two labels alone and never on which arrived first.
func foldPair(a, b pairRecord) (pairRecord, error) {
	keep := a
	keep.FromPath = mergeLabel(a.FromPath, b.FromPath)
	keep.ToPath = mergeLabel(a.ToPath, b.ToPath)
	keep.PairCount = a.PairCount + b.PairCount
	keep.EvidenceCount = a.EvidenceCount + b.EvidenceCount
	return keep, nil
}

// mergeLabel picks a pair's surviving container label: the non-empty one, and
// the smaller when both are set. A container whose label did not resolve
// renders empty (clipPath of an unlabelled node), and letting an empty label
// win would erase a path the other side did resolve.
func mergeLabel(a, b string) string {
	switch {
	case a == "":
		return b
	case b == "":
		return a
	}
	return min(a, b)
}

// lessByNode is pass 1's key for the entity sort: the node identity the fold
// groups on. It returns an ordering integer rather than a bool because that is
// what pagination.ExternalSort compares with, and because "equal" is the
// signal that makes the fold run at all.
func lessByNode(a, b impactRecord) int { return cmp.Compare(a.NodeID, b.NodeID) }

// lessByRank is pass 2's key and the served order: ruling P1's
// (ScoreMicros desc, Depth asc, NodeID asc).
//
// Name left the tie-break with P1. It is known only after hydration, and the
// whole answer is now ranked before any of it is hydrated, so keeping it would
// have made the global order depend on a fact the ranking pass does not have.
// NodeID is unique per record after pass 1's fold, so the order is still total
// and still deterministic. A served page therefore arrives already ordered and
// must NOT be re-sorted after hydration.
func lessByRank(a, b impactRecord) int {
	if c := cmp.Compare(b.scoreMicros(), a.scoreMicros()); c != 0 {
		return c
	}
	if c := cmp.Compare(a.Depth, b.Depth); c != 0 {
		return c
	}
	return cmp.Compare(a.NodeID, b.NodeID)
}

// lessByPairKey is pass 1's key for the rollup: the pair identity foldPair
// sums on. The labels are deliberately not part of it -- they are derived from
// the node ids, and keying on them would split one pair in two if a container
// were relabelled mid-walk.
func lessByPairKey(a, b pairRecord) int {
	if c := cmp.Compare(a.FromNodeID, b.FromNodeID); c != 0 {
		return c
	}
	return cmp.Compare(a.ToNodeID, b.ToNodeID)
}

// lessByPair is pass 2's key and the served order, unchanged from the order
// the rollup has always sorted by: (FromPath, ToPath, FromNodeID, ToNodeID).
func lessByPair(a, b pairRecord) int {
	if c := cmp.Compare(a.FromPath, b.FromPath); c != 0 {
		return c
	}
	if c := cmp.Compare(a.ToPath, b.ToPath); c != 0 {
		return c
	}
	return lessByPairKey(a, b)
}

// rankedTail is the handle a served page hands to the cursor it mints: the
// spool holding the records ranked AFTER this page, the lease that keeps it
// alive, and the totals the answer discloses.
//
// It carries an Offset rather than a sort key because pagination.Spools.Open
// streams a spool from the start and cannot seek (cursor.go states the
// constraint). The continuation therefore follows internal/search's shape: the
// page being served is read off the front of the spool and the remainder is
// copied record-by-record into a FRESH spool, so Offset is how many records of
// the source spool the next read skips -- never a position a caller could
// choose, and never state that grows with the answer.
type rankedTail struct {
	// SpoolID names the spool the next page reads; empty when the answer ended
	// with the page that produced this handle.
	SpoolID string
	// LeaseID is the cursor-owned retention lease minted with that spool.
	LeaseID string
	// Offset is how many RANKED records the next page skips before its own
	// first record. It does not count the spool's leading header record, which
	// every read skips unconditionally -- the same convention internal/search's
	// spoolTail applies to its metadata record -- so an Offset of zero means
	// the first ranked record and never the header.
	Offset int
	// Served is how many records every page up to and including this one has
	// served, and Total how many the whole ranked answer holds. Both are
	// cumulative facts of the ANSWER, not of the page, so a caller can see
	// that a continuation is a continuation of a known whole.
	Served int64
	Total  int64
}

// done reports that the ranked answer is exhausted, so the page that produced
// this handle mints no continuation.
func (t rankedTail) done() bool { return t.SpoolID == "" || t.Served >= t.Total }

// rankPairs is the same two passes over the package rollup (ruling P4): pass 1
// keys lessByPairKey and folds foldPair, so the counts it reports are exact
// sums over the whole walk rather than over one page; pass 2 keys lessByPair,
// which is the order the rollup has always served. Close the returned run.
//
// The two passes are what make the answer independent of arrival order: the
// fold runs once over the fully ordered stream of pass 1, so a pair split
// across any number of emitting batches sums to the same counts a single-shot
// rollup of the same edges would report, and pass 2 orders the distinct pairs
// globally rather than one batch at a time.
//
// Owned by lane P-c.
func (e *Engine) rankPairs(ctx context.Context,
	emit func(add func(pairRecord) error) error,
	stats *rankStats) (*pagination.SortedRun[pairRecord], error) {
	byKey, err := e.newPairSort("graphpairkey-", lessByPairKey)
	if err != nil {
		return nil, err
	}
	byKey = byKey.WithFold(foldPair)
	defer byKey.Close()
	if err := emit(byKey.Add); err != nil {
		return nil, err
	}
	folded, err := byKey.Sorted()
	if err != nil {
		return nil, err
	}
	stats.observe(byKey.PeakLiveRecords())
	defer folded.Close()
	// The deadline ends a page and never the answer (ruling P3), but a
	// cancelled request must not pay for a second pass over a set it will
	// never serve.
	if err := ctx.Err(); err != nil {
		return nil, typedContextError(ctx, err)
	}
	byPair, err := e.newPairSort("graphpair-", lessByPair)
	if err != nil {
		return nil, err
	}
	defer byPair.Close()
	if err := folded.Each(byPair.Add); err != nil {
		return nil, err
	}
	run, err := byPair.Sorted()
	if err != nil {
		return nil, err
	}
	stats.observe(byPair.PeakLiveRecords())
	return run, nil
}

// newPairSort opens one pass of the pair sort with the frozen codec and the
// query's own run budget. The budget is a share of resources.query_memory_bytes
// -- the same number Limits.FrontierBytes carries into the engine -- so one
// query's structures are all charged against the one admission it was granted
// rather than against a second key that could oversubscribe it.
func (e *Engine) newPairSort(prefix string,
	compare func(a, b pairRecord) int) (*pagination.ExternalSort[pairRecord], error) {
	sorter, err := pagination.NewExternalSort(e.pairSortDir(), prefix, 0,
		encodePairRecord, decodePairRecord, compare)
	if err != nil {
		return nil, err
	}
	return sorter.WithRunBytes(pagination.SortRunBytes(e.limits.FrontierBytes), sizeOfPairRecord), nil
}

// pairSortDir is where the pair sort's runs spill: beside the store's
// continuation spools when the engine has them, so an operator has one place
// to look and the store's sweep reports and reclaims the files, and the
// operating system's temporary directory otherwise -- an engine built without
// Spools has no store directory of its own, and the sort removes every file it
// creates when its runs and its output are closed.
func (e *Engine) pairSortDir() string {
	if e.spools == nil {
		return os.TempDir()
	}
	return e.spools.SortDir()
}

// rankStats reports the largest in-memory record set a ranked answer's two
// sort passes ever held. It is the entity-side counterpart of rollupStats: the
// memory invariant a bound assertion is written against, read from
// pagination.ExternalSort.PeakLiveRecords so a test asserts the sort's own
// high-water mark rather than restating the design constant.
//
// The two passes run SEQUENTIALLY -- pass 1 is closed before pass 2 opens --
// so the peak of a ranking is their MAXIMUM and never their sum.
type rankStats struct{ PeakLiveRecords int }

// observe records a high-water mark. A nil stats is the production case and
// costs one comparison.
func (s *rankStats) observe(n int) {
	if s != nil && n > s.PeakLiveRecords {
		s.PeakLiveRecords = n
	}
}

// heapProbe is the memory instrumentation an END-TO-END bound assertion reads.
// The two ranked answers rank and serve inside one call, so the sorts they
// build are created, observed and closed before the caller sees anything; a
// probe the engine carries is the only way a test can assert on what they held
// without the ranking handing its sorts out, which would let a caller keep one
// alive past the request that owns it.
//
// Engine.probe is nil in production and every observation is a nil check.
type heapProbe struct {
	// Rank is the affected-entity ranking's peak, Pairs the package rollup
	// ranking's, and Rollup the batch of edges the streaming rollup itself
	// held above them.
	Rank   rankStats
	Pairs  rankStats
	Rollup rollupStats
	// AdoptedRuns is how many spilled runs of an interrupted ranking this
	// request continued instead of re-sorting, and ReaddedRecords how many
	// records it put into a sort that the adopted runs already held. The
	// second is zero by construction and is counted anyway: it is what tells a
	// resumed ranking that REUSES its runs from one that re-sorts the whole
	// retained input and happens to reach the same answer.
	AdoptedRuns    int
	ReaddedRecords int64
}

// rankProbe, pairProbe and rollupProbe hand the ranking passes the counters to
// observe into, or nil when nothing is probing.
func (e *Engine) rankProbe() *rankStats {
	if e.probe == nil {
		return nil
	}
	return &e.probe.Rank
}

func (e *Engine) pairProbe() *rankStats {
	if e.probe == nil {
		return nil
	}
	return &e.probe.Pairs
}

func (e *Engine) rollupProbe() *rollupStats {
	if e.probe == nil {
		return nil
	}
	return &e.probe.Rollup
}
