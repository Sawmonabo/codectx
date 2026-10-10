//! The scorer's arithmetic: BM25F in f64 with a pinned logarithm and the 1e-6
//! quantization (docs/adr/ADR-0013-search-index.md decision 2).
//!
//! The logarithm is never the platform's: Go's `math.Log` and Rust's `f64::ln`
//! disagree in the last bit on about 1% of the inputs the scorer sees, so both
//! sides use the one algorithm below, the fdlibm reduction exactly as Go's
//! portable `math.Log` writes it.

/// The column weights in the fixed column order name, qualified_name,
/// signature, path, body.
pub const WEIGHTS: [u64; 5] = [5, 5, 2, 2, 1];

const K1: f64 = 1.2;
const K1_PLUS_1: f64 = 2.2;
const ONE_MINUS_B: f64 = 0.25;
const B: f64 = 0.75;

/// The natural logarithm, operation for operation as Go's portable
/// `math.Log` (fdlibm `__ieee754_log`). The constants keep the source's
/// spelling, digit for digit, so the two copies can be compared by eye.
#[allow(clippy::excessive_precision)]
pub fn pinned_log(x: f64) -> f64 {
    const LN2_HI: f64 = 6.93147180369123816490e-01;
    const LN2_LO: f64 = 1.90821492927058770002e-10;
    const L1: f64 = 6.666666666666735130e-01;
    const L2: f64 = 3.999999999940941908e-01;
    const L3: f64 = 2.857142874366239149e-01;
    const L4: f64 = 2.222219843214978396e-01;
    const L5: f64 = 1.818357216161805012e-01;
    const L6: f64 = 1.531383769920937332e-01;
    const L7: f64 = 1.479819860511658591e-01;
    if x.is_nan() || x == f64::INFINITY {
        return x;
    }
    if x < 0.0 {
        return f64::NAN;
    }
    if x == 0.0 {
        return f64::NEG_INFINITY;
    }
    let (mut f1, mut ki) = frexp(x);
    if f1 < std::f64::consts::FRAC_1_SQRT_2 {
        f1 *= 2.0;
        ki -= 1;
    }
    let f = f1 - 1.0;
    let k = ki as f64;
    let s = f / (2.0 + f);
    let s2 = s * s;
    let s4 = s2 * s2;
    let t1 = s2 * (L1 + s4 * (L3 + s4 * (L5 + s4 * L7)));
    let t2 = s4 * (L2 + s4 * (L4 + s4 * L6));
    let r = t1 + t2;
    let hfsq = 0.5 * f * f;
    k * LN2_HI - ((hfsq - (s * (hfsq + r) + k * LN2_LO)) - f)
}

/// Go's `math.Frexp` for a finite, nonzero x: x = frac · 2^exp, frac in [0.5, 1).
fn frexp(x: f64) -> (f64, i32) {
    let mut x = x;
    let mut exp = 0i32;
    // Normalize a subnormal first, as Go's normalize does.
    if x.abs() < f64::MIN_POSITIVE {
        x *= (1u64 << 52) as f64;
        exp = -52;
    }
    let bits = x.to_bits();
    exp += ((bits >> 52) & 0x7ff) as i32 - 1022;
    let frac = f64::from_bits((bits & !(0x7ffu64 << 52)) | (1022u64 << 52));
    (frac, exp)
}

/// The inverse document frequency ln(1 + (N − df + 0.5)/(df + 0.5)) over the
/// visible documents.
pub fn idf(n: u64, df: u64) -> f64 {
    if n == 0 {
        return 0.0;
    }
    let df = df.min(n);
    pinned_log(1.0 + (n as f64 - df as f64 + 0.5) / (df as f64 + 0.5))
}

/// The BM25F saturation of a weighted term frequency at document length dl.
pub fn saturation(wtf: f64, dl: u64, avgdl: f64) -> f64 {
    if wtf <= 0.0 || avgdl <= 0.0 {
        return 0.0;
    }
    wtf * K1_PLUS_1 / (wtf + K1 * (ONE_MINUS_B + B * dl as f64 / avgdl))
}

/// A score on the 1e-6 grid; a non-finite score quantizes to 0.
pub fn quantize(score: f64) -> i64 {
    if !score.is_finite() {
        return 0;
    }
    (score * 1e6).round() as i64
}

/// A block bound on the grid with a margin that keeps it above every floating
/// sum it bounds. An unbounded block (infinite bound) is never skipped.
pub fn safe_bound(x: f64) -> i64 {
    if x.is_nan() || x == f64::INFINITY {
        return i64::MAX;
    }
    quantize(x * (1.0 + 1e-9) + 1e-9)
}
