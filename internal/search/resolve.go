package search

// L2 owns this file: the Resolve endpoint's keyset paging over the tier-ordered exact and prefix candidates.

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/Sawmonabo/codectx/internal/model"
	"github.com/Sawmonabo/codectx/internal/provider"
	"github.com/Sawmonabo/codectx/internal/storage/sqlite"
)

// resolvePage is one page of the symbol endpoint: the nodes in tier order, the
// keyset continuation key ("" when the tiers are exhausted) and any completeness
// rows the operation itself contributes — today only the Section 11.6
// unavailable row for semantic_source=lsp. The caller pins the generation, signs
// LastKey into a cursor and supplies Binding and the reader's own completeness.
type resolvePage struct {
	Nodes []model.Node
	// Spans is the declaration byte interval of each node in Nodes, index for
	// index, nil where the node has none. The caller turns it into
	// model.Node.Range through the same bounded CAS window the search page
	// uses; carrying it here keeps the hydration out of the tier walk, which
	// has no content reader.
	Spans        []*model.ByteRange
	LastKey      string
	Completeness []model.CapabilityState
}

// add appends one candidate and the byte interval its position is hydrated
// from, keeping the two slices index for index.
func (p *resolvePage) add(n sqlite.StoredNode) {
	p.Nodes = append(p.Nodes, n.Node)
	p.Spans = append(p.Spans, n.Bytes)
}

// resolveSymbols answers one page of a SymbolRequest over the non-lexical
// tiers. req.Validate has already run; lastKey is the decoded Cursor.LastKey of
// the page being continued, empty for the first page.
//
// All four Section 18.1 operations are served here: resolve and
// workspace-symbols from the qualified-name, prefix and name tiers;
// document-symbols from the pinned file's nodes in document order; definition
// from the same tiers as resolve, filtered to declaration sites. Ambiguity is
// several nodes with a cursor, never a silently chosen first candidate
// (Section 14.1).
//
// model.Node.Range is hydrated by the caller from resolvePage.Spans, through
// the same bounded CAS window the search page builder uses: StoredNode carries
// byte intervals, and a candidate served without a position leaves the LINE
// column of `codectx symbol` empty for every row of every query. A node with no
// byte interval keeps a nil Range; nothing here invents a position.
func resolveSymbols(ctx context.Context, r exactReader, req model.SymbolRequest, lastKey string, limit int) (resolvePage, error) {
	if req.SemanticSource == model.SemanticLSP {
		return resolvePage{Completeness: []model.CapabilityState{lspUnavailable(req)}}, nil
	}
	limit = resolveLimit(limit)
	rank, within := noTierRank, ""
	if lastKey != "" {
		var err error
		if rank, within, err = parseResolveKey(lastKey); err != nil {
			return resolvePage{}, err
		}
	}
	if req.Operation == model.SymbolDocumentSymbols {
		return documentSymbols(ctx, r, req, rank, within, limit)
	}
	if page, ok, err := idCandidate(ctx, r, req, rank); err != nil || ok {
		return page, err
	}
	return symbolTiers(ctx, r, req, rank, within, limit)
}

// idCandidate is the canonical-id tier: `codectx symbol <name-or-id>` is
// spelled with an id and the Long text promises "canonical node ID", so a query
// that IS a canonical identifier resolves to the node it names rather than
// falling through to the name tiers, which index names and would answer an
// empty page.
//
// An identifier names exactly one node in a binding (Section 9.1), so the page
// is that node alone and carries no continuation key: there is no second
// candidate for a cursor to point at. A query that looks like an identifier but
// names nothing visible in this generation is not an error — ok is false and
// the name tiers answer the same query, which is what a repository whose
// symbols are spelled in hex needs.
func idCandidate(ctx context.Context, r exactReader, req model.SymbolRequest, rank int) (resolvePage, bool, error) {
	// A continuation is never issued for this tier, so a key here was written
	// by a symbol tier and that walk owns the request.
	if rank != noTierRank || !model.ValidHexID(req.Query) {
		return resolvePage{}, false, nil
	}
	n, err := r.Node(ctx, model.NodeID(req.Query))
	if err != nil {
		var typed *model.Error
		// "not visible in this generation" is storage's CTX_ARGUMENT_INVALID
		// (query.go:164); anything else is a real read failure and must not be
		// masked as "no such id".
		if errors.As(err, &typed) && typed.Code == model.CodeArgumentInvalid {
			return resolvePage{}, false, nil
		}
		return resolvePage{}, false, err
	}
	if req.Operation == model.SymbolDefinition && !isDeclaration(n) {
		return resolvePage{}, false, nil
	}
	var page resolvePage
	page.add(n)
	return page, true, nil
}

// resolveLimit is the last defence on the page bound: Service.pageLimit has
// already clamped the caller's limit to resources.max_page_items, and this
// keeps a direct call inside the package from exceeding the model ceiling.
func resolveLimit(limit int) int {
	if limit <= 0 || limit > model.MaxPageItems {
		return model.MaxPageItems
	}
	return limit
}

// resolveKey is digest §5's keyset key: the tier rank, then the within-tier
// keyset position that tier pages on. For the symbol tiers, which page on
// node_id, that position is the node id; for the exact_path tier, which pages
// on (start_byte, node_id), it is those two joined the same way. Both stay far
// inside Cursor.LastKey's 1024-byte cap.
func resolveKey(tier model.SearchTier, within string) string {
	return fmt.Sprintf("%d\x00%s", tier.Rank(), within)
}

// noTierRank is the rank of a request that carries no continuation key, chosen
// so it can never equal a real tier rank.
const noTierRank = -1

// fileNodeKey is the within-tier position of one node of the exact_path tier.
func fileNodeKey(n sqlite.StoredNode) string {
	var start int64
	if n.Bytes != nil {
		start = int64(n.Bytes.Start)
	}
	return strconv.FormatInt(start, 10) + "\x00" + string(n.Node.ID)
}

// parseResolveKey splits a continuation key into its tier rank and within-tier
// position. A key that is not one this endpoint wrote -- an empty key, an
// unparseable rank, a rank no symbol tier owns, or a within-tier position whose
// shape does not match that tier's keyset -- is CTX_CURSOR_INVALID rather than
// a silently restarted page. The caller supplies the first page's absence of a
// key; this function never treats "" as one.
func parseResolveKey(key string) (int, string, error) {
	head, within, ok := strings.Cut(key, "\x00")
	rank, err := strconv.Atoi(head)
	if !ok || err != nil || rank < 0 || rank >= model.TierLexicalFTS.Rank() || within == "" {
		return noTierRank, "", &model.Error{Code: model.CodeCursorInvalid, Message: "symbol cursor does not carry a symbol-tier position"}
	}
	// The within-tier position is the keyset of the tier that wrote it: the
	// exact_path tier pages on (start_byte, node_id), every symbol tier on
	// node_id alone. Checking the shape here is what makes a hand-written key
	// a rejected cursor instead of a storage argument error later.
	if rank == model.TierExactPath.Rank() {
		if _, _, err := splitFileNodeKey(within); err != nil {
			return noTierRank, "", err
		}
	} else if !model.ValidHexID(within) {
		return noTierRank, "", &model.Error{Code: model.CodeCursorInvalid, Message: "symbol cursor does not carry a node identity"}
	}
	return rank, within, nil
}

// splitFileNodeKey reads back what fileNodeKey wrote.
func splitFileNodeKey(within string) (int64, model.NodeID, error) {
	head, id, ok := strings.Cut(within, "\x00")
	start, err := strconv.ParseInt(head, 10, 64)
	if !ok || err != nil || start < 0 || id == "" {
		return 0, "", &model.Error{Code: model.CodeCursorInvalid, Message: "symbol cursor does not carry a document position"}
	}
	return start, model.NodeID(id), nil
}

// documentSymbols is the document-symbols operation: every visible node
// declared in the pinned file, in document order, keyset on (start_byte,
// node_id). SymbolRequest.Validate already requires the file id.
func documentSymbols(ctx context.Context, r exactReader, req model.SymbolRequest, rank int, within string, limit int) (resolvePage, error) {
	var afterStart int64
	var after model.NodeID
	if rank != noTierRank {
		if rank != model.TierExactPath.Rank() {
			return resolvePage{}, &model.Error{Code: model.CodeCursorInvalid, Message: "symbol cursor was not written by document-symbols"}
		}
		var err error
		if afterStart, after, err = splitFileNodeKey(within); err != nil {
			return resolvePage{}, err
		}
	}
	nodes, err := r.NodesInFile(ctx, req.FileID, afterStart, after, limit)
	if err != nil {
		return resolvePage{}, err
	}
	var page resolvePage
	for _, n := range nodes {
		page.add(n)
	}
	if len(page.Nodes) == limit {
		page.LastKey = resolveKey(model.TierExactPath, fileNodeKey(nodes[len(nodes)-1]))
	}
	return page, nil
}

// symbolTiers serves resolve, workspace-symbols and definition: the
// qualified-name, prefix and name tiers in ranking order, keyset on node_id
// within each tier, resuming at the tier the cursor stopped in. A node is
// emitted by its most specific tier only.
//
// The page carries a continuation key only when it filled: a short page means
// every remaining tier was drained, so there is nothing safe to continue to.
func symbolTiers(ctx context.Context, r exactReader, req model.SymbolRequest, rank int, within string, limit int) (resolvePage, error) {
	if rank != noTierRank && (rank < model.TierExactQualifiedName.Rank() || rank > model.TierExactName.Rank()) {
		return resolvePage{}, &model.Error{Code: model.CodeCursorInvalid, Message: "symbol cursor was not written by a symbol tier"}
	}
	declarationsOnly := req.Operation == model.SymbolDefinition
	var page resolvePage
	var lastTier model.SearchTier
	var lastNode model.NodeID
	for _, tier := range exactTiers {
		if tier.Rank() < rank {
			continue
		}
		after := model.NodeID("")
		if tier.Rank() == rank {
			after = model.NodeID(within)
		}
		for len(page.Nodes) < limit {
			nodes, err := r.Nodes(ctx, nodeFilterFor(tier, req.Query, nil), after, limit)
			if err != nil {
				return resolvePage{}, err
			}
			if len(nodes) == 0 {
				break
			}
			for _, n := range nodes {
				after = n.Node.ID
				if supersededByLowerTier(tier, n, req.Query) {
					continue
				}
				if declarationsOnly && !isDeclaration(n) {
					continue
				}
				page.add(n)
				lastTier, lastNode = tier, n.Node.ID
				if len(page.Nodes) == limit {
					break
				}
			}
			if len(nodes) < limit {
				break
			}
		}
		if len(page.Nodes) == limit {
			break
		}
	}
	if len(page.Nodes) == limit {
		page.LastKey = resolveKey(lastTier, string(lastNode))
	}
	return page, nil
}

// isDeclaration reports whether a node is a declaration site, which is what the
// definition operation answers with. The model has no is-declaration flag: a
// node declared in source carries the file it was declared in and the byte
// interval of its declaration, and a node without both (a manifest dependency,
// an unresolved provider-local entity) has no definition to point at.
func isDeclaration(n sqlite.StoredNode) bool {
	return n.Node.FileID != "" && n.Bytes != nil
}

// lspUnavailable is the Section 11.6 answer to semantic_source=lsp: an explicit
// unavailable capability for the operation that was asked for, never a silently
// substituted canonical answer. Task 13 is canonical-only, so this returns
// before any storage read. The capability is named with the operation's own
// Section 18.1 wire spelling, which is the thing the caller cannot have.
func lspUnavailable(req model.SymbolRequest) model.CapabilityState {
	id := "lsp"
	if req.Profile != "" {
		id += ":" + req.Profile
	}
	return model.CapabilityState{
		ProviderID:     id,
		Capability:     string(req.Operation),
		Scope:          provider.ScopeWorkspace,
		State:          model.CapabilityUnavailable,
		DiagnosticCode: model.CodeProviderUnavailable,
	}.WithDetail("semantic_source", string(model.SemanticLSP))
}
