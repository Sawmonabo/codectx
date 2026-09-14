package graph

import (
	"context"
	"sort"

	"github.com/Sawmonabo/codectx/internal/model"
	"github.com/Sawmonabo/codectx/internal/pagination"
)

// referenceEndpoint binds a reference continuation to this operation, so
// pagination.Signer.DecodeCursor rejects a cursor signed for a different
// endpoint whose sort key means nothing here. The endpoint alone does not
// distinguish two reference queries: referenceQueryHash pins the node and the
// operation, and checkReferenceCursor compares it, so a cursor minted for one
// symbol can never resume another symbol's walk at a foreign keyset position.
const referenceEndpoint = "graph.references"

// referenceQueryHashDomain is the Section 9.1 hash domain for the normalized
// reference query a continuation is bound to.
const referenceQueryHashDomain = "graph.references.query"

// referenceQueryHash is the normalized query identity of one reference walk.
// The keyset position a cursor carries is a RelationID in the order produced by
// THIS node and THIS operation; presenting it to any other reference query is
// CTX_CURSOR_INVALID rather than a silently repinned answer.
func referenceQueryHash(node model.NodeID, op model.ReferenceOperation) string {
	h := model.NewHasher(referenceQueryHashDomain)
	h.AddString(string(node))
	h.AddString(string(op))
	return h.Sum()
}

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
func (e *Engine) References(ctx context.Context, req model.ReferenceRequest) (page model.Page[model.ReferenceOccurrence], err error) {
	defer func() { err = typedContextError(ctx, err) }()
	// The landed model validator is the request contract -- resolved id
	// spelling, the operation and semantic-source vocabularies, the profile
	// bound and the generation/cursor exclusivity of Section 7. The engine runs
	// it rather than a private subset of it, so a request this engine accepts
	// is exactly a request the facade accepts.
	if err := req.Validate(); err != nil {
		return model.Page[model.ReferenceOccurrence]{}, err
	}
	walk, err := referenceWalkFor(req.Operation)
	if err != nil {
		return model.Page[model.ReferenceOccurrence]{}, err
	}
	binding := e.adjacency.Binding()

	if req.SemanticSource == model.SemanticLSP {
		// No traversal, no gate and no deadline: the answer is the disclosure
		// itself, and running the canonical walk anyway would be the silent
		// substitution Section 11.6 forbids. The page still validates before it
		// leaves -- this is the one return path that used to skip it, and an
		// unavailability disclosure is no more exempt from its own contract
		// than an answer is.
		page = model.Page[model.ReferenceOccurrence]{
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
		}
		if err := page.Validate(); err != nil {
			return model.Page[model.ReferenceOccurrence]{}, err
		}
		return page, nil
	}

	// The deadline wraps the gate as well as the walk, exactly as the traversal
	// and path entries do it: waiting for a slot is admitted work like any
	// other, so waiting past the request deadline is the CTX_RESOURCE_LIMIT the
	// caller must see rather than an unbounded queue. `codectx refs` carries no
	// deadline of its own unless --timeout is set, so acquiring first would
	// block behind two concurrent graph queries forever.
	ctx, cancel := context.WithDeadline(ctx, e.now().Add(e.limits.QueryTimeout))
	defer cancel()
	if e.gate != nil {
		if err := e.gate.Acquire(ctx); err != nil {
			return model.Page[model.ReferenceOccurrence]{}, err
		}
		defer e.gate.Release()
	}

	queryHash := referenceQueryHash(req.NodeID, req.Operation)
	after, err := e.resumeReferences(req, queryHash)
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

	items, last, more, clipped, err := e.referencePage(ctx, req.NodeID, walk, after, pageLimit)
	if err != nil {
		return model.Page[model.ReferenceOccurrence]{}, err
	}
	if err := e.nameOrigins(ctx, items); err != nil {
		return model.Page[model.ReferenceOccurrence]{}, err
	}

	// The same capability disclosure every other graph answer carries, from the
	// same helper, so `refs` reports the generation's capabilities rather than
	// none. The deferred-dependence flag is discarded deliberately: a reference
	// operation walks references, calls or implements (referenceWalkFor), none
	// of which is a dependence-only kind, so there is nothing for it to report.
	caps, _, err := e.completeness(ctx, walk.kinds)
	if err != nil {
		return model.Page[model.ReferenceOccurrence]{}, err
	}
	meta := model.QueryMeta{Binding: binding, Completeness: caps}
	if clipped {
		// A single relation carried more occurrences than one page may hold.
		// Those occurrences are unrecoverable once the keyset position moves
		// past their relation, so this is real truncation even when a
		// continuation is offered for the relations that follow.
		meta.Truncated = true
		meta.TruncationReason = "a relation carries more occurrences than one page holds"
	}
	if more {
		// The keyset walk stopped on a relation boundary with relations left.
		token, err := e.nextReferenceCursor(ctx, queryHash, last)
		if err != nil {
			return model.Page[model.ReferenceOccurrence]{}, err
		}
		meta.NextCursor = token
		if token == "" && !meta.Truncated {
			// No signer, or no lease store to bind the token to: the remaining
			// relations cannot be reached, so the page is truncated and says so
			// rather than presenting a bounded page as the complete set.
			meta.Truncated = true
			meta.TruncationReason = "more reference occurrences remain beyond this page"
		}
	}
	page = model.Page[model.ReferenceOccurrence]{Meta: meta, Items: items}
	if err := page.Validate(); err != nil {
		return model.Page[model.ReferenceOccurrence]{}, err
	}
	for i := range items {
		if err := items[i].Validate(); err != nil {
			return model.Page[model.ReferenceOccurrence]{}, err
		}
	}
	return page, nil
}

// nameOrigins fills each occurrence's FromName from ONE batched node read per
// page. It runs here, on the whole page, rather than inside referencePage: that
// method returns from four places, and decorating inside it would leave some
// pages named and others blank.
//
// The read is bounded by construction -- a page holds at most
// model.MaxPageItems occurrences, so it can name at most that many distinct
// from-nodes, well inside the storage batch limit -- so there is no chunking
// and, above all, no per-row lookup.
//
// A node the pinned generation does not publish simply gets no name; the id is
// still there and the renderers fall back to it. A read FAILURE is propagated
// instead, for the same reason hasMoreRelations propagates its probe: a
// swallowed error would make "this generation publishes no fact for that node"
// indistinguishable from "the store could not be read".
func (e *Engine) nameOrigins(ctx context.Context, items []model.ReferenceOccurrence) error {
	ids := make([]model.NodeID, 0, len(items))
	seen := make(map[model.NodeID]struct{}, len(items))
	for _, o := range items {
		if o.FromNodeID == "" {
			continue
		}
		if _, dup := seen[o.FromNodeID]; dup {
			continue
		}
		seen[o.FromNodeID] = struct{}{}
		ids = append(ids, o.FromNodeID)
	}
	if len(ids) == 0 {
		return nil
	}
	nodes, err := e.adjacency.NodesByID(ctx, ids)
	if err != nil {
		return err
	}
	names := make(map[model.NodeID]string, len(nodes))
	for _, n := range nodes {
		// Qualified first, plain name second: the qualified spelling is what
		// tells two same-named methods apart, and a provider that sealed none
		// leaves the plain name as the only thing there is to say.
		if name := n.QualifiedName; name != "" {
			names[n.ID] = name
			continue
		}
		if n.Name != "" {
			names[n.ID] = n.Name
		}
	}
	for i := range items {
		items[i].FromName = names[items[i].FromNodeID]
	}
	return nil
}

// nextReferenceCursor mints the continuation for a page that stopped on a
// relation boundary with relations left. It returns an empty token, not an
// error, when this workspace cannot issue one: a continuation is an optional
// convenience, and an engine built without a signer or a lease store must still
// answer the page it did compute.
//
// The token carries the pinned generation and analysis key, the normalized
// query hash and the lease that retains the facts, so it can only be replayed
// against the same generation, the same symbol and the same operation, and only
// while that generation is still pinned. That lease is minted HERE and owned by
// the cursor: the pinned reader's query lease is released when this request
// returns, so a token naming it would be refused by the next invocation.
func (e *Engine) nextReferenceCursor(ctx context.Context, queryHash string,
	last model.RelationID) (string, error) {
	if e.signer == nil || e.leases == nil || last == "" {
		return "", nil
	}
	b := e.adjacency.Binding()
	lease, err := e.leases.Acquire(ctx, b.GenerationID, b.SnapshotID, model.LeaseCursor)
	if err != nil {
		return "", err
	}
	token, err := e.signer.EncodeCursor(pagination.Cursor{
		Endpoint:     referenceEndpoint,
		GenerationID: b.GenerationID,
		AnalysisKey:  b.AnalysisKey,
		QueryHash:    queryHash,
		LastKey:      string(last),
		LeaseID:      lease.ID,
		// The cursor expires with the retention lease it names: a token that
		// outlived the lease would resume over facts nothing is holding.
		ExpiresAt: e.now().Add(e.limits.CursorTTL),
	})
	if err != nil {
		return "", e.releaseLease(ctx, lease.ID, err)
	}
	return token, nil
}

// resumeReferences turns a presented cursor into the keyset position to resume
// from. A cursor without a signer cannot be verified at all, and an unverified
// continuation is a request to read from a position nothing vouched for, so it
// is rejected rather than trusted.
func (e *Engine) resumeReferences(req model.ReferenceRequest, queryHash string) (model.RelationID, error) {
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
	if err := e.checkReferenceCursor(c, queryHash); err != nil {
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

// checkReferenceCursor rejects a verified cursor that does not describe THIS
// query. The signature proves the token is ours; it does not prove it describes
// this generation's facts or this symbol's keyset order. Resuming against a
// different generation would page through facts the first page never saw, and
// resuming against a different node or operation would start at a RelationID
// that means nothing in the new order -- both silently skip references.
func (e *Engine) checkReferenceCursor(c pagination.Cursor, queryHash string) error {
	b := e.adjacency.Binding()
	if c.GenerationID != b.GenerationID || c.AnalysisKey != b.AnalysisKey {
		return (&model.Error{Code: model.CodeCursorInvalid,
			Message: "cursor was issued against a different generation"}).
			WithDetail("endpoint", referenceEndpoint)
	}
	if c.QueryHash != queryHash {
		return (&model.Error{Code: model.CodeCursorInvalid,
			Message: "cursor was issued for a different reference query"}).
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
	after model.RelationID, pageLimit int) (items []model.ReferenceOccurrence, last model.RelationID,
	more, clipped bool, err error) {
	seeds := []model.NodeID{node}
	for {
		rels, err := e.adjacency.Edges(ctx, seeds, walk.direction, walk.kinds, after, adjacencyBatch)
		if err != nil {
			return nil, "", false, false, err
		}
		if len(rels) == 0 {
			return items, last, false, clipped, nil
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
		ev, err := e.evidence(ctx, ids, pageLimit+1)
		if err != nil {
			return nil, "", false, false, err
		}
		for _, r := range rels {
			occ := ev[r.ID]
			if len(occ) == 0 {
				after = r.ID
				continue
			}
			// clipped describes THIS page. A relation whose occurrences
			// overflow the page but which is then deferred whole to the next
			// page has clipped nothing here, so the flag is only adopted once
			// the clipped occurrences are actually appended below.
			occClipped := false
			if len(occ) > pageLimit {
				occ, occClipped = occ[:pageLimit], true
			}
			if len(items) > 0 && len(items)+len(occ) > pageLimit {
				// Stop on the relation boundary: this relation belongs whole
				// to the next page.
				return items, last, true, clipped, nil
			}
			clipped = clipped || occClipped
			sorted := append([]model.Evidence(nil), occ...)
			sort.Slice(sorted, func(i, j int) bool { return sorted[i].ID < sorted[j].ID })
			for _, row := range sorted {
				items = append(items, model.ReferenceOccurrence{
					RelationID: r.ID,
					EvidenceID: row.ID,
					Kind:       r.Kind,
					FromNodeID: r.From,
					ToNodeID:   r.To,
					// Precision, FileID and Range come from the evidence row
					// itself -- they are what makes the occurrence checkable
					// against the source, and an empty precision is not a
					// neutral default but a claim about derivation nobody made
					// (ReferenceOccurrence.Validate rejects it). Path stays
					// empty: it is a rendering convenience derived from FileID,
					// and resolving it would cost one file read per distinct
					// file on a port that offers no batched file lookup, while
					// FileID plus Range already locates the occurrence exactly.
					Precision: row.Precision,
					FileID:    row.FileID,
					// Range and Bytes are the same interval from the two sides
					// of persistence, and a hydrated evidence row carries the
					// byte one: the table keeps no line or column. Both are
					// copied rather than one converted into the other, because
					// a line number is not derivable from an offset here.
					Range:          row.Range,
					Bytes:          row.Bytes,
					SemanticSource: model.SemanticCanonical,
				})
			}
			after, last = r.ID, r.ID
			if len(items) >= pageLimit || clipped {
				more, err := e.hasMoreRelations(ctx, seeds, walk, after)
				if err != nil {
					return nil, "", false, false, err
				}
				return items, last, more, clipped, nil
			}
		}
		// No short-page break: the reader clamps the requested limit down to
		// model.MaxPageItems, so a short page is the normal case. `after`
		// advanced on every relation above, and the empty-page check at the
		// head of the loop is what ends the walk.
	}
}

// evidence hydrates the evidence backing a page of relations. It prefers the
// optional EvidenceRowReader seam, which returns whole rows, and falls back to
// the frozen identity-only Adjacency.EvidenceFor for an Adjacency that does not
// offer it. limit is the per-relation cap in both cases.
//
// The fallback yields rows carrying only an id, which is what a traversal needs
// but not what a reference occurrence does; References then rejects its own
// page through ReferenceOccurrence.Validate rather than publishing an
// occurrence whose precision class is unstated. An Adjacency used for reference
// queries therefore has to implement the seam, and the app adapter does.
func (e *Engine) evidence(ctx context.Context, relations []model.RelationID,
	limit int) (map[model.RelationID][]model.Evidence, error) {
	if rows, ok := e.adjacency.(EvidenceRowReader); ok {
		return rows.EvidenceRows(ctx, relations, limit)
	}
	ids, err := e.adjacency.EvidenceFor(ctx, relations, limit)
	if err != nil {
		return nil, err
	}
	out := make(map[model.RelationID][]model.Evidence, len(ids))
	for rel, list := range ids {
		rows := make([]model.Evidence, 0, len(list))
		for _, id := range list {
			rows = append(rows, model.Evidence{ID: id, RelationID: rel})
		}
		out[rel] = rows
	}
	return out, nil
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
