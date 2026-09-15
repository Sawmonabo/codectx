package search

import (
	"context"
	"encoding/json"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/Sawmonabo/codectx/internal/model"
	"github.com/Sawmonabo/codectx/internal/pagination"
)

// The two Section 14.4 endpoint names. A cursor minted by one is rejected by
// the other: its sort tuple and query hash mean nothing across endpoints, and
// honouring it would silently repin the request.
const (
	endpointSearch = "search"
	endpointSymbol = "symbol"
)

// queryHashDomain versions the cursor preimage. Changing the preimage without
// changing this would let a cursor minted by the old shape resume under the
// new one against a differently-filtered candidate set.
const queryHashDomain = "codectx.search.v1"

// filterSep joins the values of one filter group. It cannot occur inside a
// path, a language or a node kind, so "a\x00b" and "ab" hash differently.
const filterSep = "\x00"

// searchQueryHash is the digest §5 preimage for the search endpoint: the
// endpoint, the normalized query text and every filter, each group sorted so
// the caller's argument order cannot mint two cursors for one query, and each
// joined with a separator no value can contain. A changed filter therefore
// cannot resume a page built under the old one.
func searchQueryHash(req model.SearchRequest) string {
	kinds := make([]string, len(req.Kinds))
	for i, k := range req.Kinds {
		kinds[i] = string(k)
	}
	return model.H(queryHashDomain, endpointSearch, normalizeQuery(req.Query),
		sortedJoin(kinds), sortedJoin(req.Languages), sortedJoin(req.Paths), "")
}

// symbolQueryHash is the same preimage for the symbol endpoint. Resolve has no
// kind/language/path filters; its discriminators are the operation, the
// semantic source, the profile and the pinned file/range, which decide which
// candidates the page is drawn from exactly as a filter does. The final
// component is the ordering, which Resolve fixes as the tier keyset.
func symbolQueryHash(req model.SymbolRequest) string {
	located := string(req.FileID)
	if req.Range != nil {
		located += filterSep + strconv.FormatUint(req.Range.Start.Byte, 10) +
			filterSep + strconv.FormatUint(req.Range.End.Byte, 10)
	}
	return model.H(queryHashDomain, endpointSymbol, normalizeQuery(req.Query),
		sortedJoin([]string{string(req.Operation), string(req.SemanticSource)}),
		req.Profile, located, "tier_keyset")
}

// normalizeQuery is the normalization the hash is taken over. It is
// whitespace-only on purpose: case and Unicode folding belong to
// Store.Tokenize, which is the tokenizer search_fts itself uses, and a second
// folding here would make two queries that retrieve different documents share
// one cursor.
func normalizeQuery(q string) string { return strings.Join(strings.Fields(q), " ") }

// sortedJoin renders one filter group: sorted, so argument order does not
// matter, and separated, so the group is unambiguous.
func sortedJoin(values []string) string {
	if len(values) == 0 {
		return ""
	}
	out := slices.Clone(values)
	slices.Sort(out)
	return strings.Join(out, filterSep)
}

// newCursor mints the continuation for one request from the pinned reader's
// own binding and lease. Nothing here is taken from the client: a cursor
// describes the generation the answer was actually read from.
func newCursor(endpoint string, b model.Binding, leaseID, queryHash string, expiresAt time.Time) pagination.Cursor {
	return pagination.Cursor{
		Endpoint:     endpoint,
		GenerationID: b.GenerationID,
		AnalysisKey:  b.AnalysisKey,
		QueryHash:    queryHash,
		LeaseID:      leaseID,
		ExpiresAt:    expiresAt.UTC(),
	}
}

// decodeCursor verifies a client token for endpoint. Every rejection --
// tampered, expired, foreign endpoint, malformed -- is CTX_CURSOR_INVALID and
// never echoes the token. The generation it names is pinned afterwards by the
// caller; verifyCursor then closes the loop.
func decodeCursor(signer *pagination.Signer, token, endpoint string, now time.Time) (pagination.Cursor, error) {
	if signer == nil {
		return pagination.Cursor{}, &model.Error{Code: model.CodeInternal, Message: "search: no cursor signer is configured"}
	}
	return signer.DecodeCursor(token, endpoint, now)
}

// verifyCursor checks a decoded cursor against the generation that was pinned
// from it and the query it is being replayed with. The signature proves only
// that this installation minted the token; it does not prove the generation
// still carries the same analysis key, nor that the caller resubmitted the
// same query. Both are CTX_CURSOR_INVALID rather than a quietly different
// answer under a continued page number.
func verifyCursor(c pagination.Cursor, b model.Binding, queryHash string) error {
	if c.GenerationID != b.GenerationID || c.AnalysisKey != b.AnalysisKey {
		return &model.Error{Code: model.CodeCursorInvalid,
			Message:     "the cursor was issued against a different generation",
			Remediation: "restart the query; a continuation is never repinned onto another generation"}
	}
	if c.QueryHash != queryHash {
		return &model.Error{Code: model.CodeCursorInvalid,
			Message:     "the cursor was issued for a different query or filter set",
			Remediation: "restart the query, or resubmit it with the filters the cursor was issued for"}
	}
	return nil
}

// spoolMetaMarker discriminates the leading record of a search spool from a
// spooled hit. A model.SearchHit unmarshals into spoolMeta as zero values, so
// without the marker a spool written by an older build would silently lose its
// first hit instead of being refused.
const spoolMetaMarker = "codectx.search.page.v2"

// spooledHit is one record of a search spool: the hit as it will be served,
// and the byte interval its source range is hydrated FROM. The span travels
// with the hit because hydration is deferred to the page that actually serves
// it -- reading every tail hit's source out of the CAS, verifying its block
// hashes and scanning it for line/column positions was the largest single cost
// of a wide first page, paid for hits most callers never ask for. The answer
// is unchanged either way: hydration is a pure function of (file, interval)
// within a pinned generation, so a hit hydrated on page 3 is byte for byte the
// hit page 1 would have written.
//
// The marker above is v2 for exactly this: a spool written by a build that
// stored bare model.SearchHit records would unmarshal here into a zero hit
// with no span, and readSpool refuses such a spool rather than serving it.
type spooledHit struct {
	Hit  model.SearchHit  `json:"hit"`
	Span *model.ByteRange `json:"span,omitempty"`
}

// spoolMeta is the leading record of every search spool: the answer-level
// facts a continuation must report as the first page did. QueryMeta.Truncated
// describes the whole answer, not one page, and a continuation reads its hits
// from the spool rather than from the tiers, so without this record every page
// after the first would report a complete answer (Section 14.3, and the
// max_page_items row of docs/configuration.md). The cursor cannot carry it:
// pagination.Cursor is frozen and its SpoolHeader never reaches the read
// callback.
type spoolMeta struct {
	Spool            string `json:"spool"`
	Truncated        bool   `json:"truncated"`
	TruncationReason string `json:"truncation_reason,omitempty"`
}

// spoolHits writes the answer's metadata and the hits that remain after this
// page into a fresh spool and returns the id the next cursor carries, or ""
// when nothing remains.
//
// Search cannot page by keyset: ranking is global over the candidate set, so a
// keyset scan would have to re-rank the whole corpus to find where page 2
// starts, and an inserted generation would move the boundary. It cannot carry
// the remainder in the token either -- Section 14.3 forbids serializing
// traversal state -- and Cursor.Validate makes LastKey and SpoolID mutually
// exclusive, so a spool cannot carry a read offset alongside its id. Each page
// therefore spools exactly its own remainder and names the new spool: the
// previous spool is left alone until its lease expires, which keeps replaying
// an earlier cursor deterministic and non-destructive.
//
// The superseded spools of a walk therefore accumulate against the shared
// spool budget until their leases expire. That is a ledgered Task 20 residual
// (offset-carrying cursors, once Cursor is unfrozen); what must never happen
// is a silent failure, so exhausting the budget is Spools.reserve's typed
// CTX_RESOURCE_LIMIT and it is returned from here unchanged.
func spoolHits(spools *pagination.Spools, c pagination.Cursor, meta spoolMeta, tail func(func(spooledHit) error) error) (string, error) {
	if tail == nil {
		return "", nil
	}
	if spools == nil {
		return "", &model.Error{Code: model.CodeInternal, Message: "search: no spool store is configured"}
	}
	// The spool's header binds it to the cursor, which must not already name
	// a spool of its own: the header records the id the spool is given here.
	c.SpoolID, c.LastKey = "", ""
	sp, err := spools.Create(c)
	if err != nil {
		return "", err
	}
	// The hits are streamed from the caller rather than taken as a slice: the
	// tail of a wide answer is exactly the thing that must not be held in heap,
	// and building a []any of it here would put it back.
	meta.Spool = spoolMetaMarker
	append1 := func(v any) error {
		record, err := json.Marshal(v)
		if err != nil {
			return &model.Error{Code: model.CodeInternal, Message: "search: spooling a result: " + err.Error()}
		}
		return sp.Append(record)
	}
	if err := append1(meta); err != nil {
		releaseSpool(spools, sp)
		return "", err
	}
	if err := tail(func(h spooledHit) error { return append1(h) }); err != nil {
		releaseSpool(spools, sp)
		return "", err
	}
	if err := sp.Close(); err != nil {
		spools.Release(sp.ID())
		return "", err
	}
	return sp.ID(), nil
}

// consumed ends a continuation that has just been served: the spool it was
// replayed from and the cursor-owned lease it was minted with.
//
// A continuation is used exactly once. Its page has now been built, and every
// further page of this walk hangs off the FRESH spool and lease the answer
// carries, so leaving the consumed pair alive until the 15-minute TTL pins a
// generation against retention for state nothing will read again -- one lease
// and one spool per page of every walk in the process. Presenting the same
// token a second time is CTX_CURSOR_INVALID rather than a replayed page; that
// is the deliberate trade of Section 14.3's residual, and the client's remedy
// is the continuation it was just given.
//
// It is called only after the page validates, so a request that fails late
// leaves the continuation intact for the caller to present again. Neither
// release can fail the answer that is already built: both are reported.
func (s *Service) consumed(ctx context.Context, c pagination.Cursor) {
	if c.SpoolID != "" {
		if err := s.spools.Release(c.SpoolID); err != nil {
			s.log.Warn("a consumed continuation's spool could not be released",
				"component", "search", "error", err.Error())
		}
	}
	s.releaseLease(ctx, c.LeaseID)
}

// releaseSpool abandons a spool whose page never reached the caller, returning
// its bytes to the shared budget. Close comes first: Release accounts by the
// file's size on disk, so releasing an unflushed spool would hand the budget
// back bytes that were never written.
func releaseSpool(spools *pagination.Spools, sp *pagination.Spool) {
	sp.Close()
	spools.Release(sp.ID())
}

// readSpool replays a cursor's spool: its metadata record, at most limit hits
// of the page it is serving, and the NUMBER of hits that remain after them.
// Spools validates the header against the cursor and the lease against now, so
// an expired lease, a released spool or a spool minted for another query is
// CTX_CURSOR_INVALID here rather than a wrong page.
//
// It retains exactly one page. The spooled tail of a wide answer is the thing
// a continuation must not hold in heap -- accumulating it here would make a
// continuation's peak a function of the match count -- so the records past the
// page are counted and dropped, and spoolTail walks them again straight into
// the next spool. The walk is a sequential read of one file, which is what
// tailOf does over the ranked run on the first page.
func readSpool(ctx context.Context, spools *pagination.Spools, c pagination.Cursor, now time.Time, limit int) (spoolMeta, []spooledHit, int, error) {
	if spools == nil {
		return spoolMeta{}, nil, 0, &model.Error{Code: model.CodeInternal, Message: "search: no spool store is configured"}
	}
	var meta spoolMeta
	// The capacity is the page bound, which Service.pageLimit has already
	// clamped to model.MaxPageItems, so this allocation cannot track the
	// answer.
	hits := make([]spooledHit, 0, limit)
	rest := 0
	first := true
	err := spools.Open(ctx, c, now, func(record []byte) error {
		if err := ctx.Err(); err != nil {
			return contextErr(err)
		}
		if first {
			first = false
			if err := json.Unmarshal(record, &meta); err != nil || meta.Spool != spoolMetaMarker {
				// A spool whose leading record is not this build's metadata
				// record is one this build cannot serve a faithful page from:
				// serving it would drop a hit and misreport truncation.
				return &model.Error{Code: model.CodeCursorInvalid,
					Message:     "the cursor names a result page this build cannot replay",
					Remediation: "restart the query"}
			}
			return nil
		}
		if len(hits) == limit {
			// Past the page: counted, not decoded and not kept. The count is
			// what the spool-budget truncation reason names when the tail
			// cannot be written.
			rest++
			return nil
		}
		var h spooledHit
		if err := json.Unmarshal(record, &h); err != nil {
			return &model.Error{Code: model.CodeStorageCorrupt, Message: "a spooled result is not readable"}
		}
		hits = append(hits, h)
		return nil
	})
	if err != nil {
		return spoolMeta{}, nil, 0, err
	}
	return meta, hits, rest, nil
}

// spoolTail streams the hits after the first skip of a cursor's spool into the
// continuation sink, one at a time. It is the continuation-page counterpart of
// tailOf: the source spool is still live here -- the consumed cursor's spool
// and lease are released only after the page validates -- so the remainder is
// copied spool to spool without ever standing in heap.
func spoolTail(ctx context.Context, spools *pagination.Spools, c pagination.Cursor, now time.Time, skip int) func(func(spooledHit) error) error {
	return func(yield func(spooledHit) error) error {
		at := 0
		first := true
		return spools.Open(ctx, c, now, func(record []byte) error {
			if err := ctx.Err(); err != nil {
				return contextErr(err)
			}
			if first {
				// The leading metadata record is not a hit: the new spool
				// writes its own, so it is skipped rather than counted.
				first = false
				return nil
			}
			if at++; at <= skip {
				return nil
			}
			var h spooledHit
			if err := json.Unmarshal(record, &h); err != nil {
				return &model.Error{Code: model.CodeStorageCorrupt, Message: "a spooled result is not readable"}
			}
			return yield(h)
		})
	}
}
