//! tlog <golog.txt>: checks go_log against Go's math.Log bit for bit.
use std::io::BufRead;
fn main() {
    let f = std::fs::File::open(std::env::args().nth(1).unwrap()).unwrap();
    let (mut n, mut bad, mut std_bad) = (0, 0, 0);
    for l in std::io::BufReader::new(f).lines() {
        let l = l.unwrap();
        let mut it = l.split(' ').map(|h| u64::from_str_radix(h, 16).unwrap());
        let (x, want) = (f64::from_bits(it.next().unwrap()), it.next().unwrap());
        n += 1;
        if tbench::go_log(x).to_bits() != want { bad += 1; }
        if x.ln().to_bits() != want { std_bad += 1; }
    }
    println!("tlog: {n} values, go_log differs on {bad}, f64::ln differs on {std_bad}");
}
