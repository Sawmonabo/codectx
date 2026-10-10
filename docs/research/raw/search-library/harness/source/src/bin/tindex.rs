//! tindex <docs.jsonl> <new index dir> <threads> [default|large|<MB total>] [unit|every:<K>|end]
//!        [writer options] [schema options]
//!
//! Builds the tantivy index. Budget `default` is 50 MB per indexing thread (the
//! library's documented starting point), `large` is 2048 MB in total; the total is
//! split evenly across threads. Commit cadence `unit` (the default) commits at
//! every unit boundary, as production seals each unit; `every:K` every K units;
//! `end` only once. A final commit and the wait for background merges close every
//! build (`-final`).
//!
//! Writer options:
//!   --merge-policy log | log:min=N,del=R,maxdocs=M,layer=L,levellog=X | none | none+final
//!       (none+final: NoMergePolicy while building, then one merge of every segment)
//!   --merge-threads N           IndexWriterOptions::num_merge_threads (default 4)
//!   --batch doc|unit            add_document per document (default) or one
//!                               IndexWriter::run(Vec<UserOperation>) per unit
//!   --single-segment            Index::builder().single_segment_index_writer (cadence end only)
//!   --ram-then-persist          build in a RamDirectory, then persist() into <dir> (-persist phase)
//! Schema options (recorded in <dir>/tb-config.json): see variants::IndexConfig::parse —
//!   --tokenizer unicode61|code|default|simple|en_stem|pretokenized, --store stored|none,
//!   --fieldnorms all|text-only|none, --body-record positions|freqs, --schema full|lean,
//!   --lean-numeric, --lean-strings, --kind-fast-only, --path-facet, --kind-code,
//!   --parts-fields, --name-ngram, --field-counts, --docstore none|lz4|zstd:L, --blocksize N.
//!
//! Writes results/tantivy-<label>{-add,-final,[-persist],}.jsonl where label is
//! index-t<threads>-<budget>-<cadence> plus one tag per non-default option.
use std::time::Instant;
use tantivy::indexer::UserOperation;
use tantivy::{IndexWriter, SingleSegmentIndexWriter, TantivyDocument};
use tbench::variants::{self, IndexConfig};
use tbench::{anyhow_lite::Result, append_result, disk_bytes, read_docs, rusage, BuildLine, Doc, Fields};

struct WriterOpts {
    merge_policy: String,
    merge_threads: Option<usize>,
    batch_unit: bool,
    single_segment: bool,
    ram: bool,
}

fn parse_writer(a: &mut Vec<String>) -> Result<WriterOpts> {
    let mut o = WriterOpts { merge_policy: "log".into(), merge_threads: None, batch_unit: false, single_segment: false, ram: false };
    let mut i = 0;
    while i < a.len() {
        match a[i].as_str() {
            "--merge-policy" | "--merge-threads" | "--batch" => {
                let v = a.get(i + 1).cloned().ok_or_else(|| format!("{} needs a value", a[i]))?;
                match a[i].as_str() {
                    "--merge-policy" => {
                        variants::merge_policy(&v)?;
                        o.merge_policy = v
                    }
                    "--merge-threads" => o.merge_threads = Some(v.parse()?),
                    _ => {
                        o.batch_unit = match v.as_str() {
                            "unit" => true,
                            "doc" => false,
                            _ => return Err("--batch is doc or unit".into()),
                        }
                    }
                }
                a.drain(i..i + 2);
            }
            "--single-segment" => {
                o.single_segment = true;
                a.remove(i);
            }
            "--ram-then-persist" => {
                o.ram = true;
                a.remove(i);
            }
            _ => i += 1,
        }
    }
    Ok(o)
}

/// The two writers the build can use, behind one face.
enum Writer {
    Normal(IndexWriter<TantivyDocument>),
    Single(SingleSegmentIndexWriter<TantivyDocument>),
}

fn main() -> Result<()> {
    let mut a: Vec<String> = std::env::args().collect();
    let cfg = IndexConfig::parse(&mut a)?;
    let wo = parse_writer(&mut a)?;
    if a.len() < 4 {
        return Err("usage: tindex <docs.jsonl> <dir> <threads> [default|large|MB] [unit|every:K|end] [options]".into());
    }
    let (docs, dir, threads) = (std::path::Path::new(&a[1]), std::path::Path::new(&a[2]), a[3].parse::<usize>()?);
    let budget_arg = a.get(4).map(String::as_str).unwrap_or("default");
    let budget = match budget_arg {
        "default" => 50_000_000 * threads,
        "large" => 2048 << 20,
        mb => mb.parse::<usize>()? << 20,
    };
    let cadence = a.get(5).map(String::as_str).unwrap_or("unit");
    let every: Option<usize> = match cadence {
        "unit" => Some(1),
        "end" => None,
        c => Some(c.strip_prefix("every:").ok_or("cadence must be unit, every:K or end")?.parse()?),
    };
    if wo.single_segment && every.is_some() {
        return Err("--single-segment writes one segment at the end: use cadence end".into());
    }
    if dir.exists() && std::fs::read_dir(dir)?.next().is_some() {
        return Err(format!("{} is not empty", dir.display()).into());
    }
    std::fs::create_dir_all(dir)?;
    let mut label = format!("index-t{threads}-{budget_arg}-{}", cadence.replace(':', ""));
    let mut tags = vec![cfg.label()];
    if wo.merge_policy != "log" {
        tags.push(format!("mp{}", wo.merge_policy.replace([':', ',', '=', '+', '.'], "")));
    }
    if let Some(m) = wo.merge_threads {
        tags.push(format!("mt{m}"));
    }
    if wo.batch_unit {
        tags.push("batchunit".into());
    }
    if wo.single_segment {
        tags.push("single".into());
    }
    if wo.ram {
        tags.push("ram".into());
    }
    for t in tags.into_iter().filter(|t| !t.is_empty()) {
        label += &format!("-{t}");
    }

    let t0 = Instant::now();
    let ram = tantivy::directory::RamDirectory::create();
    // The single-segment route is Index::builder().single_segment_index_writer spelled
    // out (open_or_create on the empty directory, then SingleSegmentIndexWriter::new), so
    // the analyzers can be registered on that index before it tokenizes.
    let index: tantivy::Index = if wo.single_segment {
        let mmap = tantivy::directory::MmapDirectory::open(dir)?;
        let idx = variants::builder(&cfg)?.open_or_create(mmap)?;
        std::fs::write(dir.join(variants::CONFIG_FILE), serde_json::to_vec_pretty(&cfg)?)?;
        idx
    } else if wo.ram {
        variants::builder(&cfg)?.open_or_create(ram.clone())?
    } else {
        variants::create(dir, &cfg)?
    };
    // The analyzers must be registered before any document is tokenized; the
    // tokenizer manager is shared by every clone of the index.
    variants::register_analyzers(&index);
    let mut w = if wo.single_segment {
        Writer::Single(SingleSegmentIndexWriter::new(index.clone(), budget)?)
    } else {
        // 4 merge threads is IndexWriterOptions' own default (indexer/index_writer.rs:62).
        let ob = tantivy::indexer::IndexWriterOptions::builder()
            .num_worker_threads(threads)
            .memory_budget_per_thread(budget / threads)
            .num_merge_threads(wo.merge_threads.unwrap_or(4))
            .build();
        let w = index.writer_with_options::<TantivyDocument>(ob)?;
        w.set_merge_policy(variants::merge_policy(&wo.merge_policy)?);
        Writer::Normal(w)
    };
    let tok_index = index.clone();
    let fields = Fields::from_schema(&tok_index.schema(), cfg.tokenizer == "pretokenized")?;

    let (mut n, mut units, mut since, mut commits, mut mismatched) = (0u64, 0u64, 0usize, 0u64, 0u64);
    let mut current: Option<String> = None;
    let mut batch: Vec<UserOperation> = Vec::new();
    let flush = |w: &mut Writer, batch: &mut Vec<UserOperation>| -> Result<()> {
        if let Writer::Normal(w) = w {
            if !batch.is_empty() {
                w.run(std::mem::take(batch))?;
            }
        }
        Ok(())
    };
    let add = |w: &mut Writer, batch: &mut Vec<UserOperation>, d: &Doc| -> Result<()> {
        let doc = tbench::to_tantivy(d, &fields);
        match w {
            Writer::Single(s) => s.add_document(doc)?,
            Writer::Normal(nw) => {
                if wo.batch_unit {
                    batch.push(UserOperation::Add(doc));
                } else {
                    nw.add_document(doc)?;
                }
            }
        }
        Ok(())
    };
    read_docs(docs, |d| {
        if current.as_deref() != Some(d.unit.as_str()) {
            if current.is_some() {
                flush(&mut w, &mut batch)?;
                units += 1;
                since += 1;
                if every.is_some_and(|k| since >= k) {
                    if let Writer::Normal(nw) = &mut w {
                        nw.commit()?;
                    }
                    commits += 1;
                    since = 0;
                }
            }
            current = Some(d.unit.clone());
        }
        if d.unicode61_count() != d.token_count.max(0) as u64 {
            mismatched += 1;
        }
        add(&mut w, &mut batch, &d)?;
        n += 1;
        Ok(())
    })?;
    flush(&mut w, &mut batch)?;
    if current.is_some() {
        units += 1;
    }
    let add_ms = t0.elapsed().as_secs_f64() * 1e3;
    let (cpu_add, rss_add) = rusage();
    let line = |phase: &str, wall, cpu, rss| BuildLine {
        arm: "tantivy".into(), phase: phase.into(), wall_ms: wall, cpu_ms: cpu, peak_rss_bytes: rss, disk_bytes: disk_bytes(dir), docs: n,
    };
    append_result("tantivy", &format!("{label}-add"), &line(&format!("{label}-add"), add_ms, cpu_add, rss_add))?;

    let t1 = Instant::now();
    let final_index = match w {
        Writer::Single(s) => {
            let idx = s.finalize()?;
            commits += 1;
            idx
        }
        Writer::Normal(mut nw) => {
            nw.commit()?;
            commits += 1;
            if wo.merge_policy == "none+final" {
                let ids = tok_index.searchable_segment_ids()?;
                if ids.len() > 1 {
                    nw.merge(&ids).wait()?;
                }
            }
            nw.wait_merging_threads()?;
            tok_index.clone()
        }
    };
    let final_ms = t1.elapsed().as_secs_f64() * 1e3;
    let (cpu, rss) = rusage();
    append_result("tantivy", &format!("{label}-final"), &line(&format!("{label}-final"), final_ms, cpu - cpu_add, rss))?;
    if wo.ram {
        let t2 = Instant::now();
        let mmap = tantivy::directory::MmapDirectory::open(dir)?;
        ram.persist(&mmap)?;
        std::fs::write(dir.join(variants::CONFIG_FILE), serde_json::to_vec_pretty(&cfg)?)?;
        let ms = t2.elapsed().as_secs_f64() * 1e3;
        let (c2, r2) = rusage();
        append_result("tantivy", &format!("{label}-persist"), &line(&format!("{label}-persist"), ms, c2 - cpu, r2))?;
    }
    let total = t0.elapsed().as_secs_f64() * 1e3;
    let (cpu, rss) = rusage();
    append_result("tantivy", &label, &line(&label, total, cpu, rss))?;
    let segs = final_index.searchable_segment_metas()?.len();
    eprintln!(
        "tindex {label}: {n} docs, {units} units, {commits} commits, {segs} segments, add {add_ms:.1} ms, final {final_ms:.1} ms, total {total:.1} ms, cpu {cpu:.1} ms, peak rss {rss} B, disk {} B, token_count mismatches {mismatched}",
        disk_bytes(dir)
    );
    Ok(())
}
