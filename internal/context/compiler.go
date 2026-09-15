// Package context compiles one immutable model.ContextManifest from a
// model.ContextRequest: seeds, required scope, integer ranking, budgeting,
// slices and the persisted manifest (Section 15). It deliberately shares its
// name with the standard library "context" package, which it imports unaliased
// below at file scope; consumers alias this package contextpkg.
//
// This file holds every symbol that crosses a lane boundary. The signatures
// here are frozen: a lane that needs a different one reports the need rather
// than changing it.
package context

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"strings"
	"time"

	"github.com/Sawmonabo/codectx/internal/config"
	"github.com/Sawmonabo/codectx/internal/graph"
	"github.com/Sawmonabo/codectx/internal/model"
	"github.com/Sawmonabo/codectx/internal/pagination"
	"github.com/Sawmonabo/codectx/internal/search"
	"github.com/Sawmonabo/codectx/internal/storage/sqlite"
)

// Options composes the compiler: Store pins the generation and persists the
// manifest, Search is the Section 15.2 seed resolver, Graph the Section 15.3
// expansion source, Config the Section 20.1 [context] bounds.
type Options struct {
	Store  *sqlite.Store
	Repo   model.RepositoryID
	Search *search.Service
	Graph  GraphFactory
	Config config.Config
	Now    func() time.Time
	Logger *slog.Logger
	// SortDir is where a compile writes its external-sort runs: the workspace's
	// own spool area (pagination.Spools.SortDir), so a compile's temporary
	// bytes are swept, reported and accounted under resources.max_temp_bytes
	// exactly as a continuation spool is (ruling C5'). It is required, not
	// defaulted: falling back to the process temporary directory would put run
	// files where nothing reclaims them.
	SortDir string
}

// GraphFactory opens a bounded engine over ONE explicit generation and returns
// the release that drops its lease. Workspace.Query (app/workspace.go:112)
// satisfies it exactly, so the sqlite Adjacency adapter (app/query.go:59) is
// reused, never duplicated here.
type GraphFactory func(ctx context.Context, gen model.GenerationID) (*graph.Engine, func() error, error)

// Compiler compiles deterministic context manifests. Safe for concurrent use.
type Compiler struct {
	store   *sqlite.Store
	repo    model.RepositoryID
	search  *search.Service
	graph   GraphFactory
	cfg     config.Config
	now     func() time.Time
	log     *slog.Logger
	sortDir string
}

// New validates the options and builds a Compiler. Every dependency is
// required: the composition root builds this eagerly when a workspace opens, so
// a missing one is a wiring defect that must fail there and not per compile,
// where it would look like a data problem.
func New(o Options) (*Compiler, error) {
	switch {
	case o.Store == nil:
		return nil, argumentInvalid("context compiler requires a store")
	case o.Repo == "":
		return nil, argumentInvalid("context compiler requires a repository identity")
	case o.Search == nil:
		return nil, argumentInvalid("context compiler requires a search service")
	case o.Graph == nil:
		return nil, argumentInvalid("context compiler requires a graph factory")
	case o.Now == nil:
		return nil, argumentInvalid("context compiler requires a clock")
	case o.SortDir == "":
		return nil, argumentInvalid("context compiler requires the workspace sort directory")
	}
	log := o.Logger
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &Compiler{store: o.Store, repo: o.Repo, search: o.Search, graph: o.Graph,
		cfg: o.Config, now: o.Now, log: log, sortDir: o.SortDir}, nil
}

// Compile produces the immutable manifest for req. It pins one generation for
// the whole compile and passes that explicit generation into every downstream
// request, so an activation mid-compile can never split one manifest across two
// generations (Section 15.1). A timeout or cancellation returns an explicit
// incomplete answer and never persists a manifest.
//
// The pass order is load-bearing. Seeds and scope decide WHAT is required
// before anything is scored, file metadata is hydrated ONCE for the whole
// candidate set before ranking (the active-change boost reads Status, and the
// budget pass reuses the same rows rather than issuing a second read), and the
// budget runs last, where it may refuse a request but may never shrink the
// required scope it was handed.
func (c *Compiler) Compile(ctx context.Context, req model.ContextRequest) (model.ContextManifest, error) {
	if c == nil {
		return model.ContextManifest{}, &model.Error{Code: model.CodeInternal, Message: "the context compiler was not composed"}
	}
	if err := req.Validate(); err != nil {
		return model.ContextManifest{}, err
	}
	if timeout := c.cfg.Resources.QueryTimeout.Std(); timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}

	reader, err := c.store.PinGeneration(ctx, c.repo, req.GenerationID, c.cfg.Storage.QueryCursorTTL.Std())
	if err != nil {
		return model.ContextManifest{}, contextErr(ctx, err)
	}
	// Opened first, so released last: the graph engine below reads through this
	// same pinned generation, and a lease dropped under it would be a reader
	// outliving what it reads.
	defer reader.Close()
	binding := reader.Binding()
	gen := binding.GenerationID
	if gen == 0 {
		return model.ContextManifest{}, &model.Error{Code: model.CodeNoActiveGeneration,
			Message: "no generation is active for this repository"}
	}

	// Section 15.1: a repeated request reuses the immutable manifest rather
	// than recompiling it, and the identity is computable before any pass runs.
	id, _ := manifestIdentity(binding, req, c.cfg)
	if m, ok, err := c.reuseManifest(ctx, id); err != nil {
		return model.ContextManifest{}, err
	} else if ok {
		// Notices are not persisted (model.ContextManifest.Notices says why),
		// so the reused header carries none. The configuration-derived one is
		// re-emitted here: it is a fact about THIS request's bounds, and a
		// caller who hit the reuse path asked for the same page size as the
		// caller who compiled.
		m.Notices = c.manifestNotices(scopeResult{}, planParts{})
		if !m.ScopeComplete {
			// The reused header knows its scope is partial but not how many
			// candidates were excluded -- the count lives in the stored
			// exclusion projection, not in the manifest -- so the pointer is
			// emitted without a count rather than withheld, and it does not
			// claim exclusions exist: a scope also goes incomplete with none
			// (an unreadable evidence route), so the count-less form promises
			// only the surface, never rows. Without it a caller
			// who hits the reuse path reads scope_complete=false with nothing
			// naming the surface that says why.
			m.Notices = append(m.Notices, excludedViewNotice)
		}
		return m, nil
	}

	seeds, err := c.extractSeeds(ctx, reader, gen, req)
	if err != nil {
		return model.ContextManifest{}, contextErr(ctx, err)
	}
	caps, err := reader.Capabilities(ctx)
	if err != nil {
		return model.ContextManifest{}, contextErr(ctx, err)
	}

	engine, release, err := c.graph(ctx, gen)
	if err != nil {
		return model.ContextManifest{}, contextErr(ctx, err)
	}
	defer release()

	// The streamed pipeline's sort area. Every sort and sorted run below is
	// registered with it, so ONE deferred release removes every run file on
	// every exit path, error paths included (rulings C5/C5').
	sorts, err := newCompileSorts(c.cfg, c.sortDir)
	if err != nil {
		return model.ContextManifest{}, err
	}
	defer func() {
		if cerr := sorts.Close(); cerr != nil {
			// The plan is already decided by the time this runs, so a failed
			// removal must not fail a correct compile -- but it is real
			// temporary disk this workspace stays charged for, so the operator
			// hears about it instead of the sweeper discovering it later.
			c.logger().Warn("a context compile could not release its sort runs",
				"component", "context", "error", cerr)
		}
	}()

	// The budget is resolved BEFORE the passes run. It is a property of the
	// request, not of the plan, so a budget that cannot be resolved is reported
	// without paying for an expansion whose result could never be admitted.
	resolved, err := resolveBudget(req.Budget, c.cfg.Context)
	if err != nil {
		return model.ContextManifest{}, err
	}

	// P-A ingest. An unresolved seed is carried INTO the expansion rather than
	// around it: the expansion is what decides whether an unresolvable identity
	// is the discovery answer of ruling Q7 or the CTX_SCOPE_INCOMPLETE of an
	// explicit boundary, and it can only decide that if it sees the exclusions.
	in, err := c.passAIngest(ctx, sorts, engine, gen,
		append(append([]candidate(nil), seeds.Candidates...), seeds.Excluded...), caps)
	if err != nil {
		return model.ContextManifest{}, contextErr(ctx, err)
	}
	scopeComplete := in.Scope.ScopeComplete && !seeds.Unresolved

	// P-B hydrate.
	hydrated, err := c.passBHydrate(ctx, sorts, reader, in.Cands)
	if err != nil {
		return model.ContextManifest{}, contextErr(ctx, err)
	}

	// P-C relation attributes, over the PRE-hydration spool and before any
	// scoring: that is what relationsOnPaths reads today, and running it here
	// keeps the edge-scan reads ahead of the evidence reads exactly as today's
	// call order does. Both passes may read `in.Cands` because a sorted run is
	// replayable until the sort area is released.
	attrs, err := c.passCRelationAttributes(ctx, sorts, reader, in.Hops, in.Cands)
	if err != nil {
		return model.ContextManifest{}, contextErr(ctx, err)
	}
	if !attrs.Complete {
		// A route whose edges could not all be read is scored as inadmissible,
		// which changes the ranking. Disclosing it keeps that from being a
		// silent difference between two compiles of one generation.
		scopeComplete = false
	}

	// P-D route scoring. The edge sort folds a repeated (package, relation)
	// pair at insertion because P-E counts DISTINCT edges per package; without
	// the fold a package reached twice over one edge would over-count its
	// centrality boost.
	retainedPaths, err := newSort(sorts, "kept-path", lessPathSeq, sizeOfPath)
	if err != nil {
		return model.ContextManifest{}, err
	}
	retainedHops, err := newSort(sorts, "kept-hop", lessHopSeq, sizeOfHop)
	if err != nil {
		return model.ContextManifest{}, err
	}
	edges, err := newSort(sorts, "pkg-edge", lessPkgEdge, sizeOfPkgEdge)
	if err != nil {
		return model.ContextManifest{}, err
	}
	scored, err := c.passDRouteScoring(ctx, sorts, hydrated, in.Paths, attrs.Hops,
		retainedPaths, retainedHops, edges.WithFold(foldPkgEdgeDistinct))
	if err != nil {
		return model.ContextManifest{}, contextErr(ctx, err)
	}

	// P-E centrality, complete before P-F applies a single boost.
	counts, err := c.passECentrality(ctx, sorts, edges)
	if err != nil {
		return model.ContextManifest{}, contextErr(ctx, err)
	}

	// P-F boosts. The ranked sort's comparator IS the Section 15.3 total order,
	// so draining it is the reading order the plan is packed and stored in.
	rankedSort, err := newSort(sorts, "ranked", lessRank, sizeOfCand)
	if err != nil {
		return model.ContextManifest{}, err
	}
	if err := c.passFBoosts(ctx, scored, counts, rankedSort); err != nil {
		return model.ContextManifest{}, contextErr(ctx, err)
	}
	rankedRun, err := sortedRun(sorts, rankedSort)
	if err != nil {
		return model.ContextManifest{}, err
	}
	keptPaths, err := sortedRun(sorts, retainedPaths)
	if err != nil {
		return model.ContextManifest{}, err
	}
	keptHops, err := sortedRun(sorts, retainedHops)
	if err != nil {
		return model.ContextManifest{}, err
	}

	// P-G, P-H and P-I: what buildPlan did whole-set. Their errors are NOT
	// reclassified through contextErr, because this is where CTX_MINIMUM_BUDGET
	// is raised and a floor report must reach the caller as itself.
	measured, err := c.passGMeasure(ctx, sorts, rankedStreams{Ranked: rankedRun, Paths: keptPaths, Hops: keptHops})
	if err != nil {
		return model.ContextManifest{}, err
	}
	packed, err := c.passHPack(ctx, sorts, measured, resolved)
	if err != nil {
		return model.ContextManifest{}, err
	}
	var sink planBuffer
	parts, err := c.passIEmit(ctx, sorts, measured, packed, resolved, &sink)
	if err != nil {
		return model.ContextManifest{}, err
	}

	// The deadline can fire inside ranking or budgeting without any call
	// returning an error, and Section 14.4 forbids persisting a manifest that a
	// cancelled compile produced: the answer is explicitly incomplete instead.
	if err := ctx.Err(); err != nil {
		return model.ContextManifest{}, contextErr(ctx, err)
	}

	// The stored budget is the RESOLVED one: a persisted zero would read as
	// "unlimited" to every later consumer, when it meant "the configured
	// default" at compile time.
	stored := model.Budget{MaxEstimatedTokens: resolved.MaxTokens, MaxBytes: resolved.MaxBytes,
		MaxFiles: resolved.MaxFiles, MaxSlices: resolved.MaxSlices}
	m, err := c.persistManifest(ctx, binding, req, stored, parts, sink.entries, sink.excluded,
		in.Scope.Completeness, scopeComplete)
	if err != nil {
		return model.ContextManifest{}, err
	}
	// Set after persistence, deliberately: the notices describe how THIS
	// compile was bounded, not what the plan is, and they are neither hashed
	// nor stored.
	m.Notices = c.manifestNotices(in.Scope, parts)
	c.logger().Debug("compiled a context manifest", "component", "context", "manifest_id", string(m.ID),
		"entries", m.EntryCount, "slices", m.SliceCount, "scope_complete", m.ScopeComplete)
	return m, nil
}

// planBuffer is the compile's planSink: it collects what P-I emits so the
// single-transaction Store.PutManifest can be handed the whole plan.
//
// Entries are held deliberately and Excluded is held under protest. An entry
// list is bounded by the RESOLVED budget -- the caller's own declared window,
// the same class ruling C4 already accepts for EntryOrdinals and the MaxSlices
// array -- so holding it is a constant-factor extension of an accepted
// structure. The exclusion list is NOT of that class: C-STREAM-plan.md §1 names
// it the unbounded one, and it stays in heap here only because the manifest
// writer takes slices. Streaming it needs a row-at-a-time manifest writer in
// internal/storage/sqlite/state.go, which this lane does not own; the C-INT2
// report carries it as an open defect against that file.
type planBuffer struct {
	entries  []model.ContextEntry
	excluded []model.ExcludedContextEntry
}

func (b *planBuffer) Entry(e model.ContextEntry) error {
	b.entries = append(b.entries, e)
	return nil
}

func (b *planBuffer) Exclude(e model.ExcludedContextEntry) error {
	b.excluded = append(b.excluded, e)
	return nil
}

// manifestNotices is every non-fatal disclosure this compile owes the caller:
// the page size the configuration asked for and could not have, and the counts
// of the Section 15.3 explanation cuts the expansion and the budget pass
// applied. Each is a COUNT and not an entry-sized list -- a per-entry
// disclosure list is repository-sized in heap, which is precisely the shape a
// manifest header may not have -- and each goes through model.TruncateField so
// ContextManifest.Validate never has to refuse a compile over its own notice.
//
// An empty result is the honest answer that nothing was cut, and the field is
// omitempty, so a compile under no bound reads exactly as it did before.
func (c *Compiler) manifestNotices(scoped scopeResult, packed planParts) []string {
	var out []string
	add := func(format string, args ...any) {
		note, _ := model.TruncateField(fmt.Sprintf(format, args...), model.MaxReasonBytes)
		out = append(out, note)
	}
	if note := c.pageLimitNotice(); note != "" {
		out = append(out, note)
	}
	if n := scoped.ReasonsDropped; n > 0 {
		add("%d selection reason(s) past the %d-per-entry bound were not stored; the entries they explain are still listed in full",
			n, model.MaxReasonsPerEntry)
	}
	if n := scoped.ReasonsTruncated; n > 0 {
		add("%d selection reason(s) were truncated to the %d-byte reason bound", n, model.MaxReasonBytes)
	}
	if n := packed.Excluded; n > 0 {
		// The manifest header carries no exclusion list -- exclusions are a
		// paged projection, and embedding them would make the header
		// repository-sized -- so scope_complete=false on its own leaves the
		// caller with no route to the reasons. This notice is that route. Its
		// condition is deliberately NOT scope_complete: a plan can exclude
		// candidates with its scope complete (a budget drop is an exclusion,
		// not a gap), and a scope can be incomplete with nothing excluded, so
		// the count-bearing notice keys on the exclusions it counts.
		add("%d candidate(s) were excluded from this plan, each with a reason; %s",
			n, excludedViewPointer)
	}
	if n := packed.RelationsClipped; n > 0 {
		add("%d evidence route(s) were stored clipped to the first %d relations; the route reaches further than the manifest records",
			n, model.MaxRelationsPerPath)
	}
	return out
}

// excludedViewPointer names the projection that pages the exclusions a manifest
// header only counts, and excludedViewNotice is the count-less form the manifest
// reuse path emits. One spelling, so the two notices can never name different
// commands.
const (
	excludedViewPointer = "page them with `codectx context entries <session-id> --view excluded`"
	excludedViewNotice  = "this plan's scope is incomplete; any excluded candidates and their reasons are paged there too -- " + excludedViewPointer
)

// Close releases the compiler's own resources. It does not close the injected
// Store or Search service, which the composition root owns.
func (c *Compiler) Close() error { return nil }

// logger is the compiler's log sink. New always sets one, but Compile is also
// reachable from a Compiler built field by field, so the discarding default is
// applied here too rather than left as a nil dereference in the one line that
// logs.
func (c *Compiler) logger() *slog.Logger {
	if c.log == nil {
		return slog.New(slog.DiscardHandler)
	}
	return c.log
}

// hydrateStream is pass P-B: hydrateFiles as a stream. It reads the candidate
// spool in pageLimit() batches, resolves each batch's distinct file ids through
// ONE FilesByID, writes SizeBytes, Status, both path values (ruling C3) and the
// snapshot-absent flag onto each record, and re-emits it in seq order. The
// accumulating `out []model.FileVersion` and `byID` of hydrateFiles go away;
// the held set is one batch of records plus one page of file rows.
//
// The two path writes are the two today's pipeline performs and ruling C3
// keeps apart: hydrateFiles fills Path only when the candidate carries none
// (the value ranking reads for packageOf, centrality and its boost reason) and
// buildPlan overwrites it unconditionally (the value the total order and the
// persisted entry read). PathAtRank and PathFinal are those two values.
//
// FileMissing is the fact buildPlan learns from a `meta` lookup miss
// (budget.go:249-252). A streamed P-G holds no such map, so the record that
// observed the miss carries it.
//
// A batch-local byID is equivalent to today's whole-set one because every
// producer of an excluded candidate (seeds.go:60,100,191) sets neither NodeID
// nor FileID: Excluded implies FileID == "", so no excluded candidate can be
// hydrated by a file row another batch requested. hydrateFiles' write-back over
// ALL candidates is nevertheless reproduced below rather than narrowed to the
// eligible ones, so the equivalence is a property of the seed producers and not
// something this pass has baked in.
func (c *Compiler) hydrateStream(ctx context.Context, reader *sqlite.PinnedReader,
	s *compileSorts, in *pagination.SortedRun[candRec]) (*pagination.SortedRun[candRec], error) {
	if reader == nil {
		return nil, argumentInvalid("a streamed hydration requires a pinned reader")
	}
	if in == nil {
		return nil, argumentInvalid("a streamed hydration requires the candidate spool")
	}
	out, err := newSort[candRec](s, "hydrate", lessCandSeq, sizeOfCand)
	if err != nil {
		return nil, err
	}
	limit := c.pageLimit()
	batch := make([]candRec, 0, limit)
	flush := func() error {
		if len(batch) == 0 {
			return nil
		}
		// The eligible set is hydrateFiles' own: a candidate with no file, and
		// an excluded one, are not read for.
		ids := make([]model.FileID, 0, len(batch))
		seen := make(map[model.FileID]struct{}, len(batch))
		for _, r := range batch {
			if r.FileID == "" || r.Excluded != "" {
				continue
			}
			if _, dup := seen[r.FileID]; dup {
				continue
			}
			seen[r.FileID] = struct{}{}
			ids = append(ids, r.FileID)
		}
		byID := make(map[model.FileID]model.FileVersion, len(ids))
		if len(ids) > 0 {
			page, err := reader.FilesByID(ctx, ids)
			if err != nil {
				return err
			}
			for _, fv := range page {
				byID[fv.ID] = fv
			}
		}
		for _, r := range batch {
			if fv, ok := byID[r.FileID]; ok {
				r.SizeBytes, r.Status = fv.Size, fv.Status
				r.PathFinal = fv.Path
				if r.PathAtRank == "" {
					r.PathAtRank = fv.Path
				}
			} else if r.FileID != "" && r.Excluded == "" {
				r.FileMissing = true
			}
			if err := out.Add(r); err != nil {
				return err
			}
		}
		batch = batch[:0]
		return nil
	}
	if err := in.Each(func(r candRec) error {
		batch = append(batch, r)
		if len(batch) < limit {
			return nil
		}
		return flush()
	}); err != nil {
		return nil, err
	}
	if err := flush(); err != nil {
		return nil, err
	}
	run, err := out.Sorted()
	if err != nil {
		return nil, err
	}
	return trackRun(s, run), nil
}

// candidate is the ONE intermediate record that crosses lane boundaries: seed
// extraction fills it, scope marks its requirement, rank scores it, budget
// sizes and packs it, manifest persists it. Nothing else is shared state.
type candidate struct {
	NodeID      model.NodeID
	FileID      model.FileID
	Path        string // normalized, root-relative
	Requirement model.Requirement
	Origin      originKind // which Section 15.2 step produced it
	Depth       int        // 0 for a seed
	StartByte   int64
	ScoreMicros int64
	SizeBytes   int64 // model.FileVersion.Size; 0 until sized
	Status      model.FileStatus
	Reasons     []string             // <= model.MaxReasonsPerEntry, each <= MaxReasonBytes
	Paths       []model.RelationPath // <= cfg.Context.MaxReasonPathsPerEntry
	MorePaths   int64                // routes not enumerated (Section 15.3)
	Excluded    string               // non-empty => excluded, with this reason
}

// originKind names the Section 15.2 seed step, in discovery order.
type originKind int

const (
	originExplicitSeed originKind = iota
	originBacktick
	originPathToken
	originQualified
	originExactResolve
	originLexical
	originChangedFile
	originExpansion
)

// less is the total Section 15.3 order: requirement rank, descending
// ScoreMicros, normalized path, start byte, entity id.
func (a candidate) less(b candidate) bool {
	if ra, rb := requirementRank(a.Requirement), requirementRank(b.Requirement); ra != rb {
		return ra < rb
	}
	if a.ScoreMicros != b.ScoreMicros {
		return a.ScoreMicros > b.ScoreMicros
	}
	if a.Path != b.Path {
		return a.Path < b.Path
	}
	if a.StartByte != b.StartByte {
		return a.StartByte < b.StartByte
	}
	// The last key is unique, so the order is total. A node identity wins over
	// the file identity: a file-level candidate carries no NodeID.
	return a.entityID() < b.entityID()
}

// entityID is the final, unique tie-break key: the node identity when the
// candidate names one, else the file identity.
func (a candidate) entityID() string {
	if a.NodeID != "" {
		return string(a.NodeID)
	}
	return string(a.FileID)
}

// requirementRank orders the first tie-break key (full 0 ... optional 3). An
// unknown requirement sorts last rather than silently ranking as required.
func requirementRank(r model.Requirement) int {
	switch r {
	case model.RequirementFull:
		return 0
	case model.RequirementSymbol:
		return 1
	case model.RequirementRecommended:
		return 2
	case model.RequirementOptional:
		return 3
	}
	return 4
}

// scale is the Section 15.3 fixed point. No float exists in this package.
const scale int64 = 1_000_000

// mulScaled multiplies two scaled integers, rounding half away from zero, and
// reports overflow (Section 15.3 requires the check even though inputs bound
// the product at 4e12).
func mulScaled(a, b int64) (int64, error) {
	p := a * b
	if a != 0 && (p/a != b || (a == -1 && b == math.MinInt64)) {
		return 0, overflowErr(a, b)
	}
	half := scale / 2
	switch {
	case p >= 0 && p > math.MaxInt64-half:
		return 0, overflowErr(a, b)
	case p < 0 && p < math.MinInt64+half:
		return 0, overflowErr(a, b)
	case p >= 0:
		p += half
	default:
		p -= half
	}
	return p / scale, nil
}

// overflowErr reports a ranking arithmetic overflow as a defect, not a user
// input problem: the Section 15.3 weights bound every product well inside
// int64, so reaching this is a wiring error in a new weight.
func overflowErr(a, b int64) *model.Error {
	return (&model.Error{Code: model.CodeInternal, Message: "ranking arithmetic overflowed"}).
		WithDetail("a", fmt.Sprint(a)).WithDetail("b", fmt.Sprint(b))
}

// contributionWeight is the Section 15.3 relation contribution in scaled
// micros. Weights are compile-time constants, not config
// (internal/graph/cost.go:5-14 states why). A relation kind absent from this
// map contributes nothing and is not admitted to a path.
var contributionWeight = map[model.RelationKind]int64{
	model.RelImplements:       940_000,
	model.RelOverrides:        940_000,
	model.RelExtends:          940_000,
	model.RelTests:            920_000,
	model.RelCalls:            900_000,
	model.RelReads:            880_000,
	model.RelWrites:           880_000,
	model.RelDataFlowsTo:      880_000,
	model.RelControlDependsOn: 880_000,
	model.RelReferences:       780_000,
	model.RelExports:          780_000,
	model.RelDependsOn:        720_000,
	model.RelConfigures:       720_000,
	model.RelImports:          600_000,
	model.RelDocuments:        550_000,
	// Same package or module: containment, ownership and definition.
	model.RelContains: 350_000,
	model.RelOwns:     350_000,
	model.RelDefines:  350_000,
}

// precisionMultiplier is the Section 15.3 per-edge precision multiplier in
// scaled micros. Precision lives on Evidence, not Relation; an edge with no
// evidence row takes the heuristic multiplier.
var precisionMultiplier = map[model.Precision]int64{
	model.PrecisionCompiler:       1_000_000,
	model.PrecisionLanguageServer: 950_000,
	model.PrecisionStaticAnalysis: 900_000,
	model.PrecisionSyntax:         720_000,
	model.PrecisionHeuristic:      450_000,
}

const (
	seedContribution    int64 = 1_000_000
	exactContribution   int64 = 980_000
	depthDecay          int64 = 650_000
	boostTaskIdentifier int64 = 200_000
	boostActiveChange   int64 = 120_000
	boostAssociatedTest int64 = 100_000
	boostCentralityMax  int64 = 50_000
	maxBoostMicros      int64 = boostTaskIdentifier + boostActiveChange +
		boostAssociatedTest + boostCentralityMax
)

// compilerPolicyVersion is the frozen code-side ranking/budget policy label
// stored in every manifest header. v2 added budget.max_manifest_bytes to the
// request hash and the canonical projection: a caller budget that decides
// whether a plan is admitted is part of a request's identity, and the label is
// what tells a stored v1 manifest apart from a v2 one rather than letting the
// two collide under one id.
const compilerPolicyVersion = "codectx.context.v2"

// The model.H domains. Two lanes computing a different preimage would make the
// Section 15.1 immutable-manifest reuse lookup silently never hit, so the
// preimages are frozen here and nowhere else.
const (
	manifestIDDomain    = "codectx.manifest.id.v1"
	requestHashDomain   = "codectx.manifest.request.v1"
	canonicalHashDomain = "codectx.manifest.canonical.v1"
	hashFieldSep        = "\x00"
)

// serializedBytes is the ACTUAL canonical-JSON length of v. Section 15.4 puts
// headers, metadata, delimiters and escaping in the budget, so overhead is
// measured, never assumed.
func serializedBytes(v any) (int64, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return 0, &model.Error{Code: model.CodeInternal, Message: "context entry metadata is not serializable: " + err.Error()}
	}
	return int64(len(raw)), nil
}

// wireEncodedBytes is the worst-case transport length of n source bytes:
// base64 is the Section 16.2 fallback encoding, so 4*ceil(n/3).
func wireEncodedBytes(n int64) (int64, error) {
	if n < 0 {
		return 0, argumentInvalid("source byte count must not be negative, got %d", n)
	}
	groups := n / 3
	if n%3 != 0 {
		groups++
	}
	if groups > math.MaxInt64/4 {
		return 0, &model.Error{Code: model.CodeInternal, Message: "wire-encoded size overflowed"}
	}
	return groups * 4, nil
}

// estimateTokens is the ONLY token estimate in this package; the stored label
// is model.EstimateMethodUTF8Bytes and nothing else.
func estimateTokens(bytes int64) (int64, error) {
	tokens, err := model.EstimateTokensUTF8Bytes(bytes)
	if err != nil {
		return 0, err
	}
	return tokens, nil
}

// minimumBudget builds the Section 15.4 typed floor error. Detail keys are
// frozen: min_bytes, min_estimated_tokens, min_slices, min_files, missing.
// Required scope is never demoted, truncated or relabelled to fit a budget, so
// an impossible budget is reported with its floor instead.
func minimumBudget(floorBytes, floorTokens int64, floorSlices, floorFiles int, missing []string) *model.Error {
	return (&model.Error{
		Code:    model.CodeMinimumBudget,
		Message: "the budget cannot hold the required scope",
	}).
		WithDetail("min_bytes", fmt.Sprint(floorBytes)).
		WithDetail("min_estimated_tokens", fmt.Sprint(floorTokens)).
		WithDetail("min_slices", fmt.Sprint(floorSlices)).
		WithDetail("min_files", fmt.Sprint(floorFiles)).
		WithDetail("missing", strings.Join(missing, ",")).
		WithRemediation("raise the budget to at least the reported floor, or narrow the task")
}

// scopeIncomplete reports that a required boundary could not be resolved at
// all. A merely truncated or partially stale expansion sets ScopeComplete=false
// on the manifest instead and is not an error.
func scopeIncomplete(detail string) *model.Error {
	return (&model.Error{
		Code:    model.CodeScopeIncomplete,
		Message: "a required scope boundary could not be resolved",
	}).WithDetail("boundary", detail)
}

// argumentInvalid reports a structurally invalid request or option.
func argumentInvalid(format string, args ...any) *model.Error {
	return &model.Error{Code: model.CodeArgumentInvalid, Message: fmt.Sprintf(format, args...)}
}

// contextErr classifies a failed call against ctx: a deadline and a
// cancellation are explicit, complete-in-themselves incomplete answers
// (Section 14.4, Section 22), never an internal defect. Any other error is
// returned unchanged so a typed *model.Error from a dependency survives.
func contextErr(ctx context.Context, err error) error {
	if err == nil {
		return nil
	}
	switch {
	case errors.Is(err, context.DeadlineExceeded) || errors.Is(ctx.Err(), context.DeadlineExceeded):
		return &model.Error{Code: model.CodeQueryDeadline, Message: "the context compile exceeded its query deadline", Retryable: true}
	case errors.Is(err, context.Canceled) || errors.Is(ctx.Err(), context.Canceled):
		return &model.Error{Code: model.CodeCanceled, Message: "the context compile was canceled"}
	}
	return err
}
