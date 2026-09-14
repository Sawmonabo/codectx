package graph

import (
	"context"
	"sort"

	"github.com/Sawmonabo/codectx/internal/model"
	"github.com/Sawmonabo/codectx/internal/pagination"
)

// referenceEndpoint binds a reference continuation to this operation, so
// pagination.Signer.DecodeCursor rejects a cursor signed for a different
// endpoint whose sort key means nothing here. It does NOT distinguish two
// reference queries on the same endpoint: pagination.Cursor.QueryHash is what
// pins the node and operation, and this engine mints no cursor to put a hash
// in (see checkReferenceCursor), so a same-generation cursor for a different
// node would resume at a foreign keyset position once tokens exist.
const referenceEndpoint = "graph.references"

// referenceWalk is the direction and relation allowlist one reference
// operation walks. The mapping is fixed here rather than taken from the
// request: a reference query names an operation, not a traversal, and letting
// a caller widen the allowlist would turn "who calls this" into an arbitrary
// neighbourhood expansion under a name that promises otherwise.
type referenceWalk struct {
	direction model.Direction
	kinds     []model.RelationKind
}

// referenceWalkFor maps a reference operation onto the canonical relation
// vocabulary.
//
//   - references:      incoming `references` and `calls` -- every canonical
//     edge that names this node as its target is a place the
//     symbol is used.
//   - implements:      incoming `implements` -- the implementors of an
//     interface, which is the direction that answers
//     "who implements this", not "what does this implement".
//   - type-definition: outgoing `references` -- the vocabulary has no
//     dedicated type-of edge, and a symbol's reference to
//     its type is sealed as an outgoing `references` edge.
//
// An operation with no walk is rejected with CTX_ARGUMENT_INVALID rather than
// answered with an arbitrary neighbouring one: this is also the engine's own
// check that the request names a known operation.
func referenceWalkFor(op model.ReferenceOperation) (referenceWalk, error) {
	switch op {
	case model.ReferenceReferences:
		return referenceWalk{model.DirectionIncoming,
			[]model.RelationKind{model.RelReferences, model.RelCalls}}, nil
	case model.ReferenceImplements:
		return referenceWalk{model.DirectionIncoming,
			[]model.RelationKind{model.RelImplements}}, nil
	case model.ReferenceTypeDefinition:
		return referenceWalk{model.DirectionOutgoing,
			[]model.RelationKind{model.RelReferences}}, nil
	}
	return referenceWalk{}, (&model.Error{Code: model.CodeArgumentInvalid,
		Message: "reference operation is not a known reference operation"}).
		WithDetail("operation", string(op))
}

// References answers a canonical reference query over one resolved node.
//
// The Section 9.2 distinction it exists to preserve: a *relation* is one sealed
// canonical edge, an *occurrence* is one evidence row backing that edge
// (schema.sql:212 -- evidence carries the precision, file and byte interval,
// the relation row carries none of them). A symbol referenced twice inside one
// relation is one relation and two occurrences. This method emits one item per
// occurrence and never merges the occurrences of a relation into a single item,
// so a caller counting items gets the occurrence count and a caller counting
// distinct RelationIDs gets the relation count. Pages therefore end on a
// relation boundary: splitting one relation's occurrences across two pages is
// exactly what would let a consumer double-count the relation or collapse the
// occurrences. The one exception is a single relation carrying more
// occurrences than a whole page holds; that page is clipped and reported as
// truncated, because the alternative is an unbounded page.
//
// A request naming the lsp semantic source is answered with an
// unavailable-capability row and no records. It is never answered canonically:
// Section 11.6 forbids substituting a canonical answer for an unsupported LSP
// method, and Meta.Overlay stays nil because this engine never binds an
// overlay.
//
// A canonical relation with no evidence row contributes no occurrence and is
// not reported, because an occurrence is an evidence row: a reference with no
// location is not an answerable reference.
func (e *Engine) References(ctx context.Context, req model.ReferenceRequest) (model.Page[model.ReferenceOccurrence], error) {
	// The engine checks only what it must to answer safely. It deliberately
	// does not re-run model.ReferenceRequest.Validate: that is the request
	// boundary's job (id spelling, page/generation exclusivity, profile
	// bounds), and this engine is specified to accept already-resolved ids
	// from a caller that has already validated them. Re-validating here would
	// make the engine's contract depend on the wire id format rather than on
	// the facts it reads.
	if req.NodeID == "" {
		return model.Page[model.ReferenceOccurrence]{}, &model.Error{
			Code: model.CodeArgumentInvalid, Message: "reference query requires a resolved node id"}
	}
	if !req.SemanticSource.Valid() {
		return model.Page[model.ReferenceOccurrence]{}, (&model.Error{
			Code:    model.CodeArgumentInvalid,
			Message: "reference semantic source is not a known semantic source"}).
			WithDetail("semantic_source", string(req.SemanticSource))
	}
	walk, err := referenceWalkFor(req.Operation)
	if err != nil {
		return model.Page[model.ReferenceOccurrence]{}, err
	}
	binding := e.adjacency.Binding()

	if req.SemanticSource == model.SemanticLSP {
		// No traversal, no gate and no deadline: the answer is the disclosure
		// itself, and running the canonical walk anyway would be the silent
		// substitution Section 11.6 forbids.
		return model.Page[model.ReferenceOccurrence]{
			Meta: model.QueryMeta{
				Binding: binding,
				Completeness: []model.CapabilityState{{
					ProviderID:     "lsp",
					Capability:     string(req.Operation),
					Scope:          "workspace",
					State:          model.CapabilityUnavailable,
					DiagnosticCode: model.CodeProviderUnavailable,
					Details:        map[string]string{"reason": "semantic_source_unavailable"},
				}},
			},
		}, nil
	}

	if e.gate != nil {
		if err := e.gate.Acquire(ctx); err != nil {
			return model.Page[model.ReferenceOccurrence]{}, err
		}
		defer e.gate.Release()
	}
	ctx, cancel := context.WithDeadline(ctx, e.now().Add(e.limits.QueryTimeout))
	defer cancel()

	after, err := e.resumeReferences(req)
	if err != nil {
		return model.Page[model.ReferenceOccurrence]{}, err
	}

	pageLimit := req.Page.Limit
	if pageLimit <= 0 || pageLimit > e.limits.MaxPageItems {
		pageLimit = e.limits.MaxPageItems
	}
	if pageLimit > model.MaxPageItems {
		pageLimit = model.MaxPageItems
	}

	items, more, clipped, err := e.referencePage(ctx, req.NodeID, walk, after, pageLimit)
	if err != nil {
		return model.Page[model.ReferenceOccurrence]{}, err
	}

	meta := model.QueryMeta{Binding: binding}
	switch {
	case clipped:
		// A single relation carried more occurrences than one page may hold.
		// Reporting it as complete would understate how often the symbol is
		// used at that one edge.
		meta.Truncated = true
		meta.TruncationReason = "a relation carries more occurrences than one page holds"
	case more:
		// The keyset walk stopped on a relation boundary with relations left.
		// A continuation token cannot be minted here: pagination.Cursor pins
		// the generation lease that retains the pinned facts, and the frozen
		// Adjacency port exposes a Binding but no lease id. Until that is
		// threaded through, the honest answer is a bounded page that says so.
		meta.Truncated = true
		meta.TruncationReason = "more reference occurrences remain beyond this page"
	}
	return model.Page[model.ReferenceOccurrence]{Meta: meta, Items: items}, nil
}

// resumeReferences turns a presented cursor into the keyset position to resume
// from. A cursor without a signer cannot be verified at all, and an unverified
// continuation is a request to read from a position nothing vouched for, so it
// is rejected rather than trusted.
func (e *Engine) resumeReferences(req model.ReferenceRequest) (model.RelationID, error) {
	if req.Page.Cursor == "" {
		return "", nil
	}
	if e.signer == nil {
		return "", (&model.Error{Code: model.CodeCursorInvalid,
			Message: "this workspace does not offer query continuations"}).
			WithDetail("endpoint", referenceEndpoint)
	}
	c, err := e.signer.DecodeCursor(req.Page.Cursor, referenceEndpoint, e.now())
	if err != nil {
		return "", err
	}
	if err := e.checkReferenceCursor(c); err != nil {
		return "", err
	}
	if c.SpoolID != "" {
		// A spool-carrying cursor has an empty LastKey, so honouring it would
		// resume at position zero and silently re-serve page one as if it were
		// page two. References spools nothing; such a token is not ours to read.
		return "", (&model.Error{Code: model.CodeCursorInvalid,
			Message: "cursor carries spooled traversal state this endpoint does not produce"}).
			WithDetail("endpoint", referenceEndpoint)
	}
	return model.RelationID(c.LastKey), nil
}

// checkReferenceCursor rejects a verified cursor that belongs to a different
// pinned generation. The signature proves the token is ours; it does not prove
// it describes this generation's facts, and resuming a keyset walk against a
// different generation would page through facts the first page never saw.
func (e *Engine) checkReferenceCursor(c pagination.Cursor) error {
	b := e.adjacency.Binding()
	if c.GenerationID != b.GenerationID || c.AnalysisKey != b.AnalysisKey {
		return (&model.Error{Code: model.CodeCursorInvalid,
			Message: "cursor was issued against a different generation"}).
			WithDetail("endpoint", referenceEndpoint)
	}
	return nil
}

// referencePage walks the relations touching node in keyset order after `after`
// and hydrates their occurrences in bounded batches: one Edges round trip per
// adjacencyBatch relations and one EvidenceFor round trip per batch, never one
// query per edge. It returns the page's occurrences, whether relations remain
// beyond the page, and whether one relation's occurrences had to be clipped to
// the page bound.
func (e *Engine) referencePage(ctx context.Context, node model.NodeID, walk referenceWalk,
	after model.RelationID, pageLimit int) (items []model.ReferenceOccurrence, more, clipped bool, err error) {
	seeds := []model.NodeID{node}
	for {
		rels, err := e.adjacency.Edges(ctx, seeds, walk.direction, walk.kinds, after, adjacencyBatch)
		if err != nil {
			return nil, false, false, err
		}
		if len(rels) == 0 {
			return items, false, clipped, nil
		}
		ids := make([]model.RelationID, 0, len(rels))
		for _, r := range rels {
			ids = append(ids, r.ID)
		}
		// pageLimit+1 per relation distinguishes "exactly a page of
		// occurrences" from "more than a page", so the clipped flag is never
		// raised for a relation that happens to fill the page exactly.
		//
		// This reads Adjacency.EvidenceFor's limit as a PER-RELATION cap, not a
		// total row cap across the batch. A total-cap implementation would drop
		// the evidence of nearly every relation in a 256-relation batch and make
		// this method silently under-report occurrences, which is the one thing
		// it exists to get right. The storage implementation is bound by this.
		ev, err := e.adjacency.EvidenceFor(ctx, ids, pageLimit+1)
		if err != nil {
			return nil, false, false, err
		}
		for _, r := range rels {
			occ := ev[r.ID]
			if len(occ) == 0 {
				after = r.ID
				continue
			}
			if len(occ) > pageLimit {
				occ, clipped = occ[:pageLimit], true
			}
			if len(items) > 0 && len(items)+len(occ) > pageLimit {
				// Stop on the relation boundary: this relation belongs whole
				// to the next page.
				return items, true, clipped, nil
			}
			sorted := append([]model.EvidenceID(nil), occ...)
			sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
			for _, id := range sorted {
				items = append(items, model.ReferenceOccurrence{
					RelationID: r.ID,
					EvidenceID: id,
					Kind:       r.Kind,
					FromNodeID: r.From,
					ToNodeID:   r.To,
					// Precision, FileID, Path and Range live on the evidence
					// row, and the frozen Adjacency port returns evidence as
					// ids only. They are left empty rather than invented: a
					// fabricated precision class would misreport how the fact
					// was derived. Filling them needs EvidenceFor to return
					// rows, not ids -- a change to graph.go that this lane does
					// not own.
					SemanticSource: model.SemanticCanonical,
				})
			}
			after = r.ID
			if len(items) >= pageLimit || clipped {
				more, err := e.hasMoreRelations(ctx, seeds, walk, after)
				if err != nil {
					return nil, false, false, err
				}
				return items, more, clipped, nil
			}
		}
		if len(rels) < adjacencyBatch {
			return items, false, clipped, nil
		}
	}
}

// hasMoreRelations probes for a single relation past the page's last key, so a
// page that ends exactly on the bound reports truncation only when something
// really remains. The probe runs on the deadline-bounded context after the walk
// has already spent the budget, so it is the call most likely to fail; a
// failure is propagated rather than read as "nothing remains", because the
// latter would publish a bounded page as the complete set of references.
func (e *Engine) hasMoreRelations(ctx context.Context, seeds []model.NodeID,
	walk referenceWalk, after model.RelationID) (bool, error) {
	rels, err := e.adjacency.Edges(ctx, seeds, walk.direction, walk.kinds, after, 1)
	if err != nil {
		return false, err
	}
	return len(rels) > 0, nil
}
