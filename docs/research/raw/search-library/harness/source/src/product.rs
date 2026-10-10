//! Product mode: our lexical tier's rules on tantivy's index.
//!
//! Extension point: a custom `Collector` whose `collect_segment` is overridden, so
//! it ignores the library Weight/Scorer and walks the segment itself through
//! `InvertedIndexReader::read_postings` (frequencies, and positions for phrases),
//! the segment's alive bitset and fast-field columns. The library's
//! `Bm25StatisticsProvider` is not used: it is keyed per (field, term) and counts
//! deleted documents, while our df is per term across all five columns over live
//! documents only; and the library's `Score` is f32, which cannot carry our f64
//! score to the 1e-6 quantization exactly.
//!
//! Semantics reproduced (internal/search/lexical.go, bm25.go, rank.go):
//! - candidates are the OR of terms; a phrase matches where its tokens sit at
//!   adjacent positions in one column, counted over the first MAX_TERM_OFFSETS
//!   positions of each token per (document, column); a document whose every term
//!   contributes zero is not a candidate;
//! - df is exact over live documents; N and the token total are over live documents;
//! - BM25F with weights 5/5/2/2/1, k1 1.2, b 0.75, Go's math.Log, 1e-6 quantization;
//! - deduplication by node id, else (path, start byte), survivor = comparator minimum;
//! - the deduplicated set ordered by cmpScored; hits is its size; the page its first 200.

use crate::library::HideSet;
use crate::{bm25_idf, bm25_saturation, quantize, Fields, Filter, QTerm, WEIGHTS};
use std::cmp::Ordering;
use std::collections::HashMap;
use tantivy::collector::{Collector, SegmentCollector};
use tantivy::columnar::{Column, StrColumn};
use tantivy::postings::Postings;
use tantivy::query::Weight;
use tantivy::schema::IndexRecordOption;
use tantivy::{DocId, DocSet, Score, Searcher, SegmentReader, Term, TERMINATED};

/// storage/sqlite/search.go MaxTermOffsets.
pub const MAX_TERM_OFFSETS: usize = 512;

/// The page bound (model.MaxPageItems as `codectx search` serves it).
pub const PAGE: usize = 200;

/// Live document count and token total (SearchStats).
#[derive(Clone, Copy, Debug)]
pub struct CorpusStats {
    pub docs: i64,
    pub tokens: i64,
}

pub fn corpus_stats(searcher: &Searcher) -> tantivy::Result<CorpusStats> {
    corpus_stats_of(searcher.segment_readers(), None, DL)
}

/// The document-length column: our unicode61 token count.
pub const DL: &str = "token_count";
/// product-code's document length: the code tokenizer's token count (stacked parts included).
pub const DL_CODE: &str = "token_count_code";
/// `--dl weighted`: Σ weight(c) × len(c) over the five columns (5/5/2/2/1), from
/// the exact per-column counts `tindex --column-lengths` stores (LEN_NAMES).
pub const DL_WEIGHTED: &str = "weighted";
pub const LEN_NAMES: [&str; 5] = ["len_name", "len_qualified_name", "len_signature", "len_path", "len_body"];

/// A segment's document-length reader: one column, or the weighted sum of five.
pub enum DlCol {
    One(Column<u64>),
    Weighted(Vec<Column<u64>>),
}

impl DlCol {
    pub fn open(seg: &SegmentReader, dl: &str) -> tantivy::Result<DlCol> {
        let ff = seg.fast_fields();
        if dl != DL_WEIGHTED {
            return Ok(DlCol::One(ff.u64(dl)?));
        }
        Ok(DlCol::Weighted(LEN_NAMES.iter().map(|n| ff.u64(n)).collect::<tantivy::Result<Vec<_>>>()?))
    }
    pub fn get(&self, d: DocId) -> u64 {
        match self {
            DlCol::One(c) => c.first(d).unwrap_or(0),
            DlCol::Weighted(cs) => cs.iter().zip(WEIGHTS).map(|(c, w)| w as u64 * c.first(d).unwrap_or(0)).sum(),
        }
    }
}

/// N and the token total over the live documents of `segs` that `hide` leaves
/// visible (our statistics count visible documents only); `dl` names the length column.
pub fn corpus_stats_of(segs: &[SegmentReader], hide: Option<&HideSet>, dl: &str) -> tantivy::Result<CorpusStats> {
    let mut s = CorpusStats { docs: 0, tokens: 0 };
    for seg in segs {
        let dl = DlCol::open(seg, dl)?;
        let alive = seg.alive_bitset();
        let vis = hide.and_then(|h| h.segment(seg.segment_id()));
        for d in 0..seg.max_doc() {
            if alive.map_or(true, |a| a.is_alive(d)) && vis.as_ref().map_or(true, |v| v[d as usize]) {
                s.docs += 1;
                s.tokens += dl.get(d) as i64;
            }
        }
    }
    Ok(s)
}

/// The answer: deduplicated hit count, raw candidate count, the first page's keys.
#[derive(Debug)]
pub struct Answer {
    pub hits: u64,
    pub candidates: u64,
    pub page: Vec<String>,
}

pub struct ProductCollector {
    pub terms: Vec<QTerm>,
    pub fields: Fields,
    pub stats: CorpusStats,
    /// Applied to each scored document before the fold, as lexicalCandidates
    /// does; df and the corpus statistics stay unfiltered.
    pub filter: Filter,
    /// Per-unit visibility: hidden documents are not candidates and count in no
    /// statistic, as in our engine (pass the same set to corpus_stats_of).
    pub hide: Option<std::sync::Arc<HideSet>>,
    /// product-block: candidates come from the library's own Weight (the search is
    /// issued with a BooleanQuery of Term/PhraseQuery per field and scoring disabled,
    /// so they arrive through for_each_no_score / collect_block); the postings are
    /// then only sought at those candidates. Same answer, other candidate generator.
    pub via_weight: bool,
    /// The document-length column (DL, or DL_CODE for product-code).
    pub dl: &'static str,
    /// The 1-based page served: the top page_no*PAGE, then that slice.
    pub page_no: usize,
}

/// A string fast column as an order-preserving rank: "" (or absent) is 0, every
/// other value is above it in byte order. Ranks compare only within one segment.
#[derive(Clone)]
pub struct RankCol {
    col: Option<StrColumn>,
    shift: u64,
}

impl RankCol {
    pub fn open(seg: &SegmentReader, name: &str) -> tantivy::Result<RankCol> {
        let col = seg.fast_fields().str(name)?;
        let mut shift = 1;
        if let Some(c) = &col {
            let mut b = Vec::new();
            if c.num_terms() > 0 && c.ord_to_bytes(0, &mut b)? && b.is_empty() {
                shift = 0;
            }
        }
        Ok(RankCol { col, shift })
    }
    fn ord(&self, d: DocId) -> Option<u64> {
        self.col.as_ref().and_then(|c| c.term_ords(d).next())
    }
    fn rank(&self, d: DocId) -> u64 {
        match self.ord(d) {
            None => 0,
            Some(o) => o + self.shift,
        }
    }
}

/// What one segment hands to the merge: per term, the live documents with a
/// positive weighted frequency, plus the columns the merge reads.
pub struct SegFruit {
    max_doc: u32,
    pub per_term: Vec<Vec<(DocId, f64)>>,
    cols: SegCols,
}

/// The columns the fold reads in one segment: length, span, rank columns
/// (path, kind, name, qualified_name, signature, node_id, id) and the filter.
pub struct SegCols {
    pub dl: DlCol,
    start: Column<u64>,
    end: Column<u64>,
    strs: Vec<RankCol>,
    keep: SegFilter,
}

impl SegCols {
    pub fn open(seg: &SegmentReader, dl: &str, filter: &Filter) -> tantivy::Result<SegCols> {
        let ff = seg.fast_fields();
        let strs = STR_COLS.iter().map(|n| RankCol::open(seg, n)).collect::<tantivy::Result<Vec<_>>>()?;
        let keep = SegFilter::build(filter, &strs[C_KIND], &strs[C_PATH])?;
        Ok(SegCols { dl: DlCol::open(seg, dl)?, start: ff.u64("start")?, end: ff.u64("end")?, strs, keep })
    }
    /// The filter's verdict on a document.
    pub fn keep(&self, d: DocId) -> bool {
        self.keep.keep(&self.strs[C_KIND], &self.strs[C_PATH], d)
    }
    /// A document as a segment-local candidate.
    pub fn cand(&self, d: DocId, micros: i64) -> OrdCand {
        OrdCand {
            doc: d,
            micros,
            start: self.start.first(d).unwrap_or(0),
            end: self.end.first(d).unwrap_or(0),
            r: std::array::from_fn(|k| self.strs[k].rank(d)),
        }
    }
    /// Resolves the survivors' strings, one sorted dictionary pass per column.
    pub fn resolve(&self, surv: Vec<OrdCand>) -> tantivy::Result<Vec<Cand>> {
        let mut strings: Vec<Vec<String>> = vec![vec![String::new(); surv.len()]; STR_COLS.len()];
        for (k, rc) in self.strs.iter().enumerate() {
            let Some(col) = &rc.col else { continue };
            let mut ords: Vec<(u64, usize)> = surv.iter().enumerate().filter_map(|(i, c)| rc.ord(c.doc).map(|o| (o, i))).collect();
            ords.sort_unstable();
            let mut uniq: Vec<u64> = ords.iter().map(|x| x.0).collect();
            uniq.dedup();
            let mut vals = Vec::with_capacity(uniq.len());
            col.dictionary()
                .sorted_ords_to_term_cb(uniq.iter().copied(), |b| {
                    vals.push(String::from_utf8_lossy(b).into_owned());
                    Ok(())
                })
                .map_err(tantivy::TantivyError::from)?;
            let mut u = 0;
            for (o, i) in ords {
                while uniq[u] != o {
                    u += 1;
                }
                strings[k][i] = vals[u].clone();
            }
        }
        Ok(surv
            .iter()
            .enumerate()
            .map(|(i, c)| {
                let mut s = |k: usize| std::mem::take(&mut strings[k][i]);
                Cand {
                    micros: c.micros, path: s(0), start: c.start, end: c.end as i64, kind: s(1), name: s(2), qn: s(3),
                    sig: s(4), node: s(5), id: s(6),
                }
            })
            .collect())
    }
}

/// Folds a resolved candidate into the global map (node id, else path + start).
pub fn fold_global(global: &mut HashMap<String, Cand>, cand: Cand) {
    let key = if !cand.node.is_empty() { format!("node\0{}", cand.node) } else { format!("file\0{}\0{}", cand.path, cand.start) };
    match global.get_mut(&key) {
        Some(have) if cmp_scored(&cand, have) == Ordering::Less => *have = cand,
        Some(_) => {}
        None => {
            global.insert(key, cand);
        }
    }
}

/// The served page `page_no` (1-based) of the folded set: the top page_no*PAGE
/// in cmpScored order, then that page's slice.
pub fn page_of(global: HashMap<String, Cand>, page_no: usize) -> Vec<String> {
    let depth = page_no.max(1) * PAGE;
    let mut all: Vec<Cand> = global.into_values().collect();
    if all.len() > depth {
        all.select_nth_unstable_by(depth - 1, cmp_scored);
        all.truncate(depth);
    }
    all.sort_unstable_by(cmp_scored);
    all.into_iter().skip(depth - PAGE).map(|c| c.id).collect()
}

/// The filter as segment ordinals: allowed kind ordinals, and per path prefix
/// its exact ordinal plus the ordinal range of the values under prefix + "/".
pub struct SegFilter {
    on: bool,
    kinds: Option<Vec<u64>>,
    paths: Option<Vec<(u64, u64)>>,
}

impl SegFilter {
    pub fn build(f: &Filter, kind: &RankCol, path: &RankCol) -> tantivy::Result<SegFilter> {
        let mut out = SegFilter { on: !f.is_empty(), kinds: None, paths: None };
        if !f.kinds.is_empty() {
            let mut v = Vec::new();
            if let Some(c) = &kind.col {
                for k in &f.kinds {
                    if let Some(o) = c.dictionary().term_ord(k.as_bytes())? {
                        v.push(o);
                    }
                }
            }
            out.kinds = Some(v);
        }
        if !f.prefixes.is_empty() {
            let mut v = Vec::new();
            if let Some(c) = &path.col {
                for p in &f.prefixes {
                    if let Some(o) = c.dictionary().term_ord(p.as_bytes())? {
                        v.push((o, o));
                    }
                    let under = format!("{p}/");
                    let mut st = c.dictionary().range().ge(under.as_bytes()).into_stream()?;
                    let mut range: Option<(u64, u64)> = None;
                    while st.advance() {
                        if !st.key().starts_with(under.as_bytes()) {
                            break;
                        }
                        let o = st.term_ord();
                        range = Some(range.map_or((o, o), |(a, _)| (a, o)));
                    }
                    v.extend(range);
                }
            }
            out.paths = Some(v);
        }
        Ok(out)
    }
    pub fn keep(&self, kind: &RankCol, path: &RankCol, d: DocId) -> bool {
        if !self.on {
            return true;
        }
        let ok = |want: &Option<Vec<u64>>, col: &RankCol| match want {
            None => true,
            Some(v) => col.ord(d).is_some_and(|o| v.contains(&o)),
        };
        let in_range = match &self.paths {
            None => true,
            Some(v) => path.ord(d).is_some_and(|o| v.iter().any(|&(a, b)| a <= o && o <= b)),
        };
        ok(&self.kinds, kind) && in_range
    }
}

const STR_COLS: [&str; 7] = ["path_s", "kind", "name_s", "qn_s", "sig_s", "node_id", "id"];
const C_PATH: usize = 0;
const C_KIND: usize = 1;
const C_NODE: usize = 5;

pub struct Unused;
impl SegmentCollector for Unused {
    type Fruit = SegFruit;
    fn collect(&mut self, _: DocId, _: Score) {}
    fn harvest(self) -> SegFruit {
        unreachable!("ProductCollector overrides collect_segment")
    }
}

impl Collector for ProductCollector {
    type Fruit = Answer;
    type Child = Unused;

    fn for_segment(&self, _: u32, _: &SegmentReader) -> tantivy::Result<Unused> {
        Err(tantivy::TantivyError::InternalError("ProductCollector overrides collect_segment".into()))
    }

    fn requires_scoring(&self) -> bool {
        false
    }

    fn collect_segment(&self, weight: &dyn Weight, _ord: u32, seg: &SegmentReader) -> tantivy::Result<SegFruit> {
        let cands: Option<Vec<DocId>> = if self.via_weight {
            let mut c = Vec::new();
            weight.for_each_no_score(seg, &mut |ds| c.extend_from_slice(ds))?;
            c.sort_unstable();
            c.dedup();
            Some(c)
        } else {
            None
        };
        let max_doc = seg.max_doc();
        let alive_bits = seg.alive_bitset();
        let vis = self.hide.as_ref().and_then(|h| h.segment(seg.segment_id()));
        let alive = |d: DocId| alive_bits.map_or(true, |a| a.is_alive(d)) && vis.as_ref().map_or(true, |v| v[d as usize]);
        let mut acc = vec![0f64; max_doc as usize];
        let mut touched: Vec<DocId> = Vec::new();
        let mut per_term = Vec::with_capacity(self.terms.len());
        let mut pos: Vec<Vec<u32>> = Vec::new();
        for t in &self.terms {
            for (fi, field) in self.fields.text.iter().enumerate() {
                let inv = seg.inverted_index(*field)?;
                let w = WEIGHTS[fi];
                if !t.phrase() {
                    let term = Term::from_field_text(*field, &t.tokens[0]);
                    if let (Some(cs), Some(mut p)) = (&cands, inv.read_postings(&term, IndexRecordOption::WithFreqs)?) {
                        for &d in cs {
                            let pd = if p.doc() < d { p.seek(d) } else { p.doc() };
                            if pd == TERMINATED {
                                break;
                            }
                            if pd == d {
                                if acc[d as usize] == 0.0 {
                                    touched.push(d);
                                }
                                acc[d as usize] += w * p.term_freq() as f64;
                            }
                        }
                        continue;
                    }
                    if let Some(mut p) = inv.read_postings(&term, IndexRecordOption::WithFreqs)? {
                        let mut d = p.doc();
                        while d != TERMINATED {
                            if acc[d as usize] == 0.0 {
                                touched.push(d);
                            }
                            acc[d as usize] += w * p.term_freq() as f64;
                            d = p.advance();
                        }
                    }
                    continue;
                }
                let mut ps = Vec::with_capacity(t.tokens.len());
                for tok in &t.tokens {
                    let term = Term::from_field_text(*field, tok);
                    match inv.read_postings(&term, IndexRecordOption::WithFreqsAndPositions)? {
                        Some(p) => ps.push(p),
                        None => break,
                    }
                }
                if ps.len() < t.tokens.len() {
                    continue;
                }
                pos.resize_with(ps.len(), Vec::new);
                if let Some(cs) = &cands {
                    'cand: for &d in cs {
                        for p in ps.iter_mut() {
                            let pd = if p.doc() < d { p.seek(d) } else { p.doc() };
                            if pd == TERMINATED {
                                break 'cand;
                            }
                            if pd != d {
                                continue 'cand;
                            }
                        }
                        for (j, p) in ps.iter_mut().enumerate() {
                            p.positions(&mut pos[j]);
                            pos[j].truncate(MAX_TERM_OFFSETS);
                        }
                        let count = phrase_count(&pos[..ps.len()]);
                        if count > 0 {
                            if acc[d as usize] == 0.0 {
                                touched.push(d);
                            }
                            acc[d as usize] += w * count as f64;
                        }
                    }
                    continue;
                }
                let mut d = ps[0].doc();
                'walk: while d != TERMINATED {
                    let mut ahead = d;
                    for p in ps.iter_mut().skip(1) {
                        let pd = if p.doc() < d { p.seek(d) } else { p.doc() };
                        if pd == TERMINATED {
                            break 'walk;
                        }
                        ahead = ahead.max(pd);
                    }
                    if ahead > d {
                        d = ps[0].seek(ahead);
                        continue;
                    }
                    for (j, p) in ps.iter_mut().enumerate() {
                        p.positions(&mut pos[j]);
                        pos[j].truncate(MAX_TERM_OFFSETS);
                    }
                    let count = phrase_count(&pos[..ps.len()]);
                    if count > 0 {
                        if acc[d as usize] == 0.0 {
                            touched.push(d);
                        }
                        acc[d as usize] += w * count as f64;
                    }
                    d = ps[0].advance();
                }
            }
            touched.sort_unstable();
            let mut list = Vec::with_capacity(touched.len());
            for &d in &touched {
                if alive(d) {
                    list.push((d, acc[d as usize]));
                }
                acc[d as usize] = 0.0;
            }
            touched.clear();
            per_term.push(list);
        }
        Ok(SegFruit { max_doc, per_term, cols: SegCols::open(seg, self.dl, &self.filter)? })
    }

    fn merge_fruits(&self, fruits: Vec<SegFruit>) -> tantivy::Result<Answer> {
        let n = self.stats.docs;
        if n <= 0 || self.stats.tokens <= 0 || self.terms.is_empty() {
            return Ok(Answer { hits: 0, candidates: 0, page: Vec::new() });
        }
        let avgdl = self.stats.tokens as f64 / n as f64;
        let idf: Vec<f64> = (0..self.terms.len())
            .map(|i| bm25_idf(n, fruits.iter().map(|f| f.per_term[i].len() as i64).sum()))
            .collect();
        let mut global: HashMap<String, Cand> = HashMap::new();
        let mut candidates = 0u64;
        for f in &fruits {
            let mut score = vec![0f64; f.max_doc as usize];
            let mut hit = vec![false; f.max_doc as usize];
            let mut touched = Vec::new();
            for (i, list) in f.per_term.iter().enumerate() {
                for &(d, wtf) in list {
                    let dl = f.cols.dl.get(d) as i64;
                    if !hit[d as usize] {
                        hit[d as usize] = true;
                        touched.push(d);
                    }
                    score[d as usize] += idf[i] * bm25_saturation(wtf, dl, avgdl);
                }
            }
            // Fold inside the segment on ranks, which order exactly as the strings do here.
            let mut local: HashMap<(u64, u64, u64), OrdCand> = HashMap::new();
            for d in touched {
                if !f.cols.keep(d) {
                    continue;
                }
                candidates += 1;
                let c = f.cols.cand(d, quantize(score[d as usize]));
                fold_local(&mut local, c);
            }
            for cand in f.cols.resolve(local.into_values().collect())? {
                fold_global(&mut global, cand);
            }
        }
        let hits = global.len() as u64;
        Ok(Answer { hits, candidates, page: page_of(global, self.page_no) })
    }
}

/// Folds a segment-local candidate by its entity key (node rank, else path rank + start).
pub fn fold_local(local: &mut HashMap<(u64, u64, u64), OrdCand>, c: OrdCand) {
    let key = c.key();
    match local.get_mut(&key) {
        Some(have) if c.cmp(have) == Ordering::Less => *have = c,
        Some(_) => {}
        None => {
            local.insert(key, c);
        }
    }
}

/// lexical.go phraseCount: starts of token 0 at which token j sits at start+j.
fn phrase_count(pos: &[Vec<u32>]) -> u64 {
    let mut n = 0;
    'start: for &s in &pos[0] {
        for (j, p) in pos.iter().enumerate().skip(1) {
            if p.binary_search(&(s + j as u32)).is_err() {
                continue 'start;
            }
        }
        n += 1;
    }
    n
}

/// A segment-local candidate: string keys as segment ranks.
pub struct OrdCand {
    doc: DocId,
    pub micros: i64,
    start: u64,
    end: u64,
    r: [u64; 7],
}

impl OrdCand {
    /// The entity key within the segment.
    pub fn key(&self) -> (u64, u64, u64) {
        if self.r[C_NODE] != 0 { (0, self.r[C_NODE], 0) } else { (1, self.r[C_PATH], self.start) }
    }
    /// cmpScored on ranks: score desc, path, start, end, kind, name, qn, sig, node, id.
    pub fn cmp(&self, o: &OrdCand) -> Ordering {
        o.micros
            .cmp(&self.micros)
            .then(self.r[0].cmp(&o.r[0]))
            .then(self.start.cmp(&o.start))
            .then(self.end.cmp(&o.end))
            .then_with(|| self.r[1..].cmp(&o.r[1..]))
    }
}

/// A resolved candidate, carrying exactly what cmpScored reads.
pub struct Cand {
    pub micros: i64,
    pub path: String,
    pub start: u64,
    pub end: i64,
    pub kind: String,
    pub name: String,
    pub qn: String,
    pub sig: String,
    pub node: String,
    pub id: String,
}

/// rank.go cmpScored for lexical candidates (one tier, always a span).
pub fn cmp_scored(a: &Cand, b: &Cand) -> Ordering {
    b.micros
        .cmp(&a.micros)
        .then_with(|| a.path.cmp(&b.path))
        .then(a.start.cmp(&b.start))
        .then(a.end.cmp(&b.end))
        .then_with(|| a.kind.cmp(&b.kind))
        .then_with(|| a.name.cmp(&b.name))
        .then_with(|| a.qn.cmp(&b.qn))
        .then_with(|| a.sig.cmp(&b.sig))
        .then_with(|| a.node.cmp(&b.node))
        .then_with(|| a.id.cmp(&b.id))
}
