package mcpserver

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/Sawmonabo/codectx/internal/model"
)

// TestCallToolsWithoutRelationsWalkCallsOnly pins the call hierarchy both
// traversal tools are named for: a client that omits `relations` walks
// `calls` alone, as the CLI's callers and callees commands do, so the answer
// holds no containment or data-flow edge beside the calls. A client that names
// relations gets exactly those.
//
// Mutation caught: forwarding the empty list, which the engine reads as its
// whole default allowlist (contains, defines, data_flows_to, ...).
func TestCallToolsWithoutRelationsWalkCallsOnly(t *testing.T) {
	start := model.NodeID(strings.Repeat("a", 64))
	for _, tc := range []struct {
		tool      string
		direction model.Direction
		asked     []model.RelationKind
		want      []model.RelationKind
	}{
		{"codectx_callers", model.DirectionIncoming, nil, []model.RelationKind{model.RelCalls}},
		{"codectx_callees", model.DirectionOutgoing, nil, []model.RelationKind{model.RelCalls}},
		{"codectx_callers", model.DirectionIncoming,
			[]model.RelationKind{model.RelCalls, model.RelReferences},
			[]model.RelationKind{model.RelCalls, model.RelReferences}},
	} {
		t.Run(tc.tool, func(t *testing.T) {
			var got model.GraphRequest
			f := &fakeServices{graphFn: func(_ context.Context, r model.GraphRequest) (model.GraphResult, error) {
				got = r
				return model.GraphResult{Direction: r.Direction}, nil
			}}
			cs := connect(t, newTestServer(f))
			res, err := cs.CallTool(t.Context(), &mcp.CallToolParams{
				Name:      tc.tool,
				Arguments: graphInput{Start: []model.NodeID{start}, Relations: tc.asked},
			})
			if err != nil {
				t.Fatalf("tools/call %s: %v", tc.tool, err)
			}
			if res.IsError {
				t.Fatalf("%s reported a tool error: %s", tc.tool, firstText(res))
			}
			if got.Direction != tc.direction {
				t.Errorf("%s walked %q, want %q", tc.tool, got.Direction, tc.direction)
			}
			if !slices.Equal(got.Relations, tc.want) {
				t.Errorf("%s with relations %v walked %v, want %v: an empty list must mean `calls`, "+
					"never the engine's default allowlist with its containment and data-flow kinds",
					tc.tool, tc.asked, got.Relations, tc.want)
			}
		})
	}
}
