package mcpserver

import (
	"fmt"
	"reflect"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/Sawmonabo/codectx/internal/model"
)

// register installs the 23 tools of Section 19.2, in the digest §4 order.
//
// This is the whole tool surface in one place: there is no per-lane
// registration file, so a lane adding a tool, dropping one or renaming one is a
// single-file diff the schema snapshot in mcpserver_test.go fails on. Titles
// and descriptions are deliberately terse — Section 19.2 requires labels to be
// compact so tool schemas do not consume unnecessary client context.
//
// No prompts, resources or completion handlers are registered: Section 19.3
// scopes V1 to tools.
func register(s *mcp.Server, h *handlers) {
	// --- L2: index and discovery ---------------------------------------
	mcp.AddTool(s, toolFor[emptyInput]("codectx_index_status", "Index status",
		"Active generation, health, coherence and capability completeness."), h.indexStatus)
	mcp.AddTool(s, toolFor[refreshInput]("codectx_refresh_index", "Refresh index",
		"Build an incremental generation over the current workspace."), h.refreshIndex)
	mcp.AddTool(s, toolFor[model.OverviewRequest]("codectx_repo_overview", "Repository overview",
		"Bounded repository, package, module and language map."), h.repoOverview)
	mcp.AddTool(s, toolFor[model.SearchRequest]("codectx_search", "Search",
		"Ranked lexical, path and symbol retrieval over one generation."), h.search)
	mcp.AddTool(s, toolFor[model.SymbolRequest]("codectx_find_symbol", "Find symbol",
		"Resolve a symbol through the canonical index or the LSP overlay."), h.findSymbol)

	// --- L3: symbol composition ----------------------------------------
	mcp.AddTool(s, toolFor[symbolInfoInput]("codectx_symbol_info", "Symbol info",
		"Symbol metadata with reference evidence pinned to one generation."), h.symbolInfo)

	// --- L2: references -------------------------------------------------
	mcp.AddTool(s, toolFor[model.ReferenceRequest]("codectx_references", "References",
		"Reference, implementation or type-definition occurrences of a node."), h.references)

	// --- L3: traversal ---------------------------------------------------
	mcp.AddTool(s, toolFor[graphInput]("codectx_callers", "Callers",
		"Bounded inbound call neighborhood of one or more nodes."), h.callers)
	mcp.AddTool(s, toolFor[graphInput]("codectx_callees", "Callees",
		"Bounded outbound call neighborhood of one or more nodes."), h.callees)
	mcp.AddTool(s, toolFor[model.PathRequest]("codectx_dependency_path", "Dependency path",
		"Bounded shortest dependency path between two resolved nodes."), h.dependencyPath)
	mcp.AddTool(s, toolFor[model.ImpactRequest]("codectx_impact", "Impact",
		"Affected scope and required package boundaries, with completeness."), h.impact)

	// --- L4: session lifecycle -------------------------------------------
	mcp.AddTool(s, toolFor[model.PlanRequest]("codectx_context_plan", "Plan context",
		"Open a read session and return its manifest beside its status."), h.contextPlan)
	mcp.AddTool(s, toolFor[statusInput]("codectx_context_status", "Context status",
		"Paged per-file coverage beside the session's gate status."), h.contextStatus)
	mcp.AddTool(s, toolFor[model.SessionRequest]("codectx_context_next", "Next context item",
		"The next required read action. Metadata only; no source bytes."), h.contextNext)
	mcp.AddTool(s, toolFor[model.ContextPageRequest]("codectx_context_entries", "Context entries",
		"One paged projection of a session's current manifest."), h.contextEntries)
	mcp.AddTool(s, toolFor[model.IncludeRequest]("codectx_context_include", "Include seeds",
		"Widen a session's scope with additional seeds."), h.contextInclude)
	mcp.AddTool(s, toolFor[model.ReadChunkRequest]("codectx_read_source", "Read source",
		"The only tool that returns source bytes: one bounded chunk with a receipt."), h.readSource)

	// --- L5: review gate and capsule --------------------------------------
	mcp.AddTool(s, toolFor[model.AcknowledgeRequest]("codectx_context_acknowledge", "Acknowledge",
		"Confirm issued read receipts or a fully read file."), h.contextAcknowledge)
	mcp.AddTool(s, toolFor[model.WaiverRequest]("codectx_context_waive", "Waive file",
		"Record a reasoned waiver for a required file."), h.contextWaive)
	mcp.AddTool(s, toolFor[model.ObservationRequest]("codectx_context_record", "Record observation",
		"Record an accepted fact, rejection, contradiction or scope review."), h.contextRecord)
	mcp.AddTool(s, toolFor[model.AdvanceRequest]("codectx_context_advance", "Advance workflow",
		"Move the session to a target workflow state under its version."), h.contextAdvance)
	mcp.AddTool(s, toolFor[model.CapsuleRequest]("codectx_context_capsule", "Context capsule",
		`One bounded capsule view, or view="export" for canonical export metadata.`), h.contextCapsule)
	mcp.AddTool(s, toolFor[closeInput]("codectx_context_close", "Close session",
		"Close a session under its expected state version."), h.contextClose)
}

// toolCount is the Section 19.2 surface size. Section 19.2 lists 23 tools; the
// "24" of wave-E Q16 is an off-by-one and is ledgered. There is no doctor tool,
// which is why DiagnoseService keeps no Task 19 consumer.
const toolCount = 23

// toolFor builds one tool's metadata with the preset input schema below.
func toolFor[In any](name, title, description string) *mcp.Tool {
	return &mcp.Tool{
		Name:        name,
		Title:       title,
		Description: description,
		InputSchema: inputSchema[In](name),
	}
}

// enumSchemas is the ONE enum table, and it is keyed by Go type rather than by
// JSON property name.
//
// jsonschema-go infers a plain `"type":"string"` for a named string type, so
// Section 19.3's enum validation would otherwise be missing from all 23
// schemas. Keying by reflect.Type is what makes a single table correct: the
// property name `kind` means AcknowledgeKind on one tool and ObservationKind on
// another, and `operation` means SymbolOperation on one and ReferenceOperation
// on another, so a name-keyed table would cross-contaminate them.
//
// These overrides are applied to INPUT schemas only, and must stay that way.
// AddTool also validates outputs, and output structs legitimately carry
// zero-valued enum fields (GraphResult.Direction, SessionStatus.Phase and
// State, CapsulePage.View on an empty answer); an enum that does not list ""
// would reject them. Output schemas are left to the SDK's own inference.
//
// Schema validation does NOT replace Validate(): required-ness in an inferred
// schema comes only from the absence of omitempty, so exactly-one-of,
// distinctness and byte bounds still need the request's own Validate().
var enumSchemas = map[reflect.Type]*jsonschema.Schema{
	reflect.TypeFor[model.SemanticSource](): stringEnum(
		string(model.SemanticCanonical), string(model.SemanticLSP)),
	reflect.TypeFor[model.SymbolOperation](): stringEnum(
		string(model.SymbolResolve), string(model.SymbolDocumentSymbols),
		string(model.SymbolWorkspaceSymbols), string(model.SymbolDefinition)),
	reflect.TypeFor[model.ReferenceOperation](): stringEnum(
		string(model.ReferenceReferences), string(model.ReferenceImplements),
		string(model.ReferenceTypeDefinition)),
	reflect.TypeFor[model.ContextView](): stringEnum(
		string(model.ViewEntries), string(model.ViewSlices), string(model.ViewExcluded)),
	// The capsule view carries one spelling the model type does not: "export"
	// selects the canonical metadata projection rather than a bounded page.
	reflect.TypeFor[model.CapsuleView](): stringEnum(
		string(model.CapsuleViewAcceptedFacts), string(model.CapsuleViewRejectedFacts),
		string(model.CapsuleViewContradictions), string(model.CapsuleViewUnresolved),
		string(model.CapsuleViewCoverage), string(model.CapsuleViewWaivers),
		capsuleViewExport),
	reflect.TypeFor[model.Direction](): stringEnum(
		string(model.DirectionOutgoing), string(model.DirectionIncoming), string(model.DirectionBoth)),
	reflect.TypeFor[model.Phase](): stringEnum(
		string(model.PhaseSweep), string(model.PhaseVerify), string(model.PhaseConsolidate)),
	reflect.TypeFor[model.AcknowledgeKind](): stringEnum(
		string(model.AcknowledgeReceipt), string(model.AcknowledgeFile)),
	reflect.TypeFor[model.ObservationKind](): stringEnum(
		string(model.ObservationAcceptFact), string(model.ObservationRejectFact),
		string(model.ObservationContradiction), string(model.ObservationUnresolved),
		string(model.ObservationScopeReview)),
	reflect.TypeFor[model.WorkflowState](): stringEnum(
		string(model.StateSweepOpen), string(model.StateVerifyOpen), string(model.StateConsolidateOpen),
		string(model.StateComplete), string(model.StateClosed)),
}

// capsuleViewExport is the codectx_context_capsule spelling that routes to
// Export's canonical metadata instead of Capsule's bounded page (digest §4).
const capsuleViewExport = "export"

// stringEnum is the one schema shape in the table above.
func stringEnum(values ...string) *jsonschema.Schema {
	enum := make([]any, len(values))
	for i, v := range values {
		enum[i] = v
	}
	return &jsonschema.Schema{Type: "string", Enum: enum}
}

// inputSchema presets a tool's input schema, which the SDK honors as-is;
// inference only fills a nil one.
//
// It panics rather than returning an error because a schema that cannot be
// inferred is a build-time defect in this package, not a runtime condition: the
// tool would be unusable and the process must not come up pretending otherwise.
// register() is exercised by the schema snapshot, so the panic surfaces in
// `go test`, never in a serving process.
func inputSchema[In any](tool string) *jsonschema.Schema {
	s, err := jsonschema.For[In](&jsonschema.ForOptions{TypeSchemas: enumSchemas})
	if err != nil {
		panic(fmt.Sprintf("mcpserver: input schema for tool %q: %v", tool, err))
	}
	return s
}
