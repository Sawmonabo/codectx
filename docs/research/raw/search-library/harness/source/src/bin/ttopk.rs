//! ttopk <index dir> <queries.txt> [--search-threads N] [--shape S]
//!
//! --shape product-topk instead compares product-topk with product mode on every
//! query, pages 1 to 3 (page N from the top N*200), timing each (warm, best of 3).
//! The index needs `tblock` first.
//! --shape (library.rs Shape: nested, flat, dismax:T, ...) checks that shape's
//! TopDocs page against its own exhaustive collection (the DisjunctionMax and
//! block-WAND checks, Q14/Q15).
//!
//! Checks the reported risk that the library's pruned top-k misses a true top hit
//! on a multi-segment index. Per query it compares `library` (TopDocs top 200,
//! block-max pruning allowed) with the exhaustive collection (`library-all`'s
//! collector: every (doc, score)). The exhaustive list is ordered two ways:
//! by (score desc, DocAddress asc), which is TopDocs' documented tie rule, so
//! the pruned answer must equal it exactly; and by (score desc, id asc), which
//! is what library-all serves. Prints, per query, whether the set and the order
//! match each, and every mismatch: a true miss (a document scoring above the
//! pruned page's lowest score that the page lacks, or a page document scoring
//! below the 200th exhaustive score), a score that differs between the two
//! paths, and tie-only reorderings. Prints which index it ran on: segments,
//! documents, deleted documents.
use std::collections::{HashMap, HashSet};
use tantivy::collector::TopDocs;
use tantivy::{DocAddress, ReloadPolicy, Score};
use tbench::library::{build_query, id_of, Analyzer, AllScores, QueryOpts, Shape};
use tbench::product::PAGE;
use tbench::{anyhow_lite::Result, index_tokenizer, open_index, Filter};

fn main() -> Result<()> {
    let mut a: Vec<String> = std::env::args().collect();
    let mut threads = None;
    if let Some(i) = a.iter().position(|x| x == "--search-threads") {
        threads = Some(a.get(i + 1).ok_or("--search-threads needs N")?.parse::<usize>()?);
        a.drain(i..i + 2);
    }
    let mut qo = QueryOpts::default();
    if let Some(i) = a.iter().position(|x| x == "--shape") {
        let v = a.get(i + 1).ok_or("--shape needs a value")?.clone();
        a.drain(i..i + 2);
        if v == "product-topk" {
            return product_topk(&a, threads);
        }
        qo.shape = Shape::parse(&v)?;
    }
    if a.len() != 3 {
        return Err("usage: ttopk <dir> <queries.txt> [--search-threads N] [--shape S]".into());
    }
    let (mut index, f) = open_index(std::path::Path::new(&a[1]), None)?;
    if let Some(n) = threads {
        index.set_multithread_executor(n)?;
    }
    let an = Analyzer::for_index(&index, &f);
    let reader = index.reader_builder().reload_policy(ReloadPolicy::Manual).try_into()?;
    let s: tantivy::Searcher = reader.searcher();
    let metas = index.searchable_segment_metas()?;
    let docs: u64 = metas.iter().map(|m| m.num_docs() as u64).sum();
    let deleted: u64 = metas.iter().map(|m| m.num_deleted_docs() as u64).sum();
    println!(
        "ttopk on {}: {} segments, {docs} live documents, {deleted} deleted, tokenizer {}, search threads {}",
        a[1], metas.len(), index_tokenizer(&index, &f), threads.unwrap_or(1)
    );
    let (mut exact, mut by_id_same, mut misses_total, mut n) = (0, 0, 0, 0);
    let (mut score_diffs, mut max_ulps, mut tie_order, mut tie_set) = (0u64, 0u64, 0, 0);
    for q in std::fs::read_to_string(&a[2])?.lines().filter(|l| !l.is_empty()) {
        n += 1;
        let query = build_query(q, &index, &f, &an, &qo, &Filter::default())?;
        let pruned: Vec<(Score, DocAddress)> = s.search(&query, &TopDocs::with_limit(PAGE).order_by_score())?;
        let all = s.search(&query, &AllScores)?;
        let mut full: Vec<(Score, DocAddress, String)> =
            all.iter().map(|(sc, col, d, ord)| (*sc, DocAddress::new(*ord, *d), id_of(col, *d))).collect();
        let score_of: HashMap<DocAddress, Score> = full.iter().map(|x| (x.1, x.0)).collect();
        full.sort_by(|x, y| y.0.total_cmp(&x.0).then(x.1.cmp(&y.1)));
        let want: Vec<DocAddress> = full.iter().take(PAGE).map(|x| x.1).collect();
        let got: Vec<DocAddress> = pruned.iter().map(|x| x.1).collect();
        let id_of_addr: HashMap<DocAddress, &String> = full.iter().map(|x| (x.1, &x.2)).collect();
        let mut by_id = full.clone();
        by_id.sort_by(|x, y| y.0.total_cmp(&x.0).then_with(|| x.2.cmp(&y.2)));
        let want_id: Vec<&String> = by_id.iter().take(PAGE).map(|x| &x.2).collect();
        let got_id: Vec<&String> = got.iter().map(|x| id_of_addr[x]).collect();

        let set_same = want.iter().collect::<HashSet<_>>() == got.iter().collect::<HashSet<_>>();
        let order_same = want == got;
        let id_set_same = want_id.iter().collect::<HashSet<_>>() == got_id.iter().collect::<HashSet<_>>();
        let id_order_same = want_id == got_id;
        exact += order_same as usize;
        by_id_same += id_order_same as usize;
        println!(
            "{q:?}: {} matches; vs (score, address): set {} order {}; vs library-all (score, id): set {} order {}",
            full.len(), yes(set_same), yes(order_same), yes(id_set_same), yes(id_order_same)
        );
        // The same document's score on the two scorer paths (pruned vs exhaustive).
        for (sc, addr) in &pruned {
            let ex = score_of[addr];
            if ex != *sc {
                score_diffs += 1;
                let ulps = (ex.to_bits() as i64 - sc.to_bits() as i64).unsigned_abs();
                max_ulps = max_ulps.max(ulps);
                println!("  SCORE PATH {}: pruned {sc} exhaustive {ex} ({ulps} ulp)", id_of_addr[addr]);
            }
        }
        // True misses, judged on the exhaustive scores alone: a document scoring above
        // the lowest-scoring page document yet absent, or a page document below the
        // true 200th score.
        if let (Some(floor), Some(true200)) = (
            got.iter().map(|a| score_of[a]).min_by(|x, y| x.total_cmp(y)),
            full.get(PAGE.min(full.len()).saturating_sub(1)).map(|x| x.0),
        ) {
            let gotset: HashSet<&DocAddress> = got.iter().collect();
            for x in full.iter().filter(|x| x.0 > floor && !gotset.contains(&x.1)) {
                misses_total += 1;
                println!("  MISS {} score {} above the page's lowest score {floor}", x.2, x.0);
            }
            for addr in got.iter().filter(|a| score_of[*a] < true200) {
                misses_total += 1;
                println!("  WRONG {} score {} below the true 200th score {true200}", id_of_addr[addr], score_of[addr]);
            }
        }
        if !order_same {
            for (i, (w, g)) in want.iter().zip(&got).enumerate().filter(|(_, (w, g))| w != g) {
                println!("  order rank {}: exhaustive {} ({}) pruned {} ({})", i + 1, id_of_addr[w], score_of[w], id_of_addr[g], score_of[g]);
            }
        }
        if !id_order_same {
            // Every difference from library-all's page must be a tie: equal score.
            let boundary = by_id.get(PAGE.min(by_id.len()).saturating_sub(1)).map(|x| x.0);
            let a: HashSet<&String> = want_id.iter().copied().collect();
            let b: HashSet<&String> = got_id.iter().copied().collect();
            let score_by_id: HashMap<&String, Score> = full.iter().map(|x| (&x.2, x.0)).collect();
            let only_ties = a.symmetric_difference(&b).all(|k| Some(score_by_id[*k]) == boundary);
            if id_set_same {
                tie_order += 1;
                println!("  vs library-all: same set, order differs only among equal scores (address vs id tie rule)");
            } else if only_ties {
                tie_set += 1;
                println!("  vs library-all: the sets differ only among documents tied at the 200th score {:?}", boundary);
            } else {
                println!("  vs library-all: DIFFERENT beyond ties");
            }
        }
    }
    println!("ttopk: {n} queries; pruned == exhaustive (score, address) on {exact}; == library-all page on {by_id_same}, tie order only on {tie_order}, boundary ties only on {tie_set}; true misses {misses_total}; page scores differing between the scorer paths {score_diffs} (max {max_ulps} ulp)");
    Ok(())
}

fn yes(b: bool) -> &'static str {
    if b { "same" } else { "DIFFERS" }
}

/// ttopk --shape product-topk: product-topk against product, pages 1..=3, every query.
fn product_topk(a: &[String], threads: Option<usize>) -> Result<()> {
    use std::time::Instant;
    use tbench::product::{corpus_stats_of, ProductCollector, DL, DL_CODE};
    if a.len() != 3 {
        return Err("usage: ttopk <dir> <queries.txt> --shape product-topk [--search-threads N]".into());
    }
    let dir = std::path::Path::new(&a[1]);
    let (mut index, f) = open_index(dir, None)?;
    if let Some(n) = threads {
        index.set_multithread_executor(n)?;
    }
    let code = f.token_count_code.is_some();
    let dl = if code { DL_CODE } else { DL };
    let s = index.reader_builder().reload_policy(ReloadPolicy::Manual).try_into()?.searcher();
    let st = corpus_stats_of(s.segment_readers(), None, dl)?;
    let blocks = tbench::topk::load(dir, s.segment_readers(), dl)?;
    fn best(mut g: impl FnMut() -> Result<Vec<String>>) -> Result<(f64, Vec<String>)> {
        let mut ms = f64::MAX;
        let mut page = Vec::new();
        for _ in 0..3 {
            let t = Instant::now();
            page = g()?;
            ms = ms.min(t.elapsed().as_secs_f64() * 1e3);
        }
        Ok((ms, page))
    }
    let (mut n, mut same, mut diff, mut tp, mut tt) = (0, 0, 0, Vec::new(), Vec::new());
    let (mut scored, mut total) = (0u64, 0u64);
    for q in std::fs::read_to_string(&a[2])?.lines().filter(|l| !l.is_empty()) {
        let terms = tbench::parse_query_with(q, code);
        for page in 1..=3usize {
            n += 1;
            let pc = ProductCollector {
                terms: terms.clone(), fields: f, stats: st, filter: Filter::default(), hide: None, via_weight: false, dl, page_no: page,
            };
            let (ms_p, want) = best(|| Ok(s.search(&tantivy::query::AllQuery, &pc)?.page))?;
            let mut last = None;
            let (ms_t, got) = best(|| {
                let ans = tbench::topk::search(&s, terms.clone(), f, st, Filter::default(), None, blocks.clone(), dl, page)?;
                last = Some((ans.blocks_scored, ans.blocks_total));
                Ok(ans.page)
            })?;
            if let Some((x, y)) = last {
                scored += x;
                total += y;
            }
            if got == want {
                same += 1;
            } else {
                diff += 1;
                println!("DIFFERENT {q:?} page {page}: product {} ids, topk {} ids", want.len(), got.len());
            }
            if page == 1 {
                tp.push(ms_p);
                tt.push(ms_t);
            }
        }
    }
    let med = |v: &mut Vec<f64>| {
        v.sort_by(f64::total_cmp);
        v.get(v.len() / 2).copied().unwrap_or(f64::NAN)
    };
    println!(
        "ttopk product-topk on {} ({} segments, threads {}): {n} (query, page) pairs, identical {same}, different {diff}; blocks scored {scored} of {total}; page-1 median ms: product {:.3}, product-topk {:.3}",
        a[1], s.segment_readers().len(), threads.unwrap_or(1), med(&mut tp), med(&mut tt)
    );
    if diff > 0 {
        return Err(format!("{diff} pages differ").into());
    }
    Ok(())
}
