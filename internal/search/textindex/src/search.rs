//! One page of the lexical tier (docs/adr/ADR-0013-search-index.md decisions
//! 2 and 5): our BM25F over the view's alive documents, the exact top
//! depth+1 folded results per segment found by block-max pruning, folded
//! across segments under the content-only comparator.
//!
//! Exactness. A block is skipped only when its quantized upper bound (with a
//! margin over the floating sums) is strictly below the score of the
//! (depth+1)-th entry of a full heap, so nothing skipped could enter the top
//! depth+1 or raise a result's score. An entity's best document in a segment
//! is its best in that segment, and a result that depth+1 others beat in one
//! segment is beaten by them globally, so the union of the segments' heaps
//! holds the exact global top depth+1. `more` is then whether a (depth+1)-th
//! folded result exists.
//!
//! Counts. `matches` counts the matching visible documents that pass the
//! filters and the score floor; it is exact when no block was skipped. A
//! hit's occurrence count is exact when no block was skipped and, in every
//! segment whose heap ever turned a document away, the hit's entry was
//! admitted before the first turn-away and never left (so it saw every one
//! of its documents); otherwise it is a lower bound.

use std::cmp::{Ordering, Reverse};
use std::collections::{BTreeSet, HashMap};
use std::sync::atomic::{AtomicBool, Ordering as AtomicOrdering};
use std::sync::Arc;

use tantivy::columnar::{Column, StrColumn};
use tantivy::postings::{Postings, SegmentPostings, TermInfo};
use tantivy::schema::IndexRecordOption;
use tantivy::{DocId, DocSet, InvertedIndexReader, SegmentReader, Term, TERMINATED};

use crate::codec::{put_str, put_uvarint, put_varint, Reader};
use crate::error::{Error, Result, CX_CANCELED, CX_TOO_MANY_TERMS};
use crate::schema::{Fields, S_KIND, S_NODE, S_PATH, STRS};
use crate::score::{idf, quantize, safe_bound, saturation, WEIGHTS};
use crate::summary::{Lengths, Summary, BLOCK, BLOCK_BITS, UNBOUNDED_WTF};
use crate::tokenizer::{parse_query, QueryTerm};
use crate::view::{SegView, View};

/// The cancel byte of cx_view_search.
#[derive(Clone, Copy)]
pub struct Cancel(pub *const u8);
// SAFETY: the byte is only read, with a volatile load, while the caller keeps
// it alive for the whole call.
unsafe impl Send for Cancel {}
unsafe impl Sync for Cancel {}

impl Cancel {
    fn raised(&self) -> bool {
        // SAFETY: a non-null pointer names one byte the caller owns for the call.
        !self.0.is_null() && unsafe { std::ptr::read_volatile(self.0) } != 0
    }
}

#[derive(Clone, Copy, PartialEq)]
enum Rule {
    Total,
    Weighted,
}

struct Request {
    query: String,
    kinds: Vec<String>,
    prefixes: Vec<String>,
    depth: usize,
    min_score: i64,
    rule: Rule,
    max_terms: u64,
    ignore_summaries: bool,
}

fn decode(buf: &[u8]) -> Result<Request> {
    let mut r = Reader::new(buf);
    let query = r.str()?.to_string();
    let n = r.uvarint()?;
    let mut kinds = Vec::new();
    for _ in 0..n {
        kinds.push(r.str()?.to_string());
    }
    let n = r.uvarint()?;
    let mut prefixes = Vec::new();
    for _ in 0..n {
        prefixes.push(r.str()?.to_string());
    }
    let depth = r.uvarint()?;
    if depth == 0 {
        return Err(Error::invalid("a search depth must be at least 1"));
    }
    let depth = usize::try_from(depth).map_err(|_| Error::invalid("a search depth overflows"))?;
    let min_score = r.varint()?;
    let rule = match r.byte()? {
        0 => Rule::Total,
        1 => Rule::Weighted,
        v => return Err(Error::invalid(format!("unknown length rule {v}"))),
    };
    let max_terms = r.uvarint()?;
    let flags = r.byte()?;
    r.finish()?;
    Ok(Request { query, kinds, prefixes, depth, min_score, rule, max_terms, ignore_summaries: flags & 1 != 0 })
}

/// Answers one page (the cx_view_search layouts).
pub fn search(view: &View, req: &[u8], cancel: Cancel) -> Result<Vec<u8>> {
    let req = decode(req)?;
    let terms = parse_query(&req.query);
    if req.max_terms > 0 && terms.len() as u64 > req.max_terms {
        return Err(Error::new(CX_TOO_MANY_TERMS, terms.len().to_string()));
    }
    let fields = view.index.fields;
    let segs: Vec<(&SegmentReader, &SegView)> = view.searcher.segment_readers().iter().zip(&view.segs).collect();
    let mut page = Page {
        segments: segs.len() as u64,
        segments_without_summary: view.segs.iter().filter(|s| s.summary.is_none()).count() as u64,
        matches_exact: true,
        ..Page::default()
    };
    let length_sum = match req.rule {
        Rule::Total => view.total,
        Rule::Weighted => view.weighted,
    };
    if terms.is_empty() || view.docs == 0 || length_sum == 0 {
        return Ok(page.encode(terms.len()));
    }
    let avgdl = length_sum as f64 / view.docs as f64;

    if cancel.raised() {
        return Err(Error::new(CX_CANCELED, "the search was canceled before it began"));
    }
    let phase_a = view.index.executor.map(
        |(seg, sv): (&SegmentReader, &SegView)| Ok(segment_df(seg, sv, &fields, &terms, req.ignore_summaries)),
        segs.iter().copied(),
    )?;
    let phase_a = phase_a.into_iter().collect::<Result<Vec<_>>>()?;
    if cancel.raised() {
        return Err(Error::new(CX_CANCELED, "the search was canceled before any block was scored"));
    }
    let mut df = vec![0u64; terms.len()];
    for a in &phase_a {
        for (i, v) in a.df.iter().enumerate() {
            df[i] += v;
        }
    }
    let idfs: Vec<f64> = df.iter().map(|&d| idf(view.docs, d)).collect();

    let scored_any = AtomicBool::new(false);
    let ctx = Ctx {
        fields: &fields,
        terms: &terms,
        idf: &idfs,
        avgdl,
        rule: req.rule,
        cap: req.depth.saturating_add(1),
        min_score: req.min_score,
        kinds: &req.kinds,
        prefixes: &req.prefixes,
        ignore_summaries: req.ignore_summaries,
        cancel,
        scored_any: &scored_any,
    };
    let jobs: Vec<_> = segs.iter().copied().zip(phase_a).collect();
    let fruits = view.index.executor.map(|((seg, sv), a)| Ok(score_segment(&ctx, seg, sv, a)), jobs.into_iter())?;
    let fruits = fruits.into_iter().collect::<Result<Vec<_>>>()?;
    let canceled = fruits.iter().any(|f| f.canceled);
    if canceled && !scored_any.load(AtomicOrdering::Relaxed) {
        return Err(Error::new(CX_CANCELED, "the search was canceled before any block was scored"));
    }

    // Fold across segments.
    let skipped = fruits.iter().any(|f| f.skipped) || canceled;
    let dropping = fruits.iter().filter(|f| f.dropped).count() as u32;
    let mut global: HashMap<GlobalKey, (Hit, u64, u32)> = HashMap::new();
    for f in fruits {
        page.matches += f.matches;
        page.blocks_total += f.blocks_total;
        page.blocks_scored += f.blocks_scored;
        page.docs_scored += f.docs_scored;
        for (hit, count, complete) in f.hits {
            let seen = u32::from(f.dropped && complete);
            match global.get_mut(&hit.key()) {
                Some(e) => {
                    if cmp_hits(&hit, &e.0) == Ordering::Less {
                        e.0 = hit;
                    }
                    e.1 += count;
                    e.2 += seen;
                }
                None => {
                    global.insert(hit.key(), (hit, count, seen));
                }
            }
        }
    }
    let mut all: Vec<(Hit, u64, u32)> = global.into_values().collect();
    all.sort_by(|a, b| cmp_hits(&a.0, &b.0));
    page.more = all.len() > req.depth;
    all.truncate(req.depth);
    page.canceled = canceled;
    page.matches_exact = !skipped;
    page.hits = all.into_iter().map(|(h, n, seen)| (h, n, !skipped && seen == dropping)).collect();
    Ok(page.encode(terms.len()))
}

#[derive(Default)]
struct Page {
    more: bool,
    canceled: bool,
    matches: u64,
    matches_exact: bool,
    segments: u64,
    segments_without_summary: u64,
    blocks_total: u64,
    blocks_scored: u64,
    docs_scored: u64,
    hits: Vec<(Hit, u64, bool)>,
}

impl Page {
    fn encode(&self, terms: usize) -> Vec<u8> {
        let mut b = Vec::with_capacity(64 + self.hits.len() * 128);
        put_uvarint(&mut b, terms as u64);
        b.push(u8::from(self.more));
        b.push(u8::from(self.canceled));
        put_uvarint(&mut b, self.matches);
        b.push(u8::from(self.matches_exact));
        for v in [self.segments, self.segments_without_summary, self.blocks_total, self.blocks_scored, self.docs_scored] {
            put_uvarint(&mut b, v);
        }
        put_uvarint(&mut b, self.hits.len() as u64);
        for (h, n, exact) in &self.hits {
            for s in [&h.search_key, &h.node, &h.file, &h.path, &h.kind, &h.name, &h.qn, &h.sig] {
                put_str(&mut b, s);
            }
            put_uvarint(&mut b, h.start);
            put_uvarint(&mut b, h.end);
            put_varint(&mut b, h.micros);
            put_uvarint(&mut b, *n);
            b.push(u8::from(*exact));
        }
        b
    }
}

/// A resolved result.
struct Hit {
    micros: i64,
    path: String,
    start: u64,
    end: u64,
    kind: String,
    name: String,
    qn: String,
    sig: String,
    node: String,
    search_key: String,
    file: String,
}

#[derive(Hash, PartialEq, Eq)]
enum GlobalKey {
    Node(String),
    File(String, u64),
}

impl Hit {
    fn key(&self) -> GlobalKey {
        if self.node.is_empty() {
            GlobalKey::File(self.path.clone(), self.start)
        } else {
            GlobalKey::Node(self.node.clone())
        }
    }
}

/// Score descending, then path, start byte, end byte, kind, name, qualified
/// name, signature, node id and search key.
fn cmp_hits(a: &Hit, b: &Hit) -> Ordering {
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
        .then_with(|| a.search_key.cmp(&b.search_key))
}

/// Phase A's per-segment result: df per term, and each phrase term's exact
/// (document, weighted frequency) list over alive documents.
struct SegA {
    df: Vec<u64>,
    phrase: Vec<Option<Vec<(DocId, f64)>>>,
}

fn segment_df(seg: &SegmentReader, sv: &SegView, fields: &Fields, terms: &[QueryTerm], ignore: bool) -> Result<SegA> {
    let invs = fields.text.iter().map(|f| seg.inverted_index(*f)).collect::<tantivy::Result<Vec<_>>>()?;
    let mut a = SegA { df: Vec::with_capacity(terms.len()), phrase: Vec::with_capacity(terms.len()) };
    for t in terms {
        if t.phrase() {
            let list = phrase_list(&invs, fields, &t.tokens, sv)?;
            a.df.push(list.len() as u64);
            a.phrase.push(Some(list));
            continue;
        }
        a.phrase.push(None);
        let tok = &t.tokens[0];
        if let (false, true, Some(s)) = (ignore, sv.alive.clean, &sv.summary) {
            a.df.push(s.term(tok.as_bytes()).map_or(0, |e| u64::from(e.df)));
            continue;
        }
        // A walk of the term's five lists in doc order, counting alive documents.
        let mut ps: Vec<SegmentPostings> = Vec::new();
        for (fi, inv) in invs.iter().enumerate() {
            if let Some(p) = inv.read_postings(&Term::from_field_text(fields.text[fi], tok), IndexRecordOption::Basic)? {
                ps.push(p);
            }
        }
        let mut n = 0u64;
        loop {
            let d = ps.iter().map(|p| p.doc()).min().unwrap_or(TERMINATED);
            if d == TERMINATED {
                break;
            }
            if sv.alive.alive(d) {
                n += 1;
            }
            for p in ps.iter_mut() {
                if p.doc() == d {
                    p.advance();
                }
            }
        }
        a.df.push(n);
    }
    Ok(a)
}

/// A phrase's exact list: per column the start positions where token j sits
/// at start + j (every position counts), weighted and summed over columns,
/// alive documents only, doc ids ascending.
fn phrase_list(invs: &[Arc<InvertedIndexReader>], fields: &Fields, tokens: &[String], sv: &SegView) -> Result<Vec<(DocId, f64)>> {
    let mut all: Vec<(DocId, u64)> = Vec::new();
    let mut pos: Vec<Vec<u32>> = vec![Vec::new(); tokens.len()];
    for (fi, inv) in invs.iter().enumerate() {
        let mut ps = Vec::with_capacity(tokens.len());
        for tok in tokens {
            match inv.read_postings(&Term::from_field_text(fields.text[fi], tok), IndexRecordOption::WithFreqsAndPositions)? {
                Some(p) => ps.push(p),
                None => break,
            }
        }
        if ps.len() < tokens.len() {
            continue;
        }
        let w = WEIGHTS[fi];
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
            if sv.alive.alive(d) {
                for (j, p) in ps.iter_mut().enumerate() {
                    pos[j].clear();
                    p.positions(&mut pos[j]);
                }
                let count = phrase_count(&pos);
                if count > 0 {
                    all.push((d, w * count));
                }
            }
            d = ps[0].advance();
        }
    }
    all.sort_unstable_by_key(|x| x.0);
    let mut out: Vec<(DocId, f64)> = Vec::new();
    for (d, w) in all {
        match out.last_mut() {
            Some(last) if last.0 == d => last.1 += w as f64,
            _ => out.push((d, w as f64)),
        }
    }
    Ok(out)
}

/// The start positions of token 0 at which every token j sits at start + j.
fn phrase_count(pos: &[Vec<u32>]) -> u64 {
    let mut n = 0;
    'start: for &s in &pos[0] {
        for (j, p) in pos.iter().enumerate().skip(1) {
            let Some(want) = s.checked_add(j as u32) else { continue 'start };
            if p.binary_search(&want).is_err() {
                continue 'start;
            }
        }
        n += 1;
    }
    n
}

/// What every segment's scoring shares.
struct Ctx<'a> {
    fields: &'a Fields,
    terms: &'a [QueryTerm],
    idf: &'a [f64],
    avgdl: f64,
    rule: Rule,
    cap: usize,
    min_score: i64,
    kinds: &'a [String],
    prefixes: &'a [String],
    ignore_summaries: bool,
    cancel: Cancel,
    scored_any: &'a AtomicBool,
}

struct SegFruit {
    hits: Vec<(Hit, u64, bool)>,
    dropped: bool,
    skipped: bool,
    canceled: bool,
    matches: u64,
    blocks_total: u64,
    blocks_scored: u64,
    docs_scored: u64,
}

/// A string fast column as an order-preserving rank: an empty or absent value
/// is 0, every other value ranks above it in byte order (within one segment).
struct RankCol {
    col: Option<StrColumn>,
    shift: u64,
}

impl RankCol {
    fn open(seg: &SegmentReader, name: &str) -> Result<RankCol> {
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
        self.ord(d).map_or(0, |o| o + self.shift)
    }
}

/// The first ordinal of a sorted string dictionary at or above key.
fn first_ord_ge(c: &StrColumn, key: &[u8]) -> Result<u64> {
    let mut st = c.dictionary().range().ge(key).into_stream()?;
    Ok(if st.advance() { st.term_ord() } else { c.num_terms() as u64 })
}

/// The kind and path filters as ordinal sets of one segment.
struct SegFilter {
    kinds: Option<Vec<u64>>,
    paths: Option<Vec<(u64, u64)>>,
}

impl SegFilter {
    fn build(kinds: &[String], prefixes: &[String], kind: &RankCol, path: &RankCol) -> Result<SegFilter> {
        let mut f = SegFilter { kinds: None, paths: None };
        if !kinds.is_empty() {
            let mut v = Vec::new();
            if let Some(c) = &kind.col {
                for k in kinds {
                    if let Some(o) = c.dictionary().term_ord(k.as_bytes())? {
                        v.push(o);
                    }
                }
            }
            f.kinds = Some(v);
        }
        if !prefixes.is_empty() {
            let mut v = Vec::new();
            if let Some(c) = &path.col {
                for p in prefixes {
                    if let Some(o) = c.dictionary().term_ord(p.as_bytes())? {
                        v.push((o, o + 1));
                    }
                    // Every value starting with p + "/" lies in [p + "/", p + "0").
                    let lo = first_ord_ge(c, format!("{p}/").as_bytes())?;
                    let hi = first_ord_ge(c, format!("{p}0").as_bytes())?;
                    if lo < hi {
                        v.push((lo, hi));
                    }
                }
            }
            f.paths = Some(v);
        }
        Ok(f)
    }

    fn keep(&self, kind: &RankCol, path: &RankCol, d: DocId) -> bool {
        if let Some(v) = &self.kinds {
            if !kind.ord(d).is_some_and(|o| v.contains(&o)) {
                return false;
            }
        }
        if let Some(v) = &self.paths {
            if !path.ord(d).is_some_and(|o| v.iter().any(|&(a, b)| a <= o && o < b)) {
                return false;
            }
        }
        true
    }
}

/// The segment-local order of a candidate: score descending, then the content
/// ranks in comparator order, then the doc id (unique within the segment).
#[derive(Clone, Copy, PartialEq, Eq, PartialOrd, Ord)]
struct OrdKey {
    micros: Reverse<i64>,
    path: u64,
    start: u64,
    end: u64,
    kind: u64,
    name: u64,
    qn: u64,
    sig: u64,
    node: u64,
    search_key: u64,
    doc: DocId,
}

impl OrdKey {
    /// The fold key inside the segment: the node, else the file and start byte.
    fn entity(&self) -> (u8, u64, u64) {
        if self.node != 0 {
            (0, self.node, 0)
        } else {
            (1, self.path, self.start)
        }
    }
}

struct Slot {
    key: OrdKey,
    count: u64,
    complete: bool,
}

/// Exactly cap folded entries, best first.
struct Heap {
    cap: usize,
    order: BTreeSet<OrdKey>,
    slots: HashMap<(u8, u64, u64), Slot>,
    dropped: bool,
}

impl Heap {
    fn worst(&self) -> Option<i64> {
        if self.order.len() < self.cap {
            return None;
        }
        self.order.last().map(|k| k.micros.0)
    }

    fn offer(&mut self, k: OrdKey) {
        let e = k.entity();
        if let Some(slot) = self.slots.get_mut(&e) {
            slot.count += 1;
            if k < slot.key {
                self.order.remove(&slot.key);
                self.order.insert(k);
                slot.key = k;
            }
            return;
        }
        if self.order.len() < self.cap {
            self.order.insert(k);
            self.slots.insert(e, Slot { key: k, count: 1, complete: !self.dropped });
            return;
        }
        let worst = *self.order.last().expect("a full heap has a last entry");
        if k < worst {
            self.order.remove(&worst);
            self.slots.remove(&worst.entity());
            let complete = !self.dropped;
            self.dropped = true;
            self.order.insert(k);
            self.slots.insert(e, Slot { key: k, count: 1, complete });
        } else {
            self.dropped = true;
        }
    }
}

/// One (term, column) postings cursor that re-seeks when a block lies behind it.
struct Cursor {
    inv: Arc<InvertedIndexReader>,
    info: TermInfo,
    weight: f64,
    p: SegmentPostings,
    done_to: DocId,
}

impl Cursor {
    fn fill(&mut self, base: DocId, end: DocId, row: &mut [f64]) -> Result<()> {
        if base < self.done_to {
            self.p = self.inv.read_postings_from_terminfo(&self.info, IndexRecordOption::WithFreqs)?;
        }
        let mut d = if self.p.doc() < base { self.p.seek(base) } else { self.p.doc() };
        while d != TERMINATED && d < end {
            row[(d - base) as usize] += self.weight * f64::from(self.p.term_freq());
            d = self.p.advance();
        }
        self.done_to = end;
        Ok(())
    }
}

fn score_segment(ctx: &Ctx, seg: &SegmentReader, sv: &SegView, a: SegA) -> Result<SegFruit> {
    let f = ctx.fields;
    let nt = ctx.terms.len();
    let max_doc = seg.max_doc();
    let mut out = SegFruit {
        hits: Vec::new(),
        dropped: false,
        skipped: false,
        canceled: false,
        matches: 0,
        blocks_total: 0,
        blocks_scored: 0,
        docs_scored: 0,
    };
    if max_doc == 0 || sv.alive.docs == 0 {
        return Ok(out);
    }
    let ranks = STRS.iter().map(|n| RankCol::open(seg, n)).collect::<Result<Vec<_>>>()?;
    let filter = SegFilter::build(ctx.kinds, ctx.prefixes, &ranks[S_KIND], &ranks[S_PATH])?;
    let ff = seg.fast_fields();
    let start_col: Column<u64> = ff.u64(crate::schema::START)?;
    let end_col: Column<u64> = ff.u64(crate::schema::END)?;
    let lens = Lengths::open(seg)?;
    let dl_of = |d: DocId| {
        let (t, w) = lens.get(d);
        if ctx.rule == Rule::Total {
            t
        } else {
            w
        }
    };

    // The postings cursors of every plain term.
    let mut cursors: Vec<Vec<Cursor>> = Vec::with_capacity(nt);
    for (i, t) in ctx.terms.iter().enumerate() {
        let mut cs = Vec::new();
        if a.phrase[i].is_none() {
            for (fi, field) in f.text.iter().enumerate() {
                let inv = seg.inverted_index(*field)?;
                if let Some(info) = inv.get_term_info(&Term::from_field_text(*field, &t.tokens[0]))? {
                    let p = inv.read_postings_from_terminfo(&info, IndexRecordOption::WithFreqs)?;
                    cs.push(Cursor { inv: inv.clone(), info, weight: WEIGHTS[fi] as f64, p, done_to: 0 });
                }
            }
        }
        cursors.push(cs);
    }

    // The visit order: by bound when the segment has a usable summary, else
    // every block holding a posting in doc order.
    let summary: Option<&Summary> = if ctx.ignore_summaries { None } else { sv.summary.as_deref() };
    let ordered: Option<Vec<(u32, f64)>> = summary.map(|s| bounds(ctx, s, &a, &dl_of));
    if let Some(o) = &ordered {
        out.blocks_total = o.len() as u64;
    }

    let mut heap = Heap { cap: ctx.cap, order: BTreeSet::new(), slots: HashMap::new(), dropped: false };
    let mut wtf = vec![0f64; nt * BLOCK as usize];
    let mut phrase_at = vec![0usize; nt];
    let mut next = 0usize;
    loop {
        let block = match &ordered {
            Some(o) => {
                let Some(&(b, ub)) = o.get(next) else { break };
                let bound = safe_bound(ub);
                if ctx.min_score > 0 && bound < ctx.min_score {
                    // This block and every later one hold no document at the floor.
                    break;
                }
                if heap.worst().is_some_and(|w| bound < w) {
                    out.skipped = true;
                    break;
                }
                next += 1;
                b
            }
            None => {
                // The lowest next posting of any term starts the next block.
                let mut low = TERMINATED;
                for (i, cs) in cursors.iter().enumerate() {
                    for c in cs {
                        low = low.min(c.p.doc());
                    }
                    if let Some(list) = &a.phrase[i] {
                        if let Some(&(d, _)) = list.get(phrase_at[i]) {
                            low = low.min(d);
                        }
                    }
                }
                if low == TERMINATED {
                    break;
                }
                out.blocks_total += 1;
                low >> BLOCK_BITS
            }
        };
        if ctx.cancel.raised() {
            out.canceled = true;
            break;
        }
        let base = block << BLOCK_BITS;
        let end = base.saturating_add(BLOCK).min(max_doc);
        wtf.iter_mut().for_each(|x| *x = 0.0);
        for i in 0..nt {
            let row = &mut wtf[i * BLOCK as usize..(i + 1) * BLOCK as usize];
            if let Some(list) = &a.phrase[i] {
                let from = list.partition_point(|x| x.0 < base);
                let mut j = from;
                while j < list.len() && list[j].0 < end {
                    row[(list[j].0 - base) as usize] += list[j].1;
                    j += 1;
                }
                phrase_at[i] = j;
                continue;
            }
            for c in cursors[i].iter_mut() {
                c.fill(base, end, row)?;
            }
        }
        out.blocks_scored += 1;
        ctx.scored_any.store(true, AtomicOrdering::Relaxed);
        for off in 0..(end - base) as usize {
            let d = base + off as u32;
            if !(0..nt).any(|i| wtf[i * BLOCK as usize + off] > 0.0) || !sv.alive.alive(d) {
                continue;
            }
            if !filter.keep(&ranks[S_KIND], &ranks[S_PATH], d) {
                continue;
            }
            out.docs_scored += 1;
            let dl = dl_of(d);
            let mut score = 0f64;
            for i in 0..nt {
                let w = wtf[i * BLOCK as usize + off];
                if w > 0.0 {
                    score += ctx.idf[i] * saturation(w, dl, ctx.avgdl);
                }
            }
            let micros = quantize(score);
            if ctx.min_score > 0 && micros < ctx.min_score {
                continue;
            }
            out.matches += 1;
            heap.offer(OrdKey {
                micros: Reverse(micros),
                path: ranks[0].rank(d),
                start: start_col.first(d).unwrap_or(0),
                end: end_col.first(d).unwrap_or(0),
                kind: ranks[1].rank(d),
                name: ranks[2].rank(d),
                qn: ranks[3].rank(d),
                sig: ranks[4].rank(d),
                node: ranks[S_NODE].rank(d),
                search_key: ranks[6].rank(d),
                doc: d,
            });
        }
    }
    out.dropped = heap.dropped;
    out.hits = resolve(&ranks, heap)?;
    Ok(out)
}

/// Every block some term holds, with the sum of the terms' upper bounds,
/// ordered by bound descending (block ascending on ties). A plain term's
/// bound comes from the summary; a phrase term's from its exact list.
fn bounds(ctx: &Ctx, s: &Summary, a: &SegA, dl_of: &impl Fn(DocId) -> u64) -> Vec<(u32, f64)> {
    let mut parts: Vec<(u32, f64)> = Vec::new();
    for (i, t) in ctx.terms.iter().enumerate() {
        if let Some(list) = &a.phrase[i] {
            let mut j = 0;
            while j < list.len() {
                let b = list[j].0 >> BLOCK_BITS;
                let (mut w, mut l) = (0f64, u64::MAX);
                while j < list.len() && list[j].0 >> BLOCK_BITS == b {
                    w = w.max(list[j].1);
                    l = l.min(dl_of(list[j].0));
                    j += 1;
                }
                parts.push((b, ctx.idf[i] * saturation(w, l, ctx.avgdl)));
            }
            continue;
        }
        let Some(e) = s.term(t.tokens[0].as_bytes()) else { continue };
        for be in s.blocks(&e) {
            let ub = if be.wtf_max == UNBOUNDED_WTF {
                f64::INFINITY
            } else {
                let l = match ctx.rule {
                    Rule::Total => be.min_len_total,
                    Rule::Weighted => be.min_len_weighted,
                };
                ctx.idf[i] * saturation(f64::from(be.wtf_max), u64::from(l), ctx.avgdl)
            };
            parts.push((be.block, ub));
        }
    }
    parts.sort_unstable_by_key(|p| p.0);
    let mut blocks: Vec<(u32, f64)> = Vec::new();
    for (b, ub) in parts {
        match blocks.last_mut() {
            Some(last) if last.0 == b => last.1 += ub,
            _ => blocks.push((b, ub)),
        }
    }
    blocks.retain(|b| b.1 > 0.0);
    blocks.sort_by(|x, y| y.1.total_cmp(&x.1).then(x.0.cmp(&y.0)));
    blocks
}

/// The heap's entries with their strings, one sorted dictionary pass per column.
fn resolve(ranks: &[RankCol], heap: Heap) -> Result<Vec<(Hit, u64, bool)>> {
    let entries: Vec<Slot> = heap.slots.into_values().collect();
    let mut strings: Vec<Vec<String>> = vec![vec![String::new(); entries.len()]; STRS.len()];
    for (k, rc) in ranks.iter().enumerate() {
        let Some(col) = &rc.col else { continue };
        let mut ords: Vec<(u64, usize)> =
            entries.iter().enumerate().filter_map(|(i, s)| rc.ord(s.key.doc).map(|o| (o, i))).collect();
        ords.sort_unstable();
        let mut uniq: Vec<u64> = ords.iter().map(|x| x.0).collect();
        uniq.dedup();
        let mut vals = Vec::with_capacity(uniq.len());
        col.dictionary().sorted_ords_to_term_cb(uniq.iter().copied(), |b| {
            vals.push(String::from_utf8_lossy(b).into_owned());
            Ok(())
        })?;
        if vals.len() != uniq.len() {
            return Err(Error::corrupt("a string column's dictionary is missing ordinals"));
        }
        let mut u = 0;
        for (o, i) in ords {
            while uniq[u] != o {
                u += 1;
            }
            strings[k][i].clone_from(&vals[u]);
        }
    }
    Ok(entries
        .iter()
        .enumerate()
        .map(|(i, s)| {
            let mut take = |k: usize| std::mem::take(&mut strings[k][i]);
            let hit = Hit {
                micros: s.key.micros.0,
                path: take(0),
                start: s.key.start,
                end: s.key.end,
                kind: take(1),
                name: take(2),
                qn: take(3),
                sig: take(4),
                node: take(5),
                search_key: take(6),
                file: take(7),
            };
            (hit, s.count, s.complete)
        })
        .collect())
}
