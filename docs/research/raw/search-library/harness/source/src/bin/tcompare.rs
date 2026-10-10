//! tcompare <a.jsonl> <b.jsonl>
//!
//! Per query (in a's order), from each file's run-0 line (the first line of the
//! query when no run 0 exists): both hit counts, |A∩B| over the first 200 keys
//! of each page, and |A∩B| over the first 10.
use std::collections::{HashMap, HashSet};
use tbench::{anyhow_lite::Result, QueryLine};

fn load(p: &str) -> Result<(Vec<String>, HashMap<String, QueryLine>)> {
    let mut order = Vec::new();
    let mut by: HashMap<String, QueryLine> = HashMap::new();
    for l in std::fs::read_to_string(p)?.lines().filter(|l| !l.is_empty()) {
        let q: QueryLine = serde_json::from_str(l).map_err(|e| format!("{p}: {e}"))?;
        let replace = match by.get(&q.query) {
            None => {
                order.push(q.query.clone());
                true
            }
            Some(have) => have.run != 0 && q.run == 0,
        };
        if replace {
            by.insert(q.query.clone(), q);
        }
    }
    Ok((order, by))
}

fn overlap(a: &[String], b: &[String], n: usize) -> usize {
    let x: HashSet<&String> = a.iter().take(n).collect();
    b.iter().take(n).filter(|k| x.contains(k)).count()
}

fn main() -> Result<()> {
    let a: Vec<String> = std::env::args().collect();
    if a.len() != 3 {
        return Err("usage: tcompare <a.jsonl> <b.jsonl>".into());
    }
    let (order, ka) = load(&a[1])?;
    let (order_b, kb) = load(&a[2])?;
    println!("{:<40} {:>10} {:>10} {:>9} {:>8}", "query", "hits A", "hits B", "top200∩", "top10∩");
    for q in &order {
        let x = &ka[q];
        match kb.get(q) {
            Some(y) => println!(
                "{:<40} {:>10} {:>10} {:>5}/{:<3} {:>4}/{:<3}",
                q, x.hits, y.hits, overlap(&x.page, &y.page, 200), x.page.len().min(200).max(y.page.len().min(200)),
                overlap(&x.page, &y.page, 10), x.page.len().min(10).max(y.page.len().min(10))
            ),
            None => println!("{:<40} {:>10} {:>10}", q, x.hits, "absent"),
        }
    }
    for q in order_b.iter().filter(|q| !ka.contains_key(*q)) {
        println!("{:<40} {:>10} {:>10}", q, "absent", kb[q].hits);
    }
    Ok(())
}
