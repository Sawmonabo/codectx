// This file holds the record types, comparators, folds and sort factory every
// streaming pass of the context compiler shares, plus the entry point of each
// pass P-A .. P-I.
//
// The design it serves is that the compiler's peak heap is a function of the
// sort run budget, the page size and the resolved byte budget, and never of the
// candidate count, while the plan it produces stays byte-for-byte the one
// today's whole-set pipeline produces. Everything here exists to make that
// possible: each map the pipeline holds today becomes a sort-merge join over
// one of these records, and each nested map a streaming aggregation through one
// of these folds.
//
// The signatures in this file are frozen. A lane that needs a different one
// reports the need rather than changing it.
package context

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/Sawmonabo/codectx/internal/config"
	"github.com/Sawmonabo/codectx/internal/graph"
	"github.com/Sawmonabo/codectx/internal/model"
	"github.com/Sawmonabo/codectx/internal/pagination"
	"github.com/Sawmonabo/codectx/internal/storage/sqlite"
)

// ---------------------------------------------------------------------------
// Records
// ---------------------------------------------------------------------------
//
// Every record is encoded with encodeRecord and decoded with decodeRecord: one
// JSON codec for all of them, with short field tags. JSON and not a packed
// encoding for the reason internal/index/plan/plan.go:925-932 already settled
// for the planner's sort -- the record never leaves this process, and a codec
// that cannot drift from the struct it encodes is worth more here than the
// bytes a hand-packed one would save. encoding/json emits struct fields in
// declaration order, so the encoding is deterministic: two compiles of one
// generation produce identical run files.
//
// Round-trip normalization: every variable-length field is `omitempty`, so a
// nil slice round-trips to nil and an empty non-nil slice round-trips to nil.
// No producer here distinguishes the two.

// candRec is one candidate as the streamed pipeline carries it: the frozen
// `candidate` record minus its routes, plus the two positions the stream needs.
//
// Routes are NOT inline. `candidate.Paths` is bounded by
// context.max_reason_paths_per_entry, which is a config.Limit defaulting to
// unlimited (rank.go:145-156: "scoreRoutes stores every admissible route for
// it"), so a hub entity's route list has no bound a record may assume. An
// inline list would (a) push the encoded record past the external sort's 1 MiB
// per-record ceiling (pagination/extsort.go:246), failing the compile outright
// on exactly the repositories this wave exists to serve, and (b) make sizeOfCand
// useless as a run-budget input, since one wide candidate could fill the whole
// run buffer alone. Routes therefore travel as their own sorted streams
// (pathRec, hopRec) keyed by Seq, and P-D rebuilds one candidate's
// model.RelationPath values from the contiguous run of hops that carries its
// Seq. Reasons stay inline: model.MaxReasonsPerEntry x model.MaxReasonBytes is
// a genuine constant bound the model itself validates.
//
// Seq is the ingest order -- the arrival order that reproduces today's
// SliceStable -- and Index is the position P-G assigns in the ranked order,
// which is what today's `i` in budget.go:281-292 is. Both are on the one record
// because the same record is re-sorted under lessRank, lessFileIndex and
// lessEntityID at different points and must carry every key any of them reads.
type candRec struct {
	Seq   int64 `json:"q"`
	Index int64 `json:"i,omitempty"`

	NodeID model.NodeID `json:"n,omitempty"`
	FileID model.FileID `json:"f,omitempty"`

	// PathAtRank and PathFinal are the SAME field in the whole-set candidate,
	// written twice with different rules, and ruling C3 keeps both because
	// collapsing them is not behaviour-preserving. The whole-set
	// hydrateFiles -- which now survives only as the reference implementation
	// in stream_parity_test.go -- writes the snapshot path only when the
	// candidate carries none, and ranking reads THAT value for packageOf
	// (rank.go:265), centrality and its reason; the reference buildPlan
	// overwrites unconditionally (stream_parity_test.go:438) and the total
	// order plus the persisted entry read THAT one. A candidate
	// whose pre-set path differs from its snapshot path is therefore bucketed
	// for centrality under one package and sorted under another, so one field
	// would change per-package edge counts, boosts and ScoreMicros. A follow-up
	// may collapse them once it proves fv.Path == Path whenever Path != "".
	PathAtRank string `json:"pr,omitempty"`
	PathFinal  string `json:"pf,omitempty"`

	Requirement model.Requirement `json:"r,omitempty"`
	Origin      originKind        `json:"o,omitempty"`
	Depth       int               `json:"d,omitempty"`
	StartByte   int64             `json:"sb,omitempty"`
	ScoreMicros int64             `json:"sc,omitempty"`
	SizeBytes   int64             `json:"sz,omitempty"`
	Status      model.FileStatus  `json:"st,omitempty"`
	Reasons     []string          `json:"rs,omitempty"`
	MorePaths   int64             `json:"mp,omitempty"`
	Excluded    string            `json:"x,omitempty"`

	// FileMissing is P-B's flag that the pinned snapshot holds no row for
	// FileID. buildPlan today learns this from a map lookup miss
	// (budget.go:249-252); a streamed P-G has no such map, so the fact travels
	// on the record that observed it. It is one of the three filters P-G
	// applies before assigning Index, and mislabelling it shifts every later
	// ordinal.
	FileMissing bool `json:"fm,omitempty"`
}

// pathRec is one route of one candidate: everything a model.RelationPath holds
// except its relation list, which travels as hopRec. PathIdx is the route's
// position in the candidate's own Paths slice, so (Seq, PathIdx) reproduces the
// order scoreRoutes iterates in and its SliceStable tie-breaks stay defined.
type pathRec struct {
	Seq     int64 `json:"q"`
	PathIdx int32 `json:"p"`

	CostUnits int64              `json:"c,omitempty"`
	Evidence  []model.EvidenceID `json:"e,omitempty"`
	// HopCount is len(Relations), carried so a consumer can size the rebuilt
	// route and check the hop run it read is complete before scoring it.
	HopCount int32 `json:"h,omitempty"`
}

// hopRec is one hop of one route: the relation the route crosses at HopIdx,
// with the two attributes P-C resolves for it. Kind and Multiplier are empty
// until that join fills them, and a hop whose relation P-C could not type keeps
// an empty Kind -- which is exactly the "not known" that makes scorePath report
// the route inadmissible (rank.go:316-319) rather than free.
type hopRec struct {
	RelationID model.RelationID `json:"rel"`

	Seq     int64 `json:"q"`
	PathIdx int32 `json:"p"`
	HopIdx  int32 `json:"h"`

	Kind       model.RelationKind `json:"k,omitempty"`
	Multiplier int64              `json:"m,omitempty"`
}

// relAttrRec is one relation's resolved attributes, the streaming form of the
// two maps ranking holds today: relationsOnPaths' `out map[RelationID]Relation`
// (for the kind) and resolvePrecision's `out map[RelationID]int64` (for the
// multiplier). Both streams carry this one record and are merged by foldRelAttr
// under lessRelAttr, so the join that replaces the two maps is one sort.
//
// Multiplier is the raw evidence multiplier and may be zero for a relation with
// no visible evidence row. The heuristic floor lives in precision() and nowhere
// else, so an edge scored without evidence takes the same multiplier
// mostPrecise gives it today.
type relAttrRec struct {
	RelationID model.RelationID   `json:"rel"`
	Kind       model.RelationKind `json:"k,omitempty"`
	Multiplier int64              `json:"m,omitempty"`
}

// pkgEdgeRec is one (package, admitted edge) pair: the streaming form of one
// entry of rank's nested `centrality map[string]map[RelationID]struct{}`. The
// sort deduplicates it under lessPkgEdge with foldPkgEdgeDistinct, so a package
// counts each distinct edge once exactly as the inner map does.
type pkgEdgeRec struct {
	Pkg        string           `json:"p"`
	RelationID model.RelationID `json:"rel"`
}

// pkgCountRec is P-E's aggregate: how many distinct admitted edges this compile
// walked in one package. It is what `len(centrality[pkg])` reads today
// (rank.go:397), computed as a streaming aggregation over the deduplicated
// pkgEdgeRec stream and complete before any boost applies.
type pkgCountRec struct {
	Pkg   string `json:"p"`
	Edges int64  `json:"e"`
}

// groupRec is one fileGroup (slice.go:24-32) without its `indexes []int`. The
// index list is what makes today's grouping candidate-sized in heap; a run of
// candRecs sorted by lessFileIndex is contiguous per file, so the aggregate is
// foldable and the members are re-read from that same run when the packer's
// decision is applied at P-I.
//
// MinIndex is the group's rank -- the Index of its highest-ranked member --
// which is the order groupByFile produces groups in and therefore the order
// P-H must walk them in.
type groupRec struct {
	FileID   model.FileID `json:"f"`
	Path     string       `json:"p,omitempty"`
	Required bool         `json:"r,omitempty"`
	Bytes    int64        `json:"b,omitempty"`
	Tokens   int64        `json:"t,omitempty"`
	MinIndex int64        `json:"i"`
	// Count is how many candidates the group holds. It is the length
	// `indexes` would have had, kept so a consumer can verify it saw every
	// member of a run rather than assuming it did.
	Count int64 `json:"c,omitempty"`
}

// decisionRec is one packer verdict, the streaming form of packing.keep and
// packing.sliceOf (slice.go:78-82). It is per FILE because packPlan works on
// file-atomic groups: it keeps or drops a file's entries together, so one
// record per file decides every candidate in it.
//
// A dropped group's reason does not travel here. Drops are written straight to
// the excluded spool in appendDrops order (stream_parity_test.go:777), which is the
// exclusion order ruling C1 keeps.
type decisionRec struct {
	FileID     model.FileID `json:"f"`
	Keep       bool         `json:"k,omitempty"`
	SliceIndex int32        `json:"s,omitempty"`
}

// ---------------------------------------------------------------------------
// Conversions
// ---------------------------------------------------------------------------

// candRecOf projects a candidate onto the stream at ingest. Both path fields
// start as the candidate's own path: P-B is what makes them differ, by writing
// the snapshot path into PathFinal unconditionally and into PathAtRank only
// when the candidate carried none. Routes are not copied -- the caller writes
// them to the pathRec and hopRec streams keyed by the same seq.
func candRecOf(c candidate, seq int64) candRec {
	return candRec{
		Seq: seq, NodeID: c.NodeID, FileID: c.FileID,
		PathAtRank: c.Path, PathFinal: c.Path,
		Requirement: c.Requirement, Origin: c.Origin, Depth: c.Depth,
		StartByte: c.StartByte, ScoreMicros: c.ScoreMicros, SizeBytes: c.SizeBytes,
		Status: c.Status, Reasons: c.Reasons, MorePaths: c.MorePaths, Excluded: c.Excluded,
	}
}

// rankCandidate rehydrates the candidate as RANKING sees it: Path is
// PathAtRank, the value hydrateFiles' conditional overwrite leaves and the one
// packageOf, the centrality bucket and the boost reason read (C3). Paths is
// left nil; the caller attaches the routes it rebuilt from the hop stream.
func (r candRec) rankCandidate() candidate {
	c := r.candidate()
	c.Path = r.PathAtRank
	return c
}

// finalCandidate rehydrates the candidate as BUDGETING sees it: Path is
// PathFinal, the value the reference buildPlan's unconditional overwrite
// leaves (stream_parity_test.go:438) and the one the total order and the
// persisted entry read (C3).
func (r candRec) finalCandidate() candidate {
	c := r.candidate()
	c.Path = r.PathFinal
	return c
}

// candidate is the shared part of the two rehydrations, with no Path set: every
// caller goes through rankCandidate or finalCandidate so that choosing the
// wrong path value is never possible by omission.
func (r candRec) candidate() candidate {
	return candidate{
		NodeID: r.NodeID, FileID: r.FileID, Requirement: r.Requirement, Origin: r.Origin,
		Depth: r.Depth, StartByte: r.StartByte, ScoreMicros: r.ScoreMicros,
		SizeBytes: r.SizeBytes, Status: r.Status, Reasons: r.Reasons, MorePaths: r.MorePaths,
		Excluded: r.Excluded,
	}
}

// entityID is candidate.entityID over the record: the node identity when the
// record names one, else the file identity. It is the final tie-break of
// lessRank and the whole key of lessEntityID, so the two orders agree on it.
func (r candRec) entityID() string {
	if r.NodeID != "" {
		return string(r.NodeID)
	}
	return string(r.FileID)
}

// precision is the edge's precision multiplier as scorePath must see it: the
// resolved one when evidence produced it, and the heuristic floor otherwise.
// mostPrecise (rank.go:209-217) seeds its search at the heuristic multiplier
// and scorePath falls back to it for an unresolved edge (rank.go:324-327); this
// is the ONE place that floor is applied on the streamed path, so the producer
// side never has to decide whether "no evidence" means zero or heuristic.
func (r relAttrRec) precision() int64 {
	if floor := precisionMultiplier[model.PrecisionHeuristic]; r.Multiplier < floor {
		return floor
	}
	return r.Multiplier
}

// ---------------------------------------------------------------------------
// Comparators
// ---------------------------------------------------------------------------
//
// pagination.ExternalSort compares with a three-way func(a, b T) int, so every
// comparator here returns a negative, zero or positive value rather than a
// bool. Two kinds live below and a lane must not turn one into the other:
//
//   - TOTAL comparators (lessRank, lessHopSeq, lessPathSeq, lessFileIndex,
//     lessPkgEdge, lessGroupIndex) end in a key no two records share, so the
//     merged order is the only order and nothing depends on arrival.
//   - JOIN comparators (lessEntityID, lessRelID, lessRelAttr, lessPkg,
//     lessDecision) are deliberately single-key: they group the records one
//     fold or one merge-join works on, and the merge's stability is what keeps
//     equal records in arrival order (extsort.go:130-140). Adding a tie-break
//     to one of these changes the order its fold sees -- the same class of
//     change internal/index/plan's compareInput froze its comparator against.

// lessRank IS candidate.less -- the whole-set comparator, kept as the
// reference implementation at stream_parity_test.go:857 -- as a three-way
// comparator, with Seq as a final key so the order is total.
//
// It reads PathFinal, because the whole-set sort runs AFTER buildPlan's
// unconditional path overwrite (stream_parity_test.go:438). Reading PathAtRank instead would order
// the plan by one path and bucket centrality by another; ruling C3 exists
// because those two values can differ.
//
// Totality is load-bearing beyond determinism: P-G assigns each survivor's
// Index from this order, so a single unresolved tie shifts the ordinal of every
// later entry and with it every stored slice membership. candidate.less already
// ends in the unique entityID; Seq covers the one case entityID does not, a
// candidate that names neither a node nor a file.
func lessRank(a, b candRec) int {
	if c := cmpInt(int64(requirementRank(a.Requirement)), int64(requirementRank(b.Requirement))); c != 0 {
		return c
	}
	// Descending: a higher score ranks first.
	if c := cmpInt(b.ScoreMicros, a.ScoreMicros); c != 0 {
		return c
	}
	if c := cmpString(a.PathFinal, b.PathFinal); c != 0 {
		return c
	}
	if c := cmpInt(a.StartByte, b.StartByte); c != 0 {
		return c
	}
	if c := cmpString(a.entityID(), b.entityID()); c != 0 {
		return c
	}
	return cmpInt(a.Seq, b.Seq)
}

// lessEntityID groups a candidate stream by entity identity, for the P-A scope
// dedupe that replaces expandScope's `admitted map[string]bool` (ruling C2).
//
// The key is the entity identity ALONE, not (entityID, seq) as the plan's prose
// spells it: pagination.ExternalSort runs its fold only over records the
// comparator reports EQUAL (extsort.go:143-156), so a comparator that included
// seq would report every record distinct and fold nothing. The seq ordering the
// prose describes still holds -- runs are written in arrival order and the
// merge breaks ties by run index -- but foldMinSeq does not rely on it and
// takes the minimum explicitly.
func lessEntityID(a, b candRec) int { return cmpString(a.entityID(), b.entityID()) }

// lessFileIndex orders measured candidates by file and then by rank position:
// sort H of P-G. A file's records are contiguous under it and ascending in
// Index, so the run's first record is the file's `charged` entry
// (budget.go:263-273) and the run itself is the file's group. Total: Index is
// unique across the whole stream.
func lessFileIndex(a, b candRec) int {
	if c := cmpString(string(a.FileID), string(b.FileID)); c != 0 {
		return c
	}
	return cmpInt(a.Index, b.Index)
}

// lessRelID groups the hop stream by relation id: the join key P-C resolves
// attributes on, and the order resolvePrecision already reads its ids in
// (rank.go:189). A join comparator -- see the note above the comparators.
func lessRelID(a, b hopRec) int { return cmpString(string(a.RelationID), string(b.RelationID)) }

// lessHopSeq restores the hop stream to route order after the attribute join:
// (candidate, route, hop). Total, and contiguous per candidate, which is what
// lets P-D rebuild one candidate's routes and run scoreRoutes unchanged on a
// working set of one candidate.
func lessHopSeq(a, b hopRec) int {
	if c := cmpInt(a.Seq, b.Seq); c != 0 {
		return c
	}
	if c := cmpInt(int64(a.PathIdx), int64(b.PathIdx)); c != 0 {
		return c
	}
	return cmpInt(int64(a.HopIdx), int64(b.HopIdx))
}

// lessPathSeq orders the route stream the same way, so a candidate's routes and
// its hops are merge-joined on (Seq, PathIdx) without either side buffering.
func lessPathSeq(a, b pathRec) int {
	if c := cmpInt(a.Seq, b.Seq); c != 0 {
		return c
	}
	return cmpInt(int64(a.PathIdx), int64(b.PathIdx))
}

// lessRelAttr groups resolved relation attributes by relation id, so the kind
// stream and the precision stream fold into one row per relation. A join
// comparator.
func lessRelAttr(a, b relAttrRec) int {
	return cmpString(string(a.RelationID), string(b.RelationID))
}

// lessPkgEdge orders (package, edge) pairs so that one package's edges are
// contiguous and a repeated edge is adjacent: the dedupe that makes P-E count
// DISTINCT edges, which is what the inner map of today's centrality does.
// Total.
func lessPkgEdge(a, b pkgEdgeRec) int {
	if c := cmpString(a.Pkg, b.Pkg); c != 0 {
		return c
	}
	return cmpString(string(a.RelationID), string(b.RelationID))
}

// lessPkg groups per-package counts for the summing fold. A join comparator.
func lessPkg(a, b pkgCountRec) int { return cmpString(a.Pkg, b.Pkg) }

// lessGroupIndex orders file groups by rank -- the order groupByFile produces
// and the order packPlan must walk, since required groups are a prefix of it
// (slice.go:84-89). Total: MinIndex is one candidate's unique Index, and FileID
// breaks the tie that cannot occur.
func lessGroupIndex(a, b groupRec) int {
	if c := cmpInt(a.MinIndex, b.MinIndex); c != 0 {
		return c
	}
	return cmpString(string(a.FileID), string(b.FileID))
}

// lessDecision groups packer verdicts by file so P-I can merge-join them
// against the measured stream, which lessFileIndex already ordered by file. A
// join comparator; one file has exactly one verdict.
func lessDecision(a, b decisionRec) int {
	return cmpString(string(a.FileID), string(b.FileID))
}

// cmpInt and cmpString are the two three-way primitives every comparator above
// is built from, so no comparator hand-writes a subtraction that could overflow
// on int64 keys.
func cmpInt(a, b int64) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

func cmpString(a, b string) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

// ---------------------------------------------------------------------------
// Folds
// ---------------------------------------------------------------------------
//
// pagination.ExternalSort runs a fold exactly once, left to right, over the
// fully ordered stream (extsort.go:143-156). Every fold here is nevertheless
// order-INDEPENDENT -- min, max, sum, boolean OR, and dedupe over records that
// are equal in the field taken -- so none of them depends on that guarantee and
// a later change to the merge cannot silently change a result.

// foldMinSeq is the C2 scope dedupe: the candidate with the smallest ingest
// sequence survives, which is expandScope's "the earliest step wins"
// (scope.go:112) exactly. The map it replaces admits the first arrival and
// ignores every later one; the first arrival IS the minimum seq, so the
// survivor set and the surviving record are identical.
func foldMinSeq(a, b candRec) (candRec, error) {
	if b.Seq < a.Seq {
		return b, nil
	}
	return a, nil
}

// foldPkgEdgeDistinct collapses a repeated (package, edge) pair to one. The two
// records are equal in both fields -- lessPkgEdge compares nothing else -- so
// which one survives is not a choice.
func foldPkgEdgeDistinct(a, b pkgEdgeRec) (pkgEdgeRec, error) { return a, nil }

// foldPkgCount sums per-package edge counts, so P-E can emit partial counts as
// it walks the deduplicated edge stream and let the sort total them.
// Order-independent by addition.
func foldPkgCount(a, b pkgCountRec) (pkgCountRec, error) {
	a.Edges += b.Edges
	return a, nil
}

// foldGroup aggregates one file's candidates into its fileGroup, replacing
// groupByFile's `indexes []int` accumulation (slice.go:46-61) with a streaming
// reduction: sizes and token counts add, the requirement is an OR ("a file
// holding one required entry is required as a whole"), the rank is the minimum
// index, and the path is the one the lowest-indexed member carries -- which is
// the member groupByFile creates the group from.
func foldGroup(a, b groupRec) (groupRec, error) {
	a.Bytes += b.Bytes
	a.Tokens += b.Tokens
	a.Count += b.Count
	a.Required = a.Required || b.Required
	if b.MinIndex < a.MinIndex {
		a.MinIndex, a.Path = b.MinIndex, b.Path
	}
	return a, nil
}

// foldRelAttr merges the two attribute streams of one relation into one row:
// the kind from the edge scan and the multiplier from the evidence resolution.
//
// The multiplier is the MAXIMUM, which is mostPrecise (rank.go:209-217)
// expressed as a fold: the most precise occurrence decides, never the first one
// read, so several providers sealing one edge cannot let an added heuristic row
// lower an edge a compiler already proved. Order-independent.
//
// A producer charges one evidence row with a plain precisionMultiplier lookup,
// which is zero for an unrecognised precision: mostPrecise ignores such a row,
// and precision() supplies the floor a relation with no recognised evidence
// takes. The kind takes whichever side carries one. A relation id has exactly one kind
// in a pinned generation, so two non-empty kinds that differ is not a reachable
// state; if it ever became one, taking a's is a defined answer rather than an
// order-dependent one.
func foldRelAttr(a, b relAttrRec) (relAttrRec, error) {
	if a.Kind == "" {
		a.Kind = b.Kind
	}
	if b.Multiplier > a.Multiplier {
		a.Multiplier = b.Multiplier
	}
	return a, nil
}

// ---------------------------------------------------------------------------
// Sort sizing and lifetime
// ---------------------------------------------------------------------------

// sizeOf* charge one BUFFERED record against a sort's byte budget: the heap the
// run buffer retains while it holds the record, not the length of its encoded
// form, which is written straight to the run file and never accumulates. Each
// counts its variable-length bytes plus a fixed allowance for the struct, its
// string headers and the allocator's rounding.

// recordOverheadBytes is that fixed allowance. It is deliberately generous: a
// run budget that under-charges its records is a heap bound that does not hold.
const recordOverheadBytes = 96

func sizeOfCand(r candRec) int64 {
	n := int64(len(r.NodeID) + len(r.FileID) + len(r.PathAtRank) + len(r.PathFinal) +
		len(r.Requirement) + len(r.Status) + len(r.Excluded))
	for _, reason := range r.Reasons {
		n += int64(len(reason)) + 16
	}
	return n + recordOverheadBytes
}

func sizeOfPath(r pathRec) int64 {
	n := int64(0)
	for _, e := range r.Evidence {
		n += int64(len(e)) + 16
	}
	return n + recordOverheadBytes
}

func sizeOfHop(r hopRec) int64 {
	return int64(len(r.RelationID)+len(r.Kind)) + recordOverheadBytes
}

func sizeOfRelAttr(r relAttrRec) int64 {
	return int64(len(r.RelationID)+len(r.Kind)) + recordOverheadBytes
}

func sizeOfPkgEdge(r pkgEdgeRec) int64 {
	return int64(len(r.Pkg)+len(r.RelationID)) + recordOverheadBytes
}

func sizeOfPkgCount(r pkgCountRec) int64 {
	return int64(len(r.Pkg)) + recordOverheadBytes
}

func sizeOfGroup(r groupRec) int64 {
	return int64(len(r.FileID)+len(r.Path)) + recordOverheadBytes
}

func sizeOfDecision(r decisionRec) int64 {
	return int64(len(r.FileID)) + recordOverheadBytes
}

// encodeRecord and decodeRecord are the one codec every record above uses.
func encodeRecord[T any](v T) ([]byte, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, &model.Error{Code: model.CodeInternal,
			Message: "encoding a context compile record for its external sort: " + err.Error()}
	}
	return b, nil
}

func decodeRecord[T any](b []byte) (T, error) {
	var v T
	if err := json.Unmarshal(b, &v); err != nil {
		var zero T
		return zero, &model.Error{Code: model.CodeInternal,
			Message: "decoding a context compile record from its external sort: " + err.Error()}
	}
	return v, nil
}

// compileSorts is one compile's sort area: where runs are written, how large a
// run buffer each sort gets, and the release list that removes every run on
// every exit path.
//
// The release list is ruling C5's structural half. pagination.ExternalSort
// removes its runs when Sorted() consumes them and NOT when a spill, a collapse
// or an output creation fails (extsort.go:296-303), so every sort owes a Close
// whatever happened. Registering that Close at construction means a pass cannot
// forget one: the compile defers compileSorts.Close once and every sort it ever
// opened is covered, including on the error paths a per-pass defer would miss.
type compileSorts struct {
	// dir is where runs are written. It is the store's own sort directory
	// (pagination.Spools.SortDir), so a compile's temporary bytes sit beside
	// the continuation spools of the same store and are swept and reported as
	// real disk exactly as a spool is.
	dir string
	// runBytes is the in-memory run budget every sort of this compile is built
	// with, so the compile's peak heap is a function of it and the fan-in cap
	// rather than of the candidate count.
	runBytes int64
	// release holds one closer per opened sort and sorted run, newest first.
	release []func() error
	// observed holds one row per sort this compile opened, in open order, so a
	// test can assert the memory invariant this wave exists for: every sort's
	// live working set stays inside the run budget and the sorts that outgrew
	// it spilled rather than growing the heap. It is OBSERVATION ONLY -- no
	// pass reads it and no behaviour depends on it -- and it is bounded by the
	// number of sorts a compile opens, which is a constant of the pipeline and
	// not a function of the repository.
	observed []sortObservation
}

// sortObservation is what one sort reports about its own memory behaviour.
//
// Spilled is READ FROM THE PRIMITIVE (pagination.ExternalSort.SpilledRuns), so
// it is the count of runs the sort wrote rather than a boolean derived from
// Added > Peak. The count is what distinguishes a sort that reached its run
// budget once from one that reached it a thousand times over the same peak.
type sortObservation struct {
	// Name is the sort's name as newSort was called with it.
	Name string
	// Peak is pagination.ExternalSort.PeakLiveRecords at observation time.
	Peak int
	// Added is how many records the sort accepted.
	Added int64
	// Spilled is how many runs the sort wrote to disk; zero means the whole
	// input stayed inside one run buffer.
	Spilled int
}

// observations reports what each of this compile's sorts held, newest sort
// last. It is read by tests only; the compile itself never consults it.
func (s *compileSorts) observations() []sortObservation {
	if s == nil {
		return nil
	}
	out := make([]sortObservation, 0, len(s.observed))
	for _, o := range s.observed {
		out = append(out, o)
	}
	return out
}

// newCompileSorts opens the sort area for one compile.
//
// The run budget is derived from resources.query_memory_bytes and from nothing
// else. That key is the ONE admission a query is charged against, and
// pagination.SortRunBytes takes a sort's documented quarter share of it: a
// second key could be set so the parts oversubscribe the whole, which is the
// failure the single admission exists to prevent. No new bound is introduced
// here -- an admission left unlimited (config.Limit's zero) declares no ceiling
// at all, and the primitive's own floor then applies, which is a performance
// choice and never a reason a compile refuses work.
//
// sortDir is required rather than defaulted. Ruling C5 puts a compile's runs
// under the store's sort directory so they are swept and reported; silently
// falling back to the process temporary directory would put them where nothing
// reclaims them, and this package already refuses every missing dependency
// eagerly (New, compiler.go:99) so a wiring defect surfaces as one.
func newCompileSorts(cfg config.Config, sortDir string) (*compileSorts, error) {
	if sortDir == "" {
		return nil, argumentInvalid("a streamed context compile requires the store's sort directory")
	}
	admission := config.Limit(cfg.Resources.QueryMemoryBytes)
	return &compileSorts{
		dir:      sortDir,
		runBytes: pagination.SortRunBytes(admission.Value()),
	}, nil
}

// track registers a closer to run when the compile's sort area is released.
func (s *compileSorts) track(close func() error) {
	if close != nil {
		s.release = append(s.release, close)
	}
}

// Close releases everything this compile opened, newest first, and reports the
// first failure while still running the rest: a removal that failed must not
// leave the removals behind it undone.
func (s *compileSorts) Close() error {
	if s == nil {
		return nil
	}
	var err error
	for i := len(s.release) - 1; i >= 0; i-- {
		if cerr := s.release[i](); cerr != nil && err == nil {
			err = cerr
		}
	}
	s.release = nil
	return err
}

// newSort opens one external sort in this compile's area, byte-budgeted with
// sizeOf and registered for release. It is a free function and not a method
// because Go methods carry no type parameters of their own.
//
// The record buffer count is left at the primitive's default: runBytes is the
// bound that means something here, since these records vary in size, and the
// count is the secondary guard the primitive already applies.
func newSort[T any](s *compileSorts, name string, compare func(a, b T) int,
	sizeOf func(T) int64) (*pagination.ExternalSort[T], error) {
	if s == nil {
		return nil, argumentInvalid("a context compile sort requires an open sort area")
	}
	sorter, err := pagination.NewExternalSort(s.dir, 0,
		encodeRecord[T], decodeRecord[T], compare)
	if err != nil {
		return nil, err
	}
	// The observation hook is registered with the release list rather than read
	// at Sorted(): Close runs on EVERY exit path, so a sort that failed mid-way
	// still reports the working set it reached, and a sort read to the end
	// reports its final one. Registering it before sorter.Close keeps the
	// newest-first release order running the observation while the sort is
	// still readable.
	idx := len(s.observed)
	s.observed = append(s.observed, sortObservation{Name: name})
	s.track(func() error {
		s.observed[idx] = sortObservation{
			Name: name, Peak: sorter.PeakLiveRecords(), Added: sorter.Len(),
			Spilled: sorter.SpilledRuns(),
		}
		return nil
	})
	s.track(sorter.Close)
	return sorter.WithRunBytes(s.runBytes, sizeOf), nil
}

// trackRun registers a sorted run for release with the compile's sort area, so
// the file Sorted() produced is removed on every exit path and not only the one
// that read it to the end.
//
// Order matters and is why Close runs its list newest first: newSort registers
// the sort BEFORE Sorted() produces the run, so the run file is removed before
// the sort that wrote it. A caller that registered a run ahead of its sort would
// invert that, which is why no caller builds its own release list.
func trackRun[T any](s *compileSorts, run *pagination.SortedRun[T]) *pagination.SortedRun[T] {
	if s != nil && run != nil {
		s.track(run.Close)
	}
	return run
}

// ---------------------------------------------------------------------------
// Passes
// ---------------------------------------------------------------------------
//
// One stub per pass of C-STREAM-plan.md §2, in pipeline order, so the lane
// split is visible in the package before any of it is written. Each owning lane
// completes its own body AND its own parameter list; what is frozen here is the
// set of passes and their order; the arguments each needs are its own.

// passAIngest — §2 P-A, lane L1. expandScope appends to the candidate spool in
// admission order with a seq on every record instead of building
// the whole-set candidate list (refScope.Candidates, stream_parity_test.go),
// and its `admitted map[string]bool` becomes a sort under
// lessEntityID folded with foldMinSeq. Excluded candidates are NOT diverted:
// they stay on the one spool with Excluded set, because relationsOnPaths
// deliberately does not filter on it while hydrateFiles does, and P-I derives
// the exclusion projection by replaying this spool in seq order.
//
// It takes the seed SINK the Section 15.2 producers pushed into, not a slice of
// seeds (ruling C10): by the time this pass runs the seeds are already inside
// the sort area, and what remains is the walk and the folds over them.
func (c *Compiler) passAIngest(ctx context.Context, in *seedIngest, eng *graph.Engine,
	gen model.GenerationID, caps []model.CapabilityState, stop func() bool) (*ingested, bool, error) {
	return c.expandScopeStream(ctx, in, eng, gen, caps, stop)
}

// passBHydrate — §2 P-B, lane L1. Streams the candidate spool in pageLimit()
// batches, resolves each batch's distinct FileIDs through one FilesByID, and
// writes SizeBytes, Status, BOTH path fields (C3) and the FileMissing flag onto
// the record. The accumulating `out`/`byID` of hydrateFiles go away.
//
// As with P-A, the parameter list and result are completed by the owning lane:
// the pass needs the pinned reader and the candidate spool and yields the
// hydrated spool.
func (c *Compiler) passBHydrate(ctx context.Context, s *compileSorts,
	reader *sqlite.PinnedReader, in *pagination.SortedRun[candRec]) (*pagination.SortedRun[candRec], error) {
	return c.hydrateStream(ctx, reader, s, in)
}

// passDRouteScoring — §2 P-D, lane L3. Merge-joins the candidate stream with the
// attributed hop stream on seq; a candidate's hops are contiguous under
// lessHopSeq, so scoreRoutes and scorePath run unchanged on a working set of one
// candidate. Scored records go to the ranked stream and each admitted
// (pkg, relationID) to the centrality sort.
// The parameter list is this lane's to complete (see the header above). The
// three inputs are sorted runs and not spools because P-D is a three-way
// merge-join on Seq: the candidate stream in ingest order, the route stream
// under lessPathSeq and the attributed hop stream under lessHopSeq. The four
// outputs are sorts the caller owns, so one compile's release list covers them.
//
//   - retainedPaths / retainedHops carry the routes scoreRoutes KEPT, renumbered
//     to their retained position, which is the order today's candidate.Paths
//     holds and therefore the order P-I's EvidencePaths must read. A dropped
//     route leaves no record; its existence is disclosed by MorePaths.
//   - edges receives every admitted (package, relation) pair. It is fed from
//     routeScore.admittedEdges and NOT from the retained routes: scoreRoutes
//     appends the edges of a route that is too long to store before it drops it
//     (rank.go's MaxRelationsPerPath branch), so an over-long route still counts
//     towards its package's centrality and towards the test-or-contract boost.
func (c *Compiler) passDRouteScoring(ctx context.Context, s *compileSorts,
	cands *pagination.SortedRun[candRec],
	routes *pagination.SortedRun[pathRec],
	hops *pagination.SortedRun[hopRec],
	retainedPaths *pagination.ExternalSort[pathRec],
	retainedHops *pagination.ExternalSort[hopRec],
	edges *pagination.ExternalSort[pkgEdgeRec],
) (*pagination.SortedRun[scoredRec], error) {
	scored, err := newSort(s, "scored", lessScoredPkg, sizeOfScored)
	if err != nil {
		return nil, err
	}
	nextRoute, stopRoutes := pullRun(routes)
	defer stopRoutes()
	nextHop, stopHops := pullRun(hops)
	defer stopHops()

	// The two peeked records are the only cross-candidate state this pass
	// holds: everything else lives for one candidate.
	route, haveRoute, err := nextRoute()
	if err != nil {
		return nil, err
	}
	hop, haveHop, err := nextHop()
	if err != nil {
		return nil, err
	}
	limit := c.reasonPathLimit()

	walkErr := cands.Each(func(rec candRec) error {
		if err := ctx.Err(); err != nil {
			return contextErr(ctx, err)
		}
		var myRoutes []pathRec
		for haveRoute && route.Seq <= rec.Seq {
			if route.Seq == rec.Seq {
				myRoutes = append(myRoutes, route)
			}
			if route, haveRoute, err = nextRoute(); err != nil {
				return err
			}
		}
		var myHops []hopRec
		for haveHop && hop.Seq <= rec.Seq {
			if hop.Seq == rec.Seq {
				myHops = append(myHops, hop)
			}
			if hop, haveHop, err = nextHop(); err != nil {
				return err
			}
		}
		ws, err := rebuildRoutes(rec.Seq, myRoutes, myHops)
		if err != nil {
			return err
		}
		// rankCandidate reads PathAtRank (C3), which is the path packageOf,
		// the centrality bucket and the boost reason are taken from.
		cand := rec.rankCandidate()
		cand.Paths = ws.paths
		routed, err := scoreRoutes(cand, ws.relations, ws.precision, limit)
		if err != nil {
			return err
		}
		pkg := packageOf(rec.PathAtRank)
		for _, id := range routed.admittedEdges {
			if err := edges.Add(pkgEdgeRec{Pkg: pkg, RelationID: id}); err != nil {
				return err
			}
		}
		for idx, p := range routed.paths {
			if err := retainedPaths.Add(pathRec{Seq: rec.Seq, PathIdx: int32(idx),
				CostUnits: p.CostUnits, Evidence: p.Evidence, HopCount: int32(len(p.Relations))}); err != nil {
				return err
			}
			for hopIdx, id := range p.Relations {
				if err := retainedHops.Add(hopRec{RelationID: id, Seq: rec.Seq,
					PathIdx: int32(idx), HopIdx: int32(hopIdx),
					Kind: ws.relations[id].Kind, Multiplier: ws.precision[id]}); err != nil {
					return err
				}
			}
		}
		// The record is mutated in place and never rebuilt through candRecOf,
		// which would write PathAtRank into PathFinal and collapse the two
		// values ruling C3 keeps apart.
		rec.MorePaths = routed.morePaths
		// The routes that were not enumerated are disclosed BEFORE the route
		// reason and before P-F's boost reasons, exactly as rank appends them:
		// appendReason drops at MaxReasonsPerEntry, so the order decides which
		// explanation an entry at the bound keeps.
		for _, reason := range append(morePathsReason(rec.MorePaths), routed.reasons...) {
			rec.Reasons = appendReason(rec.Reasons, reason)
		}
		base := originContribution(rec.Origin)
		if routed.best > base {
			base = routed.best
		}
		return scored.Add(scoredRec{Cand: rec, Pkg: pkg, Base: base,
			Associated: associatedTestOrContract(routed)})
	})
	if walkErr != nil {
		return nil, walkErr
	}
	run, err := scored.Sorted()
	if err != nil {
		return nil, err
	}
	return trackRun(s, run), nil
}

// passFBoosts — §2 P-F, lane L3. Sorts the ranked stream by package,
// merge-joins it with P-E's counts, applies boostsFor, clamps, appends the
// reasons in today's order, and adds every record to the sort whose comparator
// is lessRank. No fold.
// The parameter list is this lane's to complete. scored is P-D's output under
// lessScoredPkg, counts is P-E's aggregate under lessPkg, and ranked is the sort
// whose comparator is lessRank, which P-G consumes.
//
// The join is LEFT-outer on the package: a package this compile walked no edge
// in has no pkgCountRec, and its candidates take a zero centrality boost rather
// than dropping out of the plan. That is what `len(centrality[pkg])` answers
// today for a package the map never gained a key for.
// It opens no sort of its own -- the ranked sort is P-G's and the caller owns
// it -- so it takes no compileSorts.
func (c *Compiler) passFBoosts(ctx context.Context,
	scored *pagination.SortedRun[scoredRec],
	counts *pagination.SortedRun[pkgCountRec],
	ranked *pagination.ExternalSort[candRec],
) error {
	nextCount, stopCounts := pullRun(counts)
	defer stopCounts()
	count, haveCount, err := nextCount()
	if err != nil {
		return err
	}
	return scored.Each(func(rec scoredRec) error {
		if err := ctx.Err(); err != nil {
			return contextErr(ctx, err)
		}
		for haveCount && count.Pkg < rec.Pkg {
			if count, haveCount, err = nextCount(); err != nil {
				return err
			}
		}
		var edges int64
		if haveCount && count.Pkg == rec.Pkg {
			edges = count.Edges
		}
		// boostsForCounts is the same implementation rank reaches through
		// boostsFor, so the boost set, the clamp and the reason ORDER are one
		// definition and cannot drift between the two pipelines.
		boosts, reasons := boostsForCounts(rec.Cand.rankCandidate(), rec.Associated, edges)
		score := rec.Base + boosts
		if score > maxScoreMicros {
			score = maxScoreMicros
		}
		if score < 0 {
			score = 0
		}
		cand := rec.Cand
		cand.ScoreMicros = score
		for _, reason := range reasons {
			cand.Reasons = appendReason(cand.Reasons, reason)
		}
		return ranked.Add(cand)
	})
}

// passGMeasure — §2 P-G, lane L4. Applies the three `sized` filters on ingest
// from the ranked sort -- Excluded != "", FileID == "", and P-B's FileMissing
// flag -- each emitting its exclusion, so Index counts survivors only and
// matches today's positions exactly. Re-emits under lessFileIndex, where a
// file's records are contiguous: the run's first record is its charged entry,
// measureEntry is exact, and the run yields its groupRec through foldGroup with
// no index list held.
func (c *Compiler) passGMeasure(ctx context.Context, s *compileSorts, in rankedStreams) (*measuredPlan, error) {
	excluded, err := newSort(s, "excluded", lessCandSeq, sizeOfCand)
	if err != nil {
		return nil, err
	}
	byFile, err := newSort(s, "sized", lessFileIndex, sizeOfCand)
	if err != nil {
		return nil, err
	}
	remap, err := newSort(s, "remap", lessCandSeq, sizeOfCand)
	if err != nil {
		return nil, err
	}
	// Index counts SURVIVORS, never records of the ranked stream: the
	// whole-set `i` is a position in `sized` (stream_parity_test.go:465-466),
	// so counting a filtered record
	// would shift every later ordinal and every stored slice membership with it.
	var index int64
	if err := in.Ranked.Each(func(r candRec) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		switch {
		case r.Excluded != "":
			// An earlier pass already ruled this candidate out with a reason;
			// budgeting neither revives it nor drops its reason.
			return excluded.Add(r)
		case r.FileID == "":
			r.Excluded = excludeNoFile
			return excluded.Add(r)
		case r.FileMissing:
			r.Excluded = excludeInvisible
			return excluded.Add(r)
		}
		r.Index = index
		index++
		// The route streams are keyed on Seq and every measuring walk below
		// runs in Index order, so the survivors' (Seq, Index) pairs are kept to
		// re-key them once.
		if err := remap.Add(candRec{Seq: r.Seq, Index: r.Index}); err != nil {
			return err
		}
		return byFile.Add(r)
	}); err != nil {
		return nil, err
	}

	byFileRun, err := sortedRun(s, byFile)
	if err != nil {
		return nil, err
	}
	// A file's records are contiguous and ascending in Index here, so the run's
	// first record is today's charged[FileID] (budget.go:268-273) and the flag
	// can ride the record into the Index-ordered measurement.
	sized, err := newSort(s, "measure", lessSizedIndex, sizeOfSized)
	if err != nil {
		return nil, err
	}
	var lastFile model.FileID
	firstRecord := true
	if err := byFileRun.Each(func(r candRec) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		charged := firstRecord || r.FileID != lastFile
		lastFile, firstRecord = r.FileID, false
		return sized.Add(sizedRec{Cand: r, Charged: charged})
	}); err != nil {
		return nil, err
	}

	paths, hops, err := rekeyRoutes(ctx, s, remap, in)
	if err != nil {
		return nil, err
	}

	sizedRun, err := sortedRun(s, sized)
	if err != nil {
		return nil, err
	}
	groupsByFile, err := newSort(s, "groups-file", lessGroupFile, sizeOfGroup)
	if err != nil {
		return nil, err
	}
	groupsByFile = groupsByFile.WithFold(foldGroup)
	routes := newRouteCursors(paths, hops)
	defer routes.close()
	// Phase one measures every survivor at its position in the FULL order.
	// Selection only ever removes lower-ranked candidates, so the budget is
	// checked against an upper bound and can only be under-filled.
	if err := sizedRun.Each(func(r sizedRec) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		cand := r.Cand.finalCandidate()
		routed, err := routes.take(r.Cand.Index)
		if err != nil {
			return err
		}
		cand.Paths = routed
		// The clip count is taken from phase two: phase one measures every
		// candidate including the ones selection drops.
		e, _, err := measureEntry(cand, int(r.Cand.Index), r.Charged)
		if err != nil {
			return err
		}
		return groupsByFile.Add(groupRec{
			FileID: cand.FileID, Path: cand.Path, Required: isRequired(cand.Requirement),
			Bytes: e.EstimatedBytes, Tokens: e.EstimatedTokens, MinIndex: r.Cand.Index, Count: 1,
		})
	}); err != nil {
		return nil, err
	}

	// foldGroup has reduced each file to one group; lessGroupIndex then puts
	// them in the order groupByFile produces and packPlan must walk.
	byFileGroups, err := sortedRun(s, groupsByFile)
	if err != nil {
		return nil, err
	}
	ranked, err := newSort(s, "groups-rank", lessGroupIndex, sizeOfGroup)
	if err != nil {
		return nil, err
	}
	if err := byFileGroups.Each(ranked.Add); err != nil {
		return nil, err
	}
	groups, err := sortedRun(s, ranked)
	if err != nil {
		return nil, err
	}
	excludedRun, err := sortedRun(s, excluded)
	if err != nil {
		return nil, err
	}
	return &measuredPlan{ByFile: byFileRun, Groups: groups, Excluded: excludedRun,
		Paths: paths, Hops: hops}, nil
}

// rekeyRoutes re-labels the route streams from the candidate's ingest sequence
// to its rank position, dropping the routes of candidates the P-G filters
// excluded. Both sides are ascending in Seq, so it is one merge join holding
// one record per side, and afterwards every measuring walk -- which runs in
// Index order -- reads a candidate's routes in one forward step.
func rekeyRoutes(ctx context.Context, s *compileSorts, remap *pagination.ExternalSort[candRec],
	in rankedStreams) (*pagination.SortedRun[pathRec], *pagination.SortedRun[hopRec], error) {
	remapRun, err := sortedRun(s, remap)
	if err != nil {
		return nil, nil, err
	}
	pathsByIndex, err := newSort(s, "paths-index", lessPathSeq, sizeOfPath)
	if err != nil {
		return nil, nil, err
	}
	hopsByIndex, err := newSort(s, "hops-index", lessHopSeq, sizeOfHop)
	if err != nil {
		return nil, nil, err
	}
	pathCursor := newRunCursor(in.Paths)
	defer pathCursor.stop()
	hopCursor := newRunCursor(in.Hops)
	defer hopCursor.stop()
	if err := remapRun.Each(func(m candRec) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		for pathCursor.ok && pathCursor.cur.Seq < m.Seq {
			pathCursor.advance()
		}
		for pathCursor.ok && pathCursor.cur.Seq == m.Seq {
			p := pathCursor.cur
			p.Seq = m.Index
			if err := pathsByIndex.Add(p); err != nil {
				return err
			}
			pathCursor.advance()
		}
		for hopCursor.ok && hopCursor.cur.Seq < m.Seq {
			hopCursor.advance()
		}
		for hopCursor.ok && hopCursor.cur.Seq == m.Seq {
			h := hopCursor.cur
			h.Seq = m.Index
			if err := hopsByIndex.Add(h); err != nil {
				return err
			}
			hopCursor.advance()
		}
		if err := pathCursor.err(); err != nil {
			return err
		}
		return hopCursor.err()
	}); err != nil {
		return nil, nil, err
	}
	paths, err := sortedRun(s, pathsByIndex)
	if err != nil {
		return nil, nil, err
	}
	hops, err := sortedRun(s, hopsByIndex)
	if err != nil {
		return nil, nil, err
	}
	return paths, hops, nil
}

// passHPack — §2 P-H, lane L4. Walks the group stream ordered by lessGroupIndex
// three times -- SortedRun.Each re-opens its backing file per call and only
// refuses after Close -- for the required floor, the packed slice count at that
// floor, and packPlan's forward walk, which emits decisionRecs in group order
// and drops to the excluded spool in appendDrops order (C1).
func (c *Compiler) passHPack(ctx context.Context, s *compileSorts, m *measuredPlan,
	b resolvedBudget) (*packedPlan, error) {
	if err := checkRequiredFitsStream(ctx, m.Groups, b); err != nil {
		return nil, err
	}
	verdicts, err := newSort(s, "verdicts", lessVerdict, sizeOfVerdict)
	if err != nil {
		return nil, err
	}
	if err := packPlanStream(ctx, m.Groups, b, verdicts.Add); err != nil {
		return nil, err
	}
	run, err := sortedRun(s, verdicts)
	if err != nil {
		return nil, err
	}
	return &packedPlan{Verdicts: run}, nil
}

// passIEmit — §2 P-I, lane L4. Sorts decisions under lessDecision, merge-joins
// them with the measured stream, re-sorts the kept candidates by Index to assign
// the final ordinal, re-measures them (phase two), accumulates RelationsClipped
// and appends to the streamed Plan.Units. Exclusion ordinals stream the excluded
// spool in P-A order and then the packer's drops in P-H order: the exact
// sequence buildPlan produces (C1).
func (c *Compiler) passIEmit(ctx context.Context, s *compileSorts, m *measuredPlan,
	p *packedPlan, b resolvedBudget, sink planSink) (planParts, error) {
	keep, err := newSort(s, "keep", lessKeepIndex, sizeOfKeep)
	if err != nil {
		return planParts{}, err
	}
	drops, err := newSort(s, "drops", lessDropRank, sizeOfDrop)
	if err != nil {
		return planParts{}, err
	}
	// One merge join applies the packer's per-file verdict to every member of
	// that file, which is what keeps `packing.keep` out of the heap. A kept
	// group keeps all of its members and a dropped one drops all of them, so
	// the file's charged entry is the same index in both phases.
	verdicts := newRunCursor(p.Verdicts)
	defer verdicts.stop()
	var lastFile model.FileID
	firstRecord := true
	if err := m.ByFile.Each(func(r candRec) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		charged := firstRecord || r.FileID != lastFile
		lastFile, firstRecord = r.FileID, false
		for verdicts.ok && verdicts.cur.Decision.FileID < r.FileID {
			verdicts.advance()
		}
		if err := verdicts.err(); err != nil {
			return err
		}
		if !verdicts.ok || verdicts.cur.Decision.FileID != r.FileID {
			return &model.Error{Code: model.CodeInternal,
				Message: "a sized candidate reached the plan with no packer verdict for its file"}
		}
		v := verdicts.cur
		if v.Decision.Keep {
			return keep.Add(keepRec{Cand: r, Charged: charged, Slice: v.Decision.SliceIndex, MinIndex: v.MinIndex})
		}
		return drops.Add(dropRec{Cand: r, MinIndex: v.MinIndex, Reason: v.Reason})
	}); err != nil {
		return planParts{}, err
	}

	keepRun, err := sortedRun(s, keep)
	if err != nil {
		return planParts{}, err
	}
	routes := newRouteCursors(m.Paths, m.Hops)
	defer routes.close()
	var out planParts
	var members [][]sliceMember
	// Phase two re-measures the selected entries at their FINAL ordinals, so
	// every persisted size is exact rather than the conservative bound. The
	// walk is in Index order, so the ordinal is the running count and the
	// entries reach the sink already ordered.
	var ordinal int
	if err := keepRun.Each(func(r keepRec) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		cand := r.Cand.finalCandidate()
		routed, err := routes.take(r.Cand.Index)
		if err != nil {
			return err
		}
		cand.Paths = routed
		e, clipped, err := measureEntry(cand, ordinal, r.Charged)
		if err != nil {
			return err
		}
		out.RelationsClipped += clipped
		if err := sink.Entry(e); err != nil {
			return err
		}
		for int(r.Slice) >= len(members) {
			members = append(members, nil)
		}
		members[r.Slice] = append(members[r.Slice], sliceMember{minIndex: r.MinIndex,
			index: r.Cand.Index, ordinal: ordinal, bytes: e.EstimatedBytes, tokens: e.EstimatedTokens})
		ordinal++
		out.Entries++
		return nil
	}); err != nil {
		return planParts{}, err
	}
	if out.Slices, err = assembleSlices(members); err != nil {
		return planParts{}, err
	}
	if err := checkManifestFits(out.Slices, b); err != nil {
		return planParts{}, err
	}

	// Exclusion ordinals keep today's sequence (ruling C1): the pre-sort
	// exclusions in expansion order, then the packer's drops in group order.
	emit := func(c candidate, reason string) error {
		e := model.ExcludedContextEntry{
			Ordinal:   int(out.Excluded),
			Reference: model.ContextReference{NodeID: c.NodeID, FileID: c.FileID, Path: c.Path},
			Reason:    reason,
		}
		out.Excluded++
		return sink.Exclude(e)
	}
	// A pre-sort exclusion carries PathAtRank: buildPlan's unconditional path
	// overwrite runs only on the surviving branch (budget.go:254), so the
	// excluded candidate's reference is the path it arrived with (C3).
	if err := m.Excluded.Each(func(r candRec) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		return emit(r.rankCandidate(), r.Excluded)
	}); err != nil {
		return planParts{}, err
	}
	dropRun, err := sortedRun(s, drops)
	if err != nil {
		return planParts{}, err
	}
	if err := dropRun.Each(func(r dropRec) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		return emit(r.Cand.finalCandidate(), r.Reason)
	}); err != nil {
		return planParts{}, err
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// runCursor -- the pull side of a merge join (P-C, P-D and the budget passes)
// ---------------------------------------------------------------------------

// runCursor turns pagination.SortedRun's push walk into a pull cursor, so a
// merge join can advance one sorted side while the other side is driven by
// something that cannot be inverted -- a paged store read, or another run's
// own walk.
//
// P-C needs exactly that twice. The edge scan advances through the wanted
// relation ids as the store hands back ascending keyset pages, and it must be
// able to stop mid-run when the scan's budget ends; the attribute join advances
// through the resolved attributes as the hop run is walked. Re-walking the
// sorted run from the start for each page instead would make the scan quadratic
// in the number of pages, which is the "complete but slow" outcome §5 exists to
// prevent.
//
// The producer is a goroutine because SortedRun.Each owns the read loop. It is
// bounded and joined: the channel is unbuffered so the producer is never more
// than one record ahead (the cursor holds O(1) records, never a run), and Close
// stops it with a sentinel and waits, so no walk outlives the pass that opened
// it even when the pass returns early on an error.
type runCursor[T any] struct {
	ch     chan T
	stopCh chan struct{}
	done   chan error

	cur     T
	ok      bool
	index   int64
	failure error
	closed  bool
	// drained records that the producer's own result has been collected, so
	// neither next nor Close waits on it twice.
	drained bool
}

// errCursorStopped ends the producer's walk when the consumer closes early. It
// never escapes: Close swallows exactly this error and reports any other.
var errCursorStopped = errors.New("the sorted-run cursor was closed before its run was consumed")

func newRunCursor[T any](run *pagination.SortedRun[T]) *runCursor[T] {
	c := &runCursor[T]{ch: make(chan T), stopCh: make(chan struct{}), done: make(chan error, 1), index: -1}
	go func() {
		err := run.Each(func(v T) error {
			select {
			case c.ch <- v:
				return nil
			case <-c.stopCh:
				return errCursorStopped
			}
		})
		close(c.ch)
		c.done <- err
	}()
	// The cursor is positioned on its first record before it is handed out: a
	// merge join reads `cur`/`ok` directly, and a seeking reader would take the
	// same step itself on its first call.
	c.advance()
	return c
}

// next advances the cursor one record, reporting whether one was read.
func (c *runCursor[T]) next() bool {
	if c.failure != nil || c.closed {
		return false
	}
	v, ok := <-c.ch
	if !ok {
		// The run ended -- but a walk that FAILED closes this channel too, and
		// a merge join that reads `ok` rather than closing the cursor would
		// otherwise read a short stream as a complete one. Collect the walk's
		// result here so err() reports it at the step it happened on.
		c.ok = false
		c.collect()
		return false
	}
	c.cur, c.ok = v, true
	c.index++
	return true
}

// seek advances the cursor to the first record that is not less than the key
// and reports it when it is equal, together with its ordinal in the run.
//
// The key sequence must be non-decreasing, which is what makes this a merge
// join and not a lookup: the cursor never rewinds, so the whole join costs one
// walk of each side.
func (c *runCursor[T]) seek(key T, compare func(a, b T) int) (int64, bool) {
	for {
		if !c.ok && !c.next() {
			return 0, false
		}
		cmp := compare(c.cur, key)
		if cmp > 0 {
			return 0, false
		}
		if cmp == 0 {
			return c.index, true
		}
		c.ok = false
	}
}

// Close stops the producer and returns the walk's error. It is idempotent and
// always joins, so a pass that returns early leaves no goroutine reading a run
// the compile is about to remove.
func (c *runCursor[T]) Close() error {
	if c.closed {
		return c.failure
	}
	c.closed = true
	close(c.stopCh)
	for range c.ch { //nolint:revive // drain so the producer can finish and report
	}
	c.collect()
	return c.failure
}

// collect joins the producer and keeps its error, once. A cursor read to its
// end has already collected it, and waiting on the result channel a second time
// would block forever.
func (c *runCursor[T]) collect() {
	if c.drained {
		return
	}
	c.drained = true
	if err := <-c.done; err != nil && !errors.Is(err, errCursorStopped) {
		c.failure = err
	}
}

// stop releases the cursor from a defer, where the walk's error is read back
// through err() instead. It is Close without the error return.
func (c *runCursor[T]) stop() { _ = c.Close() }

// advance steps the cursor one record, the spelling the budget passes' merge
// joins use. It is next under another name: those joins test the cursor's `ok`
// rather than the step's own result.
func (c *runCursor[T]) advance() { c.next() }

// err reports the walk's failure, so a join loop that ended because its cursor
// could not advance can tell an exhausted run from a failed read.
func (c *runCursor[T]) err() error { return c.failure }

// errStopRun ends a pulled walk early. It never escapes the helper that raises
// it: pullRun swallows exactly this sentinel and reports any other error.
var errStopRun = errors.New("context: sorted run walk stopped")
