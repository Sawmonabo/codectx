//! tdelta <docs.jsonl> <index dir> [unit] [--merge] [--delete unit|id|query]
//!        [--merge-policy SPEC] [--soft FILE]
//!
//! Deletes one unit's documents by `unit` term, re-adds them with the first one's
//! body changed (" deltachange1" appended, token_count recounted), commits, then
//! waits for the merges the commit started. Writes results/tantivy-delta-put,
//! -delta-commit, -delta (put + commit) and -delta-merge (the wait, not in delta).
//! With --merge it then force-merges every searchable segment into one with a
//! fresh writer and waits, as -delta-forcemerge, so deleted documents leave the
//! statistics; segment and deleted-document counts are printed before and after.
//!
//! --delete unit (default): one delete_term on `unit`; id: one delete_term per
//!   old document's `id`; query: delete_query(TermQuery on `unit`).
//! --merge-policy SPEC: the delta writer's merge policy (variants::merge_policy),
//!   e.g. log:del=0.2 to let deletes trigger merges.
//! --soft FILE: delete nothing; write the old documents' addresses
//!   ("<segment id>\t<doc>") to FILE for `tquery --hide FILE`, and keep segments
//!   unmerged (NoMergePolicy) so the addresses stay valid.
//! Phases gain one tag per option that is on (e.g. delta-delid, delta-soft).
use std::io::Write;
use std::time::Instant;
use tantivy::query::TermQuery;
use tantivy::schema::IndexRecordOption;
use tantivy::{ReloadPolicy, Term};
use tbench::{anyhow_lite::Result, append_result, disk_bytes, open_index, read_docs, rusage, to_tantivy, variants, BuildLine};

fn take(a: &mut Vec<String>, flag: &str) -> Result<Option<String>> {
    match a.iter().position(|x| x == flag) {
        None => Ok(None),
        Some(i) => {
            let v = a.get(i + 1).cloned().ok_or_else(|| format!("{flag} needs a value"))?;
            a.drain(i..i + 2);
            Ok(Some(v))
        }
    }
}

fn main() -> Result<()> {
    let mut a: Vec<String> = std::env::args().collect();
    let force = a.iter().any(|x| x == "--merge");
    a.retain(|x| x != "--merge");
    let delete = take(&mut a, "--delete")?.unwrap_or_else(|| "unit".into());
    if !["unit", "id", "query"].contains(&delete.as_str()) {
        return Err("--delete is unit, id or query".into());
    }
    let policy = take(&mut a, "--merge-policy")?;
    let soft = take(&mut a, "--soft")?;
    if a.len() < 3 {
        return Err("usage: tdelta <docs.jsonl> <dir> [unit] [--merge] [--delete unit|id|query] [--merge-policy SPEC] [--soft FILE]".into());
    }
    if soft.is_some() && force {
        return Err("--soft keeps the old addresses; a forced merge would renumber them".into());
    }
    let mut tag = String::new();
    if delete != "unit" {
        tag += &format!("-del{delete}");
    }
    if let Some(p) = &policy {
        tag += &format!("-mp{}", p.replace([':', ',', '=', '+', '.'], ""));
    }
    if soft.is_some() {
        tag += "-soft";
    }
    let dir = std::path::Path::new(&a[2]);
    let mut want = a.get(3).cloned();
    let mut docs = Vec::new();
    read_docs(std::path::Path::new(&a[1]), |d| {
        if want.is_none() {
            want = Some(d.unit.clone());
        }
        if want.as_deref() == Some(d.unit.as_str()) {
            docs.push(d);
        }
        Ok(())
    })?;
    let unit = want.ok_or("docs.jsonl is empty")?;
    if docs.is_empty() {
        return Err(format!("no document carries unit {unit:?}").into());
    }
    docs[0].body.push_str(" deltachange1");
    docs[0].token_count = docs[0].unicode61_count() as i64;
    let (index, f) = open_index(dir, None)?;
    // The old documents' ids and addresses, read before the writer changes anything.
    let searcher = index.reader_builder().reload_policy(ReloadPolicy::Manual).try_into()?.searcher();
    let mut old_ids = Vec::new();
    let mut old_addrs = Vec::new();
    for seg in searcher.segment_readers() {
        let units = seg.fast_fields().str("unit")?.ok_or("the unit fast field is missing")?;
        let ids = seg.fast_fields().str("id")?;
        let Some(o) = units.dictionary().term_ord(unit.as_bytes())? else { continue };
        for d in 0..seg.max_doc() {
            if seg.alive_bitset().is_some_and(|b| !b.is_alive(d)) || units.term_ords(d).next() != Some(o) {
                continue;
            }
            old_ids.push(tbench::library::id_of(&ids, d));
            old_addrs.push(format!("{}\t{d}", seg.segment_id().uuid_string()));
        }
    }
    let mut w = index.writer_with_num_threads::<tantivy::TantivyDocument>(1, 50_000_000)?;
    match (&soft, &policy) {
        (Some(_), _) => w.set_merge_policy(Box::new(tantivy::indexer::NoMergePolicy)),
        (None, Some(p)) => w.set_merge_policy(variants::merge_policy(p)?),
        _ => {}
    }
    let n = docs.len() as u64;
    let line = |phase: &str, wall| {
        let (cpu, rss) = rusage();
        BuildLine { arm: "tantivy".into(), phase: phase.into(), wall_ms: wall, cpu_ms: cpu, peak_rss_bytes: rss, disk_bytes: disk_bytes(dir), docs: n }
    };
    let t0 = Instant::now();
    match (soft.is_some(), delete.as_str()) {
        (true, _) => {}
        (false, "id") => {
            for id in &old_ids {
                w.delete_term(Term::from_field_text(f.id, id));
            }
        }
        (false, "query") => {
            w.delete_query(Box::new(TermQuery::new(Term::from_field_text(f.unit, &unit), IndexRecordOption::Basic)))?;
        }
        _ => {
            w.delete_term(Term::from_field_text(f.unit, &unit));
        }
    }
    for d in &docs {
        w.add_document(to_tantivy(d, &f))?;
    }
    let put = t0.elapsed().as_secs_f64() * 1e3;
    let t1 = Instant::now();
    w.commit()?;
    let commit = t1.elapsed().as_secs_f64() * 1e3;
    let total = t0.elapsed().as_secs_f64() * 1e3;
    append_result("tantivy", &format!("delta-put{tag}"), &line(&format!("delta-put{tag}"), put))?;
    append_result("tantivy", &format!("delta-commit{tag}"), &line(&format!("delta-commit{tag}"), commit))?;
    append_result("tantivy", &format!("delta{tag}"), &line(&format!("delta{tag}"), total))?;
    if let Some(file) = &soft {
        let mut out = std::fs::File::create(file)?;
        for l in &old_addrs {
            writeln!(out, "{l}")?;
        }
        eprintln!("tdelta --soft: {} old addresses written for tquery --hide", old_addrs.len());
    }
    let t2 = Instant::now();
    w.wait_merging_threads()?;
    let merge = t2.elapsed().as_secs_f64() * 1e3;
    append_result("tantivy", &format!("delta-merge{tag}"), &line(&format!("delta-merge{tag}"), merge))?;
    let census = |index: &tantivy::Index| -> Result<(usize, u64)> {
        let metas = index.searchable_segment_metas()?;
        Ok((metas.len(), metas.iter().map(|m| m.num_deleted_docs() as u64).sum()))
    };
    let (segs, deleted) = census(&index)?;
    eprintln!("tdelta: after the delta, {segs} segments holding {deleted} deleted documents");
    if force {
        let mut w = index.writer_with_num_threads::<tantivy::TantivyDocument>(1, 50_000_000)?;
        let t3 = Instant::now();
        let ids = index.searchable_segment_ids()?;
        if !ids.is_empty() {
            w.merge(&ids).wait()?;
        }
        w.wait_merging_threads()?;
        let fm = t3.elapsed().as_secs_f64() * 1e3;
        append_result("tantivy", &format!("delta-forcemerge{tag}"), &line(&format!("delta-forcemerge{tag}"), fm))?;
        let (segs, deleted) = census(&index)?;
        eprintln!("tdelta: forced merge {fm:.2} ms, now {segs} segments holding {deleted} deleted documents");
    }
    eprintln!("tdelta unit {unit:?}: {n} docs, put {put:.2} ms, commit {commit:.2} ms, delta {total:.2} ms, merge wait {merge:.2} ms");
    Ok(())
}
