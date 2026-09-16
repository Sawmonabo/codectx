package graph

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/Sawmonabo/codectx/internal/model"
	"github.com/Sawmonabo/codectx/internal/pagination"
)

// referenceEndpoint binds a reference continuation to this operation, so
// pagination.Signer.DecodeCursor rejects a cursor signed for a different
// endpoint whose position means nothing here. The endpoint alone does not
// distinguish two reference queries: referenceQueryHash pins the node and the
// operation, and checkReferenceCursor compares it, so a cursor minted for one
// symbol can never resume another symbol's list at a foreign offset.
const referenceEndpoint = "graph.references"

// referenceQueryHashDomain is the Section 9.1 hash domain for the normalized
// reference query a continuation is bound to.
const referenceQueryHashDomain = "graph.references.query"

// referenceQueryHash is the normalized query identity of one reference walk.
// The position a cursor carries is an offset into the relation list produced by
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
		// leaves: an unavailability disclosure is no more exempt from its own
		// contract than an answer is.
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
	var cancel context.CancelFunc
	if deadline, bounded := e.queryDeadline(ctx); bounded {
		ctx, cancel = context.WithDeadline(ctx, deadline)
	} else {
		ctx, cancel = context.WithCancel(ctx)
	}
	defer cancel()
	if e.gate != nil {
		if err := e.gate.Acquire(ctx); err != nil {
			return model.Page[model.ReferenceOccurrence]{}, err
		}
		defer e.gate.Release()
	}
	// Every storage read this answer makes resolves its own page bound against
	// the wire ceiling; a read the storage layer served at a different size
	// than it was asked for is reported on the answer rather than applied
	// silently. The evidence hydration below is the one that reaches it: it
	// asks for pageLimit+1 occurrences per relation to tell "exactly a page"
	// from "more than a page", so a page bound AT the wire ceiling asks for one
	// row more than the wire may serve and the clipped flag loses its probe.
	// That is news, and the caller now sees it.
	//
	// Only the observation sink is imported here, never the store: the engine
	// still reads facts through its own Adjacency port.
	ctx, clamps := pagination.WithPageClamps(ctx)
	var notices []string

	queryHash := referenceQueryHash(req.NodeID, req.Operation)
	resume, err := e.resumeReferences(req, queryHash)
	if err != nil {
		return model.Page[model.ReferenceOccurrence]{}, err
	}

	// The page bound is RESOLVED, not silently clamped: a caller that asked
	// for more occurrences than the configuration or the wire allows is told
	// "requested N, effective M" on the answer, exactly as a traversal's
	// bounds are. Before this the two clamps below happened in silence, which
	// is the class-G defect this wave removes.
	pageLimit, notice := resolvePageItems(req.Page.Limit, e.limits.MaxPageItems)
	notices = appendNotice(notices, notice)
	// Only a page bound the CALLER chose is reported against the wire ceiling.
	// A request that named none is "no caller-side bound" and its resolution is
	// not news -- reporting it would put a notice on every answer, which is the
	// rule pageclamp.go states for the same reason. model.PageRequest.Validate
	// already refuses a limit above the wire ceiling, so after this gate the
	// branch is reachable only from an API caller that bypasses it.
	if req.Page.Limit > 0 && pageLimit > model.MaxPageItems {
		notices = appendNotice(notices, fmt.Sprintf(
			"page.limit: effective %d, served %d (the wire ceiling)", pageLimit, model.MaxPageItems))
		pageLimit = model.MaxPageItems
	}

	items, next, more, clipped, err := e.referencePage(ctx, req.NodeID, walk, resume, queryHash, pageLimit)
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
	notices = append(notices, clamps.Notices()...)
	meta := model.QueryMeta{Binding: binding, Completeness: caps, Notices: notices}
	if clipped {
		// A single relation carried more occurrences than one page may hold.
		// Those occurrences are unrecoverable once the page moves past their
		// relation, so this is real truncation even when a continuation is
		// offered for the relations that follow.
		meta.Truncated = true
		meta.TruncationReason = "a relation carries more occurrences than one page holds"
	}
	if more {
		// The page ended on a relation boundary with relations left. next is
		// nil when this workspace cannot mint a continuation at all -- no
		// signer, no lease store or no spool store -- and then the remaining
		// relations cannot be reached, so the page is truncated and says so
		// rather than presenting a bounded page as the complete set.
		if next != nil {
			token, err := e.signReferenceCursor(*next)
			if err != nil {
				return model.Page[model.ReferenceOccurrence]{},
					e.releaseLease(ctx, next.LeaseID, err)
			}
			meta.NextCursor = token
		}
		if meta.NextCursor == "" && !meta.Truncated {
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
// instead: a swallowed error would make "this generation publishes no fact for
// that node" indistinguishable from "the store could not be read".
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

// referenceCursorVersion is the version of the private continuation payload
// below. A token minted by a build that spelled the payload differently carries
// another version and is refused rather than read with today's field meanings.
const referenceCursorVersion = 1

// referenceCursor is this endpoint's continuation payload.
//
// It is deliberately NOT a pagination.Cursor. That shape carries either a
// keyset sort tuple or a spool id, refuses the two together, and has no offset
// field at all. A reference answer is ONE globally ordered relation list, laid
// into one spool by the page that computed it, and every later page seeks to
// its own byte offset in that single file -- so the position it has to carry is
// exactly the one the shared shape cannot hold.
//
// It is signed with pagination.Signer under PurposeCursor like every other
// continuation. validate() restates the refusals pagination.Signer.DecodeCursor
// makes for the shared shape, INCLUDING the endpoint comparison: Verify checks
// the signature and the expiry and nothing else, so without that line a
// traversal's token would be readable here.
type referenceCursor struct {
	Version      int                `json:"version"`
	Endpoint     string             `json:"endpoint"`
	GenerationID model.GenerationID `json:"generation_id"`
	AnalysisKey  model.AnalysisKey  `json:"analysis_key"`
	QueryHash    string             `json:"query_hash"`
	LeaseID      string             `json:"lease_id"`
	// SpoolID names the spool holding every relation the issuing page did not
	// serve, and Offset the byte where the next page's first record begins.
	// A byte offset rather than a record index because that is what
	// pagination.Spools.OpenAt seeks to: counting records to find the position
	// would make a page cost the remainder behind it.
	SpoolID string `json:"spool_id"`
	Offset  int64  `json:"offset"`
	// Served and Total are RELATION counts over the whole answer, so `more` is
	// Served < Total. They are relation-level because the spool is: an
	// occurrence is hydrated from evidence when its relation is served, never
	// spooled, so the list the offset walks is a list of relations.
	Served    int64     `json:"served"`
	Total     int64     `json:"total"`
	ExpiresAt time.Time `json:"expires_at"`
}

// validate refuses a payload that does not describe a reference continuation.
// Every branch is a rejection a tampered or foreign token would otherwise walk
// through: the version, the endpoint, the pinned generation, the identifiers
// the spool store checks the file against, and the position itself.
func (c referenceCursor) validate() error {
	if c.Version != referenceCursorVersion {
		return cursorInvalid("cursor version is not supported")
	}
	if c.Endpoint != referenceEndpoint {
		return cursorInvalid("cursor was issued by a different endpoint")
	}
	if c.GenerationID <= 0 {
		return cursorInvalid("cursor does not pin a generation")
	}
	if !model.ValidHexID(c.QueryHash) {
		return cursorInvalid("cursor query hash must be a well-formed identifier")
	}
	// A continuation minted by a process that records no lease names none: the
	// spool it names is bound to this cursor's expiry instead.
	if c.LeaseID != "" && !model.ValidHexID(c.LeaseID) {
		return cursorInvalid("cursor lease id is malformed")
	}
	if c.AnalysisKey == "" || len(c.AnalysisKey) > model.MaxIdentifierBytes {
		return cursorInvalid("cursor does not name an analysis key")
	}
	if !model.ValidHexID(c.SpoolID) {
		// Every continuation this endpoint mints names the spool its next page
		// seeks into, so a token without one names no position to resume at.
		return cursorInvalid("a reference continuation names no result spool")
	}
	if c.Offset < 0 || c.Served < 0 || c.Total < 0 {
		return cursorInvalid("cursor carries a negative position")
	}
	if c.Served >= c.Total {
		return cursorInvalid("cursor carries a position past the end of its answer")
	}
	if c.ExpiresAt.IsZero() {
		return cursorInvalid("cursor has no expiry")
	}
	return nil
}

// spoolCursor is the binding the spool store checks the file's header against,
// exactly as the traversal endpoints project theirs.
func (c referenceCursor) spoolCursor() pagination.Cursor {
	return pagination.Cursor{
		Endpoint:     c.Endpoint,
		GenerationID: c.GenerationID,
		AnalysisKey:  c.AnalysisKey,
		QueryHash:    c.QueryHash,
		SpoolID:      c.SpoolID,
		LeaseID:      c.LeaseID,
		ExpiresAt:    c.ExpiresAt,
	}
}

// signReferenceCursor validates and signs a continuation. It does not release
// the spool or the lease on failure: the page that created them is the only one
// that may take them back, and it does.
func (e *Engine) signReferenceCursor(c referenceCursor) (string, error) {
	if err := c.validate(); err != nil {
		return "", err
	}
	payload, err := json.Marshal(c)
	if err != nil {
		return "", internalErr("cursor encoding: " + err.Error())
	}
	return e.signer.Sign(pagination.PurposeCursor, payload, c.ExpiresAt)
}

// resumeReferences turns a presented cursor into the position to resume from,
// or nil for a first page. A cursor without a signer cannot be verified at all,
// and an unverified continuation is a request to read from a position nothing
// vouched for, so it is rejected rather than trusted.
func (e *Engine) resumeReferences(req model.ReferenceRequest, queryHash string) (*referenceCursor, error) {
	if req.Page.Cursor == "" {
		return nil, nil
	}
	if e.signer == nil {
		return nil, (&model.Error{Code: model.CodeCursorInvalid,
			Message: "this workspace does not offer query continuations"}).
			WithDetail("endpoint", referenceEndpoint)
	}
	payload, err := e.signer.Verify(req.Page.Cursor, pagination.PurposeCursor, e.now())
	if err != nil {
		return nil, err
	}
	var c referenceCursor
	if err := json.Unmarshal(payload, &c); err != nil {
		return nil, cursorInvalid("cursor payload is malformed")
	}
	if err := c.validate(); err != nil {
		return nil, err
	}
	if err := e.checkReferenceCursor(c, queryHash); err != nil {
		return nil, err
	}
	return &c, nil
}

// checkReferenceCursor rejects a verified cursor that does not describe THIS
// query. The signature proves the token is ours; it does not prove it describes
// this generation's facts or this symbol's order. Resuming against a different
// generation would page through facts the first page never saw, and resuming
// against a different node or operation would seek to an offset in a list that
// means something else -- both silently skip references.
func (e *Engine) checkReferenceCursor(c referenceCursor, queryHash string) error {
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

// referenceRelation is one canonical relation of the answer, as the sort orders
// them and as the spool holds them. It carries no occurrence: evidence is
// hydrated for the relations of the page being built and never spooled, so the
// bytes a continuation retains are a function of the relation count alone.
type referenceRelation struct {
	ID   model.RelationID   `json:"id"`
	Kind model.RelationKind `json:"kind"`
	From model.NodeID       `json:"from"`
	To   model.NodeID       `json:"to"`
}

func encodeReferenceRelation(r referenceRelation) ([]byte, error) {
	b, err := json.Marshal(r)
	if err != nil {
		return nil, internalErr("graph: a reference relation could not be encoded")
	}
	return b, nil
}

func decodeReferenceRelation(b []byte) (referenceRelation, error) {
	var r referenceRelation
	if err := json.Unmarshal(b, &r); err != nil {
		return referenceRelation{}, internalErr("graph: a spooled reference relation is not readable")
	}
	return r, nil
}

// sizeOfReferenceRelation charges one buffered relation against the sort's run
// budget (ExternalSort.WithRunBytes), erring high: the record is encoded only
// when a run spills, so the charge is an estimate of the encoding it would
// produce plus the struct and allocator overhead the buffer really pays.
func sizeOfReferenceRelation(r referenceRelation) int64 {
	return int64(len(r.ID)+len(r.Kind)+len(r.From)+len(r.To)) + 128
}

// referenceHydrateBatch is how many relations are taken off the ordered list
// before their evidence is hydrated in one round trip. A page holds at most
// pageLimit occurrences and a relation with evidence contributes at least one,
// so a batch wider than the page bound could only ever help relations that
// carry no evidence at all; the adjacency batch caps it so a very large page
// bound still hydrates in bounded round trips.
func referenceHydrateBatch(pageLimit int) int {
	return max(1, min(pageLimit, adjacencyBatch))
}

// errReferencePageFull ends a walk over the ordered relations once the page is
// built. It never leaves referencePage.
var errReferencePageFull = errors.New("graph: the reference page is full")

// referencePageBuilder turns ordered relations into the page's occurrences. It
// holds the Section 9.2 rule the endpoint exists for: a page ends on a RELATION
// boundary, because splitting one relation's occurrences across two pages is
// what would let a consumer double-count the relation or collapse its
// occurrences.
type referencePageBuilder struct {
	e         *Engine
	pageLimit int
	items     []model.ReferenceOccurrence
	clipped   bool
	full      bool
}

// consume hydrates the evidence of batch in ONE round trip and appends the
// occurrences of as many of its relations as the page holds. It reports how
// many relations it CONSUMED: a relation deferred whole to the next page is not
// one of them, and neither is anything behind it.
func (b *referencePageBuilder) consume(ctx context.Context, batch []referenceRelation) (int, error) {
	if len(batch) == 0 {
		return 0, nil
	}
	ids := make([]model.RelationID, 0, len(batch))
	for _, r := range batch {
		ids = append(ids, r.ID)
	}
	// pageLimit+1 per relation distinguishes "exactly a page of occurrences"
	// from "more than a page", so the clipped flag is never raised for a
	// relation that happens to fill the page exactly.
	//
	// This reads the evidence limit as a PER-RELATION cap, not a total row cap
	// across the batch. A total-cap implementation would drop the evidence of
	// nearly every relation in a wide batch and make this method silently
	// under-report occurrences, which is the one thing it exists to get right.
	// The storage implementation is bound by this.
	ev, err := b.e.evidence(ctx, ids, b.pageLimit+1)
	if err != nil {
		return 0, err
	}
	for i, r := range batch {
		occ := ev[r.ID]
		if len(occ) == 0 {
			// A canonical relation with no evidence row contributes no
			// occurrence and is not reported, because an occurrence IS an
			// evidence row: a reference with no location is not an answerable
			// reference. It is still consumed, so the page after this one does
			// not read it again.
			continue
		}
		// clipped describes THIS page. A relation whose occurrences overflow
		// the page but which is then deferred whole to the next page has
		// clipped nothing here, so the flag is only adopted once the clipped
		// occurrences are actually appended below.
		occClipped := false
		if len(occ) > b.pageLimit {
			occ, occClipped = occ[:b.pageLimit], true
		}
		if len(b.items) > 0 && len(b.items)+len(occ) > b.pageLimit {
			// Stop on the relation boundary: this relation belongs whole to
			// the next page.
			b.full = true
			return i, nil
		}
		b.clipped = b.clipped || occClipped
		sorted := append([]model.Evidence(nil), occ...)
		sort.Slice(sorted, func(i, j int) bool { return sorted[i].ID < sorted[j].ID })
		for _, row := range sorted {
			b.items = append(b.items, model.ReferenceOccurrence{
				RelationID: r.ID,
				EvidenceID: row.ID,
				Kind:       r.Kind,
				FromNodeID: r.From,
				ToNodeID:   r.To,
				// Precision, FileID and Range come from the evidence row
				// itself -- they are what makes the occurrence checkable
				// against the source, and an empty precision is not a neutral
				// default but a claim about derivation nobody made
				// (ReferenceOccurrence.Validate rejects it). Path stays empty:
				// it is a rendering convenience derived from FileID, and
				// resolving it would cost one file read per distinct file on a
				// port that offers no batched file lookup, while FileID plus
				// Range already locates the occurrence exactly.
				Precision: row.Precision,
				FileID:    row.FileID,
				// Range and Bytes are the same interval from the two sides of
				// persistence, and a hydrated evidence row carries the byte
				// one: the table keeps no line or column. Both are copied
				// rather than one converted into the other, because a line
				// number is not derivable from an offset here.
				Range:          row.Range,
				Bytes:          row.Bytes,
				SemanticSource: model.SemanticCanonical,
			})
		}
		if len(b.items) >= b.pageLimit || b.clipped {
			b.full = true
			return i + 1, nil
		}
	}
	return len(batch), nil
}

// referencePage serves one page of the answer: the first from the packed
// adjacency, every later one from the spool the first laid down.
//
// It reports the page's occurrences, the continuation to mint (nil when none
// can be), whether relations remain beyond the page, and whether one relation's
// occurrences had to be clipped to the page bound.
func (e *Engine) referencePage(ctx context.Context, node model.NodeID, walk referenceWalk,
	resume *referenceCursor, queryHash string, pageLimit int) (items []model.ReferenceOccurrence,
	next *referenceCursor, more, clipped bool, err error) {
	if resume != nil {
		return e.resumedReferencePage(ctx, *resume, pageLimit)
	}
	return e.firstReferencePage(ctx, node, walk, queryHash, pageLimit)
}

// referenceRelations reads the node's WHOLE list in one packed-adjacency scan
// per direction and returns it ordered by canonical relation id.
//
// One scan, not a keyset page per round trip: the packed reader streams an
// owner's list in (owner, list index) order, which is a SURROGATE order and
// says nothing about the canonical ids. The answer's order is canonical -- it
// always was, and a caller's saved page must not be reshuffled by a reindex
// that renumbers surrogates -- so every entry is fed UNCONDITIONALLY into a
// disk-backed sort keyed by that canonical id. "Unconditionally" is the point:
// a sort that only ran when the list looked long would answer a short list in
// surrogate order, and nothing downstream could tell the two apart.
//
// Peak memory is the sort's run budget, never the node's degree.
func (e *Engine) referenceRelations(ctx context.Context, node model.NodeID,
	walk referenceWalk) (*pagination.SortedRun[referenceRelation], error) {
	reader, err := e.Reader()
	if err != nil {
		return nil, err
	}
	sorter, err := pagination.NewExternalSort(e.walkScratchDir(), 0,
		encodeReferenceRelation, decodeReferenceRelation,
		func(a, b referenceRelation) int { return cmp.Compare(a.ID, b.ID) })
	if err != nil {
		return nil, err
	}
	sorter = sorter.WithRunBytes(pagination.SortRunBytes(e.limits.FrontierBytes), sizeOfReferenceRelation)
	defer sorter.Close()

	refs, err := reader.Resolve(ctx, []model.NodeID{node})
	if err != nil {
		return nil, err
	}
	codes, absent := kindCodesFor(reader, walk.kinds)
	// A node this generation does not carry has no list, and an operation whose
	// kinds this generation seals none of has no rows. Both are an EMPTY
	// answer, never an error and never a wider one: a nil kind slice means
	// "every kind" to the port, so inferring it would turn "who calls this"
	// into an arbitrary neighbourhood expansion under a name that promises
	// otherwise.
	if refs[0] == 0 || absent {
		return sorter.Sorted()
	}

	buf := make([]Edge, 0, adjacencyBatch)
	flush := func() error {
		if len(buf) == 0 {
			return nil
		}
		rels := make([]RelRef, len(buf))
		nbrs := make([]NodeRef, len(buf))
		for i, edge := range buf {
			rels[i], nbrs[i] = edge.Rel, edge.Neighbour
		}
		relIDs, err := reader.RelationIDs(ctx, rels)
		if err != nil {
			return err
		}
		nodeIDs, err := reader.NodeIDs(ctx, nbrs)
		if err != nil {
			return err
		}
		for i, edge := range buf {
			kind, ok := reader.Kinds().Kind(edge.Kind)
			if !ok || relIDs[i] == "" || nodeIDs[i] == "" {
				// The generation does not publish one of the three facts the
				// occurrence is made of. Reporting a relation with a blank id,
				// kind or endpoint would fail ReferenceOccurrence.Validate and
				// take the whole page down with it.
				continue
			}
			r := referenceRelation{ID: relIDs[i], Kind: kind, From: node, To: nodeIDs[i]}
			if !edge.Outgoing {
				// The entry is on the owner's INCOMING list, so the owner is
				// the relation's to-node and the neighbour is where the
				// reference is written.
				r.From, r.To = nodeIDs[i], node
			}
			if err := sorter.Add(r); err != nil {
				return err
			}
		}
		buf = buf[:0]
		return nil
	}
	if _, err := reader.Neighbours(ctx, refs, walk.direction, codes, EdgePos{}, func(edge Edge) error {
		if err := ctx.Err(); err != nil {
			return typedContextError(ctx, err)
		}
		if buf = append(buf, edge); len(buf) < adjacencyBatch {
			return nil
		}
		return flush()
	}); err != nil {
		return nil, err
	}
	if err := flush(); err != nil {
		return nil, err
	}
	return sorter.Sorted()
}

// firstReferencePage computes the whole ordered list, serves its first page out
// of the sorted run, and lays everything it did not serve into ONE spool the
// later pages read their own byte range out of.
//
// A list that fits the page is served and nothing is written: no spool is
// adopted and no lease is minted, so a degree-one node pays for a sort of one
// record and nothing else. That is a CONSEQUENCE of "no relations remained"
// rather than a predicted shortcut -- a three-relation list where one relation
// carries five hundred occurrences does not fit the page, and predicting the
// direct serve from the relation count would have served it with no
// continuation to reach the rest.
func (e *Engine) firstReferencePage(ctx context.Context, node model.NodeID, walk referenceWalk,
	queryHash string, pageLimit int) ([]model.ReferenceOccurrence, *referenceCursor, bool, bool, error) {
	run, err := e.referenceRelations(ctx, node, walk)
	if err != nil {
		return nil, nil, false, false, err
	}
	defer run.Close()

	b := &referencePageBuilder{e: e, pageLimit: pageLimit}
	served := 0
	batch := make([]referenceRelation, 0, referenceHydrateBatch(pageLimit))
	consume := func() error {
		n, err := b.consume(ctx, batch)
		served += n
		batch = batch[:0]
		if err != nil {
			return err
		}
		if b.full {
			return errReferencePageFull
		}
		return nil
	}
	err = run.Each(func(r referenceRelation) error {
		if err := ctx.Err(); err != nil {
			return typedContextError(ctx, err)
		}
		if batch = append(batch, r); len(batch) < cap(batch) {
			return nil
		}
		return consume()
	})
	if err == nil {
		err = consume()
	}
	if err != nil && !errors.Is(err, errReferencePageFull) {
		return nil, nil, false, false, err
	}
	total := run.Len()
	if int64(served) >= total {
		return b.items, nil, false, b.clipped, nil
	}
	if e.signer == nil || e.spools == nil {
		// Nothing to bind a token to and nowhere to spill the remainder, so
		// nothing is created here -- this return precedes spools.Create -- and
		// the caller marks the answer truncated rather than presenting a
		// bounded page as the complete set.
		return b.items, nil, true, b.clipped, nil
	}

	bind := e.adjacency.Binding()
	// The lease is minted HERE and owned by the cursor: the pinned reader's
	// query lease is released when this request returns, so a token naming it
	// would be refused by the next invocation.
	//
	// A process that writes nothing to the database records none and spools
	// anyway: the spool is a filesystem write, and the entry is reclaimed by
	// the expiry stamped into its header instead of by a lease row.
	var leaseID string
	if e.leases.Retains() {
		lease, err := e.leases.Acquire(ctx, bind.GenerationID, bind.SnapshotID, model.LeaseCursor)
		if err != nil {
			return nil, nil, false, false, err
		}
		leaseID = lease.ID
	}
	next := referenceCursor{
		Version: referenceCursorVersion, Endpoint: referenceEndpoint,
		GenerationID: bind.GenerationID, AnalysisKey: bind.AnalysisKey, QueryHash: queryHash,
		LeaseID: leaseID, Served: int64(served), Total: total,
		// The cursor expires with the state it names: a token that outlived the
		// lease, or the leaseless spool's own stamped expiry, would resume over
		// facts nothing is holding.
		ExpiresAt: e.now().Add(e.limits.CursorTTL).UTC().Truncate(time.Second),
	}
	sp, err := e.spools.Create(next.spoolCursor())
	if err != nil {
		return nil, nil, false, false, e.releaseLease(ctx, leaseID, err)
	}
	// Written counts the header frame Create wrote, so this is where the first
	// record lands and where the next page begins reading.
	next.Offset = sp.Written()
	at := 0
	spill := func(r referenceRelation) error {
		if err := ctx.Err(); err != nil {
			return typedContextError(ctx, err)
		}
		if at++; at <= served {
			return nil
		}
		record, err := encodeReferenceRelation(r)
		if err != nil {
			return err
		}
		return sp.Append(record)
	}
	if err := run.Each(spill); err != nil {
		return nil, nil, false, false, e.releaseLease(ctx, leaseID, e.releaseSpool(sp, err))
	}
	if err := sp.Close(); err != nil {
		return nil, nil, false, false, e.releaseLease(ctx, leaseID, e.releaseSpool(sp, err))
	}
	next.SpoolID = sp.ID()
	return b.items, &next, true, b.clipped, nil
}

// resumedReferencePage serves one page out of the spool the first page wrote,
// starting at the byte offset the presenting cursor carries. It walks NOTHING:
// the order was settled once, and a page's cost is its own page rather than the
// remainder behind it.
func (e *Engine) resumedReferencePage(ctx context.Context, c referenceCursor,
	pageLimit int) ([]model.ReferenceOccurrence, *referenceCursor, bool, bool, error) {
	// A spool store is what a resumed page reads; a LEASE store is not. An
	// engine composed without one mints leaseless continuations, so refusing
	// them here would hand back a token that fails on every use. Every lease
	// call below is nil-safe or sits behind a lease id the token carries.
	if e.spools == nil {
		return nil, nil, false, false, cursorInvalid("continuation state has expired or was released")
	}
	sc := c.spoolCursor()
	b := &referencePageBuilder{e: e, pageLimit: pageLimit}
	chunk := referenceHydrateBatch(pageLimit)
	served, offset := c.Served, c.Offset
	for served < c.Total && !b.full {
		batch := make([]referenceRelation, 0, chunk)
		end, err := e.spools.OpenAt(ctx, sc, e.now(), offset, func(record []byte) error {
			if err := ctx.Err(); err != nil {
				return typedContextError(ctx, err)
			}
			r, err := decodeReferenceRelation(record)
			if err != nil {
				return err
			}
			if batch = append(batch, r); len(batch) == chunk {
				return pagination.ErrStopSpool
			}
			return nil
		})
		if err != nil {
			return nil, nil, false, false, err
		}
		if len(batch) == 0 {
			// The spool ended before Total. The cursor's counts and the file
			// disagree, which is corruption of the continuation state, not an
			// answer that quietly stops short.
			return nil, nil, false, false, cursorInvalid("continuation state is shorter than the answer it names")
		}
		n, err := b.consume(ctx, batch)
		if err != nil {
			return nil, nil, false, false, err
		}
		served += int64(n)
		if n == len(batch) {
			offset = end
			continue
		}
		if n == 0 {
			if b.full {
				// The first relation of this batch did not fit the page this
				// one had already partly filled, so the batch consumed
				// nothing. offset still names the batch's start, which is
				// exactly where the relation deferred here sits: the loop's
				// own `!b.full` exit arrives one iteration early and the next
				// page reads it once.
				break
			}
			return nil, nil, false, false, internalErr("graph: a reference page consumed no relation and cannot advance")
		}
		// The page filled part way through this batch, so the next one begins
		// after the records this one consumed: that is where a read of exactly
		// those records ends. It costs one extra open, only on the page that
		// stops mid-batch, and never a scan of what is left.
		if offset, err = referenceOffsetAfter(ctx, e.spools, sc, e.now(), offset, n); err != nil {
			return nil, nil, false, false, err
		}
	}
	if served >= c.Total {
		// The answer is complete. The spool is released here rather than left
		// to its deadline, so a walk read to exhaustion holds nothing, and the
		// lease it pinned goes with it -- unless this process records none, in
		// which case the row a writer-bearing page minted is not its to end and
		// that lease's own TTL and the next sweep reclaim it.
		relErr := e.spools.Release(c.SpoolID)
		if c.LeaseID != "" && e.leases.Retains() {
			relErr = e.releaseLease(ctx, c.LeaseID, relErr)
		}
		if relErr != nil {
			return nil, nil, false, false, relErr
		}
		return b.items, nil, false, b.clipped, nil
	}
	next := c
	next.Offset, next.Served = offset, served
	// A LEASED spool's liveness is its lease's (pagination.Spools.live), and
	// renewing it on every page is what keeps a long reference list reachable
	// past one cursor TTL. A leaseless spool -- and a leased one presented to a
	// process that writes nothing -- has no renewal: the token carries forward
	// the expiry its header was stamped with, so the whole list is reachable
	// for that one window and a page asked for after it is the typed
	// CTX_CURSOR_INVALID.
	if c.LeaseID != "" && e.leases.Retains() {
		if _, err := e.leases.Renew(ctx, c.LeaseID); err != nil {
			return nil, nil, false, false, err
		}
		next.ExpiresAt = e.now().Add(e.limits.CursorTTL).UTC().Truncate(time.Second)
	}
	return b.items, &next, true, b.clipped, nil
}

// referenceOffsetAfter reports the byte offset one past the n-th record after
// from. It is how a page that stopped mid-batch names its successor's start
// without the spool store having to report a per-record position.
func referenceOffsetAfter(ctx context.Context, spools *pagination.Spools, c pagination.Cursor,
	now time.Time, from int64, n int) (int64, error) {
	if n <= 0 {
		return from, nil
	}
	seen := 0
	end, err := spools.OpenAt(ctx, c, now, from, func([]byte) error {
		if err := ctx.Err(); err != nil {
			return typedContextError(ctx, err)
		}
		if seen++; seen == n {
			return pagination.ErrStopSpool
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	if seen != n {
		return 0, internalErr("graph: continuation state ended before the page it holds")
	}
	return end, nil
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
