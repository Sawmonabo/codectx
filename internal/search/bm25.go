package search

import (
	"math"
	"sync"

	"github.com/Sawmonabo/codectx/internal/model"
	"github.com/Sawmonabo/codectx/internal/storage/sqlite"
)

// columnWeight is the BM25F per-column weighting of digest §4: a name or
// qualified-name match outweighs a signature or path match, which outweighs a
// body match.
var columnWeight = map[sqlite.SearchColumn]float64{
	sqlite.ColumnName:          5,
	sqlite.ColumnQualifiedName: 5,
	sqlite.ColumnSignature:     2,
	sqlite.ColumnPath:          2,
	sqlite.ColumnBody:          1,
}

// bm25K1 and bm25B are the term-saturation and length-normalization constants
// of digest §4.
const bm25K1, bm25B = 1.2, 0.75

// weightOf is columnWeight with a closed vocabulary: a column spelling this
// build does not know contributes nothing rather than defaulting to 1, so a
// future indexed column cannot silently join the score at body weight.
func weightOf(c sqlite.SearchColumn) float64 { return columnWeight[c] }

// bm25IDF is digest §4's inverse document frequency,
// ln(1 + (N − df + 0.5) / (df + 0.5)), over the visible document count only.
// The +0.5 smoothing keeps it strictly positive even at df == N, so no floor
// or clamp is applied: a clamp here would be a deviation from the formula, not
// a safety net.
func bm25IDF(n, df int64) float64 {
	if n <= 0 {
		return 0
	}
	if df > n {
		df = n
	}
	if df < 0 {
		df = 0
	}
	return math.Log(1 + (float64(n)-float64(df)+0.5)/(float64(df)+0.5))
}

// bm25Saturation is the per-term frequency component of digest §4,
// wtf·(k1+1) / (wtf + k1·(1 − b + b·dl/avgdl)). wtf is the column-weighted sum
// Σ_c weight(c)·tf(t,d,c) — the weighted sum enters the saturation denominator
// directly, which is what makes this BM25F rather than five independent BM25
// scores summed.
func bm25Saturation(wtf float64, dl int64, avgdl float64) float64 {
	if wtf <= 0 || avgdl <= 0 {
		return 0
	}
	if dl < 0 {
		dl = 0
	}
	return wtf * (bm25K1 + 1) / (wtf + bm25K1*(1-bm25B+bm25B*float64(dl)/avgdl))
}

// quantizeScore is digest §4's quantization, int64(math.Round(score · 1e6)).
// It is the only float-to-integer conversion in the package: everything after
// it compares int64 and exact strings, which is what makes ranking identical
// on every release target. A non-finite score (an impossible corpus) quantizes
// to 0 rather than to an undefined int64.
//
// L4's ranker calls this; it must not redeclare it.
func quantizeScore(score float64) int64 {
	if math.IsNaN(score) || math.IsInf(score, 0) {
		return 0
	}
	return int64(math.Round(score * 1e6))
}

// statsKey identifies one cached document frequency. Corpus statistics are
// generation-local (digest §4), so the analysis key of the pinned generation is
// part of the identity: a df cached under one generation can never be served
// to another.
type statsKey struct {
	Key  model.AnalysisKey
	Term string
}

// statsEntry is a cached df plus the byte cost charged against the cap.
type statsEntry struct {
	df    int64
	bytes int64
}

// statsEntryOverhead is the per-entry cost charged on top of the key and term
// bytes: the map bucket slot, the two string headers and the two int64s. It is
// an accounting constant, not a measurement — its only job is to keep the cap
// conservative so the cache cannot exceed it in practice.
const statsEntryOverhead = 64

// defaultStatsCacheBytes is the cache's ceiling. A query contributes one entry
// per distinct term it carries, and resources.max_query_terms is unlimited by
// default, so this constant -- not the query -- is what bounds the cache: it
// holds thousands of distinct query terms and cannot grow with the repository.
const defaultStatsCacheBytes = 1 << 20

// statsCache caches per-term document frequencies under an explicit byte cap.
//
// It is deliberately NOT a corpus token dictionary: entries appear only for
// terms a user actually queried, and the cap is a constant rather than a
// function of the repository, so a large repository cannot turn this into a
// resident index. When the cap is reached, entries are dropped until the new
// one fits — Go's randomized map iteration picks the victims. Random eviction
// is correct here because every entry is equally cheap to recompute (one
// DocumentFrequency call) and a recency structure would cost more memory than
// it saves. Safe for concurrent use.
type statsCache struct {
	mu      sync.Mutex
	max     int64
	bytes   int64
	entries map[statsKey]statsEntry
}

// newStatsCache builds a cache holding at most max bytes of entries. A
// non-positive max disables caching entirely rather than meaning "unbounded".
func newStatsCache(max int64) *statsCache {
	return &statsCache{max: max, entries: map[statsKey]statsEntry{}}
}

// get returns the cached document frequency for k.
func (c *statsCache) get(k statsKey) (int64, bool) {
	if c == nil {
		return 0, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.entries[k]
	return e.df, ok
}

// put records df for k, evicting entries until the byte cap admits it. An
// entry larger than the whole cap is not stored at all.
func (c *statsCache) put(k statsKey, df int64) {
	if c == nil {
		return
	}
	size := int64(len(k.Key)+len(k.Term)) + statsEntryOverhead
	c.mu.Lock()
	defer c.mu.Unlock()
	if size > c.max {
		return
	}
	if old, ok := c.entries[k]; ok {
		c.bytes -= old.bytes
		delete(c.entries, k)
	}
	for c.bytes+size > c.max {
		if len(c.entries) == 0 {
			c.bytes = 0
			break
		}
		for victim, e := range c.entries {
			delete(c.entries, victim)
			c.bytes -= e.bytes
			break
		}
	}
	c.entries[k] = statsEntry{df: df, bytes: size}
	c.bytes += size
}
