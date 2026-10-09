//! tqrels <docs.jsonl> <outdir> <seed>
//!
//! Known-item query sets built from the export, for scoring every arm from the
//! result lines it already writes. The population is the tree-sitter declaration
//! documents (unit "treesitter/...") of kinds function, method, struct, class,
//! interface, type, enum and trait with a non-empty name; it is ordered by search
//! key and shuffled with a xorshift generator seeded by <seed>, and each set takes
//! up to 1000 distinct queries in that order. Writes queries-<set>.txt (one query
//! per line) and qrels-<set>.tsv (query<TAB>relevant search key, one line per key).
//!
//! name:    query = the name exactly; relevant = every document whose name equals it.
//! doc:     the body's distinct unicode61 tokens that are not tokens of the name, path
//!          or qualified name; at least 6 are needed; query = the 4 with the lowest
//!          document frequency in the export (ties by bytes); relevant = that document.
//! subword: names with at least 2 camelCase/snake_case parts; query = one part of 4+
//!          characters picked by the generator, lower-cased; relevant = every
//!          declaration document whose name has that part as a whole part.
//! ident:   the quality design's identifier band: names that are one unicode61
//!          token with at least 2 camelCase parts, ordered by that token's document
//!          frequency; 5 rare, 5 mid and 5 frequent ones (fewer on a small corpus).
//!          Each yields 4 queries: the whole identifier ("getUserName"), one inner
//!          part picked by the generator ("user"), the parts as words ("get user
//!          name") and the parts as a phrase ("\"get user name\""); relevant = every
//!          document whose name equals the identifier. A query text that another
//!          identifier already produced is skipped.
use std::collections::{BTreeSet, HashMap, HashSet};
use std::io::Write;
use tbench::{anyhow_lite::Result, read_docs, unicode61, Doc};

const KINDS: [&str; 8] = ["function", "method", "struct", "class", "interface", "type", "enum", "trait"];
const PER_SET: usize = 1000;

struct Rng(u64);
impl Rng {
    fn next(&mut self) -> u64 {
        self.0 ^= self.0 << 13;
        self.0 ^= self.0 >> 7;
        self.0 ^= self.0 << 17;
        self.0
    }
}

fn usable(q: &str) -> bool {
    !q.is_empty() && !q.contains(['"', '\t', '\n', '\r']) && !unicode61::tokenize(q).is_empty()
}

fn write_set(dir: &std::path::Path, set: &str, qs: &[(String, Vec<String>)]) -> Result<()> {
    let mut q = std::io::BufWriter::new(std::fs::File::create(dir.join(format!("queries-{set}.txt")))?);
    let mut r = std::io::BufWriter::new(std::fs::File::create(dir.join(format!("qrels-{set}.tsv")))?);
    let mut rels = 0;
    for (text, keys) in qs {
        writeln!(q, "{text}")?;
        for k in keys {
            writeln!(r, "{text}\t{k}")?;
            rels += 1;
        }
    }
    eprintln!("tqrels {set}: {} queries, {rels} relevant keys", qs.len());
    Ok(())
}

fn main() -> Result<()> {
    let a: Vec<String> = std::env::args().collect();
    if a.len() != 4 {
        return Err("usage: tqrels <docs.jsonl> <outdir> <seed>".into());
    }
    let out = std::path::Path::new(&a[2]);
    std::fs::create_dir_all(out)?;
    let seed: u64 = a[3].parse()?;
    let mut df: HashMap<String, u64> = HashMap::new();
    let mut by_name: HashMap<String, Vec<String>> = HashMap::new();
    let mut decls: Vec<Doc> = Vec::new();
    read_docs(std::path::Path::new(&a[1]), |d| {
        let mut seen = HashSet::new();
        for c in d.columns() {
            unicode61::for_each_token(c, |_, _, _, t| {
                if !seen.contains(t) {
                    seen.insert(t.to_string());
                }
            });
        }
        for t in seen {
            *df.entry(t).or_default() += 1;
        }
        if !d.name.is_empty() {
            by_name.entry(d.name.clone()).or_default().push(d.id.clone());
        }
        if d.unit.starts_with("treesitter/") && KINDS.contains(&d.kind.as_str()) && !d.name.is_empty() {
            decls.push(d);
        }
        Ok(())
    })?;
    decls.sort_by(|x, y| x.id.cmp(&y.id));
    let mut rng = Rng(seed.wrapping_mul(0x9E37_79B9_7F4A_7C15) | 1);
    for i in (1..decls.len()).rev() {
        decls.swap(i, (rng.next() % (i as u64 + 1)) as usize);
    }
    eprintln!("tqrels: {} declaration documents in the population", decls.len());

    let mut name_set = Vec::new();
    let mut used = HashSet::new();
    for d in &decls {
        if name_set.len() == PER_SET {
            break;
        }
        if usable(&d.name) && used.insert(d.name.clone()) {
            name_set.push((d.name.clone(), by_name[&d.name].clone()));
        }
    }
    write_set(out, "name", &name_set)?;

    let mut doc_set = Vec::new();
    let mut used = HashSet::new();
    for d in &decls {
        if doc_set.len() == PER_SET {
            break;
        }
        let own: HashSet<String> = [&d.name, &d.path, &d.qualified_name].iter().flat_map(|c| unicode61::tokenize(c)).collect();
        let body: BTreeSet<String> = unicode61::tokenize(&d.body).into_iter().filter(|t| !own.contains(t)).collect();
        if body.len() < 6 {
            continue;
        }
        let mut v: Vec<&String> = body.iter().collect();
        v.sort_by(|x, y| df[*x].cmp(&df[*y]).then_with(|| x.cmp(y)));
        let q = v[..4].iter().map(|s| s.as_str()).collect::<Vec<_>>().join(" ");
        if usable(&q) && used.insert(q.clone()) {
            doc_set.push((q, vec![d.id.clone()]));
        }
    }
    write_set(out, "doc", &doc_set)?;

    let parts_of = |n: &str| -> Vec<String> { unicode61::split_parts(n).iter().map(|p| p.to_lowercase()).collect() };
    let mut by_part: HashMap<String, Vec<String>> = HashMap::new();
    for d in &decls {
        let mut seen = HashSet::new();
        for p in parts_of(&d.name) {
            if seen.insert(p.clone()) {
                by_part.entry(p).or_default().push(d.id.clone());
            }
        }
    }
    let mut sub_set = Vec::new();
    let mut used = HashSet::new();
    for d in &decls {
        if sub_set.len() == PER_SET {
            break;
        }
        let parts = parts_of(&d.name);
        if parts.len() < 2 {
            continue;
        }
        let long: Vec<&String> = parts.iter().filter(|p| p.chars().count() >= 4).collect();
        if long.is_empty() {
            continue;
        }
        let q = long[(rng.next() % long.len() as u64) as usize].clone();
        if usable(&q) && used.insert(q.clone()) {
            let mut keys = by_part[&q].clone();
            keys.sort();
            sub_set.push((q, keys));
        }
    }
    write_set(out, "subword", &sub_set)?;

    let mut idents: Vec<(u64, String)> = Vec::new();
    let mut seen = HashSet::new();
    for d in &decls {
        let toks = unicode61::tokenize(&d.name);
        if toks.len() == 1 && parts_of(&d.name).len() >= 2 && seen.insert(d.name.clone()) {
            idents.push((df.get(&toks[0]).copied().unwrap_or(0), d.name.clone()));
        }
    }
    idents.sort();
    let mut pick: Vec<String> = Vec::new();
    let n = idents.len();
    let mid = n / 2;
    for i in (0..5.min(n)).chain(mid.saturating_sub(2)..(mid + 3).min(n)).chain(n.saturating_sub(5)..n) {
        if !pick.contains(&idents[i].1) {
            pick.push(idents[i].1.clone());
        }
    }
    let mut ident_set = Vec::new();
    let mut used = HashSet::new();
    for name in &pick {
        let parts = parts_of(name);
        let inner = parts[(rng.next() % parts.len() as u64) as usize].clone();
        let words = parts.join(" ");
        for q in [name.clone(), inner, words.clone(), format!("\"{words}\"")] {
            if usable(&q.replace('"', "")) && used.insert(q.clone()) {
                ident_set.push((q, by_name[name].clone()));
            }
        }
    }
    write_set(out, "ident", &ident_set)?;
    Ok(())
}
