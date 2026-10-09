//! product-topk: our BM25F (product.rs, unchanged arithmetic) with an exact
//! block-max top-k shortcut.
//!
//! Side structure (written by `tblock`, one file per segment beside the index:
//! `tb-blockmax-<segment uuid>-<dl column>.bin`): for every single token present in
//! any of the five text columns, its union document frequency in the segment
//! (all documents, deleted ones included) and, for every 128-document block of
//! doc ids that holds it, the block's maximum weighted frequency
//! wtf = Σ_column weight × tf (weights 5/5/2/2/1) and minimum document length
//! (the `token_count` column, or `token_count_code` on a code index).
//!
//! Query, per term t and block b: ub_t(b) = idf_t × sat(wtf_max, dl_min, avgdl),
//! the same `bm25_saturation` our score uses. Because sat rises with wtf and falls
//! with dl, ub_t(b) bounds t's contribution to every document of b. A phrase term's
//! (document, wtf) list is evaluated exactly (positions are needed anyway, and its
//! df needs every match), and its block bound comes from that list.
//! The search, per segment: the blocks ordered by Σ_t ub_t(b), descending; a
//! block is scored exactly (postings re-sought from the block's first doc) unless
//! the segment already holds `depth` entities and quantize(bound) is below the
//! micros of its depth-th entity θ, which ends the segment. Documents scoring
//! below θ are not folded; entities below θ are dropped.
//!
//! Exactness: a document's micros never exceed its block's quantized bound (the
//! bound carries a 1e-9 relative + absolute margin over the floating sums), so
//! nothing skipped could reach θ; ties at θ are kept and settled by our
//! comparator. A global top-`depth` entity is in its own segment's local
//! top-`depth`, because its best document is in that segment, and an entity's
//! local best is never better than its global best. So the union of the
//! segments' local survivors, folded under cmpScored, holds the exact top
//! `depth` = page × 200.
//!
//! df, needed for idf before any skipping: from the side file when the segment
//! has neither deleted nor hidden documents, else a doc-id walk of the term's
//! five postings lists counting live, visible documents; phrases from their exact
//! lists. hits is not computed here (the caller counts it apart, as library mode does).
use crate::library::HideSet;
use crate::product::{fold_global, fold_local, page_of, Cand, CorpusStats, OrdCand, ProductCollector, SegCols};
use crate::{anyhow_lite::Result, bm25_idf, bm25_saturation, quantize, Fields, Filter, QTerm, WEIGHTS};
use std::collections::HashMap;
use std::io::{Read, Write};
use std::path::Path;
use std::sync::Arc;
use tantivy::collector::{Collector, SegmentCollector};
use tantivy::index::SegmentId;
use tantivy::postings::Postings;
use tantivy::query::Weight;
use tantivy::schema::IndexRecordOption;
use tantivy::{DocId, DocSet, Score, SegmentReader, Term, TERMINATED};

pub const BLOCK_BITS: u32 = 7;
pub const BLOCK: u32 = 1 << BLOCK_BITS;

/// One token's entry in a segment's side file.
pub struct TermBlocks {
    pub df: u32,
    /// (block, wtf_max, dl_min), blocks ascending.
    pub blocks: Vec<[u32; 3]>,
}

pub struct SegBlocks(pub HashMap<String, TermBlocks>);

pub fn file_name(seg: SegmentId, dl: &str) -> String {
    format!("tb-blockmax-{}-{dl}.bin", seg.uuid_string())
}

/// Builds a segment's side structure: a merge of the five columns' sorted term
/// dictionaries, so each token is visited once with all its columns.
pub fn build(seg: &SegmentReader, f: &Fields, dl: &str) -> Result<SegBlocks> {
    let dlc = seg.fast_fields().u64(dl)?;
    let invs = f.text.iter().map(|fl| seg.inverted_index(*fl)).collect::<tantivy::Result<Vec<_>>>()?;
    let mut streams = Vec::new();
    for inv in &invs {
        streams.push(inv.terms().stream()?);
    }
    let mut cur: Vec<Option<(Vec<u8>, tantivy::postings::TermInfo)>> = streams
        .iter_mut()
        .map(|s| if s.advance() { Some((s.key().to_vec(), s.value().clone())) } else { None })
        .collect();
    let mut out = HashMap::new();
    let mut docs: Vec<(DocId, u32)> = Vec::new();
    loop {
        let Some(min) = cur.iter().flatten().map(|x| x.0.clone()).min() else { break };
        docs.clear();
        for i in 0..cur.len() {
            if cur[i].as_ref().is_some_and(|x| x.0 == min) {
                let ti = cur[i].take().expect("current term").1;
                let mut p = invs[i].read_postings_from_terminfo(&ti, IndexRecordOption::WithFreqs)?;
                let w = WEIGHTS[i] as u32;
                let mut d = p.doc();
                while d != TERMINATED {
                    docs.push((d, w * p.term_freq()));
                    d = p.advance();
                }
                cur[i] = if streams[i].advance() { Some((streams[i].key().to_vec(), streams[i].value().clone())) } else { None };
            }
        }
        docs.sort_unstable_by_key(|x| x.0);
        let mut tb = TermBlocks { df: 0, blocks: Vec::new() };
        let mut j = 0;
        while j < docs.len() {
            let d = docs[j].0;
            let mut wtf = 0;
            while j < docs.len() && docs[j].0 == d {
                wtf += docs[j].1;
                j += 1;
            }
            tb.df += 1;
            let b = d >> BLOCK_BITS;
            let l = dlc.first(d).unwrap_or(0).min(u32::MAX as u64) as u32;
            match tb.blocks.last_mut() {
                Some(e) if e[0] == b => {
                    e[1] = e[1].max(wtf);
                    e[2] = e[2].min(l);
                }
                _ => tb.blocks.push([b, wtf, l]),
            }
        }
        if let Ok(s) = String::from_utf8(min) {
            out.insert(s, tb);
        }
    }
    Ok(SegBlocks(out))
}

impl SegBlocks {
    pub fn write(&self, path: &Path) -> Result<u64> {
        let mut w = std::io::BufWriter::new(std::fs::File::create(path)?);
        let mut n = 0u64;
        let mut put = |w: &mut std::io::BufWriter<std::fs::File>, v: u32| -> Result<()> {
            w.write_all(&v.to_le_bytes())?;
            n += 4;
            Ok(())
        };
        put(&mut w, self.0.len() as u32)?;
        for (t, tb) in &self.0 {
            put(&mut w, t.len() as u32)?;
            w.write_all(t.as_bytes())?;
            put(&mut w, tb.df)?;
            put(&mut w, tb.blocks.len() as u32)?;
            for e in &tb.blocks {
                for v in e {
                    put(&mut w, *v)?;
                }
            }
        }
        w.flush()?;
        Ok(std::fs::metadata(path)?.len())
    }

    pub fn read(path: &Path) -> Result<SegBlocks> {
        let mut b = Vec::new();
        std::fs::File::open(path)
            .map_err(|e| format!("{}: {e} (build it with tblock)", path.display()))?
            .read_to_end(&mut b)?;
        let mut c = Cursor { b: &b, at: 0 };
        let n = c.u32()?;
        let mut out = HashMap::with_capacity(n as usize);
        for _ in 0..n {
            let len = c.u32()? as usize;
            let t = String::from_utf8(c.bytes(len)?.to_vec())?;
            let df = c.u32()?;
            let nb = c.u32()?;
            let mut blocks = Vec::with_capacity(nb as usize);
            for _ in 0..nb {
                blocks.push([c.u32()?, c.u32()?, c.u32()?]);
            }
            out.insert(t, TermBlocks { df, blocks });
        }
        Ok(SegBlocks(out))
    }
}

struct Cursor<'a> {
    b: &'a [u8],
    at: usize,
}

impl Cursor<'_> {
    fn bytes(&mut self, n: usize) -> Result<&[u8]> {
        let v = self.b.get(self.at..self.at + n).ok_or("truncated block-max file")?;
        self.at += n;
        Ok(v)
    }
    fn u32(&mut self) -> Result<u32> {
        Ok(u32::from_le_bytes(self.bytes(4)?.try_into().expect("4 bytes")))
    }
}

/// Every searchable segment's side file, by segment id.
pub type Blocks = Arc<HashMap<SegmentId, Arc<SegBlocks>>>;

pub fn load(dir: &Path, segs: &[SegmentReader], dl: &str) -> Result<Blocks> {
    let mut m = HashMap::new();
    for s in segs {
        m.insert(s.segment_id(), Arc::new(SegBlocks::read(&dir.join(file_name(s.segment_id(), dl)))?));
    }
    Ok(Arc::new(m))
}

fn alive_fn<'a>(seg: &'a SegmentReader, hide: &Option<Arc<HideSet>>) -> (impl Fn(DocId) -> bool + 'a, bool) {
    let bits = seg.alive_bitset();
    let vis = hide.as_ref().and_then(|h| h.segment(seg.segment_id()));
    let any_hidden = vis.as_ref().is_some_and(|v| v.iter().any(|x| !x));
    let exact = bits.is_none() && !any_hidden;
    (move |d: DocId| bits.map_or(true, |a| a.is_alive(d)) && vis.as_ref().map_or(true, |v| v[d as usize]), exact)
}

/// Phase A's per-segment result: per term, df in the segment, and a phrase
/// term's exact (doc, wtf) list.
pub struct SegA {
    df: Vec<u64>,
    phrase: Vec<Option<Vec<(DocId, f64)>>>,
}

/// Phase A: df per term (and the phrase lists), per segment.
pub struct DfCollector {
    pub terms: Vec<QTerm>,
    pub fields: Fields,
    pub hide: Option<Arc<HideSet>>,
    pub blocks: Blocks,
    pub dl: &'static str,
}

pub struct NoChild<T>(std::marker::PhantomData<T>);
impl<T: 'static + Send> SegmentCollector for NoChild<T> {
    type Fruit = T;
    fn collect(&mut self, _: DocId, _: Score) {}
    fn harvest(self) -> T {
        unreachable!("collect_segment is overridden")
    }
}

impl Collector for DfCollector {
    type Fruit = HashMap<SegmentId, SegA>;
    type Child = NoChild<(SegmentId, SegA)>;
    fn for_segment(&self, _: u32, _: &SegmentReader) -> tantivy::Result<Self::Child> {
        Err(tantivy::TantivyError::InternalError("collect_segment is overridden".into()))
    }
    fn requires_scoring(&self) -> bool {
        false
    }
    fn collect_segment(&self, weight: &dyn Weight, ord: u32, seg: &SegmentReader) -> tantivy::Result<(SegmentId, SegA)> {
        let sb = self.blocks.get(&seg.segment_id()).ok_or_else(|| tantivy::TantivyError::InternalError("no block-max file for a segment: run tblock".into()))?;
        let (alive, exact) = alive_fn(seg, &self.hide);
        // Phrase terms: product's own evaluation, so the lists are its lists.
        let phrases: Vec<QTerm> = self.terms.iter().filter(|t| t.phrase()).cloned().collect();
        let mut lists = if phrases.is_empty() {
            Vec::new()
        } else {
            let pc = ProductCollector {
                terms: phrases, fields: self.fields, stats: CorpusStats { docs: 0, tokens: 0 }, filter: Filter::default(),
                hide: self.hide.clone(), via_weight: false, dl: self.dl, page_no: 1,
            };
            pc.collect_segment(weight, ord, seg)?.per_term
        }
        .into_iter();
        let mut a = SegA { df: Vec::new(), phrase: Vec::new() };
        for t in &self.terms {
            if t.phrase() {
                let l = lists.next().expect("one list per phrase");
                a.df.push(l.len() as u64);
                a.phrase.push(Some(l));
                continue;
            }
            a.phrase.push(None);
            if exact {
                a.df.push(sb.0.get(&t.tokens[0]).map_or(0, |tb| tb.df as u64));
                continue;
            }
            let mut docs = Vec::new();
            for f in &self.fields.text {
                if let Some(mut p) = seg.inverted_index(*f)?.read_postings(&Term::from_field_text(*f, &t.tokens[0]), IndexRecordOption::Basic)? {
                    let mut d = p.doc();
                    while d != TERMINATED {
                        docs.push(d);
                        d = p.advance();
                    }
                }
            }
            docs.sort_unstable();
            docs.dedup();
            a.df.push(docs.into_iter().filter(|d| alive(*d)).count() as u64);
        }
        Ok((seg.segment_id(), a))
    }
    fn merge_fruits(&self, f: Vec<(SegmentId, SegA)>) -> tantivy::Result<Self::Fruit> {
        Ok(f.into_iter().collect())
    }
}

/// What product-topk returns, with the work it did.
#[derive(Debug, Default)]
pub struct TopkAnswer {
    pub page: Vec<String>,
    pub blocks_total: u64,
    pub blocks_scored: u64,
    pub docs_scored: u64,
}

/// Phase B: the exact top `page_no * 200` entities by block-max skipping.
pub struct TopkCollector {
    pub terms: Vec<QTerm>,
    pub fields: Fields,
    pub stats: CorpusStats,
    pub filter: Filter,
    pub hide: Option<Arc<HideSet>>,
    pub blocks: Blocks,
    pub dl: &'static str,
    pub page_no: usize,
    pub a: Arc<HashMap<SegmentId, SegA>>,
}

pub struct SegB {
    cands: Vec<Cand>,
    blocks_total: u64,
    blocks_scored: u64,
    docs_scored: u64,
}

impl TopkCollector {
    fn idf(&self) -> Vec<f64> {
        (0..self.terms.len()).map(|i| bm25_idf(self.stats.docs, self.a.values().map(|a| a.df[i] as i64).sum())).collect()
    }
}

/// The margin that keeps a bound above every floating sum it bounds.
fn safe(x: f64) -> i64 {
    quantize(x * (1.0 + 1e-9) + 1e-9)
}

impl Collector for TopkCollector {
    type Fruit = TopkAnswer;
    type Child = NoChild<SegB>;
    fn for_segment(&self, _: u32, _: &SegmentReader) -> tantivy::Result<Self::Child> {
        Err(tantivy::TantivyError::InternalError("collect_segment is overridden".into()))
    }
    fn requires_scoring(&self) -> bool {
        false
    }
    fn collect_segment(&self, _w: &dyn Weight, _ord: u32, seg: &SegmentReader) -> tantivy::Result<SegB> {
        let internal = |m: &str| tantivy::TantivyError::InternalError(m.into());
        let sb = self.blocks.get(&seg.segment_id()).ok_or_else(|| internal("no block-max file for a segment: run tblock"))?;
        let a = self.a.get(&seg.segment_id()).ok_or_else(|| internal("phase A missed a segment"))?;
        let n = self.stats.docs;
        let mut out = SegB { cands: Vec::new(), blocks_total: 0, blocks_scored: 0, docs_scored: 0 };
        if n <= 0 || self.stats.tokens <= 0 || self.terms.is_empty() {
            return Ok(out);
        }
        let avgdl = self.stats.tokens as f64 / n as f64;
        let idf = self.idf();
        let depth = self.page_no.max(1) * crate::product::PAGE;
        let max_doc = seg.max_doc();
        let nb = ((max_doc + BLOCK - 1) >> BLOCK_BITS) as usize;
        let cols = SegCols::open(seg, self.dl, &self.filter)?;
        let (alive, _) = alive_fn(seg, &self.hide);
        // Block bounds.
        let mut ub = vec![0f64; nb];
        let mut infos: Vec<Vec<(usize, tantivy::postings::TermInfo)>> = Vec::with_capacity(self.terms.len());
        for (i, t) in self.terms.iter().enumerate() {
            let mut per_field = Vec::new();
            if let Some(list) = &a.phrase[i] {
                let mut j = 0;
                while j < list.len() {
                    let b = (list[j].0 >> BLOCK_BITS) as usize;
                    let (mut w, mut l) = (0f64, i64::MAX);
                    while j < list.len() && (list[j].0 >> BLOCK_BITS) as usize == b {
                        w = w.max(list[j].1);
                        l = l.min(cols.dl.first(list[j].0).unwrap_or(0) as i64);
                        j += 1;
                    }
                    ub[b] += idf[i] * bm25_saturation(w, l, avgdl);
                }
            } else {
                if let Some(tb) = sb.0.get(&t.tokens[0]) {
                    for e in &tb.blocks {
                        ub[e[0] as usize] += idf[i] * bm25_saturation(e[1] as f64, e[2] as i64, avgdl);
                    }
                }
                for (fi, f) in self.fields.text.iter().enumerate() {
                    if let Some(ti) = seg.inverted_index(*f)?.get_term_info(&Term::from_field_text(*f, &t.tokens[0]))? {
                        per_field.push((fi, ti));
                    }
                }
            }
            infos.push(per_field);
        }
        let mut order: Vec<usize> = (0..nb).filter(|b| ub[*b] > 0.0).collect();
        out.blocks_total = order.len() as u64;
        order.sort_by(|x, y| ub[*y].total_cmp(&ub[*x]).then(x.cmp(y)));
        let invs = self.fields.text.iter().map(|f| seg.inverted_index(*f)).collect::<tantivy::Result<Vec<_>>>()?;
        let mut local: HashMap<(u64, u64, u64), OrdCand> = HashMap::new();
        let mut theta = i64::MIN;
        let mut wtf = vec![0f64; self.terms.len() * BLOCK as usize];
        for b in order {
            if local.len() >= depth && safe(ub[b]) < theta {
                break;
            }
            out.blocks_scored += 1;
            let base = (b as u32) << BLOCK_BITS;
            let end = (base + BLOCK).min(max_doc);
            wtf.iter_mut().for_each(|x| *x = 0.0);
            for i in 0..self.terms.len() {
                let row = &mut wtf[i * BLOCK as usize..(i + 1) * BLOCK as usize];
                if let Some(list) = &a.phrase[i] {
                    let from = list.partition_point(|x| x.0 < base);
                    for &(d, w) in list[from..].iter().take_while(|x| x.0 < end) {
                        row[(d - base) as usize] += w;
                    }
                    continue;
                }
                for (fi, ti) in &infos[i] {
                    let mut p = invs[*fi].read_postings_from_terminfo(ti, IndexRecordOption::WithFreqs)?;
                    let mut d = if p.doc() < base { p.seek(base) } else { p.doc() };
                    let w = WEIGHTS[*fi];
                    while d != TERMINATED && d < end {
                        row[(d - base) as usize] += w * p.term_freq() as f64;
                        d = p.advance();
                    }
                }
            }
            for off in 0..(end - base) as usize {
                let d = base + off as u32;
                if !(0..self.terms.len()).any(|i| wtf[i * BLOCK as usize + off] > 0.0) || !alive(d) {
                    continue;
                }
                out.docs_scored += 1;
                let dl = cols.dl.first(d).unwrap_or(0) as i64;
                let mut score = 0f64;
                for i in 0..self.terms.len() {
                    let w = wtf[i * BLOCK as usize + off];
                    if w > 0.0 {
                        score += idf[i] * bm25_saturation(w, dl, avgdl);
                    }
                }
                let micros = quantize(score);
                if micros < theta || !cols.keep(d) {
                    continue;
                }
                fold_local(&mut local, cols.cand(d, micros));
            }
            if local.len() >= depth {
                let mut m: Vec<i64> = local.values().map(|c| c.micros).collect();
                let k = m.len() - depth;
                m.select_nth_unstable(k);
                theta = theta.max(m[k]);
                local.retain(|_, c| c.micros >= theta);
            }
        }
        out.cands = cols.resolve(local.into_values().collect())?;
        Ok(out)
    }
    fn merge_fruits(&self, fruits: Vec<SegB>) -> tantivy::Result<TopkAnswer> {
        let mut global = HashMap::new();
        let mut ans = TopkAnswer::default();
        for f in fruits {
            ans.blocks_total += f.blocks_total;
            ans.blocks_scored += f.blocks_scored;
            ans.docs_scored += f.docs_scored;
            for c in f.cands {
                fold_global(&mut global, c);
            }
        }
        ans.page = page_of(global, self.page_no);
        Ok(ans)
    }
}

/// Both phases on a searcher (its executor decides the threads).
#[allow(clippy::too_many_arguments)]
pub fn search(
    s: &tantivy::Searcher, terms: Vec<QTerm>, fields: Fields, stats: CorpusStats, filter: Filter, hide: Option<Arc<HideSet>>,
    blocks: Blocks, dl: &'static str, page_no: usize,
) -> tantivy::Result<TopkAnswer> {
    let all = tantivy::query::AllQuery;
    let a = s.search(&all, &DfCollector { terms: terms.clone(), fields, hide: hide.clone(), blocks: blocks.clone(), dl })?;
    s.search(&all, &TopkCollector { terms, fields, stats, filter, hide, blocks, dl, page_no, a: Arc::new(a) })
}
