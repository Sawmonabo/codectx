// One Rust function per group of the control-flow and definition forms the
// Rust lowering's contract names: branches and if let, the question mark
// operator, loop with a valued break, while and while let, iterator loops
// with labels, labelled blocks, match with guards, let-else, shadowing,
// destructuring, compound and place assignments, a method on self, macros,
// closures, async blocks and a nested function item. It is a benchmark input,
// not a proof of coverage: the lowering's golden tests are that.

use std::collections::HashMap;

pub struct Counter {
    total: i64,
    seen: HashMap<String, i64>,
}

pub fn branches(x: i64, y: i64) -> i64 {
    let (x, y) = if x > y { (y, x) } else { (x, y) };
    if x == y {
        return 0;
    } else if x < 0 && y > 0 || x == -y {
        return 1;
    }
    let z = if x > 0 { x } else { -x };
    (y - x) * z
}

pub fn question(input: &str, map: &HashMap<String, i64>) -> Option<i64> {
    let n: i64 = input.trim().parse().ok()?;
    let base = map.get(input)?;
    Some(n + *base)
}

pub fn loops(items: &[i64]) -> i64 {
    let mut sum = 0;
    let mut i = 0;
    while i < items.len() {
        sum += items[i];
        i += 1;
    }
    let mut stack = items.to_vec();
    while let Some(top) = stack.pop() {
        if top < 0 {
            continue;
        }
        sum += top;
    }
    let found = loop {
        if sum % 2 == 0 {
            break sum / 2;
        }
        sum += 1;
    };
    found
}

pub fn labelled(grid: &[Vec<i64>], want: i64) -> Option<(usize, usize)> {
    let mut hit = None;
    'rows: for (r, row) in grid.iter().enumerate() {
        for (c, &v) in row.iter().enumerate() {
            if v == want {
                hit = Some((r, c));
                break 'rows;
            }
            if v > want {
                continue 'rows;
            }
        }
    }
    let first = 'check: {
        if grid.is_empty() {
            break 'check 0;
        }
        grid[0].len()
    };
    hit.map(|(r, c)| (r + first, c))
}

pub fn matching(value: Option<i64>, limit: i64) -> i64 {
    let v = match value {
        None => 0,
        Some(n) if n > limit => limit,
        Some(n @ 0..=9) => n * 2,
        Some(n) => n,
    };
    let Some(w) = value else {
        panic!("no value, limit {}", limit);
    };
    match (v, w) {
        (0, _) | (_, 0) => 0,
        (a, b) => a + b,
    }
}

impl Counter {
    pub fn add(&mut self, key: &str, by: i64) -> i64 {
        let entry = self.seen.entry(key.to_string()).or_insert(0);
        *entry += by;
        self.total += by;
        let total = self.total;
        let total = total * 2;
        println!("{} {}", key, total);
        assert!(total >= 0, "negative total");
        total
    }
}

pub fn closures(xs: &[i64]) -> (i64, impl Fn() -> i64) {
    let mut total = 0;
    let mut add = |v: i64| total += v;
    for &x in xs {
        add(x);
    }
    let snapshot = total;
    let (mut a, mut b) = (1, 2);
    (a, b) = (b, a);
    fn helper(n: i64) -> i64 {
        n + 1
    }
    let later = async move { snapshot + a + b };
    drop(later);
    (helper(total), move || snapshot)
}
