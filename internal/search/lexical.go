package search

// L3 owns this file: generation-local FTS candidate selection, tokenization through Store.Tokenize, and phrase handling from term offsets.

import (
	"context"
	"sort"
	"strconv"
	"strings"

	"github.com/Sawmonabo/codectx/internal/config"
	"github.com/Sawmonabo/codectx/internal/model"
	"github.com/Sawmonabo/codectx/internal/storage/sqlite"
)

// lexicalSource is the pinned-generation storage surface the lexical tier
// reads. *sqlite.PinnedReader satisfies it; every method it names is already
// membership-filtered, which is what makes the statistics below
// generation-local without this file ever writing a visibility clause.
type lexicalSource interface {
	SearchStats(ctx context.Context) (documents, tokens int64, err error)
	DocumentFrequency(ctx context.Context, terms []string) ([]int64, error)
	TermOccurrences(ctx context.Context, term string, after int64, limit int) ([]sqlite.TermOccurrence, error)
	Match(ctx context.Context, expression string, after int64, limit int) ([]int64, error)
	SearchDocuments(ctx context.Context, rowids []int64) ([]sqlite.SearchDocument, error)
}

// lexicalTokenizer is *sqlite.Store's Tokenize: the exact unicode61 tokenizer
// search_fts is built with. Query terms come from here and nowhere else — a
// hand-written splitter or a strings.ToLower would produce terms the index
// does not contain (digest §4).
type lexicalTokenizer interface {
	Tokenize(ctx context.Context, text string) ([]string, error)
}

// lexicalTerm is one scored query unit: a single token, or the ordered token
// sequence of a quoted phrase. A phrase scores from adjacent offsets, never
// from the sum of its tokens (digest §4).
type lexicalTerm struct{ tokens []string }

// phrase reports whether the term must be matched as an adjacent sequence.
func (t lexicalTerm) phrase() bool { return len(t.tokens) > 1 }

// key is the term's identity in the FTS expression and the statistics cache.
// Tokens joined by a space are exactly what FTS5 reads as a phrase inside a
// quoted string, so one spelling serves both.
func (t lexicalTerm) key() string { return strings.Join(t.tokens, " ") }

// lexicalHit is one scored lexical candidate. It carries only what the ranker
// needs: name, path and kind arrive later through SearchDocuments, which is
// what keeps the bounded heap small. Occurrences is the number of matched term
// instances in this document, folded across query terms and indexed columns —
// L4 sums it when deduplication folds several documents of one node.
type lexicalHit struct {
	RowID       int64
	ScoreMicros int64
	Occurrences int64
}

// lexicalOutcome reports a lower-bound answer. QueryMeta.Validate rejects a
// truncated result without a reason, so the reason is authored here, where the
// truncation is actually observed.
type lexicalOutcome struct {
	Truncated bool
	Reason    string
}

// truncatedOffsetsReason is the lower-bound explanation. Bounded well under
// model.MaxReasonBytes.
const truncatedOffsetsReason = "phrase frequencies are a lower bound: a document's term offsets exceeded the per-document offset cap"

// matchPageSize is the candidate window. It is also the hydration batch, so it
// must not exceed model.MaxPageItems, which SearchDocuments requires.
const matchPageSize = model.MaxPageItems

// occurrencePageSize is the posting-list page. It must NOT exceed
// model.MaxPageItems: storage clamps a larger request through pageLimit
// (query.go:283), which would make every full page look short and end the
// stream after one page -- silently scoring tf = 0 for every document past it.
// It still exceeds the five indexed columns by a wide margin, so the
// trailing-document trim below can never empty a page.
const occurrencePageSize = model.MaxPageItems

// lexicalTier scores the lexical_fts tier over one pinned generation. It holds
// no generation state: the pinned reader is passed per request and the only
// retained memory is the byte-capped statistics cache. Safe for concurrent use.
type lexicalTier struct {
	tok      lexicalTokenizer
	stats    *statsCache
	maxTerms config.Limit
}

// newLexicalTier builds the tier. maxTerms is resources.max_query_terms, which
// is a config.Limit: an unlimited (0) value means every term of the query is
// scored. The former 0 -> 1 coercion here silently turned "no bound" into the
// tightest bound in the tree, which is the one reading of 0 the wave forbids.
func newLexicalTier(tok lexicalTokenizer, maxTerms config.Limit, cacheBytes int64) *lexicalTier {
	return &lexicalTier{tok: tok, stats: newStatsCache(cacheBytes), maxTerms: maxTerms}
}

// search streams the lexical candidates for text and calls emit once per
// scored document, in ascending rowid — NOT in ranked order, because ranking
// is global over the candidate set and belongs to the bounded heap. The
// candidate stream is paged by rowid and never materialized whole.
func (l *lexicalTier) search(ctx context.Context, src lexicalSource, key model.AnalysisKey, text string, emit func(lexicalHit) error) (lexicalOutcome, error) {
	var out lexicalOutcome
	terms, err := l.parseQuery(ctx, text)
	if err != nil {
		return out, err
	}
	if len(terms) == 0 {
		return out, nil
	}
	n, tokens, err := src.SearchStats(ctx)
	if err != nil {
		return out, err
	}
	// An empty visible corpus has no avgdl; scoring it would divide by zero.
	if n <= 0 || tokens <= 0 {
		return out, nil
	}
	avgdl := float64(tokens) / float64(n)

	dfs, err := l.documentFrequencies(ctx, src, key, terms)
	if err != nil {
		return out, err
	}
	idfs := make([]float64, len(terms))
	for i := range terms {
		idfs[i] = bm25IDF(n, dfs[i])
	}

	streams := make([]*termStream, len(terms))
	for i, t := range terms {
		streams[i] = newTermStream(src, t)
	}

	expr := encodeFTS(terms)
	after := int64(0)
	for {
		if err := ctx.Err(); err != nil {
			return out, model.Canceled(err)
		}
		rowids, err := src.Match(ctx, expr, after, matchPageSize)
		if err != nil {
			return out, err
		}
		if len(rowids) == 0 {
			return out, nil
		}
		after = rowids[len(rowids)-1]
		docs, err := src.SearchDocuments(ctx, rowids)
		if err != nil {
			return out, err
		}
		lengths := make(map[int64]int64, len(docs))
		for _, d := range docs {
			lengths[d.RowID] = d.TokenCount
		}
		for _, rowid := range rowids {
			dl, ok := lengths[rowid]
			if !ok {
				// SearchDocuments omits rowids that are no longer visible;
				// without dl there is no defined score, so the candidate is
				// dropped rather than scored against a guessed length.
				continue
			}
			score, occ, lower, err := l.score(ctx, streams, idfs, rowid, dl, avgdl)
			if err != nil {
				return out, err
			}
			if lower && !out.Truncated {
				out.Truncated, out.Reason = true, truncatedOffsetsReason
			}
			if occ == 0 {
				continue
			}
			if err := emit(lexicalHit{RowID: rowid, ScoreMicros: quantizeScore(score), Occurrences: occ}); err != nil {
				return out, err
			}
		}
		if len(rowids) < matchPageSize {
			return out, nil
		}
	}
}

// score sums digest §4's per-term contributions for one document.
func (l *lexicalTier) score(ctx context.Context, streams []*termStream, idfs []float64, rowid, dl int64, avgdl float64) (score float64, occ int64, lower bool, err error) {
	for i, s := range streams {
		wtf, n, truncated, err := s.at(ctx, rowid)
		if err != nil {
			return 0, 0, false, err
		}
		lower = lower || truncated
		if wtf <= 0 {
			continue
		}
		score += idfs[i] * bm25Saturation(wtf, dl, avgdl)
		occ += n
	}
	return score, occ, lower, nil
}

// parseQuery turns user text into tokenizer-normalized terms. Quoted
// substrings become phrase terms; everything else is tokenized into single
// terms. Duplicates are folded, first occurrence keeping its position, so a
// term cannot be counted twice in the score.
func (l *lexicalTier) parseQuery(ctx context.Context, text string) ([]lexicalTerm, error) {
	var terms []lexicalTerm
	seen := make(map[string]bool)
	add := func(t lexicalTerm) {
		k := t.key()
		if k == "" || seen[k] {
			return
		}
		seen[k] = true
		terms = append(terms, t)
	}
	for _, seg := range splitQuoted(text) {
		tokens, err := l.tok.Tokenize(ctx, seg.text)
		if err != nil {
			return nil, err
		}
		if seg.quoted {
			add(lexicalTerm{tokens: tokens})
			continue
		}
		for _, one := range tokens {
			add(lexicalTerm{tokens: []string{one}})
		}
	}
	// resources.max_query_terms (Section 20.1) is enforced here, the first and
	// only place in the tree that sees the term count. It rejects rather than
	// silently dropping terms, matching Tokenize's rejection of
	// max_query_text_bytes: a query that scores only the first 32 of its terms
	// would return confidently wrong rankings.
	if l.maxTerms.Exceeded(int64(len(terms))) {
		return nil, &model.Error{Code: model.CodeArgumentInvalid,
			Message: "the query carries " + strconv.Itoa(len(terms)) +
				" terms, more than the resources.max_query_terms limit of " + l.maxTerms.String(),
			Remediation: "narrow the query, or raise resources.max_query_terms (0 or \"unlimited\" scores every term)"}
	}
	return terms, nil
}

// segment is one run of query text and whether the user quoted it.
type segment struct {
	text   string
	quoted bool
}

// splitQuoted splits text on double quotes. An unterminated quote closes at
// end of text rather than failing: the quote character is a phrase delimiter
// here, not syntax the user must balance, and no byte of text ever reaches the
// FTS parser unquoted either way.
func splitQuoted(text string) []segment {
	var segs []segment
	quoted := false
	for {
		i := strings.IndexByte(text, '"')
		if i < 0 {
			if text != "" {
				segs = append(segs, segment{text: text, quoted: quoted})
			}
			return segs
		}
		if i > 0 {
			segs = append(segs, segment{text: text[:i], quoted: quoted})
		}
		text, quoted = text[i+1:], !quoted
	}
}

// encodeFTS renders terms as an FTS5 expression in which every user byte sits
// inside a double-quoted string: FTS5 reads a quoted string as literal text,
// so `foo(bar)` matches as text instead of parsing as a column filter or a
// NEAR clause. Terms are joined by OR because a document matching any term is
// a candidate; BM25F, not the match operator, decides how much each term is
// worth.
func encodeFTS(terms []lexicalTerm) string {
	parts := make([]string, 0, len(terms))
	for _, t := range terms {
		parts = append(parts, ftsQuote(t.key()))
	}
	return strings.Join(parts, " OR ")
}

// ftsQuote wraps s as an FTS5 string literal, doubling any embedded quote.
// Tokenize never emits a quote (unicode61 treats it as a separator), so the
// doubling guards the invariant rather than a known input — which is exactly
// why it is unconditional.
func ftsQuote(s string) string {
	return `"` + strings.ReplaceAll(s, `"`, `""`) + `"`
}

// documentFrequencies resolves df for every term, serving the statistics cache
// first, batching the remaining single tokens into one DocumentFrequency call,
// and streaming each phrase's own document frequency.
func (l *lexicalTier) documentFrequencies(ctx context.Context, src lexicalSource, key model.AnalysisKey, terms []lexicalTerm) ([]int64, error) {
	dfs := make([]int64, len(terms))
	var pending []string
	var pendingAt []int
	for i, t := range terms {
		k := statsKey{Key: key, Term: t.key()}
		if df, ok := l.stats.get(k); ok {
			dfs[i] = df
			continue
		}
		if t.phrase() {
			df, err := phraseDocumentFrequency(ctx, src, t)
			if err != nil {
				return nil, err
			}
			dfs[i] = df
			l.stats.put(k, df)
			continue
		}
		pending = append(pending, t.tokens[0])
		pendingAt = append(pendingAt, i)
	}
	if len(pending) == 0 {
		return dfs, nil
	}
	got, err := src.DocumentFrequency(ctx, pending)
	if err != nil {
		return nil, err
	}
	if len(got) != len(pending) {
		return nil, &model.Error{Code: model.CodeInternal,
			Message: "storage returned a document-frequency row count that does not match the terms asked for"}
	}
	for j, at := range pendingAt {
		dfs[at] = got[j]
		l.stats.put(statsKey{Key: key, Term: terms[at].key()}, got[j])
	}
	return dfs, nil
}

// phraseDocumentFrequency counts the visible documents whose offsets actually
// contain the phrase — digest §4 forbids reusing a token's df for a phrase.
// The phrase can only occur where its first token occurs, so that token's
// posting list drives the scan; the cost is O(df of the rarest-bound token),
// which is why the answer is cached per (AnalysisKey, phrase).
func phraseDocumentFrequency(ctx context.Context, src lexicalSource, t lexicalTerm) (int64, error) {
	driver := &occurrenceCursor{src: src, term: t.tokens[0]}
	stream := newTermStream(src, t)
	var df, last int64
	first := true
	for {
		row, ok, err := driver.next(ctx)
		if err != nil {
			return 0, err
		}
		if !ok {
			return df, nil
		}
		if !first && row.RowID == last {
			continue
		}
		first, last = false, row.RowID
		wtf, _, _, err := stream.at(ctx, row.RowID)
		if err != nil {
			return 0, err
		}
		if wtf > 0 {
			df++
		}
	}
}

// occurrenceCursor streams one term's posting rows in ascending
// (rowid, column), holding at most one page. TermOccurrences keys on rowid
// while emitting one row per column, so a page that ends inside a document's
// columns would lose the rest of them; the trailing partial document is
// therefore dropped and re-read from the next page.
type occurrenceCursor struct {
	src   lexicalSource
	term  string
	buf   []sqlite.TermOccurrence
	i     int
	after int64
	done  bool
}

// next yields the next posting row, or ok=false once the stream is exhausted.
func (c *occurrenceCursor) next(ctx context.Context) (sqlite.TermOccurrence, bool, error) {
	for c.i >= len(c.buf) {
		if c.done {
			return sqlite.TermOccurrence{}, false, nil
		}
		rows, err := c.src.TermOccurrences(ctx, c.term, c.after, occurrencePageSize)
		if err != nil {
			return sqlite.TermOccurrence{}, false, err
		}
		if len(rows) < occurrencePageSize {
			c.done = true
		} else {
			// Drop the trailing document, whose columns may continue on the
			// next page, and rewind the keyset to just before it.
			tail := rows[len(rows)-1].RowID
			cut := len(rows)
			for cut > 0 && rows[cut-1].RowID == tail {
				cut--
			}
			// A single document cannot fill a page (five indexed columns), so
			// cut > 0 always holds; keeping the page whole if it ever did not
			// is what prevents an empty page from stalling the scan forever.
			if cut > 0 {
				rows = rows[:cut]
			}
		}
		if len(rows) == 0 {
			c.done = true
			return sqlite.TermOccurrence{}, false, nil
		}
		c.buf, c.i, c.after = rows, 0, rows[len(rows)-1].RowID
	}
	row := c.buf[c.i]
	c.i++
	return row, true, nil
}

// termStream yields one term's weighted frequency per document, walking its
// posting lists in lockstep with the candidate scan. One cursor per token: a
// phrase needs every token's offsets in the same column to count adjacency.
type termStream struct {
	term    lexicalTerm
	cursors []*occurrenceCursor
	head    []sqlite.TermOccurrence
	loaded  []bool
	live    []bool
}

// newTermStream opens one posting cursor per token of t.
func newTermStream(src lexicalSource, t lexicalTerm) *termStream {
	s := &termStream{term: t,
		cursors: make([]*occurrenceCursor, len(t.tokens)),
		head:    make([]sqlite.TermOccurrence, len(t.tokens)),
		loaded:  make([]bool, len(t.tokens)),
		live:    make([]bool, len(t.tokens)),
	}
	for i, tok := range t.tokens {
		s.cursors[i] = &occurrenceCursor{src: src, term: tok}
		s.live[i] = true
	}
	return s
}

// at returns the column-weighted frequency, the raw occurrence count and
// whether the answer is a lower bound, for one document. rowid must not
// decrease across calls: the posting lists are forward-only streams, which is
// what keeps the whole scan linear in the postings rather than quadratic.
func (s *termStream) at(ctx context.Context, rowid int64) (wtf float64, occ int64, lower bool, err error) {
	groups := make([][]sqlite.TermOccurrence, len(s.cursors))
	for i := range s.cursors {
		groups[i], err = s.collect(ctx, i, rowid)
		if err != nil {
			return 0, 0, false, err
		}
		// A phrase needs every token present; a missing one ends the work.
		if s.term.phrase() && len(groups[i]) == 0 {
			return 0, 0, false, nil
		}
	}
	if !s.term.phrase() {
		for _, row := range groups[0] {
			wtf += weightOf(row.Column) * float64(row.Count)
			occ += row.Count
		}
		return wtf, occ, false, nil
	}
	for _, column := range phraseColumns(groups) {
		count, truncated := phraseCount(groups, column)
		lower = lower || truncated
		if count == 0 {
			continue
		}
		wtf += weightOf(column) * float64(count)
		occ += count
	}
	return wtf, occ, lower, nil
}

// collect advances cursor i to rowid and returns that document's rows, holding
// the first row beyond it as lookahead.
func (s *termStream) collect(ctx context.Context, i int, rowid int64) ([]sqlite.TermOccurrence, error) {
	var out []sqlite.TermOccurrence
	for s.live[i] {
		if !s.loaded[i] {
			row, ok, err := s.cursors[i].next(ctx)
			if err != nil {
				return nil, err
			}
			if !ok {
				s.live[i] = false
				break
			}
			s.head[i], s.loaded[i] = row, true
		}
		switch {
		case s.head[i].RowID < rowid:
			s.loaded[i] = false
		case s.head[i].RowID == rowid:
			out = append(out, s.head[i])
			s.loaded[i] = false
		default:
			return out, nil
		}
	}
	return out, nil
}

// phraseColumns is the set of columns in which every token of the phrase
// appears, in a deterministic (lexicographic) column order so the float
// summation order is fixed across runs.
func phraseColumns(groups [][]sqlite.TermOccurrence) []sqlite.SearchColumn {
	counts := make(map[sqlite.SearchColumn]int)
	for _, group := range groups {
		for _, row := range group {
			counts[row.Column]++
		}
	}
	var out []sqlite.SearchColumn
	for column, n := range counts {
		if n == len(groups) {
			out = append(out, column)
		}
	}
	sort.Slice(out, func(a, b int) bool { return out[a] < out[b] })
	return out
}

// phraseCount counts the phrase's occurrences in one column: the positions at
// which token j sits exactly j offsets after token 0. Offsets ascend, so
// membership is a binary search. A truncated offset list makes the answer a
// lower bound (digest §4), which the second return reports rather than hiding.
func phraseCount(groups [][]sqlite.TermOccurrence, column sqlite.SearchColumn) (int64, bool) {
	offsets := make([][]int64, len(groups))
	truncated := false
	for i, group := range groups {
		for _, row := range group {
			if row.Column != column {
				continue
			}
			offsets[i] = row.Offsets
			truncated = truncated || row.Truncated
		}
		if len(offsets[i]) == 0 {
			return 0, truncated
		}
	}
	var count int64
	for _, start := range offsets[0] {
		ok := true
		for j := 1; j < len(offsets) && ok; j++ {
			ok = containsOffset(offsets[j], start+int64(j))
		}
		if ok {
			count++
		}
	}
	return count, truncated
}

// containsOffset reports whether the ascending offsets contain want.
func containsOffset(offsets []int64, want int64) bool {
	i := sort.Search(len(offsets), func(i int) bool { return offsets[i] >= want })
	return i < len(offsets) && offsets[i] == want
}
