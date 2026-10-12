package search

import (
	"context"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Sawmonabo/codectx/internal/model"
	"github.com/Sawmonabo/codectx/internal/pagination"
	"github.com/Sawmonabo/codectx/internal/search/textindex"
	"github.com/Sawmonabo/codectx/internal/storage/sqlite"
)

// The two Section 14.4 endpoint names. A cursor minted by one is rejected by
// the other: its sort tuple and query hash mean nothing across endpoints, and
// honouring it would silently repin the request.
const (
	endpointSearch = "search"
	endpointSymbol = "symbol"
)

// queryHashDomain separates the cursor preimage from every other use of
// model.H.
const queryHashDomain = "codectx.search.v1"

// filterSep joins the values of one filter group. It cannot occur inside a
// path, a language or a node kind, so "a\x00b" and "ab" hash differently.
const filterSep = "\x00"

// lengthRuleEnv names the benchmark harness's switch between the two document
// lengths BM25F can normalize by. It is not a setting: it exists only until the
// quality benchmark fixes one rule, and nothing documents it.
const lengthRuleEnv = "CODECTX_BENCH_LENGTH_RULE"

// lengthRule is the length rule every search of this process scores under,
// read from lengthRuleEnv once: "weighted" selects the per-column weighted
// length, anything else the total token count.
var lengthRule = sync.OnceValue(func() textindex.LengthRule {
	if os.Getenv(lengthRuleEnv) == "weighted" {
		return textindex.LengthWeighted
	}
	return textindex.LengthTotal
})

// lengthRuleName spells a length rule for the cursor preimage.
func lengthRuleName(r textindex.LengthRule) string {
	if r == textindex.LengthWeighted {
		return "weighted"
	}
	return "total"
}

// searchQueryHash is the cursor query-hash preimage for the search endpoint: the
// endpoint, the normalized query text, every filter, the minimum score and the
// length rule the lexical tier scores under. Each filter group is sorted so the
// caller's argument order cannot mint two cursors for one query, and joined with
// a separator no value can contain. A changed filter, a different minimum score
// or another length rule changes which results exist and in what order, so a
// cursor minted under one never resumes under another.
func searchQueryHash(req model.SearchRequest, rule textindex.LengthRule) string {
	kinds := make([]string, len(req.Kinds))
	for i, k := range req.Kinds {
		kinds[i] = string(k)
	}
	return model.H(queryHashDomain, endpointSearch, normalizeQuery(req.Query),
		sortedJoin(kinds), sortedJoin(req.Languages), sortedJoin(req.Paths),
		strconv.FormatInt(req.MinScoreMicros, 10), lengthRuleName(rule))
}

// searchPhase is the part of a search answer a continuation resumes in.
type searchPhase uint8

const (
	// phaseStart is a first page: nothing has been served.
	phaseStart searchPhase = iota
	// phaseExact resumes inside the exact tiers, after one exact hit.
	phaseExact
	// phaseLexical resumes in the lexical tier, after a count of its results.
	phaseLexical
)

// searchPosition is where a search page begins. A position in the exact tiers
// names the last exact hit served by its tier and node: within one tier a node
// is emitted once, so the pair identifies the hit, and the next page reads that
// hit's full content key back from the same tier (positionKey) rather than
// carrying it -- a path, a qualified name and a signature together can be far
// larger than a cursor's sort key may be. A position in the lexical tier is the
// number of lexical results already served after the service's own filters:
// the library's ranking is a pure function of the generation and the query, so
// the same count names the same place on every page.
type searchPosition struct {
	phase  searchPhase
	rank   int
	node   model.NodeID
	served int
}

// The two spellings of a position in Cursor.LastKey.
const (
	exactKeyTag   = "x"
	lexicalKeyTag = "l"
)

// lastKey renders a continuation position for Cursor.LastKey.
func (p searchPosition) lastKey() string {
	if p.phase == phaseLexical {
		return lexicalKeyTag + filterSep + strconv.Itoa(p.served)
	}
	return exactKeyTag + filterSep + strconv.Itoa(p.rank) + filterSep + string(p.node)
}

// parseSearchPosition reads back what lastKey wrote. A key this endpoint did not
// write -- empty, an unknown tag, a rank no exact tier owns, a malformed node,
// a negative count -- is CTX_CURSOR_INVALID rather than a silently restarted
// answer.
func parseSearchPosition(key string) (searchPosition, error) {
	tag, rest, ok := strings.Cut(key, filterSep)
	if !ok {
		return searchPosition{}, cursorInvalid("the search cursor does not carry a position")
	}
	switch tag {
	case lexicalKeyTag:
		served, err := strconv.Atoi(rest)
		if err != nil || served < 0 {
			return searchPosition{}, cursorInvalid("the search cursor carries a malformed lexical position")
		}
		return searchPosition{phase: phaseLexical, served: served}, nil
	case exactKeyTag:
		head, node, ok := strings.Cut(rest, filterSep)
		rank, err := strconv.Atoi(head)
		if !ok || err != nil || rank < model.TierExactPath.Rank() || rank > model.TierExactName.Rank() ||
			!model.ValidHexID(node) {
			return searchPosition{}, cursorInvalid("the search cursor carries a malformed exact-tier position")
		}
		return searchPosition{phase: phaseExact, rank: rank, node: model.NodeID(node)}, nil
	}
	return searchPosition{}, cursorInvalid("the search cursor does not carry a search position")
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
// whitespace-only on purpose: case and Unicode folding belong to the search
// index's own tokenizer, and a second folding here would make two queries that
// retrieve different documents share one cursor.
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

// cursorInvalid is the one rejection class every continuation failure of this
// package carries. It never echoes the token.
func cursorInvalid(msg string) *model.Error {
	return &model.Error{Code: model.CodeCursorInvalid, Message: msg,
		Remediation: "restart the query; a continuation is never repinned onto another generation"}
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
