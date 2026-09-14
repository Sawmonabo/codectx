package app

import (
	"context"

	"github.com/Sawmonabo/codectx/internal/model"
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

// Index builds a new generation over this workspace. Owned by L7.
func (s *Services) Index(ctx context.Context, req model.IndexRequest) (model.IndexResult, error) {
	return model.IndexResult{}, notImplemented("Index")
}

// Refresh builds an incremental generation over this workspace. Owned by L7.
func (s *Services) Refresh(ctx context.Context, req model.IndexRequest) (model.IndexResult, error) {
	return model.IndexResult{}, notImplemented("Refresh")
}

// IndexStatus reports the active generation and its capability completeness.
// Owned by L7.
func (s *Services) IndexStatus(ctx context.Context) (model.IndexStatus, error) {
	return model.IndexStatus{}, notImplemented("IndexStatus")
}

// --- ExploreService ---------------------------------------------------------

// Overview reports the repository's top-level structure. Owned by L7.
func (s *Services) Overview(ctx context.Context, req model.OverviewRequest) (model.Page[model.OverviewItem], error) {
	return model.Page[model.OverviewItem]{}, notImplemented("Overview")
}

// Search answers generation-local lexical retrieval and exact lookup. Owned by L7.
func (s *Services) Search(ctx context.Context, req model.SearchRequest) (model.Page[model.SearchHit], error) {
	return model.Page[model.SearchHit]{}, notImplemented("Search")
}

// Symbol resolves a symbol through the canonical index, or through the LSP
// overlay when the request names semantic_source=lsp. Owned by L7; the overlay
// route it dispatches to is L8's.
func (s *Services) Symbol(ctx context.Context, req model.SymbolRequest) (model.Page[model.Node], error) {
	return model.Page[model.Node]{}, notImplemented("Symbol")
}

// References answers reference, implements and type-definition evidence, from
// the canonical index or the LSP overlay. Owned by L7.
func (s *Services) References(ctx context.Context, req model.ReferenceRequest) (model.Page[model.ReferenceOccurrence], error) {
	return model.Page[model.ReferenceOccurrence]{}, notImplemented("References")
}

// Graph answers bounded callers/callees traversal. Owned by L7.
func (s *Services) Graph(ctx context.Context, req model.GraphRequest) (model.GraphResult, error) {
	return model.GraphResult{}, notImplemented("Graph")
}

// Path answers bounded dependency-path search. Owned by L7.
func (s *Services) Path(ctx context.Context, req model.PathRequest) (model.PathResult, error) {
	return model.PathResult{}, notImplemented("Path")
}

// Impact answers bounded impact analysis. Owned by L7.
func (s *Services) Impact(ctx context.Context, req model.ImpactRequest) (model.Page[model.ImpactEntry], error) {
	return model.Page[model.ImpactEntry]{}, notImplemented("Impact")
}

// --- ContextService ---------------------------------------------------------

// Plan compiles one immutable manifest and opens the actor's session over it,
// returning both. Owned by L7.
func (s *Services) Plan(ctx context.Context, req model.PlanRequest) (model.PlanResult, model.SessionStatus, error) {
	return model.PlanResult{}, model.SessionStatus{}, notImplemented("Plan")
}

// SessionStatus reports one page of per-file coverage beside the honest session
// status. Owned by L7.
func (s *Services) SessionStatus(ctx context.Context, req model.SessionRequest, page model.PageRequest) (model.Page[model.FileCoverage], model.SessionStatus, error) {
	return model.Page[model.FileCoverage]{}, model.SessionStatus{}, notImplemented("SessionStatus")
}

// Next names the next required file and byte offset to read. Metadata only: it
// never carries source bytes. Owned by L7.
func (s *Services) Next(ctx context.Context, req model.SessionRequest) (model.NextContextItem, error) {
	return model.NextContextItem{}, notImplemented("Next")
}

// Entries pages one projection of the session's current manifest. Metadata
// only. Owned by L7.
func (s *Services) Entries(ctx context.Context, req model.ContextPageRequest) (model.ContextPage, error) {
	return model.ContextPage{}, notImplemented("Entries")
}

// Include extends a pinned session's scope under a compare-and-swap. Owned by L7.
func (s *Services) Include(ctx context.Context, req model.IncludeRequest) (model.SessionStatus, error) {
	return model.SessionStatus{}, notImplemented("Include")
}

// Read serves one bounded chunk of snapshot-pinned source. It is the only
// method on this facade that returns source bytes. Owned by L7.
func (s *Services) Read(ctx context.Context, req model.ReadChunkRequest) (model.ReadChunkResponse, error) {
	return model.ReadChunkResponse{}, notImplemented("Read")
}

// Acknowledge confirms delivered chunks by echoing their signed receipts, which
// is what turns delivery into coverage. Owned by L7.
func (s *Services) Acknowledge(ctx context.Context, req model.AcknowledgeRequest) (model.SessionStatus, error) {
	return model.SessionStatus{}, notImplemented("Acknowledge")
}

// Waive records a required-file waiver. A waiver never grants strict readiness.
// Owned by L7.
func (s *Services) Waive(ctx context.Context, req model.WaiverRequest) (model.WaiverRecord, model.SessionStatus, error) {
	return model.WaiverRecord{}, model.SessionStatus{}, notImplemented("Waive")
}

// Record persists one immutable observation, scope reviews included. Owned by L7.
func (s *Services) Record(ctx context.Context, req model.ObservationRequest) (model.Observation, model.SessionStatus, error) {
	return model.Observation{}, model.SessionStatus{}, notImplemented("Record")
}

// Advance applies one guarded, version-checked transition. Owned by L7.
func (s *Services) Advance(ctx context.Context, req model.AdvanceRequest) (model.WorkflowStatus, model.SessionStatus, error) {
	return model.WorkflowStatus{}, model.SessionStatus{}, notImplemented("Advance")
}

// Capsule pages one projection of the sealed capsule. Owned by L7.
func (s *Services) Capsule(ctx context.Context, req model.CapsuleRequest) (model.CapsulePage, error) {
	return model.CapsulePage{}, notImplemented("Capsule")
}

// Export returns the whole sealed capsule. Owned by L7.
func (s *Services) Export(ctx context.Context, req model.SessionRequest) (model.Capsule, error) {
	return model.Capsule{}, notImplemented("Export")
}

// CloseSession closes the session under the caller's expected version. Owned by L7.
func (s *Services) CloseSession(ctx context.Context, req model.SessionRequest, expectedVersion int) (model.SessionStatus, error) {
	return model.SessionStatus{}, notImplemented("CloseSession")
}

// --- DiagnoseService --------------------------------------------------------

// Doctor reports this installation's health. Owned by L7.
func (s *Services) Doctor(ctx context.Context, req model.DoctorRequest) (model.DoctorReport, error) {
	return model.DoctorReport{}, notImplemented("Doctor")
}

// notImplemented is the placeholder body of every declaration L0 froze and L7
// owns. It is a typed CTX_INTERNAL rather than a panic so a command wired
// against the facade before L7 lands reports a defect instead of crashing.
func notImplemented(op string) error {
	return &model.Error{Code: model.CodeInternal, Message: "app: " + op + " is not implemented"}
}
