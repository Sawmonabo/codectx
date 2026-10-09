//! tcheck <docs.jsonl> <index dir> <queries.txt> [--kind K]... [--path-prefix P]... [--code]
//!
//! --topk also checks product-topk's page (topk.rs; `tblock` first) against the
//! same reference, and --search-threads N runs both through the executor.
//! --code checks product-code instead: the reference tokenizes with the code
//! tokenizer (camelCase parts stacked at their token's position), its document
//! length is that token count, and query terms gain their parts, against product
//! mode over a `--tokenizer code` index reading token_count_code.
//!
//! An index-free reference of our lexical rules computed straight from
//! docs.jsonl (tokenize every column, score every document, fold, sort), compared
//! query by query with product mode over the tantivy index. It guards the
//! tantivy plumbing: postings, positions, segments, alive bits, rank columns and
//! cross-segment folding. Run it on an index built from the same docs.jsonl,
//! before any delta.
use std::collections::HashMap;
use tantivy::query::AllQuery;
use tantivy::ReloadPolicy;
use tbench::product::{cmp_scored, corpus_stats_of, Cand, ProductCollector, MAX_TERM_OFFSETS, PAGE};
use tbench::{Filter, anyhow_lite::Result, bm25_idf, bm25_saturation, open_index, quantize, read_docs, unicode61, Doc, WEIGHTS};

struct Tok {
    doc: Doc,
    // per column: term -> positions
    cols: Vec<HashMap<String, Vec<u32>>>,
}

fn main() -> Result<()> {
    let a: Vec<String> = std::env::args().collect();
    let mut filter = Filter::default();
    let mut a = a;
    let code = a.iter().any(|x| x == "--code");
    let topk = a.iter().any(|x| x == "--topk");
    a.retain(|x| x != "--code" && x != "--topk");
    let mut threads = None;
    if let Some(i) = a.iter().position(|x| x == "--search-threads") {
        threads = Some(a.get(i + 1).ok_or("--search-threads needs N")?.parse::<usize>()?);
        a.drain(i..i + 2);
    }
    let mut i = 4;
    while i + 1 < a.len() {
        match a[i].as_str() {
            "--kind" => filter.kinds.push(a[i + 1].clone()),
            "--path-prefix" => filter.prefixes.push(Filter::clean_prefix(&a[i + 1])),
            o => return Err(format!("unknown option {o}").into()),
        }
        i += 2;
    }
    let mut all = Vec::new();
    read_docs(std::path::Path::new(&a[1]), |d| {
        let cols = d
            .columns()
            .iter()
            .map(|c| {
                let mut m: HashMap<String, Vec<u32>> = HashMap::new();
                if code {
                    unicode61::for_each_code_token(c, |p, _, _, t, _| m.entry(t.to_string()).or_default().push(p as u32));
                } else {
                    unicode61::for_each_token(c, |p, _, _, t| m.entry(t.to_string()).or_default().push(p as u32));
                }
                m
            })
            .collect::<Vec<HashMap<String, Vec<u32>>>>();
        let mut d = d;
        if code {
            d.token_count = cols.iter().map(|m| m.values().map(|p| p.len() as i64).sum::<i64>()).sum();
        }
        all.push(Tok { doc: d, cols });
        Ok(())
    })?;
    let n = all.len() as i64;
    let tokens: i64 = all.iter().map(|t| t.doc.token_count).sum();
    let avgdl = tokens as f64 / n as f64;

    let (mut index, fields) = open_index(std::path::Path::new(&a[2]), None)?;
    if let Some(n) = threads {
        index.set_multithread_executor(n)?;
    }
    let want_tok = if code { tbench::CODE_TOKENIZER } else { tbench::TOKENIZER };
    if tbench::index_tokenizer(&index, &fields) != want_tok {
        return Err(format!("this check needs a {want_tok} index").into());
    }
    let dl = if code { tbench::product::DL_CODE } else { tbench::product::DL };
    let reader = index.reader_builder().reload_policy(ReloadPolicy::Manual).try_into()?;
    let s: tantivy::Searcher = reader.searcher();
    let st = corpus_stats_of(s.segment_readers(), None, dl)?;
    if st.docs != n || st.tokens != tokens {
        return Err(format!("corpus stats differ: index {st:?}, reference {n} docs {tokens} tokens").into());
    }
    let blocks = if topk { Some(tbench::topk::load(std::path::Path::new(&a[2]), s.segment_readers(), dl)?) } else { None };
    let mut failed = 0;
    for q in std::fs::read_to_string(&a[3])?.lines().filter(|l| !l.is_empty()) {
        let terms = tbench::parse_query_with(q, code);
        let mut wtf = vec![vec![0f64; all.len()]; terms.len()];
        for (i, t) in terms.iter().enumerate() {
            for (di, d) in all.iter().enumerate() {
                for (c, m) in d.cols.iter().enumerate() {
                    let count = if !t.phrase() {
                        m.get(&t.tokens[0]).map_or(0, |p| p.len() as u64)
                    } else {
                        let Some(ps) = t.tokens.iter().map(|k| m.get(k)).collect::<Option<Vec<_>>>() else { continue };
                        let cap: Vec<&[u32]> = ps.iter().map(|p| &p[..p.len().min(MAX_TERM_OFFSETS)]).collect();
                        cap[0].iter().filter(|&&s| (1..cap.len()).all(|j| cap[j].binary_search(&(s + j as u32)).is_ok())).count() as u64
                    };
                    wtf[i][di] += WEIGHTS[c] * count as f64;
                }
            }
        }
        let idf: Vec<f64> = wtf.iter().map(|w| bm25_idf(n, w.iter().filter(|&&x| x > 0.0).count() as i64)).collect();
        let mut groups: HashMap<String, Cand> = HashMap::new();
        for (di, d) in all.iter().enumerate() {
            let mut score = 0f64;
            let mut any = false;
            for i in 0..terms.len() {
                if wtf[i][di] > 0.0 {
                    any = true;
                    score += idf[i] * bm25_saturation(wtf[i][di], d.doc.token_count, avgdl);
                }
            }
            if !any {
                continue;
            }
            let x = &d.doc;
            if !filter.keep(&x.kind, &x.path) {
                continue;
            }
            let c = Cand {
                micros: quantize(score), path: x.path.clone(), start: x.bytes.start, end: x.bytes.end as i64,
                kind: x.kind.clone(), name: x.name.clone(), qn: x.qualified_name.clone(), sig: x.signature.clone(),
                node: x.node_id.clone(), id: x.id.clone(),
            };
            let key = if !c.node.is_empty() { format!("node\0{}", c.node) } else { format!("file\0{}\0{}", c.path, c.start) };
            match groups.get_mut(&key) {
                Some(h) if cmp_scored(&c, h).is_lt() => *h = c,
                Some(_) => {}
                None => {
                    groups.insert(key, c);
                }
            }
        }
        let hits = groups.len() as u64;
        let mut v: Vec<Cand> = groups.into_values().collect();
        v.sort_by(cmp_scored);
        let want: Vec<String> = v.into_iter().take(PAGE).map(|c| c.id).collect();
        let got = s.search(&AllQuery, &ProductCollector { terms, fields, stats: st, filter: filter.clone(), hide: None, via_weight: false, dl, page_no: 1 })?;
        let mut ok = got.hits == hits && got.page == want;
        let mut tk = String::new();
        if topk {
            let blocks = blocks.clone().ok_or("blocks")?;
            let ans = tbench::topk::search(&s, tbench::parse_query_with(q, code), fields, st, filter.clone(), None, blocks, dl, 1)?;
            ok &= ans.page == want;
            tk = format!(", topk page {} ({}): blocks scored {} of {}", ans.page.len(), if ans.page == want { "same" } else { "DIFFERENT" }, ans.blocks_scored, ans.blocks_total);
        }
        if !ok {
            failed += 1;
        }
        println!("tcheck {q:?}: reference hits {hits}, product hits {}, page {} vs {}{tk}: {}", got.hits, want.len(), got.page.len(), if ok { "SAME" } else { "DIFFERENT" });
    }
    if failed > 0 {
        return Err(format!("{failed} queries differ").into());
    }
    Ok(())
}
