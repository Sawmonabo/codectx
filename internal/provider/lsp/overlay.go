package lsp

import (
	"context"
	"encoding/json"
	"sync"

	"github.com/Sawmonabo/codectx/internal/model"
)

// Precision is the class every overlay answer carries (Section 9.3).
const Precision = model.PrecisionLanguageServer

// SemanticSource labels every overlay answer; it is never canonical.
const SemanticSource = model.SemanticLSP

// At is a byte-authoritative query position: a snapshot file and a zero-based
// byte offset into its pinned bytes.
type At struct {
	File model.FileID
	Byte uint64
}

// Location is one validated place in the pinned snapshot. Range is the
// half-open byte range with line/column context, converted from the server's
// coordinates against the exact retained bytes; Selection, when present, is
// the identifier inside a wider declaration range.
type Location struct {
	File        model.FileID       `json:"file_id"`
	Path        string             `json:"path"`
	ContentHash string             `json:"content_hash"`
	Range       model.SourceRange  `json:"range"`
	Selection   *model.SourceRange `json:"selection,omitempty"`
}

// Symbol is one document or workspace symbol.
type Symbol struct {
	Name      string         `json:"name"`
	Detail    string         `json:"detail,omitempty"`
	Container string         `json:"container,omitempty"`
	Kind      model.NodeKind `json:"kind"`
	Location  Location       `json:"location"`
}

// CallItem is a call-hierarchy participant. It is passed back to
// IncomingCalls and OutgoingCalls, which is why it retains the server's
// opaque item verbatim.
type CallItem struct {
	Symbol
	raw json.RawMessage
}

// Call is one edge of the call hierarchy: the item at the other end and the
// validated call sites. For an incoming call the sites are in Item's file
// (the caller); for an outgoing call they are in the queried item's file.
type Call struct {
	Item  CallItem   `json:"item"`
	Sites []Location `json:"sites"`
}

// Result is one bounded overlay answer. Items holds at most the requested
// limit; Truncated reports that the server returned more. Excluded counts
// locations the server returned outside the pinned snapshot — a standard
// library or dependency cache — which were neither followed nor opened. The
// answer is labelled with the server, its version and the input digest.
type Result[T any] struct {
	Items     []T                  `json:"items"`
	Truncated bool                 `json:"truncated"`
	Excluded  int                  `json:"excluded"`
	Overlay   model.OverlayBinding `json:"overlay"`
}

// Overlay is one caller's handle on a running server. Close releases it; the
// server itself stops after the idle TTL once no handle remains.
type Overlay struct {
	s    *server
	once sync.Once
}

// Capabilities reports which operations the server advertised.
func (o *Overlay) Capabilities() Capabilities { return o.s.caps }

// Binding is the label every result from this overlay carries.
func (o *Overlay) Binding() model.OverlayBinding { return o.s.binding }

// Close releases the handle. It is idempotent.
func (o *Overlay) Close() error {
	o.once.Do(o.s.release)
	return nil
}

// Definition answers textDocument/definition at the position.
func (o *Overlay) Definition(ctx context.Context, at At, limit int) (Result[Location], error) {
	return o.locations(ctx, "textDocument/definition", o.s.caps.Definition, at, nil, limit)
}

// TypeDefinition answers textDocument/typeDefinition at the position.
func (o *Overlay) TypeDefinition(ctx context.Context, at At, limit int) (Result[Location], error) {
	return o.locations(ctx, "textDocument/typeDefinition", o.s.caps.TypeDefinition, at, nil, limit)
}

// Implementations answers textDocument/implementation at the position.
func (o *Overlay) Implementations(ctx context.Context, at At, limit int) (Result[Location], error) {
	return o.locations(ctx, "textDocument/implementation", o.s.caps.Implementations, at, nil, limit)
}

// References answers textDocument/references at the position.
func (o *Overlay) References(ctx context.Context, at At, includeDeclaration bool, limit int) (Result[Location], error) {
	return o.locations(ctx, "textDocument/references", o.s.caps.References, at,
		&referenceContext{IncludeDeclaration: includeDeclaration}, limit)
}

// locations is the shared body of the four position-to-locations operations.
func (o *Overlay) locations(ctx context.Context, method string, supported bool, at At, refCtx *referenceContext, limit int) (Result[Location], error) {
	var out Result[Location]
	limit, err := o.begin(method, supported, limit)
	if err != nil {
		return out, err
	}
	doc, pos, err := o.position(ctx, at)
	if err != nil {
		return out, err
	}
	var params any = textDocumentPositionParams{TextDocument: textDocumentIdentifier{URI: o.s.uris.uri(doc.version.Path)}, Position: pos}
	if refCtx != nil {
		params = referenceParams{textDocumentPositionParams: params.(textDocumentPositionParams), Context: *refCtx}
	}
	var raw json.RawMessage
	if err := o.call(ctx, method, params, &raw); err != nil {
		return out, err
	}
	links, err := decodeLocations(method, raw)
	if err != nil {
		return out, err
	}
	out.Overlay = o.s.binding
	for _, l := range links {
		loc, ok, err := o.locate(ctx, l.TargetURI, l.TargetRange, l.TargetSelectionRange)
		if err != nil {
			return Result[Location]{}, err
		}
		if !ok {
			out.Excluded++
			continue
		}
		if len(out.Items) >= limit {
			out.Truncated = true
			break
		}
		out.Items = append(out.Items, loc)
	}
	return out, nil
}

// DocumentSymbols answers textDocument/documentSymbol for one file. A
// hierarchical answer is flattened in document order with Container set to
// the enclosing symbol's name.
func (o *Overlay) DocumentSymbols(ctx context.Context, file model.FileID, limit int) (Result[Symbol], error) {
	var out Result[Symbol]
	limit, err := o.begin("textDocument/documentSymbol", o.s.caps.DocumentSymbols, limit)
	if err != nil {
		return out, err
	}
	doc, notFound, err := o.s.document(ctx, file)
	if err != nil {
		return out, err
	}
	if notFound {
		return out, invalid("file %s is not in the pinned snapshot", file)
	}
	if err := o.s.ensureOpen(doc); err != nil {
		return out, err
	}
	var raw json.RawMessage
	if err := o.call(ctx, "textDocument/documentSymbol", documentSymbolParams{TextDocument: textDocumentIdentifier{URI: o.s.uris.uri(doc.version.Path)}}, &raw); err != nil {
		return out, err
	}
	out.Overlay = o.s.binding
	if len(raw) == 0 || string(raw) == "null" {
		return out, nil
	}
	var probe []struct {
		Location *json.RawMessage `json:"location"`
	}
	if err := json.Unmarshal(raw, &probe); err != nil {
		return out, outputInvalid("textDocument/documentSymbol returned a result of the wrong shape: %v", err)
	}
	if len(probe) > 0 && probe[0].Location != nil {
		var flat []symbolInformation
		if err := json.Unmarshal(raw, &flat); err != nil {
			return out, outputInvalid("textDocument/documentSymbol returned malformed SymbolInformation: %v", err)
		}
		for _, si := range flat {
			if len(out.Items) >= limit {
				out.Truncated = true
				break
			}
			loc, ok, err := o.locate(ctx, si.Location.URI, si.Location.Range, nil)
			if err != nil {
				return Result[Symbol]{}, err
			}
			if !ok {
				out.Excluded++
				continue
			}
			out.Items = append(out.Items, Symbol{Name: si.Name, Container: si.ContainerName, Kind: nodeKindOf(si.Kind), Location: loc})
		}
		return out, nil
	}
	var tree []documentSymbol
	if err := json.Unmarshal(raw, &tree); err != nil {
		return out, outputInvalid("textDocument/documentSymbol returned malformed DocumentSymbol: %v", err)
	}
	if err := o.flatten(doc, tree, "", limit, &out); err != nil {
		return Result[Symbol]{}, err
	}
	return out, nil
}

// flatten walks a DocumentSymbol tree in order, validating every range in
// the queried document. Depth is bounded by the frame's JSON depth check.
func (o *Overlay) flatten(doc *document, symbols []documentSymbol, container string, limit int, out *Result[Symbol]) error {
	for _, ds := range symbols {
		if len(out.Items) >= limit {
			out.Truncated = true
			return nil
		}
		rng, err := doc.rangeOf(ds.Range, o.s.enc)
		if err != nil {
			return outputInvalid("textDocument/documentSymbol range for %q does not describe %s: %v", truncate(ds.Name, 64), doc.version.Path, err)
		}
		sel, err := doc.rangeOf(ds.SelectionRange, o.s.enc)
		if err != nil {
			return outputInvalid("textDocument/documentSymbol selection range for %q does not describe %s: %v", truncate(ds.Name, 64), doc.version.Path, err)
		}
		out.Items = append(out.Items, Symbol{
			Name: ds.Name, Detail: ds.Detail, Container: container, Kind: nodeKindOf(ds.Kind),
			Location: Location{File: doc.version.ID, Path: doc.version.Path, ContentHash: doc.version.ContentHash, Range: rng, Selection: &sel},
		})
		if err := o.flatten(doc, ds.Children, ds.Name, limit, out); err != nil {
			return err
		}
	}
	return nil
}

// WorkspaceSymbols answers workspace/symbol for a query string.
func (o *Overlay) WorkspaceSymbols(ctx context.Context, query string, limit int) (Result[Symbol], error) {
	var out Result[Symbol]
	limit, err := o.begin("workspace/symbol", o.s.caps.WorkspaceSymbols, limit)
	if err != nil {
		return out, err
	}
	if len(query) > model.MaxQueryTextBytes {
		return out, invalid("workspace symbol query exceeds %d bytes", model.MaxQueryTextBytes)
	}
	var symbols []workspaceSymbol
	if err := o.call(ctx, "workspace/symbol", workspaceSymbolParams{Query: query}, &symbols); err != nil {
		return out, err
	}
	out.Overlay = o.s.binding
	for _, ws := range symbols {
		if len(out.Items) >= limit {
			out.Truncated = true
			break
		}
		var l location
		if err := json.Unmarshal(ws.Location, &l); err != nil || l.URI == "" {
			return Result[Symbol]{}, outputInvalid("workspace/symbol returned %q without a full location", truncate(ws.Name, 64))
		}
		var probe struct {
			Range *lspRange `json:"range"`
		}
		if json.Unmarshal(ws.Location, &probe) != nil || probe.Range == nil {
			// This client did not advertise resolve support, so a location
			// without a range is a result that cannot be validated.
			return Result[Symbol]{}, outputInvalid("workspace/symbol returned %q without a range", truncate(ws.Name, 64))
		}
		loc, ok, err := o.locate(ctx, l.URI, l.Range, nil)
		if err != nil {
			return Result[Symbol]{}, err
		}
		if !ok {
			out.Excluded++
			continue
		}
		out.Items = append(out.Items, Symbol{Name: ws.Name, Container: ws.ContainerName, Kind: nodeKindOf(ws.Kind), Location: loc})
	}
	return out, nil
}

// PrepareCallHierarchy answers textDocument/prepareCallHierarchy at the
// position; the returned items feed IncomingCalls and OutgoingCalls.
func (o *Overlay) PrepareCallHierarchy(ctx context.Context, at At, limit int) (Result[CallItem], error) {
	var out Result[CallItem]
	limit, err := o.begin("textDocument/prepareCallHierarchy", o.s.caps.CallHierarchy, limit)
	if err != nil {
		return out, err
	}
	doc, pos, err := o.position(ctx, at)
	if err != nil {
		return out, err
	}
	var items []json.RawMessage
	if err := o.call(ctx, "textDocument/prepareCallHierarchy",
		textDocumentPositionParams{TextDocument: textDocumentIdentifier{URI: o.s.uris.uri(doc.version.Path)}, Position: pos}, &items); err != nil {
		return out, err
	}
	out.Overlay = o.s.binding
	for _, raw := range items {
		if len(out.Items) >= limit {
			out.Truncated = true
			break
		}
		item, ok, err := o.callItem(ctx, raw)
		if err != nil {
			return Result[CallItem]{}, err
		}
		if !ok {
			out.Excluded++
			continue
		}
		out.Items = append(out.Items, item)
	}
	return out, nil
}

// IncomingCalls answers callHierarchy/incomingCalls for a prepared item.
func (o *Overlay) IncomingCalls(ctx context.Context, item CallItem, limit int) (Result[Call], error) {
	return o.calls(ctx, "callHierarchy/incomingCalls", item, limit)
}

// OutgoingCalls answers callHierarchy/outgoingCalls for a prepared item.
func (o *Overlay) OutgoingCalls(ctx context.Context, item CallItem, limit int) (Result[Call], error) {
	return o.calls(ctx, "callHierarchy/outgoingCalls", item, limit)
}

func (o *Overlay) calls(ctx context.Context, method string, item CallItem, limit int) (Result[Call], error) {
	var out Result[Call]
	limit, err := o.begin(method, o.s.caps.CallHierarchy, limit)
	if err != nil {
		return out, err
	}
	if len(item.raw) == 0 {
		return out, invalid("the call hierarchy item was not returned by PrepareCallHierarchy on this overlay")
	}
	var raw json.RawMessage
	if err := o.call(ctx, method, callHierarchyCallsParams{Item: item.raw}, &raw); err != nil {
		return out, err
	}
	out.Overlay = o.s.binding
	if len(raw) == 0 || string(raw) == "null" {
		return out, nil
	}
	incoming := method == "callHierarchy/incomingCalls"
	var edges []struct {
		From       json.RawMessage `json:"from"`
		To         json.RawMessage `json:"to"`
		FromRanges []lspRange      `json:"fromRanges"`
	}
	if err := json.Unmarshal(raw, &edges); err != nil {
		return out, outputInvalid("%s returned a result of the wrong shape: %v", method, err)
	}
	for _, e := range edges {
		if len(out.Items) >= limit {
			out.Truncated = true
			break
		}
		other := e.To
		if incoming {
			other = e.From
		}
		peer, ok, err := o.callItem(ctx, other)
		if err != nil {
			return Result[Call]{}, err
		}
		if !ok {
			out.Excluded++
			continue
		}
		// Call sites are in the caller's document: the peer for an incoming
		// call, the queried item for an outgoing one.
		siteFile := item.Location.File
		if incoming {
			siteFile = peer.Location.File
		}
		doc, notFound, err := o.s.document(ctx, siteFile)
		if err != nil {
			return Result[Call]{}, err
		}
		if notFound {
			out.Excluded++
			continue
		}
		call := Call{Item: peer}
		for _, r := range e.FromRanges {
			rng, err := doc.rangeOf(r, o.s.enc)
			if err != nil {
				return Result[Call]{}, outputInvalid("%s call site does not describe %s: %v", method, doc.version.Path, err)
			}
			call.Sites = append(call.Sites, Location{File: doc.version.ID, Path: doc.version.Path, ContentHash: doc.version.ContentHash, Range: rng})
			if len(call.Sites) >= model.MaxRecordsPerResult {
				break
			}
		}
		out.Items = append(out.Items, call)
	}
	return out, nil
}

// callItem validates one CallHierarchyItem. ok is false for an item outside
// the pinned snapshot.
func (o *Overlay) callItem(ctx context.Context, raw json.RawMessage) (CallItem, bool, error) {
	var it callHierarchyItem
	if err := json.Unmarshal(raw, &it); err != nil {
		return CallItem{}, false, outputInvalid("call hierarchy item is malformed: %v", err)
	}
	loc, ok, err := o.locate(ctx, it.URI, it.Range, &it.SelectionRange)
	if err != nil || !ok {
		return CallItem{}, ok, err
	}
	return CallItem{Symbol: Symbol{Name: it.Name, Detail: it.Detail, Kind: nodeKindOf(it.Kind), Location: loc}, raw: raw}, true, nil
}

// begin checks the server is serving and the operation is advertised, and
// normalizes the page limit: zero is the default page, anything over the
// page bound or negative is invalid.
func (o *Overlay) begin(method string, supported bool, limit int) (int, error) {
	if err := o.s.running(); err != nil {
		return 0, err
	}
	if !supported {
		return 0, unavailable("the language server does not advertise %s", method).
			WithDetail("method", method).WithDetail("reason", "unsupported").WithDetail("profile", o.s.profile.Name)
	}
	switch {
	case limit < 0 || limit > model.MaxPageItems:
		return 0, invalid("limit %d must be between 0 and %d", limit, model.MaxPageItems)
	case limit == 0:
		return model.MaxPageItems, nil
	}
	return limit, nil
}

// position loads the queried document, synchronizes it to the server and
// converts the byte offset to the negotiated encoding.
func (o *Overlay) position(ctx context.Context, at At) (*document, position, error) {
	doc, notFound, err := o.s.document(ctx, at.File)
	if err != nil {
		return nil, position{}, err
	}
	if notFound {
		return nil, position{}, invalid("file %s is not in the pinned snapshot", at.File)
	}
	if !isText(doc.data) {
		return nil, position{}, invalid("file %s is not UTF-8 text; a language server cannot address positions in it", doc.version.Path)
	}
	if err := o.s.ensureOpen(doc); err != nil {
		return nil, position{}, err
	}
	pos, err := doc.positionOf(at.Byte, o.s.enc)
	if err != nil {
		return nil, position{}, err
	}
	return doc, pos, nil
}

// call issues one request under the request timeout.
func (o *Overlay) call(ctx context.Context, method string, params, result any) error {
	ctx, cancel := context.WithTimeout(ctx, o.s.opts.RequestTimeout)
	defer cancel()
	if err := o.s.conn.call(ctx, method, params, result); err != nil {
		return err
	}
	return nil
}

// locate validates one server location: a file URI that maps into the
// materialization and names a snapshot file, and a range that describes that
// file's exact pinned bytes. ok is false for a location outside the snapshot;
// a malformed URI or a range that does not fit the bytes is an error, because
// a wrong position is a wrong source attribution.
func (o *Overlay) locate(ctx context.Context, uri string, rng lspRange, selection *lspRange) (Location, bool, error) {
	rel, ok, err := o.s.uris.relOf(uri)
	if err != nil || !ok {
		return Location{}, false, err
	}
	doc, notFound, err := o.s.document(ctx, model.NewFileID(o.s.view.Header().RepositoryID, rel))
	if err != nil {
		return Location{}, false, err
	}
	if notFound {
		return Location{}, false, nil
	}
	r, err := doc.rangeOf(rng, o.s.enc)
	if err != nil {
		return Location{}, false, outputInvalid("location range does not describe %s: %v", rel, err)
	}
	loc := Location{File: doc.version.ID, Path: doc.version.Path, ContentHash: doc.version.ContentHash, Range: r}
	if selection != nil {
		s, err := doc.rangeOf(*selection, o.s.enc)
		if err != nil {
			return Location{}, false, outputInvalid("selection range does not describe %s: %v", rel, err)
		}
		loc.Selection = &s
	}
	return loc, true, nil
}

// decodeLocations normalizes the definition-family result shapes — a single
// Location, a Location array, a LocationLink array or null — to links.
func decodeLocations(method string, raw json.RawMessage) ([]locationLink, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	if raw[0] == '{' {
		var l location
		if err := json.Unmarshal(raw, &l); err != nil {
			return nil, outputInvalid("%s returned a malformed location: %v", method, err)
		}
		return []locationLink{{TargetURI: l.URI, TargetRange: l.Range}}, nil
	}
	var probe []struct {
		URI       string `json:"uri"`
		TargetURI string `json:"targetUri"`
	}
	if err := json.Unmarshal(raw, &probe); err != nil {
		return nil, outputInvalid("%s returned a result of the wrong shape: %v", method, err)
	}
	if len(probe) == 0 {
		return nil, nil
	}
	if probe[0].TargetURI != "" {
		var links []locationLink
		if err := json.Unmarshal(raw, &links); err != nil {
			return nil, outputInvalid("%s returned malformed location links: %v", method, err)
		}
		return links, nil
	}
	var locs []location
	if err := json.Unmarshal(raw, &locs); err != nil {
		return nil, outputInvalid("%s returned malformed locations: %v", method, err)
	}
	links := make([]locationLink, len(locs))
	for i, l := range locs {
		links[i] = locationLink{TargetURI: l.URI, TargetRange: l.Range}
	}
	return links, nil
}
