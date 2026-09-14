package mcpserver

import (
	"log/slog"
	"time"

	"github.com/Sawmonabo/codectx/internal/app"
	"github.com/Sawmonabo/codectx/internal/config"
	"github.com/Sawmonabo/codectx/internal/model"
)

// handlers carries everything the 23 tool handlers need.
//
// It holds the FOUR NARROW FACADE INTERFACES and never *app.Services.
// *app.Services is a concrete struct and therefore cannot be faked, while
// app.IndexService, app.ExploreService, app.ContextService and
// app.DiagnoseService exist for exactly this ("The four narrow interfaces below
// exist for consumers to depend on" — internal/app/services.go) and *Services
// satisfies all four. That is the seam that lets every lane's test rows run
// against an in-package fake while the facade bodies are still landing, and it
// is why internal/cli/mcp.go can hand ws.Services() to all four fields.
//
// log writes to STDERR. stdout belongs to the SDK's framing (see doc.go).
type handlers struct {
	index   app.IndexService
	explore app.ExploreService
	context app.ContextService
	// diagnose has no Task 19 consumer: Section 19.2 lists no doctor tool, so
	// Doctor's typed refusal needs no wire surface. It is held here rather than
	// dropped because New takes the whole facade and a fifth constructor
	// signature later would be the churn; this is wave-E Q14's existing ledger
	// entry, not a new gap.
	diagnose app.DiagnoseService

	// cfg carries the Section 20 bounds. L1's limitMiddleware reads
	// resources.max_metadata_response_bytes, max_concurrent_queries,
	// max_concurrent_graph_queries and query_timeout from it; L4's readSource
	// reads max_source_response_bytes.
	cfg   config.Config
	build model.BuildInfo
	log   *slog.Logger
}

// The frozen per-tool method set. Every name below is registered by
// registry.go and stubbed in its lane's file; a lane implements its own block
// and touches no other file. A registration without a handler, or a handler
// without a registration, is a defect the schema snapshot catches.
//
//	explore.go  — L2: indexStatus, refreshIndex, repoOverview, search, findSymbol, references
//	graph.go    — L3: symbolInfo, callers, callees, dependencyPath, impact
//	session.go  — L4: contextPlan, contextStatus, contextNext, contextEntries, contextInclude, readSource
//	gate.go     — L5: contextAcknowledge, contextWaive, contextRecord, contextAdvance, contextCapsule, contextClose
//
// Frozen by L0 as a comment, implemented by the named lane:
//
//	server.go            — L1:  func New(Options) (*Server, error); func (*Server) Serve(ctx context.Context) error
//	limits.go            — L1:  func (s *Server) limitMiddleware() mcp.Middleware
//	internal/cli/mcp.go  — INT: newMCPCommand(build model.BuildInfo) *cobra.Command
//	                            for `codectx mcp serve --repo PATH [--watch]`
//
// notImplemented is what every stub returns until its lane lands. It is a
// typed CTX_INTERNAL, never a panic: a half-built server must still answer the
// protocol rather than kill the process and the session with it.
func (h *handlers) notImplemented(tool string) error {
	return toolFailure(h.log, &model.Error{
		Code:    model.CodeInternal,
		Message: "tool " + tool + " is not implemented in this build",
	})
}

// --- Local composite types --------------------------------------------------
//
// Every other tool's In is the landed model request type VERBATIM: they already
// carry complete lowercase JSON tags and no internal-only field, and
// hand-declaring 23 twins is the parallel implementation policy.md forbids.
// The types below are the only new ones. A lane that wants another reports to
// the controller rather than adding it.

// emptyInput is the argument type of a tool that takes none. The SDK infers
// `{"type":"object"}` from it, which is what the protocol requires.
type emptyInput struct{}

// refreshInput deliberately has NO watch field: a tool never starts a watcher.
// The handler maps this onto model.IndexRequest with Watch forced false; the
// watcher is the serve process's decision, made once from mcp.watch.
type refreshInput struct {
	Full    bool `json:"full"`
	Rebuild bool `json:"rebuild"`
}

// symbolInfoInput drives the composed Symbol+References answer of tool 6. It
// names no operation: symbol_info fixes resolve and references, which is what
// makes it one answer rather than a second general call path.
type symbolInfoInput struct {
	GenerationID   model.GenerationID   `json:"generation_id,omitempty"`
	Query          string               `json:"query"`
	SemanticSource model.SemanticSource `json:"semantic_source"`
	Profile        string               `json:"profile,omitempty"`
	Page           model.PageRequest    `json:"page"`
}

// symbolInfoOutput is metadata beside evidence. The handler threads the first
// answer's Page.Meta.Binding.GenerationID into the second request, so the two
// halves can never straddle a generation change.
type symbolInfoOutput struct {
	Symbol   model.Page[model.Node]                `json:"symbol"`
	Evidence model.Page[model.ReferenceOccurrence] `json:"evidence"`
}

// graphInput omits direction entirely: codectx_callers and codectx_callees fix
// it from the tool name, so a client cannot ask callers for outbound edges.
type graphInput struct {
	GenerationID model.GenerationID   `json:"generation_id,omitempty"`
	Start        []model.NodeID       `json:"start"`
	Relations    []model.RelationKind `json:"relations,omitempty"`
	MaxDepth     int                  `json:"max_depth"`
	MaxVisited   int                  `json:"max_visited"`
	MaxEdges     int                  `json:"max_edges"`
	Page         model.PageRequest    `json:"page"`
}

// planOutput shows the manifest and the opened session's status together, so
// `context_plan` needs no second round trip — the facade returns both.
type planOutput struct {
	Plan   model.PlanResult    `json:"plan"`
	Status model.SessionStatus `json:"status"`
}

// statusInput pages coverage for one session. Session and actor are TOOL
// ARGUMENTS, never MCP wire state: no handler reads an identity from
// CallToolRequest.
type statusInput struct {
	SessionID model.SessionID   `json:"session_id"`
	ActorID   string            `json:"actor_id"`
	Page      model.PageRequest `json:"page"`
}

// statusOutput pairs the coverage page with the session status.
type statusOutput struct {
	Coverage model.Page[model.FileCoverage] `json:"coverage"`
	Status   model.SessionStatus            `json:"status"`
}

// waiveOutput pairs the recorded waiver with the resulting session status, so
// a caller sees immediately that a waiver does not make a session ready.
type waiveOutput struct {
	Waiver model.WaiverRecord  `json:"waiver"`
	Status model.SessionStatus `json:"status"`
}

// recordOutput pairs the stored observation with the resulting status.
type recordOutput struct {
	Observation model.Observation   `json:"observation"`
	Status      model.SessionStatus `json:"status"`
}

// advanceOutput pairs the workflow transition with the resulting status.
type advanceOutput struct {
	Workflow model.WorkflowStatus `json:"workflow"`
	Status   model.SessionStatus  `json:"status"`
}

// capsuleOutput is exactly one of its two fields. The six model.CapsuleView
// values return the bounded page; view="export" returns the metadata
// projection.
type capsuleOutput struct {
	Page   *model.CapsulePage `json:"page,omitempty"`
	Export *capsuleExport     `json:"export,omitempty"`
}

// capsuleExport is the canonical export METADATA projection and NEVER the
// capsule body. A capsule is bounded by context.max_capsule_bytes at 8 MiB
// against a resources.max_metadata_response_bytes ceiling of 256 KiB, so
// returning model.Capsule whole over this tool could not honour the response
// bound. Section 19.2's "bounded capsule page or canonical export metadata" is
// this field set: identity, binding, both hashes, the scope version, the
// strict-gate flag, per-section counts and the creation time — everything
// needed to verify or fetch an export, and no capsule content.
type capsuleExport struct {
	SessionID           model.SessionID `json:"session_id"`
	ActorID             string          `json:"actor_id"`
	Binding             model.Binding   `json:"binding"`
	ManifestHash        string          `json:"manifest_hash"`
	CanonicalHash       string          `json:"canonical_hash"`
	ScopeVersion        int             `json:"scope_version"`
	StrictGateSatisfied bool            `json:"strict_gate_satisfied"`
	Counts              map[string]int  `json:"counts"`
	CreatedAt           time.Time       `json:"created_at"`
}

// closeInput carries the optimistic-concurrency version the facade's
// CloseSession takes beside the session request.
type closeInput struct {
	SessionID       model.SessionID `json:"session_id"`
	ActorID         string          `json:"actor_id"`
	ExpectedVersion int             `json:"expected_version"`
}
