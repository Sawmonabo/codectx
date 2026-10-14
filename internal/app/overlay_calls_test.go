package app

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/Sawmonabo/codectx/internal/model"
	"github.com/Sawmonabo/codectx/internal/provider/lsp"
	"github.com/Sawmonabo/codectx/internal/storage/sqlite"
)

// fakeCallHierarchy answers one prepared item per position and the calls the
// test names for it, recording the limits it was asked with.
type fakeCallHierarchy struct {
	prepared map[uint64]lsp.CallItem
	incoming map[string][]lsp.Call
	outgoing map[string][]lsp.Call
	// truncate marks a calls answer as having had more than the limit.
	truncate bool
	asked    []string
}

func (f *fakeCallHierarchy) PrepareCallHierarchy(_ context.Context, at lsp.At, limit int) (lsp.Result[lsp.CallItem], error) {
	if limit <= 0 || limit > model.MaxPageItems {
		return lsp.Result[lsp.CallItem]{}, errors.New("prepare limit outside the page bound")
	}
	item, ok := f.prepared[at.Byte]
	if !ok {
		return lsp.Result[lsp.CallItem]{}, nil
	}
	return lsp.Result[lsp.CallItem]{Items: []lsp.CallItem{item}}, nil
}

func (f *fakeCallHierarchy) IncomingCalls(_ context.Context, item lsp.CallItem, limit int) (lsp.Result[lsp.Call], error) {
	f.asked = append(f.asked, "incoming:"+item.Name)
	return f.page(f.incoming[item.Name], limit), nil
}

func (f *fakeCallHierarchy) OutgoingCalls(_ context.Context, item lsp.CallItem, limit int) (lsp.Result[lsp.Call], error) {
	f.asked = append(f.asked, "outgoing:"+item.Name)
	return f.page(f.outgoing[item.Name], limit), nil
}

func (f *fakeCallHierarchy) page(calls []lsp.Call, limit int) lsp.Result[lsp.Call] {
	out := lsp.Result[lsp.Call]{}
	for _, c := range calls {
		if len(out.Items) >= limit {
			out.Truncated = true
			break
		}
		out.Items = append(out.Items, c)
	}
	return out
}

func callFixture() (fake *fakeCallHierarchy, seedA, seedB overlayCallSeed, fileMain, fileUtil model.FileID) {
	fileMain = model.FileID(strings.Repeat("1", 64))
	fileUtil = model.FileID(strings.Repeat("2", 64))
	at := func(b uint64) model.SourceRange {
		return model.SourceRange{Start: model.Position{Byte: b, Line: 1, Column: uint32(b)}, End: model.Position{Byte: b + 5, Line: 1, Column: uint32(b + 5)}}
	}
	item := func(name string, file model.FileID, path string, b uint64) lsp.CallItem {
		rng := at(b)
		return lsp.CallItem{Symbol: lsp.Symbol{Name: name, Kind: model.NodeFunction,
			Location: lsp.Location{File: file, Path: path, ContentHash: string(file), Range: rng, Selection: &rng}}}
	}
	site := func(file model.FileID, path string, b uint64) lsp.Location {
		return lsp.Location{File: file, Path: path, ContentHash: string(file), Range: at(b)}
	}
	run, serve, helper := item("run", fileMain, "main.go", 10), item("serve", fileMain, "main.go", 40), item("helper", fileUtil, "util.go", 70)
	fake = &fakeCallHierarchy{
		prepared: map[uint64]lsp.CallItem{10: run, 40: serve},
		// helper calls both run and serve, from util.go.
		incoming: map[string][]lsp.Call{
			"run":   {{Item: helper, Sites: []lsp.Location{site(fileUtil, "util.go", 80)}}},
			"serve": {{Item: helper, Sites: []lsp.Location{site(fileUtil, "util.go", 90)}}},
		},
		// run calls helper, from main.go.
		outgoing: map[string][]lsp.Call{
			"run": {{Item: helper, Sites: []lsp.Location{site(fileMain, "main.go", 12)}}},
		},
	}
	seedA = overlayCallSeed{node: model.NodeID(strings.Repeat("a", 64)), at: lsp.At{File: fileMain, Byte: 10}}
	seedB = overlayCallSeed{node: model.NodeID(strings.Repeat("b", 64)), at: lsp.At{File: fileMain, Byte: 40}}
	return fake, seedA, seedB, fileMain, fileUtil
}

// TestOverlayCallsAnswerOneLabeledLevel pins the mapping of a call hierarchy
// level onto the overlay shapes: the edge's ends follow the direction (the
// peer calls the seed's item for callers, the seed's item calls the peer for
// callees), the sites are filed under the caller, every node is an overlay
// node, and a peer reached from two seeds is one node.
// Mutation caught: swapping From/To for one direction, filing sites under the
// callee, asking outgoing calls for a callers request, appending a peer once
// per edge instead of once per answer, or dropping a seed the server prepared
// nothing for without naming it.
func TestOverlayCallsAnswerOneLabeledLevel(t *testing.T) {
	ctx := context.Background()
	limits := overlayCallLimits{edges: 10, nodes: model.MaxRecordsPerResult}

	fake, seedA, seedB, fileMain, fileUtil := callFixture()
	in, err := walkOverlayCalls(ctx, fake, []overlayCallSeed{seedA, seedB}, model.DirectionIncoming, limits)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(fake.asked, ",") != "incoming:run,incoming:serve" {
		t.Fatalf("asked %v, want the incoming calls of both prepared items", fake.asked)
	}
	// run, then the peer helper, then serve; helper is shared by both edges.
	if len(in.nodes) != 3 || in.nodes[0].Name != "run" || in.nodes[1].Name != "helper" || in.nodes[2].Name != "serve" {
		t.Fatalf("nodes = %+v, want run, helper, serve once each", in.nodes)
	}
	for i, n := range in.nodes {
		if n.SemanticSource != model.SemanticLSP || n.ID != "" {
			t.Errorf("node %d = %+v, want an lsp overlay node with no canonical id", i, n)
		}
	}
	wantIn := []model.OverlayCall{
		{Seed: seedA.node, From: 1, To: 0, SiteFile: fileUtil, SitePath: "util.go"},
		{Seed: seedB.node, From: 1, To: 2, SiteFile: fileUtil, SitePath: "util.go"},
	}
	checkCalls(t, "callers", in, wantIn)

	fake, seedA, _, fileMain, _ = callFixture()
	out, err := walkOverlayCalls(ctx, fake, []overlayCallSeed{seedA}, model.DirectionOutgoing, limits)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(fake.asked, ",") != "outgoing:run" {
		t.Fatalf("asked %v, want the outgoing calls of the prepared item", fake.asked)
	}
	checkCalls(t, "callees", out, []model.OverlayCall{
		{Seed: seedA.node, From: 0, To: 1, SiteFile: fileMain, SitePath: "main.go"},
	})
	// A seed the server prepares nothing for is named, not dropped silently.
	fake, seedA, _, _, _ = callFixture()
	unknown := overlayCallSeed{node: model.NodeID(strings.Repeat("c", 64)), at: lsp.At{File: seedA.at.File, Byte: 99}}
	partial, err := walkOverlayCalls(ctx, fake, []overlayCallSeed{seedA, unknown}, model.DirectionIncoming, limits)
	if err != nil {
		t.Fatal(err)
	}
	if len(partial.unprepared) != 1 || partial.unprepared[0] != unknown.node {
		t.Errorf("unprepared = %v, want the seed the server prepared nothing for", partial.unprepared)
	}
	if out.truncated || out.budgetCut || in.truncated || in.budgetCut {
		t.Errorf("a whole level reported as cut: in %+v out %+v", in, out)
	}
}

func checkCalls(t *testing.T, label string, w overlayCallWalk, want []model.OverlayCall) {
	t.Helper()
	if len(w.calls) != len(want) {
		t.Fatalf("%s: calls = %+v, want %d", label, w.calls, len(want))
	}
	for i, c := range w.calls {
		if c.Seed != want[i].Seed || c.From != want[i].From || c.To != want[i].To ||
			c.SiteFile != want[i].SiteFile || c.SitePath != want[i].SitePath || len(c.Sites) != 1 {
			t.Errorf("%s: call %d = %+v, want %+v", label, i, c, want[i])
		}
		if err := c.Validate(w.nodes); err != nil {
			t.Errorf("%s: call %d does not validate: %v", label, i, err)
		}
	}
}

// TestOverlayCallsBudgetCutIsDisclosed pins that the request's own edge
// budget ends an overlay answer as a disclosed cut, distinct from the server
// having more than the page bound.
// Mutation caught: ignoring MaxEdges on the lsp route (two edges returned for
// a budget of one), or reporting a budget cut as the page bound's truncation.
func TestOverlayCallsBudgetCutIsDisclosed(t *testing.T) {
	fake, seedA, seedB, _, _ := callFixture()
	limits := overlayCallBounds(10, model.GraphRequest{MaxEdges: 1})
	w, err := walkOverlayCalls(context.Background(), fake, []overlayCallSeed{seedA, seedB}, model.DirectionIncoming, limits)
	if err != nil {
		t.Fatal(err)
	}
	if len(w.calls) != 1 || !w.budgetCut || w.truncated {
		t.Fatalf("calls %d, budgetCut %v, truncated %v; want one call cut by the request's budget",
			len(w.calls), w.budgetCut, w.truncated)
	}
	page := overlayCallBounds(1, model.GraphRequest{})
	fake, seedA, seedB, _, _ = callFixture()
	w, err = walkOverlayCalls(context.Background(), fake, []overlayCallSeed{seedA, seedB}, model.DirectionIncoming, page)
	if err != nil {
		t.Fatal(err)
	}
	if len(w.calls) != 1 || w.budgetCut || !w.truncated {
		t.Fatalf("calls %d, budgetCut %v, truncated %v; want one call cut by the page bound",
			len(w.calls), w.budgetCut, w.truncated)
	}
}

// TestOverlayCallsRefuseWhatTheProtocolCannotAnswer pins Section 11.6's typed
// unavailable answer for a call-hierarchy request the server cannot answer as
// asked: a relation other than calls, both directions at once, more than one
// level. The canonical engine is never asked in their place.
// Mutation caught: dropping any one screen (the request then reaches the
// server and is answered as one level of calls, which is not what was asked).
func TestOverlayCallsRefuseWhatTheProtocolCannotAnswer(t *testing.T) {
	base := model.GraphRequest{Relations: []model.RelationKind{model.RelCalls}, Direction: model.DirectionIncoming}
	if err := overlayCallsAnswerable(base); err != nil {
		t.Fatalf("one level of incoming calls refused: %v", err)
	}
	one := base
	one.MaxDepth = 1
	if err := overlayCallsAnswerable(one); err != nil {
		t.Fatalf("depth 1 refused: %v", err)
	}
	for name, edit := range map[string]func(*model.GraphRequest){
		"no relation":     func(r *model.GraphRequest) { r.Relations = nil },
		"references":      func(r *model.GraphRequest) { r.Relations = []model.RelationKind{model.RelCalls, model.RelReferences} },
		"both directions": func(r *model.GraphRequest) { r.Direction = model.DirectionBoth },
		"two levels":      func(r *model.GraphRequest) { r.MaxDepth = 2 },
	} {
		req := base
		edit(&req)
		var typed *model.Error
		if err := overlayCallsAnswerable(req); !errors.As(err, &typed) || typed.Code != model.CodeProviderUnavailable {
			t.Errorf("%s: %v, want %s", name, err, model.CodeProviderUnavailable)
		}
	}
}

// TestOverlayCallsPrepareAtTheName pins that a seed's item is prepared at its
// name token, where a server resolves the identifier, and at its first byte
// only when the node stores no name.
// Mutation caught: preparing at the declaration's first byte (the `func`
// keyword), where a server prepares no item and every seed answers empty.
func TestOverlayCallsPrepareAtTheName(t *testing.T) {
	file := model.FileID(strings.Repeat("1", 64))
	stored := sqlite.StoredNode{Node: model.Node{FileID: file},
		Bytes: &model.ByteRange{Start: 100, End: 180}, NameBytes: &model.ByteRange{Start: 105, End: 108}}
	if at := overlayPosition(stored); at.File != file || at.Byte != 105 {
		t.Errorf("prepared at %+v, want the name token at byte 105", at)
	}
	stored.NameBytes = nil
	if at := overlayPosition(stored); at.Byte != 100 {
		t.Errorf("a node with no name token prepared at %+v, want its first byte 100", at)
	}
}
