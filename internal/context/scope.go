// This file is owned by Task 15 lane L2. It holds
// Section 15.2 required-scope expansion over graph.Engine.Impact, the boundary-to-relation allowlist and the ScopeComplete rules.
//
// The shared contract it builds on (candidate, the ranking constants and the
// typed error constructors) is frozen in compiler.go and is not edited here.
package context

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/Sawmonabo/codectx/internal/config"
	"github.com/Sawmonabo/codectx/internal/graph"
	"github.com/Sawmonabo/codectx/internal/model"
	"github.com/Sawmonabo/codectx/internal/pagination"
)

// scopeRelations is the Section 15.2 boundary allowlist, expressed as the
// relation kinds that reach each boundary: callers and callees (calls),
// contracts and types (implements, extends, overrides, defines), state
// ownership (reads, writes, data_flows_to, control_depends_on), dependencies
// (depends_on, imports), configuration (configures), tests (tests),
// documentation (documents) and integration or registration boundaries
// (references, exports).
//
// Containment kinds are deliberately absent: `contains` and `owns` express
// "same package or module", which Section 15.3 scores as a weak ranking
// contribution, not a boundary a decision depends on. Expanding over them would
// pull a whole package into required scope through one seed.
var scopeRelations = []model.RelationKind{
	model.RelCalls,
	model.RelImplements, model.RelExtends, model.RelOverrides, model.RelDefines,
	model.RelReads, model.RelWrites, model.RelDataFlowsTo, model.RelControlDependsOn,
	model.RelDependsOn, model.RelImports,
	model.RelConfigures,
	model.RelTests,
	model.RelDocuments,
	model.RelReferences, model.RelExports,
}

// scopeResult is the expansion pass's SCALAR verdict: the capability rows that
// made the answer less than complete, the ScopeComplete verdict and the two
// explanation-cut counts. The candidates themselves are not here -- they are
// the sorted streams finishIngest answers -- so nothing about this record is
// sized by the repository, which is what lets a checkpoint carry it whole.
//
// Exclusions are not a separate list either: a candidate whose Excluded reason
// is set is the excluded record (compiler.go), so an omission cannot be dropped
// on the way from one pass to the next.
type scopeResult struct {
	// Completeness holds the non-fresh capability rows behind ScopeComplete,
	// deduplicated and ordered, for the manifest header.
	Completeness []model.CapabilityState
	// ScopeComplete is write-once-false: every assignment below sets it false
	// and none sets it true, so no later pass can repair a truncated walk, a
	// degraded capability or an unresolved seed by scoring around it.
	ScopeComplete bool
	// ReasonsDropped and ReasonsTruncated count the Section 15.3 explanation
	// cuts boundReasons applied across the whole expansion. They are COUNTS and
	// not one notice per entry: an entry-sized disclosure list is
	// repository-sized in heap, which is the shape this wave removes, while the
	// two counts are what a caller deciding whether an explanation is whole
	// actually needs. The compiler turns them into manifest notices.
	ReasonsDropped   int64
	ReasonsTruncated int64
}

// boundaryRequirement is how much of an admitted boundary the actor must read.
// The relation allowlist decides WHICH boundaries are admitted; the node kind
// decides HOW MUCH of one, because that is a property of the artifact rather
// than of the edge that reached it: a contract, a test and a configuration file
// are only meaningful whole, while a calling function is meaningful at its own
// symbol.
//
// Distance weakens the claim: a boundary d hops out is demoted d-1 ranks, so
// the second hop of a dependency chain is recommended rather than required.
// Demotion is by DISTANCE only — never by score, which Section 15.4 forbids.
func boundaryRequirement(kind model.NodeKind, depth int) model.Requirement {
	rank := requirementRank(baseRequirement(kind))
	if depth > 1 {
		rank += depth - 1
	}
	if rank > requirementRank(model.RequirementOptional) {
		rank = requirementRank(model.RequirementOptional)
	}
	return requirementByRank[rank]
}

// requirementByRank inverts requirementRank (compiler.go) so a demotion is
// arithmetic on the single ordering the tie-break chain already uses, rather
// than a second requirement ladder that could drift from it.
var requirementByRank = [...]model.Requirement{
	model.RequirementFull, model.RequirementSymbol,
	model.RequirementRecommended, model.RequirementOptional,
}

// baseRequirement is the requirement of a boundary reached in one hop.
func baseRequirement(kind model.NodeKind) model.Requirement {
	switch kind {
	case model.NodeInterface, model.NodeClass, model.NodeStruct, model.NodeEnum,
		model.NodeTest, model.NodeConfiguration, model.NodeFile:
		// Contracts, types, tests and configuration are read whole: half a
		// contract is a misreading of it, and Section 15.4 forbids serving a
		// partial artifact as if it were complete.
		return model.RequirementFull
	case model.NodeFunction, model.NodeMethod, model.NodeField, model.NodeVariable,
		model.NodeConstant, model.NodeEndpoint, model.NodeDatabaseEntity:
		// A caller, a callee or a state owner is required, at its own symbol.
		return model.RequirementSymbol
	case model.NodeDocument, model.NodePackage, model.NodeModule, model.NodeNamespace,
		model.NodeDependency, model.NodeBuildTarget:
		return model.RequirementRecommended
	}
	// An unknown kind informs rather than binds: it is never silently required.
	return model.RequirementOptional
}

// boundReasons clamps an entry's explanation to the Section 15.3 caps and
// returns what it had to cut. The engine already bounds them, so this guards a
// future engine change from producing a manifest that
// model.ContextEntry.Validate would reject at persistence time, one pass too
// late to explain itself.
//
// The cut is no longer silent. Both halves are counted and surfaced as manifest
// notices by the compiler: an explanation shortened without saying so reads as
// the whole reason an entity was selected. The flag does NOT ride on the
// entry's own Reasons -- the caps this function enforces are exactly the ones
// ContextEntry.Validate checks, so appending a disclosure there would produce
// MaxReasonsPerEntry+1 reasons and turn a disclosed clamp into a failed
// compile. The per-reason cut goes through model.TruncateField, which never
// leaves a partial UTF-8 sequence behind as the raw slice did.
func boundReasons(reasons []string) (out []string, dropped, truncated int64) {
	if len(reasons) > model.MaxReasonsPerEntry {
		dropped = int64(len(reasons) - model.MaxReasonsPerEntry)
		reasons = reasons[:model.MaxReasonsPerEntry]
	}
	out = make([]string, 0, len(reasons))
	for _, r := range reasons {
		bounded, original := model.TruncateField(r, model.MaxReasonBytes)
		if original > len(bounded) {
			truncated++
		}
		out = append(out, bounded)
	}
	return out, dropped, truncated
}

// boundPaths applies context.max_reason_paths_per_entry to the routes an
// expansion entry arrives with, and returns the number it could not carry so
// the caller discloses them rather than losing them.
//
// It no longer floors the setting at model.MaxReasonPathsPerEntry. That
// constant is a report threshold, not a wire ceiling: rank.go honours a
// configured value above it, and re-clamping to 3 here would silently undo
// the operator's setting one lane later -- the class-G shape this wave removes.
// config.Limit owns the test, so unlimited keeps every route.
func boundPaths(paths []model.RelationPath, max config.Limit) ([]model.RelationPath, int64) {
	if !max.Exceeded(int64(len(paths))) {
		return append([]model.RelationPath(nil), paths...), 0
	}
	keep := max.Int()
	return append([]model.RelationPath(nil), paths[:keep]...), int64(len(paths) - keep)
}

// countBoundPaths answers only what boundPaths would have dropped. The walk
// keeps the COUNT on the candidate record and sends the routes themselves to
// the route sorts, so calling boundPaths there allocated and discarded one
// path slice per entry -- once per boundary the walk admits, which is the
// hottest loop P-A has.
func countBoundPaths(paths []model.RelationPath, max config.Limit) int64 {
	if !max.Exceeded(int64(len(paths))) {
		return 0
	}
	return int64(len(paths) - max.Int())
}

// degradedCapabilities is every capability row that is not fresh, from the
// pinned report and from the walk's own disclosure, deduplicated on the
// (provider, capability, scope) identity and ordered so two compiles of the
// same generation produce byte-identical manifest headers.
func degradedCapabilities(reported, disclosed []model.CapabilityState) []model.CapabilityState {
	seen := make(map[[3]string]bool)
	out := make([]model.CapabilityState, 0, len(reported)+len(disclosed))
	for _, group := range [][]model.CapabilityState{reported, disclosed} {
		for _, c := range group {
			if c.State == model.CapabilityFresh {
				continue
			}
			key := [3]string{c.ProviderID, c.Capability, c.Scope}
			if seen[key] {
				continue
			}
			seen[key] = true
			out = append(out, c)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.ProviderID != b.ProviderID {
			return a.ProviderID < b.ProviderID
		}
		if a.Capability != b.Capability {
			return a.Capability < b.Capability
		}
		return a.Scope < b.Scope
	})
	if len(out) == 0 {
		return nil
	}
	return out
}

// ---------------------------------------------------------------------------
// C-STREAM pass P-A: the streamed expansion (lane L1)
// ---------------------------------------------------------------------------

// ingested is what pass P-A hands the rest of the streamed pipeline: the one
// candidate spool in admission order, the two route streams keyed by the same
// seq, and the scope verdict WITHOUT its Candidates slice -- the slice is the
// repo-sized structure this pass exists to remove.
//
// Excluded candidates are on Cands, not diverted: relationsOnPaths deliberately
// does not filter on Excluded while hydrateFiles does, and P-I derives the
// exclusion projection by replaying this spool in seq order (plan Section 2
// P-A, ruling C1). Every run is registered with the compile's sort area, so all
// three are released by compileSorts.Close on every exit path.
type ingested struct {
	Cands *pagination.SortedRun[candRec]
	Paths *pagination.SortedRun[pathRec]
	Hops  *pagination.SortedRun[hopRec]
	Scope scopeResult
}

// lessCandSeq orders a candidate stream by ingest sequence: the admission order
// expandScope appends in today, and the order every later pass replays the
// spool in (P-B's batches, P-I's exclusion projection). It is total because seq
// is assigned once per ingested candidate and never reused.
//
// L0 froze no seq-primary candRec comparator -- lessRank is score-primary and
// lessFileIndex file-primary -- so this is the shared one; P-C and P-I replay
// seq-ordered candidate streams and must use it rather than each defining its
// own.
func lessCandSeq(a, b candRec) int { return cmpInt(a.Seq, b.Seq) }

// seedStage records where in the seed stream one Section 15.2 discovery step
// began. It is what lets a user-set context.max_seeds, applied to the FOLDED
// seed stream, still name the steps it cut: the stream carries no step label,
// only an origin, and several steps share one origin. There is one row per step
// -- a constant of Section 15.2, not a function of the repository.
type seedStage struct {
	origin   originKind
	step     string
	firstSeq int64
}

// seedIngest is the push sink every Section 15.2 producer writes its seeds to
// (ruling C10).
//
// Before it, extractSeeds accumulated every admitted seed in a slice and a
// `seen map[string]bool` beside it, and the compile held both until the
// expansion consumed them: two repository-sized structures on the heap for a
// pipeline whose whole point is that its peak is one run buffer. Now a producer
// pushes each seed as it finds it, straight into the sorts the expansion
// already runs, and nothing between discovery and the walk grows with the seed
// count.
//
// The dedupe moves with it. `add`'s map keyed on the entity identity, first
// writer wins, is exactly what the scope-seed sort's lessEntityID comparator
// and foldMinSeq fold already do over the same key, because seq is assigned in
// discovery order: the earliest step that found an entity keeps it. A seed
// carrying NO entity identity has no dedupe key, and is admitted in arrival
// order rather than dropped -- the map dropped it, silently, for want of a key.
type seedIngest struct {
	sorts *compileSorts
	cfg   config.Context
	// seedSort folds the identity-carrying seeds by entity, keeping the
	// earliest; candSort is the admission-order spool every later pass replays;
	// entitySort is the union fold the walk's entries join the survivors in;
	// the two route sorts take the walk's routes as each page arrives.
	seedSort, entitySort, candSort *pagination.ExternalSort[candRec]
	pathSort                       *pagination.ExternalSort[pathRec]
	hopSort                        *pagination.ExternalSort[hopRec]
	// seq is the arrival counter: the seed's discovery position, then the
	// walk's entries after it. It is the only per-seed state this sink keeps.
	seq int64
	// resolved counts the seeds that carry an identity, which is what decides
	// whether a walk is run at all.
	resolved int
	stages   []seedStage
	scope    scopeResult

	// start, cursor and started are P-A's mid-walk carry. start is the walk's
	// root list, cursor the graph walk's continuation and started the record
	// that foldSeeds has already run.
	//
	// They are fields rather than locals because a plan deadline may end this
	// pass INSIDE the walk: the checkpoint that boundary writes carries these
	// three, and the call that resumes rebuilds a sink out of them. start
	// cannot be recomputed on the continuation -- seedSort is consumed by
	// foldSeeds and nothing after it reads seedSort, stages or resolved -- and
	// it must be repeated byte for byte anyway, because graph.Impact binds the
	// start list into the walk's own query hash.
	start   []model.NodeID
	cursor  string
	started bool
}

// newSeedIngest opens the sort area's seed-side sorts and returns the sink the
// Section 15.2 producers push into.
func (c *Compiler) newSeedIngest(s *compileSorts) (*seedIngest, error) {
	if s == nil {
		return nil, argumentInvalid("a streamed scope expansion requires an open sort area")
	}
	in := &seedIngest{sorts: s, cfg: c.cfg.Context, scope: scopeResult{ScopeComplete: true}}
	var err error
	if in.seedSort, err = newSort[candRec](s, "scope-seed", lessEntityID, sizeOfCand); err != nil {
		return nil, err
	}
	in.seedSort = in.seedSort.WithFold(foldMinSeq)
	if in.entitySort, err = newSort[candRec](s, "scope-entity", lessEntityID, sizeOfCand); err != nil {
		return nil, err
	}
	in.entitySort = in.entitySort.WithFold(foldMinSeq)
	if in.candSort, err = newSort[candRec](s, "scope-cand", lessCandSeq, sizeOfCand); err != nil {
		return nil, err
	}
	// The two route sorts are opened here rather than in finishIngest because
	// the walk emits into them AS EACH PAGE ARRIVES: retaining the pages'
	// entries until the fold had spoken would make the pass hold the whole
	// walk, which is the structure this wave removes. They therefore carry the
	// routes of entries the fold later drops, and finishIngest filters them
	// against the surviving candidate stream.
	if in.pathSort, err = newSort[pathRec](s, "scope-path", lessPathSeq, sizeOfPath); err != nil {
		return nil, err
	}
	if in.hopSort, err = newSort[hopRec](s, "scope-hop", lessHopSeq, sizeOfHop); err != nil {
		return nil, err
	}
	return in, nil
}

// BeginStep names the Section 15.2 step whose seeds follow. A producer calls it
// once per step, before pushing that step's first seed, and the sink records
// only the arrival position the step started at.
func (in *seedIngest) BeginStep(origin originKind, step string) {
	if n := len(in.stages); n > 0 && in.stages[n-1].step == step {
		return
	}
	in.stages = append(in.stages, seedStage{origin: origin, step: step, firstSeq: in.seq})
}

// Admit takes one discovered seed. It is the sink: the whole of what used to be
// expandScope's loop over the accumulated slice, run once per seed as the
// producer finds it.
//
// An excluded candidate is not diverted -- it goes on the admission spool with
// its reason, which is where P-I's exclusion projection reads it -- except that
// an EXPLICIT seed the caller named and nothing answers ends the compile, the
// one Section 15.2 boundary that is an error rather than a discovery answer.
func (in *seedIngest) Admit(sd candidate) error {
	seq := in.seq
	in.seq++
	if sd.Excluded != "" {
		if sd.Origin == originExplicitSeed {
			return scopeIncomplete(sd.Path)
		}
		// An unresolved extracted token stays visible as an exclusion and
		// carries the answer down to discovery rather than out of it.
		in.scope.ScopeComplete = false
		return in.candSort.Add(candRecOf(sd, seq))
	}
	in.resolved++
	sd.Depth = 0
	if sd.Requirement == "" {
		// Every Section 15.2 seed producer assigns its own requirement; the
		// default stands only for a seed that carries none, which would
		// otherwise rank below optional and be droppable.
		sd.Requirement = model.RequirementFull
	}
	if sd.entityID() == "" {
		// No identity, so no dedupe key and no fold: the seed goes straight to
		// the admission spool in arrival order.
		return in.candSort.Add(candRecOf(sd, seq))
	}
	return in.seedSort.Add(candRecOf(sd, seq))
}

// emitRoutes writes one walk entry's routes into the two route sorts.
func (in *seedIngest) emitRoutes(seq int64, raw []model.RelationPath) error {
	paths, _ := boundPaths(raw, in.cfg.MaxReasonPathsPerEntry)
	for pi := range paths {
		p := paths[pi]
		if err := in.pathSort.Add(pathRec{
			Seq: seq, PathIdx: int32(pi), CostUnits: p.CostUnits,
			Evidence: p.Evidence, HopCount: int32(len(p.Relations)),
		}); err != nil {
			return err
		}
		for hi, rel := range p.Relations {
			if err := in.hopSort.Add(hopRec{
				RelationID: rel, Seq: seq, PathIdx: int32(pi), HopIdx: int32(hi),
			}); err != nil {
				return err
			}
		}
	}
	return nil
}

// foldSeeds drains the entity fold into seq order, applies the ONE count bound
// over the seed set -- the user-set context.max_seeds -- and builds the walk's
// start set from the survivors.
//
// The bound is applied HERE, to the folded stream, and not by the producers:
// a producer counting its own admissions counts duplicates the fold has not
// collapsed yet, so the same task cut at the same limit answered differently
// depending on how many of its identities two steps had both named. The set
// this keeps is the first N distinct entities in discovery order, which is what
// the map-and-slice version kept. It is unlimited by default; a limit that
// cuts is disclosed, per step, as an exclusion AND leaves the scope incomplete.
func (in *seedIngest) foldSeeds() ([]model.NodeID, error) {
	s := in.sorts
	seedRun, err := in.seedSort.Sorted()
	if err != nil {
		return nil, err
	}
	trackRun(s, seedRun)
	seedSeqSort, err := newSort[candRec](s, "scope-seedseq", lessCandSeq, sizeOfCand)
	if err != nil {
		return nil, err
	}
	if err := seedRun.Each(func(r candRec) error { return seedSeqSort.Add(r) }); err != nil {
		return nil, err
	}
	seedSeqRun, err := seedSeqSort.Sorted()
	if err != nil {
		return nil, err
	}
	trackRun(s, seedSeqRun)

	limit := in.cfg.MaxSeeds
	admitted := int64(0)
	cutAt := int64(-1)
	// start is built from the deduped survivors in seq order, never at ingest:
	// a duplicate seed never reached the start logic before the fold, so
	// counting it here would both reorder the walk's roots and mis-trigger the
	// overflow disclosure below.
	// The walk's root width is the user-set context.max_start_nodes, unlimited
	// by default, so the capacity hint is the admitted count and never a
	// constant: preallocating a hard ceiling would allocate for a bound that no
	// longer exists.
	width := in.cfg.MaxStartNodes
	start := []model.NodeID{}
	unwalkedRoots := int64(0)
	if err := seedSeqRun.Each(func(r candRec) error {
		if !limit.IsUnlimited() && admitted >= limit.Value() {
			if cutAt < 0 {
				cutAt = r.Seq
			}
			in.scope.ScopeComplete = false
			return nil
		}
		admitted++
		if err := in.entitySort.Add(r); err != nil {
			return err
		}
		if r.NodeID == "" {
			return nil
		}
		if !width.Exceeded(int64(len(start)) + 1) {
			start = append(start, r.NodeID)
			return nil
		}
		// More seeds than the user-set root width admits: the boundaries of the
		// seeds that did not start are unexplored, and the answer says so
		// rather than reading as an exhaustive scope. HOW MANY did not start is
		// counted and disclosed below -- an incomplete verdict on its own does
		// not tell a caller whether one root or a thousand went unexplored.
		//
		// The overflow is never chunked into a second walk: graph.Impact ranks
		// one walk's boundaries globally, so a second walk from the remaining
		// roots would answer its own local ranking rather than extend this
		// one's order.
		unwalkedRoots++
		in.scope.ScopeComplete = false
		return nil
	}); err != nil {
		return nil, err
	}
	if cutAt >= 0 {
		if err := in.discloseCut(cutAt, limit); err != nil {
			return nil, err
		}
	}
	if unwalkedRoots > 0 {
		if err := in.discloseUnwalkedRoots(unwalkedRoots, width); err != nil {
			return nil, err
		}
	}
	return start, nil
}

// discloseUnwalkedRoots reports the resolved seeds that did not become walk
// roots, with their count and the limit that stopped them, as one exclusion row
// on the manifest.
//
// It is reachable only when the operator SET context.max_start_nodes: the key
// is unlimited by default, so a default install walks from every resolvable
// seed and this row never appears. A cut the operator asked for is still never
// silent -- the count is what tells a caller whether one root or a thousand
// went unexplored, which a bare ScopeComplete=false does not.
func (in *seedIngest) discloseUnwalkedRoots(n int64, width config.Limit) error {
	seq := in.seq
	in.seq++
	return in.candSort.Add(candRecOf(candidate{
		// As with seedCut: the row names the step that stopped, because
		// ContextReference refuses a reference naming no entity at all.
		Path:   "seed discovery: walk roots",
		Origin: originExpansion,
		Excluded: boundReason(fmt.Sprintf(
			"%d resolved seeds beyond the context.max_start_nodes limit of %s were not walked; "+
				"their boundaries are unexplored", n, width)),
	}, seq))
}

// discloseCut names every Section 15.2 step the context.max_seeds bound stopped:
// the step the first dropped seed belongs to, and every step after it, one
// exclusion row each, exactly as the per-step cut the producers used to report.
// A step is stopped when any of its arrivals lies at or past the cut.
func (in *seedIngest) discloseCut(cutAt int64, limit config.Limit) error {
	for i, st := range in.stages {
		last := int64(-1)
		if i+1 < len(in.stages) {
			last = in.stages[i+1].firstSeq - 1
		} else {
			last = in.seq - 1
		}
		if last < cutAt {
			continue
		}
		seq := in.seq
		in.seq++
		if err := in.candSort.Add(candRecOf(seedCut(st.origin, st.step, limit), seq)); err != nil {
			return err
		}
	}
	return nil
}

// expandScopeStream is expandScope as sorted streams. It produces the same
// candidates, in the same admission order, with the same scope verdict, holding
// one sort run buffer per sort instead of a whole-set candidate list and
// `admitted map[string]bool` (ruling C2).
//
// The dedupe is two folds and no join. Seeds that participate in today's
// dedupe (resolved, and carrying an entity identity) go through a first
// lessEntityID/foldMinSeq sort so that `start` can be built from the survivors
// before the walk runs; those survivors and every impact entry then go through
// a second one. foldMinSeq over that union reproduces BOTH of today's dedupe
// sites at once: a seed survivor's seq is always smaller than any impact
// entry's, so a boundary the walk reaches again keeps its seed requirement,
// and two impact entries fold to the earlier one.
// Re-folding an already-folded seed survivor is idempotent.
//
// It takes the sink the seeds were pushed into rather than a slice of them:
// the seeds are already inside the sorts by the time this runs, and seq is the
// sink's arrival counter continued by the walk.
// stop is the pass's own halt question, asked after each page of the walk: a
// true answer with more walk left ends the PASS with its carry intact instead
// of ending the compile, and the caller checkpoints it (ruling C7). It is the
// same predicate runPasses asks at a pass boundary, so a deadline that lands
// inside the walk and one that lands behind it drive the identical continuation.
func (c *Compiler) expandScopeStream(ctx context.Context, in *seedIngest, eng *graph.Engine,
	gen model.GenerationID, caps []model.CapabilityState, stop func() bool) (*ingested, bool, error) {
	if in == nil {
		return nil, false, argumentInvalid("a streamed scope expansion requires an open seed sink")
	}
	if eng == nil {
		return nil, false, argumentInvalid("scope expansion requires a graph engine")
	}
	if gen == 0 {
		return nil, false, argumentInvalid("scope expansion requires an explicit pinned generation")
	}
	cfg := in.cfg
	s := in.sorts
	out := &ingested{}
	finish := func() (*ingested, bool, error) {
		out.Scope = in.scope
		done, err := finishIngest(s, out, in.entitySort, in.candSort, in.pathSort, in.hopSort)
		return done, false, err
	}

	// A resumed sink has already folded its seeds and carries the root list it
	// built, so the fold and the two no-walk verdicts below run once per walk
	// and never again on a continuation.
	if !in.started {
		start, err := in.foldSeeds()
		if err != nil {
			return nil, false, err
		}
		in.start, in.started = start, true

		switch {
		case in.resolved == 0:
			// Discovery: nothing resolved, so nothing is required and no walk is
			// run. Seeds already carry their own exclusion reasons.
			in.scope.ScopeComplete = false
			return finish()
		case len(in.start) == 0:
			// Files resolved but no symbol did, so no boundary can be walked from
			// them. Each seed keeps the requirement its Section 15.2 step assigned;
			// the scope is not complete.
			in.scope.ScopeComplete = false
			in.scope.Completeness = degradedCapabilities(caps, nil)
			return finish()
		}
	}
	start := in.start

	// The walk is read TO EXHAUSTION. graph.Impact answers one globally ranked
	// PAGE and hands back the cursor that continues it; reading that page and
	// dropping the cursor delivered a fraction of the boundaries and reported
	// the result as a whole scope. A page is the width of one read, never a
	// ceiling on the walk: the only bounds that may end it are the user-set
	// context.max_graph_depth / max_visited_nodes / max_graph_edges, which ride
	// inside the request and are disclosed through Meta.Truncated.
	//
	// Every continuation repeats the issuing request byte for byte apart from
	// the cursor, including the page limit: graph.Impact binds the limit into
	// the continuation's query hash, and a page that asked for a different one
	// would cut the ranked list somewhere the first page never stopped.
	//
	// Nothing accumulates across pages except the seq counter and the
	// deduplicated capability rows: each page's entries go straight into the
	// dedupe sort and their routes straight into the route sorts, so the pass
	// holds one page and its sorts' run buffers however long the walk runs.
	pageSize := c.pageLimit()
	// The per-page capability rows are the one thing the walk accumulates, so
	// they are seeded from the carry a continuation restored: degradedCapabilities
	// folds an already-folded list to itself, which is what makes resuming the
	// walk leave the disclosure exactly where an uninterrupted walk leaves it.
	disclosed := in.scope.Completeness
	cursor := in.cursor
	for {
		impact, err := eng.Impact(ctx, model.ImpactRequest{
			// The generation is pinned by the cursor on a continuation, and
			// naming both is refused: a cursor already carries the generation
			// its first page was answered from.
			GenerationID: pinnedGeneration(gen, cursor),
			Start:        start,
			Relations:    scopeRelations,
			Direction:    model.DirectionBoth,
			MaxDepth:     cfg.MaxGraphDepth.Int(),
			MaxVisited:   cfg.MaxVisitedNodes.Int(),
			MaxEdges:     cfg.MaxGraphEdges.Int(),
			Page:         model.PageRequest{Limit: pageSize, Cursor: cursor},
		})
		if err != nil {
			return nil, false, contextErr(ctx, err)
		}
		// A page the deadline cut comes back truncated with neither a cursor
		// nor an entry: the walk stalled before it could advance, and
		// graph.Impact deliberately answers no continuation because it would be
		// the cursor this request already carried (impact.go:243-253). Reading
		// that empty cursor as exhaustion would serve a fraction of the walk as
		// a whole scope, so the halt below re-issues this same page instead.
		stalled := impact.Meta.Truncated && impact.Meta.NextCursor == "" && len(impact.Entries) == 0
		next := impact.Meta.NextCursor
		if stalled {
			next = cursor
		}
		halt := stop != nil && stop() && (next != "" || stalled)
		// A page whose truncation is the DEADLINE'S OWN does not narrow the
		// scope when this call halts on it: the continuation is what answers
		// that truncation, and narrowing would make a resumed plan report an
		// incomplete scope the uninterrupted compile never reports.
		//
		// The reason is what decides it, and only the deadline's reason
		// qualifies: a page can carry a user-set depth, frontier or visited
		// bound AND arrive exactly as the halt fires, and suppressing THAT
		// would publish a plan claiming a complete scope over a walk the
		// operator's own bound cut. markTruncated keeps the FIRST reason, so a
		// bound that fired before the deadline is the one on the page.
		//
		// Matched on the documented wire string (docs/queries.md) rather than
		// imported: graph's reason constants are unexported, and the reason a
		// caller reads is the contract both sides share.
		deadlineCut := strings.HasPrefix(impact.Meta.TruncationReason, walkDeadlineReason)
		if impact.Meta.Truncated && !(halt && deadlineCut) {
			in.scope.ScopeComplete = false
		}
		// Deduplicated per page rather than appended: the rows are a property
		// of the generation and every page repeats the same disclosure, so
		// folding them as they arrive keeps this bounded by the number of
		// distinct capabilities instead of by the number of pages.
		disclosed = degradedCapabilities(nil, append(disclosed, impact.Meta.Completeness...))
		for j := range impact.Entries {
			e := impact.Entries[j]
			cand := candidate{
				NodeID: e.NodeID,
				FileID: e.FileID,
				// Path is deliberately left empty: candidate.Path is a FILE PATH
				// everywhere else and ImpactEntry.Name is a qualified name.
				Requirement: boundaryRequirement(e.Kind, e.Depth),
				Origin:      originExpansion,
				Depth:       e.Depth,
			}
			var dropped, truncated int64
			cand.Reasons, dropped, truncated = boundReasons(e.Reasons)
			in.scope.ReasonsDropped += dropped
			in.scope.ReasonsTruncated += truncated
			// Only the disclosure count is kept on the record; the routes
			// themselves go to the route sorts, and finishIngest keeps the ones
			// whose candidate survived the fold.
			cand.MorePaths = countBoundPaths(e.Paths, cfg.MaxReasonPathsPerEntry)
			seq := in.seq
			in.seq++
			if err := in.entitySort.Add(candRecOf(cand, seq)); err != nil {
				return nil, false, err
			}
			if err := in.emitRoutes(seq, e.Paths); err != nil {
				return nil, false, err
			}
		}
		if halt {
			// The pass ends here with its four pre-fold sorts unfolded and the
			// walk's place in hand. Nothing is finished and nothing is dropped:
			// the caller checkpoints this sink and the next call re-enters the
			// loop at `next`.
			in.cursor = next
			in.scope.Completeness = disclosed
			return nil, true, nil
		}
		if impact.Meta.NextCursor == "" {
			break
		}
		cursor = impact.Meta.NextCursor
	}
	in.scope.Completeness = degradedCapabilities(caps, disclosed)
	if len(in.scope.Completeness) > 0 {
		in.scope.ScopeComplete = false
	}
	return finish()
}

// walkDeadlineReason is graph.Impact's truncation reason for a page the query
// deadline ended. It is matched as a PREFIX: the variant for a page that
// stalled before it could advance extends it.
const walkDeadlineReason = "query deadline reached"

// pinnedGeneration is the generation a walk request carries: the pinned one on
// the first page and none on a continuation, because a cursor already pins the
// generation its walk was answered from and naming both is refused
// (PageRequest.ValidatePinned).
func pinnedGeneration(gen model.GenerationID, cursor string) model.GenerationID {
	if cursor != "" {
		return 0
	}
	return gen
}

// finishIngest drains the dedupe survivors into the output sort, materializes
// the candidate spool in seq order, and keeps the routes of the entries that
// survived.
//
// It is the ONE tail of expandScopeStream: the two early returns above reach it
// as well, so no path hands the next pass an unfinalized sort.
//
// The route streams arrive holding a record for EVERY entry the walk delivered,
// because they are written page by page, before the fold has said which entries
// survive. Keeping only the survivors' routes is a forward merge-join on seq:
// both the candidate spool and each route stream ascend in it, so one walk over
// each, with no side buffered, reproduces exactly the set today's loop over the
// admitted candidates emitted. The filter is not optional -- P-C reads the hop
// stream as its `wanted` set, and a dropped duplicate's hops would inflate that
// count, resize the bitset and move the scan's early exit.
//
// Only an expansion entry carries routes: a model.RelationPath is produced by a
// graph walk, and every seed producer (seeds.go) resolves through search and
// attaches none. A seed producer that began attaching one would lose it here,
// and with it the edges it puts on P-C's wanted set, so it must emit its routes
// through this same stream rather than on the candidate.
func finishIngest(s *compileSorts, out *ingested,
	entitySort, candSort *pagination.ExternalSort[candRec],
	pathSort *pagination.ExternalSort[pathRec], hopSort *pagination.ExternalSort[hopRec],
) (*ingested, error) {
	entityRun, err := entitySort.Sorted()
	if err != nil {
		return nil, err
	}
	trackRun(s, entityRun)
	if err := entityRun.Each(func(r candRec) error { return candSort.Add(r) }); err != nil {
		return nil, err
	}
	candRun, err := candSort.Sorted()
	if err != nil {
		return nil, err
	}
	trackRun(s, candRun)

	rawPaths, err := pathSort.Sorted()
	if err != nil {
		return nil, err
	}
	trackRun(s, rawPaths)
	rawHops, err := hopSort.Sorted()
	if err != nil {
		return nil, err
	}
	trackRun(s, rawHops)
	if out.Paths, err = keptBySeq(s, "scope-path-kept", rawPaths, candRun,
		func(r pathRec) int64 { return r.Seq }, lessPathSeq, sizeOfPath); err != nil {
		return nil, err
	}
	if out.Hops, err = keptBySeq(s, "scope-hop-kept", rawHops, candRun,
		func(r hopRec) int64 { return r.Seq }, lessHopSeq, sizeOfHop); err != nil {
		return nil, err
	}
	out.Cands = candRun
	return out, nil
}

// keptBySeq writes the records of src whose Seq appears in cands, in src's own
// order, and returns them as a run.
//
// It is a forward merge-join and holds two records: one candidate and one
// source record. Seq is unique per candidate and both streams ascend in it, so
// the candidate cursor only ever moves forward, and a source record whose seq
// the fold dropped finds no candidate at its seq and is skipped.
func keptBySeq[T any](s *compileSorts, name string, src *pagination.SortedRun[T],
	cands *pagination.SortedRun[candRec], seqOf func(T) int64,
	less func(a, b T) int, sizeOf func(T) int64) (*pagination.SortedRun[T], error) {
	out, err := newSort(s, name, less, sizeOf)
	if err != nil {
		return nil, err
	}
	next, stop := pullRun(cands)
	defer stop()
	cur, have, err := next()
	if err != nil {
		return nil, err
	}
	if err := src.Each(func(v T) error {
		seq := seqOf(v)
		for have && cur.Seq < seq {
			if cur, have, err = next(); err != nil {
				return err
			}
		}
		if have && cur.Seq == seq {
			return out.Add(v)
		}
		return nil
	}); err != nil {
		return nil, err
	}
	run, err := out.Sorted()
	if err != nil {
		return nil, err
	}
	return trackRun(s, run), nil
}
