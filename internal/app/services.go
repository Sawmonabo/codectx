package app

import (
	"context"
	"errors"
	"strconv"
	"time"

	"github.com/Sawmonabo/codectx/internal/graph"
	"github.com/Sawmonabo/codectx/internal/model"
	"github.com/Sawmonabo/codectx/internal/pagination"
	"github.com/Sawmonabo/codectx/internal/storage/sqlite"
	"github.com/Sawmonabo/codectx/internal/workflow"
)

// Services is the frozen application facade of Section 19.2: the one surface
// the CLI (Task 18) and the MCP server (Task 19) are planned against, so both
// product adapters drive the same operations rather than two drifting copies.
//
// It is a struct rather than a set of methods on *Workspace because *Workspace
// already owns the names Search, Coverage and Close, which collide with
// ExploreService.Search and ContextService.CloseSession. The four narrow
// interfaces below exist for consumers to depend on; *Services satisfies all
// four.
//
// Every signature uses only internal/model types (Section 17, Step 3:
// "Interface declarations depend only on model contracts"), so no engine, view,
// store or *sql.Tx can leave this package. Each method Validate()s its request
// before touching a service and converts typed errors without carrying raw
// paths, SQL or secrets.
//
// Lifetime: a *Services is created per *Workspace and is valid only for that
// workspace's lifetime. The CLI opens and closes a report workspace per
// command, so a *Services must never be cached across commands.
type Services struct {
	w *Workspace
}

// Services is the one accessor Tasks 18 and 19 call. It is cheap: the services
// it routes to were composed when the workspace opened.
func (w *Workspace) Services() *Services { return &Services{w: w} }

// The four interfaces are asserted against *Services here rather than at each
// consumer, so a signature that drifts fails in this package instead of in the
// CLI and the MCP server independently.
var (
	_ IndexService    = (*Services)(nil)
	_ ExploreService  = (*Services)(nil)
	_ ContextService  = (*Services)(nil)
	_ DiagnoseService = (*Services)(nil)
)

// IndexService builds and reports on generations. `init`, `watch` and the
// `tools` commands are deliberately not here: they write project config,
// stream, or need Workspace.Resolver()/ToolStore() directly, and Section 19.2
// lists none of them as MCP tools.
type IndexService interface {
	Index(ctx context.Context, req model.IndexRequest) (model.IndexResult, error)
	Refresh(ctx context.Context, req model.IndexRequest) (model.IndexResult, error)
	IndexStatus(ctx context.Context) (model.IndexStatus, error)
}

// ExploreService answers the read-only discovery and traversal endpoints.
// Symbol and References carry SemanticSource and Profile, so the same two
// methods serve the canonical index and the LSP overlay; GraphRequest,
// PathRequest and ImpactRequest carry none, because traversal is canonical-only.
type ExploreService interface {
	Overview(ctx context.Context, req model.OverviewRequest) (model.Page[model.OverviewItem], error)
	Search(ctx context.Context, req model.SearchRequest) (model.Page[model.SearchHit], error)
	Symbol(ctx context.Context, req model.SymbolRequest) (model.Page[model.Node], error)
	References(ctx context.Context, req model.ReferenceRequest) (model.Page[model.ReferenceOccurrence], error)
	Graph(ctx context.Context, req model.GraphRequest) (model.GraphResult, error)
	Path(ctx context.Context, req model.PathRequest) (model.PathResult, error)
	Impact(ctx context.Context, req model.ImpactRequest) (model.Page[model.ImpactEntry], error)
}

// ContextService is the Section 16/17 session lifecycle end to end: plan, read
// to coverage, review, advance, seal and close. Plan returns the opened
// session's status beside the manifest so `context plan` shows both without a
// second round trip.
type ContextService interface {
	Plan(ctx context.Context, req model.PlanRequest) (model.PlanResult, model.SessionStatus, error)
	SessionStatus(ctx context.Context, req model.SessionRequest, page model.PageRequest) (model.Page[model.FileCoverage], model.SessionStatus, error)
	Next(ctx context.Context, req model.SessionRequest) (model.NextContextItem, error)
	Entries(ctx context.Context, req model.ContextPageRequest) (model.ContextPage, error)
	Include(ctx context.Context, req model.IncludeRequest) (model.SessionStatus, error)
	Read(ctx context.Context, req model.ReadChunkRequest) (model.ReadChunkResponse, error)
	Acknowledge(ctx context.Context, req model.AcknowledgeRequest) (model.SessionStatus, error)
	Waive(ctx context.Context, req model.WaiverRequest) (model.WaiverRecord, model.SessionStatus, error)
	Record(ctx context.Context, req model.ObservationRequest) (model.Observation, model.SessionStatus, error)
	Advance(ctx context.Context, req model.AdvanceRequest) (model.WorkflowStatus, model.SessionStatus, error)
	Capsule(ctx context.Context, req model.CapsuleRequest) (model.CapsulePage, error)
	Export(ctx context.Context, req model.SessionRequest) (model.Capsule, error)
	CloseSession(ctx context.Context, req model.SessionRequest, expectedVersion int) (model.SessionStatus, error)
}

// DiagnoseService reports on this installation's health. `doctor --deep` is not
// here for the same reason `init` is not on IndexService.
type DiagnoseService interface {
	Doctor(ctx context.Context, req model.DoctorRequest) (model.DoctorReport, error)
}

// --- IndexService -----------------------------------------------------------

// Index builds a new generation over this workspace. The coordinator owns the
// workspace-lock check and the run mutex, so this adds no second gate: it
// validates the request and hands it over.
func (s *Services) Index(ctx context.Context, req model.IndexRequest) (model.IndexResult, error) {
	if err := req.Validate(); err != nil {
		return model.IndexResult{}, err
	}
	res, err := s.w.coord.Index(ctx, req)
	if err != nil {
		return model.IndexResult{}, s.fail("index", err)
	}
	return res, nil
}

// Refresh builds an incremental generation over this workspace.
//
// Coordinator.Refresh takes a path hint it deliberately does not act on and
// model.IndexRequest carries no paths, so nothing is dropped by passing none.
// What Refresh cannot honour is --full or --rebuild: both name a different
// operation (Index), and silently ignoring a flag the caller set is the one
// answer this must not give, so it is refused as an argument error.
func (s *Services) Refresh(ctx context.Context, req model.IndexRequest) (model.IndexResult, error) {
	if err := req.Validate(); err != nil {
		return model.IndexResult{}, err
	}
	if req.Full || req.Rebuild {
		return model.IndexResult{}, &model.Error{Code: model.CodeArgumentInvalid,
			Message:     "refresh is incremental; --full and --rebuild build a new generation",
			Remediation: "run index --full or index --rebuild instead"}
	}
	res, err := s.w.coord.Refresh(ctx, nil)
	if err != nil {
		return model.IndexResult{}, s.fail("refresh", err)
	}
	return res, nil
}

// IndexStatus reports the active generation and its capability completeness.
func (s *Services) IndexStatus(ctx context.Context) (model.IndexStatus, error) {
	st, err := s.w.coord.Status(ctx)
	if err != nil {
		return model.IndexStatus{}, s.fail("index status", err)
	}
	return st, nil
}

// --- ExploreService ---------------------------------------------------------

// Overview reports the repository's top-level structure.
//
// It has no producer to route to: the bounded package/module/language map of
// model.OverviewItem needs per-container file, symbol and byte aggregates that
// no landed service computes, and ruling Q8 forbids Task 17 adding a second
// aggregate to get them. The request is still validated, so a malformed one is
// refused as an argument error rather than as a defect, and the answer is an
// explicit typed refusal naming the owner. See deviation D1 in the lane report.
func (s *Services) Overview(ctx context.Context, req model.OverviewRequest) (model.Page[model.OverviewItem], error) {
	if err := req.Validate(); err != nil {
		return model.Page[model.OverviewItem]{}, err
	}
	return model.Page[model.OverviewItem]{}, notProduced("repo_overview", "the repository map aggregate")
}

// Search answers generation-local lexical retrieval and exact lookup.
func (s *Services) Search(ctx context.Context, req model.SearchRequest) (model.Page[model.SearchHit], error) {
	if err := req.Validate(); err != nil {
		return model.Page[model.SearchHit]{}, err
	}
	page, err := s.w.s.search.Search(ctx, req)
	if err != nil {
		return model.Page[model.SearchHit]{}, s.fail("search", err)
	}
	return page, nil
}

// Symbol resolves a symbol through the canonical index, or through the LSP
// overlay when the request names semantic_source=lsp.
//
// The two sources are switched on explicitly rather than defaulted: an empty
// SemanticSource is already invalid on the request, and letting an unknown
// spelling fall through to canonical would answer a live-source question with
// index facts.
func (s *Services) Symbol(ctx context.Context, req model.SymbolRequest) (model.Page[model.Node], error) {
	if err := req.Validate(); err != nil {
		return model.Page[model.Node]{}, err
	}
	switch req.SemanticSource {
	case model.SemanticCanonical:
		page, err := s.w.s.search.Resolve(ctx, req)
		if err != nil {
			return model.Page[model.Node]{}, s.fail("symbol", err)
		}
		return page, nil
	case model.SemanticLSP:
		page, err := s.w.s.overlaySymbols(ctx, req)
		if err != nil {
			return model.Page[model.Node]{}, s.fail("symbol", err)
		}
		return page, nil
	}
	return model.Page[model.Node]{}, unknownSource("symbol", req.SemanticSource)
}

// References answers reference, implements and type-definition evidence, from
// the canonical index or the LSP overlay.
func (s *Services) References(ctx context.Context, req model.ReferenceRequest) (model.Page[model.ReferenceOccurrence], error) {
	if err := req.Validate(); err != nil {
		return model.Page[model.ReferenceOccurrence]{}, err
	}
	switch req.SemanticSource {
	case model.SemanticCanonical:
		var page model.Page[model.ReferenceOccurrence]
		err := s.withEngine(ctx, req.GenerationID, func(e *graph.Engine) error {
			var err error
			page, err = e.References(ctx, req)
			return err
		})
		if err != nil {
			return model.Page[model.ReferenceOccurrence]{}, s.fail("references", err)
		}
		return page, nil
	case model.SemanticLSP:
		page, err := s.w.s.overlayReferences(ctx, req)
		if err != nil {
			return model.Page[model.ReferenceOccurrence]{}, s.fail("references", err)
		}
		return page, nil
	}
	return model.Page[model.ReferenceOccurrence]{}, unknownSource("references", req.SemanticSource)
}

// Graph answers bounded callers/callees traversal.
//
// It routes to Engine.Neighbors, the one traversal that walks the request's own
// direction and relation allowlist: GraphRequest carries no operation selector,
// so `callers` and `callees` are that request with Direction and Relations set,
// and Engine.Callers/Callees would pin them a second time from a name this
// signature does not carry.
func (s *Services) Graph(ctx context.Context, req model.GraphRequest) (model.GraphResult, error) {
	if err := req.Validate(); err != nil {
		return model.GraphResult{}, err
	}
	var res model.GraphResult
	err := s.withEngine(ctx, req.GenerationID, func(e *graph.Engine) error {
		var err error
		res, err = e.Neighbors(ctx, req)
		return err
	})
	if err != nil {
		return model.GraphResult{}, s.fail("graph", err)
	}
	return res, nil
}

// Path answers bounded dependency-path search.
func (s *Services) Path(ctx context.Context, req model.PathRequest) (model.PathResult, error) {
	if err := req.Validate(); err != nil {
		return model.PathResult{}, err
	}
	var res model.PathResult
	err := s.withEngine(ctx, req.GenerationID, func(e *graph.Engine) error {
		var err error
		res, err = e.ShortestPath(ctx, req)
		return err
	})
	if err != nil {
		return model.PathResult{}, s.fail("dependency path", err)
	}
	return res, nil
}

// Impact answers bounded impact analysis.
//
// The engine answers a model.ImpactResult and the frozen facade returns a page,
// so the ranked entries and their metadata are projected onto it. The package
// rollup and the visited/edge accounting the result also carries have no home
// in model.Page; a caller that needs them asks Graph for the rollup. See
// deviation D3 in the lane report.
func (s *Services) Impact(ctx context.Context, req model.ImpactRequest) (model.Page[model.ImpactEntry], error) {
	if err := req.Validate(); err != nil {
		return model.Page[model.ImpactEntry]{}, err
	}
	var res model.ImpactResult
	err := s.withEngine(ctx, req.GenerationID, func(e *graph.Engine) error {
		var err error
		res, err = e.Impact(ctx, req)
		return err
	})
	if err != nil {
		return model.Page[model.ImpactEntry]{}, s.fail("impact", err)
	}
	return model.Page[model.ImpactEntry]{Meta: res.Meta, Items: res.Entries}, nil
}

// --- ContextService ---------------------------------------------------------

// Plan compiles one immutable manifest and opens the actor's session over it,
// returning both.
//
// The two steps are deliberately not one store transaction: the compiler
// publishes the manifest on its own and OpenSession is idempotent per actor and
// idempotency key, so a session that fails to open leaves a reusable manifest
// rather than a half-written one.
func (s *Services) Plan(ctx context.Context, req model.PlanRequest) (model.PlanResult, model.SessionStatus, error) {
	if err := req.Validate(); err != nil {
		return model.PlanResult{}, model.SessionStatus{}, err
	}
	manifest, err := s.w.Compile(ctx, req.Context)
	if err != nil {
		return model.PlanResult{}, model.SessionStatus{}, s.fail("context plan", err)
	}
	status, err := s.w.s.coverage.OpenSession(ctx, req, manifest.ID)
	if err != nil {
		return model.PlanResult{}, model.SessionStatus{}, s.fail("context plan", err)
	}
	return model.PlanResult{Manifest: manifest, SessionID: status.SessionID, ActorID: status.ActorID}, status, nil
}

// SessionStatus reports one page of per-file coverage beside the honest session
// status.
func (s *Services) SessionStatus(ctx context.Context, req model.SessionRequest, page model.PageRequest) (model.Page[model.FileCoverage], model.SessionStatus, error) {
	if err := req.Validate(); err != nil {
		return model.Page[model.FileCoverage]{}, model.SessionStatus{}, err
	}
	if err := page.Validate(); err != nil {
		return model.Page[model.FileCoverage]{}, model.SessionStatus{}, err
	}
	files, status, err := s.w.s.coverage.Status(ctx, req, page)
	if err != nil {
		return model.Page[model.FileCoverage]{}, model.SessionStatus{}, s.fail("context status", err)
	}
	return files, status, nil
}

// Next names the next required file and byte offset to read. Metadata only: it
// never carries source bytes.
func (s *Services) Next(ctx context.Context, req model.SessionRequest) (model.NextContextItem, error) {
	if err := req.Validate(); err != nil {
		return model.NextContextItem{}, err
	}
	item, err := s.w.s.coverage.Next(ctx, req)
	if err != nil {
		return model.NextContextItem{}, s.fail("context next", err)
	}
	return item, nil
}

// Include extends a pinned session's scope under a compare-and-swap.
func (s *Services) Include(ctx context.Context, req model.IncludeRequest) (model.SessionStatus, error) {
	if err := req.Validate(); err != nil {
		return model.SessionStatus{}, err
	}
	wf, err := s.workflow()
	if err != nil {
		return model.SessionStatus{}, err
	}
	status, err := wf.Include(ctx, req)
	if err != nil {
		return model.SessionStatus{}, s.fail("context include", err)
	}
	return status, nil
}

// Read serves one bounded chunk of snapshot-pinned source. It is the only
// method on this facade that returns source bytes.
func (s *Services) Read(ctx context.Context, req model.ReadChunkRequest) (model.ReadChunkResponse, error) {
	if err := req.Validate(); err != nil {
		return model.ReadChunkResponse{}, err
	}
	res, err := s.w.s.coverage.Read(ctx, req)
	if err != nil {
		return model.ReadChunkResponse{}, s.fail("read source", err)
	}
	return res, nil
}

// Acknowledge confirms delivered chunks by echoing their signed receipts, which
// is what turns delivery into coverage.
func (s *Services) Acknowledge(ctx context.Context, req model.AcknowledgeRequest) (model.SessionStatus, error) {
	if err := req.Validate(); err != nil {
		return model.SessionStatus{}, err
	}
	status, err := s.w.s.coverage.Acknowledge(ctx, req)
	if err != nil {
		return model.SessionStatus{}, s.fail("context acknowledge", err)
	}
	return status, nil
}

// Waive records a required-file waiver. A waiver never grants strict readiness.
func (s *Services) Waive(ctx context.Context, req model.WaiverRequest) (model.WaiverRecord, model.SessionStatus, error) {
	if err := req.Validate(); err != nil {
		return model.WaiverRecord{}, model.SessionStatus{}, err
	}
	wf, err := s.workflow()
	if err != nil {
		return model.WaiverRecord{}, model.SessionStatus{}, err
	}
	record, status, err := wf.Waive(ctx, req)
	if err != nil {
		return model.WaiverRecord{}, model.SessionStatus{}, s.fail("context waive", err)
	}
	return record, status, nil
}

// Record persists one immutable observation, scope reviews included.
func (s *Services) Record(ctx context.Context, req model.ObservationRequest) (model.Observation, model.SessionStatus, error) {
	if err := req.Validate(); err != nil {
		return model.Observation{}, model.SessionStatus{}, err
	}
	wf, err := s.workflow()
	if err != nil {
		return model.Observation{}, model.SessionStatus{}, err
	}
	observation, status, err := wf.Record(ctx, req)
	if err != nil {
		return model.Observation{}, model.SessionStatus{}, s.fail("context record", err)
	}
	return observation, status, nil
}

// Advance applies one guarded, version-checked transition.
func (s *Services) Advance(ctx context.Context, req model.AdvanceRequest) (model.WorkflowStatus, model.SessionStatus, error) {
	if err := req.Validate(); err != nil {
		return model.WorkflowStatus{}, model.SessionStatus{}, err
	}
	wf, err := s.workflow()
	if err != nil {
		return model.WorkflowStatus{}, model.SessionStatus{}, err
	}
	workflowStatus, status, err := wf.Advance(ctx, req)
	if err != nil {
		return model.WorkflowStatus{}, model.SessionStatus{}, s.fail("context advance", err)
	}
	return workflowStatus, status, nil
}

// Capsule pages one projection of the sealed capsule.
func (s *Services) Capsule(ctx context.Context, req model.CapsuleRequest) (model.CapsulePage, error) {
	if err := req.Validate(); err != nil {
		return model.CapsulePage{}, err
	}
	wf, err := s.workflow()
	if err != nil {
		return model.CapsulePage{}, err
	}
	page, err := wf.Capsule(ctx, req)
	if err != nil {
		return model.CapsulePage{}, s.fail("context capsule", err)
	}
	return page, nil
}

// Export returns the whole sealed capsule.
func (s *Services) Export(ctx context.Context, req model.SessionRequest) (model.Capsule, error) {
	if err := req.Validate(); err != nil {
		return model.Capsule{}, err
	}
	wf, err := s.workflow()
	if err != nil {
		return model.Capsule{}, err
	}
	capsule, err := wf.Export(ctx, req)
	if err != nil {
		return model.Capsule{}, s.fail("context export", err)
	}
	return capsule, nil
}

// CloseSession closes the session under the caller's expected version.
//
// It spends two calls on purpose. The transition is the workflow service's,
// which owns the Section 17.1 guards and answers a model.WorkflowStatus; the
// frozen facade answers a model.SessionStatus, which only the readiness
// evaluator builds. Routing the close through coverage.Service.Close instead
// would give the right type in one call and a second closing path, which the
// no-drift rule forbids; changing the frozen workflow.Close signature is not a
// fill-in lane's to do, so it is reported (deviation D2) rather than done.
//
// A Status that fails after a successful Close loses the report, not the close:
// the transition is already committed and the caller's next status shows it.
func (s *Services) CloseSession(ctx context.Context, req model.SessionRequest, expectedVersion int) (model.SessionStatus, error) {
	if err := req.Validate(); err != nil {
		return model.SessionStatus{}, err
	}
	if expectedVersion < 1 {
		return model.SessionStatus{}, &model.Error{Code: model.CodeArgumentInvalid,
			Message:     "context close expected_version is required; versions start at 1",
			Remediation: "pass the state_version the last status reported"}
	}
	wf, err := s.workflow()
	if err != nil {
		return model.SessionStatus{}, err
	}
	if _, err := wf.Close(ctx, req, expectedVersion); err != nil {
		return model.SessionStatus{}, s.fail("context close", err)
	}
	status, err := wf.Status(ctx, req)
	if err != nil {
		return model.SessionStatus{}, s.fail("context close", err)
	}
	return status, nil
}

// --- DiagnoseService --------------------------------------------------------

// Doctor reports this installation's health.
//
// Like Overview it has no producer to route to: Section 22's checks live in
// internal/diagnostics, which Task 22 builds, and assembling a DoctorReport
// here would be that package written twice. The request is validated and the
// answer is an explicit typed refusal naming the owner. See deviation D1.
func (s *Services) Doctor(ctx context.Context, req model.DoctorRequest) (model.DoctorReport, error) {
	if err := req.Validate(); err != nil {
		return model.DoctorReport{}, err
	}
	return model.DoctorReport{}, notProduced("doctor", "the diagnostics reporter")
}

// --- shared routing helpers -------------------------------------------------

// workflow returns the composed workflow service. The field is wired by the
// integration lane; until it is, every method that needs it answers a typed
// defect rather than dereferencing nil, because a panic in a product adapter is
// strictly worse than a refusal that names the wiring gap.
func (s *Services) workflow() (*workflow.Service, error) {
	if s.w.s.workflow == nil {
		return nil, &model.Error{Code: model.CodeInternal,
			Message: "app: this workspace composed no workflow service"}
	}
	return s.w.s.workflow, nil
}

// withEngine builds the graph engine for the pinned generation, runs one query
// against it and releases the reader's lease on every path. A release failure
// is reported only when the query itself succeeded: the query's own failure is
// the one the caller needs.
func (s *Services) withEngine(ctx context.Context, gen model.GenerationID,
	query func(*graph.Engine) error) (err error) {
	engine, release, err := s.w.Query(ctx, gen)
	if err != nil {
		return err
	}
	defer func() {
		if cerr := release(); cerr != nil && err == nil {
			err = cerr
		}
	}()
	return query(engine)
}

// fail is the facade's error boundary. A typed error is the answer the whole
// tree already agreed on, so it passes through untouched -- rewrapping it would
// break errors.As and the Section 18.2 exit-code mapping. Cancellation and an
// expired deadline are typed here because a bare context error reaching the CLI
// is read as an invalid argument. Anything else untyped is a defect whose text
// may carry a path or a SQL fragment, so it is logged and answered with a
// bounded CTX_INTERNAL that carries neither.
func (s *Services) fail(op string, err error) error {
	if err == nil {
		return nil
	}
	var typed *model.Error
	if errors.As(err, &typed) {
		return err
	}
	switch {
	case errors.Is(err, context.Canceled):
		return model.Canceled(err)
	case errors.Is(err, context.DeadlineExceeded):
		return &model.Error{Code: model.CodeQueryDeadline,
			Message: "the " + op + " request did not finish within its deadline"}
	}
	s.w.s.logger.Error("an app service failed with an untyped error",
		"component", "app", "operation", op, "error", err.Error())
	return &model.Error{Code: model.CodeInternal,
		Message: "app: " + op + " failed unexpectedly"}
}

// notProduced is the answer of a facade method whose producer is another task's
// to build. It is a CTX_INTERNAL because Section 22 adds no code for it and the
// caller cannot correct it, and it names the missing producer so an operator
// reading the message learns what is absent rather than that something broke.
func notProduced(op, producer string) error {
	return &model.Error{Code: model.CodeInternal,
		Message:     "app: " + op + " has no producer in this build",
		Remediation: "this operation waits on " + producer}
}

// unknownSource refuses a semantic source this facade cannot route. The request
// types already reject an unknown spelling, so this is the guard that keeps a
// widened enum from silently answering a live-source question with index facts.
func unknownSource(op string, source model.SemanticSource) error {
	return (&model.Error{Code: model.CodeArgumentInvalid,
		Message: "app: " + op + " cannot answer this semantic source"}).
		WithDetail("semantic_source", string(source))
}

// --- ContextService: manifest projections ----------------------------------

// entriesEndpoint pins the continuation this method issues. A cursor carries
// the endpoint that minted it, so a status or search token cannot be replayed
// here and silently repin the page.
const entriesEndpoint = "context_entries"

// entriesQueryHashDomain versions the entries cursor preimage. Changing what
// the preimage folds without changing this label would let an old token resume
// a projection it no longer describes.
const entriesQueryHashDomain = "codectx.app.context_entries.v1"

// Entries pages one projection of the session's current manifest. Metadata
// only: an entry names a node, a file and a budget, never source bytes.
//
// The manifest pages are read straight from the store because no service owns
// this projection -- the compiler writes the manifest and the coverage service
// reads only its required files -- and its continuation is the same signed
// pagination.Cursor every other paged endpoint in the tree issues, under this
// endpoint's own name and query hash. A session that has expired still reads:
// the projection describes a manifest, which is immutable, and refusing it
// would hide the scope an operator is asking about.
func (s *Services) Entries(ctx context.Context, req model.ContextPageRequest) (model.ContextPage, error) {
	if err := req.Validate(); err != nil {
		return model.ContextPage{}, err
	}
	st := s.w.s
	rec, err := st.store.Session(ctx, req.SessionID, req.ActorID)
	if err != nil && !(rec.ID != "" && expiredSession(err)) {
		return model.ContextPage{}, s.fail("context entries", err)
	}
	after, err := s.resumeEntries(req, rec)
	if err != nil {
		return model.ContextPage{}, s.fail("context entries", err)
	}
	// One row past the page is read so "there is more" is a fact rather than a
	// guess from a full page, and it is dropped before the page is built.
	limit := entriesLimit(st.cfg.Resources.MaxPageItems, req.Page.Limit)
	page := model.ContextPage{
		Meta:       model.QueryMeta{Binding: rec.Binding},
		SessionID:  rec.ID,
		ManifestID: rec.ManifestID,
		View:       req.View,
	}
	var last int
	var more bool
	switch req.View {
	case model.ViewEntries:
		rows, err := st.store.ManifestEntries(ctx, rec.ManifestID, after, limit+1)
		if err != nil {
			return model.ContextPage{}, s.fail("context entries", err)
		}
		if more = len(rows) > limit; more {
			rows = rows[:limit]
		}
		if len(rows) > 0 {
			last = rows[len(rows)-1].Ordinal
		}
		page.Entries = rows
	case model.ViewSlices:
		rows, err := st.store.ManifestSlices(ctx, rec.ManifestID, after, limit+1)
		if err != nil {
			return model.ContextPage{}, s.fail("context entries", err)
		}
		if more = len(rows) > limit; more {
			rows = rows[:limit]
		}
		if len(rows) > 0 {
			last = rows[len(rows)-1].Index
		}
		page.Slices = rows
	case model.ViewExcluded:
		rows, err := st.store.ManifestExcluded(ctx, rec.ManifestID, after, limit+1)
		if err != nil {
			return model.ContextPage{}, s.fail("context entries", err)
		}
		if more = len(rows) > limit; more {
			rows = rows[:limit]
		}
		if len(rows) > 0 {
			last = rows[len(rows)-1].Ordinal
		}
		page.Excluded = rows
	default:
		// ContextPageRequest.Validate already refuses an unknown view; this is
		// the guard that keeps a widened enum from paging an empty projection.
		return model.ContextPage{}, (&model.Error{Code: model.CodeArgumentInvalid,
			Message: "app: context entries cannot page this view"}).
			WithDetail("view", string(req.View))
	}
	if more {
		token, why, err := s.entriesCursor(ctx, rec, req.View, last)
		if err != nil {
			return model.ContextPage{}, s.fail("context entries", err)
		}
		if token != "" {
			page.Meta.NextCursor = token
		} else {
			page.Meta.Truncated, page.Meta.TruncationReason = true, why
		}
	}
	return page, nil
}

// entriesLimit resolves the page size: the caller's, bounded by the configured
// page ceiling, and kept one below the protocol maximum so the one extra row
// this reads to detect a continuation can never overflow the page bound.
func entriesLimit(configured, requested int) int {
	limit := requested
	if limit <= 0 || (configured > 0 && limit > configured) {
		limit = configured
	}
	if limit <= 0 || limit >= model.MaxPageItems {
		limit = model.MaxPageItems - 1
	}
	return limit
}

// entriesQueryHash folds everything that makes two entry pages different:
// the session, its actor, the manifest the projection reads and the view. A
// cursor whose hash does not match was issued for another page and is refused
// rather than honoured against this one.
func entriesQueryHash(rec sqlite.SessionRecord, view model.ContextView) string {
	return model.H(entriesQueryHashDomain, entriesEndpoint, string(rec.ID), rec.ActorID,
		string(rec.ManifestID), string(view))
}

// resumeEntries turns a presented cursor into the ordinal to resume after. A
// cursor is a claim about which page this is, so one naming another generation,
// another projection or a spool this endpoint never writes is refused; honoring
// it would silently re-serve or skip manifest records.
func (s *Services) resumeEntries(req model.ContextPageRequest, rec sqlite.SessionRecord) (int, error) {
	if req.Page.Cursor == "" {
		return 0, nil
	}
	c, err := s.w.s.signer.DecodeCursor(req.Page.Cursor, entriesEndpoint, time.Now())
	if err != nil {
		return 0, err
	}
	if c.GenerationID != rec.Binding.GenerationID || c.AnalysisKey != rec.Binding.AnalysisKey {
		return 0, cursorInvalid("cursor was issued against a different generation")
	}
	if c.QueryHash != entriesQueryHash(rec, req.View) {
		return 0, cursorInvalid("cursor was issued for a different session, actor or view")
	}
	if c.SpoolID != "" {
		return 0, cursorInvalid("cursor carries spooled traversal state this endpoint does not produce")
	}
	after, err := strconv.Atoi(c.LastKey)
	if err != nil || after < 0 {
		return 0, cursorInvalid("cursor sort key is not a manifest ordinal")
	}
	return after, nil
}

// entriesCursor signs the continuation for the next page, or reports why there
// cannot be one. Like every other continuation in the tree it carries a fresh
// cursor-owned retention lease and expires with the session, because a page
// resumed after the session is gone would describe scope nobody holds.
func (s *Services) entriesCursor(ctx context.Context, rec sqlite.SessionRecord,
	view model.ContextView, last int) (token, why string, err error) {
	st := s.w.s
	if st.leases == nil {
		return "", "this workspace does not retain context continuations", nil
	}
	if !model.ValidHexID(string(rec.Binding.AnalysisKey)) {
		// The generation is still staging, so there is no analysis key to pin
		// the continuation to and a cursor cannot be well formed.
		return "", "this session's generation is not yet analysed, so its manifest cannot be continued", nil
	}
	lease, err := st.leases.Acquire(ctx, rec.Binding.GenerationID, rec.Binding.SnapshotID, model.LeaseCursor)
	if err != nil {
		return "", "", err
	}
	token, err = st.signer.EncodeCursor(pagination.Cursor{
		Endpoint:     entriesEndpoint,
		GenerationID: rec.Binding.GenerationID,
		AnalysisKey:  rec.Binding.AnalysisKey,
		QueryHash:    entriesQueryHash(rec, view),
		LastKey:      strconv.Itoa(last),
		LeaseID:      lease.ID,
		ExpiresAt:    rec.ExpiresAt,
	})
	if err != nil {
		// The request's own context may already be done, so the release runs on
		// a fresh one: a leaked lease pins a generation against retention for a
		// page nobody can ask for.
		if rerr := st.leases.Release(context.WithoutCancel(ctx), lease.ID); rerr != nil {
			st.logger.Warn("a context continuation lease could not be released",
				"component", "app", "error", rerr.Error())
		}
		return "", "", err
	}
	return token, "", nil
}

// cursorInvalid is the one spelling of a refused continuation in this package.
func cursorInvalid(message string) error {
	return &model.Error{Code: model.CodeCursorInvalid, Message: message,
		Remediation: "drop the cursor and ask for the first page again"}
}

// expiredSession reports whether err is the store's lazy expiry, which every
// read path treats as a partial answer rather than as a missing record.
func expiredSession(err error) bool {
	var typed *model.Error
	return errors.As(err, &typed) && typed.Code == model.CodeSessionExpired
}
