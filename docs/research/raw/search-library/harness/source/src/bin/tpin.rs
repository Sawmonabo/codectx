//! tpin <index dir (a copy: it is changed)> <docs.jsonl> <queries.txt> [unit]
//!
//! A held Searcher is a pinned generation: answers taken from it before a delta
//! and a forced merge must equal the answers it gives after, in the same process.
//! 1. Open a Manual reader and an OnCommitWithDelay reader; hold searcher A.
//! 2. Answer every query from A (product page and hits, library top-200 ids).
//! 3. Delta: delete `unit` (the first unit by default), re-add its documents with
//!    " deltachange1" appended to the first body, commit; time from the commit's
//!    return until the OnCommitWithDelay reader serves the new generation.
//! 4. Force-merge every segment into one and wait (old segment files are deleted
//!    from the directory; A still maps them).
//! 5. Answer again from A: must be identical. Time Manual reader.reload() and
//!    report the new searcher's segment count.
use std::time::{Duration, Instant};
use tantivy::collector::TopDocs;
use tantivy::query::AllQuery;
use tantivy::{ReloadPolicy, Searcher, Term};
use tbench::library::{id_of, library_query};
use tbench::product::{corpus_stats, ProductCollector, PAGE};
use tbench::{anyhow_lite::Result, open_index, parse_query, read_docs, to_tantivy, Fields, Filter};

type Ans = Vec<(u64, Vec<String>, Vec<String>)>;

fn answers(s: &Searcher, f: &Fields, qs: &[String]) -> Result<Ans> {
    let st = corpus_stats(s)?;
    let mut out = Vec::new();
    for q in qs {
        let c = ProductCollector { terms: parse_query(q), fields: *f, stats: st, filter: Filter::default(), hide: None, via_weight: false, dl: tbench::product::DL, page_no: 1 };
        let p = s.search(&AllQuery, &c)?;
        let lq = library_query(q, f, &Filter::default(), false)?;
        let top = s.search(&*lq, &TopDocs::with_limit(PAGE).order_by_score())?;
        let mut ids = Vec::new();
        for (_, a) in top {
            ids.push(id_of(&s.segment_reader(a.segment_ord).fast_fields().str("id")?, a.doc_id));
        }
        out.push((p.hits, p.page, ids));
    }
    Ok(out)
}

fn main() -> Result<()> {
    let a: Vec<String> = std::env::args().collect();
    if a.len() < 4 {
        return Err("usage: tpin <index dir copy> <docs.jsonl> <queries.txt> [unit]".into());
    }
    let dir = std::path::Path::new(&a[1]);
    let qs: Vec<String> = std::fs::read_to_string(&a[3])?.lines().filter(|l| !l.is_empty()).map(String::from).collect();
    let mut want = a.get(4).cloned();
    let mut docs = Vec::new();
    read_docs(std::path::Path::new(&a[2]), |d| {
        if want.is_none() {
            want = Some(d.unit.clone());
        }
        if want.as_deref() == Some(d.unit.as_str()) {
            docs.push(d);
        }
        Ok(())
    })?;
    let unit = want.ok_or("docs.jsonl is empty")?;
    docs[0].body.push_str(" deltachange1");
    docs[0].token_count = docs[0].unicode61_count() as i64;
    let (index, f) = open_index(dir, None)?;
    let manual = index.reader_builder().reload_policy(ReloadPolicy::Manual).try_into()?;
    let auto = index.reader_builder().reload_policy(ReloadPolicy::OnCommitWithDelay).try_into()?;
    let held = manual.searcher();
    let gen0 = held.generation().generation_id();
    let before = answers(&held, &f, &qs)?;

    let mut w = index.writer_with_num_threads::<tantivy::TantivyDocument>(1, 50_000_000)?;
    w.delete_term(Term::from_field_text(f.unit, &unit));
    for d in &docs {
        w.add_document(to_tantivy(d, &f))?;
    }
    w.commit()?;
    let t = Instant::now();
    let auto_gen = auto.searcher().generation().generation_id();
    let mut visible_ms = None;
    while t.elapsed() < Duration::from_secs(10) {
        if auto.searcher().generation().generation_id() != auto_gen || auto.searcher().num_docs() != held.num_docs() {
            visible_ms = Some(t.elapsed().as_secs_f64() * 1e3);
            break;
        }
        std::thread::sleep(Duration::from_micros(200));
    }
    let ids = index.searchable_segment_ids()?;
    let t2 = Instant::now();
    if ids.len() > 1 {
        w.merge(&ids).wait()?;
    }
    w.wait_merging_threads()?;
    let merge_ms = t2.elapsed().as_secs_f64() * 1e3;
    let after = answers(&held, &f, &qs)?;
    let t3 = Instant::now();
    manual.reload()?;
    let reload_ms = t3.elapsed().as_secs_f64() * 1e3;
    let fresh = manual.searcher();
    let same = before == after;
    let changed = answers(&fresh, &f, &qs)? != before;
    println!(
        "{}",
        serde_json::json!({
            "queries": qs.len(), "held_generation": gen0, "held_answers_equal": same,
            "fresh_answers_differ": changed, "on_commit_with_delay_visible_ms": visible_ms,
            "forced_merge_ms": merge_ms, "manual_reload_ms": reload_ms,
            "segments_held": held.segment_readers().len(), "segments_fresh": fresh.segment_readers().len(),
        })
    );
    if !same {
        return Err("the held searcher answered differently after the delta and merge".into());
    }
    Ok(())
}
