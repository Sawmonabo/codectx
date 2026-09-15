// Package search answers the Section 14 discovery endpoints over one pinned
// generation: exact/prefix symbol and path lookup, and generation-local BM25
// lexical retrieval. It holds no graph, context-compiler, MCP or LSP concern.
package search

import (
	"context"
	"log/slog"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/Sawmonabo/codectx/internal/config"
	"github.com/Sawmonabo/codectx/internal/lang"
	"github.com/Sawmonabo/codectx/internal/model"
	"github.com/Sawmonabo/codectx/internal/pagination"
	"github.com/Sawmonabo/codectx/internal/storage/sqlite"
)

// Options composes the service: Store+Repo pin generations, Signer signs
// cursors, Spools hold ranked pages, Resources is Section 20.1.
//
// Content is the content-addressed store the composition root already opens.
// The frozen digest §2 signature carried no such field; without it
// SearchHit.Range is always nil, which Section 14.2 counts as a silent
// capability reduction rather than a shortcut, so the field was added here
// rather than the promise dropped there.
type Options struct {
	Store  *sqlite.Store
	Repo   model.RepositoryID
	Signer *pagination.Signer
	Spools *pagination.Spools
	// Leases mints the cursor-scoped retention lease a continuation names. It
	// is the app stack's one pagination.Leases over the same store, passed in
	// rather than built here so search and graph cannot drift into two wrappers
	// with two TTLs.
	Leases    *pagination.Leases
	Content   ContentReader
	Resources config.Resources
	CursorTTL time.Duration
	Now       func() time.Time
	Logger    *slog.Logger
}

// Service answers the Section 14 discovery endpoints. Safe for concurrent use:
// every field below is read-only after New, and all per-request state (the
// pinned reader, the collector, the hydrator's blob cache) lives on the stack
// of the call that made it.
type Service struct {
	store   *sqlite.Store
	repo    model.RepositoryID
	signer  *pagination.Signer
	spools  *pagination.Spools
	leases  *pagination.Leases
	content ContentReader
	lexical *lexicalTier
	maxPage int
	timeout time.Duration
	ttl     time.Duration
	now     func() time.Time
	log     *slog.Logger
}

// New validates the options and builds the service.
//
// Every dependency is required: the composition root builds this eagerly when
// a workspace opens, so a missing one is a wiring defect that must fail there
// and not per query, where it would look like a data problem.
func New(o Options) (*Service, error) {
	switch {
	case o.Store == nil:
		return nil, optionErr("a store")
	case o.Signer == nil:
		return nil, optionErr("a cursor signer")
	case o.Spools == nil:
		return nil, optionErr("a spool store")
	case o.Leases == nil:
		return nil, optionErr("a cursor lease minter")
	case o.Content == nil:
		return nil, optionErr("a content reader for source positions")
	}
	if !model.ValidHexID(string(o.Repo)) {
		return nil, optionErr("a repository identity")
	}
	timeout := o.Resources.QueryTimeout.Std()
	// max_query_terms is a config.Limit: 0 means unlimited, which is its
	// default, so it is deliberately absent from this check. Only the two
	// plain ints that must be positive to size a page and a deadline are
	// checked here.
	if timeout <= 0 || o.Resources.MaxPageItems <= 0 {
		return nil, optionErr("positive query_timeout and max_page_items")
	}
	ttl := o.CursorTTL
	if ttl <= 0 {
		ttl = pagination.DefaultCursorTTL
	}
	now := o.Now
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}
	logger := o.Logger
	if logger == nil {
		logger = slog.Default()
	}
	return &Service{
		store:   o.Store,
		repo:    o.Repo,
		signer:  o.Signer,
		spools:  o.Spools,
		leases:  o.Leases,
		content: o.Content,
		lexical: newLexicalTier(o.Store, o.Resources.MaxQueryTerms, defaultStatsCacheBytes),
		maxPage: o.Resources.MaxPageItems,
		timeout: timeout,
		ttl:     ttl,
		now:     now,
		log:     logger,
	}, nil
}

// optionErr reports a missing or unusable composition input.
func optionErr(what string) error {
	return &model.Error{Code: model.CodeInternal, Message: "search: the service was composed without " + what}
}

// Close releases everything the service holds open. The store, signer and
// spools are the composition root's and are closed there in reverse order;
// this service opens nothing of its own, and every per-request resource is
// released on the path that acquired it.
func (s *Service) Close() error { return nil }

// pageLimit resolves a request's page bound against resources.max_page_items,
// which New validated and the service kept. Zero means the endpoint default,
// which Section 20.1 fixes as the configured maximum, never "unlimited"; a
// request above it is clamped to it, so lowering the key really does lower the
// pages this service serves rather than only the model ceiling.
func (s *Service) pageLimit(limit int) int {
	maximum := s.maxPage
	if maximum <= 0 || maximum > model.MaxPageItems {
		maximum = model.MaxPageItems
	}
	if limit <= 0 || limit > maximum {
		return maximum
	}
	return limit
}

// Search answers the lexical + exact discovery endpoint, paging through a
// bounded spool because ranking is global over the candidate set.
//
// The order of work is Section 14's: the request is bounded before any read,
// one generation is pinned for the whole request, membership filtering and the
// request's own filters are applied to every tier BEFORE scoring and
// deduplication, deduplication precedes paging, and the page that is served
// carries the binding it was read from.
func (s *Service) Search(ctx context.Context, req model.SearchRequest) (model.Page[model.SearchHit], error) {
	var empty model.Page[model.SearchHit]
	if err := req.Validate(); err != nil {
		return empty, err
	}
	filter, err := newHitFilter(req)
	if err != nil {
		return empty, err
	}
	ctx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()
	now := s.now()
	hash := searchQueryHash(req)

	generation := req.GenerationID
	var cursor pagination.Cursor
	if req.Page.Cursor != "" {
		if cursor, err = decodeCursor(s.signer, req.Page.Cursor, endpointSearch, now); err != nil {
			return empty, err
		}
		generation = cursor.GenerationID
	}
	reader, err := s.store.PinGeneration(ctx, s.repo, generation, s.ttl)
	if err != nil {
		return empty, err
	}
	defer reader.Close()
	binding := reader.Binding()

	var (
		hits      []model.SearchHit
		truncated bool
		reason    string
	)
	if req.Page.Cursor != "" {
		if err := verifyCursor(cursor, binding, hash); err != nil {
			return empty, err
		}
		var meta spoolMeta
		if meta, hits, err = readSpool(ctx, s.spools, cursor, now); err != nil {
			return empty, err
		}
		// The answer-level truncation the FIRST page computed. A continuation
		// reads hits from the spool, never from the tiers, so it has nothing
		// of its own to compute it from and must carry it forward or report a
		// complete answer for an answer that is not.
		truncated, reason = meta.Truncated, meta.TruncationReason
	} else if hits, truncated, reason, err = s.rank(ctx, reader, req, filter); err != nil {
		return empty, err
	}

	limit := s.pageLimit(req.Page.Limit)
	rest := []model.SearchHit(nil)
	if len(hits) > limit {
		hits, rest = hits[:limit], hits[limit:]
	}
	meta := model.QueryMeta{Binding: binding, Truncated: truncated, TruncationReason: reason}
	if meta.Completeness, err = reader.Capabilities(ctx); err != nil {
		return empty, err
	}
	if len(rest) > 0 {
		// A spool budget that is already full must end THIS page, never the
		// query: the hits of page 1 are in hand and refusing to serve them
		// because the tail could not be written is the class-D refusal the
		// scale posture forbids. The answer says it is incomplete and why, and
		// carries no continuation because there is nothing to continue from.
		// Every other spool failure -- a disk fault, a corrupt spool -- is a
		// real fault and still surfaces.
		meta.NextCursor, err = s.spoolNext(ctx, binding, hash, now, spoolMeta{Truncated: truncated, TruncationReason: reason}, rest)
		switch {
		case err == nil:
		case pagination.IsBudgetExhausted(err):
			// This reason OVERWRITES whatever a tier reported. A tier's
			// truncation says the candidate set was bounded; this one says a
			// named number of already-ranked hits were thrown away and there is
			// no continuation to reach them. Reporting only the tier would hide
			// exactly the drop the posture forbids.
			meta.Truncated, meta.NextCursor = true, ""
			meta.TruncationReason = spoolBudgetFullReason(len(rest))
			s.log.Warn("a search answer was cut short by the spool disk budget", "component", "search",
				"generation_id", int64(binding.GenerationID), "dropped_hits", len(rest))
		default:
			return empty, err
		}
	}
	page := model.Page[model.SearchHit]{Meta: meta, Items: hits}
	if err := page.Validate(); err != nil {
		return empty, err
	}
	for i := range page.Items {
		if err := page.Items[i].Validate(); err != nil {
			return empty, err
		}
	}
	if req.Page.Cursor != "" {
		s.consumed(ctx, cursor)
	}
	s.log.Debug("answered a search query", "component", "search", "generation_id", int64(binding.GenerationID),
		"hits", len(page.Items), "truncated", meta.Truncated, "continued", meta.NextCursor != "")
	return page, nil
}

// spoolNext writes the remainder of a ranked answer to a fresh spool and signs
// the cursor that names it.
//
// The cursor carries a NEW cursor-owned retention lease, not the pinned
// reader's query lease: that one is released when this request returns, and a
// continuation whose lease is gone is refused by the spool store and leaves
// the generation free for retention to collect. The lease expires with the
// cursor, so nothing here pins a generation for longer than the token lives.
func (s *Service) spoolNext(ctx context.Context, b model.Binding, hash string, now time.Time, answer spoolMeta, rest []model.SearchHit) (string, error) {
	lease, err := s.leases.Acquire(ctx, b.GenerationID, b.SnapshotID, model.LeaseCursor)
	if err != nil {
		return "", err
	}
	next := newCursor(endpointSearch, b, lease.ID, hash, now.Add(s.ttl))
	id, err := spoolHits(s.spools, next, answer, rest)
	if err != nil {
		s.releaseLease(ctx, lease.ID)
		return "", err
	}
	next.SpoolID = id
	token, err := s.signer.EncodeCursor(next)
	if err != nil {
		s.spools.Release(id)
		s.releaseLease(ctx, lease.ID)
		return "", err
	}
	return token, nil
}

// releaseLease returns a lease whose cursor never reached the caller. The
// request's own context may already be done, so the release runs on a fresh
// one: leaking the lease would pin a generation against retention for the full
// cursor TTL for a page nobody can ask for.
func (s *Service) releaseLease(ctx context.Context, id string) {
	if err := s.leases.Release(context.WithoutCancel(ctx), id); err != nil {
		s.log.Warn("a continuation lease could not be released", "component", "search", "error", err.Error())
	}
}

// hitFacts is everything about one candidate that the bounded ranking struct
// deliberately does not carry: the hit as it will be served, and the byte
// interval its source range is hydrated from. It is recorded once per distinct
// deduplication key, so it costs one entry per distinct result rather than one
// per candidate.
type hitFacts struct {
	hit  model.SearchHit
	span *model.ByteRange
}

// rank runs every tier for one request, folds the candidates and returns the
// whole ranked answer with its source ranges hydrated.
//
// The full answer is hydrated, not just the first page, because every hit that
// is not served now is spooled and will be served by a continuation that has
// no byte intervals left to hydrate from -- a spool record is a SearchHit.
// Hydration is therefore paid once per distinct result (at most maxRankedHits)
// rather than once per page.
func (s *Service) rank(ctx context.Context, reader *sqlite.PinnedReader, req model.SearchRequest, filter hitFilter) ([]model.SearchHit, bool, string, error) {
	c := newCollector()
	facts := make(map[string]hitFacts)

	// The exact tiers first. model.MaxPageItems is the READ size of one keyset
	// step -- storage clamps any larger request to it (pageLimit), so asking for
	// more would be a bound this code believes and the database does not -- and
	// no longer a bound on how many candidates a tier may yield. Candidates are
	// streamed into the collector, so the whole tier result never materialises
	// as a slice; the collector's ranked set and its spool tail own the bound.
	if err := exactCandidates(ctx, reader, req.Query, req.Kinds, model.MaxPageItems,
		func(e exactHit) error {
			r, ok := exactRanked(e)
			if !ok || !filter.keep(e.Path) {
				return nil
			}
			record(facts, r, exactHitOf(e))
			c.add(r, reasonFor(e.Tier))
			return nil
		}); err != nil {
		return nil, false, "", err
	}

	outcome, err := s.lexicalCandidates(ctx, reader, req, filter, c, facts)
	if err != nil {
		return nil, false, "", err
	}

	truncated, reason := c.truncation()
	if !truncated && outcome.Truncated {
		truncated, reason = true, outcome.Reason
	}

	items := c.results()
	hits := make([]model.SearchHit, 0, len(items))
	spans := make([]model.ByteRange, 0, len(items))
	located := make([]int, 0, len(items))
	for _, it := range items {
		f, ok := facts[dedupKey(it.ranked)]
		if !ok {
			continue
		}
		hit := f.hit
		hit.Tier, hit.ScoreMicros, hit.Reasons = it.Tier, it.ScoreMicros, it.Reasons
		// A candidate that survived is one occurrence even when nothing folded
		// into it: an exact-tier node contributes no lexical document of its
		// own, and a served hit that reported zero occurrences would read as a
		// result that was not actually found.
		hit.OccurrenceCount = max(it.Occurrences, 1)
		if f.span != nil {
			located = append(located, len(hits))
			spans = append(spans, *f.span)
		}
		hits = append(hits, hit)
	}
	if err := s.hydrate(ctx, reader, hits, located, spans); err != nil {
		return nil, false, "", err
	}
	return hits, truncated, reason, nil
}

// hydrate fills the source range of every hit that has a byte interval. A node
// with none -- a manifest dependency has no declaration site -- keeps a nil
// Range, which is the honest answer rather than a fabricated position.
func (s *Service) hydrate(ctx context.Context, reader *sqlite.PinnedReader, hits []model.SearchHit, located []int, spans []model.ByteRange) error {
	if len(located) == 0 {
		return nil
	}
	sub := make([]model.SearchHit, len(located))
	for i, at := range located {
		sub[i] = hits[at]
	}
	h := newHydrator(reader, s.store, s.content)
	if err := h.hydratePage(ctx, sub, spans); err != nil {
		return err
	}
	for i, at := range located {
		hits[at].Range = sub[i].Range
	}
	return nil
}

// hydrateNodes fills the source range of every resolved candidate that has a
// byte interval, through the same bounded CAS window the search page uses. A
// node with none -- a manifest dependency has no declaration site -- keeps a
// nil Range, which the human table renders as "-" rather than as line 0.
func (s *Service) hydrateNodes(ctx context.Context, reader *sqlite.PinnedReader, nodes []model.Node, spans []*model.ByteRange) error {
	if len(nodes) != len(spans) {
		return &model.Error{Code: model.CodeInternal,
			Message: "search: resolution produced " + strconv.Itoa(len(spans)) + " byte ranges for " + strconv.Itoa(len(nodes)) + " candidates"}
	}
	var h *hydrator
	for i := range nodes {
		if spans[i] == nil {
			continue
		}
		if h == nil {
			h = newHydrator(reader, s.store, s.content)
		}
		rng, err := h.hydrate(ctx, nodes[i].FileID, *spans[i])
		if err != nil {
			return err
		}
		nodes[i].Range = rng
	}
	return nil
}

// lexicalCandidates streams the lexical tier into the collector, hydrating the
// candidate documents in batches because the tier emits rowids and the sort
// tuple needs a path, a start byte and an identity.
func (s *Service) lexicalCandidates(ctx context.Context, reader *sqlite.PinnedReader, req model.SearchRequest,
	filter hitFilter, c *collector, facts map[string]hitFacts) (lexicalOutcome, error) {
	pending := make([]lexicalHit, 0, model.MaxPageItems)
	flush := func() error {
		if len(pending) == 0 {
			return nil
		}
		rowids := make([]int64, len(pending))
		for i, h := range pending {
			rowids[i] = h.RowID
		}
		docs, err := reader.SearchDocuments(ctx, rowids)
		if err != nil {
			return err
		}
		byRowID := make(map[int64]sqlite.SearchDocument, len(docs))
		for _, d := range docs {
			byRowID[d.RowID] = d
		}
		for _, h := range pending {
			d, ok := byRowID[h.RowID]
			// SearchDocuments omits rowids that are not visible; a candidate
			// with no document has no path to filter and no identity to fold.
			if !ok || !filter.keep(d.Path) || !matchesKinds(d.Kind, req.Kinds) {
				continue
			}
			r := ranked{Tier: model.TierLexicalFTS, ScoreMicros: h.ScoreMicros, Path: d.Path,
				StartByte: d.Bytes.Start, NodeID: d.NodeID, SearchKey: d.ID, RowID: d.RowID,
				// One per lexical DOCUMENT, so several documents of one node
				// fold to that many occurrences (digest §4). The matched term
				// instances inside a document are the score's business, not
				// the occurrence count's.
				Occurrences: 1}
			bytes := d.Bytes
			record(facts, r, hitFacts{span: &bytes, hit: model.SearchHit{NodeID: d.NodeID, FileID: d.FileID,
				Path: d.Path, Kind: d.Kind, Name: d.Name, QualifiedName: d.QualifiedName, Signature: d.Signature}})
			c.add(r, reasonFor(model.TierLexicalFTS))
		}
		pending = pending[:0]
		return nil
	}
	outcome, err := s.lexical.search(ctx, reader, reader.Binding().AnalysisKey, req.Query, func(h lexicalHit) error {
		pending = append(pending, h)
		if len(pending) < model.MaxPageItems {
			return nil
		}
		return flush()
	})
	if err != nil {
		return outcome, err
	}
	return outcome, flush()
}

// record remembers the servable facts of a candidate the first time its
// deduplication key is seen. The first writer wins: every candidate under one
// key describes the same entity, and the survivor of the fold is decided by
// the sort tuple, not by which tier was read first.
func record(facts map[string]hitFacts, r ranked, f hitFacts) {
	key := dedupKey(r)
	if _, seen := facts[key]; !seen {
		facts[key] = f
	}
}

// exactRanked builds the sort tuple of one exact-tier candidate. Digest §4/Q6
// scores every exact and prefix tier 0: tier rank, not score, separates them,
// and a candidate that also matched lexically keeps that lexical score when
// the two fold together.
//
// It reports false for a node with no file: SearchHit requires a file and a
// path, so a manifest-only entity cannot be served as a search hit at all and
// is dropped here rather than failing the whole page at Validate.
func exactRanked(e exactHit) (ranked, bool) {
	if e.Node.Node.FileID == "" || e.Path == "" {
		return ranked{}, false
	}
	var start uint64
	if e.Node.Bytes != nil {
		start = e.Node.Bytes.Start
	}
	return ranked{Tier: e.Tier, ScoreMicros: 0, Path: e.Path, StartByte: start,
		NodeID: e.Node.Node.ID, SearchKey: string(e.Node.Node.ID)}, true
}

// exactHitOf projects the stored node an exact tier carried into the hit it
// will be served as. It never goes back through SearchDocuments: an exact
// candidate has no search_fts rowid, and that contract omits missing rowids,
// so the round trip would blank the hit or drop it.
func exactHitOf(e exactHit) hitFacts {
	n := e.Node.Node
	return hitFacts{
		span: e.Node.Bytes,
		hit: model.SearchHit{NodeID: n.ID, FileID: n.FileID, Path: e.Path, Kind: n.Kind,
			Name: n.Name, QualifiedName: n.QualifiedName, Signature: n.Signature},
	}
}

// reasonFor is the bounded explanation a hit carries for the tier it was found
// by. The exact and prefix tiers score 0 by construction, so without this a
// caller would see a hit ranked above a scoring one with nothing saying why.
func reasonFor(t model.SearchTier) string { return "matched the " + string(t) + " tier" }

// spoolBudgetFullReason is the reason a caller sees when the hits of this page
// were served but the remainder could not be written to the query spool. It
// names the number of hits that were dropped, which is the fact a caller needs
// to decide whether to narrow the query or to wait and retry.
func spoolBudgetFullReason(dropped int) string {
	return "the shared query spool ran out of disk budget, so " + strconv.Itoa(dropped) +
		" further ranked hits were dropped and this answer has no continuation; " +
		"retry after outstanding cursors expire, or raise resources.max_temp_bytes"
}

// hitFilter is SearchRequest.Languages and .Paths, applied uniformly to every
// tier -- including the lexical candidates, before they are scored -- so the
// truncation counts and the occurrence counts describe the answer the caller
// actually asked for.
//
// Paths are root-relative path PREFIXES: a hit is kept when its file path
// equals a listed prefix or lies under it. Languages are matched by
// lang.Of(path), the one place a repository path becomes a language name, so a
// filter cannot disagree with the language the index recorded.
type hitFilter struct {
	prefixes  []string
	languages map[string]bool
}

// newHitFilter normalizes the request's filters once. A path that is not
// root-relative is CTX_ARGUMENT_INVALID rather than a filter that quietly
// matches nothing: Section 14.1 rejects a bad filter before any work.
func newHitFilter(req model.SearchRequest) (hitFilter, error) {
	var f hitFilter
	for i, p := range req.Paths {
		clean := strings.TrimSuffix(strings.ReplaceAll(strings.TrimSpace(p), `\`, "/"), "/")
		if clean == "" || strings.HasPrefix(clean, "/") || clean != path.Clean(clean) || strings.HasPrefix(clean, "../") || clean == ".." {
			return hitFilter{}, &model.Error{Code: model.CodeArgumentInvalid,
				Message:     "search.paths[" + strconv.Itoa(i) + "] is not a root-relative repository path",
				Remediation: "list paths relative to the repository root, with \"/\" separators and no \"..\" segment"}
		}
		f.prefixes = append(f.prefixes, clean)
	}
	if len(req.Languages) > 0 {
		f.languages = make(map[string]bool, len(req.Languages))
		for _, l := range req.Languages {
			f.languages[strings.ToLower(strings.TrimSpace(l))] = true
		}
	}
	return f, nil
}

// keep reports whether a hit at this repository path survives the filters.
func (f hitFilter) keep(p string) bool {
	if f.languages != nil && !f.languages[lang.Of(p)] {
		return false
	}
	if len(f.prefixes) == 0 {
		return true
	}
	for _, prefix := range f.prefixes {
		if p == prefix || strings.HasPrefix(p, prefix+"/") {
			return true
		}
	}
	return false
}

// Resolve answers the symbol endpoint, paging by keyset over the tier-ordered
// exact and prefix lookups. Ambiguity is every candidate plus a continuation,
// never a silently chosen first one (Section 14.1).
func (s *Service) Resolve(ctx context.Context, req model.SymbolRequest) (model.Page[model.Node], error) {
	var empty model.Page[model.Node]
	if err := req.Validate(); err != nil {
		return empty, err
	}
	ctx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()
	now := s.now()
	hash := symbolQueryHash(req)

	generation := req.GenerationID
	var cursor pagination.Cursor
	var err error
	if req.Page.Cursor != "" {
		if cursor, err = decodeCursor(s.signer, req.Page.Cursor, endpointSymbol, now); err != nil {
			return empty, err
		}
		generation = cursor.GenerationID
	}
	reader, err := s.store.PinGeneration(ctx, s.repo, generation, s.ttl)
	if err != nil {
		return empty, err
	}
	defer reader.Close()
	binding := reader.Binding()
	if req.Page.Cursor != "" {
		if err := verifyCursor(cursor, binding, hash); err != nil {
			return empty, err
		}
	}

	result, err := resolveSymbols(ctx, reader, req, cursor.LastKey, s.pageLimit(req.Page.Limit))
	if err != nil {
		return empty, err
	}
	if err := s.hydrateNodes(ctx, reader, result.Nodes, result.Spans); err != nil {
		return empty, err
	}
	meta := model.QueryMeta{Binding: binding}
	if meta.Completeness, err = reader.Capabilities(ctx); err != nil {
		return empty, err
	}
	meta.Completeness = append(meta.Completeness, result.Completeness...)
	if result.LastKey != "" {
		if meta.NextCursor, err = s.keysetNext(ctx, binding, hash, now, result.LastKey); err != nil {
			return empty, err
		}
	}
	page := model.Page[model.Node]{Meta: meta, Items: result.Nodes}
	if err := page.Validate(); err != nil {
		return empty, err
	}
	if req.Page.Cursor != "" {
		s.consumed(ctx, cursor)
	}
	s.log.Debug("answered a symbol query", "component", "search", "generation_id", int64(binding.GenerationID),
		"operation", string(req.Operation), "candidates", len(page.Items), "continued", meta.NextCursor != "")
	return page, nil
}

// keysetNext signs the Resolve continuation. It takes a cursor-owned lease for
// the same reason the spooled one does: the pinned reader's query lease ends
// with this request, and a continuation that named it would point at a
// generation retention is free to collect.
func (s *Service) keysetNext(ctx context.Context, b model.Binding, hash string, now time.Time, lastKey string) (string, error) {
	lease, err := s.leases.Acquire(ctx, b.GenerationID, b.SnapshotID, model.LeaseCursor)
	if err != nil {
		return "", err
	}
	next := newCursor(endpointSymbol, b, lease.ID, hash, now.Add(s.ttl))
	next.LastKey = lastKey
	token, err := s.signer.EncodeCursor(next)
	if err != nil {
		s.releaseLease(ctx, lease.ID)
		return "", err
	}
	return token, nil
}
