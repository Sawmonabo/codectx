package mcpserver

import (
	"context"
	"encoding/json"
	"slices"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/Sawmonabo/codectx/internal/model"
)

// TestAFieldThatDefaultsOnZeroIsNeverRequired calls codectx_search with its
// query alone and reads the nested input schemas the top-level snapshot does
// not reach: the page's limit and the context budget's fields. Each means "the
// configured or endpoint default" at zero, so a client that leaves one out must
// be served the default rather than refused by the schema before the handler
// runs.
//
// Mutation that fails it: drop omitempty from PageRequest's page or limit tag,
// or from any of the four Budget fields or ContextRequest.Budget (the call is
// refused for a missing field, or the nested required list names it).
func TestAFieldThatDefaultsOnZeroIsNeverRequired(t *testing.T) {
	var got model.SearchRequest
	searched := false
	f := &fakeServices{searchFn: func(_ context.Context, r model.SearchRequest) (model.Page[model.SearchHit], error) {
		got, searched = r, true
		return model.Page[model.SearchHit]{}, nil
	}}
	cs := connect(t, newTestServer(f))
	res, err := cs.CallTool(t.Context(), &mcp.CallToolParams{Name: "codectx_search", Arguments: map[string]any{"query": "parse"}})
	if err != nil {
		t.Fatalf("tools/call codectx_search: %v", err)
	}
	if res.IsError || !searched {
		t.Fatalf("a search naming only its query was refused (%q); the page defaults at zero", firstText(res))
	}
	if got.Page.Limit != 0 || got.Query != "parse" {
		t.Fatalf("the handler saw %+v, want the query and the zero (default) page", got)
	}

	type shape struct {
		Required   []string          `json:"required"`
		Properties map[string]*shape `json:"properties"`
	}
	schemas := map[string]shape{}
	for tool, err := range cs.Tools(t.Context(), nil) {
		if err != nil {
			t.Fatalf("tools/list: %v", err)
		}
		raw, err := json.Marshal(tool.InputSchema)
		if err != nil {
			t.Fatal(err)
		}
		var s shape
		if err := json.Unmarshal(raw, &s); err != nil {
			t.Fatal(err)
		}
		schemas[tool.Name] = s
	}
	page := schemas["codectx_search"].Properties["page"]
	if page == nil || len(page.Required) != 0 {
		t.Fatalf("codectx_search page schema = %+v, want limit and cursor optional", page)
	}
	planCtx := schemas["codectx_context_plan"].Properties["context"]
	if planCtx == nil || slices.Contains(planCtx.Required, "budget") {
		t.Fatalf("codectx_context_plan context schema = %+v, want the budget optional", planCtx)
	}
	if budget := planCtx.Properties["budget"]; budget == nil || len(budget.Required) != 0 {
		t.Fatalf("codectx_context_plan budget schema = %+v, want every budget field optional", budget)
	}
}
