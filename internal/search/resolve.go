package search

// L2 owns this file: the Resolve endpoint's keyset paging over the tier-ordered exact and prefix candidates.

import (
	"context"
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
	Nodes        []model.Node
	LastKey      string
	Completeness []model.CapabilityState
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
// model.Node.Range stays nil: StoredNode carries byte intervals by design and
// position hydration is the paged, CAS-bounded step Q10 assigns to the search
// page builder. Resolve's callers get Bytes-derived positions there or not at
// all; nothing here invents a position.
func resolveSymbols(ctx context.Context, r exactReader, req model.SymbolRequest, lastKey string, limit int) (resolvePage, error) {
	if req.SemanticSource == model.SemanticLSP {
		return resolvePage{Completeness: []model.CapabilityState{lspUnavailable(req)}}, nil
	}
	limit = resolveLimit(limit)
	rank, within, err := parseResolveKey(lastKey)
	if err != nil {
		return resolvePage{}, err
	}
	if req.Operation == model.SymbolDocumentSymbols {
		return documentSymbols(ctx, r, req, rank, within, limit)
	}
	return symbolTiers(ctx, r, req, rank, within, limit)
}

// resolveLimit applies the Section 20.1 page bound; zero means the endpoint
// default, which for a symbol page is the maximum.
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
// position; the first page has no key and reports rank noTierRank. A key that is
// not one this endpoint wrote is CTX_CURSOR_INVALID rather than a silently
// restarted page.
func parseResolveKey(key string) (int, string, error) {
	if key == "" {
		return noTierRank, "", nil
	}
	head, within, ok := strings.Cut(key, "\x00")
	rank, err := strconv.Atoi(head)
	if !ok || err != nil || rank < 0 || rank >= model.TierLexicalFTS.Rank() || within == "" {
		return noTierRank, "", &model.Error{Code: model.CodeCursorInvalid, Message: "symbol cursor does not carry a symbol-tier position"}
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
	page := resolvePage{Nodes: make([]model.Node, 0, len(nodes))}
	for _, n := range nodes {
		page.Nodes = append(page.Nodes, n.Node)
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
	page := resolvePage{Nodes: make([]model.Node, 0, limit)}
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
				page.Nodes = append(page.Nodes, n.Node)
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
