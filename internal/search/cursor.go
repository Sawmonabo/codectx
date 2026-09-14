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

// resolveKey is the digest §5 keyset continuation for Resolve: the tier
// ordinal and the node id of the last row served. It fits the 1024-byte
// Cursor.LastKey bound, which is why Resolve needs no spool.
func resolveKey(tier model.SearchTier, node model.NodeID) string {
	return strconv.Itoa(tier.Rank()) + "\x00" + string(node)
}

// parseResolveKey reads a continuation key back. A malformed key is a tampered
// or foreign cursor, not an internal defect.
func parseResolveKey(key string) (rank int, node model.NodeID, err error) {
	sep := strings.IndexByte(key, 0)
	if sep < 0 {
		return 0, "", &model.Error{Code: model.CodeCursorInvalid, Message: "the cursor's sort key is malformed"}
	}
	rank, perr := strconv.Atoi(key[:sep])
	if perr != nil || rank < 0 {
		return 0, "", &model.Error{Code: model.CodeCursorInvalid, Message: "the cursor's sort key is malformed"}
	}
	node = model.NodeID(key[sep+1:])
	if node != "" && !model.ValidHexID(string(node)) {
		return 0, "", &model.Error{Code: model.CodeCursorInvalid, Message: "the cursor's sort key is malformed"}
	}
	return rank, node, nil
}

// spoolHits writes the hits that remain after this page into a fresh spool and
// returns the id the next cursor carries, or "" when nothing remains.
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
func spoolHits(spools *pagination.Spools, c pagination.Cursor, hits []model.SearchHit) (string, error) {
	if len(hits) == 0 {
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
	for _, h := range hits {
		record, err := json.Marshal(h)
		if err != nil {
			releaseSpool(spools, sp)
			return "", &model.Error{Code: model.CodeInternal, Message: "search: spooling a result: " + err.Error()}
		}
		if err := sp.Append(record); err != nil {
			releaseSpool(spools, sp)
			return "", err
		}
	}
	if err := sp.Close(); err != nil {
		spools.Release(sp.ID())
		return "", err
	}
	return sp.ID(), nil
}

// releaseSpool abandons a spool whose page never reached the caller, returning
// its bytes to the shared budget. Close comes first: Release accounts by the
// file's size on disk, so releasing an unflushed spool would hand the budget
// back bytes that were never written.
func releaseSpool(spools *pagination.Spools, sp *pagination.Spool) {
	sp.Close()
	spools.Release(sp.ID())
}

// readSpool replays the hits a cursor's spool holds, in ranked order. Spools
// validates the header against the cursor and the lease against now, so an
// expired lease, a released spool or a spool minted for another query is
// CTX_CURSOR_INVALID here rather than a wrong page.
func readSpool(ctx context.Context, spools *pagination.Spools, c pagination.Cursor, now time.Time) ([]model.SearchHit, error) {
	if spools == nil {
		return nil, &model.Error{Code: model.CodeInternal, Message: "search: no spool store is configured"}
	}
	var hits []model.SearchHit
	err := spools.Open(ctx, c, now, func(record []byte) error {
		if err := ctx.Err(); err != nil {
			return contextErr(err)
		}
		var h model.SearchHit
		if err := json.Unmarshal(record, &h); err != nil {
			return &model.Error{Code: model.CodeStorageCorrupt, Message: "a spooled result is not readable"}
		}
		hits = append(hits, h)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return hits, nil
}
