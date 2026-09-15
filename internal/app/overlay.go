package app

// L8 owns this file: the semantic_source=lsp route of Section 11.5/11.6.
//
// The facade dispatches here when a SymbolRequest or a ReferenceRequest names
// semantic_source=lsp; semantic_source=canonical stays with the search service
// and internal/search is not edited for this route (internal/search/resolve.go
// keeps answering a direct search call with its own unavailable row).
//
// Everything this file produces is an OVERLAY answer, never a canonical fact:
//
//   - every row is labelled model.SemanticLSP and carries no canonical
//     relation or evidence id, because nothing was sealed;
//   - every page carries the overlay's model.OverlayBinding in QueryMeta, so a
//     consumer can tell which server, at which version, over which input
//     digest, produced it;
//   - nothing here writes: no store call in this file mutates anything, so an
//     overlay answer can never become a canonical fact and can never grant
//     coverage credit for a file the actor has not actually read;
//   - the overlay handle is released on every path, including every error
//     path, so a page that fails does not leak a language-server slot.
//
// The generation is pinned for the call so the snapshot the server is
// materialized over is the one the answer is labelled with, and the pin is
// released with the overlay.

import (
	"context"
	"encoding/json"
	"strconv"

	"github.com/Sawmonabo/codectx/internal/model"
	"github.com/Sawmonabo/codectx/internal/provider"
	"github.com/Sawmonabo/codectx/internal/provider/lsp"
	"github.com/Sawmonabo/codectx/internal/storage/sqlite"
)

// overlayProviderID is the provider identity a completeness row from this
// route is published under. The profile is appended when the request named
// one, which is the same spelling the canonical path's unavailable row uses,
// so a consumer reading both sees one identity for the overlay.
func overlayProviderID(profile string) string {
	if profile == "" {
		return "lsp"
	}
	return "lsp:" + profile
}

// overlayRoute is one open overlay answer in progress: the pinned reader that
// names the generation and its snapshot, and the server handle over that
// snapshot. close releases both, in reverse.
type overlayRoute struct {
	reader  *sqlite.PinnedReader
	overlay *lsp.Overlay
	binding model.Binding
	limit   int
}

// openOverlay resolves the profile, pins the generation and opens the server
// over the pinned snapshot.
//
// The profile is resolved FIRST, before anything is pinned: an unresolvable
// payload surfaces the toolchain's own CTX_TOOL_* error verbatim (offline, an
// unsupported platform, a corrupt store and an invalid override are different
// conditions a caller must be able to tell apart), and paying for a generation
// pin to reach the same answer would only widen the window in which it is held.
//
// Every failure after the pin releases it, so the only way out of this function
// with a lease held is a returned route the caller closes.
func (s *stack) openOverlay(ctx context.Context, gen model.GenerationID, profile string, page model.PageRequest) (*overlayRoute, error) {
	if profile == "" {
		return nil, &model.Error{Code: model.CodeArgumentInvalid,
			Message: "a semantic_source=lsp request must name the language server profile that answers it"}
	}
	// The overlay is an ephemeral answer over a running server: there is no
	// continuation to resume, and honouring a cursor minted by the canonical
	// path would answer a different question than the one the cursor pins.
	if page.Cursor != "" {
		return nil, &model.Error{Code: model.CodeArgumentInvalid,
			Message: "a semantic_source=lsp request is a single bounded page and cannot continue a cursor"}
	}
	if s.lsp == nil {
		return nil, (&model.Error{Code: model.CodeProviderUnavailable,
			Message: "the language server overlay is not composed in this workspace"}).WithDetail("profile", profile)
	}
	resolved, err := lsp.Resolve(ctx, s.resolver, s.cfg, profile)
	if err != nil {
		return nil, err
	}
	reader, err := s.store.PinGeneration(ctx, s.repo, gen, s.cfg.Storage.QueryCursorTTL.Std())
	if err != nil {
		return nil, err
	}
	binding := reader.Binding()
	view, err := s.view(ctx, binding.SnapshotID)
	if err != nil {
		reader.Close()
		return nil, err
	}
	overlay, err := s.lsp.Open(ctx, view, resolved)
	if err != nil {
		reader.Close()
		return nil, err
	}
	return &overlayRoute{reader: reader, overlay: overlay, binding: binding, limit: s.overlayLimit(page.Limit)}, nil
}

// closeOverlay releases the server handle and then the generation pin. Both run
// even when the first fails: a lease left held outlives the command that took
// it. Neither failure can be returned -- the caller is already answering -- so
// each is logged at Warn, because PinnedReader.Close is what releases the
// retention lease and a silent failure there is a generation pinned against
// retention with nothing to say so.
func (s *stack) closeOverlay(r *overlayRoute) {
	if err := r.overlay.Close(); err != nil {
		s.logger.Warn("a language server overlay handle could not be closed",
			"component", "app", "error", err.Error())
	}
	if err := r.reader.Close(); err != nil {
		s.logger.Warn("a pinned generation reader could not be released",
			"component", "app", "error", err.Error())
	}
}

// overlayLimit clamps the caller's page bound to this workspace's
// resources.max_page_items and then to the model ceiling. Zero means the
// endpoint default, which here is the configured bound.
func (s *stack) overlayLimit(requested int) int {
	limit := s.cfg.Resources.MaxPageItems
	if limit <= 0 || limit > model.MaxPageItems {
		limit = model.MaxPageItems
	}
	if requested > 0 && requested < limit {
		limit = requested
	}
	return limit
}

// overlayMeta labels one overlay page: the pinned binding it was read over, the
// overlay binding that identifies the server, and the honest bounds.
//
// Truncated means the server had more to say than the page bound allowed.
// Excluded is different and is not truncation: those are locations the server
// named outside the pinned snapshot -- a standard library, a dependency cache
// -- which were neither followed nor opened, so they are reported as a partial
// capability rather than silently dropped.
//
// An EMPTY page is disclosed too, and answered is what says so. A language
// server that cannot analyse the workspace at all -- its own toolchain missing
// from PATH is the ordinary case -- answers every request with zero results and
// no protocol error, which is byte-identical to a workspace where the symbol
// genuinely does not exist. This route cannot tell those apart, so it says
// exactly that: an `unavailable` row carrying the reason, which reaches a --json
// consumer as completeness and a human reader as a warning (queryWarnings
// promotes unavailable and failed rows, and prints their `reason` detail). The
// alternative -- ok:true, items:[], completeness:null -- reports an overlay that
// analysed nothing as a confident empty answer.
func overlayMeta(binding model.Binding, overlay model.OverlayBinding, capability, profile string, truncated bool, excluded, answered int) model.QueryMeta {
	meta := model.QueryMeta{Binding: binding, Overlay: &overlay}
	if truncated {
		meta.Truncated = true
		meta.TruncationReason = "the language server returned more results than the page bound"
	}
	state := model.CapabilityState{
		ProviderID: overlayProviderID(profile),
		Capability: capability,
		Scope:      provider.ScopeWorkspace,
	}.WithDetail("semantic_source", string(model.SemanticLSP))
	switch {
	case excluded > 0:
		// Partial rather than unavailable even when every located answer was
		// excluded: the server did analyse the workspace and named those
		// locations, they simply sit outside the pinned snapshot.
		state.State = model.CapabilityPartial
		meta.Completeness = []model.CapabilityState{state.WithDetail("excluded_locations", strconv.Itoa(excluded))}
	case answered == 0:
		state.State = model.CapabilityUnavailable
		meta.Completeness = []model.CapabilityState{state.WithDetail("reason",
			"the language server returned no results for this request; this route cannot tell an absent symbol from a workspace the server could not analyse")}
	}
	return meta
}

// overlaySymbols answers a SymbolRequest through the language server overlay.
// req.Validate has already run on the facade.
//
// All four Section 18.1 symbol operations are reachable: resolve and
// workspace-symbols are both the server's workspace/symbol -- resolve is the
// same question asked of the overlay, and the overlay has no second index to
// ask -- document-symbols is textDocument/documentSymbol over the pinned file,
// and definition is textDocument/definition at the pinned position.
//
// A capability the server did not advertise is the overlay's own
// CTX_PROVIDER_UNAVAILABLE naming the method, never a substituted canonical
// answer and never a silent empty page.
func (s *stack) overlaySymbols(ctx context.Context, req model.SymbolRequest) (model.Page[model.Node], error) {
	if req.SemanticSource != model.SemanticLSP {
		return model.Page[model.Node]{}, &model.Error{Code: model.CodeInternal,
			Message: "app: a canonical symbol request reached the language server overlay route"}
	}
	route, err := s.openOverlay(ctx, req.GenerationID, req.Profile, req.Page)
	if err != nil {
		return model.Page[model.Node]{}, err
	}
	defer s.closeOverlay(route)

	var (
		nodes     []model.Node
		truncated bool
		excluded  int
	)
	switch req.Operation {
	case model.SymbolResolve, model.SymbolWorkspaceSymbols:
		res, err := route.overlay.WorkspaceSymbols(ctx, req.Query, route.limit)
		if err != nil {
			return model.Page[model.Node]{}, err
		}
		nodes, truncated, excluded = overlaySymbolNodes(res.Items), res.Truncated, res.Excluded
	case model.SymbolDocumentSymbols:
		res, err := route.overlay.DocumentSymbols(ctx, req.FileID, route.limit)
		if err != nil {
			return model.Page[model.Node]{}, err
		}
		nodes, truncated, excluded = overlaySymbolNodes(res.Items), res.Truncated, res.Excluded
	case model.SymbolDefinition:
		// The server answers a position, not a name: the caller must say which
		// pinned byte it means. Guessing one from the query would answer about
		// a different symbol without saying so.
		if req.FileID == "" || req.Range == nil {
			return model.Page[model.Node]{}, &model.Error{Code: model.CodeArgumentInvalid,
				Message: "the definition operation through the language server overlay needs the pinned file and range of the symbol"}
		}
		res, err := route.overlay.Definition(ctx, lsp.At{File: req.FileID, Byte: req.Range.Start.Byte}, route.limit)
		if err != nil {
			return model.Page[model.Node]{}, err
		}
		nodes, truncated, excluded = overlayDefinitionNodes(req.Query, res.Items), res.Truncated, res.Excluded
	default:
		return model.Page[model.Node]{}, &model.Error{Code: model.CodeArgumentInvalid,
			Message: "symbol operation " + strconv.Quote(string(req.Operation)) + " has no language server overlay route"}
	}
	page := model.Page[model.Node]{
		Meta:  overlayMeta(route.binding, route.overlay.Binding(), string(req.Operation), req.Profile, truncated, excluded, len(nodes)),
		Items: nodes,
	}
	if err := page.Validate(); err != nil {
		return model.Page[model.Node]{}, err
	}
	for _, n := range page.Items {
		if err := n.Validate(); err != nil {
			return model.Page[model.Node]{}, err
		}
	}
	return page, nil
}

// overlaySymbolNodes turns server symbols into overlay nodes. They carry no
// canonical id -- nothing was sealed -- so the pinned file, content hash and
// range are the whole result and are what makes the answer verifiable.
func overlaySymbolNodes(symbols []lsp.Symbol) []model.Node {
	nodes := make([]model.Node, 0, len(symbols))
	for _, sym := range symbols {
		rng := sym.Location.Range
		nodes = append(nodes, model.Node{
			Kind: sym.Kind,
			Name: sym.Name,
			// Detail is the server's own description of the symbol, which is
			// its signature where the server provides one. It is passed
			// through, never synthesized. The drop this used to describe is
			// unreachable now: internal/provider/lsp bounds every Symbol
			// field in its one constructor and records the cut in
			// truncated_fields, so a detail arriving here is already within
			// the bound and overlayBounded passes it through unchanged. It
			// stays as the defensive floor for a detail no constructor
			// bounded, and would drop rather than truncate there, because a
			// truncated signature with nothing recording the cut is a
			// different signature presented as whole.
			Signature:      overlayBounded(sym.Detail, model.MaxSignatureBytes),
			FileID:         sym.Location.File,
			ContentHash:    sym.Location.ContentHash,
			Range:          &rng,
			Metadata:       overlayContainer(sym.Container),
			SemanticSource: model.SemanticLSP,
		})
	}
	return nodes
}

// overlayBounded drops a value that would breach the field bound it is written
// into. Truncating would publish a value the source does not contain.
func overlayBounded(value string, limit int) string {
	if len(value) > limit {
		return ""
	}
	return value
}

// overlayContainer carries the enclosing symbol's name, which is what tells two
// same-named methods apart. It is NOT folded into QualifiedName: the qualified
// spelling differs per language and joining the two with a separator this
// package chose would be a name no source file contains.
func overlayContainer(container string) json.RawMessage {
	if container == "" {
		return nil
	}
	raw, err := json.Marshal(struct {
		Container string `json:"container"`
	}{Container: container})
	// A name that cannot be encoded, or one long enough to breach the metadata
	// bound, is dropped rather than truncated into a different name.
	if err != nil || len(raw) > model.MaxMetadataBytes {
		return nil
	}
	return raw
}

// overlayDefinitionNodes turns definition locations into overlay nodes. A
// definition result is a bare lsp.Location: the protocol's textDocument/
// definition response carries a range and nothing else, so the overlay has no
// SymbolKind to derive a node kind from here (unlike the symbol routes, which
// map the kind the server sent). The queried name is therefore carried through
// and the kind is model.NodeVariable -- the same honest fallback the protocol
// mapping uses for a symbol whose kind is not a declaration kind this client
// knows: a named entity, never a fabricated declaration kind.
func overlayDefinitionNodes(query string, locations []lsp.Location) []model.Node {
	nodes := make([]model.Node, 0, len(locations))
	for _, loc := range locations {
		rng := loc.Range
		nodes = append(nodes, model.Node{
			Kind:           model.NodeVariable,
			Name:           query,
			FileID:         loc.File,
			ContentHash:    loc.ContentHash,
			Range:          &rng,
			SemanticSource: model.SemanticLSP,
		})
	}
	return nodes
}

// overlayReferences answers a ReferenceRequest through the language server
// overlay. req.Validate has already run on the facade.
//
// The request names a canonical node and the server answers a position, so the
// node's own declaration site is the position queried. A node with no
// declaration site in this generation -- a manifest dependency, an unresolved
// provider-local entity -- has no position to ask about and is refused rather
// than answered about some other byte.
func (s *stack) overlayReferences(ctx context.Context, req model.ReferenceRequest) (model.Page[model.ReferenceOccurrence], error) {
	if req.SemanticSource != model.SemanticLSP {
		return model.Page[model.ReferenceOccurrence]{}, &model.Error{Code: model.CodeInternal,
			Message: "app: a canonical reference request reached the language server overlay route"}
	}
	route, err := s.openOverlay(ctx, req.GenerationID, req.Profile, req.Page)
	if err != nil {
		return model.Page[model.ReferenceOccurrence]{}, err
	}
	defer s.closeOverlay(route)

	stored, err := route.reader.Node(ctx, req.NodeID)
	if err != nil {
		return model.Page[model.ReferenceOccurrence]{}, err
	}
	if stored.Node.FileID == "" || stored.Bytes == nil {
		return model.Page[model.ReferenceOccurrence]{}, &model.Error{Code: model.CodeArgumentInvalid,
			Message: "the node has no declaration site in the pinned snapshot, so the language server overlay has no position to query"}
	}
	at := lsp.At{File: stored.Node.FileID, Byte: stored.Bytes.Start}

	var res lsp.Result[lsp.Location]
	switch req.Operation {
	case model.ReferenceReferences:
		// The declaration is the position being queried and the caller already
		// holds it, so it is not asked for a second time as its own reference.
		res, err = route.overlay.References(ctx, at, false, route.limit)
	case model.ReferenceImplements:
		res, err = route.overlay.Implementations(ctx, at, route.limit)
	case model.ReferenceTypeDefinition:
		res, err = route.overlay.TypeDefinition(ctx, at, route.limit)
	default:
		return model.Page[model.ReferenceOccurrence]{}, &model.Error{Code: model.CodeArgumentInvalid,
			Message: "reference operation " + strconv.Quote(string(req.Operation)) + " has no language server overlay route"}
	}
	if err != nil {
		return model.Page[model.ReferenceOccurrence]{}, err
	}
	occurrences := overlayOccurrences(req.Operation, req.NodeID, res.Items)
	page := model.Page[model.ReferenceOccurrence]{
		Meta:  overlayMeta(route.binding, route.overlay.Binding(), string(req.Operation), req.Profile, res.Truncated, res.Excluded, len(occurrences)),
		Items: occurrences,
	}
	if err := page.Validate(); err != nil {
		return model.Page[model.ReferenceOccurrence]{}, err
	}
	for _, o := range page.Items {
		if err := o.Validate(); err != nil {
			return model.Page[model.ReferenceOccurrence]{}, err
		}
	}
	return page, nil
}

// overlayOccurrences turns located answers into overlay occurrences.
//
// Each operation names a different edge, and the queried node sits at a
// different end of it, so both are stated rather than left to the reader:
// a reference occurrence points AT the queried node (the located site
// references it), an implementation points at it too (the located symbol
// implements it), and a type definition points AWAY from it (the queried node
// depends on the located type). The end the server did not name stays empty,
// as does the relation and evidence id: no canonical edge was sealed for any
// of these and claiming one would make an ephemeral answer look like a fact.
func overlayOccurrences(op model.ReferenceOperation, node model.NodeID, locations []lsp.Location) []model.ReferenceOccurrence {
	out := make([]model.ReferenceOccurrence, 0, len(locations))
	for _, loc := range locations {
		rng := loc.Range
		occ := model.ReferenceOccurrence{
			Precision:      lsp.Precision,
			FileID:         loc.File,
			Path:           loc.Path,
			Range:          &rng,
			SemanticSource: model.SemanticLSP,
		}
		switch op {
		case model.ReferenceReferences:
			occ.Kind, occ.ToNodeID = model.RelReferences, node
		case model.ReferenceImplements:
			occ.Kind, occ.ToNodeID = model.RelImplements, node
		case model.ReferenceTypeDefinition:
			occ.Kind, occ.FromNodeID = model.RelDependsOn, node
		}
		out = append(out, occ)
	}
	return out
}
