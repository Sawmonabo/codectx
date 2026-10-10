//! teval <qrels-<set>.tsv> <queries-<set>.txt> <results.jsonl>...
//!
//! Scores each (arm, phase) found in the result files on one query set, from the
//! run-0 line of each query (else its first line): MRR@10, Success@1, Success@10,
//! Recall@200 (relevant keys in the first 200 / relevant keys), mean hits, and the
//! queries with zero hits. A query with no result line counts as missing and is
//! scored as a miss with zero hits. Keys not in the qrels are simply not relevant.
use std::collections::{BTreeMap, HashMap, HashSet};
use tbench::{anyhow_lite::Result, QueryLine};

fn main() -> Result<()> {
    let a: Vec<String> = std::env::args().collect();
    if a.len() < 4 {
        return Err("usage: teval <qrels.tsv> <queries.txt> <results.jsonl>...".into());
    }
    let set = std::path::Path::new(&a[1])
        .file_stem()
        .and_then(|s| s.to_str())
        .map(|s| s.trim_start_matches("qrels-").to_string())
        .unwrap_or_default();
    let mut rel: HashMap<String, HashSet<String>> = HashMap::new();
    for l in std::fs::read_to_string(&a[1])?.lines() {
        if let Some((q, k)) = l.split_once('\t') {
            rel.entry(q.to_string()).or_default().insert(k.to_string());
        }
    }
    let queries: Vec<String> = std::fs::read_to_string(&a[2])?.lines().filter(|l| !l.is_empty()).map(String::from).collect();
    let mut by: BTreeMap<(String, String), HashMap<String, QueryLine>> = BTreeMap::new();
    for p in &a[3..] {
        for l in std::fs::read_to_string(p)?.lines().filter(|l| !l.is_empty()) {
            let q: QueryLine = serde_json::from_str(l).map_err(|e| format!("{p}: {e}"))?;
            let m = by.entry((q.arm.clone(), q.phase.clone())).or_default();
            let replace = m.get(&q.query).map_or(true, |h| h.run != 0 && q.run == 0);
            if replace {
                m.insert(q.query.clone(), q);
            }
        }
    }
    println!("set {set}: {} queries", queries.len());
    println!("{:<34} {:>7} {:>6} {:>7} {:>9} {:>10} {:>6} {:>8}", "arm/phase", "MRR@10", "S@1", "S@10", "R@200", "mean hits", "zero", "missing");
    for ((arm, phase), lines) in &by {
        let (mut mrr, mut s1, mut s10, mut r200, mut hits, mut zero, mut missing) = (0f64, 0, 0, 0f64, 0f64, 0, 0);
        for q in &queries {
            let empty = HashSet::new();
            let r = rel.get(q).unwrap_or(&empty);
            let Some(l) = lines.get(q) else {
                missing += 1;
                zero += 1;
                continue;
            };
            hits += l.hits as f64;
            if l.hits == 0 {
                zero += 1;
            }
            if let Some(rank) = l.page.iter().take(10).position(|k| r.contains(k)) {
                mrr += 1.0 / (rank + 1) as f64;
                s10 += 1;
                if rank == 0 {
                    s1 += 1;
                }
            }
            if !r.is_empty() {
                r200 += l.page.iter().take(200).filter(|k| r.contains(*k)).count() as f64 / r.len() as f64;
            }
        }
        let n = queries.len().max(1) as f64;
        println!(
            "{:<34} {:>7.4} {:>6.4} {:>7.4} {:>9.4} {:>10.1} {:>6} {:>8}",
            format!("{arm}/{phase}"), mrr / n, s1 as f64 / n, s10 as f64 / n, r200 / n, hits / n, zero, missing
        );
    }
    Ok(())
}
