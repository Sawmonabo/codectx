package mcpserver

import (
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/Sawmonabo/codectx/internal/app"
	"github.com/Sawmonabo/codectx/internal/config"
	"github.com/Sawmonabo/codectx/internal/model"
)

// ---------------------------------------------------------------------------
// The deterministic facade seam.
// ---------------------------------------------------------------------------

// fakeServices implements the four narrow facade interfaces with one function
// field per method. Fields, not method bodies: a lane sets only the field its
// row needs and never edits a shared body, so five lanes adding rows to this
// file do not conflict over the fake.
//
// A nil field answers with a typed CTX_INTERNAL, so a row that forgets to set
// the method it exercises fails loudly instead of silently seeing a zero value.
type fakeServices struct {
	indexFn       func(context.Context, model.IndexRequest) (model.IndexResult, error)
	refreshFn     func(context.Context, model.IndexRequest) (model.IndexResult, error)
	indexStatusFn func(context.Context) (model.IndexStatus, error)

	overviewFn   func(context.Context, model.OverviewRequest) (model.Page[model.OverviewItem], error)
	searchFn     func(context.Context, model.SearchRequest) (model.Page[model.SearchHit], error)
	symbolFn     func(context.Context, model.SymbolRequest) (model.Page[model.Node], error)
	referencesFn func(context.Context, model.ReferenceRequest) (model.Page[model.ReferenceOccurrence], error)
	graphFn      func(context.Context, model.GraphRequest) (model.GraphResult, error)
	pathFn       func(context.Context, model.PathRequest) (model.PathResult, error)
	// impactFn tracks ExploreService.Impact. Task 17 INT changes that method to
	// return (model.ImpactResult, error); when it lands, this field's type and
	// the Impact method below are the only two lines here that change.
	impactFn func(context.Context, model.ImpactRequest) (model.Page[model.ImpactEntry], error)

	planFn          func(context.Context, model.PlanRequest) (model.PlanResult, model.SessionStatus, error)
	sessionStatusFn func(context.Context, model.SessionRequest, model.PageRequest) (model.Page[model.FileCoverage], model.SessionStatus, error)
	nextFn          func(context.Context, model.SessionRequest) (model.NextContextItem, error)
	entriesFn       func(context.Context, model.ContextPageRequest) (model.ContextPage, error)
	includeFn       func(context.Context, model.IncludeRequest) (model.SessionStatus, error)
	readFn          func(context.Context, model.ReadChunkRequest) (model.ReadChunkResponse, error)
	acknowledgeFn   func(context.Context, model.AcknowledgeRequest) (model.SessionStatus, error)
	waiveFn         func(context.Context, model.WaiverRequest) (model.WaiverRecord, model.SessionStatus, error)
	recordFn        func(context.Context, model.ObservationRequest) (model.Observation, model.SessionStatus, error)
	advanceFn       func(context.Context, model.AdvanceRequest) (model.WorkflowStatus, model.SessionStatus, error)
	capsuleFn       func(context.Context, model.CapsuleRequest) (model.CapsulePage, error)
	exportFn        func(context.Context, model.SessionRequest) (model.Capsule, error)
	closeSessionFn  func(context.Context, model.SessionRequest, int) (model.SessionStatus, error)

	doctorFn func(context.Context, model.DoctorRequest) (model.DoctorReport, error)
}

var (
	_ app.IndexService    = (*fakeServices)(nil)
	_ app.ExploreService  = (*fakeServices)(nil)
	_ app.ContextService  = (*fakeServices)(nil)
	_ app.DiagnoseService = (*fakeServices)(nil)
)

// unset is what a nil function field answers with.
func unset(method string) error {
	return &model.Error{Code: model.CodeInternal, Message: "fake facade method " + method + " is not set by this row"}
}

func (f *fakeServices) Index(ctx context.Context, r model.IndexRequest) (model.IndexResult, error) {
	if f.indexFn == nil {
		return model.IndexResult{}, unset("Index")
	}
	return f.indexFn(ctx, r)
}

func (f *fakeServices) Refresh(ctx context.Context, r model.IndexRequest) (model.IndexResult, error) {
	if f.refreshFn == nil {
		return model.IndexResult{}, unset("Refresh")
	}
	return f.refreshFn(ctx, r)
}

func (f *fakeServices) IndexStatus(ctx context.Context) (model.IndexStatus, error) {
	if f.indexStatusFn == nil {
		return model.IndexStatus{}, unset("IndexStatus")
	}
	return f.indexStatusFn(ctx)
}

func (f *fakeServices) Overview(ctx context.Context, r model.OverviewRequest) (model.Page[model.OverviewItem], error) {
	if f.overviewFn == nil {
		return model.Page[model.OverviewItem]{}, unset("Overview")
	}
	return f.overviewFn(ctx, r)
}

func (f *fakeServices) Search(ctx context.Context, r model.SearchRequest) (model.Page[model.SearchHit], error) {
	if f.searchFn == nil {
		return model.Page[model.SearchHit]{}, unset("Search")
	}
	return f.searchFn(ctx, r)
}

func (f *fakeServices) Symbol(ctx context.Context, r model.SymbolRequest) (model.Page[model.Node], error) {
	if f.symbolFn == nil {
		return model.Page[model.Node]{}, unset("Symbol")
	}
	return f.symbolFn(ctx, r)
}

func (f *fakeServices) References(ctx context.Context, r model.ReferenceRequest) (model.Page[model.ReferenceOccurrence], error) {
	if f.referencesFn == nil {
		return model.Page[model.ReferenceOccurrence]{}, unset("References")
	}
	return f.referencesFn(ctx, r)
}

func (f *fakeServices) Graph(ctx context.Context, r model.GraphRequest) (model.GraphResult, error) {
	if f.graphFn == nil {
		return model.GraphResult{}, unset("Graph")
	}
	return f.graphFn(ctx, r)
}

func (f *fakeServices) Path(ctx context.Context, r model.PathRequest) (model.PathResult, error) {
	if f.pathFn == nil {
		return model.PathResult{}, unset("Path")
	}
	return f.pathFn(ctx, r)
}

func (f *fakeServices) Impact(ctx context.Context, r model.ImpactRequest) (model.Page[model.ImpactEntry], error) {
	if f.impactFn == nil {
		return model.Page[model.ImpactEntry]{}, unset("Impact")
	}
	return f.impactFn(ctx, r)
}

func (f *fakeServices) Plan(ctx context.Context, r model.PlanRequest) (model.PlanResult, model.SessionStatus, error) {
	if f.planFn == nil {
		return model.PlanResult{}, model.SessionStatus{}, unset("Plan")
	}
	return f.planFn(ctx, r)
}

func (f *fakeServices) SessionStatus(ctx context.Context, r model.SessionRequest, p model.PageRequest) (model.Page[model.FileCoverage], model.SessionStatus, error) {
	if f.sessionStatusFn == nil {
		return model.Page[model.FileCoverage]{}, model.SessionStatus{}, unset("SessionStatus")
	}
	return f.sessionStatusFn(ctx, r, p)
}

func (f *fakeServices) Next(ctx context.Context, r model.SessionRequest) (model.NextContextItem, error) {
	if f.nextFn == nil {
		return model.NextContextItem{}, unset("Next")
	}
	return f.nextFn(ctx, r)
}

func (f *fakeServices) Entries(ctx context.Context, r model.ContextPageRequest) (model.ContextPage, error) {
	if f.entriesFn == nil {
		return model.ContextPage{}, unset("Entries")
	}
	return f.entriesFn(ctx, r)
}

func (f *fakeServices) Include(ctx context.Context, r model.IncludeRequest) (model.SessionStatus, error) {
	if f.includeFn == nil {
		return model.SessionStatus{}, unset("Include")
	}
	return f.includeFn(ctx, r)
}

func (f *fakeServices) Read(ctx context.Context, r model.ReadChunkRequest) (model.ReadChunkResponse, error) {
	if f.readFn == nil {
		return model.ReadChunkResponse{}, unset("Read")
	}
	return f.readFn(ctx, r)
}

func (f *fakeServices) Acknowledge(ctx context.Context, r model.AcknowledgeRequest) (model.SessionStatus, error) {
	if f.acknowledgeFn == nil {
		return model.SessionStatus{}, unset("Acknowledge")
	}
	return f.acknowledgeFn(ctx, r)
}

func (f *fakeServices) Waive(ctx context.Context, r model.WaiverRequest) (model.WaiverRecord, model.SessionStatus, error) {
	if f.waiveFn == nil {
		return model.WaiverRecord{}, model.SessionStatus{}, unset("Waive")
	}
	return f.waiveFn(ctx, r)
}

func (f *fakeServices) Record(ctx context.Context, r model.ObservationRequest) (model.Observation, model.SessionStatus, error) {
	if f.recordFn == nil {
		return model.Observation{}, model.SessionStatus{}, unset("Record")
	}
	return f.recordFn(ctx, r)
}

func (f *fakeServices) Advance(ctx context.Context, r model.AdvanceRequest) (model.WorkflowStatus, model.SessionStatus, error) {
	if f.advanceFn == nil {
		return model.WorkflowStatus{}, model.SessionStatus{}, unset("Advance")
	}
	return f.advanceFn(ctx, r)
}

func (f *fakeServices) Capsule(ctx context.Context, r model.CapsuleRequest) (model.CapsulePage, error) {
	if f.capsuleFn == nil {
		return model.CapsulePage{}, unset("Capsule")
	}
	return f.capsuleFn(ctx, r)
}

func (f *fakeServices) Export(ctx context.Context, r model.SessionRequest) (model.Capsule, error) {
	if f.exportFn == nil {
		return model.Capsule{}, unset("Export")
	}
	return f.exportFn(ctx, r)
}

func (f *fakeServices) CloseSession(ctx context.Context, r model.SessionRequest, v int) (model.SessionStatus, error) {
	if f.closeSessionFn == nil {
		return model.SessionStatus{}, unset("CloseSession")
	}
	return f.closeSessionFn(ctx, r, v)
}

func (f *fakeServices) Doctor(ctx context.Context, r model.DoctorRequest) (model.DoctorReport, error) {
	if f.doctorFn == nil {
		return model.DoctorReport{}, unset("Doctor")
	}
	return f.doctorFn(ctx, r)
}

// ---------------------------------------------------------------------------
// The in-memory client harness.
// ---------------------------------------------------------------------------

// testSchemaVersion is the build schema version every row asserts against. It
// is deliberately not a real release value: a row that passes with the empty
// string would not prove the envelope carries the build's version at all.
const testSchemaVersion = "test-schema-1"

// newTestHandlers builds the handlers seam directly. It does not call New:
// New and Options are L1's, and building the server here keeps the whole test
// file independent of L1's landing.
func newTestHandlers(f *fakeServices) *handlers {
	return &handlers{
		index:    f,
		explore:  f,
		context:  f,
		diagnose: f,
		cfg:      config.Defaults(),
		build:    model.BuildInfo{Version: "0.0.0-test", SchemaVersion: testSchemaVersion},
		log:      slog.New(slog.DiscardHandler),
	}
}

// newTestServer registers all 23 tools over the given fake. register panics if
// jsonschema inference fails for any In or Out type, so simply reaching the
// return statement is the reflection smoke test for the whole surface.
func newTestServer(f *fakeServices) *mcp.Server {
	s := mcp.NewServer(&mcp.Implementation{Name: "codectx", Version: "0.0.0-test"}, nil)
	register(s, newTestHandlers(f))
	return s
}

// connect runs a real mcp.Client against the server over the SDK's in-memory
// transports — a real session, not a handler called directly, so the schema
// validation, the envelope marshaling and the tool-error packing are all on the
// path every row exercises. It returns the client session; the caller gets
// clean shutdown at test end.
func connect(t *testing.T, s *mcp.Server) *mcp.ClientSession {
	t.Helper()
	ctx := t.Context()
	clientT, serverT := mcp.NewInMemoryTransports()
	serverSession, err := s.Connect(ctx, serverT, nil)
	if err != nil {
		t.Fatalf("server connect: %v", err)
	}
	c := mcp.NewClient(&mcp.Implementation{Name: "codectx-test", Version: "0.0.0-test"}, nil)
	cs, err := c.Connect(ctx, clientT, nil)
	if err != nil {
		t.Fatalf("client connect: %v", err)
	}
	t.Cleanup(func() {
		_ = cs.Close()
		_ = serverSession.Wait()
	})
	return cs
}

// ---------------------------------------------------------------------------
// The schema snapshot.
// ---------------------------------------------------------------------------

// toolSchemas is the frozen Section 19.2 surface: every tool name and the
// input fields its schema marks required.
//
// What it protects: a tool silently disappearing from the registry, a new one
// appearing unreviewed, and a required argument quietly becoming optional.
// Required-ness here is inferred solely from the absence of omitempty on the
// model request type, so dropping a tag on a landed model field would relax the
// wire contract of a tool in a diff that never mentions this package. That is
// exactly the silent change a snapshot exists to catch.
//
// codectx_index_status takes no arguments, so it requires none.
var toolSchemas = map[string][]string{
	"codectx_index_status":        nil,
	"codectx_refresh_index":       {"full", "rebuild"},
	"codectx_repo_overview":       {"depth", "page"},
	"codectx_search":              {"query", "page"},
	"codectx_find_symbol":         {"query", "operation", "semantic_source", "page"},
	"codectx_symbol_info":         {"query", "semantic_source", "page"},
	"codectx_references":          {"node_id", "operation", "semantic_source", "page"},
	"codectx_callers":             {"start", "max_depth", "max_visited", "max_edges", "page"},
	"codectx_callees":             {"start", "max_depth", "max_visited", "max_edges", "page"},
	"codectx_dependency_path":     {"from", "to", "max_depth", "max_visited"},
	"codectx_impact":              {"start", "direction", "max_depth", "max_visited", "max_edges", "page"},
	"codectx_context_plan":        {"context", "actor_id"},
	"codectx_context_status":      {"session_id", "actor_id", "page"},
	"codectx_context_next":        {"session_id", "actor_id"},
	"codectx_context_entries":     {"session_id", "actor_id", "view", "page"},
	"codectx_context_include":     {"session_id", "actor_id", "seeds", "expected_version"},
	"codectx_read_source":         {"session_id", "actor_id", "file_id", "offset", "max_bytes"},
	"codectx_context_acknowledge": {"session_id", "actor_id", "kind"},
	"codectx_context_waive":       {"session_id", "actor_id", "file_id", "reason"},
	"codectx_context_record":      {"session_id", "actor_id", "expected_scope_version", "kind", "note"},
	"codectx_context_advance":     {"session_id", "actor_id", "target", "expected_version"},
	"codectx_context_capsule":     {"session_id", "actor_id", "view", "page"},
	"codectx_context_close":       {"session_id", "actor_id", "expected_version"},
}

// toolEnums is the other half of the snapshot: the enum presets registry.go
// attaches, without which Section 19.3's enum validation would be missing from
// every schema, because jsonschema-go infers a bare "string" for a named string
// type. Two representative tools are pinned rather than all ten enum types: one
// where the table must not cross-contaminate a same-named field on another tool
// (operation is SymbolOperation here and ReferenceOperation on
// codectx_references), and the capsule view, whose wire vocabulary is the six
// model.CapsuleView values PLUS the "export" spelling the model type does not
// carry.
var toolEnums = map[string]map[string][]string{
	"codectx_find_symbol": {
		"operation":       {"resolve", "document-symbols", "workspace-symbols", "definition"},
		"semantic_source": {"canonical", "lsp"},
	},
	"codectx_references": {
		"operation": {"references", "implements", "type-definition"},
	},
	"codectx_context_capsule": {
		"view": {"accepted_facts", "rejected_facts", "contradictions", "unresolved",
			"coverage", "waivers", "export"},
	},
	// A NESTED enum: model.Phase occurs only inside PlanRequest.Context, one
	// level down. It is the sole coverage of the recursion, which every enum
	// type would silently lose if the preset only reached top-level fields.
	"codectx_context_plan": {
		"context.phase": {"sweep", "verify", "consolidate"},
	},
}

// schemaShape is the decoded form of a listed tool's input schema. It is
// recursive because the enum table must reach nested objects too: model.Phase
// occurs ONLY at depth 1 (PlanRequest.Context.Phase), which is a different path
// through jsonschema-go's reflection than a top-level field.
type schemaShape struct {
	Required   []string                `json:"required"`
	Properties map[string]*schemaShape `json:"properties"`
	Enum       []string                `json:"enum"`
}

func TestToolSchemaSnapshot(t *testing.T) {
	cs := connect(t, newTestServer(&fakeServices{}))

	seen := map[string]schemaShape{}
	for tool, err := range cs.Tools(t.Context(), nil) {
		if err != nil {
			t.Fatalf("tools/list: %v", err)
		}
		if tool.Title == "" || tool.Description == "" {
			t.Errorf("tool %q: Title and Description are required by Section 19.2", tool.Name)
		}
		raw, err := json.Marshal(tool.InputSchema)
		if err != nil {
			t.Fatalf("tool %q: marshal input schema: %v", tool.Name, err)
		}
		var shape schemaShape
		if err := json.Unmarshal(raw, &shape); err != nil {
			t.Fatalf("tool %q: decode input schema: %v", tool.Name, err)
		}
		seen[tool.Name] = shape
	}

	if len(seen) != toolCount {
		t.Errorf("tools/list returned %d tools, want %d", len(seen), toolCount)
	}
	for name, want := range toolSchemas {
		shape, listed := seen[name]
		if !listed {
			t.Errorf("tool %q is registered nowhere", name)
			continue
		}
		if !equalStrings(shape.Required, want) {
			t.Errorf("tool %q required fields = %v, want %v", name, shape.Required, want)
		}
	}
	for name := range seen {
		if _, expected := toolSchemas[name]; !expected {
			t.Errorf("tool %q is registered but not in the snapshot", name)
		}
	}
	for name, fields := range toolEnums {
		for path, want := range fields {
			got := enumAt(seen[name], path)
			if !equalStrings(got, want) {
				t.Errorf("tool %q field %q enum = %v, want %v", name, path, got, want)
			}
		}
	}
}

// enumAt walks a dotted property path and returns the enum it finds, or nil.
func enumAt(shape schemaShape, path string) []string {
	cur := &shape
	for _, field := range strings.Split(path, ".") {
		next := cur.Properties[field]
		if next == nil {
			return nil
		}
		cur = next
	}
	return cur.Enum
}

func equalStrings(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

// ---------------------------------------------------------------------------
// The scenario table.
// ---------------------------------------------------------------------------

// scenario is one end-to-end tool call over a real client session.
//
// facade configures the deterministic seam for this row only; check inspects
// what actually came back over the transport. Rows live under their lane's
// marker and nowhere else, so five lanes extend this table without conflicting.
type scenario struct {
	name   string
	facade func(*fakeServices)
	tool   string
	args   any
	check  func(*testing.T, *mcp.CallToolResult)
}

var scenarios = []scenario{
	// L0 row.
	{
		// Failure mode: the one fully worked tool stops round-tripping — the
		// envelope loses the build's schema version, warnings serializes as
		// null instead of [], or the facade's answer does not survive
		// StructuredContent. Every other lane's handler copies this shape, so
		// this row failing means all 23 are suspect.
		name: "index_status round-trips the shared envelope",
		facade: func(f *fakeServices) {
			f.indexStatusFn = func(context.Context) (model.IndexStatus, error) {
				return model.IndexStatus{FileCount: 7, SourceBytes: 1024, WatchActive: true}, nil
			}
		},
		tool: "codectx_index_status",
		args: emptyInput{},
		check: func(t *testing.T, res *mcp.CallToolResult) {
			if res.IsError {
				t.Fatalf("index_status reported a tool error: %s", firstText(res))
			}
			var got result[model.IndexStatus]
			decode(t, res, &got)
			if got.SchemaVersion != testSchemaVersion {
				t.Errorf("schema_version = %q, want %q", got.SchemaVersion, testSchemaVersion)
			}
			if got.Warnings == nil || len(got.Warnings) != 0 {
				t.Errorf("warnings = %#v, want an empty non-nil slice", got.Warnings)
			}
			if got.Data.FileCount != 7 || got.Data.SourceBytes != 1024 || !got.Data.WatchActive {
				t.Errorf("data = %+v, want the facade's answer verbatim", got.Data)
			}
		},
	},

	// L1 rows.

	// L2 rows.

	// L3 rows.
	{
		// Failure mode: symbol_info's two facade calls straddle a generation
		// change, so the evidence describes a different snapshot than the
		// metadata beside it and a model reasons about a symbol using
		// occurrences that no longer refer to it. The handler threads the
		// generation the FIRST answer pinned into the second request; nothing
		// else in the package can catch it being dropped, because passing the
		// caller's own generation_id through instead compiles and looks right.
		//
		// The fake is deliberately asymmetric so the row cannot pass with the
		// threading removed: the input names no generation, Symbol answers
		// pinned to 41, and References echoes back whatever generation it was
		// handed. Threaded => 41; unthreaded => 0.
		name: "symbol_info evidence is pinned to the generation its metadata came from",
		facade: func(f *fakeServices) {
			f.symbolFn = func(_ context.Context, r model.SymbolRequest) (model.Page[model.Node], error) {
				return model.Page[model.Node]{
					Meta:  model.QueryMeta{Binding: model.Binding{GenerationID: 41}},
					Items: []model.Node{{ID: model.NodeID(strings.Repeat("a", 64)), Name: r.Query}},
				}, nil
			}
			f.referencesFn = func(_ context.Context, r model.ReferenceRequest) (model.Page[model.ReferenceOccurrence], error) {
				return model.Page[model.ReferenceOccurrence]{
					Meta:  model.QueryMeta{Binding: model.Binding{GenerationID: r.GenerationID}},
					Items: []model.ReferenceOccurrence{},
				}, nil
			}
		},
		tool: "codectx_symbol_info",
		args: symbolInfoInput{
			Query:          "Parse",
			SemanticSource: model.SemanticCanonical,
			Page:           model.PageRequest{Limit: 10},
		},
		check: func(t *testing.T, res *mcp.CallToolResult) {
			if res.IsError {
				t.Fatalf("symbol_info reported a tool error: %s", firstText(res))
			}
			var got result[symbolInfoOutput]
			decode(t, res, &got)
			symbolGen := got.Data.Symbol.Meta.Binding.GenerationID
			evidenceGen := got.Data.Evidence.Meta.Binding.GenerationID
			if symbolGen != 41 {
				t.Fatalf("symbol generation_id = %d, want 41", symbolGen)
			}
			if evidenceGen != symbolGen {
				t.Errorf("evidence generation_id = %d, want the symbol's %d", evidenceGen, symbolGen)
			}
		},
	},

	// L4 rows.

	// L5 rows.
}

func TestScenarios(t *testing.T) {
	for _, sc := range scenarios {
		t.Run(sc.name, func(t *testing.T) {
			f := &fakeServices{}
			if sc.facade != nil {
				sc.facade(f)
			}
			cs := connect(t, newTestServer(f))
			res, err := cs.CallTool(t.Context(), &mcp.CallToolParams{
				Name:      sc.tool,
				Arguments: sc.args,
			})
			if err != nil {
				t.Fatalf("tools/call %s raised a protocol error: %v", sc.tool, err)
			}
			sc.check(t, res)
		})
	}
}

// decode reads a successful call's StructuredContent into v.
func decode(t *testing.T, res *mcp.CallToolResult, v any) {
	t.Helper()
	if res.StructuredContent == nil {
		t.Fatalf("call returned no structured content; text was %q", firstText(res))
	}
	// On the client side StructuredContent is the decoded JSON value, so it is
	// re-marshaled to reach the typed envelope.
	raw, err := json.Marshal(res.StructuredContent)
	if err != nil {
		t.Fatalf("re-marshal structured content: %v", err)
	}
	if err := json.Unmarshal(raw, v); err != nil {
		t.Fatalf("decode structured content: %v", err)
	}
}

// firstText returns the first text content block, which is where the SDK puts a
// tool error's message.
func firstText(res *mcp.CallToolResult) string {
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			return tc.Text
		}
	}
	return ""
}
