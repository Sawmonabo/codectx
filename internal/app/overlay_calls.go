package app

// This file is the semantic_source=lsp route of a callers or callees request
// (Sections 11.5, 11.6): one level of the managed language server's call
// hierarchy, answered as a labeled overlay exactly as overlay.go answers
// symbols and references -- every node is a model.SemanticLSP node with no
// canonical id, every edge is a model.OverlayCall rather than a canonical
// relation, the page carries the server's model.OverlayBinding, and nothing is
// written.
//
// The LSP 3.17 call hierarchy is resolved in two steps: a CallHierarchyItem is
// prepared at a text document position (textDocument/prepareCallHierarchy),
// and for that item the server answers its direct incoming or outgoing calls
// (callHierarchy/incomingCalls, callHierarchy/outgoingCalls), each with the
// call sites in the caller. One request is one level, so this route answers
// one level: a deeper walk would be a client-driven expansion over items whose
// identity the protocol leaves opaque, with no continuation to resume a walk a
// budget cut, and is refused as unavailable rather than approximated.

import (
	"context"
	"strconv"

	"github.com/Sawmonabo/codectx/internal/model"
	"github.com/Sawmonabo/codectx/internal/provider/lsp"
	"github.com/Sawmonabo/codectx/internal/storage/sqlite"
)

// callHierarchy is the part of an *lsp.Overlay this route drives.
type callHierarchy interface {
	PrepareCallHierarchy(ctx context.Context, at lsp.At, limit int) (lsp.Result[lsp.CallItem], error)
	IncomingCalls(ctx context.Context, item lsp.CallItem, limit int) (lsp.Result[lsp.Call], error)
	OutgoingCalls(ctx context.Context, item lsp.CallItem, limit int) (lsp.Result[lsp.Call], error)
}

// overlayCallsUnavailable is the typed answer for a call-hierarchy request the
// overlay cannot answer as asked. It is never a substituted canonical answer.
func overlayCallsUnavailable(reason string) error {
	return (&model.Error{Code: model.CodeProviderUnavailable,
		Message: "the language server call hierarchy cannot answer this request: " + reason}).
		WithDetail("semantic_source", string(model.SemanticLSP)).
		WithDetail("reason", reason)
}

// overlayCallsAnswerable screens a request before anything is resolved or
// pinned: the protocol answers `calls` only, one direction per request, one
// level per request.
func overlayCallsAnswerable(req model.GraphRequest) error {
	if len(req.Relations) == 0 {
		// The canonical meaning of no relations is the engine's wider
		// allowlist, which a call hierarchy does not answer.
		return overlayCallsUnavailable("the request names no relation; the call hierarchy answers `calls` only")
	}
	for _, k := range req.Relations {
		if k != model.RelCalls {
			return overlayCallsUnavailable("relation " + strconv.Quote(string(k)) + " is not a call; the call hierarchy answers `calls` only")
		}
	}
	if req.Direction != model.DirectionIncoming && req.Direction != model.DirectionOutgoing {
		return overlayCallsUnavailable("direction " + strconv.Quote(string(req.Direction)) +
			" is not one call hierarchy request; ask incoming and outgoing calls separately")
	}
	if req.MaxDepth > 1 {
		return overlayCallsUnavailable("depth " + strconv.Itoa(req.MaxDepth) +
			" is more than the one level a call hierarchy request answers")
	}
	return nil
}

// overlayCalls answers a GraphRequest through the language server overlay.
// req.Validate has already run on the facade.
func (s *stack) overlayCalls(ctx context.Context, req model.GraphRequest) (model.GraphResult, error) {
	if req.Source() != model.SemanticLSP {
		return model.GraphResult{}, &model.Error{Code: model.CodeInternal,
			Message: "app: a canonical graph request reached the language server overlay route"}
	}
	if err := overlayCallsAnswerable(req); err != nil {
		return model.GraphResult{}, err
	}
	route, err := s.pinOverlay(ctx, req.GenerationID, req.Profile, req.Page)
	if err != nil {
		return model.GraphResult{}, err
	}
	defer s.closeOverlay(route)

	seeds := make([]overlayCallSeed, 0, len(req.Start))
	root, rooted := "", false
	for _, id := range req.Start {
		stored, err := route.reader.Node(ctx, id)
		if err != nil {
			return model.GraphResult{}, err
		}
		if stored.Node.FileID == "" || stored.Bytes == nil {
			return model.GraphResult{}, (&model.Error{Code: model.CodeArgumentInvalid,
				Message: "the node has no declaration site in the pinned snapshot, so the language server overlay has no position to query"}).
				WithDetail("node_id", string(id))
		}
		// One answer is labeled with one server, and a server is rooted at
		// one project, so every seed must belong to the same project.
		// The stored path is written with the fact ('' only for a node with
		// no file); a row without one reads the path from the snapshot, as
		// attachServer does, rather than root the server at the workspace.
		path := stored.Path
		if path == "" {
			rc, fv, err := route.view.Open(ctx, stored.Node.FileID)
			if err != nil {
				return model.GraphResult{}, err
			}
			rc.Close()
			path = fv.Path
		}
		seedRoot, err := route.profile.ProjectRoot(ctx, route.view, path)
		if err != nil {
			return model.GraphResult{}, err
		}
		if rooted && seedRoot != root {
			return model.GraphResult{}, (&model.Error{Code: model.CodeArgumentInvalid,
				Message: "the seeds belong to different projects, which different language servers answer; ask the overlay for each project's seeds separately"}).
				WithDetail("node_id", string(id))
		}
		root, rooted = seedRoot, true
		seeds = append(seeds, overlayCallSeed{node: id, at: overlayCallPosition(stored)})
	}
	route.profile.Root = root
	overlay, err := s.lsp.Open(ctx, route.view, route.profile)
	if err != nil {
		return model.GraphResult{}, err
	}
	route.overlay = overlay

	walk, err := walkOverlayCalls(ctx, overlay, seeds, req.Direction, overlayCallBounds(route.limit, req))
	if err != nil {
		return model.GraphResult{}, err
	}
	capability := "callers"
	if req.Direction == model.DirectionOutgoing {
		capability = "callees"
	}
	// answered counts the nodes, not the edges: a server that prepared an
	// item for the seed has analysed it, so an item with no calls is a
	// confident empty level; only a seed the server prepared nothing for is
	// the indistinguishable empty answer overlayMeta discloses.
	meta := overlayMeta(route.binding, overlay.Binding(), capability, req.Profile,
		walk.truncated || walk.budgetCut, walk.excluded, len(walk.nodes))
	if walk.budgetCut && !walk.truncated {
		meta.TruncationReason = "the request's visited or edge budget ended the answer"
	}
	// A seed the server prepared nothing for is named, so an answer for the
	// other seeds is not read as covering it.
	for _, id := range walk.unprepared {
		meta.Notices = append(meta.Notices, "the language server prepared no call hierarchy item for seed "+string(id))
	}
	res := model.GraphResult{
		Meta:         meta,
		Direction:    req.Direction,
		Nodes:        walk.nodes,
		Relations:    []model.Relation{},
		VisitedCount: int64(len(walk.nodes)),
		EdgeCount:    int64(len(walk.calls)),
		MaxDepth:     1,
		Calls:        walk.calls,
	}
	if err := res.Validate(); err != nil {
		return model.GraphResult{}, err
	}
	return res, nil
}

// overlayCallPosition is where a seed's item is prepared: its name token. A
// call hierarchy item is resolved for the identifier at the position, and the
// first byte of a declaration is its keyword (`func`, `def`, `public`), where
// a server prepares nothing. A node that stores no name token is asked at its
// first byte. The caller has checked the node has a declaration site.
func overlayCallPosition(stored sqlite.StoredNode) lsp.At {
	at := lsp.At{File: stored.Node.FileID, Byte: stored.Bytes.Start}
	if stored.NameBytes != nil {
		at.Byte = stored.NameBytes.Start
	}
	return at
}

// overlayCallSeed is one start node and the position its item is prepared at.
type overlayCallSeed struct {
	node model.NodeID
	at   lsp.At
}

// overlayCallLimits bound one overlay call answer: edges by the page bound and
// the request's edge budget, nodes by the request's visited budget and the
// page width every record list of a result is held to.
type overlayCallLimits struct {
	edges, nodes int
	// edgesByBudget and nodesByBudget say whether the request's own budget,
	// rather than the page, set the bound, which is what the truncation
	// reason names.
	edgesByBudget, nodesByBudget bool
}

// overlayCallBounds resolves the bounds of one answer. A zero budget is the
// endpoint default, as on every request (Section 20.1).
func overlayCallBounds(pageLimit int, req model.GraphRequest) overlayCallLimits {
	b := overlayCallLimits{edges: pageLimit, nodes: model.MaxRecordsPerResult}
	if req.MaxEdges > 0 && req.MaxEdges < b.edges {
		b.edges, b.edgesByBudget = req.MaxEdges, true
	}
	if req.MaxVisited > 0 && req.MaxVisited < b.nodes {
		b.nodes, b.nodesByBudget = req.MaxVisited, true
	}
	return b
}

// overlayCallWalk is one level of call hierarchy, mapped onto the overlay
// shapes of model.GraphResult.
type overlayCallWalk struct {
	nodes []model.Node
	calls []model.OverlayCall
	// truncated is the server having said more than the page bound;
	// budgetCut is the request's own visited or edge budget ending it.
	truncated, budgetCut bool
	excluded             int
	// unprepared are the seeds the server prepared no item for.
	unprepared []model.NodeID
}

// overlayNodeKey identifies one overlay node within an answer: the same item
// reached from two seeds, or prepared and also named as a peer, is one node.
type overlayNodeKey struct {
	file       model.FileID
	start, end uint64
	name       string
	kind       model.NodeKind
}

// walkOverlayCalls prepares every seed's item and asks for its calls in dir.
// Prepared items come first in nodes, in seed order; each edge's peer follows
// at its first appearance.
func walkOverlayCalls(ctx context.Context, ch callHierarchy, seeds []overlayCallSeed, dir model.Direction, b overlayCallLimits) (overlayCallWalk, error) {
	w := overlayCallWalk{nodes: []model.Node{}, calls: []model.OverlayCall{}}
	index := map[overlayNodeKey]int{}
	// cut records which bound ended the answer.
	cut := func(byBudget bool) {
		if byBudget {
			w.budgetCut = true
		} else {
			w.truncated = true
		}
	}
	add := func(item lsp.CallItem) (int, bool) {
		loc := item.Location
		key := overlayNodeKey{file: loc.File, start: loc.Range.Start.Byte, end: loc.Range.End.Byte, name: item.Name, kind: item.Kind}
		if i, ok := index[key]; ok {
			return i, true
		}
		if len(w.nodes) >= b.nodes {
			cut(b.nodesByBudget)
			return 0, false
		}
		index[key] = len(w.nodes)
		w.nodes = append(w.nodes, overlaySymbolNodes([]lsp.Symbol{item.Symbol})[0])
		return len(w.nodes) - 1, true
	}
	for _, seed := range seeds {
		if len(w.nodes) >= b.nodes {
			cut(b.nodesByBudget)
			return w, nil
		}
		// A prepare is one page like every overlay operation, so its bound is
		// the nodes still admissible, within the page width.
		prepLimit, prepByBudget := b.nodes-len(w.nodes), b.nodesByBudget
		if prepLimit > model.MaxPageItems {
			prepLimit, prepByBudget = model.MaxPageItems, false
		}
		prepared, err := ch.PrepareCallHierarchy(ctx, seed.at, prepLimit)
		if err != nil {
			return overlayCallWalk{}, err
		}
		w.excluded += prepared.Excluded
		if prepared.Truncated {
			cut(prepByBudget)
		}
		if len(prepared.Items) == 0 && prepared.Excluded == 0 {
			w.unprepared = append(w.unprepared, seed.node)
		}
		for _, item := range prepared.Items {
			self, ok := add(item)
			if !ok {
				return w, nil
			}
			if len(w.calls) >= b.edges {
				cut(b.edgesByBudget)
				return w, nil
			}
			ask := ch.IncomingCalls
			if dir == model.DirectionOutgoing {
				ask = ch.OutgoingCalls
			}
			res, err := ask(ctx, item, b.edges-len(w.calls))
			if err != nil {
				return overlayCallWalk{}, err
			}
			w.excluded += res.Excluded
			if res.Truncated {
				cut(b.edgesByBudget)
			}
			for _, call := range res.Items {
				peer, ok := add(call.Item)
				if !ok {
					return w, nil
				}
				from, to := peer, self
				if dir == model.DirectionOutgoing {
					from, to = self, peer
				}
				w.calls = append(w.calls, overlayCallEdge(seed.node, from, to, w.nodes[from].FileID, call.Sites))
			}
		}
	}
	return w, nil
}

// overlayCallEdge maps one protocol call onto an overlay edge. The protocol
// places every site in the caller (fromRanges) and the overlay validates all
// of one call's sites against that one document, so the first site names the
// file of all of them; the edge's Validate refuses an answer whose site file
// is not the caller's rather than relabel it.
func overlayCallEdge(seed model.NodeID, from, to int, callerFile model.FileID, sites []lsp.Location) model.OverlayCall {
	edge := model.OverlayCall{Seed: seed, From: from, To: to, SiteFile: callerFile, Sites: make([]model.SourceRange, 0, len(sites))}
	if len(sites) > 0 {
		edge.SiteFile, edge.SitePath = sites[0].File, sites[0].Path
	}
	for _, site := range sites {
		edge.Sites = append(edge.Sites, site.Range)
	}
	return edge
}
