package app

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/Sawmonabo/codectx/internal/coverage"
	"github.com/Sawmonabo/codectx/internal/diagnostics"
	"github.com/Sawmonabo/codectx/internal/graph"
	"github.com/Sawmonabo/codectx/internal/model"
	"github.com/Sawmonabo/codectx/internal/pagination"
	"github.com/Sawmonabo/codectx/internal/search"
	"github.com/Sawmonabo/codectx/internal/storage/sqlite"
	"github.com/Sawmonabo/codectx/internal/workflow"
)

// Services is the frozen application facade of Section 19.2: the one surface
// the CLI and the MCP server are built against, so both product adapters drive
// the same operations rather than two drifting copies.
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
// Concurrency: a *Services is safe for concurrent use by multiple
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
	// read routes the question-answering half of this facade through the
	// composition's reader handle. See Workspace.ReadServices.
	read bool
}

// Services is the one accessor the CLI and MCP adapters call. It is cheap: the services
// it routes to were composed when the workspace opened.
func (w *Workspace) Services() *Services { return &Services{w: w} }

// ReadServices is the same facade with its ExploreService half bound to the
// composition's READER handle. It exists for the one process that indexes and
// answers questions at the same time: the MCP server, whose codectx_refresh_index
// holds an ingestion group open while an agent calls codectx_search beside it.
// Through Services that search pinned a generation on the writing handle, and a
// pin writes a retention lease -- so the tool call force-committed the refresh's
// group and then queued behind the writer. Through ReadServices it reaches no
// write transaction at all.
//
// In every composition but the server's the reader handle IS the writing one,
// so this returns the same behaviour as Services: nothing else needs to know
// which process it is in.
//
// Only the read half is affected. Index, Refresh and the whole ContextService
// record rows and stay on the writer, which is what they are for.
func (w *Workspace) ReadServices() *Services { return &Services{w: w, read: true} }

// searchService is the search half of whichever handle this facade is bound to.
func (s *Services) searchService() *search.Service {
	if s.read {
		return s.w.s.querySearch
	}
	return s.w.s.search
}

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
	NodeMetadata(ctx context.Context, req model.NodeMetadataRequest) (model.Page[model.MetadataRow], error)
	Graph(ctx context.Context, req model.GraphRequest) (model.GraphResult, error)
	Path(ctx context.Context, req model.PathRequest) (model.PathResult, error)
	Impact(ctx context.Context, req model.ImpactRequest) (model.ImpactResult, error)
	// Read serves a read outside a session (no session_id): one node or one
	// byte range of a pinned generation, earning no coverage. A session read
	// goes through ContextService.Read, which writes the issued chunk.
	Read(ctx context.Context, req model.ReadChunkRequest) (model.ReadChunkResponse, error)
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
	CapsuleRows(ctx context.Context, req model.SessionRequest, list model.CapsuleList,
		after string, limit int) ([]model.CapsuleRow, string, error)
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
// The block is a request field rather than a second call so one
// answer describes one moment: a caller that asked status and then resources
// would be reading two instants and reporting them as one. It stays off by
// default because sampling the host walks the content-addressed store and the
// temporary directories, and an ordinary status must stay cheap.
//
// A failure to sample is the request's failure, not a quieter status: the
// caller asked for resources, so answering without them would report a
// measurement as absent when it was refused. Absence inside the block still
// means "not measurable on this host", which is the sampler's own contract.
//
// It answers through the composition's query handle, which is a SECOND,
// read-only handle only where the composition has one -- the serving process,
// which both answers and builds. In every other composition that handle is the
// composition's own store, so what this guarantees is not a separate
// connection but that the MCP status tool, registered against the writing
// facade beside refresh, does not pin on the writer and commit that refresh's
// ingestion group early.
func (s *Services) IndexStatus(ctx context.Context, req model.StatusRequest) (model.IndexStatus, error) {
	if err := req.Validate(); err != nil {
		return model.IndexStatus{}, err
	}
	st, err := s.w.status.Status(ctx)
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
	page, err := s.searchService().Search(ctx, req)
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
		page, err := s.searchService().Resolve(ctx, req)
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
// entries, because the result also carries the per-package rollup and the
// visited/edge accounting that `codectx impact` prints, and model.Page has no
// home for either. A facade that silently dropped them would answer less than
// the command it serves.
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
// The compile is CONTINUABLE: when it runs out of query
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
	// A compile that ended at a pass boundary is returned BEFORE any
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
	// The gate-aware status is the workflow evaluator's and nobody else's,
	// so the freshly opened session is described by the same
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
// them.
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
// method on this facade that returns source bytes, and it has two modes.
//
// A session read goes to the coverage service, which checks the actor and the
// session's scope, persists the issued chunk and signs its receipt.
//
// A read outside a session is served here and never reaches the coverage
// service: it names a node or a byte range of one generation, writes no
// issued chunk, signs no receipt and changes no coverage. See readOutside.
func (s *Services) Read(ctx context.Context, req model.ReadChunkRequest) (model.ReadChunkResponse, error) {
	if err := req.Validate(); err != nil {
		return model.ReadChunkResponse{}, err
	}
	if !req.InSession() {
		res, err := s.readOutside(ctx, req)
		if err != nil {
			return model.ReadChunkResponse{}, s.fail("read source", err)
		}
		return res, nil
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

// CapsuleRows reads one keyset page of one sealed capsule list and the cursor
// that continues it.
//
// Export answers a capsule's identity and per-list counts; its records are
// rows. A caller that must render the capsule whole -- `codectx context export`
// is the only one -- walks each list through this call, so the records held in
// this process are one page's worth however large the session was.
func (s *Services) CapsuleRows(ctx context.Context, req model.SessionRequest, list model.CapsuleList,
	after string, limit int) ([]model.CapsuleRow, string, error) {
	if err := req.Validate(); err != nil {
		return nil, "", err
	}
	wf, err := s.workflow()
	if err != nil {
		return nil, "", err
	}
	rows, next, err := wf.CapsuleRows(ctx, req, list, after, limit)
	if err != nil {
		return nil, "", s.fail("context export", err)
	}
	return rows, next, nil
}

// CloseSession closes the session under the caller's expected version.
//
// It spends two calls on purpose. The transition is the workflow service's,
// which owns the Section 17.1 guards and answers a model.WorkflowStatus; the
// frozen facade answers a model.SessionStatus, which only the readiness
// evaluator builds. There is no second closing path to take instead: the
// coverage service has no Close of its own and no status aggregate beside the
// evaluator's.
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
// against it and releases the reader's lease on every path.
//
// A release failure NEVER becomes the answer. The page is already computed and
// correct by the time the lease is dropped, so promoting the release error
// turned a good answer -- entries, cursor and all -- into a failed query and
// left the caller nothing to page from; and when the query had failed too, the
// release error was dropped silently. It is reported on the operator channel
// instead, the same way a continuation lease that cannot be released is.
func (s *Services) withEngine(ctx context.Context, gen model.GenerationID,
	query func(*graph.Engine) error) error {
	engine, release, err := s.w.query(ctx, gen, s.read)
	if err != nil {
		return err
	}
	defer func() {
		if cerr := release(); cerr != nil {
			s.w.s.logger.Warn("a pinned query lease could not be released",
				"component", "app", "error", cerr.Error())
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
	if req.View == model.ViewRoutes {
		return s.routesPage(ctx, req, rec, manifest)
	}
	if req.View == model.ViewReasons {
		return s.reasonsPage(ctx, req, rec, manifest)
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
		token, why, err := s.entriesCursor(ctx, rec, req.View, strconv.Itoa(last))
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
	view model.ContextView, last string) (token, why string, err error) {
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
		LastKey:      last,
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

// --- ContextService: reading outside a session -----------------------------

// readSourceEndpoint pins the continuation a read outside a session issues, so
// a search, graph or manifest token cannot be replayed here.
const readSourceEndpoint = "read_source"

// readSourceQueryHashDomain versions the read cursor's query-hash preimage.
// Changing what the preimage folds without changing this label would let an
// old token resume a span it no longer describes.
const readSourceQueryHashDomain = "codectx.app.read_source.v1"

// spanKind is what a read outside a session names: one node, or one file.
type spanKind string

const (
	spanNode spanKind = "node"
	spanFile spanKind = "file"
)

// spanKey is the selector of a read outside a session and the byte its chunk
// starts at. A first call builds it from the request; a continuation reads it
// from the signed cursor, which is why the request names no selector beside a
// cursor. at is unset on a first node read, which starts at the node.
type spanKey struct {
	kind spanKind
	id   string
	at   uint64
	set  bool
	// label is how a refusal names what was asked for: the path as the caller
	// spelled it, or the id.
	label string
}

// queryHash folds the selector, which is what makes two spans different. The
// position travels in the cursor's sort key, beside it.
func (k spanKey) queryHash() string {
	return model.H(readSourceQueryHashDomain, readSourceEndpoint, string(k.kind), k.id)
}

// lastKey renders the continuation's sort key: the selector and the next
// chunk's first byte.
func (k spanKey) lastKey(next uint64) string {
	return string(k.kind) + ":" + k.id + ":" + strconv.FormatUint(next, 10)
}

// parseSpanKey reads a continuation's sort key back. The token is signed, so a
// malformed key is a cursor this endpoint never minted.
func parseSpanKey(lastKey string) (spanKey, error) {
	kind, rest, ok := strings.Cut(lastKey, ":")
	id, at, ok2 := strings.Cut(rest, ":")
	if !ok || !ok2 || (spanKind(kind) != spanNode && spanKind(kind) != spanFile) || !model.ValidHexID(id) {
		return spanKey{}, cursorInvalid("cursor does not name a node or a file to read")
	}
	offset, err := strconv.ParseUint(at, 10, 64)
	if err != nil {
		return spanKey{}, cursorInvalid("cursor does not name a byte to continue from")
	}
	return spanKey{kind: spanKind(kind), id: id, at: offset, set: true, label: id}, nil
}

// requestSpanKey is the selector a first call names. A path is only hashed
// into the file id the index derives from the same root-relative path; nothing
// is opened on it, so a path the snapshot does not hold -- one the capture
// skipped as a symbolic link, or one that was never there -- is simply a file
// id the manifest has no row for. The path is first put in the one spelling
// the capture stores (repositoryPath), so `./a.go` and `pkg/../a.go` name
// a.go; one that climbs out of the tree is refused rather than hashed.
func requestSpanKey(repo model.RepositoryID, req model.ReadChunkRequest) (spanKey, error) {
	switch {
	case req.NodeID != "":
		return spanKey{kind: spanNode, id: string(req.NodeID), label: string(req.NodeID)}, nil
	case req.FileID != "":
		return spanKey{kind: spanFile, id: string(req.FileID), at: req.Offset, set: true, label: string(req.FileID)}, nil
	}
	p, err := repositoryPath(req.Path)
	if err != nil {
		return spanKey{}, err
	}
	return spanKey{kind: spanFile, id: string(model.NewFileID(repo, p)), at: req.Offset, set: true,
		label: strconv.Quote(p)}, nil
}

// repositoryPath is p in the spelling the capture stores a file under:
// slash-separated (filepath.ToSlash, so only the platform's own separator is
// rewritten and a backslash in a Unix file name stays a name byte), lexically
// clean, and relative to the repository root. A path that is absolute, empty,
// the root itself or climbs above it names no file of the repository and is
// CTX_ARGUMENT_INVALID.
func repositoryPath(p string) (string, error) {
	clean, ok := model.CleanRepoPath(filepath.ToSlash(p))
	if !ok || filepath.IsAbs(p) {
		return "", &model.Error{Code: model.CodeArgumentInvalid,
			Message:     "read_chunk.path " + strconv.Quote(p) + " is not a file path inside the repository",
			Remediation: "name the file relative to the repository root, with no \"..\" segment that leaves it"}
	}
	return clean, nil
}

// sourceSpan is one resolved read: the file it reads, the pinned manifest row's
// content hash and size, and the bytes [at, end) still to serve.
type sourceSpan struct {
	file    model.FileID
	node    model.NodeID
	hash    string
	at, end uint64
}

// readOutside serves one chunk of a node or a byte range of one generation,
// outside any session.
//
// The generation is the one the request names, the active one when it names
// none, or the one a continuation's token pins -- never the active one for a
// continuation, so a later page cannot describe another generation than the
// first. It is pinned by a query lease for the call, exactly as every query
// is, and the bytes come from the snapshot view's verified content store, never
// from the working tree.
//
// The chunk is cut by the same rules a session read uses: source.PlanChunk for
// every boundary and the base64 fallback, the same size arithmetic against
// coverage.chunk_bytes, coverage.max_chunk_bytes and
// resources.max_source_response_bytes. The span's end is handed to the planner
// as its end of input, so a node's last chunk ends at the node's last byte and
// is not trimmed back to a line break, and a span longer than one chunk
// continues by a signed cursor that holds a cursor lease, so the generation
// cannot be collected between pages.
//
// It reads through the facade's own handle: the reader handle on a reader
// facade, which records no lease and mints a continuation bound to its expiry
// alone, the way every other answer on that handle does.
func (s *Services) readOutside(ctx context.Context, req model.ReadChunkRequest) (_ model.ReadChunkResponse, err error) {
	st := s.w.s
	ctx, cancel := model.QueryDeadline(ctx, st.cfg.Resources.QueryTimeout.Std())
	defer cancel()
	defer func() { err = model.ClassifyQueryDeadline(ctx, "read source", err) }()

	now := time.Now()
	gen := req.GenerationID
	var key spanKey
	var resumed *pagination.Cursor
	if req.Cursor == "" {
		if key, err = requestSpanKey(st.repo, req); err != nil {
			return model.ReadChunkResponse{}, err
		}
	} else {
		c, err := st.signer.DecodeCursor(req.Cursor, readSourceEndpoint, now)
		if err != nil {
			return model.ReadChunkResponse{}, err
		}
		if c.SpoolID != "" {
			return model.ReadChunkResponse{}, cursorInvalid("cursor carries spooled state this endpoint does not produce")
		}
		if key, err = parseSpanKey(c.LastKey); err != nil {
			return model.ReadChunkResponse{}, err
		}
		if c.QueryHash != key.queryHash() {
			return model.ReadChunkResponse{}, cursorInvalid("cursor was issued for a different node or file")
		}
		gen, resumed = c.GenerationID, &c
	}

	store, leases := st.store, st.leases
	if s.read && st.queryStore != st.store {
		store, leases = st.queryStore, nil
	}
	reader, err := store.PinGeneration(ctx, st.repo, gen, st.cfg.Storage.QueryCursorTTL.Std())
	if err != nil {
		if resumed != nil && sqlite.IsGenerationCollected(err) {
			return model.ReadChunkResponse{}, cursorInvalid("the generation this cursor pins has been collected; read the span from its start again")
		}
		return model.ReadChunkResponse{}, err
	}
	defer func() {
		if cerr := reader.Close(); cerr != nil {
			st.logger.Warn("a pinned query lease could not be released",
				"component", "app", "error", cerr.Error())
		}
	}()
	binding := reader.Binding()
	if resumed != nil && (resumed.GenerationID != binding.GenerationID || resumed.AnalysisKey != binding.AnalysisKey) {
		return model.ReadChunkResponse{}, cursorInvalid("cursor was issued against a different generation")
	}

	span, err := resolveSpan(ctx, reader, key)
	if err != nil {
		return model.ReadChunkResponse{}, err
	}
	view, err := st.view(ctx, binding.SnapshotID)
	if err != nil {
		return model.ReadChunkResponse{}, err
	}
	// The coverage service's own sizer, so a chunk is the same size in and
	// outside a session.
	raw, err := coverageLimits(st.cfg).RawChunkBytes(req.MaxBytes, span.at)
	if err != nil {
		return model.ReadChunkResponse{}, err
	}
	end := min(span.at+uint64(raw), span.end)

	// The view returns exactly [at, end) with the whole-file position of each
	// end and the bytes before at that decide whether it is a boundary; the
	// planner is handed the span's end as its end of input.
	window, _, err := view.Read(ctx, span.file, model.ByteRange{Start: span.at, End: end})
	if err != nil {
		return model.ReadChunkResponse{}, err
	}
	if uint64(len(window.Bytes)) != end-span.at {
		return model.ReadChunkResponse{}, &model.Error{Code: model.CodeInternal, Message: fmt.Sprintf(
			"the source returned %d bytes for the %d-byte window [%d,%d)", len(window.Bytes), end-span.at, span.at, end)}
	}
	chunk, err := coverage.CutChunk(window, span.at, span.end, raw)
	if err != nil {
		return model.ReadChunkResponse{}, err
	}
	resp := model.ReadChunkResponse{
		Binding:     binding,
		FileID:      span.file,
		NodeID:      span.node,
		ContentHash: span.hash,
		ByteRange:   chunk.Plan.Range,
		LineRange:   chunk.Lines,
		Encoding:    chunk.Plan.Encoding,
		Content:     chunk.Content,
		PartialLine: chunk.Plan.PartialLine,
		NextOffset:  chunk.Plan.NextOffset,
	}
	if err := resp.Validate(); err != nil {
		return model.ReadChunkResponse{}, err
	}
	if chunk.Plan.NextOffset == nil {
		// The span is served whole: the lease the presented token held has
		// nothing left to retain. A handle that records no lease ends nothing,
		// because the row a writer minted is not this handle's to end.
		if resumed != nil && resumed.LeaseID != "" && leases.Retains() {
			if rerr := leases.Release(context.WithoutCancel(ctx), resumed.LeaseID); rerr != nil {
				st.logger.Warn("a source read continuation lease could not be released",
					"component", "app", "error", rerr.Error())
			}
		}
		return resp, nil
	}
	if resp.NextCursor, err = s.readSourceCursor(ctx, leases, binding, key, *chunk.Plan.NextOffset, resumed, now); err != nil {
		return model.ReadChunkResponse{}, err
	}
	return resp, nil
}

// resolveSpan turns a selector into the bytes it reads, in the pinned
// generation and nowhere else.
//
// A node resolves in that generation: one node id can live in several
// generations at different ranges, and the pinned generation's range and file
// row agree by construction, so a node that moved is read where the asked
// generation has it. A node or a file the generation does not hold -- deleted,
// never captured, or a working-tree overlay row, which is never a canonical
// fact -- is "not in this generation". A node whose content hash, file row or
// range disagrees with the pinned manifest is an integrity failure of the
// index, not a refusal of the request.
func resolveSpan(ctx context.Context, reader *sqlite.PinnedReader, key spanKey) (sourceSpan, error) {
	gen := reader.Binding().GenerationID
	if key.kind == spanNode {
		n, err := reader.Node(ctx, model.NodeID(key.id))
		if err != nil {
			if argumentInvalid(err) {
				return sourceSpan{}, notInGeneration("node", key.label, gen)
			}
			return sourceSpan{}, err
		}
		if n.Bytes == nil || n.Node.FileID == "" {
			return sourceSpan{}, (&model.Error{Code: model.CodeArgumentInvalid,
				Message: fmt.Sprintf("node %s has no source range in generation %d", key.label, gen)}).
				WithDetail("generation_id", strconv.FormatInt(int64(gen), 10))
		}
		fv, err := reader.File(ctx, n.Node.FileID)
		if err != nil {
			if argumentInvalid(err) {
				return sourceSpan{}, sourceIntegrity(key.label, gen, "its file is not in the generation's manifest")
			}
			return sourceSpan{}, err
		}
		switch {
		case fv.Status == model.FileDeleted:
			return sourceSpan{}, sourceIntegrity(key.label, gen, "its file is a deletion in the generation's manifest")
		case n.Node.ContentHash != fv.ContentHash:
			return sourceSpan{}, sourceIntegrity(key.label, gen, "its content hash disagrees with the manifest's")
		case n.Bytes.Start > n.Bytes.End || n.Bytes.End > uint64(fv.Size):
			return sourceSpan{}, sourceIntegrity(key.label, gen, "its range does not lie within its file")
		}
		at := n.Bytes.Start
		if key.set {
			if key.at < n.Bytes.Start || key.at > n.Bytes.End {
				return sourceSpan{}, cursorInvalid("cursor continues outside the node it names")
			}
			at = key.at
		}
		return sourceSpan{file: n.Node.FileID, node: n.Node.ID, hash: fv.ContentHash, at: at, end: n.Bytes.End}, nil
	}
	fv, err := reader.File(ctx, model.FileID(key.id))
	if err != nil {
		if argumentInvalid(err) {
			return sourceSpan{}, notInGeneration("file", key.label, gen)
		}
		return sourceSpan{}, err
	}
	if fv.Status == model.FileDeleted {
		return sourceSpan{}, notInGeneration("file", key.label, gen)
	}
	size := uint64(fv.Size)
	if key.at > size {
		return sourceSpan{}, &model.Error{Code: model.CodeArgumentInvalid, Message: fmt.Sprintf(
			"read_chunk.offset is %d, past the %d-byte file", key.at, size)}
	}
	return sourceSpan{file: fv.ID, hash: fv.ContentHash, at: key.at, end: size}, nil
}

// readSourceCursor signs the continuation of a span longer than one chunk.
//
// A first page takes a fresh cursor-owned retention lease, never the call's
// query lease, which is released when this call returns. A later page renews
// the lease its token carries and hands it forward, so a span read to its end
// holds one lease however many pages it took, and the generation outlives a
// retention pass between any two of them. A handle that records no lease
// mints the token anyway, bound to its expiry alone: a token that already names
// a lease keeps that lease's expiry, and one that names none is good for one
// cursor lifetime from now.
func (s *Services) readSourceCursor(ctx context.Context, leases *pagination.Leases, b model.Binding,
	key spanKey, next uint64, resumed *pagination.Cursor, now time.Time) (string, error) {
	st := s.w.s
	c := pagination.Cursor{
		Endpoint:     readSourceEndpoint,
		GenerationID: b.GenerationID,
		AnalysisKey:  b.AnalysisKey,
		QueryHash:    key.queryHash(),
		LastKey:      key.lastKey(next),
		ExpiresAt:    now.Add(st.cfg.Storage.QueryCursorTTL.Std()),
	}
	var minted string
	switch {
	case resumed != nil && resumed.LeaseID != "" && leases.Retains():
		expires, err := leases.Renew(ctx, resumed.LeaseID)
		if err != nil {
			return "", err
		}
		c.LeaseID, c.ExpiresAt = resumed.LeaseID, expires
	case resumed != nil && resumed.LeaseID != "":
		c.LeaseID, c.ExpiresAt = resumed.LeaseID, resumed.ExpiresAt
	case leases.Retains():
		lease, err := leases.Acquire(ctx, b.GenerationID, b.SnapshotID, model.LeaseCursor)
		if err != nil {
			return "", err
		}
		c.LeaseID, c.ExpiresAt, minted = lease.ID, lease.ExpiresAt, lease.ID
	}
	token, err := st.signer.EncodeCursor(c)
	if err != nil {
		if minted != "" {
			if rerr := leases.Release(context.WithoutCancel(ctx), minted); rerr != nil {
				st.logger.Warn("a source read continuation lease could not be released",
					"component", "app", "error", rerr.Error())
			}
		}
		return "", err
	}
	return token, nil
}

// argumentInvalid reports whether err is the store's refusal of an id the
// pinned generation does not hold.
func argumentInvalid(err error) bool {
	var typed *model.Error
	return errors.As(err, &typed) && typed.Code == model.CodeArgumentInvalid
}

// notInGeneration is the one refusal of a node, file or path the pinned
// generation does not hold.
func notInGeneration(what, label string, gen model.GenerationID) error {
	return (&model.Error{Code: model.CodeArgumentInvalid,
		Message:     fmt.Sprintf("%s %s is not in this generation (%d)", what, label, gen),
		Remediation: "pass the generation_id of the answer that named it, or look it up again in this generation"}).
		WithDetail("generation_id", strconv.FormatInt(int64(gen), 10))
}

// sourceIntegrity is a node whose stored range or hash disagrees with the
// pinned manifest it was published against.
func sourceIntegrity(label string, gen model.GenerationID, why string) error {
	return (&model.Error{Code: model.CodeSourceIntegrity,
		Message: fmt.Sprintf("node %s cannot be read from generation %d: %s", label, gen, why)}).
		WithDetail("generation_id", strconv.FormatInt(int64(gen), 10))
}
