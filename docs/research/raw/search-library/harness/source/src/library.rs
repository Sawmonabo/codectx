//! The library modes' shared pieces: how a library query is built (shape,
//! weights, side fields, filters), the collectors that answer it, an alive-only
//! statistics provider, and the per-unit visibility set (`--hide-units`).
use crate::{anyhow_lite::Result, unicode61, variants, Fields, Filter, WEIGHTS};
use std::collections::HashMap;
use std::ops::Bound;
use std::sync::{Arc, Mutex};
use tantivy::collector::{Collector, SegmentCollector};
use tantivy::columnar::StrColumn;
use tantivy::query::{
    BooleanQuery, Bm25StatisticsProvider, BoostQuery, ConstScoreQuery, DisjunctionMaxQuery, EnableScoring, Explanation,
    FastFieldRangeQuery, FuzzyTermQuery, InvertedIndexRangeQuery, Occur, PhrasePrefixQuery, PhraseQuery, Query,
    QueryParser, RangeQuery, RegexQuery, Scorer, TermQuery, TermSetQuery, Weight,
};
use tantivy::schema::{Field, IndexRecordOption};
use tantivy::tokenizer::TextAnalyzer;
use tantivy::index::SegmentId;
use tantivy::{DocId, DocSet, Index, Score, Searcher, SegmentReader, Term, TERMINATED};

pub fn regex_escape(s: &str) -> String {
    let mut out = String::with_capacity(s.len() * 2);
    for c in s.chars() {
        if "\\.+*?()|[]{}^$#&-~\"".contains(c) {
            out.push('\\');
        }
        out.push(c);
    }
    out
}

/// How query text becomes terms: our unicode61, the code variant (camelCase parts
/// as extra terms), or a stock tantivy analyzer for indexes built with one.
#[derive(Clone)]
pub enum Analyzer {
    U61,
    Code,
    Stock(TextAnalyzer),
}

impl Analyzer {
    /// The analyzer matching an index's text fields.
    pub fn for_index(index: &Index, f: &Fields) -> Analyzer {
        match crate::index_tokenizer(index, f).as_str() {
            crate::TOKENIZER => Analyzer::U61,
            crate::CODE_TOKENIZER => Analyzer::Code,
            _ => index.tokenizer_for_field(f.text[0]).map(Analyzer::Stock).unwrap_or(Analyzer::U61),
        }
    }
    fn tokens(&self, s: &str) -> Vec<(String, String)> {
        // (term, original spelling)
        let mut out = Vec::new();
        match self {
            Analyzer::Stock(a) => {
                let mut a = a.clone();
                let mut ts = a.token_stream(s);
                while ts.advance() {
                    let t = ts.token();
                    out.push((t.text.clone(), s[t.offset_from..t.offset_to].to_string()));
                }
            }
            _ => unicode61::for_each_token(s, |_, from, to, t| out.push((t.to_string(), s[from..to].to_string()))),
        }
        out
    }
}

/// One query term with the spelling it came from (for camelCase side fields).
#[derive(Clone, Debug)]
pub struct LTerm {
    pub tokens: Vec<String>,
    pub spellings: Vec<String>,
}

impl LTerm {
    pub fn phrase(&self) -> bool {
        self.tokens.len() > 1
    }
}

/// lexical.go parseQuery's segmentation with a chosen analyzer; with Code each
/// unquoted token also yields its camelCase parts as terms.
pub fn parse_terms(text: &str, an: &Analyzer) -> Vec<LTerm> {
    let mut terms: Vec<LTerm> = Vec::new();
    let mut seen = std::collections::HashSet::new();
    let mut add = |t: LTerm| {
        let k = t.tokens.join(" ");
        if k.is_empty() || !seen.insert(k) {
            return;
        }
        terms.push(t);
    };
    let mut quoted = false;
    for (i, seg) in text.split('"').enumerate() {
        if i > 0 {
            quoted = !quoted;
        }
        if seg.is_empty() {
            continue;
        }
        let toks = an.tokens(seg);
        if quoted {
            add(LTerm { tokens: toks.iter().map(|x| x.0.clone()).collect(), spellings: toks.into_iter().map(|x| x.1).collect() });
        } else {
            for (t, sp) in toks {
                if matches!(an, Analyzer::Code) {
                    for p in variants::query_parts(&sp) {
                        add(LTerm { tokens: vec![p.clone()], spellings: vec![p] });
                    }
                }
                add(LTerm { tokens: vec![t], spellings: vec![sp] });
            }
        }
    }
    terms
}

/// The query's shape (features/05-queries.md, features/06-scoring.md).
#[derive(Clone, Debug, PartialEq)]
pub enum Shape {
    /// SHOULD over terms of SHOULD over fields of Boost(Term|Phrase): the baseline.
    Nested,
    /// One SHOULD over every (token, field) Boost(Term WithFreqs): block-WAND eligible.
    Flat,
    /// Per term DisjunctionMax(tie) over the boosted fields.
    DisMax(f32),
    /// Nested, but every term required (with_minimum_required_clauses = terms).
    Msm,
    /// Nested, plus per single token and field Boost(FuzzyTermQuery(d=1, transposition), 0.1).
    Fuzzy,
    /// The query's tokens as one phrase whose last token is a prefix, per field;
    /// max expansions (0 = u32::MAX).
    PhrasePrefix(u32),
    /// QueryParser over the five fields with the weights as field boosts,
    /// parse_query_lenient on the raw text.
    Parser,
    /// `--combined` index: per term one Term|Phrase on the `all` field, SHOULD
    /// over terms (one BM25 over the concatenated columns).
    Combined,
    /// `--combined` index: per term SHOULD of the five boosted fields plus
    /// Boost(all, W).
    CombinedFields(f32),
}

impl Shape {
    pub fn parse(s: &str) -> Result<Shape> {
        Ok(match s {
            "nested" => Shape::Nested,
            "flat" => Shape::Flat,
            "msm" => Shape::Msm,
            "fuzzy" => Shape::Fuzzy,
            "parser" => Shape::Parser,
            "combined" => Shape::Combined,
            "combined+fields" => Shape::CombinedFields(1.0),
            x if x.starts_with("combined+fields:") => Shape::CombinedFields(x[16..].parse()?),
            x if x.starts_with("dismax:") => Shape::DisMax(x[7..].parse()?),
            x if x.starts_with("phrase-prefix:") => Shape::PhrasePrefix(x[14..].parse()?),
            _ => return Err("--shape is nested|flat|dismax:T|msm|fuzzy|phrase-prefix:N|parser|combined|combined+fields[:W]".into()),
        })
    }
}

/// How the kind filter is applied.
#[derive(Clone, Debug, PartialEq)]
pub enum KindVia {
    /// MUST ConstScore(0) over a SHOULD of TermQuery(kind, Basic); on a --kind-fast-only
    /// index, RangeQuery [k, k] on the fast field (TermQuery's own fallback, which it
    /// takes only when scoring is off).
    Terms,
    /// MUST ConstScore(0) over TermSetQuery.
    TermSet,
    /// tantivy's BytesFilterCollector on the kind column.
    Bytes,
    /// The bench's own collector wrapper on the kind column's ordinals (KeepCollector).
    Collector,
    /// FilterCollector on the u64 kind_code column (--kind-code index).
    Code,
}

/// How the path filter is applied (P itself, or under P/).
#[derive(Clone, Debug, PartialEq)]
pub enum PathVia {
    Regex,
    Range,
    FastRange,
    InvRange,
    Facet,
    Collector,
}

#[derive(Clone, Debug)]
pub struct QueryOpts {
    pub shape: Shape,
    pub weights: [f32; 5],
    pub kind_via: KindVia,
    pub path_via: PathVia,
    /// PhraseQuery slop for quoted phrases (0 = exact adjacency, our rule).
    pub slop: u32,
}

impl Default for QueryOpts {
    fn default() -> Self {
        QueryOpts { shape: Shape::Nested, weights: WEIGHTS.map(|w| w as f32), kind_via: KindVia::Terms, path_via: PathVia::Regex, slop: 0 }
    }
}

fn boosted(q: Box<dyn Query>, w: f32) -> Box<dyn Query> {
    Box::new(BoostQuery::new(q, w))
}

fn term_q(field: Field, t: &str) -> Box<dyn Query> {
    Box::new(TermQuery::new(Term::from_field_text(field, t), IndexRecordOption::WithFreqs))
}

/// The per-term disjuncts for one term: five boosted fields, plus side fields
/// (camelCase parts, name 3-grams) when the index has them.
fn disjuncts(t: &LTerm, f: &Fields, o: &QueryOpts) -> Vec<Box<dyn Query>> {
    let mut v: Vec<Box<dyn Query>> = Vec::new();
    for (i, field) in f.text.iter().enumerate() {
        let q: Box<dyn Query> = if t.phrase() {
            let mut pq = PhraseQuery::new(t.tokens.iter().map(|s| Term::from_field_text(*field, s)).collect());
            pq.set_slop(o.slop);
            Box::new(pq)
        } else {
            term_q(*field, &t.tokens[0])
        };
        v.push(boosted(q, o.weights[i]));
        if o.shape == Shape::Fuzzy && !t.phrase() {
            let fz = FuzzyTermQuery::new(Term::from_field_text(*field, &t.tokens[0]), 1, true);
            v.push(boosted(Box::new(fz), 0.1));
        }
    }
    if let (Some(np), Some(qp)) = (f.name_parts, f.qn_parts) {
        // Parts of each token (or the token itself) at consecutive positions.
        let mut seq: Vec<String> = Vec::new();
        for (tok, sp) in t.tokens.iter().zip(&t.spellings) {
            let parts = variants::query_parts(sp);
            if parts.is_empty() {
                seq.push(tok.clone());
            } else {
                seq.extend(parts);
            }
        }
        for field in [np, qp] {
            if seq.len() == 1 {
                v.push(boosted(term_q(field, &seq[0]), 2.0));
            } else if t.phrase() {
                v.push(boosted(Box::new(PhraseQuery::new(seq.iter().map(|s| Term::from_field_text(field, s)).collect())), 2.0));
            } else {
                for s in &seq {
                    v.push(boosted(term_q(field, s), 2.0));
                }
            }
        }
    }
    if let Some(ng) = f.name_ngram {
        if !t.phrase() {
            let chars: Vec<char> = t.tokens[0].chars().collect();
            if chars.len() >= 3 {
                let grams: Vec<(Occur, Box<dyn Query>)> = chars
                    .windows(3)
                    .map(|w| (Occur::Must, term_q(ng, &w.iter().collect::<String>())))
                    .collect();
                v.push(Box::new(BooleanQuery::new(grams)));
            }
        }
    }
    v
}

/// The scored part of a library query (no filters).
pub fn text_query(text: &str, index: &Index, f: &Fields, an: &Analyzer, o: &QueryOpts) -> Result<Box<dyn Query>> {
    let terms = parse_terms(text, an);
    Ok(match &o.shape {
        Shape::Parser => {
            let mut p = QueryParser::for_index(index, f.text.to_vec());
            for (i, field) in f.text.iter().enumerate() {
                p.set_field_boost(*field, o.weights[i]);
            }
            p.parse_query_lenient(text).0
        }
        Shape::PhrasePrefix(max) => {
            let toks: Vec<String> = terms.iter().flat_map(|t| t.tokens.clone()).collect();
            let mut per_field: Vec<(Occur, Box<dyn Query>)> = Vec::new();
            if !toks.is_empty() {
                for (i, field) in f.text.iter().enumerate() {
                    let mut q = PhrasePrefixQuery::new(toks.iter().map(|s| Term::from_field_text(*field, s)).collect());
                    q.set_max_expansions(if *max == 0 { u32::MAX } else { *max });
                    per_field.push((Occur::Should, boosted(Box::new(q), o.weights[i])));
                }
            }
            Box::new(BooleanQuery::new(per_field))
        }
        Shape::Combined | Shape::CombinedFields(_) => {
            let all = f.all.ok_or("--shape combined needs an index built with --combined")?;
            let outer: Vec<(Occur, Box<dyn Query>)> = terms
                .iter()
                .map(|t| {
                    let on_all: Box<dyn Query> = if t.phrase() {
                        let mut pq = PhraseQuery::new(t.tokens.iter().map(|s| Term::from_field_text(all, s)).collect());
                        pq.set_slop(o.slop);
                        Box::new(pq)
                    } else {
                        term_q(all, &t.tokens[0])
                    };
                    let q: Box<dyn Query> = match o.shape {
                        Shape::CombinedFields(w) => {
                            let mut v: Vec<(Occur, Box<dyn Query>)> = disjuncts(t, f, o).into_iter().map(|q| (Occur::Should, q)).collect();
                            v.push((Occur::Should, boosted(on_all, w)));
                            Box::new(BooleanQuery::new(v))
                        }
                        _ => on_all,
                    };
                    (Occur::Should, q)
                })
                .collect();
            Box::new(BooleanQuery::new(outer))
        }
        Shape::Flat => {
            let mut flat: Vec<(Occur, Box<dyn Query>)> = Vec::new();
            for t in &terms {
                if t.phrase() || f.name_parts.is_some() || f.name_ngram.is_some() {
                    flat.push((Occur::Should, Box::new(BooleanQuery::new(disjuncts(t, f, o).into_iter().map(|q| (Occur::Should, q)).collect()))));
                } else {
                    for q in disjuncts(t, f, o) {
                        flat.push((Occur::Should, q));
                    }
                }
            }
            Box::new(BooleanQuery::new(flat))
        }
        Shape::DisMax(tie) => {
            let outer: Vec<(Occur, Box<dyn Query>)> = terms
                .iter()
                .map(|t| (Occur::Should, Box::new(DisjunctionMaxQuery::with_tie_breaker(disjuncts(t, f, o), *tie)) as Box<dyn Query>))
                .collect();
            Box::new(BooleanQuery::new(outer))
        }
        _ => {
            let outer: Vec<(Occur, Box<dyn Query>)> = terms
                .iter()
                .map(|t| (Occur::Should, Box::new(BooleanQuery::new(disjuncts(t, f, o).into_iter().map(|q| (Occur::Should, q)).collect())) as Box<dyn Query>))
                .collect();
            if o.shape == Shape::Msm && !outer.is_empty() {
                let n = outer.len();
                Box::new(BooleanQuery::with_minimum_required_clauses(outer, n))
            } else {
                Box::new(BooleanQuery::new(outer))
            }
        }
    })
}

/// The full library query: the text query plus the query-side filters (the
/// collector-side ones are applied by `search_filtered`).
pub fn build_query(text: &str, index: &Index, f: &Fields, an: &Analyzer, o: &QueryOpts, filter: &Filter) -> Result<Box<dyn Query>> {
    let text_q = text_query(text, index, f, an, o)?;
    let mut must: Vec<(Occur, Box<dyn Query>)> = vec![(Occur::Must, text_q)];
    if !filter.kinds.is_empty() {
        let q: Option<Box<dyn Query>> = match o.kind_via {
            KindVia::Terms => {
                // TermQuery falls back to a fast-field range only when scoring is off
                // (query/term_query/term_query.rs:131-137); under a scored query it
                // errors on a FAST-only field, so that index gets the same range here.
                let indexed = index.schema().get_field_entry(f.kind).is_indexed();
                Some(Box::new(BooleanQuery::new(
                    filter
                        .kinds
                        .iter()
                        .map(|k| {
                            let t = Term::from_field_text(f.kind, k);
                            let q: Box<dyn Query> = if indexed {
                                Box::new(TermQuery::new(t, IndexRecordOption::Basic))
                            } else {
                                Box::new(RangeQuery::new(Bound::Included(t.clone()), Bound::Included(t)))
                            };
                            (Occur::Should, q)
                        })
                        .collect(),
                )))
            }
            KindVia::TermSet => Some(Box::new(TermSetQuery::new(filter.kinds.iter().map(|k| Term::from_field_text(f.kind, k))))),
            KindVia::Bytes | KindVia::Collector | KindVia::Code => None,
        };
        if let Some(q) = q {
            must.push((Occur::Must, Box::new(ConstScoreQuery::new(q, 0.0))));
        }
    }
    if !filter.prefixes.is_empty() && o.path_via != PathVia::Collector {
        let mut any: Vec<(Occur, Box<dyn Query>)> = Vec::new();
        for p in &filter.prefixes {
            let under = format!("{p}/");
            // "P0" is the first string after every "P/…": '0' follows '/'.
            let above = format!("{p}0");
            let lo = Bound::Included(Term::from_field_text(f.path_s, &under));
            let hi = Bound::Excluded(Term::from_field_text(f.path_s, &above));
            if o.path_via == PathVia::Facet {
                let fp = f.path_facet.ok_or("--path-filter facet needs an index built with --path-facet")?;
                any.push((Occur::Should, Box::new(TermQuery::new(Term::from_facet(fp, &variants::path_facet(p)), IndexRecordOption::Basic))));
                continue;
            }
            any.push((Occur::Should, Box::new(TermQuery::new(Term::from_field_text(f.path_s, p), IndexRecordOption::Basic))));
            let q: Box<dyn Query> = match o.path_via {
                PathVia::Regex => Box::new(RegexQuery::from_pattern(&format!("{}/.*", regex_escape(p)), f.path_s)?),
                PathVia::Range => Box::new(RangeQuery::new(lo, hi)),
                PathVia::FastRange => Box::new(FastFieldRangeQuery::new(lo, hi)),
                _ => Box::new(InvertedIndexRangeQuery::new(lo, hi)),
            };
            any.push((Occur::Should, q));
        }
        must.push((Occur::Must, Box::new(ConstScoreQuery::new(Box::new(BooleanQuery::new(any)), 0.0))));
    }
    if must.len() == 1 {
        return Ok(must.pop().expect("text query").1);
    }
    Ok(Box::new(BooleanQuery::new(must)))
}

/// The baseline library query (nested shape, query-side filters).
pub fn library_query(text: &str, f: &Fields, filter: &Filter, code: bool) -> Result<Box<dyn Query>> {
    // The parser shape alone needs the index; the baseline does not.
    let an = if code { Analyzer::Code } else { Analyzer::U61 };
    let o = QueryOpts::default();
    let terms = parse_terms(text, &an);
    let outer: Vec<(Occur, Box<dyn Query>)> = terms
        .iter()
        .map(|t| (Occur::Should, Box::new(BooleanQuery::new(disjuncts(t, f, &o).into_iter().map(|q| (Occur::Should, q)).collect())) as Box<dyn Query>))
        .collect();
    let text_q: Box<dyn Query> = Box::new(BooleanQuery::new(outer));
    if filter.is_empty() {
        return Ok(text_q);
    }
    let mut must: Vec<(Occur, Box<dyn Query>)> = vec![(Occur::Must, text_q)];
    if !filter.kinds.is_empty() {
        let any: Vec<(Occur, Box<dyn Query>)> = filter
            .kinds
            .iter()
            .map(|k| (Occur::Should, Box::new(TermQuery::new(Term::from_field_text(f.kind, k), IndexRecordOption::Basic)) as Box<dyn Query>))
            .collect();
        must.push((Occur::Must, Box::new(ConstScoreQuery::new(Box::new(BooleanQuery::new(any)), 0.0))));
    }
    if !filter.prefixes.is_empty() {
        let mut any: Vec<(Occur, Box<dyn Query>)> = Vec::new();
        for p in &filter.prefixes {
            any.push((Occur::Should, Box::new(TermQuery::new(Term::from_field_text(f.path_s, p), IndexRecordOption::Basic))));
            any.push((Occur::Should, Box::new(RegexQuery::from_pattern(&format!("{}/.*", regex_escape(p)), f.path_s)?)));
        }
        must.push((Occur::Must, Box::new(ConstScoreQuery::new(Box::new(BooleanQuery::new(any)), 0.0))));
    }
    Ok(Box::new(BooleanQuery::new(must)))
}

/// library-all's collector: every (doc, score) of every segment, nothing pruned.
/// Each item is (score, the segment's id column, doc id within the segment, segment ordinal).
pub struct AllScores;
pub struct AllScoresSeg {
    ids: Option<StrColumn>,
    hits: Vec<(DocId, Score)>,
    ord: u32,
}
impl Collector for AllScores {
    type Fruit = Vec<(Score, Option<StrColumn>, DocId, u32)>;
    type Child = AllScoresSeg;
    fn for_segment(&self, ord: u32, seg: &SegmentReader) -> tantivy::Result<AllScoresSeg> {
        Ok(AllScoresSeg { ids: seg.fast_fields().str("id")?, hits: Vec::new(), ord })
    }
    fn requires_scoring(&self) -> bool {
        true
    }
    fn merge_fruits(&self, fruits: Vec<Self::Fruit>) -> tantivy::Result<Self::Fruit> {
        Ok(fruits.into_iter().flatten().collect())
    }
}
impl SegmentCollector for AllScoresSeg {
    type Fruit = Vec<(Score, Option<StrColumn>, DocId, u32)>;
    fn collect(&mut self, doc: DocId, score: Score) {
        self.hits.push((doc, score));
    }
    fn harvest(self) -> Self::Fruit {
        let (ids, ord) = (self.ids, self.ord);
        self.hits.into_iter().map(|(d, s)| (s, ids.clone(), d, ord)).collect()
    }
}

/// A docset-only collector: the matching doc ids, no scores, via collect_block.
pub struct VecDocs;
pub struct VecDocsSeg(Vec<DocId>);
impl Collector for VecDocs {
    type Fruit = usize;
    type Child = VecDocsSeg;
    fn for_segment(&self, _: u32, _: &SegmentReader) -> tantivy::Result<VecDocsSeg> {
        Ok(VecDocsSeg(Vec::new()))
    }
    fn requires_scoring(&self) -> bool {
        false
    }
    fn merge_fruits(&self, fruits: Vec<usize>) -> tantivy::Result<usize> {
        Ok(fruits.into_iter().sum())
    }
}
impl SegmentCollector for VecDocsSeg {
    type Fruit = usize;
    fn collect(&mut self, doc: DocId, _: Score) {
        self.0.push(doc);
    }
    fn collect_block(&mut self, docs: &[DocId]) {
        self.0.extend_from_slice(docs);
    }
    fn harvest(self) -> usize {
        self.0.len()
    }
}

pub fn id_of(col: &Option<StrColumn>, d: DocId) -> String {
    let mut s = String::new();
    if let Some(c) = col {
        if let Some(o) = c.term_ords(d).next() {
            let _ = c.ord_to_str(o, &mut s);
        }
    }
    s
}

// ---------------------------------------------------------------------------
// Per-unit visibility (--hide-units): ONE index holding every document, plus a
// per-searcher visibility set, the way our engine keeps retained snapshots.

/// Which documents are visible, per segment, built once per searcher from the
/// `unit` fast field: a unit is hidden when a seeded FNV hash of (seed, unit)
/// falls in the lowest `pct` percent (or when an explicit address list hides it).
pub struct HideSet {
    pub pct: f64,
    pub seed: u64,
    visible: HashMap<SegmentId, Arc<Vec<bool>>>,
    pub hidden_docs: u64,
}

pub fn unit_hidden(unit: &[u8], pct: f64, seed: u64) -> bool {
    let mut h: u64 = 0xcbf29ce484222325 ^ seed.wrapping_mul(0x9E37_79B9_7F4A_7C15);
    for &b in unit {
        h ^= b as u64;
        h = h.wrapping_mul(0x100000001b3);
    }
    // Final avalanche so neighbouring seeds pick unrelated sets.
    h ^= h >> 33;
    h = h.wrapping_mul(0xff51afd7ed558ccd);
    h ^= h >> 33;
    ((h % 1_000_000) as f64) < pct * 10_000.0
}

impl HideSet {
    pub fn build(s: &Searcher, pct: f64, seed: u64) -> Result<HideSet> {
        let mut visible = HashMap::new();
        let mut hidden_docs = 0;
        for seg in s.segment_readers() {
            let col = seg.fast_fields().str("unit")?.ok_or("the unit fast field is missing")?;
            let mut hidden_ord = Vec::with_capacity(col.num_terms());
            col.dictionary()
                .sorted_ords_to_term_cb(0..col.num_terms() as u64, |b| {
                    hidden_ord.push(unit_hidden(b, pct, seed));
                    Ok(())
                })
                .map_err(tantivy::TantivyError::from)?;
            let mut v = vec![true; seg.max_doc() as usize];
            for d in 0..seg.max_doc() {
                if let Some(o) = col.term_ords(d).next() {
                    if hidden_ord[o as usize] {
                        v[d as usize] = false;
                        if seg.alive_bitset().map_or(true, |a| a.is_alive(d)) {
                            hidden_docs += 1;
                        }
                    }
                }
            }
            visible.insert(seg.segment_id(), Arc::new(v));
        }
        Ok(HideSet { pct, seed, visible, hidden_docs })
    }

    /// Hides an explicit list of (segment id, doc) addresses (tdelta --soft).
    pub fn from_addresses(s: &Searcher, file: &str) -> Result<HideSet> {
        let mut by: HashMap<String, Vec<DocId>> = HashMap::new();
        for l in std::fs::read_to_string(file)?.lines().filter(|l| !l.is_empty()) {
            let (seg, d) = l.split_once('\t').ok_or("hide file lines are <segment id>\\t<doc>")?;
            by.entry(seg.to_string()).or_default().push(d.parse()?);
        }
        let mut visible = HashMap::new();
        let mut hidden_docs = 0;
        for seg in s.segment_readers() {
            let mut v = vec![true; seg.max_doc() as usize];
            if let Some(ds) = by.get(&seg.segment_id().uuid_string()) {
                for &d in ds {
                    if (d as usize) < v.len() && v[d as usize] {
                        v[d as usize] = false;
                        hidden_docs += 1;
                    }
                }
            }
            visible.insert(seg.segment_id(), Arc::new(v));
        }
        Ok(HideSet { pct: f64::NAN, seed: 0, visible, hidden_docs })
    }

    pub fn segment(&self, id: SegmentId) -> Option<Arc<Vec<bool>>> {
        self.visible.get(&id).cloned()
    }
}

/// A Query wrapper whose Weight intersects the inner scorer with the visibility
/// set, and forwards for_each_pruning with hidden documents filtered out of the
/// callback, so a pruning inner weight (block-WAND) keeps pruning.
pub struct HideQuery {
    pub inner: Box<dyn Query>,
    pub hide: Arc<HideSet>,
}

impl Clone for HideQuery {
    fn clone(&self) -> Self {
        HideQuery { inner: self.inner.box_clone(), hide: self.hide.clone() }
    }
}

impl std::fmt::Debug for HideQuery {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        write!(f, "HideQuery({:?})", self.inner)
    }
}

impl Query for HideQuery {
    fn weight(&self, enable_scoring: EnableScoring<'_>) -> tantivy::Result<Box<dyn Weight>> {
        Ok(Box::new(HideWeight { inner: self.inner.weight(enable_scoring)?, hide: self.hide.clone() }))
    }
    fn query_terms<'a>(&'a self, visitor: &mut dyn FnMut(&'a Term, bool)) {
        self.inner.query_terms(visitor)
    }
}

struct HideWeight {
    inner: Box<dyn Weight>,
    hide: Arc<HideSet>,
}

impl Weight for HideWeight {
    fn scorer(&self, reader: &SegmentReader, boost: Score) -> tantivy::Result<Box<dyn Scorer>> {
        let inner = self.inner.scorer(reader, boost)?;
        Ok(match self.hide.segment(reader.segment_id()) {
            None => inner,
            Some(vis) => {
                let mut s = HideScorer { inner, vis };
                s.skip_hidden();
                Box::new(s)
            }
        })
    }
    fn explain(&self, reader: &SegmentReader, doc: DocId) -> tantivy::Result<Explanation> {
        self.inner.explain(reader, doc)
    }
    fn for_each_pruning(
        &self,
        threshold: Score,
        reader: &SegmentReader,
        callback: &mut dyn FnMut(DocId, Score) -> Score,
    ) -> tantivy::Result<()> {
        let Some(vis) = self.hide.segment(reader.segment_id()) else {
            return self.inner.for_each_pruning(threshold, reader, callback);
        };
        let mut t = threshold;
        self.inner.for_each_pruning(threshold, reader, &mut |d, s| {
            if vis[d as usize] {
                t = callback(d, s);
            }
            t
        })
    }
}

struct HideScorer {
    inner: Box<dyn Scorer>,
    vis: Arc<Vec<bool>>,
}

impl HideScorer {
    fn skip_hidden(&mut self) -> DocId {
        let mut d = self.inner.doc();
        while d != TERMINATED && !self.vis[d as usize] {
            d = self.inner.advance();
        }
        d
    }
}

impl DocSet for HideScorer {
    fn advance(&mut self) -> DocId {
        self.inner.advance();
        self.skip_hidden()
    }
    fn seek(&mut self, target: DocId) -> DocId {
        if self.inner.doc() < target {
            self.inner.seek(target);
        }
        self.skip_hidden()
    }
    fn doc(&self) -> DocId {
        self.inner.doc()
    }
    fn size_hint(&self) -> u32 {
        self.inner.size_hint()
    }
}

impl Scorer for HideScorer {
    fn score(&mut self) -> Score {
        self.inner.score()
    }
}

/// The filtering-collector route for the same visibility: wraps any collector and
/// drops hidden documents before its segment collector sees them. It does not
/// forward an inner collector's own collect_segment (TopDocs' pruning path), so
/// it measures what a plain collector wrapper costs.
pub struct HideCollector<C> {
    pub inner: C,
    pub hide: Arc<HideSet>,
}

pub struct HideSegment<S> {
    inner: S,
    vis: Option<Arc<Vec<bool>>>,
}

impl<C: Collector> Collector for HideCollector<C> {
    type Fruit = C::Fruit;
    type Child = HideSegment<C::Child>;
    fn for_segment(&self, ord: u32, seg: &SegmentReader) -> tantivy::Result<Self::Child> {
        Ok(HideSegment { inner: self.inner.for_segment(ord, seg)?, vis: self.hide.segment(seg.segment_id()) })
    }
    fn requires_scoring(&self) -> bool {
        self.inner.requires_scoring()
    }
    fn merge_fruits(&self, f: Vec<<C::Child as SegmentCollector>::Fruit>) -> tantivy::Result<C::Fruit> {
        self.inner.merge_fruits(f)
    }
}

impl<S: SegmentCollector> SegmentCollector for HideSegment<S> {
    type Fruit = S::Fruit;
    fn collect(&mut self, doc: DocId, score: Score) {
        if self.vis.as_ref().map_or(true, |v| v[doc as usize]) {
            self.inner.collect(doc, score);
        }
    }
    fn collect_block(&mut self, docs: &[DocId]) {
        match &self.vis {
            None => self.inner.collect_block(docs),
            Some(v) => {
                let kept: Vec<DocId> = docs.iter().copied().filter(|d| v[*d as usize]).collect();
                self.inner.collect_block(&kept);
            }
        }
    }
    fn harvest(self) -> S::Fruit {
        self.inner.harvest()
    }
}

// ---------------------------------------------------------------------------
// Statistics over live (and visible) documents for the library's BM25.

/// A Bm25StatisticsProvider over live documents (and, with a HideSet, visible
/// ones). `union`: df counts a document once across the five text fields (our df);
/// otherwise per (field, term) as tantivy does, but live-only. Token totals come
/// from the per-column count fields (--field-counts) when the index has them,
/// else from the decoded one-byte fieldnorms of live documents (approximate).
/// One provider serves one searcher; its answers are cached for that searcher's
/// life, as a product would cache them per snapshot.
pub struct LiveStats {
    pub searcher: Searcher,
    pub fields: Fields,
    pub union: bool,
    pub hide: Option<Arc<HideSet>>,
    cache: Mutex<HashMap<Term, u64>>,
    tokens: Mutex<HashMap<Field, u64>>,
    docs: Mutex<Option<u64>>,
}

impl LiveStats {
    pub fn new(searcher: &Searcher, fields: Fields, union: bool, hide: Option<Arc<HideSet>>) -> Self {
        LiveStats {
            searcher: searcher.clone(), fields, union, hide,
            cache: Mutex::new(HashMap::new()), tokens: Mutex::new(HashMap::new()), docs: Mutex::new(None),
        }
    }
    fn live(&self, seg: &SegmentReader) -> impl Fn(DocId) -> bool {
        let alive = seg.alive_bitset().cloned();
        let vis = self.hide.as_ref().and_then(|h| h.segment(seg.segment_id()));
        move |d| alive.as_ref().map_or(true, |a| a.is_alive(d)) && vis.as_ref().map_or(true, |v| v[d as usize])
    }
}

impl Bm25StatisticsProvider for LiveStats {
    fn total_num_tokens(&self, field: Field) -> tantivy::Result<u64> {
        if let Some(v) = self.tokens.lock().unwrap().get(&field) {
            return Ok(*v);
        }
        let col = self.fields.text.iter().position(|f| *f == field);
        let mut n = 0u64;
        for seg in self.searcher.segment_readers() {
            let live = self.live(seg);
            match (col, self.fields.counts) {
                (Some(i), Some(cs)) => {
                    let c = seg.fast_fields().u64(tantivy::schema::Schema::get_field_name(&self.searcher.schema(), cs[i]))?;
                    for d in 0..seg.max_doc() {
                        if live(d) {
                            n += c.first(d).unwrap_or(0);
                        }
                    }
                }
                _ => {
                    let norms = seg.get_fieldnorms_reader(field)?;
                    for d in 0..seg.max_doc() {
                        if live(d) {
                            n += norms.fieldnorm(d) as u64;
                        }
                    }
                }
            }
        }
        self.tokens.lock().unwrap().insert(field, n);
        Ok(n)
    }
    fn total_num_docs(&self) -> tantivy::Result<u64> {
        if let Some(v) = *self.docs.lock().unwrap() {
            return Ok(v);
        }
        let mut n = 0u64;
        for seg in self.searcher.segment_readers() {
            let live = self.live(seg);
            n += (0..seg.max_doc()).filter(|d| live(*d)).count() as u64;
        }
        *self.docs.lock().unwrap() = Some(n);
        Ok(n)
    }
    fn doc_freq(&self, term: &Term) -> tantivy::Result<u64> {
        if let Some(v) = self.cache.lock().unwrap().get(term) {
            return Ok(*v);
        }
        let fields: Vec<Field> = if self.union && self.fields.text.contains(&term.field()) {
            self.fields.text.to_vec()
        } else {
            vec![term.field()]
        };
        let text = term.value().as_str().map(str::to_string);
        let mut n = 0u64;
        for seg in self.searcher.segment_readers() {
            let live = self.live(seg);
            let mut docs: Vec<DocId> = Vec::new();
            for f in &fields {
                let t = match &text {
                    Some(s) => Term::from_field_text(*f, s),
                    None => term.clone(),
                };
                if let Some(mut p) = seg.inverted_index(*f)?.read_postings(&t, IndexRecordOption::Basic)? {
                    let mut d = p.doc();
                    while d != TERMINATED {
                        docs.push(d);
                        d = p.advance();
                    }
                }
            }
            docs.sort_unstable();
            docs.dedup();
            n += docs.into_iter().filter(|d| live(*d)).count() as u64;
        }
        self.cache.lock().unwrap().insert(term.clone(), n);
        Ok(n)
    }
}

/// The bench's own filtering wrapper: keeps the documents a Filter accepts,
/// matched on the kind and path columns' segment ordinals (product.rs SegFilter,
/// the same rule product mode applies). Like HideCollector it does not forward the
/// inner collector's collect_segment, so TopDocs loses its pruning path.
pub struct KeepCollector<C> {
    pub inner: C,
    pub filter: Filter,
}

pub struct KeepSegment<S> {
    inner: S,
    kind: crate::product::RankCol,
    path: crate::product::RankCol,
    keep: crate::product::SegFilter,
    buf: Vec<DocId>,
}

impl<C: Collector> Collector for KeepCollector<C> {
    type Fruit = C::Fruit;
    type Child = KeepSegment<C::Child>;
    fn for_segment(&self, ord: u32, seg: &SegmentReader) -> tantivy::Result<Self::Child> {
        let kind = crate::product::RankCol::open(seg, "kind")?;
        let path = crate::product::RankCol::open(seg, "path_s")?;
        let keep = crate::product::SegFilter::build(&self.filter, &kind, &path)?;
        Ok(KeepSegment { inner: self.inner.for_segment(ord, seg)?, kind, path, keep, buf: Vec::new() })
    }
    fn requires_scoring(&self) -> bool {
        self.inner.requires_scoring()
    }
    fn merge_fruits(&self, f: Vec<<C::Child as SegmentCollector>::Fruit>) -> tantivy::Result<C::Fruit> {
        self.inner.merge_fruits(f)
    }
}

impl<S: SegmentCollector> SegmentCollector for KeepSegment<S> {
    type Fruit = S::Fruit;
    fn collect(&mut self, doc: DocId, score: Score) {
        if self.keep.keep(&self.kind, &self.path, doc) {
            self.inner.collect(doc, score);
        }
    }
    fn collect_block(&mut self, docs: &[DocId]) {
        self.buf.clear();
        for &d in docs {
            if self.keep.keep(&self.kind, &self.path, d) {
                self.buf.push(d);
            }
        }
        if !self.buf.is_empty() {
            self.inner.collect_block(&self.buf);
        }
    }
    fn harvest(self) -> S::Fruit {
        self.inner.harvest()
    }
}

/// The alive-set route for visibility: the searcher's segments reopened with a
/// custom alive bitset (live AND visible), in the searcher's segment order, so
/// DocAddress ordinals agree with the searcher.
pub fn alive_readers(index: &Index, s: &Searcher, hide: &HideSet) -> Result<Vec<SegmentReader>> {
    let segs = index.searchable_segments()?;
    let mut out = Vec::new();
    for r in s.segment_readers() {
        let seg = segs.iter().find(|g| g.id() == r.segment_id()).ok_or("a searcher segment is not searchable")?;
        let max = r.max_doc();
        let vis = hide.segment(r.segment_id());
        let mut bits = tantivy_common::BitSet::with_max_value(max);
        for d in 0..max {
            if r.alive_bitset().map_or(true, |a| a.is_alive(d)) && vis.as_ref().map_or(true, |v| v[d as usize]) {
                bits.insert(d);
            }
        }
        let mut bytes = Vec::new();
        tantivy::fastfield::write_alive_bitset(&bits, &mut bytes)?;
        let alive = tantivy::fastfield::AliveBitSet::open(tantivy::directory::OwnedBytes::new(bytes));
        out.push(SegmentReader::open_with_custom_alive_set(seg, Some(alive))?);
    }
    Ok(out)
}
