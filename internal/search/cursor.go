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
	"github.com/Sawmonabo/codectx/internal/storage/sqlite"
)

// The two Section 14.4 endpoint names. A cursor minted by one is rejected by
// the other: its sort tuple and query hash mean nothing across endpoints, and
// honouring it would silently repin the request.
const (
	endpointSearch = "search"
	endpointSymbol = "symbol"
)

// queryHashDomain versions the cursor preimage. Changing the preimage without
// changing this would let a cursor minted under one preimage resume under
// another against a differently-filtered candidate set.
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

// searchCursorVersion versions the private continuation payload below. A token
// minted by a build that spelled the payload differently carries another
// version and is refused rather than read with today's field meanings.
const searchCursorVersion = 1

// searchCursor is the search endpoint's continuation payload.
//
// It is deliberately NOT a pagination.Cursor. That shape carries either a
// keyset sort tuple or a spool id, refuses the two together, and has no offset
// field at all. A search answer is ONE globally ordered candidate list, and
// ADR-0007 Decision 3 serves every page after the first out of one spool by
// byte offset -- so the position it has to carry is exactly the one the shared
// shape cannot hold. The Resolve endpoint keeps the shared shape: its pages are
// a keyset walk with no spool.
//
// It is signed with pagination.Signer under PurposeCursor like every other
// continuation. validate() restates the refusals pagination.Signer.DecodeCursor
// makes for the shared shape, INCLUDING the endpoint comparison: Verify checks
// the signature and the expiry and nothing else, so without that line a
// traversal's token would be readable here.
type searchCursor struct {
	Version      int                `json:"version"`
	Endpoint     string             `json:"endpoint"`
	GenerationID model.GenerationID `json:"generation_id"`
	AnalysisKey  model.AnalysisKey  `json:"analysis_key"`
	QueryHash    string             `json:"query_hash"`
	LeaseID      string             `json:"lease_id"`
	// SpoolID names the spool holding this answer's candidates and Offset the
	// byte where the next page's first record begins. A byte offset rather than
	// a record index because that is what pagination.Spools.OpenAt seeks to:
	// counting records to find the position would make a page cost the
	// remainder behind it.
	SpoolID string `json:"spool_id"`
	Offset  int64  `json:"offset"`
	// Ordered says which of the two spool shapes SpoolID names. The first page
	// writes the candidates RAW, in the order the deduplication pass happened
	// to present them, because ordering them is the cost Decision 3 defers; the
	// first continuation sorts them once and writes the ordered tail, which
	// every later page reads by offset. Offset is meaningless until then, so a
	// raw cursor must carry none.
	Ordered bool `json:"ordered"`
	// Served and Total are candidate counts over the whole answer, so a further
	// page exists exactly while Served < Total.
	Served int64 `json:"served"`
	Total  int64 `json:"total"`
	// Truncated and TruncationReason are the ANSWER-level facts the first page
	// computed. A continuation reads its hits from the spool, never from the
	// tiers, so it has nothing of its own to compute them from and must carry
	// them forward or report a complete answer for an answer that is not.
	Truncated        bool      `json:"truncated,omitempty"`
	TruncationReason string    `json:"truncation_reason,omitempty"`
	ExpiresAt        time.Time `json:"expires_at"`
}

// validate refuses a payload that does not describe a search continuation.
// Every branch is a rejection a tampered or foreign token would otherwise walk
// through: the version, the endpoint, the pinned generation, the identifiers
// the spool store checks the file against, and the position itself.
func (c searchCursor) validate() error {
	switch {
	case c.Version != searchCursorVersion:
		return cursorInvalid("cursor version is not supported")
	case c.Endpoint != endpointSearch:
		return cursorInvalid("cursor was issued by a different endpoint")
	case c.GenerationID <= 0:
		return cursorInvalid("cursor does not pin a generation")
	case !model.ValidHexID(c.QueryHash):
		return cursorInvalid("cursor query hash must be a well-formed identifier")
	case c.LeaseID != "" && !model.ValidHexID(c.LeaseID):
		// A continuation minted by a process that records no lease names none:
		// the spool it names is bound to this cursor's expiry instead, which is
		// the predicate pagination.Spools.live applies to a leaseless entry.
		return cursorInvalid("cursor lease id is malformed")
	case !model.ValidHexID(string(c.AnalysisKey)):
		return cursorInvalid("cursor does not name an analysis key")
	case !model.ValidHexID(c.SpoolID):
		return cursorInvalid("a search continuation names no result spool")
	case c.Offset < 0 || c.Served < 0:
		return cursorInvalid("cursor carries a negative position")
	case c.Served >= c.Total:
		return cursorInvalid("cursor carries a position past the end of its answer")
	case !c.Ordered && c.Offset != 0:
		// A raw spool has no order, so no byte offset into it names a page.
		return cursorInvalid("cursor carries an offset into an unordered result spool")
	case c.Truncated && c.TruncationReason == "":
		return cursorInvalid("cursor reports a truncated answer with no reason")
	case c.ExpiresAt.IsZero():
		return cursorInvalid("cursor has no expiry")
	}
	return nil
}

// spoolCursor is the binding the spool store checks the file's header against.
func (c searchCursor) spoolCursor() pagination.Cursor {
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

// cursorInvalid is the one rejection class every continuation failure of this
// endpoint carries. It never echoes the token.
func cursorInvalid(msg string) *model.Error {
	return &model.Error{Code: model.CodeCursorInvalid, Message: msg,
		Remediation: "restart the query; a continuation is never repinned onto another generation"}
}

// newSearchCursor mints the continuation of one search answer from the pinned
// reader's own binding and the lease that retains it. Nothing here is taken
// from the client: a cursor describes the generation the answer was actually
// read from.
func (s *Service) newSearchCursor(b model.Binding, hash, leaseID string, now time.Time) searchCursor {
	return searchCursor{
		Version: searchCursorVersion, Endpoint: endpointSearch,
		GenerationID: b.GenerationID, AnalysisKey: b.AnalysisKey, QueryHash: hash,
		LeaseID: leaseID, ExpiresAt: now.Add(s.ttl).UTC().Truncate(time.Second),
	}
}

// signSearchCursor validates and signs a continuation. It does not release the
// spool or the lease on failure: the page that created them is the only one
// that may take them back, and it does.
func (s *Service) signSearchCursor(c searchCursor) (string, error) {
	if err := c.validate(); err != nil {
		return "", err
	}
	payload, err := json.Marshal(c)
	if err != nil {
		return "", &model.Error{Code: model.CodeInternal, Message: "search: cursor encoding: " + err.Error()}
	}
	return s.signer.Sign(pagination.PurposeCursor, payload, c.ExpiresAt)
}

// resumeSearch turns a presented token into the position to resume from. An
// unverified continuation is a request to read from a position nothing
// vouched for, so a missing signer is a rejection rather than a trusted token.
func (s *Service) resumeSearch(token, hash string, now time.Time) (searchCursor, error) {
	var c searchCursor
	if s.signer == nil {
		return c, &model.Error{Code: model.CodeInternal, Message: "search: no cursor signer is configured"}
	}
	payload, err := s.signer.Verify(token, pagination.PurposeCursor, now)
	if err != nil {
		return c, err
	}
	if err := json.Unmarshal(payload, &c); err != nil {
		return c, cursorInvalid("cursor payload is malformed")
	}
	if err := c.validate(); err != nil {
		return c, err
	}
	// The signature proves this installation minted the token; it does not
	// prove the caller resubmitted the query it was minted for. A changed
	// filter resuming an old spool would page through a different answer under
	// a continued page number.
	if c.QueryHash != hash {
		return c, cursorInvalid("the cursor was issued for a different query or filter set")
	}
	return c, nil
}

// checkSearchCursor rejects a verified cursor against the generation that was
// pinned from it. The generation may have been re-analysed under the same id,
// and resuming over facts the first page never saw silently skips hits.
func checkSearchCursor(c searchCursor, b model.Binding) error {
	if c.GenerationID != b.GenerationID || c.AnalysisKey != b.AnalysisKey {
		return cursorInvalid("the cursor was issued against a different generation")
	}
	return nil
}

// spoolBinding is the pagination.Cursor a spool is CREATED under: the same
// binding the continuation carries, with no spool id, because Create mints the
// id it writes into the header.
func (c searchCursor) spoolBinding() pagination.Cursor {
	sc := c.spoolCursor()
	sc.SpoolID = ""
	return sc
}

// consumed ends a keyset continuation that has just been served: the
// cursor-owned lease it was minted with. A continuation is used exactly once,
// and every further page of the walk hangs off the fresh lease the answer
// carries, so leaving it alive until the TTL pins a generation against
// retention for state nothing will read again.
//
// It is called only after the page validates, so a request that fails late
// leaves the continuation intact for the caller to present again.
func (s *Service) consumed(ctx context.Context, c pagination.Cursor) {
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

// resumePinFailure types the failure a CONTINUATION meets when the generation
// its token pins has been collected between the page that minted it and the
// page presenting it. Both endpoints here pin the generation the TOKEN names,
// not the active one, so this is the across-call end of the snapshot a
// writerless pin holds: within a call nothing can be collected under the read,
// across calls the generation can go, and the caller must be told which it is.
//
// The store refuses a generation with no row as an invalid ARGUMENT, which is
// the right answer for an operator who typed --generation and the wrong one for
// a caller that presented a token: nothing about its argument is malformed, the
// position it names is simply gone, and what it needs to be told is to start
// again from the first page.
func resumePinFailure(resumed bool, err error) error {
	if !resumed || !sqlite.IsGenerationCollected(err) {
		return err
	}
	return cursorInvalid("the generation this cursor pins has been collected; re-run from the first page")
}
