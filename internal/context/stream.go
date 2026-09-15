// This file is the C-STREAM interface freeze (lane L0). It holds the record
// types, comparators, folds and sort factory every streaming pass of the
// context compiler shares, plus one stub per pass P-A .. P-I of
// .superpowers/sdd/implementation-plan/C-STREAM-plan.md §2.
//
// The goal of the wave is that the compiler's peak heap is a function of the
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

	"github.com/Sawmonabo/codectx/internal/config"
	"github.com/Sawmonabo/codectx/internal/model"
	"github.com/Sawmonabo/codectx/internal/pagination"
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

	// PathAtRank and PathFinal are the SAME field in today's candidate, written
	// twice with different rules, and ruling C3 keeps both because collapsing
	// them is not behaviour-preserving. hydrateFiles writes the snapshot path
	// only when the candidate carries none (compiler.go:339-343) and ranking
	// reads THAT value for packageOf, centrality and its reason
	// (rank.go:88,396); buildPlan overwrites unconditionally (budget.go:254)
	// and the total order plus the persisted entry read THAT one. A candidate
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
// the excluded spool in appendDrops order (slice.go:141-146), which is the
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
// PathFinal, the value buildPlan's unconditional overwrite leaves (budget.go:254)
// and the one the total order and the persisted entry read (C3).
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
//     change internal/index/plan/plan.go:958-961 froze its comparator against.

// lessRank IS candidate.less (compiler.go:461-477) as a three-way comparator,
// with Seq as a final key so the order is total.
//
// It reads PathFinal, because today's sort runs AFTER buildPlan's unconditional
// path overwrite (budget.go:254,261). Reading PathAtRank instead would order
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
	// prefix namespaces this compile's run files inside the shared directory.
	prefix string
	// release holds one closer per opened sort and sorted run, newest first.
	release []func() error
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
// eagerly (compiler.go:60-76) so a wiring defect surfaces as one.
func newCompileSorts(cfg config.Config, sortDir string) (*compileSorts, error) {
	if sortDir == "" {
		return nil, argumentInvalid("a streamed context compile requires the store's sort directory")
	}
	admission := config.Limit(cfg.Resources.QueryMemoryBytes)
	return &compileSorts{
		dir:      sortDir,
		runBytes: pagination.SortRunBytes(admission.Value()),
		prefix:   "ctx-compile-",
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
	sorter, err := pagination.NewExternalSort(s.dir, s.prefix+name+"-", 0,
		encodeRecord[T], decodeRecord[T], compare)
	if err != nil {
		return nil, err
	}
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

// errNotImplemented is the body of every pass this freeze declares and does not
// write. It is a defect code and not a user-facing one: reaching it means a
// lane's pass was wired in before it existed, which is a composition error.
func errNotImplemented(pass string) error {
	return &model.Error{Code: model.CodeInternal,
		Message: "the streamed context compile pass " + pass + " is not implemented"}
}

// ---------------------------------------------------------------------------
// Passes
// ---------------------------------------------------------------------------
//
// One stub per pass of C-STREAM-plan.md §2, in pipeline order, so the lane
// split is visible in the package before any of it is written. Each owning lane
// completes its own body AND its own parameter list; what is frozen here is the
// set of passes, their order and which lane owns each, not the arguments a pass
// will need. None has a caller until L5 wires Compile.

// passAIngest — §2 P-A, lane L1. expandScope appends to the candidate spool in
// admission order with a seq on every record instead of building
// res.Candidates, and its `admitted map[string]bool` becomes a sort under
// lessEntityID folded with foldMinSeq. Excluded candidates are NOT diverted:
// they stay on the one spool with Excluded set, because relationsOnPaths
// deliberately does not filter on it while hydrateFiles does, and P-I derives
// the exclusion projection by replaying this spool in seq order.
func (c *Compiler) passAIngest(ctx context.Context, s *compileSorts) error {
	return errNotImplemented("P-A ingest")
}

// passBHydrate — §2 P-B, lane L1. Streams the candidate spool in pageLimit()
// batches, resolves each batch's distinct FileIDs through one FilesByID, and
// writes SizeBytes, Status, BOTH path fields (C3) and the FileMissing flag onto
// the record. The accumulating `out`/`byID` of hydrateFiles go away.
func (c *Compiler) passBHydrate(ctx context.Context, s *compileSorts) error {
	return errNotImplemented("P-B hydrate")
}

// passCRelationAttributes — §2 P-C, lane L2. Explodes the hop stream into a
// sort under lessRelID; its deduplicated key stream is the distinct relation-id
// order resolvePrecision already reads in. Precision walks that stream through
// EvidenceBatch in pageLimit() batches; kind comes from the node scan, whose
// every edge goes into a sort as a relAttrRec instead of into a map. foldRelAttr
// merges the two, and the completeness verdict is the matched distinct count
// against the wanted count -- today's len(out) == len(wanted).
func (c *Compiler) passCRelationAttributes(ctx context.Context, s *compileSorts) error {
	return errNotImplemented("P-C relation attributes")
}

// passDRouteScoring — §2 P-D, lane L3. Merge-joins the candidate stream with the
// attributed hop stream on seq; a candidate's hops are contiguous under
// lessHopSeq, so scoreRoutes and scorePath run unchanged on a working set of one
// candidate. Scored records go to the ranked stream and each admitted
// (pkg, relationID) to the centrality sort.
func (c *Compiler) passDRouteScoring(ctx context.Context, s *compileSorts) error {
	return errNotImplemented("P-D route scoring")
}

// passECentrality — §2 P-E, lane L2. Deduplicates the centrality sort under
// lessPkgEdge with foldPkgEdgeDistinct and counts each package's distinct edges
// into pkgCountRec: today's nested centrality map as a streaming aggregation,
// still complete before any boost applies.
func (c *Compiler) passECentrality(ctx context.Context, s *compileSorts) error {
	return errNotImplemented("P-E centrality")
}

// passFBoosts — §2 P-F, lane L3. Sorts the ranked stream by package,
// merge-joins it with P-E's counts, applies boostsFor, clamps, appends the
// reasons in today's order, and adds every record to the sort whose comparator
// is lessRank. No fold.
func (c *Compiler) passFBoosts(ctx context.Context, s *compileSorts) error {
	return errNotImplemented("P-F boosts")
}

// passGMeasure — §2 P-G, lane L4. Applies the three `sized` filters on ingest
// from the ranked sort -- Excluded != "", FileID == "", and P-B's FileMissing
// flag -- each emitting its exclusion, so Index counts survivors only and
// matches today's positions exactly. Re-emits under lessFileIndex, where a
// file's records are contiguous: the run's first record is its charged entry,
// measureEntry is exact, and the run yields its groupRec through foldGroup with
// no index list held.
func (c *Compiler) passGMeasure(ctx context.Context, s *compileSorts) error {
	return errNotImplemented("P-G provisional measure and groups")
}

// passHPack — §2 P-H, lane L4. Walks the group stream ordered by lessGroupIndex
// three times -- SortedRun.Each re-opens its backing file per call and only
// refuses after Close -- for the required floor, the packed slice count at that
// floor, and packPlan's forward walk, which emits decisionRecs in group order
// and drops to the excluded spool in appendDrops order (C1).
func (c *Compiler) passHPack(ctx context.Context, s *compileSorts) error {
	return errNotImplemented("P-H required check and packing")
}

// passIEmit — §2 P-I, lane L4. Sorts decisions under lessDecision, merge-joins
// them with the measured stream, re-sorts the kept candidates by Index to assign
// the final ordinal, re-measures them (phase two), accumulates RelationsClipped
// and appends to the streamed Plan.Units. Exclusion ordinals stream the excluded
// spool in P-A order and then the packer's drops in P-H order: the exact
// sequence buildPlan produces (C1).
func (c *Compiler) passIEmit(ctx context.Context, s *compileSorts) error {
	return errNotImplemented("P-I ordinals, units, slices and exclusions")
}
