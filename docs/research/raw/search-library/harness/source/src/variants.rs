//! Index-time variants: schema options, docstore settings, analyzers, how a
//! document is built, and the writer's merge policy. An index records the schema
//! options it was built with in `tb-config.json` beside tantivy's files (tantivy's
//! garbage collection only deletes files it manages), so `tdelta` builds
//! documents the same way and `tquery` knows the query-side analyzer.

use crate::{anyhow_lite::Result, unicode61, Doc, Fields, COUNT_NAMES, CODE_TOKENIZER, TEXT_NAMES, TOKENIZER};
use serde::{Deserialize, Serialize};
use std::path::Path;
use tantivy::indexer::{LogMergePolicy, MergePolicy, NoMergePolicy};
use tantivy::schema::{
    FacetOptions, IndexRecordOption, NumericOptions, Schema, TextFieldIndexing, TextOptions, FAST, STORED, STRING,
};
use tantivy::store::{Compressor, ZstdCompressor};
use tantivy::tokenizer::{
    LowerCaser, NgramTokenizer, PreTokenizedString, SimpleTokenizer, TextAnalyzer, Token, Tokenizer,
};
use tantivy::{Index, IndexSettings};

pub const CONFIG_FILE: &str = "tb-config.json";
pub const SIMPLE_TOKENIZER: &str = "simple";
pub const PARTS_TOKENIZER: &str = "parts";
pub const NGRAM_TOKENIZER: &str = "ngram3";

/// How an index's schema and settings were chosen. Every default reproduces the
/// arm's baseline schema.
#[derive(Clone, Debug, Serialize, Deserialize)]
#[serde(default)]
pub struct IndexConfig {
    /// unicode61 | code | default (tantivy's) | simple (SimpleTokenizer+LowerCaser)
    /// | en_stem (tantivy's) | pretokenized (unicode61 tokens fed as PreTokenizedString).
    pub tokenizer: String,
    /// Keep the STORED flags (id, kind, path_s, unit, token_count).
    pub store: bool,
    /// all | text-only (off on every raw STRING field) | none (off everywhere).
    pub fieldnorms: String,
    /// positions | freqs: the body column's record option.
    pub body_record: String,
    /// token_count, start, end as FAST only (no inverted index, not stored).
    pub lean_numeric: bool,
    /// name_s, qn_s, sig_s, node_id as FAST only.
    pub lean_strings: bool,
    /// kind as FAST only: its TermQuery filter takes the fast-field fallback.
    pub kind_fast_only: bool,
    pub path_facet: bool,
    pub kind_code: bool,
    pub parts_fields: bool,
    pub name_ngram: bool,
    pub field_counts: bool,
    /// One extra text field `all` holding the five columns as five values (the
    /// stock library's nearest thing to BM25F: one field, one length, one df).
    pub combined: bool,
    /// none | lz4 | zstd:<level>
    pub docstore: String,
    pub blocksize: usize,
}

impl Default for IndexConfig {
    fn default() -> Self {
        IndexConfig {
            tokenizer: TOKENIZER.into(),
            store: true,
            fieldnorms: "all".into(),
            body_record: "positions".into(),
            lean_numeric: false,
            lean_strings: false,
            kind_fast_only: false,
            path_facet: false,
            kind_code: false,
            parts_fields: false,
            name_ngram: false,
            field_counts: false,
            combined: false,
            docstore: "lz4".into(),
            blocksize: 16384,
        }
    }
}

impl IndexConfig {
    /// The config recorded beside an index, or the baseline for an index built
    /// before configs were recorded.
    pub fn load(dir: &Path) -> IndexConfig {
        std::fs::read(dir.join(CONFIG_FILE)).ok().and_then(|b| serde_json::from_slice(&b).ok()).unwrap_or_default()
    }

    /// Consumes the schema options from `args`, leaving the rest.
    pub fn parse(args: &mut Vec<String>) -> Result<IndexConfig> {
        let mut c = IndexConfig::default();
        let mut i = 0;
        while i < args.len() {
            let flag = args[i].clone();
            let mut take = || -> Result<String> {
                let v = args.get(i + 1).cloned().ok_or_else(|| format!("{flag} needs a value"))?;
                args.drain(i..i + 2);
                Ok(v)
            };
            match flag.as_str() {
                "--tokenizer" => c.tokenizer = take()?,
                "--fieldnorms" => c.fieldnorms = take()?,
                "--body-record" => c.body_record = take()?,
                "--docstore" => c.docstore = take()?,
                "--blocksize" => c.blocksize = take()?.parse()?,
                "--store" => {
                    let v = take()?;
                    c.store = match v.as_str() {
                        "stored" => true,
                        "none" => false,
                        _ => return Err("--store is stored or none".into()),
                    }
                }
                "--schema" => match take()?.as_str() {
                    "full" => {}
                    // features/02-settings-fields.md §8: nothing stored, keys raw + fast,
                    // identity strings and numbers fast only, text fieldnorms kept.
                    "lean" => {
                        c.store = false;
                        c.fieldnorms = "text-only".into();
                        c.lean_numeric = true;
                        c.lean_strings = true;
                    }
                    _ => return Err("--schema is full or lean".into()),
                },
                f @ ("--lean-numeric" | "--lean-strings" | "--kind-fast-only" | "--path-facet" | "--kind-code"
                | "--parts-fields" | "--name-ngram" | "--field-counts" | "--combined") => {
                    match f {
                        "--lean-numeric" => c.lean_numeric = true,
                        "--lean-strings" => c.lean_strings = true,
                        "--kind-fast-only" => c.kind_fast_only = true,
                        "--path-facet" => c.path_facet = true,
                        "--kind-code" => c.kind_code = true,
                        "--parts-fields" => c.parts_fields = true,
                        "--name-ngram" => c.name_ngram = true,
                        "--combined" => c.combined = true,
                        _ => c.field_counts = true,
                    }
                    args.remove(i);
                }
                _ => i += 1,
            }
        }
        let toks = [TOKENIZER, CODE_TOKENIZER, "default", SIMPLE_TOKENIZER, "en_stem", "pretokenized"];
        if !toks.contains(&c.tokenizer.as_str()) {
            return Err(format!("--tokenizer must be one of {toks:?}").into());
        }
        if !["all", "text-only", "none"].contains(&c.fieldnorms.as_str()) {
            return Err("--fieldnorms is all, text-only or none".into());
        }
        if !["positions", "freqs"].contains(&c.body_record.as_str()) {
            return Err("--body-record is positions or freqs".into());
        }
        compressor(&c.docstore)?;
        Ok(c)
    }

    /// A short label of every non-default option, for phase names.
    pub fn label(&self) -> String {
        let d = IndexConfig::default();
        let mut p = Vec::new();
        if self.tokenizer != d.tokenizer {
            p.push(self.tokenizer.replace('_', ""));
        }
        if !self.store {
            p.push("nostore".into());
        }
        if self.fieldnorms != d.fieldnorms {
            p.push(format!("fn{}", self.fieldnorms.replace('-', "")));
        }
        if self.body_record != d.body_record {
            p.push("bodyfreqs".into());
        }
        for (on, name) in [
            (self.lean_numeric, "leannum"),
            (self.lean_strings, "leanstr"),
            (self.kind_fast_only, "kindfast"),
            (self.path_facet, "facet"),
            (self.kind_code, "kindcode"),
            (self.parts_fields, "parts"),
            (self.name_ngram, "ngram"),
            (self.field_counts, "counts"),
            (self.combined, "combined"),
        ] {
            if on {
                p.push(name.into());
            }
        }
        if self.docstore != d.docstore {
            p.push(self.docstore.replace(':', ""));
        }
        if self.blocksize != d.blocksize {
            p.push(format!("bs{}", self.blocksize));
        }
        p.join("-")
    }

    /// The analyzer name the five text fields carry in the schema.
    fn text_tokenizer(&self) -> &str {
        if self.tokenizer == "pretokenized" { TOKENIZER } else { &self.tokenizer }
    }
}

fn compressor(s: &str) -> Result<Compressor> {
    Ok(match s {
        "none" => Compressor::None,
        "lz4" => Compressor::Lz4,
        z => {
            let level = z.strip_prefix("zstd:").ok_or("--docstore is none, lz4 or zstd:<level>")?.parse()?;
            Compressor::Zstd(ZstdCompressor { compression_level: Some(level) })
        }
    })
}

pub fn build_schema(c: &IndexConfig) -> Schema {
    let mut b = Schema::builder();
    let text_fn = c.fieldnorms != "none";
    let raw_fn = c.fieldnorms == "all";
    let text = |tok: &str, rec: IndexRecordOption| {
        TextOptions::default().set_indexing_options(
            TextFieldIndexing::default().set_tokenizer(tok).set_index_option(rec).set_fieldnorms(text_fn),
        )
    };
    for n in TEXT_NAMES {
        let rec = if n == "body" && c.body_record == "freqs" {
            IndexRecordOption::WithFreqs
        } else {
            IndexRecordOption::WithFreqsAndPositions
        };
        b.add_text_field(n, text(c.text_tokenizer(), rec));
    }
    // A raw key: indexed as one token (Basic), fast, optionally stored.
    let raw = |stored: bool| {
        let mut o = TextOptions::default()
            .set_indexing_options(
                TextFieldIndexing::default()
                    .set_tokenizer("raw")
                    .set_index_option(IndexRecordOption::Basic)
                    .set_fieldnorms(raw_fn),
            )
            .set_fast(None);
        if stored {
            o = o.set_stored();
        }
        o
    };
    let fast_only = || TextOptions::default().set_fast(None);
    b.add_text_field("id", raw(c.store));
    b.add_text_field("node_id", if c.lean_strings { fast_only() } else { raw(false) });
    b.add_text_field("kind", if c.kind_fast_only { fast_only() } else { raw(c.store) });
    b.add_text_field("path_s", raw(c.store));
    for n in ["name_s", "qn_s", "sig_s"] {
        b.add_text_field(n, if c.lean_strings { fast_only() } else { raw(false) });
    }
    b.add_text_field("unit", raw(c.store));
    let num = |indexed: bool, stored: bool| {
        let mut o = NumericOptions::default().set_fast();
        if indexed {
            o = o.set_indexed().set_fieldnorm();
        }
        if stored {
            o = o.set_stored();
        }
        o
    };
    let _ = (STRING, STORED, FAST);
    b.add_u64_field("token_count", num(!c.lean_numeric, c.store && !c.lean_numeric));
    b.add_u64_field("start", num(false, false));
    b.add_u64_field("end", num(false, false));
    if c.path_facet {
        b.add_facet_field("path_facet", FacetOptions::default());
    }
    if c.kind_code {
        b.add_u64_field("kind_code", num(false, false));
    }
    if c.parts_fields {
        b.add_text_field("name_parts", text(PARTS_TOKENIZER, IndexRecordOption::WithFreqsAndPositions));
        b.add_text_field("qn_parts", text(PARTS_TOKENIZER, IndexRecordOption::WithFreqsAndPositions));
    }
    if c.name_ngram {
        b.add_text_field("name_ngram", text(NGRAM_TOKENIZER, IndexRecordOption::WithFreqs));
    }
    if c.field_counts {
        for n in COUNT_NAMES {
            b.add_u64_field(n, num(false, false));
        }
    }
    if c.combined {
        b.add_text_field("all", text(c.text_tokenizer(), IndexRecordOption::WithFreqsAndPositions));
    }
    if c.tokenizer == CODE_TOKENIZER {
        // product-code's document length: what the code tokenizer emits over the
        // five columns, stacked parts included.
        b.add_u64_field(crate::product::DL_CODE, num(false, false));
    }
    b.build()
}

/// Creates the index directory's tantivy index with `c`'s schema and docstore
/// settings, and records `c` beside it.
pub fn create(dir: &Path, c: &IndexConfig) -> Result<Index> {
    let settings = IndexSettings {
        docstore_compression: compressor(&c.docstore)?,
        docstore_blocksize: c.blocksize,
        ..IndexSettings::default()
    };
    let index = Index::builder().schema(build_schema(c)).settings(settings).create_in_dir(dir)?;
    std::fs::write(dir.join(CONFIG_FILE), serde_json::to_vec_pretty(c)?)?;
    Ok(index)
}

/// The settings and schema for an index created through another route
/// (single-segment writer, RAM directory).
pub fn builder(c: &IndexConfig) -> Result<tantivy::IndexBuilder> {
    let settings = IndexSettings {
        docstore_compression: compressor(&c.docstore)?,
        docstore_blocksize: c.blocksize,
        ..IndexSettings::default()
    };
    Ok(Index::builder().schema(build_schema(c)).settings(settings))
}

pub fn register_analyzers(index: &Index) {
    let m = index.tokenizers();
    m.register(TOKENIZER, TextAnalyzer::from(unicode61::Unicode61));
    m.register(CODE_TOKENIZER, TextAnalyzer::from(unicode61::Code));
    m.register(SIMPLE_TOKENIZER, TextAnalyzer::builder(SimpleTokenizer::default()).filter(LowerCaser).build());
    m.register(PARTS_TOKENIZER, TextAnalyzer::from(PartsOnly));
    if let Ok(ng) = NgramTokenizer::all_ngrams(3, 3) {
        m.register(NGRAM_TOKENIZER, TextAnalyzer::builder(ng).filter(LowerCaser).build());
    }
}

/// A stable code for a kind string (FNV-1a), for the u64 kind filter.
pub fn kind_code(kind: &str) -> u64 {
    let mut h: u64 = 0xcbf29ce484222325;
    for b in kind.bytes() {
        h ^= b as u64;
        h = h.wrapping_mul(0x100000001b3);
    }
    h
}

/// The path as a facet: "/" + its components.
pub fn path_facet(path: &str) -> tantivy::schema::Facet {
    tantivy::schema::Facet::from_path(path.split('/').filter(|c| !c.is_empty()))
}

pub fn to_doc(d: &Doc, f: &Fields) -> tantivy::TantivyDocument {
    let mut t = tantivy::TantivyDocument::default();
    for (i, c) in d.columns().iter().enumerate() {
        if f.pretokenized {
            let mut tokens = Vec::new();
            unicode61::for_each_token(c, |p, from, to, s| {
                tokens.push(Token { offset_from: from, offset_to: to, position: p, text: s.to_string(), position_length: 1 })
            });
            t.add_pre_tokenized_text(f.text[i], PreTokenizedString { text: c.to_string(), tokens });
        } else {
            t.add_text(f.text[i], c);
        }
    }
    t.add_text(f.id, &d.id);
    t.add_text(f.node_id, &d.node_id);
    t.add_text(f.kind, &d.kind);
    t.add_text(f.path_s, &d.path);
    t.add_text(f.name_s, &d.name);
    t.add_text(f.qn_s, &d.qualified_name);
    t.add_text(f.sig_s, &d.signature);
    t.add_text(f.unit, &d.unit);
    t.add_u64(f.token_count, d.token_count.max(0) as u64);
    t.add_u64(f.start, d.bytes.start);
    t.add_u64(f.end, d.bytes.end);
    if let Some(p) = f.path_facet {
        t.add_facet(p, path_facet(&d.path));
    }
    if let Some(k) = f.kind_code {
        t.add_u64(k, kind_code(&d.kind));
    }
    if let (Some(n), Some(q)) = (f.name_parts, f.qn_parts) {
        t.add_text(n, &d.name);
        t.add_text(q, &d.qualified_name);
    }
    if let Some(g) = f.name_ngram {
        t.add_text(g, &d.name);
    }
    if let Some(cs) = f.counts {
        for (i, c) in d.columns().iter().enumerate() {
            t.add_u64(cs[i], unicode61::count(c));
        }
    }
    if let Some(a) = f.all {
        for c in d.columns() {
            t.add_text(a, c);
        }
    }
    if let Some(tc) = f.token_count_code {
        let mut n = 0u64;
        for c in d.columns() {
            unicode61::for_each_code_token(c, |_, _, _, _, _| n += 1);
        }
        t.add_u64(tc, n);
    }
    t
}

/// The parts-only analyzer of the side fields: for every unicode61 token whose
/// spelling has two or more camelCase parts, each folded part, at consecutive
/// positions (so the parts of one identifier form a phrase). Single-part tokens
/// yield nothing: the main fields already hold them.
#[derive(Clone, Default)]
pub struct PartsOnly;

impl Tokenizer for PartsOnly {
    type TokenStream<'a> = unicode61::Unicode61Stream;
    fn token_stream<'a>(&'a mut self, text: &'a str) -> unicode61::Unicode61Stream {
        let mut tokens = Vec::new();
        let mut pos = 0;
        unicode61::for_each_token(text, |_, from, to, _| {
            let parts = unicode61::split_parts(&text[from..to]);
            if parts.len() >= 2 {
                for part in parts {
                    let folded = unicode61::fold_str(part);
                    if !folded.is_empty() {
                        tokens.push(Token { offset_from: from, offset_to: to, position: pos, text: folded, position_length: 1 });
                        pos += 1;
                    }
                }
            }
        });
        unicode61::Unicode61Stream::new(tokens)
    }
}

/// Parts of one query token for the side fields (folded), or none.
pub fn query_parts(token_spelling: &str) -> Vec<String> {
    let parts = unicode61::split_parts(token_spelling);
    if parts.len() < 2 {
        return Vec::new();
    }
    parts.iter().map(|p| unicode61::fold_str(p)).filter(|p| !p.is_empty()).collect()
}

/// The merge policy named by `--merge-policy`: `log` (tantivy's default),
/// `log:min=N,del=R,maxdocs=M,layer=L,levellog=X` (LogMergePolicy knobs), or `none`
/// (NoMergePolicy). `none+final` is `none` plus one merge of every segment at the
/// end of the build; that part is the caller's.
pub fn merge_policy(spec: &str) -> Result<Box<dyn MergePolicy>> {
    if spec == "none" || spec == "none+final" {
        return Ok(Box::new(NoMergePolicy));
    }
    let mut p = LogMergePolicy::default();
    if let Some(knobs) = spec.strip_prefix("log:") {
        for kv in knobs.split(',') {
            let (k, v) = kv.split_once('=').ok_or("a log knob is key=value")?;
            match k {
                "min" => p.set_min_num_segments(v.parse()?),
                "del" => p.set_del_docs_ratio_before_merge(v.parse()?),
                "maxdocs" => p.set_max_docs_before_merge(v.parse()?),
                "layer" => p.set_min_layer_size(v.parse()?),
                "levellog" => p.set_level_log_size(v.parse()?),
                _ => return Err(format!("unknown log merge knob {k}").into()),
            }
        }
    } else if spec != "log" {
        return Err("--merge-policy is log, log:<knobs>, none or none+final".into());
    }
    Ok(Box::new(p))
}
