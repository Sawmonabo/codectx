//! tgen <out docs.jsonl> <docs> <out queries.txt> <out tokcheck.jsonl>
//! A synthetic docs.jsonl in the contract's shape: filesystem units of chunk
//! documents that share their file's node id, and tree-sitter units of symbol
//! documents; mixed-case identifiers, digits, accents, CJK and one over-long token.
use std::io::Write;
use tbench::{Bytes, Doc};

struct Rng(u64);
impl Rng {
    fn next(&mut self) -> u64 {
        self.0 ^= self.0 << 13;
        self.0 ^= self.0 >> 7;
        self.0 ^= self.0 << 17;
        self.0
    }
    fn pick<'a>(&mut self, xs: &[&'a str]) -> &'a str {
        xs[(self.next() % xs.len() as u64) as usize]
    }
    fn below(&mut self, n: u64) -> u64 {
        self.next() % n
    }
}

const WORDS: &[&str] = &[
    "client", "server", "Pod", "pod", "controller", "reconcile", "Reconcile", "informer", "cache", "queue",
    "watch", "handler", "error", "err", "context", "ctx", "return", "func", "struct", "interface", "string",
    "int64", "nil", "true", "false", "config", "Config", "namespace", "kube_proxy", "apiServer", "HTTPClient",
    "parseURL", "v1beta1", "x86_64", "café", "naïve", "Größe", "ÉLAN", "über", "Ωmega", "日本語", "中文",
    "résumé", "coöperate", "i18n", "utf-8", "TODO", "fixme", "the", "a", "of", "and", "to", "in",
    "zebra", "quokka", "xylophone", "deltachange0",
];
const DIRS: &[&str] = &["pkg/api", "pkg/controller", "cmd/server", "internal/cache", "docs", "staging/src/k8s.io/client-go"];
const KINDS: &[&str] = &["function", "method", "type", "variable", "constant", "field"];

fn sentence(r: &mut Rng, n: u64) -> String {
    let mut s = String::new();
    for i in 0..n {
        if i > 0 {
            s.push_str(r.pick(&[" ", " ", " ", ".", ", ", "(", ") ", "\n", "\t", "::", "->", "_", "/"]));
        }
        s.push_str(r.pick(WORDS));
    }
    s
}

fn hash(parts: &[&str]) -> String {
    let mut h: u64 = 0xcbf29ce484222325;
    for p in parts {
        for b in p.bytes().chain([0u8]) {
            h ^= b as u64;
            h = h.wrapping_mul(0x100000001b3);
        }
    }
    format!("{h:016x}{:016x}", h.rotate_left(29) ^ 0x9e3779b97f4a7c15)
}

fn main() {
    let a: Vec<String> = std::env::args().collect();
    let (out, total): (&str, usize) = (&a[1], a[2].parse().unwrap());
    let mut r = Rng(0x5eed_1234_abcd_ef01);
    let mut docs: Vec<Doc> = Vec::new();
    let mut fileno = 0;
    while docs.len() < total {
        fileno += 1;
        let dir = r.pick(DIRS);
        let path = format!("{dir}/file_{fileno}{}", r.pick(&[".go", ".go", ".md", ".py", "_test.go"]));
        let file_id = hash(&["file", &path]);
        let file_node = hash(&["node", &path]);
        // Filesystem unit: chunks of the file, all under the file's node id.
        let chunks = 1 + r.below(4);
        let mut at = 0u64;
        for c in 0..chunks {
            let words = 20 + r.below(200);
            let mut body = sentence(&mut r, words);
            if fileno == 7 && c == 0 {
                body = format!("{}中文 tail", "x".repeat(32767));
            }
            if fileno == 9 && c == 0 {
                body = "the the the the ".repeat(300) + "kube_proxy apiServer";
            }
            let len = body.len() as u64;
            docs.push(Doc {
                id: hash(&["chunk", &file_id, &c.to_string()]),
                node_id: file_node.clone(),
                file_id: file_id.clone(),
                path: path.clone(),
                kind: "file".into(),
                name: String::new(),
                qualified_name: String::new(),
                signature: String::new(),
                bytes: Bytes { start: at, end: at + len },
                body,
                token_count: 0,
                unit: format!("filesystem/{dir}"),
            });
            at += len;
        }
        // Tree-sitter unit: one symbol document per declaration.
        if path.ends_with(".go") || path.ends_with(".py") {
            for s in 0..r.below(12) {
                let name = format!("{}{}", r.pick(WORDS), r.pick(&["", "Handler", "_v2", "Config", ""]));
                let qn = format!("{}.{}", dir.replace('/', "."), name);
                let sig = if r.below(3) == 0 { String::new() } else { format!("func {}({} {}) error", name, r.pick(WORDS), r.pick(WORDS)) };
                let words = 3 + r.below(15);
                let body = if r.below(2) == 0 { String::new() } else { sentence(&mut r, words) };
                let start = r.below(at.max(1));
                docs.push(Doc {
                    id: hash(&["symbol", &file_id, &s.to_string()]),
                    node_id: hash(&["sym", &path, &s.to_string()]),
                    file_id: file_id.clone(),
                    path: path.clone(),
                    kind: r.pick(KINDS).into(),
                    name: if s == 3 { String::new() } else { name },
                    qualified_name: qn,
                    signature: sig,
                    bytes: Bytes { start, end: start + r.below(400) },
                    body,
                    token_count: 0,
                    unit: format!("treesitter/{path}"),
                });
            }
        }
    }
    docs.truncate(total);
    // Contract: lines grouped by unit, in build order (first appearance).
    let mut order: Vec<String> = Vec::new();
    let mut by: std::collections::HashMap<String, Vec<Doc>> = Default::default();
    for d in docs {
        if !by.contains_key(&d.unit) {
            order.push(d.unit.clone());
        }
        by.entry(d.unit.clone()).or_default().push(d);
    }
    let mut w = std::io::BufWriter::new(std::fs::File::create(out).unwrap());
    let mut tc = std::io::BufWriter::new(std::fs::File::create(&a[4]).unwrap());
    let mut n = 0;
    for u in &order {
        for mut d in by.remove(u).unwrap() {
            d.token_count = d.unicode61_count() as i64;
            for c in d.columns() {
                writeln!(tc, "{}", serde_json::to_string(c).unwrap()).unwrap();
            }
            writeln!(w, "{}", serde_json::to_string(&d).unwrap()).unwrap();
            n += 1;
        }
    }
    writeln!(tc, "{}", serde_json::to_string(" \u{301} a\u{301}b \u{301}\u{301} ﬁ ǅ İstanbul ΣΑΣ ß ＡＢＣ ①② ٣ \u{e000}x\u{200b}y 😀z").unwrap()).unwrap();
    writeln!(tc, "{}", serde_json::to_string(&format!("{}中", "a".repeat(32767))).unwrap()).unwrap();
    let qs = [
        "the", "pod", "controller", "error", "a",
        "café", "Größe", "日本語", "quokka", "deltachange0",
        "reconcile informer", "kube_proxy apiServer", "HTTPClient parseURL",
        "\"the the\"", "\"kube_proxy apiServer\"", "\"client server\"", "\"x86_64\"", "\"func reconcile\" cache",
    ];
    std::fs::write(&a[3], qs.join("\n") + "\n").unwrap();
    eprintln!("tgen: {n} documents in {} units", order.len());
}
