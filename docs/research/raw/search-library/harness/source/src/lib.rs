//! Shared pieces of the tantivy bench arm: the contract document, the schema,
//! query parsing, our BM25F arithmetic, and run accounting.

pub mod library;
pub mod variants;
pub mod product;
pub mod topk;
pub mod unicode61;

use serde::{Deserialize, Serialize};
use std::io::{BufRead, Write};
use std::path::{Path, PathBuf};
use tantivy::schema::{Field, Schema};
use tantivy::Index;

/// One `docs.jsonl` line: model.SearchUnit plus its unit.
#[derive(Deserialize, Serialize, Clone, Debug)]
pub struct Doc {
    pub id: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub node_id: String,
    pub file_id: String,
    pub path: String,
    pub kind: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub name: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub qualified_name: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub signature: String,
    pub bytes: Bytes,
    pub body: String,
    pub token_count: i64,
    pub unit: String,
}

#[derive(Deserialize, Serialize, Clone, Copy, Debug)]
pub struct Bytes {
    pub start: u64,
    pub end: u64,
}

impl Doc {
    /// The five indexed columns in search_fts order.
    pub fn columns(&self) -> [&str; 5] {
        [&self.name, &self.qualified_name, &self.signature, &self.path, &self.body]
    }
    /// The unicode61 token count over all five columns: search_units.token_count.
    pub fn unicode61_count(&self) -> u64 {
        self.columns().iter().map(|c| unicode61::count(c)).sum()
    }
}

/// Streams `docs.jsonl`, calling `f` per document in file order.
pub fn read_docs(path: &Path, mut f: impl FnMut(Doc) -> anyhow_lite::Result<()>) -> anyhow_lite::Result<()> {
    let file = std::fs::File::open(path).map_err(|e| format!("open {}: {e}", path.display()))?;
    let r = std::io::BufReader::with_capacity(1 << 20, file);
    for (n, line) in r.lines().enumerate() {
        let line = line?;
        if line.is_empty() {
            continue;
        }
        let d: Doc = serde_json::from_str(&line).map_err(|e| format!("{}:{}: {e}", path.display(), n + 1))?;
        f(d)?;
    }
    Ok(())
}

/// A tiny error alias so the binaries can `?` io, tantivy and string errors alike.
pub mod anyhow_lite {
    pub type Error = Box<dyn std::error::Error + Send + Sync>;
    pub type Result<T> = std::result::Result<T, Error>;
}

/// The tokenizer names registered on every index: unicode61 (our engine's), and
/// the code-aware variant (unicode61 plus camelCase parts), chosen at index time.
pub const TOKENIZER: &str = "unicode61";
pub const CODE_TOKENIZER: &str = "code";

/// BM25F column weights, search_fts order: name, qualified_name, signature, path, body.
pub const WEIGHTS: [f64; 5] = [5.0, 5.0, 2.0, 2.0, 1.0];

/// The schema's fields, resolved by name from an index's schema. `text` are the
/// five indexed columns; the rest are identity and comparator keys. The optional
/// fields exist only on indexes built with the matching `tindex` schema option.
#[derive(Clone, Copy)]
pub struct Fields {
    pub text: [Field; 5],
    pub id: Field,
    pub node_id: Field,
    pub kind: Field,
    pub path_s: Field,
    pub name_s: Field,
    pub qn_s: Field,
    pub sig_s: Field,
    pub unit: Field,
    pub token_count: Field,
    pub start: Field,
    pub end: Field,
    /// `--path-facet`: the path as a facet.
    pub path_facet: Option<Field>,
    /// `--kind-code`: the kind as a stable u64 code (variants::kind_code).
    pub kind_code: Option<Field>,
    /// `--parts-fields`: camelCase parts of name and qualified name, parts only.
    pub name_parts: Option<Field>,
    pub qn_parts: Option<Field>,
    /// `--name-ngram`: 3-grams of the name.
    pub name_ngram: Option<Field>,
    /// `--field-counts`: the unicode61 token count of each text column.
    pub counts: Option<[Field; 5]>,
    /// `--combined`: the five columns as values of one text field.
    pub all: Option<Field>,
    /// `--column-lengths`: each column's exact token count (product::LEN_NAMES).
    pub lens: Option<[Field; 5]>,
    /// `--tokenizer code`: the code tokenizer's token count (product-code's length).
    pub token_count_code: Option<Field>,
    /// `--tokenizer pretokenized`: documents carry unicode61 tokens directly.
    pub pretokenized: bool,
}

pub const TEXT_NAMES: [&str; 5] = ["name", "qualified_name", "signature", "path", "body"];
pub const COUNT_NAMES: [&str; 5] = ["tc_name", "tc_qualified_name", "tc_signature", "tc_path", "tc_body"];

impl Fields {
    pub fn from_schema(s: &Schema, pretokenized: bool) -> anyhow_lite::Result<Fields> {
        let g = |n: &str| s.get_field(n).map_err(|e| format!("schema has no field {n}: {e}"));
        let o = |n: &str| s.get_field(n).ok();
        let lens = product::LEN_NAMES.iter().map(|n| o(n)).collect::<Option<Vec<_>>>().map(|v| [v[0], v[1], v[2], v[3], v[4]]);
        let counts = COUNT_NAMES.iter().map(|n| o(n)).collect::<Option<Vec<_>>>().map(|v| [v[0], v[1], v[2], v[3], v[4]]);
        Ok(Fields {
            text: [g(TEXT_NAMES[0])?, g(TEXT_NAMES[1])?, g(TEXT_NAMES[2])?, g(TEXT_NAMES[3])?, g(TEXT_NAMES[4])?],
            id: g("id")?, node_id: g("node_id")?, kind: g("kind")?, path_s: g("path_s")?, name_s: g("name_s")?,
            qn_s: g("qn_s")?, sig_s: g("sig_s")?, unit: g("unit")?, token_count: g("token_count")?,
            start: g("start")?, end: g("end")?,
            path_facet: o("path_facet"), kind_code: o("kind_code"), name_parts: o("name_parts"), qn_parts: o("qn_parts"),
            name_ngram: o("name_ngram"), counts, lens, all: o("all"), token_count_code: o(product::DL_CODE), pretokenized,
        })
    }
}

/// Opens an existing index (its schema and `tb-config.json` say how it was
/// built), or creates one from `create`. Every analyzer the variants use is
/// registered either way.
pub fn open_index(dir: &Path, create: Option<&variants::IndexConfig>) -> anyhow_lite::Result<(Index, Fields)> {
    let index = match create {
        Some(cfg) => variants::create(dir, cfg)?,
        None => Index::open_in_dir(dir)?,
    };
    variants::register_analyzers(&index);
    let cfg = variants::IndexConfig::load(dir);
    let fields = Fields::from_schema(&index.schema(), cfg.tokenizer == "pretokenized")?;
    Ok((index, fields))
}

/// Opens an existing index through a given directory (for example an mmap
/// directory with madvise), registering the analyzers.
pub fn open_index_in(dir: &Path, directory: Box<dyn tantivy::Directory>) -> anyhow_lite::Result<(Index, Fields)> {
    let index = Index::open(directory)?;
    variants::register_analyzers(&index);
    let cfg = variants::IndexConfig::load(dir);
    let fields = Fields::from_schema(&index.schema(), cfg.tokenizer == "pretokenized")?;
    Ok((index, fields))
}

pub fn to_tantivy(d: &Doc, f: &Fields) -> tantivy::TantivyDocument {
    variants::to_doc(d, f)
}

/// One scored query unit: a token, or a quoted phrase's tokens (lexical.go lexicalTerm).
#[derive(Clone, Debug)]
pub struct QTerm {
    pub tokens: Vec<String>,
}

impl QTerm {
    pub fn phrase(&self) -> bool {
        self.tokens.len() > 1
    }
    pub fn key(&self) -> String {
        self.tokens.join(" ")
    }
}

/// lexical.go parseQuery + splitQuoted: quoted runs become phrases, the rest single
/// tokens; duplicates by key are folded, first occurrence kept; empty keys dropped.
pub fn parse_query(text: &str) -> Vec<QTerm> {
    parse_query_with(text, false)
}

/// The tokenizer an existing index's text fields were built with.
pub fn index_tokenizer(index: &Index, f: &Fields) -> String {
    match index.schema().get_field_entry(f.text[0]).field_type() {
        tantivy::schema::FieldType::Str(o) => {
            o.get_indexing_options().map(|i| i.tokenizer().to_string()).unwrap_or_default()
        }
        _ => String::new(),
    }
}

/// parse_query, and with `code` the code-aware variant: an unquoted run also
/// yields each camelCase part as its own term; a phrase stays whole tokens,
/// because parts share their token's position.
pub fn parse_query_with(text: &str, code: bool) -> Vec<QTerm> {
    let mut terms: Vec<QTerm> = Vec::new();
    let mut seen = std::collections::HashSet::new();
    let mut add = |t: QTerm| {
        let k = t.key();
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
        let toks = unicode61::tokenize(seg);
        if quoted {
            add(QTerm { tokens: toks });
        } else if code {
            unicode61::for_each_code_token(seg, |_, _, _, t, _| add(QTerm { tokens: vec![t.to_string()] }));
        } else {
            for t in toks {
                add(QTerm { tokens: vec![t] });
            }
        }
    }
    terms
}

/// Go's amd64 math.Log (math/log_amd64.s), operation for operation, for x > 0 finite.
pub fn go_log(x: f64) -> f64 {
    const HSQRT2: f64 = 7.07106781186547524401e-01;
    const LN2HI: f64 = 6.93147180369123816490e-01;
    const LN2LO: f64 = 1.90821492927058770002e-10;
    const L1: f64 = 6.666666666666735130e-01;
    const L2: f64 = 3.999999999940941908e-01;
    const L3: f64 = 2.857142874366239149e-01;
    const L4: f64 = 2.222219843214978396e-01;
    const L5: f64 = 1.818357216161805012e-01;
    const L6: f64 = 1.531383769920937332e-01;
    const L7: f64 = 1.479819860511658591e-01;
    if x.is_nan() || x == f64::INFINITY {
        return x;
    }
    if x < 0.0 {
        return f64::NAN;
    }
    if x == 0.0 {
        return f64::NEG_INFINITY;
    }
    let bits = x.to_bits();
    let mut f1 = f64::from_bits((bits & 0x000F_FFFF_FFFF_FFFF) | 0x3FE0_0000_0000_0000);
    let mut k = (((bits >> 52) & 0x7FF) as i32 - 0x3FE) as f64;
    // The assembly tests CMPSD not-less-than, i.e. f1 <= HSqrt2, where the pure-Go path says <.
    if f1 <= HSQRT2 {
        k -= 1.0;
        f1 *= 2.0;
    }
    let f = f1 - 1.0;
    let s = f / (2.0 + f);
    let s2 = s * s;
    let s4 = s2 * s2;
    let t1 = s2 * (L1 + s4 * (L3 + s4 * (L5 + s4 * L7)));
    let t2 = s4 * (L2 + s4 * (L4 + s4 * L6));
    let r = t1 + t2;
    let hfsq = 0.5 * f * f;
    k * LN2HI - ((hfsq - (s * (hfsq + r) + k * LN2LO)) - f)
}

/// bm25.go bm25IDF.
pub fn bm25_idf(n: i64, df: i64) -> f64 {
    if n <= 0 {
        return 0.0;
    }
    let df = df.clamp(0, n);
    go_log(1.0 + (n as f64 - df as f64 + 0.5) / (df as f64 + 0.5))
}

const K1: f64 = 1.2;
const B: f64 = 0.75;

/// bm25.go bm25Saturation, same evaluation order.
pub fn bm25_saturation(wtf: f64, dl: i64, avgdl: f64) -> f64 {
    if wtf <= 0.0 || avgdl <= 0.0 {
        return 0.0;
    }
    let dl = dl.max(0);
    wtf * (K1 + 1.0) / (wtf + K1 * (1.0 - B + B * dl as f64 / avgdl))
}

/// bm25.go quantizeScore.
pub fn quantize(score: f64) -> i64 {
    if !score.is_finite() {
        return 0;
    }
    (score * 1e6).round() as i64
}

/// The request filters (search.go hitFilter.keep and exact.go matchesKinds): a
/// kind must equal one of `kinds`; a path must equal a prefix or start with
/// prefix + "/". Prefixes are cleaned as newHitFilter does (trimmed, "\\" to "/",
/// trailing "/" dropped). Empty lists filter nothing.
#[derive(Clone, Debug, Default)]
pub struct Filter {
    pub kinds: Vec<String>,
    pub prefixes: Vec<String>,
}

impl Filter {
    pub fn is_empty(&self) -> bool {
        self.kinds.is_empty() && self.prefixes.is_empty()
    }
    pub fn clean_prefix(p: &str) -> String {
        p.trim().replace('\\', "/").trim_end_matches('/').to_string()
    }
    pub fn keep(&self, kind: &str, path: &str) -> bool {
        (self.kinds.is_empty() || self.kinds.iter().any(|k| k == kind))
            && (self.prefixes.is_empty()
                || self.prefixes.iter().any(|p| path == p || (path.starts_with(p.as_str()) && path.as_bytes().get(p.len()) == Some(&b'/'))))
    }
    /// The recorded spelling, e.g. "kind=function|method;path=pkg/api".
    pub fn describe(&self) -> Option<String> {
        if self.is_empty() {
            return None;
        }
        let mut parts = Vec::new();
        if !self.kinds.is_empty() {
            parts.push(format!("kind={}", self.kinds.join("|")));
        }
        if !self.prefixes.is_empty() {
            parts.push(format!("path={}", self.prefixes.join("|")));
        }
        Some(parts.join(";"))
    }
}

/// Contract query line.
#[derive(Serialize, Deserialize)]
pub struct QueryLine {
    pub arm: String,
    pub phase: String,
    pub query: String,
    pub run: u32,
    pub wall_ms: f64,
    pub hits: u64,
    pub page: Vec<String>,
    /// Present only on filtered runs: Filter::describe.
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub filter: Option<String>,
    /// Present only when the multi-threaded search executor was on.
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub search_threads: Option<usize>,
    /// Present only on --page runs: the 1-based page served.
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub page_no: Option<usize>,
    /// Present only when a non-default query option is on: the options, verbatim.
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub variant: Option<String>,
    /// Present only on hiding runs: the percentage hidden (of units for --hide-units,
    /// of live documents for --hide FILE).
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub hidden_pct: Option<f64>,
    /// Present only on hiding runs: whether the scoring statistics still count
    /// hidden documents (true for the library's own statistics).
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub stats_include_hidden: Option<bool>,
    /// Present only on hiding runs: the visibility set's build, timed apart from the query.
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub hide_build_ms: Option<f64>,
}

/// Contract build line.
#[derive(Serialize)]
pub struct BuildLine {
    pub arm: String,
    pub phase: String,
    pub wall_ms: f64,
    pub cpu_ms: f64,
    pub peak_rss_bytes: u64,
    pub disk_bytes: u64,
    pub docs: u64,
}

/// Process CPU (user+sys) in ms and peak RSS in bytes, from getrusage.
pub fn rusage() -> (f64, u64) {
    let mut u: libc::rusage = unsafe { std::mem::zeroed() };
    unsafe { libc::getrusage(libc::RUSAGE_SELF, &mut u) };
    let ms = |t: libc::timeval| t.tv_sec as f64 * 1e3 + t.tv_usec as f64 / 1e3;
    (ms(u.ru_utime) + ms(u.ru_stime), u.ru_maxrss as u64 * 1024)
}

/// Total bytes of the regular files under `dir`.
pub fn disk_bytes(dir: &Path) -> u64 {
    let mut n = 0;
    if let Ok(rd) = std::fs::read_dir(dir) {
        for e in rd.flatten() {
            if let Ok(m) = e.metadata() {
                if m.is_dir() {
                    n += disk_bytes(&e.path());
                } else {
                    n += m.len();
                }
            }
        }
    }
    n
}

/// The results directory: $TB_RESULTS, else `results` under the current directory
/// (run from the bench root to land in its results/).
pub fn results_dir() -> PathBuf {
    std::env::var("TB_RESULTS").map(PathBuf::from).unwrap_or_else(|_| PathBuf::from("results"))
}

/// Appends one JSON line to results/<arm>-<phase>.jsonl.
pub fn append_result<T: Serialize>(arm: &str, phase: &str, line: &T) -> anyhow_lite::Result<()> {
    let dir = results_dir();
    std::fs::create_dir_all(&dir)?;
    let path = dir.join(format!("{arm}-{phase}.jsonl"));
    let mut f = std::fs::OpenOptions::new().create(true).append(true).open(&path)?;
    writeln!(f, "{}", serde_json::to_string(line)?)?;
    Ok(())
}
