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
	// Spools is the workspace's retained-state store. A compile that ends on
	// its query deadline persists the streams a completed pass produced into a
	// LEASED state directory here (Spools.AdoptDir) and answers a signed
	// continuation cursor instead of a manifest; the next call reopens that
	// directory through Spools.OpenDir and finishes the compile. It is
	// optional: a workspace composed without it (or without Signer/Leases)
	// offers no continuations and a deadline ends the answer as it did before.
	Spools *pagination.Spools
	// Signer signs and verifies the continuation cursor, under the shared
	// pagination.PurposeCursor every other paged surface uses.
	Signer *pagination.Signer
	// Leases owns the retention lease a continuation's state directory is
	// adopted under, so a continuation nobody resumes is reclaimed by the
	// ordinary sweep rather than left behind.
	Leases *pagination.Leases
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
	spools  *pagination.Spools
	signer  *pagination.Signer
	leases  *pagination.Leases
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
	}
	// One sort area, named once. A composition that hands over the spool store
	// takes its sort directory from it rather than repeating it, so a compile's
	// run files and its continuation state can never land in two places.
	if o.SortDir == "" && o.Spools != nil {
		o.SortDir = o.Spools.SortDir()
	}
	switch {
	case o.SortDir == "":
		return nil, argumentInvalid("context compiler requires the workspace sort directory")
	}
	log := o.Logger
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &Compiler{store: o.Store, repo: o.Repo, search: o.Search, graph: o.Graph,
		cfg: o.Config, now: o.Now, log: log, sortDir: o.SortDir,
		spools: o.Spools, signer: o.Signer, leases: o.Leases}, nil
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
	res, err := c.compile(ctx, req, "", false)
	if err != nil {
		return model.ContextManifest{}, err
	}
	return res.Manifest, nil
}

// CompileResult is one call of a CONTINUABLE compile: either the finished
// manifest, or the report that this call ended on its query deadline at a pass
// boundary together with the token the next call resumes from.
//
// The two are exclusive by construction. A truncated result carries the zero
// manifest and nothing is persisted (Section 14.4): a partial plan is never
// stored, so no later reader can mistake a continuation for an answer.
type CompileResult struct {
	Manifest         model.ContextManifest
	Truncated        bool
	TruncationReason string
	NextCursor       string
}

// truncationDeadline is the only reason a compile ends at a pass boundary.
const truncationDeadline = "deadline"

// CompilePage compiles req, resuming from cursor when one is given, and ends at
// a pass boundary rather than failing when the query deadline fires (ruling
// C7). An empty cursor starts the compile from its first pass.
//
// A caller that presents no cursor and ignores NextCursor gets exactly what
// Compile gives it, because Compile is this method with continuations switched
// off: a workspace with no spool store, signer or lease store mints no token
// and the deadline ends the answer as an error, unchanged.
func (c *Compiler) CompilePage(ctx context.Context, req model.ContextRequest, cursor string) (CompileResult, error) {
	return c.compile(ctx, req, cursor, true)
}

// compile is the whole pipeline. paging says whether this call may end at a
// pass boundary with a continuation instead of raising the deadline.
func (c *Compiler) compile(ctx context.Context, req model.ContextRequest, cursor string, paging bool) (CompileResult, error) {
	if c == nil {
		return CompileResult{}, &model.Error{Code: model.CodeInternal, Message: "the context compiler was not composed"}
	}
	if err := req.Validate(); err != nil {
		return CompileResult{}, err
	}
	if timeout := c.cfg.Resources.QueryTimeout.Std(); timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}

	reader, err := c.store.PinGeneration(ctx, c.repo, req.GenerationID, c.cfg.Storage.QueryCursorTTL.Std())
	if err != nil {
		return CompileResult{}, contextErr(ctx, err)
	}
	// Opened first, so released last: the graph engine below reads through this
	// same pinned generation, and a lease dropped under it would be a reader
	// outliving what it reads.
	defer reader.Close()
	binding := reader.Binding()
	gen := binding.GenerationID
	if gen == 0 {
		return CompileResult{}, &model.Error{Code: model.CodeNoActiveGeneration,
			Message: "no generation is active for this repository"}
	}

	// Section 15.1: a repeated request reuses the immutable manifest rather
	// than recompiling it, and the identity is computable before any pass runs.
	id, _ := manifestIdentity(binding, req, c.cfg)
	// The manifest identity IS the continuation's request hash: it already
	// folds the binding, the request and the configured bounds, so a token can
	// never resume a different request and a configuration change between two
	// calls ends the continuation rather than splicing two compiles.
	requestHash := string(id)
	if m, ok, err := c.reuseManifest(ctx, id); err != nil {
		return CompileResult{}, err
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
		return CompileResult{Manifest: m}, nil
	}

	// A presented cursor is validated and its state opened BEFORE any sort is
	// created, so a token this compile may not resume costs nothing.
	var resumed *resumedHalf
	if cursor != "" {
		r, rerr := c.openResumed(ctx, cursor, binding, requestHash)
		if rerr != nil {
			return CompileResult{}, rerr
		}
		// The checkpoint's runs are ADOPTED by this call's sort area and
		// consumed by the merge that reads them, so the retention ends with
		// this call whether it finishes the plan or fails: a continuation is
		// resumable once, exactly as a spooled page is.
		defer c.releaseState(ctx, r.Cursor.StateID, r.Cursor.LeaseID)
		resumed = r
	}

	// The streamed pipeline's sort area. Every sort and sorted run below is
	// registered with it, so ONE deferred release removes every run file on
	// every exit path, error paths included (rulings C5/C5').
	sorts, err := newCompileSorts(c.cfg, c.sortDir)
	if err != nil {
		return CompileResult{}, err
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
		return CompileResult{}, err
	}

	// P-A through P-F, or the checkpoint a previous call left instead of them.
	var (
		ranked        rankedStreams
		scoped        scopeResult
		scopeComplete bool
	)
	if resumed != nil {
		ranked, scoped, scopeComplete, err = c.restoreHalf(sorts, resumed)
		if err != nil {
			return CompileResult{}, err
		}
	} else {
		ranked, scoped, scopeComplete, err = c.frontHalf(ctx, reader, gen, req, sorts)
		if err != nil {
			return CompileResult{}, err
		}
		// The pass boundary ruling C7 names. Every stream the budget half reads
		// is complete and nothing has been persisted, so this is where a
		// compile that has run out of deadline can stop and be continued. The
		// checkpoint is taken ONLY here, because this is the only boundary
		// whose carry is persisted; a deadline anywhere else still ends the
		// call as the error it was before.
		if paging && c.deadlineReached(ctx) {
			token, cerr := c.checkpointHalf(ctx, binding, requestHash, ranked, scoped, scopeComplete)
			if cerr != nil {
				return CompileResult{}, cerr
			}
			return CompileResult{Truncated: true, TruncationReason: truncationDeadline, NextCursor: token}, nil
		}
	}

	// P-G, P-H and P-I: what buildPlan did whole-set. Their errors are NOT
	// reclassified through contextErr, because this is where CTX_MINIMUM_BUDGET
	// is raised and a floor report must reach the caller as itself.
	measured, err := c.passGMeasure(ctx, sorts, ranked)
	if err != nil {
		return CompileResult{}, err
	}
	packed, err := c.passHPack(ctx, sorts, measured, resolved)
	if err != nil {
		return CompileResult{}, err
	}
	// The exclusion projection is spooled, not collected. It is the one
	// repository-sized list a compile produces, so P-I streams it into this
	// sort and persistManifest replays the sorted run twice -- once to fold the
	// canonical hash, once to write the rows -- instead of holding it. The sort
	// is registered with the compile's area, so its run files are removed on
	// every exit path with every other run, and it is opened here rather than
	// inside P-I so that release order stays the area's single responsibility.
	exclSort, err := newSort(sorts, "excluded", lessExcludedOrdinal, sizeOfExcluded)
	if err != nil {
		return CompileResult{}, err
	}
	sink := planBuffer{excluded: exclSort}
	parts, err := c.passIEmit(ctx, sorts, measured, packed, resolved, &sink)
	if err != nil {
		return CompileResult{}, err
	}
	exclRun, err := sortedRun(sorts, exclSort)
	if err != nil {
		return CompileResult{}, err
	}

	// The deadline can fire inside ranking or budgeting without any call
	// returning an error, and Section 14.4 forbids persisting a manifest that a
	// cancelled compile produced: the answer is explicitly incomplete instead.
	if err := ctx.Err(); err != nil {
		return CompileResult{}, contextErr(ctx, err)
	}

	// The stored budget is the RESOLVED one: a persisted zero would read as
	// "unlimited" to every later consumer, when it meant "the configured
	// default" at compile time.
	stored := model.Budget{MaxEstimatedTokens: resolved.MaxTokens, MaxBytes: resolved.MaxBytes,
		MaxFiles: resolved.MaxFiles, MaxSlices: resolved.MaxSlices}
	// exclRun is read INSIDE persistManifest, and the sort area's deferred
	// release above is what removes it afterwards. That ordering is the reason
	// the release is deferred in Compile rather than taken by whichever pass
	// produced the run: the exclusion run outlives P-I and dies with the
	// compile, not with the pass.
	m, err := c.persistManifest(ctx, binding, req, stored, parts, sink.entries, exclRun,
		scoped.Completeness, scopeComplete)
	if err != nil {
		return CompileResult{}, err
	}
	// Set after persistence, deliberately: the notices describe how THIS
	// compile was bounded, not what the plan is, and they are neither hashed
	// nor stored.
	m.Notices = c.manifestNotices(scoped, parts)
	c.logger().Debug("compiled a context manifest", "component", "context", "manifest_id", string(m.ID),
		"entries", m.EntryCount, "slices", m.SliceCount, "scope_complete", m.ScopeComplete)
	return CompileResult{Manifest: m}, nil
}

// frontHalf runs P-A through P-F: ingest, hydrate, relation attributes, route
// scoring, centrality and the boosts. Its answer is the three sorted streams
// the budget half consumes, plus the scope carry a manifest header and its
// notices are built from.
//
// It is a method of its own because it is exactly what a resumed compile does
// NOT run: a continuation restores these three streams and this carry from its
// checkpoint and enters the budget half directly. Keeping the boundary as a
// function signature is what makes "the checkpoint holds everything the second
// half reads" a thing the compiler checks rather than a comment.
func (c *Compiler) frontHalf(ctx context.Context, reader *sqlite.PinnedReader, gen model.GenerationID,
	req model.ContextRequest, sorts *compileSorts,
) (rankedStreams, scopeResult, bool, error) {
	seeds, err := c.extractSeeds(ctx, reader, gen, req)
	if err != nil {
		return rankedStreams{}, scopeResult{}, false, contextErr(ctx, err)
	}
	caps, err := reader.Capabilities(ctx)
	if err != nil {
		return rankedStreams{}, scopeResult{}, false, contextErr(ctx, err)
	}

	engine, release, err := c.graph(ctx, gen)
	if err != nil {
		return rankedStreams{}, scopeResult{}, false, contextErr(ctx, err)
	}
	defer release()

	// P-A ingest. An unresolved seed is carried INTO the expansion rather than
	// around it: the expansion is what decides whether an unresolvable identity
	// is the discovery answer of ruling Q7 or the CTX_SCOPE_INCOMPLETE of an
	// explicit boundary, and it can only decide that if it sees the exclusions.
	in, err := c.passAIngest(ctx, sorts, engine, gen,
		append(append([]candidate(nil), seeds.Candidates...), seeds.Excluded...), caps)
	if err != nil {
		return rankedStreams{}, scopeResult{}, false, contextErr(ctx, err)
	}
	scopeComplete := in.Scope.ScopeComplete && !seeds.Unresolved

	// P-B hydrate.
	hydrated, err := c.passBHydrate(ctx, sorts, reader, in.Cands)
	if err != nil {
		return rankedStreams{}, scopeResult{}, false, contextErr(ctx, err)
	}

	// P-C relation attributes, over the PRE-hydration spool and before any
	// scoring: that is what relationsOnPaths reads today, and running it here
	// keeps the edge-scan reads ahead of the evidence reads exactly as today's
	// call order does. Both passes may read `in.Cands` because a sorted run is
	// replayable until the sort area is released.
	attrs, err := c.passCRelationAttributes(ctx, sorts, reader, in.Hops, in.Cands)
	if err != nil {
		return rankedStreams{}, scopeResult{}, false, contextErr(ctx, err)
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
		return rankedStreams{}, scopeResult{}, false, err
	}
	retainedHops, err := newSort(sorts, "kept-hop", lessHopSeq, sizeOfHop)
	if err != nil {
		return rankedStreams{}, scopeResult{}, false, err
	}
	edges, err := newSort(sorts, "pkg-edge", lessPkgEdge, sizeOfPkgEdge)
	if err != nil {
		return rankedStreams{}, scopeResult{}, false, err
	}
	scored, err := c.passDRouteScoring(ctx, sorts, hydrated, in.Paths, attrs.Hops,
		retainedPaths, retainedHops, edges.WithFold(foldPkgEdgeDistinct))
	if err != nil {
		return rankedStreams{}, scopeResult{}, false, contextErr(ctx, err)
	}

	// P-E centrality, complete before P-F applies a single boost.
	counts, err := c.passECentrality(ctx, sorts, edges)
	if err != nil {
		return rankedStreams{}, scopeResult{}, false, contextErr(ctx, err)
	}

	// P-F boosts. The ranked sort's comparator IS the Section 15.3 total order,
	// so draining it is the reading order the plan is packed and stored in.
	rankedSort, err := newSort(sorts, "ranked", lessRank, sizeOfCand)
	if err != nil {
		return rankedStreams{}, scopeResult{}, false, err
	}
	if err := c.passFBoosts(ctx, scored, counts, rankedSort); err != nil {
		return rankedStreams{}, scopeResult{}, false, contextErr(ctx, err)
	}
	rankedRun, err := sortedRun(sorts, rankedSort)
	if err != nil {
		return rankedStreams{}, scopeResult{}, false, err
	}
	keptPaths, err := sortedRun(sorts, retainedPaths)
	if err != nil {
		return rankedStreams{}, scopeResult{}, false, err
	}
	keptHops, err := sortedRun(sorts, retainedHops)
	if err != nil {
		return rankedStreams{}, scopeResult{}, false, err
	}

	return rankedStreams{Ranked: rankedRun, Paths: keptPaths, Hops: keptHops}, in.Scope, scopeComplete, nil
}

// planBuffer is the compile's planSink: it collects the entries P-I emits so
// the single-transaction Store.PutManifest can be handed them, and spools the
// exclusions it emits so that method never sees a slice of them.
//
// The split is the memory class of each list. An entry list is bounded by the
// RESOLVED budget -- the caller's own declared window, the same class ruling C4
// already accepts for EntryOrdinals and the MaxSlices array -- so holding it is
// a constant-factor extension of an accepted structure. The exclusion list is
// the one C-STREAM-plan.md §1 names unbounded: it goes to the external sort,
// whose peak live records are a function of the run budget and never of how
// many candidates were excluded.
type planBuffer struct {
	entries []model.ContextEntry
	// excluded is the spool P-I's exclusions stream into, in the ordinal order
	// P-I assigns them (ruling C1). It is required: a nil sink here would drop
	// every exclusion silently, which is the omission Section 15.4 forbids.
	excluded *pagination.ExternalSort[model.ExcludedContextEntry]
}

func (b *planBuffer) Entry(e model.ContextEntry) error {
	b.entries = append(b.entries, e)
	return nil
}

func (b *planBuffer) Exclude(e model.ExcludedContextEntry) error {
	if b.excluded == nil {
		return &model.Error{Code: model.CodeInternal,
			Message: "the compiled plan's exclusion spool was not opened"}
	}
	return b.excluded.Add(e)
}

// lessExcludedOrdinal orders the exclusion projection by the ordinal P-I
// assigned, which IS ruling C1's sequence (pre-sort exclusions in expansion
// order, then the packer's drops in group order). Ordinals are unique, so the
// order is total and the sort is a spill-capable identity over an already
// ordered stream rather than a re-ordering.
func lessExcludedOrdinal(a, b model.ExcludedContextEntry) int {
	switch {
	case a.Ordinal < b.Ordinal:
		return -1
	case a.Ordinal > b.Ordinal:
		return 1
	}
	return 0
}

// sizeOfExcluded charges one exclusion against the run budget: the two ids, the
// path and the reason, plus the per-record overhead every other record pays.
func sizeOfExcluded(x model.ExcludedContextEntry) int64 {
	return int64(len(x.Reference.NodeID)+len(x.Reference.FileID)+len(x.Reference.Path)+len(x.Reason)) +
		recordOverheadBytes
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
