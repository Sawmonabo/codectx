package mcpserver

import (
	"context"
	"encoding/json"
	"errors"
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
	indexStatusFn func(context.Context, model.StatusRequest) (model.IndexStatus, error)

	overviewFn   func(context.Context, model.OverviewRequest) (model.Page[model.OverviewItem], error)
	searchFn     func(context.Context, model.SearchRequest) (model.Page[model.SearchHit], error)
	symbolFn     func(context.Context, model.SymbolRequest) (model.Page[model.Node], error)
	referencesFn func(context.Context, model.ReferenceRequest) (model.Page[model.ReferenceOccurrence], error)
	graphFn      func(context.Context, model.GraphRequest) (model.GraphResult, error)
	pathFn       func(context.Context, model.PathRequest) (model.PathResult, error)
	// impactFn tracks ExploreService.Impact, which returns model.ImpactResult
	// whole (Task 17 INT).
	impactFn func(context.Context, model.ImpactRequest) (model.ImpactResult, error)

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
}

var (
	_ app.IndexService   = (*fakeServices)(nil)
	_ app.ExploreService = (*fakeServices)(nil)
	_ app.ContextService = (*fakeServices)(nil)
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

func (f *fakeServices) IndexStatus(ctx context.Context, r model.StatusRequest) (model.IndexStatus, error) {
	if f.indexStatusFn == nil {
		return model.IndexStatus{}, unset("IndexStatus")
	}
	return f.indexStatusFn(ctx, r)
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

func (f *fakeServices) Impact(ctx context.Context, r model.ImpactRequest) (model.ImpactResult, error) {
	if f.impactFn == nil {
		return model.ImpactResult{}, unset("Impact")
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

// CapsuleRows is on app.ContextService for the CLI's whole-capsule export. No
// tool reaches it -- codectx_context_capsule pages through Capsule and projects
// Export -- so the fake refuses it rather than answering a page a tool would
// then be believed to serve.
func (f *fakeServices) CapsuleRows(_ context.Context, _ model.SessionRequest, _ model.CapsuleList,
	_ string, _ int) ([]model.CapsuleRow, string, error) {
	return nil, "", unset("CapsuleRows")
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
		index:   f,
		explore: f,
		context: f,
		cfg:     config.Defaults(),
		build:   model.BuildInfo{Version: "0.0.0-test", SchemaVersion: testSchemaVersion},
		log:     slog.New(slog.DiscardHandler),
	}
}

// newTestServer registers all 23 tools over the given fake. register panics if
// jsonschema inference fails for any In or Out type, so simply reaching the
// return statement is the reflection smoke test for the whole surface.
func newTestServer(f *fakeServices) *mcp.Server {
	// Built through New, never by hand: New is where limitMiddleware is
	// installed, so a server assembled here directly would certify a wiring
	// nobody ships and no bound could ever be exercised over the transport.
	h := newTestHandlers(f)
	s, err := New(Options{
		Index:   h.index,
		Explore: h.explore,
		Context: h.context,
		Config:  h.cfg,
		Build:   h.build,
		Logger:  h.log,
	})
	if err != nil {
		panic(err)
	}
	return s.mcp
}

// connect runs a real mcp.Client against the server over the SDK's in-memory
// transports — a real session, not a handler called directly, so the schema
// validation, the envelope marshaling and the tool-error packing are all on the
// path every row exercises. It returns the client session; the caller gets
// clean shutdown at test end.
func connect(t *testing.T, s *mcp.Server) *mcp.ClientSession {
	t.Helper()
	return connectSession(t, s, true)
}

// connectSession is connect with the shutdown assertion made optional.
//
// wantClean is false for exactly one caller: the row that abandons a call
// mid-flight. That row's client tears the in-memory pipe down while the server
// is still handling the cancellation it just sent, so whether the server's read
// side observes a clean EOF or a closed pipe is a transport race, not a
// property of the server. Asserting there would make the suite flaky; the
// invariant is asserted on every other session instead.
func connectSession(t *testing.T, s *mcp.Server, wantClean bool) *mcp.ClientSession {
	t.Helper()
	ctx := t.Context()
	clientT, serverT := mcp.NewInMemoryTransports()
	// The server session outlives t.Context(), which is canceled just BEFORE
	// Cleanup runs: under that context Wait would report the harness teardown
	// rather than the session shutdown the assertion below is about.
	serverSession, err := s.Connect(context.WithoutCancel(ctx), serverT, nil)
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
		// Failure mode: the session stops ending cleanly on client
		// disconnect -- a handler left running, a transport not drained --
		// and the server keeps a workspace it no longer serves. Wait returns
		// the session's own outcome, so discarding it is how a dirty shutdown
		// goes unnoticed in-package.
		if err := serverSession.Wait(); err != nil && wantClean {
			t.Errorf("server session did not shut down cleanly: %v", err)
		}
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
// codectx_refresh_index takes no arguments, so it
// require none.
var toolSchemas = map[string][]string{
	"codectx_index_status":        {"resources"},
	"codectx_refresh_index":       nil,
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

	// wantCallErr says this row expects CallTool ITSELF to fail rather than
	// return a result. Every other row keeps the fatal-on-unexpected rule: a
	// protocol error is a defect unless the row is about one. check is not
	// called for such a row -- there is no result to inspect.
	wantCallErr bool
	// callCtx replaces the context the call is made with, and is where a row
	// that needs the call abandoned mid-flight arranges it. It is handed the
	// fake so the handler that must be running when the cancellation arrives
	// can be the thing that triggers it, which is what makes the row
	// deterministic instead of timing-dependent.
	callCtx func(*testing.T, *fakeServices) context.Context
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
			f.indexStatusFn = func(_ context.Context, r model.StatusRequest) (model.IndexStatus, error) {
				// The request reaches the facade as sent: ruling Q1 makes the
				// resource block a field of this one answer, and a handler that
				// dropped the flag would report "not measured" for a block the
				// caller explicitly asked for.
				if !r.Resources {
					return model.IndexStatus{}, unset("IndexStatus: resources was not forwarded")
				}
				return model.IndexStatus{FileCount: 7, SourceBytes: 1024, WatchActive: true}, nil
			}
		},
		tool: "codectx_index_status",
		args: model.StatusRequest{Resources: true},
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
	oversizedArgumentsRow(),

	{
		// Failure mode: a client that goes away mid-call stops being an
		// observable outcome. The SDK answers a canceled tools/call with
		// (nil, context canceled) -- no CallToolResult at all -- so a caller
		// that only ever inspects a result cannot tell an abandoned call from
		// one that never happened, and the middleware's own bounded waits
		// (limits.acquire, the per-call deadline) hang off the same ctx. This
		// row pins the SDK's ACTUAL behaviour, observed rather than presumed.
		name: "a canceled tools/call reports the cancellation, not a result",
		tool: "codectx_index_status",
		args: model.StatusRequest{},
		callCtx: func(_ *testing.T, f *fakeServices) context.Context {
			ctx, cancel := context.WithCancel(context.Background())
			f.indexStatusFn = func(handlerCtx context.Context, _ model.StatusRequest) (model.IndexStatus, error) {
				// The call is abandoned while this handler is the one running,
				// and the handler waits for the cancellation to reach it, so
				// neither side of the transport races the other.
				cancel()
				<-handlerCtx.Done()
				return model.IndexStatus{}, handlerCtx.Err()
			}
			return ctx
		},
		wantCallErr: true,
	},

	// L2 rows.
	{
		// Failure mode: an LSP overlay answer reaches the model looking like a
		// sealed canonical fact. Two ways that happens, both guarded here: the
		// handler rewrites semantic_source/profile on the way in, so the facade
		// silently answers a live-source question with index facts and the LSP
		// route becomes unreachable; or the handler rebuilds the page and drops
		// QueryMeta.Overlay, so the ephemeral dirty-worktree label of Section
		// 11.6 / 19.3 is lost in serialization. Also asserts the answer carries
		// no source bytes: only codectx_read_source may return those.
		name: "find_symbol labels an lsp answer and returns no source bytes",
		facade: func(f *fakeServices) {
			f.symbolFn = func(_ context.Context, r model.SymbolRequest) (model.Page[model.Node], error) {
				if r.SemanticSource != model.SemanticLSP || r.Profile != "gopls" {
					return model.Page[model.Node]{}, &model.Error{
						Code:    model.CodeArgumentInvalid,
						Message: "handler altered the semantic route: " + string(r.SemanticSource) + "/" + r.Profile,
					}
				}
				return model.Page[model.Node]{
					Meta: model.QueryMeta{Overlay: &model.OverlayBinding{
						ProviderID:      "gopls",
						ProviderVersion: "1.2.3",
						InputDigest:     "d1",
					}},
					Items: []model.Node{{
						Kind:   model.NodeFunction,
						Name:   "Serve",
						FileID: "f1",
						Range: &model.SourceRange{
							Start: model.Position{Byte: 0, Line: 1, Column: 1},
							End:   model.Position{Byte: 10, Line: 2, Column: 1},
						},
						// Non-empty Metadata is load-bearing, not decoration:
						// it is json.RawMessage, so without registry.go's
						// output-schema preset the SDK validates this object
						// against an inferred ["null","array"] and turns the
						// whole answer into a JSON-RPC protocol error. An empty
						// Metadata is dropped by omitempty and never reaches
						// that check. The LSP overlay really does populate it
						// (app/overlay.go's container).
						Metadata:       json.RawMessage(`{"container":"internal/mcpserver"}`),
						SemanticSource: model.SemanticLSP,
					}},
				}, nil
			}
		},
		tool: "codectx_find_symbol",
		args: model.SymbolRequest{
			Query:          "Serve",
			Operation:      model.SymbolResolve,
			SemanticSource: model.SemanticLSP,
			Profile:        "gopls",
			Page:           model.PageRequest{Limit: 10},
		},
		check: func(t *testing.T, res *mcp.CallToolResult) {
			if res.IsError {
				t.Fatalf("find_symbol reported a tool error: %s", firstText(res))
			}
			var got result[model.Page[model.Node]]
			decode(t, res, &got)
			if got.Data.Meta.Overlay == nil {
				t.Fatalf("meta.overlay is nil; an lsp answer reached the client unlabelled")
			}
			if got.Data.Meta.Overlay.ProviderID != "gopls" || got.Data.Meta.Overlay.ProviderVersion != "1.2.3" {
				t.Errorf("meta.overlay = %+v, want the facade's label verbatim", *got.Data.Meta.Overlay)
			}
			if len(got.Data.Items) != 1 || got.Data.Items[0].SemanticSource != model.SemanticLSP {
				t.Fatalf("items = %+v, want one node labelled lsp", got.Data.Items)
			}
			if string(got.Data.Items[0].Metadata) != `{"container":"internal/mcpserver"}` {
				t.Errorf("items[0].metadata = %s, want the facade's raw JSON verbatim",
					got.Data.Items[0].Metadata)
			}
			// model.Node carries no body today; this keeps it that way by
			// failing the moment a source-bearing field (ReadChunkResponse's
			// "content") appears on this tool's wire answer.
			raw, err := json.Marshal(res.StructuredContent)
			if err != nil {
				t.Fatalf("marshal structured content: %v", err)
			}
			if strings.Contains(string(raw), `"content"`) {
				t.Errorf("find_symbol answer carries source bytes: %s", raw)
			}
		},
	},
	{
		// Failure mode: a not-yet-implemented producer is laundered into "no
		// results". Overview has no producer this wave and refuses with a typed
		// *model.Error; a handler that swallowed it and returned an empty page
		// would leave the model unable to tell "this repository has no
		// packages" from "this capability does not exist yet" — the silent
		// capability reduction Section 30.1 forbids. The fixture code is
		// deliberately NOT CTX_INTERNAL, so a handler that dropped the typed
		// error and returned an untyped one (which toolFailure reduces to
		// CTX_INTERNAL) fails this row too. Asserted on IsError + text: SDK
		// v1.7.0 leaves StructuredContent empty on the error path.
		name: "repo_overview surfaces a refusing Overview as a tool error",
		facade: func(f *fakeServices) {
			f.overviewFn = func(context.Context, model.OverviewRequest) (model.Page[model.OverviewItem], error) {
				// The producer now exists, so the refusal this row drives is a
				// real one a live workspace can answer with: no generation has
				// been published yet. The invariant is unchanged and is not
				// about which refusal it is -- an empty page must never stand
				// in for one.
				return model.Page[model.OverviewItem]{}, &model.Error{
					Code:        model.CodeNoActiveGeneration,
					Message:     "this repository has no active generation",
					Remediation: "run codectx index first",
				}
			}
		},
		tool: "codectx_repo_overview",
		args: model.OverviewRequest{Depth: 1, Page: model.PageRequest{Limit: 10}},
		check: func(t *testing.T, res *mcp.CallToolResult) {
			if !res.IsError {
				t.Fatalf("repo_overview returned a success envelope for a refusing facade: %#v", res.StructuredContent)
			}
			text := firstText(res)
			if !strings.Contains(text, model.CodeNoActiveGeneration) {
				t.Errorf("tool error text = %q, want the typed code %s", text, model.CodeNoActiveGeneration)
			}
			if !strings.Contains(text, "run codectx index first") {
				t.Errorf("tool error text = %q, want the facade's remediation", text)
			}
		},
	},

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
	//
	// Both rows are wrapped in a func literal so the row's fixtures and its
	// actor-aware facade live in one scope: the first row needs the SAME facade
	// for two sessions, and package-level helpers would collide with the other
	// lanes appending at their own markers.
	func() scenario {
		const (
			session  = model.SessionID("a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1")
			file     = model.FileID("b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2")
			owner    = "actor-alpha"
			receipt  = "receipt-token-for-actor-alpha"
			source   = "package model // SENTINEL SOURCE BYTES"
			intruder = "actor-beta"
		)
		// read models the coverage service: it issues bytes and a receipt to the
		// session's owning actor and refuses anyone else with the Section 22 code.
		read := func(_ context.Context, r model.ReadChunkRequest) (model.ReadChunkResponse, error) {
			if r.ActorID != owner {
				return model.ReadChunkResponse{}, &model.Error{
					Code:        model.CodeActorMismatch,
					Message:     "this session belongs to another actor",
					Remediation: "plan a session for this actor",
				}
			}
			return model.ReadChunkResponse{
				FileID:    r.FileID,
				ByteRange: model.ByteRange{End: uint64(len(source))},
				Encoding:  model.EncodingUTF8,
				Content:   source,
				Receipt:   receipt,
			}, nil
		}
		return scenario{
			// Failure mode: actor isolation is lost at the wire. Either the receipt
			// that proves bytes were issued does not survive the envelope, which
			// makes coverage unprovable and every acknowledgement a guess, or a
			// second actor's refusal arrives as something other than a tool error:
			// a CTX_ACTOR_MISMATCH raised as a PROTOCOL error aborts the call with
			// no code the model can read, and a refusal that still carried an
			// envelope would hand a foreign actor the session's source bytes.
			name:   "read_source echoes the receipt and denies a second actor",
			facade: func(f *fakeServices) { f.readFn = read },
			tool:   "codectx_read_source",
			args: model.ReadChunkRequest{
				SessionID: session,
				ActorID:   owner,
				FileID:    file,
				MaxBytes:  4096,
			},
			check: func(t *testing.T, res *mcp.CallToolResult) {
				if res.IsError {
					t.Fatalf("the owning actor's read reported a tool error: %s", firstText(res))
				}
				var got result[model.ReadChunkResponse]
				decode(t, res, &got)
				if got.Data.Receipt != receipt {
					t.Errorf("receipt = %q, want the issued receipt %q echoed verbatim", got.Data.Receipt, receipt)
				}
				if got.Data.Content != source {
					t.Errorf("content = %q, want the issued chunk %q", got.Data.Content, source)
				}

				// The same facade, a second actor, replaying the receipt it just saw.
				second := connect(t, newTestServer(&fakeServices{readFn: read}))
				denied, err := second.CallTool(t.Context(), &mcp.CallToolParams{
					Name: "codectx_read_source",
					Arguments: model.ReadChunkRequest{
						SessionID:       session,
						ActorID:         intruder,
						FileID:          file,
						MaxBytes:        4096,
						ConfirmReceipts: []string{receipt},
					},
				})
				if err != nil {
					t.Fatalf("the second actor's read raised a protocol error, want a tool error: %v", err)
				}
				if !denied.IsError {
					t.Fatalf("the second actor's read succeeded; want a tool error")
				}
				text := firstText(denied)
				if !strings.Contains(text, model.CodeActorMismatch) {
					t.Errorf("denial text = %q, want it to carry %s", text, model.CodeActorMismatch)
				}
				if strings.Contains(text, source) {
					t.Errorf("the denial leaked source bytes: %q", text)
				}
				if denied.StructuredContent != nil {
					t.Errorf("the denial carried structured content %#v, want none", denied.StructuredContent)
				}
			},
		}
	}(),
	func() scenario {
		const (
			session = model.SessionID("c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3")
			file    = model.FileID("d4d4d4d4d4d4d4d4d4d4d4d4d4d4d4d4d4d4d4d4d4d4d4d4d4d4d4d4d4d4d4d4")
			source  = "func Secret() {} // SENTINEL SOURCE BYTES"
		)
		return scenario{
			// Failure mode: codectx_context_next starts carrying source bytes.
			// Section 19.2 marks it metadata only and codectx_read_source is the one
			// tool that may return a body, because every issued byte must travel
			// with a receipt coverage can later require. A next answer that smuggles
			// content — in the item, in a warning, anywhere in the envelope — serves
			// source outside the receipt path and silently voids the coverage gate.
			//
			// readFn is set so the sentinel is genuinely reachable from this
			// handler's facade; without it the assertion could not fail and would
			// prove nothing.
			name: "context_next returns metadata only, no source bytes",
			facade: func(f *fakeServices) {
				f.readFn = func(_ context.Context, r model.ReadChunkRequest) (model.ReadChunkResponse, error) {
					return model.ReadChunkResponse{FileID: r.FileID, Encoding: model.EncodingUTF8, Content: source, Receipt: "receipt"}, nil
				}
				f.nextFn = func(context.Context, model.SessionRequest) (model.NextContextItem, error) {
					return model.NextContextItem{
						Action:    "read",
						FileID:    file,
						Path:      "internal/model/context.go",
						Size:      int64(len(source)),
						Remaining: 3,
					}, nil
				}
			},
			tool: "codectx_context_next",
			args: model.SessionRequest{SessionID: session, ActorID: "actor-alpha"},
			check: func(t *testing.T, res *mcp.CallToolResult) {
				if res.IsError {
					t.Fatalf("context_next reported a tool error: %s", firstText(res))
				}
				var got result[model.NextContextItem]
				decode(t, res, &got)
				if got.Data.Path != "internal/model/context.go" || got.Data.Size != int64(len(source)) {
					t.Errorf("data = %+v, want the facade's metadata verbatim", got.Data)
				}
				raw, err := json.Marshal(res.StructuredContent)
				if err != nil {
					t.Fatalf("re-marshal structured content: %v", err)
				}
				if strings.Contains(string(raw), source) {
					t.Fatalf("context_next carried source bytes: %s", raw)
				}
			},
		}
	}(),

	// L5 rows.
	{
		// Failure mode: a waived session is reported ready to implement. A
		// waiver is an audit record and never grants strict readiness
		// (Section 16.3, and SessionStatus.Validate refuses the combination),
		// so the honest verdict lives in the status the gate returns. If the
		// handler substituted its own status, dropped it from waiveOutput, or
		// the two readiness booleans collapsed into one on the wire, an agent
		// would be told to implement against a required file nobody read.
		name: "gate: waive keeps ready_for_implementation false over the wire",
		facade: func(f *fakeServices) {
			f.waiveFn = func(_ context.Context, r model.WaiverRequest) (model.WaiverRecord, model.SessionStatus, error) {
				waiver := model.WaiverRecord{
					SessionID:   r.SessionID,
					ActorID:     r.ActorID,
					FileID:      r.FileID,
					ContentHash: "content-hash-of-the-waived-file",
					Reason:      r.Reason,
				}
				status := model.SessionStatus{
					SessionID:               r.SessionID,
					ActorID:                 r.ActorID,
					ReadCompleteForSnapshot: true,
					ReadyForImplementation:  false,
					StrictGateSatisfied:     false,
					RequiredFiles:           3,
					FullyServedFiles:        2,
					WaivedFiles:             1,
				}
				return waiver, status, nil
			}
		},
		tool: "codectx_context_waive",
		args: model.WaiverRequest{
			SessionID: model.SessionID(strings.Repeat("a", 64)),
			ActorID:   "actor-1",
			FileID:    model.FileID(strings.Repeat("b", 64)),
			Reason:    "vendored third-party source",
		},
		check: func(t *testing.T, res *mcp.CallToolResult) {
			if res.IsError {
				t.Fatalf("context_waive reported a tool error: %s", firstText(res))
			}
			var got result[waiveOutput]
			decode(t, res, &got)
			if string(got.Data.Waiver.FileID) != strings.Repeat("b", 64) || got.Data.Waiver.Reason != "vendored third-party source" {
				t.Errorf("waiver = %+v, want the facade's record verbatim", got.Data.Waiver)
			}
			// ReadCompleteForSnapshot pins the row against a vacuous pass: a
			// zero-valued or dropped status would also report
			// ready_for_implementation=false.
			if !got.Data.Status.ReadCompleteForSnapshot {
				t.Fatalf("status = %+v, want the facade's status verbatim", got.Data.Status)
			}
			if got.Data.Status.WaivedFiles != 1 {
				t.Errorf("waived_files = %d, want 1", got.Data.Status.WaivedFiles)
			}
			if got.Data.Status.ReadyForImplementation || got.Data.Status.StrictGateSatisfied {
				t.Errorf("a waived session reported ready_for_implementation=%v strict_gate_satisfied=%v, want both false",
					got.Data.Status.ReadyForImplementation, got.Data.Status.StrictGateSatisfied)
			}
		},
	},
	func() scenario {
		// Failure mode: view="export" answers with capsule RECORDS. A sealed
		// capsule now carries only identity and its eight per-list counts --
		// the records are rows read one keyset page at a time -- so the failure
		// this row still guards is the projection: an export that omits a
		// list's count, or that also returns a page, makes a caller believe it
		// has been told how much the capsule holds when it has not.
		//
		// The row is a closure so the fixture capsule is shared by facade and
		// check without a package-level helper another lane would collide on.
		sessionID := model.SessionID(strings.Repeat("a", 64))
		manifestHash := strings.Repeat("d", 64)
		canonicalHash := strings.Repeat("e", 64)
		capsule := model.Capsule{
			SessionID: sessionID,
			ActorID:   "actor-1",
			Binding: model.Binding{
				RepositoryID: model.RepositoryID(strings.Repeat("1", 64)),
				SnapshotID:   model.SnapshotID(strings.Repeat("2", 64)),
				GenerationID: 42,
			},
			ManifestHash:  manifestHash,
			CanonicalHash: canonicalHash,
			ScopeVersion:  3,
			Counts: model.CapsuleCounts{
				Scope: 2, AcceptedFacts: 7, RejectedFacts: 1, Contradictions: 3,
				Unresolved: 4, ScopeReviewIDs: 5, Coverage: 2000, Waivers: 1,
			},
			StrictGateSatisfied: false,
		}
		ceiling := config.Defaults().Resources.MaxMetadataResponseBytes
		return scenario{
			name: "gate: capsule export returns identity and per-list counts, never records",
			facade: func(f *fakeServices) {
				f.exportFn = func(_ context.Context, _ model.SessionRequest) (model.Capsule, error) {
					return capsule, nil
				}
			},
			tool: "codectx_context_capsule",
			args: model.CapsuleRequest{
				SessionID: sessionID, ActorID: "actor-1",
				View: model.CapsuleView(capsuleViewExport),
				Page: model.PageRequest{Limit: 50},
			},
			check: func(t *testing.T, res *mcp.CallToolResult) {
				if res.IsError {
					t.Fatalf("context_capsule export reported a tool error: %s", firstText(res))
				}
				raw, err := json.Marshal(res.StructuredContent)
				if err != nil {
					t.Fatalf("marshal the structured answer: %v", err)
				}
				if int64(len(raw)) > ceiling {
					t.Errorf("export answer is %d bytes, over the %d-byte metadata ceiling", len(raw), ceiling)
				}
				var got result[capsuleOutput]
				decode(t, res, &got)
				if got.Data.Page != nil {
					t.Errorf("export answer also carries a capsule page: %+v", got.Data.Page)
				}
				if got.Data.Export == nil {
					t.Fatalf("export answer carries no projection")
				}
				e := got.Data.Export
				if e.CanonicalHash != canonicalHash || e.ManifestHash != manifestHash ||
					e.ScopeVersion != 3 || e.Binding.GenerationID != 42 || e.StrictGateSatisfied {
					t.Errorf("export = %+v, want the capsule's canonical metadata", *e)
				}
				// Every one of the eight lists a caller may page must be
				// counted here, at the count the capsule sealed.
				for _, list := range model.CapsuleListOrder {
					if e.Counts[string(list)] != capsule.Counts.Of(list) {
						t.Errorf("counts[%s] = %d, want %d", list, e.Counts[string(list)], capsule.Counts.Of(list))
					}
				}
			},
		}
	}(),
}

func TestScenarios(t *testing.T) {
	for _, sc := range scenarios {
		t.Run(sc.name, func(t *testing.T) {
			f := &fakeServices{}
			if sc.facade != nil {
				sc.facade(f)
			}
			cs := connectSession(t, newTestServer(f), sc.callCtx == nil)
			ctx := t.Context()
			if sc.callCtx != nil {
				ctx = sc.callCtx(t, f)
			}
			res, err := cs.CallTool(ctx, &mcp.CallToolParams{
				Name:      sc.tool,
				Arguments: sc.args,
			})
			if err != nil {
				if !sc.wantCallErr {
					t.Fatalf("tools/call %s raised a protocol error: %v", sc.tool, err)
				}
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("tools/call %s failed with %v, want the cancellation", sc.tool, err)
				}
				if res != nil {
					t.Errorf("a canceled call also returned %+v; the SDK reports no result", res)
				}
				return
			}
			if sc.wantCallErr {
				t.Fatalf("tools/call %s returned a result; want the call to fail", sc.tool)
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

// oversizedArgumentsRow is a constructor rather than a literal so the row can
// own a call flag that both its facade and its check close over.
//
// Failure mode: an oversized tools/call frame stops being refused outright and
// reaches a handler, so the facade pays a query for input the bound exists to
// reject. The row asserts the refusal carries CTX_RESOURCE_LIMIT and that
// ExploreService.Search was never entered.
//
// What it deliberately does NOT claim: that the cap precedes the SDK's decode
// and schema validation. The SDK reports a schema failure as a TOOL error, so a
// post-decode cap would still win this row -- the ordering cannot be observed
// from the client. It is a structural guarantee instead, and it is stated where
// it holds, at the json.RawMessage read in limitMiddleware (limits.go).
//
// The "never entered" assertion is defence in depth and is over-determined
// today: this frame is also schema-invalid, and every SearchRequest field is
// bounded by Validate, so no oversized frame can reach the facade by another
// route either. What mutation-proves this row is neutering the cap, which
// substitutes the SDK's schema error for the CTX_RESOURCE_LIMIT refusal.
func oversizedArgumentsRow() scenario {
	var searched bool
	return scenario{
		name: "oversized tool arguments are refused with CTX_RESOURCE_LIMIT before any handler runs",
		facade: func(f *fakeServices) {
			searched = false
			f.searchFn = func(context.Context, model.SearchRequest) (model.Page[model.SearchHit], error) {
				searched = true
				return model.Page[model.SearchHit]{}, nil
			}
		},
		tool: "codectx_search",
		args: map[string]any{"pad": strings.Repeat("x", 300_000)},
		check: func(t *testing.T, res *mcp.CallToolResult) {
			if !res.IsError {
				t.Fatalf("oversized arguments were accepted: %+v", res.StructuredContent)
			}
			text := firstText(res)
			if !strings.HasPrefix(text, model.CodeResourceLimit+":") {
				t.Errorf("tool error = %q, want the %s refusal", text, model.CodeResourceLimit)
			}
			if searched {
				t.Errorf("the facade was entered for a frame the bound refuses")
			}
		},
	}
}
