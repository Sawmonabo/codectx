//! tquery <index dir> <queries.txt> <runs> <mode> [options]
//!
//! Per query: run 0 is cold, in a fresh child process (index open untimed, the
//! timed part is the query alone; the OS page cache is whatever it is); runs
//! 1..=runs are warm, in this process. Writes results/tantivy-<mode>-<phase>.jsonl;
//! the phase is "query" plus one tag per option that is on, unless --phase names it.
//!
//! Modes:
//!   library       the library query (see library.rs), TopDocs top 200 (offset with
//!                 --page), ids from the store (or the id fast field). Timed: search
//!                 + id fetch. hits: a separate, untimed Count (or --collect tuple-count).
//!   library-all   the same query and BM25, but every (doc, score) is collected; the
//!                 set is ordered score desc, id asc, and the page sliced from it.
//!   product       our rules (product.rs); hits are deduplicated entities.
//!   product-code  product mode (our BM25F, exact statistics, our comparator) on a
//!                 `--tokenizer code` index: query terms gain their camelCase parts
//!                 (parse_query_with code), document length is token_count_code.
//!   library-combined  library-all's exhaustive collection over a `--combined` index
//!                 with --shape combined (BM25 over `all` alone) by default, or
//!                 --shape combined+fields[:W] (all, boost W, SHOULD with the five).
//!   product-topk  product's exact page through block-max skipping (topk.rs): needs
//!                 `tblock <dir>` first. Follows the index: unicode61 rules, or
//!                 product-code's on a code index. --page N serves page N from the top
//!                 N*200. hits: product's own count, run apart and untimed.
//!   product-block product's answer with candidates from the library's Weight
//!                 (for_each_no_score), postings sought only at the candidates.
//!   count         the hit count alone: --count-via query (Weight::count per segment,
//!                 which reads df when nothing is deleted) | collector (Count).
//!   docset        the matching set, no scores: --docset-via docset (DocSetCollector,
//!                 a HashSet of addresses) | vec (a Vec of ids through collect_block).
//!   agg-kind      top 200 plus hits per kind: --agg tuple ((TopDocs, AggregationCollector)
//!                 in one search) | separate (two searches) | custom ((TopDocs, own
//!                 per-kind-ordinal counter)). Terms aggregation on kind, size 1000.
//!   facets        top 200 plus counts under "/" of the path facet (--path-facet index):
//!                 --facets tuple | separate.
//!
//! Query options (library-family modes):
//!   --shape nested|flat|dismax:T|msm|fuzzy|phrase-prefix:N|parser   (default nested)
//!   --weights a/b/c/d/e                       field boosts (default 5/5/2/2/1)
//!   --slop N                                  PhraseQuery slop for quoted phrases (default 0)
//!   --kind-filter terms|termset|bytes|collector|code   (default terms: MUST ConstScore(0))
//!   --path-filter regex|range|ffrange|invrange|facet|collector   (default regex)
//!   --collect top|tuple-count|tuple-id|tweak  (library; default top)
//!       tuple-count: (TopDocs, Count) in one search; tuple-id: order_by((score desc,
//!       id asc)); tweak: tweak_score with the identity, pricing the hook alone.
//!   --stats default|live|live-union           BM25 statistics (library.rs LiveStats)
//!   --id-source store|fast                    (library; fast when nothing is stored)
//! Reader options (all modes):
//!   --doc-cache N         IndexReaderBuilder::doc_store_cache_num_blocks; the cache
//!                         stats go to <arm>-<phase>-cache.jsonl
//!   --madvise normal|random|sequential|willneed   MmapDirectory::open_with_madvice
//!   --warm-stats          (product) a Warmer computes the corpus statistics per searcher
//!                         generation at reader open, so the cold query does not
//!   --time-open           the cold child's open (+ warm) time to <arm>-<phase>-open.jsonl
//!   --search-threads N    the searcher's multi-threaded executor, segments in parallel
//!                         (every mode; product modes merge under our comparator, so the
//!                         answer is the single-threaded one)
//! Filters (all modes): --kind K, --path-prefix P (P itself or under P/).
//! Visibility (all modes):
//!   --hide-units PCT --hide-seed S   hide PCT% of units by a seeded hash of unit
//!   --hide FILE                      hide the addresses listed (tdelta --soft)
//!   --hide-via weight|collector|alive-set   (default weight)
//!       weight: a Weight wrapper intersecting the scorer, forwarding pruning;
//!       collector: a filtering collector wrapper; alive-set: the segments reopened
//!       with a custom alive bitset (one thread). Product mode always hides inside
//!       its own collector, or through the alive set.
//!   The library's own statistics still count hidden documents (stats_include_hidden).
//! Output: --page N (library family), --explain K (library: explain() of the first K
//!   hits, timed, value checked against the score, to <arm>-<phase>-explain.jsonl;
//!   with --explain-qrels QRELS.tsv also each relevant document's explanation and
//!   exhaustive rank),
//!   --phase NAME.
use std::collections::HashMap;
use std::io::Write;
use std::sync::{Arc, Mutex};
use std::time::Instant;
use tantivy::aggregation::{AggContextParams, AggregationCollector, AggregationLimitsGuard};
use tantivy::collector::sort_key::{SortBySimilarityScore, SortByString};
use tantivy::collector::{
    BytesFilterCollector, Collector, Count, DocSetCollector, FacetCollector, FilterCollector, SegmentCollector, TopDocs,
};
use tantivy::directory::{Advice, MmapDirectory};
use tantivy::query::{AllQuery, Bm25StatisticsProvider, EnableScoring, Query};
use tantivy::schema::Value;
use tantivy::{DocAddress, DocId, Index, Order, ReloadPolicy, Score, Searcher, SegmentReader, TantivyDocument, Warmer};
use tbench::library::{
    alive_readers, build_query, id_of, Analyzer, HideCollector, HideQuery, HideSet, KeepCollector, KindVia, LiveStats,
    library_query, AllScores, PathVia, QueryOpts, Shape, VecDocs,
};
use tbench::topk::{DfCollector, TopkCollector};
use tbench::product::{corpus_stats_of, CorpusStats, ProductCollector, PAGE};
use tbench::{anyhow_lite::Result, append_result, parse_query, parse_query_with, results_dir, variants, Fields, Filter, QueryLine};

/// Options that take a value, and the ones that do not.
const VALUED: &[&str] = &[
    "--kind", "--path-prefix", "--search-threads", "--phase", "--page", "--explain", "--explain-qrels", "--shape", "--weights",
    "--slop", "--kind-filter", "--path-filter", "--collect", "--stats", "--id-source", "--doc-cache", "--madvise",
    "--hide-units", "--hide-seed", "--hide", "--hide-via", "--count-via", "--docset-via", "--agg", "--facets",
];
const FLAGS: [&str; 2] = ["--warm-stats", "--time-open"];
/// Options recorded in their own result field (not in `variant`).
const OWN_FIELD: [&str; 7] = ["--kind", "--path-prefix", "--search-threads", "--page", "--phase", "--explain", "--explain-qrels"];

#[derive(Clone, Default)]
struct Opts {
    raw: Vec<String>,
    filter: Filter,
    threads: Option<usize>,
    phase: Option<String>,
    page_no: Option<usize>,
    explain: Option<usize>,
    explain_qrels: Option<String>,
    q: QueryOpts,
    collect: String,
    stats: String,
    id_fast: Option<bool>,
    doc_cache: Option<usize>,
    madvise: Option<String>,
    warm_stats: bool,
    time_open: bool,
    hide_pct: Option<f64>,
    hide_seed: u64,
    hide_file: Option<String>,
    hide_via: String,
    count_via: String,
    docset_via: String,
    agg: String,
    facets: String,
}

impl Opts {
    fn parse(args: &[String]) -> Result<Opts> {
        let mut o = Opts {
            raw: args.to_vec(),
            collect: "top".into(),
            stats: "default".into(),
            hide_via: "weight".into(),
            count_via: "collector".into(),
            docset_via: "docset".into(),
            agg: "tuple".into(),
            facets: "tuple".into(),
            ..Opts::default()
        };
        let mut i = 0;
        while i < args.len() {
            let k = args[i].as_str();
            if FLAGS.contains(&k) {
                match k {
                    "--warm-stats" => o.warm_stats = true,
                    _ => o.time_open = true,
                }
                i += 1;
                continue;
            }
            if !VALUED.contains(&k) {
                return Err(format!("unknown option {k}").into());
            }
            let v = args.get(i + 1).ok_or_else(|| format!("{k} needs a value"))?.clone();
            match k {
                "--kind" => o.filter.kinds.push(v),
                "--path-prefix" => {
                    let p = Filter::clean_prefix(&v);
                    if p.is_empty() {
                        return Err("--path-prefix must name a root-relative path".into());
                    }
                    o.filter.prefixes.push(p)
                }
                "--search-threads" => o.threads = Some(v.parse()?),
                "--phase" => o.phase = Some(v),
                "--page" => {
                    let n: usize = v.parse()?;
                    if n == 0 {
                        return Err("--page counts from 1".into());
                    }
                    o.page_no = Some(n)
                }
                "--explain" => o.explain = Some(v.parse()?),
                "--explain-qrels" => o.explain_qrels = Some(v),
                "--shape" => o.q.shape = Shape::parse(&v)?,
                "--weights" => {
                    let w: Vec<f32> = v.split('/').map(str::parse).collect::<std::result::Result<_, _>>()?;
                    o.q.weights = w.try_into().map_err(|_| "--weights takes five values a/b/c/d/e")?;
                }
                "--slop" => o.q.slop = v.parse()?,
                "--kind-filter" => {
                    o.q.kind_via = match v.as_str() {
                        "terms" => KindVia::Terms,
                        "termset" => KindVia::TermSet,
                        "bytes" => KindVia::Bytes,
                        "collector" => KindVia::Collector,
                        "code" => KindVia::Code,
                        _ => return Err("--kind-filter is terms|termset|bytes|collector|code".into()),
                    }
                }
                "--path-filter" => {
                    o.q.path_via = match v.as_str() {
                        "regex" => PathVia::Regex,
                        "range" => PathVia::Range,
                        "ffrange" => PathVia::FastRange,
                        "invrange" => PathVia::InvRange,
                        "facet" => PathVia::Facet,
                        "collector" => PathVia::Collector,
                        _ => return Err("--path-filter is regex|range|ffrange|invrange|facet|collector".into()),
                    }
                }
                "--collect" => o.collect = one_of(k, v, &["top", "tuple-count", "tuple-id", "tweak"])?,
                "--stats" => o.stats = one_of(k, v, &["default", "live", "live-union"])?,
                "--id-source" => o.id_fast = Some(one_of(k, v, &["store", "fast"])? == "fast"),
                "--doc-cache" => o.doc_cache = Some(v.parse()?),
                "--madvise" => o.madvise = Some(one_of(k, v, &["normal", "random", "sequential", "willneed"])?),
                "--hide-units" => {
                    let p: f64 = v.parse()?;
                    if !(0.0..=100.0).contains(&p) {
                        return Err("--hide-units is a percentage, 0 to 100".into());
                    }
                    o.hide_pct = Some(p)
                }
                "--hide-seed" => o.hide_seed = v.parse()?,
                "--hide" => o.hide_file = Some(v),
                "--hide-via" => o.hide_via = one_of(k, v, &["weight", "collector", "alive-set"])?,
                "--count-via" => o.count_via = one_of(k, v, &["query", "collector"])?,
                "--docset-via" => o.docset_via = one_of(k, v, &["docset", "vec"])?,
                "--agg" => o.agg = one_of(k, v, &["tuple", "separate", "custom"])?,
                _ => o.facets = one_of(k, v, &["tuple", "separate"])?,
            }
            i += 2;
        }
        if o.hide_pct.is_some() && o.hide_file.is_some() {
            return Err("--hide-units and --hide are alternatives".into());
        }
        Ok(o)
    }

    fn hiding(&self) -> bool {
        self.hide_pct.is_some() || self.hide_file.is_some()
    }

    /// The options that are on, verbatim, minus those with their own result field.
    fn variant(&self) -> Option<String> {
        let mut out = Vec::new();
        let mut i = 0;
        while i < self.raw.len() {
            let k = &self.raw[i];
            let n = if FLAGS.contains(&k.as_str()) { 1 } else { 2 };
            if !OWN_FIELD.contains(&k.as_str()) {
                out.extend(self.raw[i..(i + n).min(self.raw.len())].iter().cloned());
            }
            i += n;
        }
        (!out.is_empty()).then(|| out.join(" "))
    }

    fn phase(&self) -> String {
        if let Some(p) = &self.phase {
            return p.clone();
        }
        let mut p = "query".to_string();
        if let Some(n) = self.threads {
            p += &format!("-s{n}");
        }
        if !self.filter.is_empty() {
            p += "-filtered";
        }
        if let Some(n) = self.page_no {
            p += &format!("-p{n}");
        }
        if let Some(v) = self.variant() {
            for part in v.split("--").filter(|s| !s.trim().is_empty()) {
                let tag: String = part.chars().filter(|c| c.is_ascii_alphanumeric() || *c == '.').collect();
                p += &format!("-{tag}");
            }
        }
        p
    }
}

fn one_of(k: &str, v: String, allowed: &[&str]) -> Result<String> {
    if allowed.contains(&v.as_str()) {
        Ok(v)
    } else {
        Err(format!("{k} is one of {}", allowed.join("|")).into())
    }
}

/// Corpus statistics per searcher generation, computed when the reader warms.
struct StatsWarmer(Mutex<HashMap<u64, CorpusStats>>, &'static str);

impl Warmer for StatsWarmer {
    fn warm(&self, s: &Searcher) -> tantivy::Result<()> {
        let st = corpus_stats_of(s.segment_readers(), None, self.1)?;
        self.0.lock().unwrap().insert(s.generation().generation_id(), st);
        Ok(())
    }
    fn garbage_collect(&self, live: &[&tantivy::SearcherGeneration]) {
        let keep: Vec<u64> = live.iter().map(|g| g.generation_id()).collect();
        self.0.lock().unwrap().retain(|k, _| keep.contains(k));
    }
}

/// Everything a run reads, built once per process.
struct Ctx {
    mode: String,
    o: Opts,
    index: Index,
    f: Fields,
    s: Searcher,
    an: Analyzer,
    _reader: tantivy::IndexReader,
    warmer: Option<Arc<StatsWarmer>>,
    hide: Option<Arc<HideSet>>,
    hide_build_ms: Option<f64>,
    /// alive-set route: the searcher's segments with live AND visible as alive.
    readers: Option<Vec<SegmentReader>>,
    live: Option<LiveStats>,
    /// product-topk's side files, by segment.
    blocks: Option<tbench::topk::Blocks>,
    id_fast: bool,
    /// The id fast column of each searcher segment, opened once.
    id_cols: Vec<Option<tantivy::columnar::StrColumn>>,
    open_ms: f64,
}

fn open(dir: &str, mode: &str, o: &Opts) -> Result<Ctx> {
    let t0 = Instant::now();
    let path = std::path::Path::new(dir);
    let (mut index, f) = match &o.madvise {
        None => tbench::open_index(path, None)?,
        Some(a) => {
            let adv = match a.as_str() {
                "normal" => Advice::Normal,
                "random" => Advice::Random,
                "sequential" => Advice::Sequential,
                _ => Advice::WillNeed,
            };
            tbench::open_index_in(path, Box::new(MmapDirectory::open_with_madvice(path, adv)?))?
        }
    };
    if let Some(n) = o.threads {
        index.set_multithread_executor(n)?;
    }
    let an = Analyzer::for_index(&index, &f);
    let warmer = o.warm_stats.then(|| Arc::new(StatsWarmer(Mutex::new(HashMap::new()), dl_of(mode, &f))));
    let mut rb = index.reader_builder().reload_policy(ReloadPolicy::Manual);
    if let Some(n) = o.doc_cache {
        rb = rb.doc_store_cache_num_blocks(n);
    }
    if let Some(w) = &warmer {
        let weak: std::sync::Weak<dyn Warmer> = Arc::downgrade(&(w.clone() as Arc<dyn Warmer>));
        rb = rb.warmers(vec![weak]);
    }
    let reader = rb.try_into()?;
    let s = reader.searcher();
    let open_ms = t0.elapsed().as_secs_f64() * 1e3;
    let t1 = Instant::now();
    let hide = match (&o.hide_pct, &o.hide_file) {
        (Some(p), _) => Some(Arc::new(HideSet::build(&s, *p, o.hide_seed)?)),
        (_, Some(file)) => Some(Arc::new(HideSet::from_addresses(&s, file)?)),
        _ => None,
    };
    let readers = match (&hide, o.hide_via.as_str()) {
        (Some(h), "alive-set") => Some(alive_readers(&index, &s, h)?),
        _ => None,
    };
    let hide_build_ms = hide.as_ref().map(|_| t1.elapsed().as_secs_f64() * 1e3);
    if let Some(h) = &hide {
        eprintln!("visibility: {} documents hidden, built in {:.3} ms (via {})", h.hidden_docs, hide_build_ms.unwrap_or(0.0), o.hide_via);
    }
    let live = match o.stats.as_str() {
        "default" => None,
        u => Some(LiveStats::new(&s, f, u == "live-union", hide.clone())),
    };
    let stored = variants::IndexConfig::load(path).store;
    let id_fast = o.id_fast.unwrap_or(!stored);
    if !id_fast && !stored {
        return Err("--id-source store needs an index that stores id".into());
    }
    let id_cols = s.segment_readers().iter().map(|r| r.fast_fields().str("id")).collect::<tantivy::Result<Vec<_>>>()?;
    // product-topk reads its side files at open, timed with the open (--time-open).
    let t2 = Instant::now();
    let blocks = if mode == "product-topk" { Some(tbench::topk::load(path, s.segment_readers(), dl_of(mode, &f))?) } else { None };
    let open_ms = open_ms + t2.elapsed().as_secs_f64() * 1e3;
    Ok(Ctx { mode: mode.into(), o: o.clone(), index, f, s, an, _reader: reader, warmer, hide, hide_build_ms, readers, live, blocks, id_fast, id_cols, open_ms })
}

impl Ctx {
    /// The library query with query-side filters and, on the weight route, the
    /// visibility wrapper.
    fn query(&self, text: &str) -> Result<Box<dyn Query>> {
        let q = build_query(text, &self.index, &self.f, &self.an, &self.o.q, &self.o.filter)?;
        Ok(match (&self.hide, self.o.hide_via.as_str()) {
            (Some(h), "weight") => Box::new(HideQuery { inner: q, hide: h.clone() }),
            _ => q,
        })
    }

    fn stats_provider(&self) -> Option<&dyn Bm25StatisticsProvider> {
        self.live.as_ref().map(|l| l as &dyn Bm25StatisticsProvider)
    }

    /// Runs `c` with every collector-side filter wrapped around it.
    fn exec<C: Collector>(&self, q: &dyn Query, c: C) -> Result<C::Fruit> {
        let kinds = self.o.filter.kinds.clone();
        if kinds.is_empty() {
            return self.exec_path(q, c);
        }
        match self.o.q.kind_via {
            KindVia::Bytes => {
                let p = move |b: &[u8]| kinds.iter().any(|k| k.as_bytes() == b);
                self.exec_path(q, BytesFilterCollector::new("kind".into(), p, c))
            }
            KindVia::Code => {
                if self.f.kind_code.is_none() {
                    return Err("--kind-filter code needs an index built with --kind-code".into());
                }
                let codes: Vec<u64> = kinds.iter().map(|k| variants::kind_code(k)).collect();
                let p = move |v: u64| codes.contains(&v);
                self.exec_path(q, FilterCollector::new("kind_code".into(), p, c))
            }
            KindVia::Collector => {
                self.exec_path(q, KeepCollector { inner: c, filter: Filter { kinds, prefixes: Vec::new() } })
            }
            _ => self.exec_path(q, c),
        }
    }

    fn exec_path<C: Collector>(&self, q: &dyn Query, c: C) -> Result<C::Fruit> {
        if self.o.q.path_via == PathVia::Collector && !self.o.filter.prefixes.is_empty() {
            let filter = Filter { kinds: Vec::new(), prefixes: self.o.filter.prefixes.clone() };
            return self.exec_hide(q, KeepCollector { inner: c, filter });
        }
        self.exec_hide(q, c)
    }

    fn exec_hide<C: Collector>(&self, q: &dyn Query, c: C) -> Result<C::Fruit> {
        match (&self.hide, self.o.hide_via.as_str()) {
            (Some(h), "collector") => self.exec_final(q, &HideCollector { inner: c, hide: h.clone() }),
            _ => self.exec_final(q, &c),
        }
    }

    fn scoring(&self, needs: bool) -> EnableScoring<'_> {
        match (needs, self.stats_provider()) {
            (false, _) => EnableScoring::disabled_from_searcher(&self.s),
            (true, Some(p)) => EnableScoring::enabled_from_statistics_provider(p, &self.s),
            (true, None) => EnableScoring::enabled_from_searcher(&self.s),
        }
    }

    fn exec_final<C: Collector>(&self, q: &dyn Query, c: &C) -> Result<C::Fruit> {
        if let Some(rs) = &self.readers {
            // The alive-set route: Searcher::search_with_executor's body over the
            // reopened segments, on this thread.
            let w = q.weight(self.scoring(c.requires_scoring()))?;
            let mut fruits = Vec::with_capacity(rs.len());
            for (ord, r) in rs.iter().enumerate() {
                fruits.push(c.collect_segment(w.as_ref(), ord as u32, r)?);
            }
            return Ok(c.merge_fruits(fruits)?);
        }
        Ok(match self.stats_provider() {
            Some(p) => self.s.search_with_statistics_provider(q, c, p)?,
            None => self.s.search(q, c)?,
        })
    }

    fn id(&self, addr: DocAddress) -> Result<String> {
        if self.id_fast {
            return Ok(id_of(&self.id_cols[addr.segment_ord as usize], addr.doc_id));
        }
        let d: TantivyDocument = self.s.doc(addr)?;
        Ok(d.get_first(self.f.id).and_then(|v| v.as_str()).unwrap_or_default().to_string())
    }

    fn top(&self) -> TopDocs {
        TopDocs::with_limit(PAGE).and_offset((self.o.page_no.unwrap_or(1) - 1) * PAGE)
    }

    /// library mode's page: (ids, addresses with scores, hits if the collector counted).
    fn library_page(&self, q: &dyn Query) -> Result<(Vec<String>, Vec<(Score, DocAddress)>, Option<u64>)> {
        let (top, hits): (Vec<(Score, DocAddress)>, Option<u64>) = match self.o.collect.as_str() {
            "tuple-count" => {
                let (t, n) = self.exec(q, (self.top().order_by_score(), Count))?;
                (t, Some(n as u64))
            }
            "tuple-id" => {
                let key = ((SortBySimilarityScore, Order::Desc), (SortByString::for_field("id"), Order::Asc));
                let t = self.exec(q, self.top().order_by(key))?;
                (t.into_iter().map(|((s, _), a)| (s, a)).collect(), None)
            }
            "tweak" => {
                let t = self.exec(q, self.top().tweak_score(|_: &SegmentReader| |_: DocId, s: Score| s))?;
                (t, None)
            }
            _ => (self.exec(q, self.top().order_by_score())?, None),
        };
        let mut ids = Vec::with_capacity(top.len());
        for (_, a) in &top {
            ids.push(self.id(*a)?);
        }
        Ok((ids, top, hits))
    }

    fn product_stats(&self, cache: &mut Option<CorpusStats>) -> Result<CorpusStats> {
        if let Some(st) = cache {
            return Ok(*st);
        }
        let st = if let Some(w) = &self.warmer {
            *w.0.lock().unwrap().get(&self.s.generation().generation_id()).ok_or("the warmer has not seen this searcher")?
        } else {
            match &self.readers {
                Some(rs) => corpus_stats_of(rs, None, dl_of(&self.mode, &self.f))?,
                None => corpus_stats_of(self.s.segment_readers(), self.hide.as_deref(), dl_of(&self.mode, &self.f))?,
            }
        };
        Ok(*cache.insert(st))
    }

    /// One timed run: (wall ms, hits, page).
    fn run(&self, text: &str, stats: &mut Option<CorpusStats>) -> Result<(f64, u64, Vec<String>)> {
        let t = Instant::now();
        let (hits, page) = match self.mode.as_str() {
            "library" => {
                let q = self.query(text)?;
                let (ids, _, counted) = self.library_page(&*q)?;
                let wall = t.elapsed().as_secs_f64() * 1e3;
                let hits = match counted {
                    Some(n) => n,
                    None => self.exec(&*q, Count)? as u64,
                };
                return Ok((wall, hits, ids));
            }
            "library-all" | "library-combined" => {
                let q = self.query(text)?;
                let mut all = self.exec(&*q, AllScores)?;
                let hits = all.len() as u64;
                // Only documents scoring at least the (n*PAGE)-th best can be served
                // on pages 1..=n; the ids that break their ties are read for those alone.
                let depth = self.o.page_no.unwrap_or(1) * PAGE;
                if all.len() > depth {
                    all.select_nth_unstable_by(depth - 1, |a, b| b.0.total_cmp(&a.0));
                    let floor = all[depth - 1].0;
                    all.retain(|x| x.0 >= floor);
                }
                let mut keyed: Vec<(Score, String)> = all.iter().map(|(sc, col, d, _)| (*sc, id_of(col, *d))).collect();
                keyed.sort_by(|a, b| b.0.total_cmp(&a.0).then_with(|| a.1.cmp(&b.1)));
                (hits, keyed.into_iter().skip(depth - PAGE).take(PAGE).map(|x| x.1).collect())
            }
            "product-topk" => {
                let st = self.product_stats(stats)?;
                let dl = dl_of(&self.mode, &self.f);
                let terms = if dl == tbench::product::DL_CODE { parse_query_with(text, true) } else { parse_query(text) };
                let blocks = self.blocks.clone().ok_or("product-topk needs the block-max files (tblock)")?;
                let hide = if self.readers.is_some() { None } else { self.hide.clone() };
                let dfc = DfCollector { terms: terms.clone(), fields: self.f, hide: hide.clone(), blocks: blocks.clone(), dl };
                let a = self.exec_final(&AllQuery, &dfc)?;
                let c = TopkCollector {
                    terms: terms.clone(), fields: self.f, stats: st, filter: self.o.filter.clone(), hide: hide.clone(), blocks, dl,
                    page_no: self.o.page_no.unwrap_or(1), a: Arc::new(a),
                };
                let ans = self.exec_final(&AllQuery, &c)?;
                let wall = t.elapsed().as_secs_f64() * 1e3;
                // hits, untimed and apart, as library mode counts them: product's own count.
                let pc = ProductCollector { terms, fields: self.f, stats: st, filter: self.o.filter.clone(), hide, via_weight: false, dl, page_no: 1 };
                let hits = self.exec_final(&AllQuery, &pc)?.hits;
                eprintln!(
                    "  product-topk {text:?}: blocks scored {} of {}, documents scored {}",
                    ans.blocks_scored, ans.blocks_total, ans.docs_scored
                );
                return Ok((wall, hits, ans.page));
            }
            "product" | "product-block" | "product-code" => {
                let st = self.product_stats(stats)?;
                let block = self.mode == "product-block";
                let c = ProductCollector {
                    terms: if self.mode == "product-code" { parse_query_with(text, true) } else { parse_query(text) },
                    dl: dl_of(&self.mode, &self.f),
                    page_no: self.o.page_no.unwrap_or(1),
                    fields: self.f,
                    stats: st,
                    filter: self.o.filter.clone(),
                    hide: if self.readers.is_some() { None } else { self.hide.clone() },
                    via_weight: block,
                };
                let q: Box<dyn Query> = if block { library_query(text, &self.f, &Filter::default(), false)? } else { Box::new(AllQuery) };
                let ans = self.exec_final(&*q, &c)?;
                eprintln!("  {} {text:?}: {} candidate documents, {} hits", self.mode, ans.candidates, ans.hits);
                (ans.hits, ans.page)
            }
            "count" => {
                let q = self.query(text)?;
                let n = if self.o.count_via == "query" {
                    let w = q.weight(EnableScoring::disabled_from_searcher(&self.s))?;
                    let mut n = 0u64;
                    for r in self.readers.as_deref().unwrap_or(self.s.segment_readers()) {
                        n += w.count(r)? as u64;
                    }
                    n
                } else {
                    self.exec(&*q, Count)? as u64
                };
                (n, Vec::new())
            }
            "docset" => {
                let q = self.query(text)?;
                let n = if self.o.docset_via == "vec" { self.exec(&*q, VecDocs)? } else { self.exec(&*q, DocSetCollector)?.len() };
                (n as u64, Vec::new())
            }
            "agg-kind" => {
                let q = self.query(text)?;
                let (top, counts) = match self.o.agg.as_str() {
                    "custom" => self.exec(&*q, (self.top().order_by_score(), KindCounts))?,
                    "separate" => {
                        let top = self.exec(&*q, self.top().order_by_score())?;
                        (top, kind_buckets(self.exec(&*q, self.agg_collector()?)?)?)
                    }
                    _ => {
                        let (top, a) = self.exec(&*q, (self.top().order_by_score(), self.agg_collector()?))?;
                        (top, kind_buckets(a)?)
                    }
                };
                let mut ids = Vec::with_capacity(top.len());
                for (_, a) in &top {
                    ids.push(self.id(*a)?);
                }
                let wall = t.elapsed().as_secs_f64() * 1e3;
                let mut v: Vec<_> = counts.into_iter().collect();
                v.sort();
                eprintln!("  agg-kind {text:?}: {v:?}");
                return Ok((wall, v.iter().map(|x| x.1).sum(), ids));
            }
            "facets" => {
                let fp = self.f.path_facet.ok_or("facets mode needs an index built with --path-facet")?;
                let q = self.query(text)?;
                let mut fc = FacetCollector::for_field(self.index.schema().get_field_name(fp));
                fc.add_facet("/");
                let (top, counts) = if self.o.facets == "separate" {
                    let top = self.exec(&*q, self.top().order_by_score())?;
                    (top, self.exec(&*q, fc)?)
                } else {
                    self.exec(&*q, (self.top().order_by_score(), fc))?
                };
                let mut ids = Vec::with_capacity(top.len());
                for (_, a) in &top {
                    ids.push(self.id(*a)?);
                }
                let wall = t.elapsed().as_secs_f64() * 1e3;
                let roots: Vec<(String, u64)> = counts.get("/").map(|(f, n)| (f.to_string(), n)).collect();
                eprintln!("  facets {text:?}: {} top-level folders, {} docs under them", roots.len(), roots.iter().map(|x| x.1).sum::<u64>());
                return Ok((wall, roots.iter().map(|x| x.1).sum(), ids));
            }
            m => return Err(format!("unknown mode {m}").into()),
        };
        Ok((t.elapsed().as_secs_f64() * 1e3, hits, page))
    }

    fn agg_collector(&self) -> Result<AggregationCollector> {
        let req = serde_json::json!({"kinds": {"terms": {"field": "kind", "size": 1000}}});
        let aggs = serde_json::from_value(req)?;
        Ok(AggregationCollector::from_aggs(aggs, AggContextParams::new(AggregationLimitsGuard::new(None, None), self.index.tokenizers().clone())))
    }

    /// Untimed run of explain() of the first k hits, each timed, each checked
    /// against its collected score; appended to <arm>-<phase>-explain.jsonl.
    fn write_explain(&self, arm: &str, phase: &str, text: &str, k: usize) -> Result<()> {
        let q = self.query(text)?;
        let (ids, top, _) = self.library_page(&*q)?;
        let w = q.weight(self.scoring(true))?;
        let dir = results_dir();
        std::fs::create_dir_all(&dir)?;
        let mut out = std::fs::OpenOptions::new().create(true).append(true).open(dir.join(format!("{arm}-{phase}-explain.jsonl")))?;
        for (rank, ((score, addr), id)) in top.iter().zip(ids).take(k).enumerate() {
            let reader = match &self.readers {
                Some(rs) => &rs[addr.segment_ord as usize],
                None => self.s.segment_reader(addr.segment_ord),
            };
            let t = Instant::now();
            let e = w.explain(reader, addr.doc_id)?;
            let ms = t.elapsed().as_secs_f64() * 1e3;
            let ex: serde_json::Value = serde_json::from_str(&e.to_pretty_json())?;
            let matches = e.value() == *score;
            writeln!(
                out,
                "{}",
                serde_json::json!({"query": text, "rank": rank + 1, "id": id, "score": score, "explain_ms": ms,
                    "value_matches_score": matches, "explanation": ex})
            )?;
        }
        // --explain-qrels: each relevant document too, with its exhaustive rank
        // (1 + the documents scoring strictly above it).
        let Some(file) = &self.o.explain_qrels else { return Ok(()) };
        let rel: Vec<String> = std::fs::read_to_string(file)?
            .lines()
            .filter_map(|l| l.split_once('\t'))
            .filter(|(qq, _)| *qq == text)
            .map(|(_, k)| k.to_string())
            .collect();
        if rel.is_empty() {
            return Ok(());
        }
        let all = self.exec(&*q, AllScores)?;
        for id in rel {
            let idq = tantivy::query::TermQuery::new(tantivy::Term::from_field_text(self.f.id, &id), tantivy::schema::IndexRecordOption::Basic);
            for addr in self.s.search(&idq, &DocSetCollector)? {
                let reader = self.s.segment_reader(addr.segment_ord);
                let e = w.explain(reader, addr.doc_id).ok();
                let score = e.as_ref().map(|e| e.value());
                let rank = score.map(|sc| 1 + all.iter().filter(|x| x.0 > sc).count());
                let ex: serde_json::Value = match &e {
                    Some(e) => serde_json::from_str(&e.to_pretty_json())?,
                    None => serde_json::Value::Null,
                };
                writeln!(out, "{}", serde_json::json!({"query": text, "relevant": true, "rank": rank, "id": id, "score": score, "explanation": ex}))?;
            }
        }
        Ok(())
    }
}

/// agg-kind's bucket counts from the aggregation result.
fn kind_buckets(r: tantivy::aggregation::agg_result::AggregationResults) -> Result<HashMap<String, u64>> {
    let v = serde_json::to_value(&r)?;
    let mut out = HashMap::new();
    for b in v["kinds"]["buckets"].as_array().cloned().unwrap_or_default() {
        out.insert(b["key"].as_str().unwrap_or_default().to_string(), b["doc_count"].as_u64().unwrap_or(0));
    }
    if let Some(n) = v["kinds"]["sum_other_doc_count"].as_u64().filter(|n| *n > 0) {
        return Err(format!("the terms aggregation left {n} documents in other buckets").into());
    }
    Ok(out)
}

/// agg-kind --agg custom: hits per kind ordinal, resolved to strings once per segment.
struct KindCounts;
struct KindCountsSeg {
    col: Option<tantivy::columnar::StrColumn>,
    counts: Vec<u64>,
}
impl Collector for KindCounts {
    type Fruit = HashMap<String, u64>;
    type Child = KindCountsSeg;
    fn for_segment(&self, _: u32, seg: &SegmentReader) -> tantivy::Result<KindCountsSeg> {
        let col = seg.fast_fields().str("kind")?;
        let n = col.as_ref().map_or(0, |c| c.num_terms());
        Ok(KindCountsSeg { col, counts: vec![0; n] })
    }
    fn requires_scoring(&self) -> bool {
        false
    }
    fn merge_fruits(&self, fruits: Vec<HashMap<String, u64>>) -> tantivy::Result<HashMap<String, u64>> {
        let mut out = HashMap::new();
        for f in fruits {
            for (k, n) in f {
                *out.entry(k).or_insert(0) += n;
            }
        }
        Ok(out)
    }
}
impl SegmentCollector for KindCountsSeg {
    type Fruit = HashMap<String, u64>;
    fn collect(&mut self, doc: DocId, _: Score) {
        if let Some(c) = &self.col {
            for o in c.term_ords(doc) {
                self.counts[o as usize] += 1;
            }
        }
    }
    fn collect_block(&mut self, docs: &[DocId]) {
        for &d in docs {
            self.collect(d, 0.0);
        }
    }
    fn harvest(self) -> HashMap<String, u64> {
        let mut out = HashMap::new();
        if let Some(c) = &self.col {
            for (o, n) in self.counts.iter().enumerate().filter(|x| *x.1 > 0) {
                let mut s = String::new();
                let _ = c.ord_to_str(o as u64, &mut s);
                out.insert(s, *n);
            }
        }
        out
    }
}

const MODES: [&str; 11] = [
    "library", "library-all", "library-combined", "product", "product-block", "product-code", "product-topk", "count", "docset", "agg-kind",
    "facets",
];

/// The document-length column a mode reads on an index: product-code reads the
/// code tokenizer's count; product-topk follows the index (code or unicode61).
fn dl_of(mode: &str, f: &Fields) -> &'static str {
    let code = mode == "product-code" || (mode == "product-topk" && f.token_count_code.is_some());
    if code { tbench::product::DL_CODE } else { tbench::product::DL }
}

/// library-combined's default shape.
fn normalize(mode: &str, o: &mut Opts) {
    if mode == "library-combined" && o.q.shape == Shape::Nested {
        o.q.shape = Shape::Combined;
    }
}

fn check(mode: &str, o: &Opts, cx: &Ctx) -> Result<()> {
    let product = mode.starts_with("product");
    if o.page_no.is_some() && !["library", "library-all", "library-combined"].contains(&mode) && !product {
        return Err("--page applies to the library and product modes".into());
    }
    if o.explain.is_some() && mode != "library" {
        return Err("--explain applies to library mode".into());
    }
    if o.collect != "top" && mode != "library" {
        return Err("--collect applies to library mode".into());
    }
    if mode == "product-code" {
        if crate_tokenizer(cx) != tbench::CODE_TOKENIZER || cx.f.token_count_code.is_none() {
            return Err("product-code needs an index built with --tokenizer code (it stores token_count_code)".into());
        }
    } else if mode == "product-topk" {
        let code = crate_tokenizer(cx) == tbench::CODE_TOKENIZER && cx.f.token_count_code.is_some();
        if !code && crate_tokenizer(cx) != tbench::TOKENIZER {
            return Err("product-topk needs a unicode61 index, or a code index with token_count_code".into());
        }
    } else if product && crate_tokenizer(cx) != tbench::TOKENIZER {
        return Err("product mode reproduces our engine and needs a unicode61 index".into());
    }
    if mode == "library-combined" && !matches!(o.q.shape, Shape::Combined | Shape::CombinedFields(_)) {
        return Err("library-combined takes --shape combined or combined+fields[:W]".into());
    }
    if product && (o.stats != "default" || o.q.shape != Shape::Nested || o.q.slop != 0 || o.q.kind_via != KindVia::Terms || o.q.path_via != PathVia::Regex) {
        return Err("product mode has its own scoring and filters; --stats, --shape and the filter routes apply to the library family".into());
    }
    if o.warm_stats && (!product || o.hiding()) {
        return Err("--warm-stats applies to product mode without hiding (the warmer runs before the visibility set exists)".into());
    }
    if o.hide_via == "alive-set" && o.threads.is_some() {
        return Err("the alive-set route searches its reopened segments on one thread".into());
    }
    if mode == "count" && o.count_via == "query" {
        let collector_side = o.q.kind_via != KindVia::Terms && o.q.kind_via != KindVia::TermSet && !o.filter.kinds.is_empty()
            || o.q.path_via == PathVia::Collector && !o.filter.prefixes.is_empty()
            || o.hiding() && o.hide_via == "collector";
        if collector_side {
            return Err("--count-via query counts the Weight; collector-side filters need --count-via collector".into());
        }
    }
    Ok(())
}

fn crate_tokenizer(cx: &Ctx) -> String {
    tbench::index_tokenizer(&cx.index, &cx.f)
}

/// (wall, hits, page, open ms, hide build ms) from a cold child.
type Cold = (f64, u64, Vec<String>, f64, Option<f64>);

fn main() -> Result<()> {
    let a: Vec<String> = std::env::args().collect();
    if a.len() >= 5 && a[1] == "--cold" {
        let mut o = Opts::parse(&a[5..])?;
        normalize(&a[3], &mut o);
        let cx = open(&a[2], &a[3], &o)?;
        let (wall, hits, page) = cx.run(&a[4], &mut None)?;
        let out: Cold = (wall, hits, page, cx.open_ms, cx.hide_build_ms);
        println!("{}", serde_json::to_string(&out)?);
        return Ok(());
    }
    if a.len() < 5 || !MODES.contains(&a[4].as_str()) {
        return Err(format!("usage: tquery <dir> <queries.txt> <runs> <{}> [options]", MODES.join("|")).into());
    }
    let (dir, mode, runs) = (&a[1], &a[4], a[3].parse::<u32>()?);
    let mut o = Opts::parse(&a[5..])?;
    normalize(mode, &mut o);
    let arm = format!("tantivy-{mode}");
    let phase = o.phase();
    let queries = std::fs::read_to_string(&a[2])?;
    let cx = open(dir, mode, &o)?;
    check(mode, &o, &cx)?;
    // The warm process reads the corpus statistics once, untimed, as our reader does at open.
    let mut stats = if mode.starts_with("product") { Some(cx.product_stats(&mut None)?) } else { None };
    let hidden_pct = o.hide_pct.or(o.hide_file.as_ref().map(|_| cx.hide.as_ref().map_or(0.0, |h| {
        let n: u64 = cx.s.segment_readers().iter().map(|r| r.num_docs() as u64).sum();
        if n == 0 { 0.0 } else { 100.0 * h.hidden_docs as f64 / n as f64 }
    })));
    let stats_include_hidden = o.hiding().then(|| !mode.starts_with("product") && o.stats == "default");
    let exe = std::env::current_exe()?;
    let side = |name: &str, v: serde_json::Value| -> Result<()> {
        let d = results_dir();
        std::fs::create_dir_all(&d)?;
        let mut f = std::fs::OpenOptions::new().create(true).append(true).open(d.join(format!("{arm}-{phase}-{name}.jsonl")))?;
        writeln!(f, "{v}")?;
        Ok(())
    };
    for q in queries.lines().filter(|l| !l.is_empty()) {
        let out = std::process::Command::new(&exe).args(["--cold", dir, mode, q]).args(&o.raw).output()?;
        if !out.status.success() {
            return Err(format!("cold run of {q:?} failed: {}", String::from_utf8_lossy(&out.stderr)).into());
        }
        let (wall, hits, page, open_ms, cold_hide_ms): Cold = serde_json::from_slice(&out.stdout)?;
        if o.time_open {
            side("open", serde_json::json!({"query": q, "open_ms": open_ms, "warm_stats": o.warm_stats, "madvise": o.madvise}))?;
        }
        let line = |run, wall_ms, hits, page, hide_ms| QueryLine {
            arm: arm.clone(), phase: phase.clone(), query: q.into(), run, wall_ms, hits, page,
            filter: o.filter.describe(), search_threads: o.threads, page_no: o.page_no, variant: o.variant(),
            hidden_pct, stats_include_hidden, hide_build_ms: hide_ms,
        };
        append_result(&arm, &phase, &line(0, wall, hits, page.clone(), cold_hide_ms))?;
        let mut warm = Vec::new();
        for r in 1..=runs {
            let (w, h, p) = cx.run(q, &mut stats)?;
            if h != hits || p != page {
                return Err(format!("warm run {r} of {q:?} answered differently from the cold run").into());
            }
            warm.push(w);
            append_result(&arm, &phase, &line(r, w, h, p, cx.hide_build_ms))?;
        }
        if let Some(k) = o.explain {
            cx.write_explain(&arm, &phase, q, k)?;
        }
        if o.doc_cache.is_some() {
            let c = cx.s.doc_store_cache_stats();
            side("cache", serde_json::json!({"query": q, "num_entries": c.num_entries, "cache_hits": c.cache_hits, "cache_misses": c.cache_misses}))?;
        }
        warm.sort_by(f64::total_cmp);
        let med = warm.get(warm.len() / 2).copied().unwrap_or(f64::NAN);
        eprintln!("{arm}/{phase} {q:?}: hits {hits}, page {}, cold {wall:.3} ms, warm median {med:.3} ms", page.len());
    }
    Ok(())
}
