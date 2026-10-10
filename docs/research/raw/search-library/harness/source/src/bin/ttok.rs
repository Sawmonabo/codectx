//! ttok <strings.jsonl>: the (offset, hex term) pairs per JSON string line, in
//! the exact format of tokgen's `tok` mode, for a byte-for-byte diff.
use std::io::{BufRead, Write};
fn main() {
    let path = std::env::args().nth(1).expect("ttok <strings.jsonl>");
    let r = std::io::BufReader::new(std::fs::File::open(path).unwrap());
    let mut w = std::io::BufWriter::new(std::io::stdout());
    for line in r.lines() {
        let s: String = serde_json::from_str(&line.unwrap()).unwrap();
        let mut out: Vec<serde_json::Value> = Vec::new();
        tbench::unicode61::for_each_token(&s, |p, _, _, t| {
            out.push((p as i64).into());
            out.push(t.bytes().map(|b| format!("{b:02x}")).collect::<String>().into());
        });
        writeln!(w, "{}", serde_json::to_string(&out).unwrap()).unwrap();
    }
}
