//! One page of the lexical tier (docs/adr/ADR-0013-search-index.md decisions
//! 2 and 5): our BM25F over the view's alive documents, the exact top
//! depth+1 folded results per segment found by block-max pruning, folded
//! across segments under the content-only comparator.
//!
//! Order. A result is a fold group (a node, or a file and start byte) ranked by
//! its best document under `cmp_hits`: score descending, then the content key.
//! The key ends in the node id and the search key, so two documents tie on it
//! only when they are one group's, and the order of results is total.
//!
//! Resume. A request may name a position `after`: the score and the document of
//! a result already served. The page is then the results whose best document
//! sorts strictly after that document's key, which is the suffix of the deep
//! read past it. A document at or before the position is never a candidate, and
//! it marks its group as served: a group whose best document lies at or before
//! the position was served there, whatever its other documents score.
//!
//! Exactness. A block is skipped only when its quantized upper bound (with a
//! margin over the floating sums) is strictly below the score of the worst
//! entry of a full heap. The heap holds documents after the position only, so
//! that score is at most the position's, and a skipped block holds no document
//! at or before the position: every group the position marks is marked. Within
//! a segment the heap ends holding the cap best candidate groups, each at its
//! best document, so every candidate group the segment saw that sorts at or
//! before the heap's worst entry is in it. A segment that turned a group away
//! or skipped a block (truncated) therefore answers exactly up to its worst
//! entry, and the union of the segments answers exactly up to the least worst
//! entry of the truncated ones (the cut). Groups another segment marked are
//! removed, and when fewer than depth+1 results remain above the cut, which
//! only marked groups holding heap places can cause, the segments are scored
//! again with twice the heap. `more` is then whether a (depth+1)-th folded
//! result exists.
//!
//! Counts. `matches` counts the matching visible documents that pass the
//! filters; it is exact when no block was skipped. A
//! hit's occurrence count is exact when no block was skipped and, in every
//! segment whose heap ever turned a document away, the hit's entry was
//! admitted before the first turn-away and never left (so it saw every one
//! of its documents); otherwise it is a lower bound.

use std::cmp::{Ordering, Reverse};
use std::collections::{BTreeSet, HashMap, HashSet};
use std::sync::atomic::{AtomicBool, AtomicU64, Ordering as AtomicOrdering};
use std::sync::Arc;

use tantivy::columnar::{Column, StrColumn};
use tantivy::postings::{Postings, SegmentPostings, TermInfo};
use tantivy::schema::IndexRecordOption;
use tantivy::{DocId, DocSet, Executor, InvertedIndexReader, SegmentReader, Term, TERMINATED};

use crate::codec::{put_str, put_uvarint, put_varint, Reader};
use crate::error::{Error, Result, CX_CANCELED, CX_NO_POSITION, CX_TOO_MANY_TERMS};
use crate::schema::{Fields, END, START, STRS, S_KIND, S_NODE, S_PATH};
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
    rule: Rule,
    max_terms: u64,
    ignore_summaries: bool,
    /// The resume position: a served result's score and its document's id.
    after: Option<(i64, u64)>,
    /// The bytes the page may hold (0: unbounded), what the caller holds per
    /// result beyond its strings, and an upper bound on the groups the resume
    /// position marks (Page::new_marks of the page before).
    max_bytes: u64,
    result_bytes: u64,
    marked_bound: u64,
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
    let rule = match r.byte()? {
        0 => Rule::Total,
        1 => Rule::Weighted,
        v => return Err(Error::invalid(format!("unknown length rule {v}"))),
    };
    let max_terms = r.uvarint()?;
    let max_bytes = r.uvarint()?;
    let result_bytes = r.uvarint()?;
    let marked_bound = r.uvarint()?;
    let flags = r.byte()?;
    if flags & !3 != 0 {
        return Err(Error::invalid(format!("unknown request flags {flags}")));
    }
    let after = if flags & 2 != 0 { Some((r.varint()?, r.uvarint()?)) } else { None };
    r.finish()?;
    Ok(Request {
        query,
        kinds,
        prefixes,
        depth,
        rule,
        max_terms,
        ignore_summaries: flags & 1 != 0,
        after,
        max_bytes,
        result_bytes,
        marked_bound,
    })
}

/// Answers one page (the cx_view_search layouts).
pub fn search(view: &View, req: &[u8], cancel: Cancel) -> Result<Vec<u8>> {
    search_reading(view, req, cancel, Read::Keys)
}

/// The search, its merge first reading what `first` names of each result.
/// Read::Keys is the search; Read::Strings is the one-result-at-a-time read a
/// bounded page falls back to, which the tests compare it with.
fn search_reading(view: &View, req: &[u8], cancel: Cancel, first: Read) -> Result<Vec<u8>> {
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

    let after = match req.after {
        Some((micros, doc)) => Some(read_after(view, &fields, micros, doc)?),
        None => None,
    };

    let scored_any = AtomicBool::new(false);
    let mut cap = req.depth.saturating_add(1);
    // The memory bound. A page's allocations that grow with its depth or its
    // position are the heaps of the segments scored at once, every segment's
    // run, the marked groups and the results served; each is charged at its
    // size (the constants below), and a bounded page never holds more than
    // max_bytes of them. The heap is sized to fit beside the marked groups'
    // bound, never below two places, so every page makes progress.
    let live = view.segs.iter().filter(|s| s.alive.docs > 0).count() as u64;
    let heaps = live.min(view.index.search_threads());
    let place = live * RUN_ENTRY_BYTES + heaps * HEAP_PLACE_BYTES;
    let bounded = req.max_bytes > 0;
    if bounded && place > 0 {
        let room = req.max_bytes.saturating_sub(req.marked_bound.saturating_mul(MARKED_BYTES));
        let fit = usize::try_from(room / place).unwrap_or(usize::MAX);
        cap = cap.min(fit.max(2));
    }
    let (mut blocks_scored, mut docs_scored) = (0u64, 0u64);
    loop {
        let ctx = Ctx {
            fields: &fields,
            terms: &terms,
            idf: &idfs,
            avgdl,
            rule: req.rule,
            cap,
            kinds: &req.kinds,
            prefixes: &req.prefixes,
            ignore_summaries: req.ignore_summaries,
            after: after.as_ref(),
            cancel,
            scored_any: &scored_any,
        };
        let jobs: Vec<_> = segs.iter().copied().zip(phase_a.iter()).collect();
        let fruits = view.index.executor.map(|((seg, sv), a)| Ok(score_segment(&ctx, seg, sv, a)), jobs.into_iter())?;
        let fruits = fruits.into_iter().collect::<Result<Vec<_>>>()?;
        let canceled = fruits.iter().any(|f| f.canceled);
        if canceled && !scored_any.load(AtomicOrdering::Relaxed) {
            return Err(Error::new(CX_CANCELED, "the search was canceled before any block was scored"));
        }

        // Merge the segments' runs.
        let skipped = fruits.iter().any(|f| f.skipped) || canceled;
        let dropping = fruits.iter().filter(|f| f.dropped).count() as u32;
        let cut: Option<Hit> = fruits.iter().filter_map(|f| f.boundary.as_ref()).min_by(|x, y| cmp_hits(x, y)).cloned();
        page.matches = 0;
        page.blocks_total = 0;
        page.marked = 0;
        for f in &fruits {
            page.matches += f.matches;
            page.blocks_total += f.blocks_total;
            page.marked += f.marked.len() as u64;
            blocks_scored += f.blocks_scored;
            docs_scored += f.docs_scored;
        }
        let marked_bytes = page.marked.saturating_mul(MARKED_BYTES);
        let runs_bytes = live.saturating_mul(cap as u64).saturating_mul(RUN_ENTRY_BYTES);
        let scoring_bytes = place.saturating_mul(cap as u64).saturating_add(marked_bytes);
        let budget = if bounded { req.max_bytes.saturating_sub(runs_bytes + marked_bytes) } else { u64::MAX };
        let mut m = merge(&fruits, cut.as_ref(), req.depth, first, budget, req.result_bytes)?;
        if bounded && first == Read::Keys {
            // A bounded page reads its results' strings in one sorted pass per
            // column while their charges fit the bound. A key charge is never
            // more than the full one, so when the full charges of the results
            // the keys merge took fit, a merge reading each result's strings as
            // it meets it takes the same results and stops no later; only when
            // they do not fit does that merge run, stopping where the bound
            // falls. The page is the same either way.
            let n = m.out.len().min(req.depth);
            let fit = resolve(&view.index.executor, &fruits, &mut m.out[..n], Some((budget, req.result_bytes)))?;
            if !fit {
                drop(m);
                m = merge(&fruits, cut.as_ref(), req.depth, Read::Strings, budget, req.result_bytes)?;
            }
        }
        if !m.stopped && m.out.len() <= req.depth && cut.is_some() && !canceled {
            // Marked groups held run places: fewer than depth+1 results are
            // known exactly, and more may exist past the cut. The heap doubles
            // while it fits the bound; a page that knows no result yet doubles
            // regardless, so the read always advances.
            let next = cap.saturating_mul(2);
            let fits = !bounded || place.saturating_mul(next as u64).saturating_add(marked_bytes) <= req.max_bytes;
            if fits || m.out.is_empty() {
                cap = next;
                continue;
            }
        }
        // A page the bound ended early holds an exact prefix of the results
        // after the position, and says more may follow.
        let short = !canceled && (m.stopped || (m.out.len() <= req.depth && cut.is_some()));
        page.more = m.out.len() > req.depth || short;
        let mut merged = m.out;
        merged.truncate(req.depth);
        page.new_marks = page.marked + merged.last().map_or(0, |x| x.pops);
        resolve(&view.index.executor, &fruits, &mut merged, None)?;
        let spent: u64 = merged.iter().filter_map(|x| x.hit.as_ref()).map(|h| charge(h, req.result_bytes)).sum();
        page.bytes = scoring_bytes.max(runs_bytes + marked_bytes + spent);
        page.heaps = heaps;
        page.cap = cap as u64;
        page.canceled = canceled;
        page.matches_exact = !skipped;
        page.blocks_scored = blocks_scored;
        page.docs_scored = docs_scored;
        page.hits = finish(&view.index.executor, &fruits, merged)?
            .into_iter()
            .map(|(h, n, seen)| (h, n, !skipped && seen == dropping))
            .collect();
        return Ok(page.encode(terms.len()));
    }
}

/// What one heap place of a segment being scored holds: an OrdKey (a score
/// and nine u64 ranks plus a u32 doc: 88 bytes) in the BTreeSet, whose nodes
/// hold eleven keys in 984 bytes and are at least half full (at most 200 bytes
/// a key), and in the HashMap from the fold group (24 bytes) to its Slot (104
/// bytes), whose 128-byte buckets run at 7/8 load and double (at most 296 bytes
/// an entry); and the run entry the heap becomes before it is freed (24).
const HEAP_PLACE_BYTES: u64 = 200 + 296 + 24;
/// One place of a scored segment's run, held until the merge ends.
const RUN_ENTRY_BYTES: u64 = std::mem::size_of::<RunEntry>() as u64;
/// One group the resume position marked: a 24-byte entity in a HashSet whose
/// buckets run at 7/8 load and double.
const MARKED_BYTES: u64 = 58;
/// One served result beyond its strings: the met-group set (a 32-byte key, 76
/// bytes a bucket at worst), the merged entry (288), the count pass's entity
/// map (75) and count (16), the result twice as the page is assembled (240
/// each) and its encoded fields twice (80 each, the buffer grows by doubling).
const RESULT_BYTES: u64 = 76 + 288 + 75 + 16 + 240 + 240 + 160;
/// How many copies of a served result's strings are held: the library's hit,
/// its key in the met-group set and in the merged entry, and the encoded
/// buffer twice; and the caller's copy of the buffer and its decoded strings.
const RESULT_STRING_COPIES: u64 = 5 + 2;

/// One merged result: the segment and document of its best document, its
/// fold key (None when one segment alone has a run and marks, so no other
/// segment can hold its group), its strings when they were read during the
/// merge, and how many run entries the merge had taken when it was met (every
/// one sorts at or before it).
struct Merged {
    seg: usize,
    doc: DocId,
    micros: i64,
    key: Option<GlobalKey>,
    hit: Option<Hit>,
    pops: u64,
}

/// The merge's answer: the results, the bytes they were charged, and whether
/// the bound ended the merge before depth+1 results.
struct Merge {
    out: Vec<Merged>,
    spent: u64,
    stopped: bool,
}

impl Hit {
    fn string_bytes(&self) -> u64 {
        [&self.path, &self.kind, &self.name, &self.qn, &self.sig, &self.node, &self.search_key, &self.file]
            .iter()
            .map(|s| s.len() as u64)
            .sum()
    }
}

/// What one served result is charged.
fn charge(hit: &Hit, result_bytes: u64) -> u64 {
    RESULT_BYTES + result_bytes + RESULT_STRING_COPIES * hit.string_bytes()
}

/// What a merge reads of each result it meets: its fold key alone, or all its
/// strings.
#[derive(Clone, Copy, PartialEq)]
enum Read {
    Keys,
    Strings,
}

/// The first `depth`+1 folded results in global order, by a k-way merge of
/// the segments' runs: each run is already in comparator order, so only the
/// heads are compared, by score alone unless two heads tie on it, when their
/// strings are read and the content key decides. Merging stops at the cut,
/// past which a truncated segment is not exact, and before a result whose
/// charge would pass `budget` (the first result is always taken). A group's
/// first result met is its best document; a later one of the same group is
/// skipped, and so is a group some segment marked as served at or before the
/// resume position.
///
/// Read::Strings reads each result's strings as it is met and charges them
/// all. Read::Keys reads only the fold key, from one column (two for a result
/// without a node), and none when one segment alone has a run and marks, and
/// charges the key's strings alone, which is never more than the full charge;
/// `resolve` then reads the strings one sorted pass per column and segment.
fn merge(
    fruits: &[SegFruit],
    cut: Option<&Hit>,
    depth: usize,
    read: Read,
    budget: u64,
    result_bytes: u64,
) -> Result<Merge> {
    let alone = fruits.iter().filter(|f| !f.run.is_empty() || !f.marked.is_empty()).count() <= 1;
    let mut at = vec![0usize; fruits.len()];
    let mut heads: Vec<Option<Hit>> = vec![None; fruits.len()];
    let mut met: HashSet<GlobalKey> = HashSet::new();
    let mut m = Merge { out: Vec::new(), spent: 0, stopped: false };
    let mut pops = 0u64;
    while m.out.len() <= depth {
        let mut best: Option<usize> = None;
        for i in 0..fruits.len() {
            let Some(e) = fruits[i].run.get(at[i]) else { continue };
            let Some(b) = best else {
                best = Some(i);
                continue;
            };
            let eb = &fruits[b].run[at[b]];
            let better = match e.micros.cmp(&eb.micros) {
                Ordering::Greater => true,
                Ordering::Less => false,
                Ordering::Equal => {
                    head(fruits, &at, &mut heads, i)?;
                    head(fruits, &at, &mut heads, b)?;
                    let (x, y) = (heads[i].as_ref().expect("read"), heads[b].as_ref().expect("read"));
                    cmp_hits(x, y) == Ordering::Less
                }
            };
            if better {
                best = Some(i);
            }
        }
        let Some(i) = best else { break };
        let e = fruits[i].run[at[i]];
        if let Some(c) = cut {
            let beyond = match e.micros.cmp(&c.micros) {
                Ordering::Less => true,
                Ordering::Greater => false,
                Ordering::Equal => {
                    head(fruits, &at, &mut heads, i)?;
                    cmp_hits(heads[i].as_ref().expect("read"), c) == Ordering::Greater
                }
            };
            if beyond {
                break;
            }
        }
        let cols = fruits[i].cols.as_ref().expect("a segment with a run has its columns");
        let mut hit = heads[i].take();
        if read == Read::Strings && hit.is_none() {
            hit = Some(cols.hit(e.doc, e.micros)?);
        }
        let key = match (&hit, alone) {
            (_, true) => None,
            (Some(h), false) => Some(h.key()),
            (None, false) => Some(cols.key(e.doc)?),
        };
        if key.as_ref().is_some_and(|k| met.contains(k)) {
            at[i] += 1;
            pops += 1;
            continue;
        }
        let mut served = false;
        if let Some(k) = &key {
            for f in fruits.iter().filter(|f| !f.marked.is_empty()) {
                let ranks = &f.cols.as_ref().expect("a segment that marked has its columns").ranks;
                if local_entity(ranks, k)?.is_some_and(|x| f.marked.contains(&x)) {
                    served = true;
                    break;
                }
            }
        }
        if !served {
            let c = match (&hit, &key) {
                (Some(h), _) => charge(h, result_bytes),
                (None, Some(GlobalKey::Node(n) | GlobalKey::File(n, _))) => {
                    RESULT_BYTES + result_bytes + RESULT_STRING_COPIES * n.len() as u64
                }
                (None, None) => RESULT_BYTES + result_bytes,
            };
            if !m.out.is_empty() && m.spent.saturating_add(c) > budget {
                m.stopped = true;
                break;
            }
            m.spent += c;
        }
        at[i] += 1;
        pops += 1;
        if let Some(k) = &key {
            met.insert(k.clone());
        }
        if !served {
            m.out.push(Merged { seg: i, doc: e.doc, micros: e.micros, key, hit, pops });
        }
    }
    Ok(m)
}

/// Reads the strings of segment i's head, once.
fn head(fruits: &[SegFruit], at: &[usize], heads: &mut [Option<Hit>], i: usize) -> Result<()> {
    if heads[i].is_none() {
        let e = fruits[i].run[at[i]];
        let cols = fruits[i].cols.as_ref().expect("a segment with a run has its columns");
        heads[i] = Some(cols.hit(e.doc, e.micros)?);
    }
    Ok(())
}

/// Reads the strings of the results the merge did not read, one sorted
/// dictionary pass per column, the segments in parallel as they were scored.
/// Under a bound (`bound` is the budget and the caller's bytes a result) it
/// gives up, returning false, as soon as the results' full charges pass the
/// budget while more than one result is held: exactly when a merge reading
/// each result's strings as it meets it would have stopped before the last of
/// them. Every read adds its charge to one shared total, so the outcome does
/// not depend on the order the segments run in.
fn resolve(exec: &Executor, fruits: &[SegFruit], merged: &mut [Merged], bound: Option<(u64, u64)>) -> Result<bool> {
    let mut spent = 0u64;
    let mut limit = u64::MAX;
    if let Some((budget, rb)) = bound {
        for m in merged.iter() {
            spent += m.hit.as_ref().map_or(RESULT_BYTES + rb, |h| charge(h, rb));
        }
        if merged.len() > 1 {
            limit = budget;
        }
    }
    if spent > limit {
        return Ok(false);
    }
    let mut jobs: Vec<(usize, Vec<usize>)> = Vec::new();
    for s in 0..fruits.len() {
        let want: Vec<usize> = (0..merged.len()).filter(|&i| merged[i].seg == s && merged[i].hit.is_none()).collect();
        if !want.is_empty() {
            jobs.push((s, want));
        }
    }
    if jobs.is_empty() {
        return Ok(true);
    }
    let spent = AtomicU64::new(spent);
    let docs: Vec<Vec<(DocId, i64)>> =
        jobs.iter().map(|(_, want)| want.iter().map(|&i| (merged[i].doc, merged[i].micros)).collect()).collect();
    let read = exec.map(
        |(s, d): (usize, &Vec<(DocId, i64)>)| {
            let cols = fruits[s].cols.as_ref().expect("a segment with a run has its columns");
            Ok(cols.hits(d, &spent, limit))
        },
        jobs.iter().map(|j| j.0).zip(docs.iter()),
    )?;
    let mut fit = true;
    for ((_, want), r) in jobs.into_iter().zip(read) {
        match r? {
            Some(hits) => {
                for (i, h) in want.into_iter().zip(hits) {
                    merged[i].hit = Some(h);
                }
            }
            None => fit = false,
        }
    }
    Ok(fit)
}

/// The served results with their occurrence counts. A result's count sums
/// every run's entry of its group, found by the group's entity in that
/// segment's ranks, and `seen` counts the dropping segments whose entry of it
/// was complete. The segments are counted in parallel as they were scored:
/// each looks the groups' entities up once and matches its run by its
/// documents' ranks.
fn finish(exec: &Executor, fruits: &[SegFruit], served: Vec<Merged>) -> Result<Vec<(Hit, u64, u32)>> {
    let served: Vec<(Merged, Hit)> = served
        .into_iter()
        .map(|mut m| {
            let h = m.hit.take().expect("every served result was read");
            (m, h)
        })
        .collect();
    let segs = fruits.iter().enumerate().filter(|(_, f)| f.cols.is_some() && !f.run.is_empty()).map(|(s, _)| s);
    let per = exec.map(|s: usize| Ok(count_segment(fruits, s, &served)), segs)?;
    let mut counts = vec![(0u64, 0u32); served.len()];
    for r in per {
        for (i, n, seen) in r? {
            counts[i].0 += n;
            counts[i].1 += seen;
        }
    }
    Ok(served.into_iter().zip(counts).map(|((_, h), (n, seen))| (h, n, seen)).collect())
}

/// One segment's share of the served results' counts: (result, count, seen)
/// for each of its run's entries in a served group.
fn count_segment(fruits: &[SegFruit], s: usize, served: &[(Merged, Hit)]) -> Result<Vec<(usize, u64, u32)>> {
    let f = &fruits[s];
    let cols = f.cols.as_ref().expect("a segment with a run has its columns");
    let mut ents: HashMap<Entity, usize> = HashMap::with_capacity(served.len());
    for (i, (m, h)) in served.iter().enumerate() {
        let e = if m.seg == s {
            Some(cols.entity(m.doc))
        } else {
            match &m.key {
                Some(k) => local_entity(&cols.ranks, k)?,
                None => local_entity(&cols.ranks, &h.key())?,
            }
        };
        if let Some(e) = e {
            ents.insert(e, i);
        }
    }
    let mut counts = Vec::with_capacity(ents.len());
    for e in &f.run {
        if let Some(&i) = ents.get(&cols.entity(e.doc)) {
            counts.push((i, e.count, u32::from(f.dropped && e.complete)));
        }
    }
    Ok(counts)
}

/// The position a request resumes after, read back from its document: the
/// full comparator key of the result last served. The document is found by its
/// id among the view's alive documents; an id the view does not hold is
/// CX_NO_POSITION.
struct After {
    micros: i64,
    /// The string columns in STRS order.
    strs: Vec<Vec<u8>>,
    start: u64,
    end: u64,
}

fn read_after(view: &View, fields: &Fields, micros: i64, doc_id: u64) -> Result<After> {
    let term = Term::from_field_u64(fields.doc_id, doc_id);
    for (seg, sv) in view.searcher.segment_readers().iter().zip(&view.segs) {
        let inv = seg.inverted_index(fields.doc_id)?;
        let Some(mut p) = inv.read_postings(&term, IndexRecordOption::Basic)? else { continue };
        let mut d = p.doc();
        while d != TERMINATED {
            if sv.alive.alive(d) {
                let mut strs = Vec::with_capacity(STRS.len());
                for name in STRS {
                    let rc = RankCol::open(seg, name)?;
                    let mut b = Vec::new();
                    if let (Some(c), Some(o)) = (&rc.col, rc.ord(d)) {
                        if !c.ord_to_bytes(o, &mut b)? {
                            return Err(Error::corrupt("a string column's dictionary is missing an ordinal"));
                        }
                    }
                    strs.push(b);
                }
                let ff = seg.fast_fields();
                let start = ff.u64(START)?.first(d).unwrap_or(0);
                let end = ff.u64(END)?.first(d).unwrap_or(0);
                return Ok(After { micros, strs, start, end });
            }
            d = p.advance();
        }
    }
    Err(Error::new(CX_NO_POSITION, "the resume position names no document of this view"))
}

/// The position in one segment's ordinals. Each string is coded against the
/// segment's sorted dictionary as twice the rank of its value when the segment
/// holds the value, else as the odd number between the ranks around it, so a
/// document's string, coded as twice its rank, compares with it exactly as the
/// strings compare.
struct SegAfter {
    micros: i64,
    strs: [u64; 7],
    start: u64,
    end: u64,
}

impl SegAfter {
    fn new(a: &After, ranks: &[RankCol]) -> Result<SegAfter> {
        let mut strs = [0u64; 7];
        for (i, v) in strs.iter_mut().enumerate() {
            *v = rank_code(&ranks[i], &a.strs[i])?;
        }
        Ok(SegAfter { micros: a.micros, strs, start: a.start, end: a.end })
    }

    /// Whether a document sorts at or before the position.
    fn holds(&self, k: &OrdKey) -> bool {
        let doc = (
            Reverse(k.micros.0),
            2 * k.path,
            k.start,
            k.end,
            2 * k.kind,
            2 * k.name,
            2 * k.qn,
            2 * k.sig,
            2 * k.node,
            2 * k.search_key,
        );
        let s = &self.strs;
        let pos = (Reverse(self.micros), s[0], self.start, self.end, s[1], s[2], s[3], s[4], s[5], s[6]);
        doc <= pos
    }
}

/// A string's code in one segment (SegAfter): RankCol ranks the empty string 0
/// and every value of the dictionary above it in byte order.
fn rank_code(rc: &RankCol, v: &[u8]) -> Result<u64> {
    if v.is_empty() {
        return Ok(0);
    }
    let Some(c) = &rc.col else { return Ok(1) };
    let g = first_ord_ge(c, v)?;
    let mut b = Vec::new();
    let present = g < c.num_terms() as u64 && c.ord_to_bytes(g, &mut b)? && b == v;
    let rank = g + rc.shift;
    Ok(if present { 2 * rank } else { 2 * rank - 1 })
}

/// A fold group inside one segment: the node's rank, else the file's path rank
/// and the start byte.
type Entity = (u8, u64, u64);

/// A result's group in a segment's ranks, or None when that segment holds no
/// document of it.
fn local_entity(ranks: &[RankCol], key: &GlobalKey) -> Result<Option<Entity>> {
    let (rc, text, start) = match key {
        GlobalKey::Node(n) => (&ranks[S_NODE], n, None),
        GlobalKey::File(p, st) => (&ranks[S_PATH], p, Some(*st)),
    };
    let Some(c) = &rc.col else { return Ok(None) };
    let Some(o) = c.dictionary().term_ord(text.as_bytes())? else { return Ok(None) };
    Ok(Some(match start {
        Some(st) => (1, o + rc.shift, st),
        None => (0, o + rc.shift, 0),
    }))
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
    /// Segments scored at once, the heap places each held, and the groups the
    /// resume position marked: what the page's memory scaled with.
    heaps: u64,
    cap: u64,
    marked: u64,
    /// An upper bound on the groups a page resuming after this page's last
    /// result marks: this page's marked groups plus every run entry the merge
    /// took up to that result, since each sorts at or before it.
    new_marks: u64,
    /// The bytes the page's growing allocations peaked at, as charged.
    bytes: u64,
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
        for v in [
            self.segments,
            self.segments_without_summary,
            self.blocks_total,
            self.blocks_scored,
            self.docs_scored,
            self.heaps,
            self.cap,
            self.marked,
            self.new_marks,
            self.bytes,
        ] {
            put_uvarint(&mut b, v);
        }
        put_uvarint(&mut b, self.hits.len() as u64);
        for (h, n, exact) in &self.hits {
            put_uvarint(&mut b, h.doc_id);
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
#[derive(Clone)]
struct Hit {
    /// The doc id of the document the result is, which a later page names as
    /// its resume position.
    doc_id: u64,
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

#[derive(Clone, Hash, PartialEq, Eq)]
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
            if let Some(p) =
                inv.read_postings(&Term::from_field_text(fields.text[fi], tok), IndexRecordOption::Basic)?
            {
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
fn phrase_list(
    invs: &[Arc<InvertedIndexReader>],
    fields: &Fields,
    tokens: &[String],
    sv: &SegView,
) -> Result<Vec<(DocId, f64)>> {
    let mut all: Vec<(DocId, u64)> = Vec::new();
    let mut pos: Vec<Vec<u32>> = vec![Vec::new(); tokens.len()];
    for (fi, inv) in invs.iter().enumerate() {
        let mut ps = Vec::with_capacity(tokens.len());
        for tok in tokens {
            match inv
                .read_postings(&Term::from_field_text(fields.text[fi], tok), IndexRecordOption::WithFreqsAndPositions)?
            {
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
    kinds: &'a [String],
    prefixes: &'a [String],
    ignore_summaries: bool,
    after: Option<&'a After>,
    cancel: Cancel,
    scored_any: &'a AtomicBool,
}

struct SegFruit {
    /// The segment's candidate groups at their best documents, in comparator
    /// order, without strings: a run entry is 24 bytes, and strings are read
    /// only for the results the merge serves or compares on a tied score.
    run: Vec<RunEntry>,
    /// The heap's worst entry when the segment is truncated: it turned a group
    /// away or skipped a block, so it answers exactly only up to this entry.
    boundary: Option<Hit>,
    /// The groups with a document at or before the resume position, in this
    /// segment's ranks, which `ranks` resolves another segment's results into.
    marked: HashSet<Entity>,
    /// The segment's columns, which resolve a run entry's strings and another
    /// segment's results into this segment's ranks; None for an empty segment.
    cols: Option<SegCols>,
    dropped: bool,
    skipped: bool,
    canceled: bool,
    matches: u64,
    blocks_total: u64,
    blocks_scored: u64,
    docs_scored: u64,
}

/// One candidate group of a segment at its best document.
#[derive(Clone, Copy)]
struct RunEntry {
    micros: i64,
    count: u64,
    doc: DocId,
    complete: bool,
}

/// The columns a segment's results are read from.
struct SegCols {
    ranks: Vec<RankCol>,
    ids: Column<u64>,
    start: Column<u64>,
    end: Column<u64>,
}

impl SegCols {
    /// One string of a document.
    fn text(&self, k: usize, d: DocId) -> Result<String> {
        let rc = &self.ranks[k];
        let (Some(c), Some(o)) = (&rc.col, rc.ord(d)) else { return Ok(String::new()) };
        let mut b = Vec::new();
        if !c.ord_to_bytes(o, &mut b)? {
            return Err(Error::corrupt("a string column's dictionary is missing an ordinal"));
        }
        Ok(String::from_utf8_lossy(&b).into_owned())
    }

    /// A document's fold group in this segment's ranks (OrdKey::entity).
    fn entity(&self, d: DocId) -> Entity {
        let node = self.ranks[S_NODE].rank(d);
        if node != 0 {
            (0, node, 0)
        } else {
            (1, self.ranks[S_PATH].rank(d), self.start.first(d).unwrap_or(0))
        }
    }

    /// A document's fold key: its node, else its file's path and start byte.
    fn key(&self, d: DocId) -> Result<GlobalKey> {
        let node = self.text(S_NODE, d)?;
        Ok(if node.is_empty() {
            GlobalKey::File(self.text(S_PATH, d)?, self.start.first(d).unwrap_or(0))
        } else {
            GlobalKey::Node(node)
        })
    }

    /// Documents as results, their strings read by one sorted dictionary pass
    /// per column, which decodes each dictionary block once for every ordinal
    /// it holds.
    /// Each string is charged to `spent` at its copies for every document that
    /// holds it, and the read gives up (None) before `spent` passes `limit`.
    fn hits(&self, docs: &[(DocId, i64)], spent: &AtomicU64, limit: u64) -> Result<Option<Vec<Hit>>> {
        let mut strs: Vec<Vec<String>> = vec![vec![String::new(); docs.len()]; STRS.len()];
        for (k, rc) in self.ranks.iter().enumerate() {
            let Some(col) = &rc.col else { continue };
            let mut ords: Vec<(u64, usize)> =
                docs.iter().enumerate().filter_map(|(i, &(d, _))| rc.ord(d).map(|o| (o, i))).collect();
            ords.sort_unstable();
            let mut uniq: Vec<(u64, u64)> = Vec::new();
            for &(o, _) in &ords {
                match uniq.last_mut() {
                    Some(u) if u.0 == o => u.1 += 1,
                    _ => uniq.push((o, 1)),
                }
            }
            let mut vals = Vec::with_capacity(uniq.len());
            let mut over = false;
            let pass = col.dictionary().sorted_ords_to_term_cb(uniq.iter().map(|u| u.0), |b| {
                let v = String::from_utf8_lossy(b);
                let c = RESULT_STRING_COPIES.saturating_mul(v.len() as u64).saturating_mul(uniq[vals.len()].1);
                if spent.fetch_add(c, AtomicOrdering::Relaxed).saturating_add(c) > limit {
                    over = true;
                    return Err(std::io::Error::other("the bound"));
                }
                vals.push(v.into_owned());
                Ok(())
            });
            if over {
                return Ok(None);
            }
            pass?;
            if vals.len() != uniq.len() {
                return Err(Error::corrupt("a string column's dictionary is missing ordinals"));
            }
            let mut u = 0;
            for (o, i) in ords {
                while uniq[u].0 != o {
                    u += 1;
                }
                strs[k][i].clone_from(&vals[u]);
            }
        }
        Ok(Some(
            docs.iter()
                .enumerate()
                .map(|(i, &(d, micros))| {
                    let mut take = |k: usize| std::mem::take(&mut strs[k][i]);
                    Hit {
                        doc_id: self.ids.first(d).unwrap_or(0),
                        micros,
                        path: take(0),
                        start: self.start.first(d).unwrap_or(0),
                        end: self.end.first(d).unwrap_or(0),
                        kind: take(1),
                        name: take(2),
                        qn: take(3),
                        sig: take(4),
                        node: take(5),
                        search_key: take(6),
                        file: take(7),
                    }
                })
                .collect(),
        ))
    }

    /// A document as a result, its strings read one by one.
    fn hit(&self, d: DocId, micros: i64) -> Result<Hit> {
        Ok(Hit {
            doc_id: self.ids.first(d).unwrap_or(0),
            micros,
            path: self.text(0, d)?,
            start: self.start.first(d).unwrap_or(0),
            end: self.end.first(d).unwrap_or(0),
            kind: self.text(1, d)?,
            name: self.text(2, d)?,
            qn: self.text(3, d)?,
            sig: self.text(4, d)?,
            node: self.text(5, d)?,
            search_key: self.text(6, d)?,
            file: self.text(7, d)?,
        })
    }
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

fn score_segment(ctx: &Ctx, seg: &SegmentReader, sv: &SegView, a: &SegA) -> Result<SegFruit> {
    let f = ctx.fields;
    let nt = ctx.terms.len();
    let max_doc = seg.max_doc();
    let mut out = SegFruit {
        run: Vec::new(),
        boundary: None,
        marked: HashSet::new(),
        cols: None,
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
    let after = ctx.after.map(|a| SegAfter::new(a, &ranks)).transpose()?;
    let ff = seg.fast_fields();
    let ids: Column<u64> = ff.u64(crate::schema::DOC_ID)?;
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
    let ordered: Option<Vec<(u32, f64)>> = summary.map(|s| bounds(ctx, s, a, &dl_of));
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
            out.matches += 1;
            let k = OrdKey {
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
            };
            if let Some(p) = &after {
                // A document at or before the position is no candidate, and
                // its group was served there: none of the group's documents is.
                if p.holds(&k) {
                    out.marked.insert(k.entity());
                    continue;
                }
                if out.marked.contains(&k.entity()) {
                    continue;
                }
            }
            heap.offer(k);
        }
    }
    out.dropped = heap.dropped;
    let truncated = heap.dropped || out.skipped;
    let cols = SegCols { ranks, ids, start: start_col, end: end_col };
    if truncated {
        if let Some(w) = heap.order.last() {
            out.boundary = Some(cols.hit(w.doc, w.micros.0)?);
        }
    }
    // A group marked after it took a heap place keeps that place until here;
    // the boundary above counts it, and the result is dropped now.
    out.run = Vec::with_capacity(heap.order.len());
    for k in &heap.order {
        let e = k.entity();
        if out.marked.contains(&e) {
            continue;
        }
        let slot = &heap.slots[&e];
        out.run.push(RunEntry { micros: k.micros.0, count: slot.count, doc: k.doc, complete: slot.complete });
    }
    drop(heap);
    out.cols = Some(cols);
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

#[cfg(test)]
mod tests {
    use super::*;
    use crate::index::{Index, Options, WRITER_MEMORY_FLOOR_PER_THREAD};

    /// One document: its doc id, node, path and body.
    type Doc<'a> = (u64, &'a str, &'a str, &'a str);

    /// A test index holding one segment per entry of `segments`, every
    /// document visible, and its view.
    struct Fixture {
        dir: std::path::PathBuf,
        view: View,
    }

    impl Drop for Fixture {
        fn drop(&mut self) {
            let _ = std::fs::remove_dir_all(&self.dir);
        }
    }

    fn fixture(name: &str, segments: &[&[Doc]]) -> Fixture {
        let dir = std::env::temp_dir().join(format!("cx-search-{name}-{}", std::process::id()));
        let _ = std::fs::remove_dir_all(&dir);
        let opts = Options {
            writable: true,
            writer_threads: 1,
            writer_memory_bytes: WRITER_MEMORY_FLOOR_PER_THREAD,
            merge_threads: 1,
            search_threads: 1,
            view_cache_bytes: 1 << 20,
        };
        let index = Index::open(&dir, opts).expect("open");
        let mut top = 0u64;
        for (i, docs) in segments.iter().enumerate() {
            let mut b = Vec::new();
            put_str(&mut b, &format!("unit-{i}"));
            put_uvarint(&mut b, docs.len() as u64);
            for &(id, node, path, body) in docs.iter() {
                top = top.max(id);
                put_uvarint(&mut b, id);
                put_str(&mut b, &format!("sk-{id}"));
                put_str(&mut b, node);
                put_str(&mut b, &format!("file-{path}"));
                put_str(&mut b, path);
                put_str(&mut b, "function");
                put_str(&mut b, node);
                put_str(&mut b, node);
                put_str(&mut b, "");
                put_uvarint(&mut b, 0);
                put_uvarint(&mut b, body.len() as u64);
                put_str(&mut b, body);
            }
            index.add(&b).expect("add");
            index.commit(b"").expect("commit");
        }
        let visible = vec![0xffu8; (top / 8 + 1) as usize];
        let view = View::open(Arc::new(index), 1, &visible).expect("view");
        assert_eq!(view.segs.len(), segments.len(), "one segment per commit");
        Fixture { dir, view }
    }

    /// A served hit: its doc id, node, path, score and occurrence count.
    #[derive(Debug, PartialEq)]
    struct Got {
        doc: u64,
        node: String,
        path: String,
        micros: i64,
        occurrences: u64,
    }

    fn page(f: &Fixture, depth: u64, after: Option<(i64, u64)>, max_bytes: u64) -> (bool, Vec<Got>) {
        page_reading(f, depth, after, max_bytes, Read::Keys)
    }

    fn page_reading(
        f: &Fixture,
        depth: u64,
        after: Option<(i64, u64)>,
        max_bytes: u64,
        first: Read,
    ) -> (bool, Vec<Got>) {
        let mut b = Vec::new();
        put_str(&mut b, "omega");
        put_uvarint(&mut b, 0);
        put_uvarint(&mut b, 0);
        put_uvarint(&mut b, depth);
        b.push(0);
        put_uvarint(&mut b, 0);
        put_uvarint(&mut b, max_bytes);
        put_uvarint(&mut b, 0);
        put_uvarint(&mut b, 0);
        match after {
            Some((micros, doc)) => {
                b.push(2);
                put_varint(&mut b, micros);
                put_uvarint(&mut b, doc);
            }
            None => b.push(0),
        }
        let out = search_reading(&f.view, &b, Cancel(std::ptr::null()), first).expect("search");
        let mut r = Reader::new(&out);
        r.uvarint().expect("terms");
        let more = r.byte().expect("more") != 0;
        r.byte().expect("canceled");
        r.uvarint().expect("matches");
        r.byte().expect("matches_exact");
        for _ in 0..10 {
            r.uvarint().expect("stats");
        }
        let n = r.uvarint().expect("hits");
        let mut hits = Vec::new();
        for _ in 0..n {
            let doc = r.uvarint().expect("doc");
            let _search_key = r.str().expect("search_key");
            let node = r.str().expect("node").to_string();
            let _file = r.str().expect("file");
            let path = r.str().expect("path").to_string();
            for _ in 0..4 {
                r.str().expect("string");
            }
            r.uvarint().expect("start");
            r.uvarint().expect("end");
            let micros = r.varint().expect("micros");
            let occurrences = r.uvarint().expect("occurrences");
            r.byte().expect("exact");
            hits.push(Got { doc, node, path, micros, occurrences });
        }
        r.finish().expect("the whole page was read");
        (more, hits)
    }

    /// Every hit of the answer, page by page at `depth` under `max_bytes`.
    fn walk(f: &Fixture, depth: u64, max_bytes: u64) -> Vec<Got> {
        let mut all: Vec<Got> = Vec::new();
        let mut after = None;
        for _ in 0..100 {
            let (more, hits) = page(f, depth, after, max_bytes);
            assert!(!hits.is_empty() || !more, "a page that says more holds a hit");
            if let Some(last) = hits.last() {
                after = Some((last.micros, last.doc));
            }
            all.extend(hits);
            if !more {
                return all;
            }
        }
        panic!("the walk did not end");
    }

    /// Tied scores across segments are ordered by the content key, whichever
    /// segment holds them, and a page stops at its depth with `more` set.
    /// Mutation: break a tie between two segments' heads by segment order.
    #[test]
    fn ties_across_segments_follow_the_content_key() {
        let a: &[Doc] = &[(1, "n1", "p/a1.go", "omega"), (3, "n3", "p/a3.go", "omega")];
        let b: &[Doc] = &[(2, "n2", "p/a2.go", "omega"), (4, "n4", "p/a4.go", "omega")];
        let f = fixture("ties", &[a, b]);
        let (more, hits) = page(&f, 3, None, 0);
        let paths: Vec<&str> = hits.iter().map(|h| h.path.as_str()).collect();
        assert_eq!(paths, ["p/a1.go", "p/a2.go", "p/a3.go"]);
        assert!(more, "a fourth result exists past the depth");
        assert!(hits.windows(2).all(|w| w[0].micros == w[1].micros), "the bodies tie on score");
        let last = hits.last().expect("hit");
        let (more, rest) = page(&f, 3, Some((last.micros, last.doc)), 0);
        assert!(!more);
        assert_eq!(rest.iter().map(|h| h.path.as_str()).collect::<Vec<_>>(), ["p/a4.go"]);
    }

    /// A group with documents in two segments is served once, at its best
    /// document, with every run's document counted, and a later page does not
    /// serve it again from the other segment.
    /// Mutation: fold groups per segment only, and the node is served twice.
    #[test]
    fn a_group_across_segments_is_served_once_with_its_count() {
        let a: &[Doc] = &[(1, "g", "p/g.go", "omega omega"), (2, "x", "p/x.go", "omega omega omega")];
        let b: &[Doc] = &[(3, "g", "p/g.go", "omega"), (4, "y", "p/y.go", "omega")];
        let f = fixture("group", &[a, b]);
        let all = walk(&f, 1, 0);
        let nodes: Vec<&str> = all.iter().map(|h| h.node.as_str()).collect();
        assert_eq!(nodes.iter().filter(|n| **n == "g").count(), 1, "g served once: {nodes:?}");
        assert_eq!(nodes.len(), 3, "x, g and y: {nodes:?}");
        let g = all.iter().find(|h| h.node == "g").expect("g");
        assert_eq!(g.doc, 1, "g is served at its best document");
        let (_, first) = page(&f, 3, None, 0);
        let g = first.iter().find(|h| h.node == "g").expect("g on one page");
        assert_eq!(g.occurrences, 2, "both segments' documents of g are counted");
    }

    /// A bound too small for the depth ends pages early, each an exact prefix
    /// saying more, and walking them serves the unbounded answer.
    /// Mutation: drop the bound's early stop, or serve the bounded page
    /// without `more`.
    #[test]
    fn a_bounded_page_is_an_exact_prefix() {
        let a: &[Doc] = &[
            (1, "n1", "p/a1.go", "omega"),
            (3, "n3", "p/a3.go", "omega omega"),
            (5, "n5", "p/a5.go", "omega omega omega"),
        ];
        let b: &[Doc] =
            &[(2, "n2", "p/a2.go", "omega"), (4, "n4", "p/a4.go", "omega omega"), (6, "n6", "p/a6.go", "omega")];
        let f = fixture("bound", &[a, b]);
        let free = walk(&f, 6, 0);
        assert_eq!(free.len(), 6);
        let (more, first) = page(&f, 6, None, 1);
        assert_eq!(first.len(), 1, "a one-byte bound serves the one result progress needs");
        assert!(more);
        let bounded = walk(&f, 6, 1);
        assert_eq!(bounded, free);
    }

    /// A bounded page reads its results' strings in one sorted pass while
    /// they fit, and serves exactly the page a read of one result at a time
    /// serves, page by page, under every bound: where the keys alone pass it,
    /// where only the full strings do, and where everything fits; on one
    /// segment and on two.
    /// Mutation: let the sorted pass ignore the bound, or skip the fallback
    /// when it gives up.
    #[test]
    fn a_bounded_page_is_the_same_whichever_way_its_strings_are_read() {
        let long = |n: usize| format!("p/{}.go", "x".repeat(n));
        let (p1, p2, p3, p4, p5, p6) = (long(40), long(900), long(10), long(2500), long(300), long(1200));
        let all: Vec<Doc> = vec![
            (1, "n1", &p1, "omega"),
            (2, "n2", &p2, "omega omega"),
            (3, "n3", &p3, "omega omega omega"),
            (4, "n4", &p4, "omega"),
            (5, "n5", &p5, "omega omega"),
            (6, "n6", &p6, "omega"),
        ];
        for (name, segments) in [("one", vec![&all[..]]), ("two", vec![&all[..3], &all[3..]])] {
            let f = fixture(&format!("read-{name}"), &segments);
            for max_bytes in (1..120_000).step_by(397) {
                let mut after = None;
                for _ in 0..20 {
                    let keys = page_reading(&f, 4, after, max_bytes, Read::Keys);
                    let strings = page_reading(&f, 4, after, max_bytes, Read::Strings);
                    assert_eq!(keys, strings, "{name} segments, {max_bytes} bytes, after {after:?}");
                    let (more, hits) = keys;
                    if !more {
                        break;
                    }
                    let last = hits.last().expect("a page that says more holds a hit");
                    after = Some((last.micros, last.doc));
                }
            }
        }
    }
}
