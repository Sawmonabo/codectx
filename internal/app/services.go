package app

import (
	"context"
	"errors"
	"strconv"
	"time"

	"github.com/Sawmonabo/codectx/internal/diagnostics"
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
//
// Concurrency (Task 19, Q2): a *Services is safe for concurrent use by multiple
// goroutines over one workspace, which is what lets an MCP server answer
// several tool calls against a single open workspace. It holds one pointer and
// no mutable state of its own, and every service it routes to is either
// immutable after composition (search, context compiler, workflow -- all of
// whose fields are read-only after New) or explicitly synchronised: the stack's
// memoised views and generation bindings are guarded by viewsMu, the graph
// engine is built per request behind the process-scoped concurrency gate, and
// the store is a *sql.DB, which is itself concurrency-safe. There is no lazy
// initialisation anywhere behind this facade -- every service is composed
// eagerly in open() -- so no request can be the one that builds a field another
// request is reading.
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
	IndexStatus(ctx context.Context, req model.StatusRequest) (model.IndexStatus, error)
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
	Impact(ctx context.Context, req model.ImpactRequest) (model.ImpactResult, error)
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

// IndexStatus reports the active generation and its capability completeness,
// and -- only when the request asks for it -- the Section 23 resource block.
//
// The block is a request field rather than a second call (ruling Q1) so one
// answer describes one moment: a caller that asked status and then resources
// would be reading two instants and reporting them as one. It stays off by
// default because sampling the host walks the content-addressed store and the
// temporary directories, and an ordinary status must stay cheap.
//
// A failure to sample is the request's failure, not a quieter status: the
// caller asked for resources, so answering without them would report a
// measurement as absent when it was refused. Absence inside the block still
// means "not measurable on this host", which is the sampler's own contract.
func (s *Services) IndexStatus(ctx context.Context, req model.StatusRequest) (model.IndexStatus, error) {
	if err := req.Validate(); err != nil {
		return model.IndexStatus{}, err
	}
	st, err := s.w.coord.Status(ctx)
	if err != nil {
		return model.IndexStatus{}, s.fail("index status", err)
	}
	if !req.Resources {
		return st, nil
	}
	svc, err := s.diagnostics()
	if err != nil {
		return model.IndexStatus{}, err
	}
	report, err := svc.Resources(ctx)
	if err != nil {
		return model.IndexStatus{}, s.fail("index status", err)
	}
	st.Resources = &report
	return st, nil
}

// --- ExploreService ---------------------------------------------------------

// Overview reports the repository's top-level structure: one bounded,
// generation-pinned page of container nodes, each carrying what it directly
// holds.
//
// It routes through withEngine like every other traversal endpoint, because the
// map is read from the same pinned generation and under the same lease
// discipline: the engine's container enumeration is only meaningful inside the
// binding withEngine pins, and releasing that lease on every path is what keeps
// a repository map from retaining a generation after the page is printed.
func (s *Services) Overview(ctx context.Context, req model.OverviewRequest) (model.Page[model.OverviewItem], error) {
	if err := req.Validate(); err != nil {
		return model.Page[model.OverviewItem]{}, err
	}
	var page model.Page[model.OverviewItem]
	err := s.withEngine(ctx, req.GenerationID, func(e *graph.Engine) error {
		var err error
		page, err = e.Overview(ctx, req)
		return err
	})
	if err != nil {
		return model.Page[model.OverviewItem]{}, s.fail("repo overview", err)
	}
	return page, nil
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
// It routes to Engine.Neighbors, the one traversal the engine exposes: it walks
// the request's own direction and relation allowlist, and GraphRequest carries
// no operation selector, so `callers` and `callees` are that request with
// Direction and Relations set rather than two further engine entry points.
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
// It returns the engine's whole model.ImpactResult rather than a page of its
// entries. L0 froze this method as model.Page[model.ImpactEntry]; INT changed
// it (wave-e ruling "Rulings on L7 FACADE deviations", D3) because the result
// also carries the per-package rollup and the visited/edge accounting that
// `codectx impact` already prints, and model.Page has no home for either. A
// facade that silently dropped them would make the facade path a downgrade from
// the command it is meant to replace. This is the only Task 17 change to a
// frozen facade signature.
func (s *Services) Impact(ctx context.Context, req model.ImpactRequest) (model.ImpactResult, error) {
	if err := req.Validate(); err != nil {
		return model.ImpactResult{}, err
	}
	var res model.ImpactResult
	err := s.withEngine(ctx, req.GenerationID, func(e *graph.Engine) error {
		var err error
		res, err = e.Impact(ctx, req)
		return err
	})
	if err != nil {
		return model.ImpactResult{}, s.fail("impact", err)
	}
	return res, nil
}

// --- ContextService ---------------------------------------------------------

// Plan compiles one immutable manifest and opens the actor's session over it,
// returning both.
//
// The two steps are deliberately not one store transaction: the compiler
// publishes the manifest on its own and OpenSession is idempotent per actor and
// idempotency key, so a session that fails to open leaves a reusable manifest
// rather than a half-written one.
//
// The compile is CONTINUABLE (rulings C7 and C9): when it runs out of query
// deadline it ends the pass it is in and answers a truncated PlanResult
// carrying the token the caller presents back as PlanRequest.Cursor. No session
// is opened on that path -- see the truncation branch below.
func (s *Services) Plan(ctx context.Context, req model.PlanRequest) (model.PlanResult, model.SessionStatus, error) {
	if err := req.Validate(); err != nil {
		return model.PlanResult{}, model.SessionStatus{}, err
	}
	res, err := s.w.CompilePage(ctx, req.Context, req.Cursor)
	if err != nil {
		return model.PlanResult{}, model.SessionStatus{}, s.fail("context plan", err)
	}
	// Ruling C9: a compile that ended at a pass boundary is returned BEFORE any
	// session is opened. There is no manifest for a session to bind to -- a
	// partial plan is never persisted -- so opening one here would leave a
	// session row pointing at a manifest that does not exist, and the actor
	// would have to close it before continuing. The status is the zero one on
	// purpose: there is no session to describe yet.
	if res.Truncated {
		return model.PlanResult{
			ActorID:          req.ActorID,
			Truncated:        true,
			TruncationReason: res.TruncationReason,
			NextCursor:       res.NextCursor,
		}, model.SessionStatus{}, nil
	}
	manifest := res.Manifest
	id, err := s.w.s.coverage.OpenSession(ctx, req, manifest.ID)
	if err != nil {
		return model.PlanResult{}, model.SessionStatus{}, s.fail("context plan", err)
	}
	// The gate-aware status is the workflow evaluator's and nobody else's
	// (ruling VF1), so the freshly opened session is described by the same
	// readiness answer every later call reports. The extra read is a session
	// read, not a second aggregate: Section 16.3 is still decided by one
	// CoverageSummary call inside that evaluator.
	status, err := s.sessionStatus(ctx, model.SessionRequest{SessionID: id, ActorID: req.ActorID})
	if err != nil {
		return model.PlanResult{}, model.SessionStatus{}, s.fail("context plan", err)
	}
	return model.PlanResult{Manifest: manifest, SessionID: id, ActorID: req.ActorID}, status, nil
}

// sessionStatus is the facade's one route to a model.SessionStatus: the
// workflow readiness evaluator. Every endpoint that answers a status composes
// this rather than building one, so ready_for_implementation,
// strict_gate_satisfied and the guarantee limit mean the same thing on all of
// them (ruling VF1).
func (s *Services) sessionStatus(ctx context.Context, req model.SessionRequest) (model.SessionStatus, error) {
	wf, err := s.workflow()
	if err != nil {
		return model.SessionStatus{}, err
	}
	return wf.Status(ctx, req)
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
	files, err := s.w.s.coverage.Status(ctx, req, page)
	if err != nil {
		return model.Page[model.FileCoverage]{}, model.SessionStatus{}, s.fail("context status", err)
	}
	status, err := s.sessionStatus(ctx, req)
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
	if err := s.w.s.coverage.Acknowledge(ctx, req); err != nil {
		return model.SessionStatus{}, s.fail("context acknowledge", err)
	}
	// Read after the confirmation, so the status reports the coverage this call
	// just granted rather than the state before it.
	status, err := s.sessionStatus(ctx, model.SessionRequest{SessionID: req.SessionID, ActorID: req.ActorID})
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
// evaluator builds (deviation D2, accepted). There is no second closing path to
// take instead: ruling VF1 removed coverage.Service.Close along with the
// duplicate status aggregate that was its only reason to exist.
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

// Doctor reports this installation's health: the Section 22 check list, run
// against this workspace.
//
// The report is returned as internal/diagnostics produced it. A failing check
// is not an error -- a doctor that refused to answer because something it
// checks is broken would be useless precisely when it is needed -- so only a
// failure to PRODUCE the list at all reaches the caller as an error.
func (s *Services) Doctor(ctx context.Context, req model.DoctorRequest) (model.DoctorReport, error) {
	if err := req.Validate(); err != nil {
		return model.DoctorReport{}, err
	}
	svc, err := s.diagnostics()
	if err != nil {
		return model.DoctorReport{}, err
	}
	report, err := svc.Doctor(ctx, req)
	if err != nil {
		return model.DoctorReport{}, s.fail("doctor", err)
	}
	return report, nil
}

// --- shared routing helpers -------------------------------------------------

// diagnostics returns the composed Section 22/23 reporter, for the same reason
// and with the same shape as workflow below: open() builds it eagerly, so the
// nil branch is unreachable through a workspace this package opened, and a
// refusal that names the wiring gap beats a panic in a product adapter.
func (s *Services) diagnostics() (*diagnostics.Service, error) {
	if s.w.s.diagnose == nil {
		return nil, &model.Error{Code: model.CodeInternal,
			Message: "app: this workspace composed no diagnostics service"}
	}
	return s.w.s.diagnose, nil
}

// workflow returns the composed workflow service. open() builds it eagerly and
// fails the composition if it cannot, so the nil branch is unreachable through
// a workspace this package opened; it is kept because *Services is reachable
// from any *Workspace value a test or a future composition constructs, and a
// panic in a product adapter is strictly worse than a refusal that names the
// wiring gap.
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
	// The manifest is read for the capability states it was compiled against:
	// a projection that omitted them would tell an operator less about the
	// generation behind this scope than `context status` already does.
	manifest, err := st.store.Manifest(ctx, rec.ManifestID)
	if err != nil {
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
		Meta:       model.QueryMeta{Binding: rec.Binding, Completeness: manifest.Completeness},
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
	// The page's own invariants -- a bounded QueryMeta and records for exactly
	// the view it declares -- are checked before it leaves the facade, so a
	// widened view or a mis-assembled projection fails here rather than
	// reaching a caller as a well-formed lie.
	if err := page.Validate(); err != nil {
		return model.ContextPage{}, s.fail("context entries", err)
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
//
// The no-cursor answer is -1, not 0. Manifest ordinals and slice indexes run
// 0..n-1 and every store page is `WHERE ordinal > ?`, so a 0 sentinel would
// silently drop the first record of every first page. An integer keyset has no
// free out-of-band value the way coverage's file-id keyset has the empty string.
func (s *Services) resumeEntries(req model.ContextPageRequest, rec sqlite.SessionRecord) (int, error) {
	if req.Page.Cursor == "" {
		return -1, nil
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
	// A cursor's key is the last ordinal served, so 0 is legitimate here even
	// though it is not a legitimate sentinel above.
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
