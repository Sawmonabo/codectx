// Package search answers the Section 14 discovery endpoints over one pinned
// generation: exact/prefix symbol and path lookup, and generation-local BM25
// lexical retrieval. It holds no graph, context-compiler, MCP or LSP concern.
package search

import (
	"context"
	"errors"
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
	// runBytes is the in-memory run budget one query's ranking sort may hold,
	// a share of resources.query_memory_bytes.
	runBytes int64
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
	// max_query_terms is a config.Limit: 0 means unlimited, which is its
	// default, so it is deliberately absent from this check. So is
	// query_timeout: zero is its "no deadline" spelling and its default, and a
	// query nobody bounded is meant to return the COMPLETE answer rather than
	// the first page of one. max_page_items stays positive because it sizes a
	// wire page, which is a lossless bound the next cursor carries the rest of.
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
		spools:   o.Spools,
		leases:   o.Leases,
		content:  o.Content,
		lexical:  newLexicalTier(o.Store, o.Resources.MaxQueryTerms, defaultStatsCacheBytes),
		maxPage:  o.Resources.MaxPageItems,
		runBytes: pagination.SortRunBytes(o.Resources.QueryMemoryBytes),
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
// spools are the composition root's and are closed there in reverse order;
// this service opens nothing of its own, and every per-request resource is
// released on the path that acquired it.
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
	// resources.query_timeout, WHEN SET, bounds the CANDIDATE SEARCH and
	// nothing else. It is unlimited by default and it is never a ceiling: a
	// search nobody bounded ranks every candidate, and a caller who set a
	// deadline of their own keeps it (model.QueryDeadline). That search is the
	// only unbounded part of this answer -- the tiers walk their keysets to
	// the end -- and ruling Q4 says a deadline ends a PAGE, not an answer: the
	// hits ranked before it are a real ordered prefix, and a query that
	// returned nothing at all because it took too long to look is the
	// refusal the scale posture forbids. Everything after the search (pinning,
	// selecting one page, writing the candidates into the continuation spool)
	// is work over a set already in hand and runs under the CALLER's context --
	// so a search that ended on its deadline still has a live context to serve
	// its page with, and a caller who stopped asking still stops all of it.
	// Every storage read this answer makes resolves its page bound against the
	// wire ceiling; a request the storage layer served at a different size than
	// it was asked for is reported on the answer rather than applied silently.
	ctx, clamps := sqlite.WithPageClamps(ctx)
	now := s.now()
	hash := searchQueryHash(req)

	generation := req.GenerationID
	var resumed searchCursor
	if req.Page.Cursor != "" {
		if resumed, err = s.resumeSearch(req.Page.Cursor, hash, now); err != nil {
			return empty, err
		}
		generation = resumed.GenerationID
	}
	reader, err := s.store.PinGeneration(ctx, s.repo, generation, s.ttl)
	if err != nil {
		return empty, err
	}
	defer reader.Close()
	binding := reader.Binding()

	limit, clampNotice := s.pageLimit(req.Page.Limit)
	var (
		hits      []model.SearchHit
		next      string
		truncated bool
		reason    string
	)
	if req.Page.Cursor != "" {
		if err := checkSearchCursor(resumed, binding); err != nil {
			return empty, err
		}
		truncated, reason = resumed.Truncated, resumed.TruncationReason
		if hits, next, err = s.resumedPage(ctx, reader, resumed, limit); err != nil {
			return empty, err
		}
	} else {
		var run *pagination.SortedRun[scored]
		searchCtx, searchCancel := model.QueryDeadline(ctx, s.timeout)
		run, truncated, reason, err = s.rank(searchCtx, reader, req, filter)
		searchCancel()
		if err != nil {
			return empty, err
		}
		defer run.Close()
		if hits, next, truncated, reason, err = s.firstPage(ctx, reader, run, limit,
			binding, hash, now, truncated, reason); err != nil {
			return empty, err
		}
	}

	meta := model.QueryMeta{Binding: binding, Truncated: truncated, TruncationReason: reason,
		NextCursor: next, Notices: clamps.Notices()}
	if clampNotice != "" {
		meta.Notices = append(meta.Notices, clampNotice)
	}
	if meta.Completeness, err = reader.Capabilities(ctx); err != nil {
		return empty, err
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
	s.log.Debug("answered a search query", "component", "search", "generation_id", int64(binding.GenerationID),
		"hits", len(page.Items), "truncated", meta.Truncated, "continued", meta.NextCursor != "")
	return page, nil
}

// firstPage serves page 1 out of the bounded heap and, when the answer runs
// past it, lays every scored candidate into the raw continuation spool
// (ADR-0007 Decision 3). It returns the page, the continuation token, and the
// answer-level truncation as the spool write left it.
//
// ONE walk of the candidate set does both. The heap holds one page plus one
// entry, so on the arrival that first overflows the page the heap holds
// exactly the candidates seen so far: that is the moment the spool is opened
// and those candidates are laid into it, and every later arrival is appended as
// it passes. An answer that fits one page never reaches that moment and
// therefore writes no spool, takes no lease and runs no sort.
func (s *Service) firstPage(ctx context.Context, reader *sqlite.PinnedReader, run *pagination.SortedRun[scored],
	limit int, b model.Binding, hash string, now time.Time, truncated bool, reason string) ([]model.SearchHit, string, bool, string, error) {
	h := &pageHeap{limit: limit}
	w := &rawWriter{svc: s, binding: b, hash: hash, now: now}
	total := int64(0)
	err := run.Each(func(v scored) error {
		if err := ctx.Err(); err != nil {
			return contextErr(err)
		}
		// The deduplication pass is closed, so a survivor's folded score is
		// final and becomes the score it is ranked, spooled and served with.
		v.ScoreMicros = v.Folded
		total++
		if w.started() {
			if err := w.append(ctx, v); err != nil {
				return err
			}
			h.offer(v)
			return nil
		}
		h.offer(v)
		if !h.full() {
			return nil
		}
		return w.start(ctx, h.items)
	})
	if err != nil {
		w.abandon(ctx)
		return nil, "", false, "", err
	}
	hits, err := s.hydrated(ctx, reader, h.page())
	if err != nil {
		w.abandon(ctx)
		return nil, "", false, "", err
	}
	spool, err := w.close(ctx)
	if err != nil {
		return nil, "", false, "", err
	}
	if w.dropped {
		// A spool budget that is already full must end THIS page, never the
		// query: the hits of page 1 are in hand and refusing to serve them
		// because the tail could not be written is the class-D refusal the
		// scale posture forbids. This reason OVERWRITES whatever a tier
		// reported: a tier's truncation says the candidate set was bounded,
		// this one says a named number of already-ranked hits were thrown away
		// and there is no continuation to reach them.
		dropped := int(total) - len(hits)
		s.log.Warn("a search answer was cut short by the spool disk budget", "component", "search",
			"generation_id", int64(b.GenerationID), "dropped_hits", dropped)
		return hits, "", true, spoolBudgetFullReason(dropped), nil
	}
	if spool == "" {
		return hits, "", truncated, reason, nil
	}
	c := s.newSearchCursor(b, hash, w.lease, now)
	c.SpoolID, c.Served, c.Total = spool, int64(len(hits)), total
	c.Truncated, c.TruncationReason = truncated, reason
	token, err := s.signSearchCursor(c)
	if err != nil {
		s.spools.Release(spool)
		s.releaseLease(ctx, w.lease)
		return nil, "", false, "", err
	}
	return hits, token, truncated, reason, nil
}

// rawWriter lays an answer's scored candidates into the raw continuation
// spool, in the order the deduplication pass presents them, under the sort's
// own record codec. Ordering them is exactly the cost ADR-0007 Decision 3
// defers to the request that needs it.
//
// It is opened LAZILY, by the first candidate past the page bound, so a
// single-page answer takes neither a lease nor a spool. The cursor-owned lease
// is minted here and not the pinned reader's query lease: that one is released
// when the request returns, and a continuation whose lease is gone is refused
// by the spool store.
type rawWriter struct {
	svc     *Service
	binding model.Binding
	hash    string
	now     time.Time
	lease   string
	spool   *pagination.Spool
	// dropped records that the shared spool budget refused a record. The walk
	// continues -- the count of candidates is what the truncation reason names
	// -- but nothing further is written and the answer carries no continuation.
	dropped bool
}

// started reports whether the spool has been opened, or opening it has already
// been given up on.
func (w *rawWriter) started() bool { return w.spool != nil || w.dropped }

// start opens the spool and writes every candidate the heap holds.
func (w *rawWriter) start(ctx context.Context, held []scored) error {
	lease, err := w.svc.leases.Acquire(ctx, w.binding.GenerationID, w.binding.SnapshotID, model.LeaseCursor)
	if err != nil {
		return err
	}
	w.lease = lease.ID
	c := w.svc.newSearchCursor(w.binding, w.hash, lease.ID, w.now)
	sp, err := w.svc.spools.Create(c.spoolBinding())
	if err != nil {
		w.svc.releaseLease(ctx, lease.ID)
		w.lease = ""
		if pagination.IsBudgetExhausted(err) {
			w.dropped = true
			return nil
		}
		return err
	}
	w.spool = sp
	for _, v := range held {
		if err := w.append(ctx, v); err != nil {
			return err
		}
	}
	return nil
}

// append writes one candidate. A budget refusal ends the WRITING, not the
// walk; every other spool failure -- a disk fault, a corrupt spool -- is a real
// fault and still surfaces.
func (w *rawWriter) append(ctx context.Context, v scored) error {
	if w.spool == nil {
		return nil
	}
	record, err := encodeScored(v)
	if err != nil {
		return err
	}
	if err := w.spool.Append(record); err != nil {
		if pagination.IsBudgetExhausted(err) {
			w.abandon(ctx)
			w.dropped = true
			return nil
		}
		return err
	}
	return nil
}

// close flushes the spool and returns the id the continuation names, or "" when
// the answer fit one page or the budget refused it.
func (w *rawWriter) close(ctx context.Context) (string, error) {
	if w.spool == nil {
		return "", nil
	}
	if err := w.spool.Close(); err != nil {
		w.svc.spools.Release(w.spool.ID())
		w.svc.releaseLease(ctx, w.lease)
		w.spool = nil
		return "", err
	}
	return w.spool.ID(), nil
}

// abandon returns a spool and lease whose page never reached the caller.
func (w *rawWriter) abandon(ctx context.Context) {
	if w.spool != nil {
		releaseSpool(w.svc.spools, w.spool)
		w.spool = nil
	}
	if w.lease != "" {
		w.svc.releaseLease(ctx, w.lease)
		w.lease = ""
	}
}

// resumedPage serves one page out of the spool the walk is reading, ordering
// the candidates first if this is the first continuation (ADR-0007 Decision 3).
// Once the order is settled the page costs its own page and never the
// remainder behind it: it seeks to the byte offset the presented cursor
// carries and stops at its own last record.
func (s *Service) resumedPage(ctx context.Context, reader *sqlite.PinnedReader, c searchCursor,
	limit int) ([]model.SearchHit, string, error) {
	if !c.Ordered {
		if err := s.orderSpool(ctx, &c); err != nil {
			return nil, "", err
		}
	}
	now := s.now()
	chunk := make([]scored, 0, min(limit, model.MaxPageItems))
	end, err := s.spools.OpenAt(ctx, c.spoolCursor(), now, c.Offset, func(record []byte) error {
		if err := ctx.Err(); err != nil {
			return contextErr(err)
		}
		v, err := decodeScored(record)
		if err != nil {
			return err
		}
		if chunk = append(chunk, v); len(chunk) == limit {
			return pagination.ErrStopSpool
		}
		return nil
	})
	if err != nil {
		return nil, "", err
	}
	if len(chunk) == 0 {
		// The spool ended before Total. The cursor's counts and the file
		// disagree, which is corruption of the continuation state, not an
		// answer that quietly stops short.
		return nil, "", cursorInvalid("continuation state is shorter than the answer it names")
	}
	hits, err := s.hydrated(ctx, reader, chunk)
	if err != nil {
		return nil, "", err
	}
	served := c.Served + int64(len(chunk))
	if served >= c.Total {
		// The answer is complete. The spool and the lease it pinned are
		// released here rather than left to their TTL, so a walk read to
		// exhaustion holds nothing.
		if err := s.spools.Release(c.SpoolID); err != nil {
			s.releaseLease(ctx, c.LeaseID)
			return nil, "", err
		}
		s.releaseLease(ctx, c.LeaseID)
		return hits, "", nil
	}
	// The spool's liveness is its lease's and the token's is its own expiry, so
	// a cursor that carried page one's expiry forward would bound the WHOLE
	// answer by one CursorTTL: a long answer would stop being reachable part
	// way through, which is truncation by clock.
	if _, err := s.leases.Renew(ctx, c.LeaseID); err != nil {
		return nil, "", err
	}
	next := c
	next.Offset, next.Served = end, served
	next.ExpiresAt = now.Add(s.ttl).UTC().Truncate(time.Second)
	token, err := s.signSearchCursor(next)
	if err != nil {
		return nil, "", err
	}
	return hits, token, nil
}

// orderSpool runs the external merge sort ONCE, on the first continuation,
// over the raw candidate spool the first page wrote, and lays the ordered tail
// -- the candidates page 1 did not serve -- into the spool every later page
// reads by byte offset. c is advanced onto that spool.
//
// The raw spool is released as soon as the ordered run exists and BEFORE the
// tail spool is created, so the shared byte budget is never charged for both at
// once: the ordered run lives in the sort directory, which
// resources.max_temp_bytes deliberately does not bound (pagination.Spools.SortDir).
func (s *Service) orderSpool(ctx context.Context, c *searchCursor) error {
	sorter, err := newScoredSort(s.spools.SortDir(), "searchrank-", s.runBytes, cmpScored)
	if err != nil {
		return err
	}
	defer sorter.Close()
	now := s.now()
	if err := s.spools.Open(ctx, c.spoolCursor(), now, func(record []byte) error {
		if err := ctx.Err(); err != nil {
			return contextErr(err)
		}
		v, err := decodeScored(record)
		if err != nil {
			return err
		}
		return sorter.Add(v)
	}); err != nil {
		return err
	}
	run, err := sorter.Sorted()
	if err != nil {
		return err
	}
	defer run.Close()
	if run.Len() != c.Total {
		return cursorInvalid("continuation state holds a different number of results than the answer it names")
	}
	if err := s.spools.Release(c.SpoolID); err != nil {
		return err
	}
	sp, err := s.spools.Create(c.spoolBinding())
	if err != nil {
		return err
	}
	// Written counts the header frame Create wrote, so this is where the first
	// record lands and where this page begins reading.
	offset := sp.Written()
	at := int64(0)
	err = run.Each(func(v scored) error {
		if err := ctx.Err(); err != nil {
			return contextErr(err)
		}
		// The candidates page 1 served are not written again: the walk resumes
		// at the first record the caller has not seen.
		if at++; at <= c.Served {
			return nil
		}
		record, err := encodeScored(v)
		if err != nil {
			return err
		}
		return sp.Append(record)
	})
	if err == nil {
		err = sp.Close()
	}
	if err != nil {
		releaseSpool(s.spools, sp)
		return err
	}
	c.SpoolID, c.Offset, c.Ordered = sp.ID(), offset, true
	return nil
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

// hitFacts groups the servable facts of one candidate for the collector's add:
// the hit as it will be served, and the byte interval its source range is
// hydrated from. They travel on the candidate itself, so this is an argument
// group and nothing holds one.
type hitFacts struct {
	hit  model.SearchHit
	span *model.ByteRange
}

// rank runs every tier for one request and folds the candidates into the
// distinct set, which it answers as a disk-backed sorted run in
// deduplication-key order together with the answer-level truncation.
//
// It neither ranks nor hydrates. Rank order is settled downstream, by the
// bounded page heap for the page that is served and by one external sort over
// the raw spool on the first continuation. Hydration is paid ONE PAGE AT A
// TIME by whichever request serves that page, out of the byte interval each
// record carries with it -- a spool record is a scored candidate, span
// included, so a continuation hydrates its own page from the same facts.
func (s *Service) rank(ctx context.Context, reader *sqlite.PinnedReader, req model.SearchRequest, filter hitFilter) (*pagination.SortedRun[scored], bool, string, error) {
	c, err := newCollector(s.spools.SortDir(), s.runBytes)
	if err != nil {
		return nil, false, "", err
	}
	defer c.Close()

	// The exact tiers first. model.MaxPageItems is the READ size of one keyset
	// step -- storage clamps any larger request to it (pageLimit), so asking for
	// more would be a bound this code believes and the database does not -- not
	// a bound on how many candidates a tier may yield. Candidates are
	// streamed into the collector, so neither the whole tier result nor the
	// distinct set ever materialises in heap.
	deadline := false
	if err := exactCandidates(ctx, reader, req.Query, req.Kinds, model.MaxPageItems,
		func(e exactHit) error {
			r, ok := exactRanked(e)
			if !ok || !filter.keep(e.Path) {
				return nil
			}
			return c.add(r, exactHitOf(e), reasonFor(e.Tier))
		}); err != nil {
		// Ruling Q4: the time budget ends a PAGE, not an answer. What the
		// collector already holds is a real ordered prefix of the answer, and
		// throwing it away to return an error means the widest queries -- the
		// only ones that ever reach the deadline -- have no servable answer at
		// all. The deadline stops the tiers; everything below still runs.
		if !isQueryDeadline(err) {
			return nil, false, "", err
		}
		deadline = true
	}

	var outcome lexicalOutcome
	if !deadline {
		var err error
		if outcome, err = s.lexicalCandidates(ctx, reader, req, filter, c); err != nil {
			if !isQueryDeadline(err) {
				return nil, false, "", err
			}
			deadline = true
		}
	}

	truncated, reason := c.truncation()
	if !truncated && outcome.Truncated {
		truncated, reason = true, outcome.Reason
	}
	if deadline {
		// The deadline OVERWRITES a tier's reason: a tier's truncation says the
		// candidate set was bounded, this says the query stopped looking.
		truncated, reason = true, deadlineReason
	}

	run, err := c.deduped()
	if err != nil {
		return nil, false, "", err
	}
	return run, truncated, reason, nil
}

// deadlineReason is the truncation a page carries when a deadline ended the
// candidate search. It names BOTH places that deadline can come from, because
// either can be the one that fired: resources.query_timeout is unlimited by
// default, so on a shipped configuration the deadline is usually the caller's
// own `--timeout`, and a reason that named only the configured key would send
// the operator to tune a setting that had nothing to do with it. The answer
// still carries its continuation: the hits ranked before the deadline are
// paged and their tail is spooled like any other answer's.
const deadlineReason = "the query time budget (--timeout, or resources.query_timeout) " +
	"ended the candidate search; this page holds the hits ranked before it, and the cursor continues them"

// isQueryDeadline reports whether err is a time-budget failure. It is the one
// error the candidate search converts into a truncated page rather than a
// failed query.
func isQueryDeadline(err error) bool {
	var typed *model.Error
	if errors.As(err, &typed) {
		return typed.Code == model.CodeQueryDeadline
	}
	// A storage read returns the bare context error; only this package maps it
	// to the typed answer, and the tiers are below that mapping.
	return errors.Is(err, context.DeadlineExceeded)
}

// hydrated projects one chunk of ranked candidates into served hits and fills
// their source ranges.
//
// Hydration is paid per chunk rather than once for the whole answer: a
// hydrator caches one blob record per file, so hydrating a whole answer
// through one of them would be a second structure sized by the answer in place
// of the one this lane removed.
func (s *Service) hydrated(ctx context.Context, reader *sqlite.PinnedReader, chunk []scored) ([]model.SearchHit, error) {
	hits := make([]model.SearchHit, 0, len(chunk))
	spans := make([]model.ByteRange, 0, len(chunk))
	located := make([]int, 0, len(chunk))
	for _, it := range chunk {
		if it.Span != nil {
			located = append(located, len(hits))
			spans = append(spans, *it.Span)
		}
		hits = append(hits, servableHit(it))
	}
	if err := s.hydrate(ctx, reader, hits, located, spans); err != nil {
		return nil, err
	}
	return hits, nil
}

// servableHit projects a ranked candidate into the hit that is served.
func servableHit(it scored) model.SearchHit {
	hit := it.Hit
	hit.Tier, hit.ScoreMicros, hit.Reasons = it.Tier, it.ScoreMicros, it.Reasons
	// A candidate that survived is one occurrence even when nothing folded
	// into it: an exact-tier node contributes no lexical document of its own,
	// and a served hit that reported zero occurrences would read as a result
	// that was not actually found.
	hit.OccurrenceCount = max(it.Occurrences, 1)
	return hit
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

// lexicalCandidates streams the lexical tier into the collector. The tier
// emits the search document it scored, so the sort tuple's path, start byte
// and identity -- and the servable facts -- come out of the read the tier had
// already paid for. There is no second hydration pass and no pending batch
// here: the candidate is consumed on arrival, in the order the tier emits it.
func (s *Service) lexicalCandidates(ctx context.Context, reader *sqlite.PinnedReader, req model.SearchRequest,
	filter hitFilter, c *collector) (lexicalOutcome, error) {
	return s.lexical.search(ctx, readerPostings{reader}, reader.Binding().AnalysisKey, req.Query, func(h lexicalHit) error {
		d := h.Doc
		// The filters run HERE and not inside the tier: the tier's scoring
		// pass is what observes a document whose term offsets overflowed, and
		// that lower-bound flag belongs to the answer's meta whether or not
		// the document survives a path or kind filter.
		if !filter.keep(d.Path) || !matchesKinds(d.Kind, req.Kinds) {
			return nil
		}
		r := ranked{Tier: model.TierLexicalFTS, ScoreMicros: h.ScoreMicros, Path: d.Path,
			StartByte: d.Bytes.Start, NodeID: d.NodeID, SearchKey: d.ID, RowID: d.RowID,
			// One per lexical DOCUMENT, so several documents of one node
			// fold to that many occurrences (digest §4). The matched term
			// instances inside a document are the score's business, not
			// the occurrence count's.
			Occurrences: 1}
		bytes := d.Bytes
		return c.add(r, hitFacts{span: &bytes, hit: model.SearchHit{NodeID: d.NodeID, FileID: d.FileID,
			Path: d.Path, Kind: d.Kind, Name: d.Name, QualifiedName: d.QualifiedName, Signature: d.Signature}},
			reasonFor(model.TierLexicalFTS))
	})
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
		return empty, err
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
