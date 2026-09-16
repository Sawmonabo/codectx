package mcpserver

import (
	"encoding/json"
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
	addTool(s, "codectx_index_status", "Index status",
		"Active generation, health, coherence and capability completeness. Set resources=true for the accounting block.", h.indexStatus)
	addTool(s, "codectx_refresh_index", "Refresh index",
		"Build an incremental generation over the current workspace. Pass a progress token to "+
			"receive one progress notification per finished stage, at most one a second; ask for a "+
			"logging level to receive each finished stage as a log message carrying its row.", h.refreshIndex)
	addTool(s, "codectx_repo_overview", "Repository overview",
		"Bounded repository, package, module and language map.", h.repoOverview)
	addTool(s, "codectx_search", "Search",
		"Ranked lexical, path and symbol retrieval over one generation.", h.search)
	addTool(s, "codectx_find_symbol", "Find symbol",
		"Resolve a symbol through the canonical index or the LSP overlay.", h.findSymbol)

	// --- L3: symbol composition ----------------------------------------
	addTool(s, "codectx_symbol_info", "Symbol info",
		"Symbol metadata with reference evidence pinned to one generation.", h.symbolInfo)

	// --- L2: references -------------------------------------------------
	addTool(s, "codectx_references", "References",
		"Reference, implementation or type-definition occurrences of a node.", h.references)

	// --- L3: traversal ---------------------------------------------------
	addTool(s, "codectx_callers", "Callers",
		"Bounded inbound call neighborhood of one or more nodes.", h.callers)
	addTool(s, "codectx_callees", "Callees",
		"Bounded outbound call neighborhood of one or more nodes.", h.callees)
	addTool(s, "codectx_dependency_path", "Dependency path",
		"Bounded shortest dependency path between two resolved nodes. `direction` chooses "+
			"which way edges are followed: \"outgoing\" (the default: what `from` depends on), "+
			"\"incoming\" (what depends on it) or \"both\" (either way). A node that is only ever "+
			"called has no outgoing route to its callers, so a pair connected against the edge "+
			"direction reports no path until `direction` is incoming or both.", h.dependencyPath)
	addTool(s, "codectx_impact", "Impact",
		"Affected scope and required package boundaries, with completeness.", h.impact)

	// --- L4: session lifecycle -------------------------------------------
	addTool(s, "codectx_context_plan", "Plan context",
		"Open a read session and return its manifest beside its status.", h.contextPlan)
	addTool(s, "codectx_context_status", "Context status",
		"Paged per-file coverage beside the session's gate status.", h.contextStatus)
	addTool(s, "codectx_context_next", "Next context item",
		"The next required read action. Metadata only; no source bytes.", h.contextNext)
	addTool(s, "codectx_context_entries", "Context entries",
		"One paged projection of a session's current manifest.", h.contextEntries)
	addTool(s, "codectx_context_include", "Include seeds",
		"Widen a session's scope with additional seeds.", h.contextInclude)
	addTool(s, "codectx_read_source", "Read source",
		"The only tool that returns source bytes: one bounded chunk with a receipt.", h.readSource)

	// --- L5: review gate and capsule --------------------------------------
	addTool(s, "codectx_context_acknowledge", "Acknowledge",
		"Confirm issued read receipts or a fully read file.", h.contextAcknowledge)
	addTool(s, "codectx_context_waive", "Waive file",
		"Record a reasoned waiver for a required file.", h.contextWaive)
	addTool(s, "codectx_context_record", "Record observation",
		"Record an accepted fact, rejection, contradiction or scope review.", h.contextRecord)
	addTool(s, "codectx_context_advance", "Advance workflow",
		"Move the session to a target workflow state under its version.", h.contextAdvance)
	addTool(s, "codectx_context_capsule", "Context capsule",
		`One keyset page of one capsule list named by view, continued with meta.next_cursor; `+
			`view="export" returns the capsule's identity and per-list record counts instead.`, h.contextCapsule)
	addTool(s, "codectx_context_close", "Close session",
		"Close a session under its expected state version.", h.contextClose)
}

// toolCount is the Section 19.2 surface size. Section 19.2 lists 23 tools; the
// "24" of wave-E Q16 is an off-by-one and is ledgered. There is no doctor tool,
// which is why this package takes no app.DiagnoseService at all.
const toolCount = 23

// addTool registers one tool with BOTH of its schemas preset from the handler's
// own request and answer types.
//
// In and Out are inferred from the handler value, never written at the call
// site: an explicitly named input type could drift from the handler's actual
// parameter type and preset a schema for a struct the tool does not decode.
// Presetting both halves is also what closes the output-schema defect the
// outputSchemas table below describes -- the SDK infers an output schema
// whenever one is absent, and its inference of json.RawMessage does not match
// what that field marshals to.
func addTool[In, Out any](s *mcp.Server, name, title, description string,
	h mcp.ToolHandlerFor[In, Out]) {
	mcp.AddTool(s, &mcp.Tool{
		Name:         name,
		Title:        title,
		Description:  description,
		InputSchema:  schemaFor[In](name, "input", enumSchemas),
		OutputSchema: schemaFor[Out](name, "output", outputSchemas),
	}, h)
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
	// selects the identity-and-counts projection rather than a page of one list.
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
// Export's identity-and-counts metadata instead of one keyset page of one
// capsule list (digest §4).
const capsuleViewExport = "export"

// stringEnum is the one schema shape in the table above.
func stringEnum(values ...string) *jsonschema.Schema {
	enum := make([]any, len(values))
	for i, v := range values {
		enum[i] = v
	}
	return &jsonschema.Schema{Type: "string", Enum: enum}
}

// outputSchemas is the OUTPUT side of the same one-table mechanism: the same
// jsonschema.ForOptions.TypeSchemas hook, keyed by reflect.Type, applied to the
// answer envelope instead of the request. It is a separate table, not a second
// mechanism, because enumSchemas must NOT reach an output -- the reason is
// spelled out above: output structs legitimately carry zero-valued enum fields
// that an enum list without "" would reject.
//
// What it corrects: json.RawMessage is []byte, which jsonschema-go infers as
// ["null","array"], while the value marshals as whatever JSON it holds -- an
// object for model.Node.Metadata (model/facts.go). Every non-empty answer
// carrying such a node therefore failed the SDK's output validation and came
// back to the client as a JSON-RPC PROTOCOL error instead of a result, with no
// Section 22 code to read (codectx_find_symbol, codectx_symbol_info,
// codectx_callers, codectx_callees and codectx_dependency_path all return
// nodes). internal/model's wire shape is correct and is exercised by the CLI,
// so the correction belongs here.
//
// Keying by type rather than by property path is what makes this one entry
// sufficient: it covers json.RawMessage wherever it occurs in any tool's answer,
// at any depth, today and for any field a later type adds. The schema is the
// empty (always-true) one, which is the honest description of a field whose
// contents are deliberately unconstrained JSON.
var outputSchemas = map[reflect.Type]*jsonschema.Schema{
	reflect.TypeFor[json.RawMessage](): {},
}

// schemaFor presets one half of a tool's schema, which the SDK honors as-is;
// inference only fills a nil one. presets selects which of the two type tables
// above applies; half names the side for the panic message.
//
// It panics rather than returning an error because a schema that cannot be
// inferred is a build-time defect in this package, not a runtime condition: the
// tool would be unusable and the process must not come up pretending otherwise.
// register() is exercised by the schema snapshot, so the panic surfaces in
// `go test`, never in a serving process.
func schemaFor[T any](tool, half string, presets map[reflect.Type]*jsonschema.Schema) *jsonschema.Schema {
	s, err := jsonschema.For[T](&jsonschema.ForOptions{TypeSchemas: presets})
	if err != nil {
		panic(fmt.Sprintf("mcpserver: %s schema for tool %q: %v", half, tool, err))
	}
	return s
}
