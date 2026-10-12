// Package search answers the Section 14 discovery endpoints over one pinned
// generation: exact/prefix symbol and path lookup, and the ranked lexical tier
// served by the generation's view of the embedded search index. It holds no
// graph, context-compiler, MCP or LSP concern.
package search

import (
	"cmp"
	"container/heap"
	"context"
	"errors"
	"log/slog"
	"math"
	"path"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/Sawmonabo/codectx/internal/config"
	"github.com/Sawmonabo/codectx/internal/lang"
	"github.com/Sawmonabo/codectx/internal/model"
	"github.com/Sawmonabo/codectx/internal/pagination"
	"github.com/Sawmonabo/codectx/internal/search/textindex"
	"github.com/Sawmonabo/codectx/internal/storage/sqlite"
)

// Options composes the service: Store+Repo pin generations, Signer signs
// cursors, Resources is Section 20.1.
//
// Content is the content-addressed store the composition root already opens.
// Without it SearchHit.Range is always nil, which Section 14.2 counts as a
// silent capability reduction.
type Options struct {
	Store  *sqlite.Store
	Repo   model.RepositoryID
	Signer *pagination.Signer
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
// pinned reader, the index view, the hydrator's blob cache) lives on the stack
// of the call that made it.
type Service struct {
	store   *sqlite.Store
	repo    model.RepositoryID
	signer  *pagination.Signer
	leases  *pagination.Leases
	content ContentReader
	maxPage int
	// maxTerms is resources.max_query_terms; unlimited (zero) scores every
	// term of a query.
	maxTerms config.Limit
	// rule is the document length the lexical tier normalizes by (lengthRule).
	rule textindex.LengthRule
	// timeout is resources.query_timeout: the DEFAULT deadline of one query,
	// never a ceiling, and unlimited (zero) unless the operator set it. Every
	// use goes through model.QueryDeadline, which leaves a caller's own
	// deadline in charge and installs nothing at zero.
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
	case o.Leases == nil:
		return nil, optionErr("a cursor lease minter")
	case o.Content == nil:
		return nil, optionErr("a content reader for source positions")
	}
	if !model.ValidHexID(string(o.Repo)) {
		return nil, optionErr("a repository identity")
	}
	// max_query_terms is a config.Limit: 0 means unlimited, which is its
	// default, so it is deliberately absent from this check. So is
	// query_timeout: zero is its "no deadline" spelling and its default.
	// max_page_items stays positive because it sizes a wire page, which is a
	// lossless bound the next cursor carries the rest of.
	if o.Resources.MaxPageItems <= 0 {
		return nil, optionErr("a positive max_page_items")
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
		store:    o.Store,
		repo:     o.Repo,
		signer:   o.Signer,
		leases:   o.Leases,
		content:  o.Content,
		maxPage:  o.Resources.MaxPageItems,
		maxTerms: o.Resources.MaxQueryTerms,
		rule:     lengthRule(),
		timeout:  o.Resources.QueryTimeout.Std(),
		ttl:      ttl,
		now:      now,
		log:      logger,
	}, nil
}

// optionErr reports a missing or unusable composition input.
func optionErr(what string) error {
	return &model.Error{Code: model.CodeInternal, Message: "search: the service was composed without " + what}
}

// Close releases everything the service holds open. The store, signer and
// leases are the composition root's and are closed there; this service opens
// nothing of its own, and every per-request resource is released on the path
// that acquired it.
func (s *Service) Close() error { return nil }

// pageLimit resolves a request's page bound against resources.max_page_items,
// which New validated and the service kept. Zero means the endpoint default,
// which Section 20.1 fixes as the configured maximum, never "unlimited"; a
// request above it is clamped to it, so lowering the key really does lower the
// pages this service serves rather than only the model ceiling.
//
// It also answers the NOTICE that clamp owes the caller. model.PageRequest
// refuses a limit above model.MaxPageItems outright, so the storage layer's
// own clamp collector can never fire on this path -- every read below asks for
// exactly the wire ceiling. The reachable silent clamp is this one: an
// operator who lowers resources.max_page_items has every request above it
// served short, and a caller that cannot tell a clamped page from the end of
// an answer is the silence the scale posture forbids. The answer carries the
// notice and, as always, a cursor to read the rest.
func (s *Service) pageLimit(limit int) (int, string) {
	maximum := s.maxPage
	if maximum <= 0 || maximum > model.MaxPageItems {
		maximum = model.MaxPageItems
	}
	if limit <= 0 {
		return maximum, ""
	}
	if limit > maximum {
		return maximum, "a page bound was clamped: requested " + strconv.Itoa(limit) +
			", effective " + strconv.Itoa(maximum) +
			" (the configured resources.max_page_items; continue with the cursor to read the rest)"
	}
	return limit, ""
}

// Search answers the exact + lexical discovery endpoint one page at a time.
//
// An answer is one sequence: every exact-tier hit first, in tier order and
// within a tier by content key (exactHeap), then the lexical results in the
// search index's ranking, less the results the exact tiers already served.
// A page is the next `limit` of that sequence after the cursor's position, and
// it carries a continuation exactly when more of the sequence exists.
//
// The order of work is Section 14's: the request is bounded before any read,
// one generation is pinned for the whole request, the request's filters are
// applied to every tier before a page is drawn, and the page that is served
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
	// Every storage read this answer makes resolves its page bound against the
	// wire ceiling; a request the storage layer served at a different size than
	// it was asked for is reported on the answer rather than applied silently.
	ctx, clamps := sqlite.WithPageClamps(ctx)
	now := s.now()
	hash := searchQueryHash(req, s.rule)

	resumed := req.Page.Cursor != ""
	generation := req.GenerationID
	var cursor pagination.Cursor
	var pos searchPosition
	if resumed {
		if cursor, err = decodeCursor(s.signer, req.Page.Cursor, endpointSearch, now); err != nil {
			return empty, err
		}
		if pos, err = parseSearchPosition(cursor.LastKey); err != nil {
			return empty, err
		}
		generation = cursor.GenerationID
	}
	reader, err := s.store.PinGeneration(ctx, s.repo, generation, s.ttl)
	if err != nil {
		return empty, resumePinFailure(resumed, err)
	}
	defer reader.Close()
	binding := reader.Binding()
	if resumed {
		if err := verifyCursor(cursor, binding, hash); err != nil {
			return empty, err
		}
	}
	// Every page filters, so every page reads the snapshot's tagger.
	if err := filter.pin(ctx, s.store, binding.SnapshotID); err != nil {
		return empty, err
	}
	limit, clampNotice := s.pageLimit(req.Page.Limit)

	// resources.query_timeout, WHEN SET, bounds the CANDIDATE SEARCH and
	// nothing else: the exact tiers and the index search. It is unlimited by
	// default and never a ceiling: a caller who set a deadline of their own
	// keeps it (model.QueryDeadline). A deadline ends a PAGE, not an answer:
	// the hits found before it are served, marked truncated. Hydration runs
	// under the CALLER's context, so a search that ended on its deadline still
	// has a live context to serve its page with, and a caller who stopped
	// asking still stops all of it.
	searchCtx, searchCancel := model.QueryDeadline(ctx, s.timeout)
	ans, err := s.collect(searchCtx, reader, req, filter, pos, limit)
	searchCancel()
	if err != nil {
		return empty, err
	}
	hits, err := s.hydrated(ctx, reader, ans.hits, ans.spans)
	if err != nil {
		return empty, err
	}

	meta := model.QueryMeta{Binding: binding, Notices: clamps.Notices()}
	if ans.deadline {
		meta.Truncated, meta.TruncationReason = true, deadlineReason
	}
	if clampNotice != "" {
		meta.Notices = append(meta.Notices, clampNotice)
	}
	if meta.Completeness, err = reader.Capabilities(ctx); err != nil {
		return empty, err
	}
	if ans.next != nil && !ans.deadline {
		if meta.NextCursor, err = s.keysetNext(ctx, reader, endpointSearch, binding, hash, now, ans.next.lastKey()); err != nil {
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
	if resumed {
		s.consumed(ctx, cursor)
	}
	attrs := []any{"component", "search", "generation_id", int64(binding.GenerationID),
		"hits", len(page.Items), "truncated", meta.Truncated, "continued", meta.NextCursor != ""}
	if lx := ans.lexical; lx != nil {
		attrs = append(attrs, "matches", lx.Matches, "matches_exact", lx.MatchesExact,
			"segments_without_summary", lx.Stats.SegmentsWithoutSummary,
			"blocks_scored", lx.Stats.BlocksScored, "blocks_total", lx.Stats.BlocksTotal)
	}
	s.log.Debug("answered a search query", attrs...)
	return page, nil
}

// searchAnswer is one page's hits before hydration: each hit and the byte
// interval its source range is resolved from (nil where it has none), the
// position the next page begins at (nil when the answer ends here), whether a
// deadline ended the candidate search, and the last index page read, which only
// the debug log consumes.
type searchAnswer struct {
	hits     []model.SearchHit
	spans    []*model.ByteRange
	next     *searchPosition
	deadline bool
	lexical  *textindex.Page
}

// add appends one hit and its span.
func (a *searchAnswer) add(hit model.SearchHit, span *model.ByteRange) {
	a.hits = append(a.hits, hit)
	a.spans = append(a.spans, span)
}

// collect draws one page of the answer from pos under the search deadline.
//
// The exact phase runs unless the cursor is already in the lexical tier; it
// keeps the limit+1 least exact hits after the position, so a page that fills
// from the exact tiers alone knows whether any exact hit follows it. When it
// leaves room, the lexical phase fills the rest from the start of the lexical
// sequence (or after the cursor's lexical key), and is asked for one result
// beyond the page so the continuation is minted exactly when more exist. The
// continuation's key is the last SERVED result's, so the result past the page
// is read again, and filtered again, by the next page.
func (s *Service) collect(ctx context.Context, reader *sqlite.PinnedReader, req model.SearchRequest,
	filter hitFilter, pos searchPosition, limit int) (searchAnswer, error) {
	var ans searchAnswer
	normalized, pathFile, err := queryFile(ctx, reader, req.Query)
	if err != nil {
		return ended(ctx, ans, err)
	}
	room := limit
	var after *textindex.Position
	if pos.phase == phaseLexical {
		after = pos.after
	} else {
		held, err := s.exactPhase(ctx, reader, req, filter, pos, limit)
		for i, e := range held {
			if i == limit {
				break
			}
			ans.add(e.served())
		}
		if err != nil {
			return ended(ctx, ans, err)
		}
		if len(held) > limit {
			last := held[limit-1]
			ans.next = &searchPosition{phase: phaseExact, rank: last.Tier.Rank(), node: last.Node.Node.ID}
			return ans, nil
		}
		room = limit - len(held)
	}
	fold := newExactFold(reader, req, normalized, pathFile, filter)
	page, kept, err := s.lexicalPhase(ctx, reader, req, filter, fold, after, room)
	ans.lexical = page
	if err != nil {
		return ended(ctx, ans, err)
	}
	for _, h := range kept[:min(len(kept), room)] {
		bytes := h.Bytes
		ans.add(model.SearchHit{NodeID: h.NodeID, FileID: h.FileID, Path: h.Path, Kind: h.Kind, Name: h.Name,
			QualifiedName: h.QualifiedName, Signature: h.Signature, Tier: model.TierLexical,
			ScoreMicros: h.ScoreMicros,
			// A result is one occurrence even when the search counted none of
			// its documents: a served hit reporting zero occurrences would read
			// as a result that was not actually found.
			OccurrenceCount: max(h.Occurrences, 1),
			Reasons:         []string{reasonFor(model.TierLexical)}}, &bytes)
	}
	if page != nil && page.Canceled {
		// The context ended the index search: what it returned is the best it
		// had found, not the page's true ranking, so it is served truncated and
		// with no continuation.
		return ended(ctx, ans, model.Canceled(ctx.Err()))
	}
	if len(kept) > room {
		next := searchPosition{phase: phaseLexical}
		if room > 0 {
			at := kept[room-1].At()
			next.after = &at
		} else {
			next.after = after
		}
		ans.next = &next
	}
	return ans, nil
}

// ended settles an error the candidate search met. A deadline -- the configured
// query budget or the caller's own -- ends the page: the hits in hand are served,
// marked truncated, with no continuation. A caller that stopped asking, and any
// other failure, fails the answer.
func ended(ctx context.Context, ans searchAnswer, err error) (searchAnswer, error) {
	if errors.Is(ctx.Err(), context.DeadlineExceeded) && isDeadline(err) {
		ans.deadline, ans.next = true, nil
		return ans, nil
	}
	var typed *model.Error
	if errors.As(err, &typed) {
		return searchAnswer{}, err
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return searchAnswer{}, contextErr(err)
	}
	return searchAnswer{}, err
}

// isDeadline reports whether err is a time-budget failure: a typed deadline, or
// a bare or joined context deadline as a storage read or the index returns it.
func isDeadline(err error) bool {
	var typed *model.Error
	if errors.As(err, &typed) && typed.Code == model.CodeQueryDeadline {
		return true
	}
	return errors.Is(err, context.DeadlineExceeded)
}

// deadlineReason is the truncation a page carries when a deadline ended the
// candidate search. It names BOTH places that deadline can come from, because
// either can be the one that fired: resources.query_timeout is unlimited by
// default, so on a shipped configuration the deadline is usually the caller's
// own `--timeout`, and a reason that named only the configured key would send
// the operator to tune a setting that had nothing to do with it.
const deadlineReason = "the query time budget (--timeout, or resources.query_timeout) " +
	"ended the candidate search; this page holds the hits found before it and has no continuation"

// exactEntry is one exact-tier hit on its way to a page.
type exactEntry struct{ exactHit }

// served projects the entry into the hit that is served and its byte interval.
// The exact tiers are not ranked: a hit scores 0 and is one occurrence, and its
// reason names the tier so a caller can see why it sorts above scored hits.
func (e exactEntry) served() (model.SearchHit, *model.ByteRange) {
	n := e.Node.Node
	return model.SearchHit{NodeID: n.ID, FileID: n.FileID, Path: e.Path, Kind: n.Kind, Name: n.Name,
		QualifiedName: n.QualifiedName, Signature: n.Signature, Tier: e.Tier, ScoreMicros: 0,
		OccurrenceCount: 1, Reasons: []string{reasonFor(e.Tier)}}, e.Node.Bytes
}

// servableExact reports whether an exact candidate can be served under the
// request's filters. SearchHit requires a file and a path, so a manifest-only
// entity cannot be served as a search hit at all and is dropped here rather
// than failing the whole page at Validate.
func servableExact(e exactHit, filter hitFilter) bool {
	return e.Node.Node.FileID != "" && e.Path != "" && filter.keep(e.Path)
}

// cmpExact is the order of the exact tiers: tier rank, then the content key
// (path, start byte, end byte, kind, name, qualified name, signature), with the
// node id last only as a totality backstop. Every key but the node id is derived
// from the repository's content, so two indexes of one tree serve one order.
func cmpExact(a, b exactEntry) int {
	if c := cmp.Compare(a.Tier.Rank(), b.Tier.Rank()); c != 0 {
		return c
	}
	if c := cmp.Compare(a.Path, b.Path); c != 0 {
		return c
	}
	if c := cmp.Compare(nodeStartByte(a.Node), nodeStartByte(b.Node)); c != 0 {
		return c
	}
	if c := cmp.Compare(spanEnd(a.Node.Bytes), spanEnd(b.Node.Bytes)); c != 0 {
		return c
	}
	an, bn := a.Node.Node, b.Node.Node
	if c := cmp.Compare(an.Kind, bn.Kind); c != 0 {
		return c
	}
	if c := cmp.Compare(an.Name, bn.Name); c != 0 {
		return c
	}
	if c := cmp.Compare(an.QualifiedName, bn.QualifiedName); c != 0 {
		return c
	}
	if c := cmp.Compare(an.Signature, bn.Signature); c != 0 {
		return c
	}
	return cmp.Compare(an.ID, bn.ID)
}

// spanEnd is a node's end byte, or -1 when it has no byte range: below every
// real end byte and the same in every index, so the order stays total.
func spanEnd(s *model.ByteRange) int64 {
	if s == nil {
		return -1
	}
	return int64(s.End)
}

// exactHeap keeps the least `bound` exact hits it is offered under cmpExact. The
// root is the entry that sorts last, which an overflowing offer evicts, so its
// memory is the bound and never the tier's match count. exactCandidates emits a
// node once, at its tier, so the heap holds no set of node ids.
type exactHeap struct {
	items []exactEntry
	bound int
}

func newExactHeap(bound int) *exactHeap { return &exactHeap{bound: bound} }

func (h *exactHeap) Len() int           { return len(h.items) }
func (h *exactHeap) Less(i, j int) bool { return cmpExact(h.items[i], h.items[j]) > 0 }
func (h *exactHeap) Swap(i, j int)      { h.items[i], h.items[j] = h.items[j], h.items[i] }
func (h *exactHeap) Push(x any)         { h.items = append(h.items, x.(exactEntry)) }

func (h *exactHeap) Pop() any {
	last := h.items[len(h.items)-1]
	h.items = h.items[:len(h.items)-1]
	return last
}

// offer admits one entry, evicting the worst once more than bound are held.
func (h *exactHeap) offer(e exactEntry) {
	heap.Push(h, e)
	if len(h.items) > h.bound {
		heap.Pop(h)
	}
}

// sorted is the held entries in served order.
func (h *exactHeap) sorted() []exactEntry {
	out := make([]exactEntry, len(h.items))
	copy(out, h.items)
	slices.SortFunc(out, cmpExact)
	return out
}

// exactPhase walks the exact tiers from the cursor's position and answers the
// least limit+1 hits after it, in served order. On an error it still answers
// what it held, so a deadline can serve them.
//
// A position names its last hit by tier and node; the full content key that
// hit sorted under is read back from that tier first (positionKey), and every
// later candidate is compared against it.
func (s *Service) exactPhase(ctx context.Context, reader exactReader, req model.SearchRequest,
	filter hitFilter, pos searchPosition, limit int) ([]exactEntry, error) {
	span := allExactTiers
	var after *exactEntry
	if pos.phase == phaseExact {
		span.from = pos.rank
		key, err := positionKey(ctx, reader, req, filter, pos)
		if err != nil {
			return nil, err
		}
		after = &key
	}
	h := newExactHeap(limit + 1)
	err := exactCandidates(ctx, reader, req.Query, req.Kinds, filter, model.MaxPageItems, span, func(e exactHit) error {
		if !servableExact(e, filter) {
			return nil
		}
		entry := exactEntry{e}
		if after != nil && cmpExact(entry, *after) <= 0 {
			return nil
		}
		h.offer(entry)
		return nil
	})
	return h.sorted(), err
}

// errPositionFound stops the walk positionKey runs once it has its hit.
var errPositionFound = errors.New("search: the continuation's position was found")

// positionKey reads back the hit a continuation's exact position names: the
// tier the position names is walked until the node appears. Within one tier a
// node is emitted once, and within one pinned generation the tier answers the
// same facts every time, so the hit found is the one the previous page served.
// A position whose hit is not in its tier was not minted for this answer.
func positionKey(ctx context.Context, reader exactReader, req model.SearchRequest, filter hitFilter,
	pos searchPosition) (exactEntry, error) {
	var found exactEntry
	err := exactCandidates(ctx, reader, req.Query, req.Kinds, filter, model.MaxPageItems,
		tierSpan{from: pos.rank, to: pos.rank}, func(e exactHit) error {
			if e.Node.Node.ID != pos.node || !servableExact(e, filter) {
				return nil
			}
			found = exactEntry{e}
			return errPositionFound
		})
	switch {
	case errors.Is(err, errPositionFound):
		return found, nil
	case err != nil:
		return exactEntry{}, err
	}
	return exactEntry{}, cursorInvalid("the search cursor names a position that is not in its answer")
}

// lexicalPhase reads the lexical results after `after` (nil: from the first)
// through the generation's index view until it holds more than `room` of them
// after the service's own filters, or the index has no more. It answers the
// last index page (nil when none was read) and the kept results, in order.
//
// The index applies the kinds and the path prefixes itself; the languages
// (through the snapshot's tagger) and the exact fold (exactFold) are applied
// here, so the index can return fewer usable results than it was asked for.
// The first request asks for room+1 results; while the kept list is short and
// the index says more exist, the next request resumes after the last result
// the previous one returned and asks for twice as many. No request re-reads a
// result an earlier one returned, so a page costs the results it reads, never
// the results served before it.
func (s *Service) lexicalPhase(ctx context.Context, reader *sqlite.PinnedReader, req model.SearchRequest,
	filter hitFilter, fold exactFold, after *textindex.Position, room int) (*textindex.Page, []textindex.Hit, error) {
	var read *textindex.Page
	view, err := reader.SearchView(ctx)
	if err != nil {
		return read, nil, err
	}
	defer view.Close()
	want := room + 1
	depth := want
	var kept []textindex.Hit
	for {
		r := textindex.Request{Query: req.Query, Kinds: req.Kinds, PathPrefixes: filter.prefixes, Depth: depth,
			After: after, LengthRule: s.rule, MaxTerms: s.maxTerms.Value()}
		if observeIndexRequest != nil {
			observeIndexRequest(r)
		}
		page, err := view.Search(ctx, r)
		if err != nil {
			return read, nil, s.lexicalErr(err)
		}
		read = &page
		served, err := fold.served(ctx, page.Hits)
		if err != nil {
			return read, kept, err
		}
		for _, h := range page.Hits {
			if len(kept) == want {
				break
			}
			if filter.keep(h.Path) && !served[h.NodeID] {
				kept = append(kept, h)
			}
		}
		// A canceled page is the best found so far, not the ranking's next
		// results: it is served as it is, truncated, and ends the read.
		if page.Canceled || len(kept) == want || !page.More || len(page.Hits) == 0 {
			return read, kept, nil
		}
		at := page.Hits[len(page.Hits)-1].At()
		after = &at
		if depth > math.MaxInt/2 {
			depth = math.MaxInt
		} else {
			depth *= 2
		}
	}
}

// observeIndexRequest, when set, sees every request lexicalPhase sends the
// index, before it is sent. It is nil in production: it is the seam a test
// observes the reads of a page through.
var observeIndexRequest func(textindex.Request)

// lexicalErr types an index refusal. A query over resources.max_query_terms is
// refused rather than scored on its first terms: a query that scores only some
// of its terms would return confidently wrong rankings.
func (s *Service) lexicalErr(err error) error {
	if errors.Is(err, textindex.ErrUnknownPosition) {
		return cursorInvalid("the search cursor names a position that is not in its answer")
	}
	var many *textindex.TooManyTerms
	if errors.As(err, &many) {
		return &model.Error{Code: model.CodeArgumentInvalid,
			Message: "the query carries " + strconv.FormatInt(many.Terms, 10) +
				" terms, more than the resources.max_query_terms limit of " + s.maxTerms.String(),
			Remediation: "narrow the query, or raise resources.max_query_terms (0 or \"unlimited\" scores every term)"}
	}
	return err
}

// exactFold recognizes a lexical result whose node the exact tiers served for
// the same request, so the answer serves that node once, at its exact tier. It
// asks tierRule, the rule the exact walk applies: over every visible row of the
// node and the request's filters, not over the result's own fields, which are
// one unit's document, nor over reader.Node, which is one precedence winner.
type exactFold struct{ rule *tierRule }

// newExactFold builds the fold for one request. Its tier reads are point reads
// (size 1): a page's nodes are sparse in any one tier.
func newExactFold(reader exactReader, req model.SearchRequest, normalized string, pathFile model.FileID,
	filter hitFilter) exactFold {
	return exactFold{rule: newTierRule(reader, req.Query, req.Kinds, filter, normalized, pathFile, 1)}
}

// served answers which of the hits' nodes the exact tiers served. The nodes are
// asked in ascending id order, so each tier's keyset is read forward and a tier
// that holds none of them costs one read.
func (f exactFold) served(ctx context.Context, hits []textindex.Hit) (map[model.NodeID]bool, error) {
	ids := make([]model.NodeID, 0, len(hits))
	for _, h := range hits {
		if h.NodeID != "" {
			ids = append(ids, h.NodeID)
		}
	}
	slices.Sort(ids)
	ids = slices.Compact(ids)
	out := make(map[model.NodeID]bool, len(ids))
	for _, id := range ids {
		owned, err := f.rule.owned(ctx, id)
		if err != nil {
			return nil, err
		}
		out[id] = owned
	}
	return out, nil
}

// hydrated fills the source range of every hit of one page that has a byte
// interval; spans[i] belongs to hits[i].
func (s *Service) hydrated(ctx context.Context, reader *sqlite.PinnedReader, hits []model.SearchHit,
	spans []*model.ByteRange) ([]model.SearchHit, error) {
	located := make([]int, 0, len(hits))
	ranges := make([]model.ByteRange, 0, len(hits))
	for i, sp := range spans {
		if sp != nil {
			located = append(located, i)
			ranges = append(ranges, *sp)
		}
	}
	if err := s.hydrate(ctx, reader, hits, located, ranges); err != nil {
		return nil, err
	}
	return hits, nil
}

// releaseLease returns a lease whose cursor never reached the caller. The
// request's own context may already be done, so the release runs on a fresh
// one: leaking the lease would pin a generation against retention for the full
// cursor TTL for a page nobody can ask for.
func (s *Service) releaseLease(ctx context.Context, id string) {
	if id == "" {
		// A keyset continuation minted by a writerless process holds none.
		return
	}
	if err := s.leases.Release(context.WithoutCancel(ctx), id); err != nil {
		s.log.Warn("a continuation lease could not be released", "component", "search", "error", err.Error())
	}
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

// reasonFor is the bounded explanation a hit carries for the tier it was found
// by. The exact and prefix tiers score 0 by construction, so without this a
// caller would see a hit ranked above a scoring one with nothing saying why.
func reasonFor(t model.SearchTier) string { return "matched the " + string(t) + " tier" }

// hitFilter is SearchRequest.Languages and .Paths, applied uniformly to every
// tier. The lexical tier applies the path prefixes inside the search index and
// this package applies the languages to its results, so every page of one
// answer is drawn from the same filtered sequence.
//
// Paths are root-relative path PREFIXES: a hit is kept when its file path
// equals a listed prefix or lies under it. Languages are matched by the pinned
// snapshot's lang.Tagger, the one place a repository path becomes a language
// name, so a filter cannot disagree with the language the manifest recorded,
// a header's included.
type hitFilter struct {
	prefixes  []string
	languages map[string]bool
	tag       lang.Tagger
}

// pin binds the language filter to the pinned snapshot's tagger. The snapshot
// header is read only when a language filter is set, since only it tags.
func (f *hitFilter) pin(ctx context.Context, store *sqlite.Store, id model.SnapshotID) error {
	if f.languages == nil {
		return nil
	}
	snap, err := store.Snapshot(ctx, id)
	if err != nil {
		return err
	}
	f.tag = lang.For(snap)
	return nil
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
	if f.languages != nil && !f.languages[f.tag.Of(p)] {
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
	ctx, cancel := model.QueryDeadline(ctx, s.timeout)
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
		return empty, resumePinFailure(req.Page.Cursor != "", err)
	}
	defer reader.Close()
	binding := reader.Binding()
	if req.Page.Cursor != "" {
		if err := verifyCursor(cursor, binding, hash); err != nil {
			return empty, err
		}
	}

	limit, clampNotice := s.pageLimit(req.Page.Limit)
	result, err := resolveSymbols(ctx, reader, req, cursor.LastKey, limit)
	if err != nil {
		return empty, err
	}
	if err := s.hydrateNodes(ctx, reader, result.Nodes, result.Spans); err != nil {
		return empty, err
	}
	meta := model.QueryMeta{Binding: binding}
	if clampNotice != "" {
		meta.Notices = append(meta.Notices, clampNotice)
	}
	if meta.Completeness, err = reader.Capabilities(ctx); err != nil {
		return empty, err
	}
	meta.Completeness = append(meta.Completeness, result.Completeness...)
	if result.LastKey != "" {
		if meta.NextCursor, err = s.keysetNext(ctx, reader, endpointSymbol, binding, hash, now, result.LastKey); err != nil {
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

// keysetNext signs a keyset continuation of either endpoint. It carries its whole position in
// the token and writes nothing, so the only thing it needs of the workspace is
// that the generation it names still be readable when it is presented.
//
// A writer-bearing process still takes a cursor-owned lease, which retains that
// generation for the token's life. A process that opened the store read-only
// takes none and mints the token anyway: it has nothing on disk to keep alive,
// and if the generation is collected before the token comes back, the pin finds
// no row and the caller is told to re-run from the first page. Refusing the
// continuation instead would make `symbol` page only once for the whole length
// of another process's index, which is the one thing a writerless answer exists
// to avoid.
func (s *Service) keysetNext(ctx context.Context, reader *sqlite.PinnedReader, endpoint string, b model.Binding,
	hash string, now time.Time, lastKey string) (string, error) {
	var leaseID string
	if reader.Continuable() {
		lease, err := s.leases.Acquire(ctx, b.GenerationID, b.SnapshotID, model.LeaseCursor)
		if err != nil {
			return "", err
		}
		leaseID = lease.ID
	}
	next := newCursor(endpoint, b, leaseID, hash, now.Add(s.ttl))
	next.LastKey = lastKey
	token, err := s.signer.EncodeCursor(next)
	if err != nil {
		s.releaseLease(ctx, leaseID)
		return "", err
	}
	return token, nil
}
