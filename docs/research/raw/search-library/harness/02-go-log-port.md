# 02 — The IDF's logarithm: a bit-identical port of Go's math.Log

**Why it matters.** Our score is quantized to integers, `round(score · 1e6)`, and pages are ordered on those integers. A last-place difference in `ln` can move a score across a .5 micro boundary and reorder a page. IDF is `ln(1 + (N − df + 0.5)/(df + 0.5))`, so the logarithm must match Go exactly.

**What Go runs.** On amd64 (and s390x), `math.Log` dispatches to assembly (`math/log_asm.go`: `haveArchLog = true`; `math/log_amd64.s`), not to the pure-Go `log`. The assembly is the same msun algorithm with one difference: its reduction step tests `CMPSD … 5` (not-less-than), so it takes `f1 <= √2/2` where the pure-Go code says `f1 < √2/2`. The build uses `GOAMD64=v1`, so Go fuses no multiply-adds. Rust does not fuse either.

**Port.** `go_log` in `source/src/lib.rs` follows the assembly operation for operation: frexp via bit masks, the `<=` reduction, and the same polynomial evaluation order. `bm25_saturation` keeps Go's expression order, `wtf·(k1+1) / (wtf + k1·(1 − b + b·dl/avgdl))`. Rounding is half away from zero in both languages (`math.Round`, `f64::round`).

**Check.** `source/probes/golog/main.go` prints the bits of `x` and `math.Log(x)` for 200 127 values:
- edge values: 1, 2, 0.5, √2/2 and its neighbours;
- every IDF argument for N = 1, 4, 13, … up to 600 000 with df doubling;
- 200 000 pseudo-random arguments.

`tlog` compares them. Result: **`go_log` differs on 0; Rust's `f64::ln` (glibc) differs on 1 891** (about 0.9 %). Using the platform `ln` would have been a real source of page differences.
