# 06 — Dependence-core benchmark aggregate over the public corpus matrix

Provenance: the benchmark package `internal/bench` (`TestParseMemory`, `TestFunctionAnalysis`, `TestFileAnalysis`;
row schema in the doc comments of `internal/bench/flowbench_test.go` and `internal/bench/allocator.go`), test binary
built at commit `b6dd3f6`, run 2026-09-26 by the benchmark task, memory-capped, one corpus at a time, every run exit 0.
Corpora and pins: kubernetes `dfd7b93a`, home-assistant core `5d010719`, elasticsearch `3273b67c`, vscode `529ee190`,
rust-lang/rust `5ceaf660`, llvm-project `4257da8e`, and a checkout of the TypeScript compiler at `cf8cf4f6` — which at
that pin is the compiler's Go port with the TypeScript test data, not a TypeScript project (see `05-`). Only the Go and
JavaScript lowerings exist at that commit, so FUNCTION and FILE rows cover those two languages only. Every figure below
is computed per file, then aggregated; this research ran no benchmark. The aggregate is reproduced verbatim.

Column reading. PARSE: `ratio` is the counted native peak (core tree-sitter allocations through the Go hook) divided by
source bytes, per file; `ratio_agg` is sum over sum; `maxPeakMiB@srcKiB` is the largest single-file native peak and
that file's size; `scannerNull` counts rows whose external-scanner bytes are uncounted (plain libc, see
`internal/bench/allocator.go`), which is every Python, C++ and Rust row; `err%` is the share of files that parsed with
errors. FUNCTION: node quantiles per function, the arena's largest footprint, `bound_ratio` (arena bytes after the
three dependence passes ÷ the 96·N+64 design figure), and total milliseconds split as lowering / post-dominators /
control dependence / def-use. FILE: single-threaded parse+lower+analyse wall per corpus and language.

```
PARSE (tree files): corpus lang files srcMiB nativePeakMiB(sum) ratio_agg ratio_p50 p90 p99 max | maxPeakMiB@srcKiB | scannerNull err% parse_s
typescript go 5115 26.21 430.39 16.42 21.77 29.83 37.46 50.63 | 37.36@1466.57 | 0 11.03 2.25
typescript javascript 13131 28.04 734.34 26.19 25.98 42.62 70.93 109.21 | 19.71@264.15 | 0 81.65 11.77
typescript tsx 350 0.25 5.38 21.87 12.92 22.58 22.58 22.58 | 0.24@18.82 | 0 8.00 0.03
typescript typescript 12804 26.91 529.90 19.69 22.32 39.96 66.00 80.43 | 49.76@3077.90 | 0 8.49 2.43
kubernetes c 6 0.01 0.15 15.95 - - - - | 0.05@1.49 | 0 33.33 0.00
kubernetes go 17858 179.53 3695.06 20.58 20.24 27.61 36.49 80.24 | 58.63@1664.11 | 0 0.86 19.21
kubernetes javascript 2 0.03 0.64 22.26 22.26 22.26 22.26 22.26 | 0.62@28.61 | 0 0.00 0.00
kubernetes python 7 0.03 0.65 24.52 26.91 28.72 28.72 28.72 | 0.25@9.48 | 7 0.00 0.00
home-assistant-core javascript 3 0.00 0.03 23.24 - - - - | 0.01@0.58 | 0 0.00 0.00
home-assistant-core python 18931 118.33 2780.94 23.50 22.77 27.84 32.99 77.92 | 11.37@490.54 | 18931 0.06 11.41
home-assistant-core typescript 2 0.00 0.02 30.30 - - - - | 0.01@0.51 | 0 0.00 0.00
elasticsearch c 8 7.40 137.24 18.54 18.42 24.48 24.48 24.48 | 136.74@7550.02 | 0 62.50 6.87
elasticsearch cpp 31 2.95 47.08 15.93 24.48 28.54 30.16 30.16 | 39.09@2684.37 | 31 93.55 0.81
elasticsearch java 32622 294.53 4939.37 16.77 16.85 21.48 26.86 49.30 | 9.93@514.99 | 0 0.04 24.12
elasticsearch javascript 2 0.00 0.03 28.93 - - - - | 0.03@0.97 | 0 0.00 0.00
elasticsearch python 4 0.01 0.42 30.46 33.76 33.76 33.76 33.76 | 0.29@8.89 | 4 0.00 0.00
elasticsearch typescript 45 0.28 6.52 22.96 24.10 27.93 28.43 28.43 | 0.89@32.82 | 0 0.00 0.03
vscode c 3 0.04 0.23 5.83 4.50 4.50 4.50 4.50 | 0.16@36.62 | 0 66.67 0.00
vscode cpp 22 0.12 2.26 18.16 30.32 30.32 30.32 30.32 | 1.86@101.16 | 22 40.91 0.02
vscode go 4 0.00 0.06 26.64 - - - - | 0.04@1.42 | 0 25.00 0.00
vscode java 7 0.01 0.09 18.05 - - - - | 0.03@1.29 | 0 0.00 0.00
vscode javascript 155 1.35 34.05 25.27 25.56 34.46 150.43 150.43 | 4.94@33.63 | 0 3.87 0.14
vscode python 75 0.06 1.65 26.94 32.37 32.37 32.37 32.37 | 0.51@23.99 | 75 4.00 0.01
vscode rust 88 0.88 24.45 27.74 29.25 34.70 43.41 55.11 | 2.94@120.52 | 88 1.14 0.09
vscode tsx 269 2.96 50.17 16.92 18.79 25.06 34.28 34.84 | 2.05@61.16 | 0 5.95 0.24
vscode typescript 13675 168.80 3679.53 21.80 22.40 28.17 38.14 54.81 | 135.09@8192.09 | 0 0.24 16.64
rust c 131 1.17 8.73 7.43 24.17 31.29 34.93 34.93 | 2.65@222.22 | 0 16.03 0.13
rust cpp 18 0.21 3.67 17.49 17.76 19.64 21.50 21.50 | 1.34@72.53 | 18 16.67 0.04
rust javascript 166 1.82 83.09 45.56 18.74 24.58 65.45 65.45 | 68.79@1076.30 | 0 0.00 0.34
rust python 44 0.43 10.44 24.45 24.07 30.49 31.85 31.85 | 1.37@58.48 | 44 0.00 0.05
rust rust 38999 138.55 3360.91 24.26 22.47 37.74 72.60 147.84 | 131.88@2630.12 | 38999 9.16 13.40
rust typescript 31 0.29 5.84 19.96 21.29 24.72 25.66 25.66 | 0.99@54.59 | 0 6.45 0.03
llvm-project c 32317 317.98 3504.64 11.02 9.59 21.94 31.60 260.09 | 303.85@15747.85 | 0 51.56 59.06
llvm-project cpp 40457 426.83 7381.27 17.29 18.73 27.40 44.98 300.80 | 75.07@1796.58 | 40457 31.70 77.25
llvm-project javascript 29 0.28 5.88 20.92 18.84 25.21 32.50 32.50 | 1.53@90.24 | 0 0.00 0.03
llvm-project python 3119 15.19 348.13 22.91 22.68 29.48 39.45 47.33 | 2.74@143.04 | 3119 0.00 1.51
llvm-project rust 2 0.00 0.09 18.83 18.71 18.71 18.71 18.71 | 0.09@4.79 | 2 0.00 0.00
llvm-project typescript 34 0.11 2.16 19.94 19.90 21.92 29.11 29.11 | 0.24@14.83 | 0 0.00 0.01

FUNCTION: corpus lang fns nodes_p50 p99 max | arenaKiB_max bound_ratio p50 p99 max | total_ms lower pd cd du
elasticsearch javascript 4 9 25 25 | 7.23 1.44 3.01 3.01 | 0.22 0.00 0.00 0.01
home-assistant-core javascript 3 3 3 3 | 0.27 0.78 0.78 0.78 | 0.07 0.00 0.00 0.00
kubernetes go 228765 7 81 1909 | 1123.10 1.02 3.85 25.20 | 11599.01 167.51 34.37 341.91
kubernetes javascript 37 5 370 370 | 537.24 0.88 15.46 15.46 | 4.67 0.04 0.01 0.39
llvm-project javascript 835 4 43 323 | 29.64 0.83 2.40 3.90 | 32.78 0.32 0.07 0.42
rust javascript 8452 7 81 3278 | 1566.33 0.92 5.89 15.63 | 1390.41 6.12 1.18 16.29
typescript go 25072 9 72 4429 | 634.17 0.95 4.84 22.92 | 2552.18 20.10 3.89 42.65
typescript javascript 97145 3 53 20005 | 1724.06 0.80 2.66 18.30 | 2145.20 38.02 8.16 46.49
vscode go 7 7 31 31 | 4.05 1 1.37 1.37 | 0.15 0.00 0.00 0.00
vscode javascript 1764 6 102 11162 | 831.48 0.88 5.14 16.27 | 209.40 1.79 0.32 3.76

FILE: corpus lang files wall_s arenaPeakKiB_max nativePeakMiB_max
elasticsearch javascript 2 0.00 8.40 0.03
home-assistant-core javascript 3 0.00 0.37 0.02
kubernetes go 17858 51.47 1155.03 58.63
kubernetes javascript 2 0.01 556.84 0.63
llvm-project javascript 29 0.10 41.50 1.53
rust javascript 166 2.15 1738.33 68.80
typescript go 5115 7.16 820.11 37.36
typescript javascript 13131 17.30 2603.13 19.65
vscode go 4 0.00 5.32 0.04
vscode javascript 155 0.56 1191.27 4.94
```

Whole-run maximum resident set of the benchmark process (`/usr/bin/time -v`, one process, single-threaded):
elasticsearch 301,900 KiB · home-assistant core 57,244 · kubernetes 253,484 · llvm-project 584,912 · rust 380,196 ·
TypeScript checkout 223,412 · vscode 307,388. Wall: 1:39.82, 0:34.75, 3:52.92, 6:12.69, 1:00.84, 2:06.29, 1:00.36.

## What the lead verified at the source

- **Header language.** `internal/provider/treesitter/lang/lang.go:79` gives `.h` to the C grammar and `:80` gives C++
  only `.cc .cpp .cxx .hpp .hh .hxx`; `ByExtension` (`:107-115`) takes the first grammar declaring the extension.
  Recomputed from the llvm-project rows by extension: `.h` 17,897 files, 13,377 with parse errors (**74.7%**); `.c`
  14,420 files, 3,285 with errors (22.8%). The C column's 51.56% error share is therefore mostly C++ headers parsed as
  C. A header's language is a property of the repository (what includes it), not of its extension.
- **Lowering cost.** kubernetes Go: 228,765 functions, 11,599 ms total of which lowering is the remainder after
  post-dominators 167.5, control dependence 34.4 and def-use 341.9 ms — about 95% lowering. The binding's
  `ChildByFieldName` allocates and frees a C string per call (`go-tree-sitter@v0.25.0/node.go:189-192`:
  `C.CString(fieldName)` … `go_free`). The Go lowering calls it 49 times (`worker/lower_go.go`, `grep -c`) and
  never uses the id form; the JavaScript lowering uses the id form 102 times and the name form never, resolving ids
  once per process through `mustField` (`worker/lower.go:96-102`, `FieldIdForName`). Lowering per function:
  kubernetes Go ≈ 48 µs (11,055 ms / 228,765), the TypeScript checkout's JavaScript ≈ 21 µs (2,052 ms / 97,145),
  rust-lang/rust's JavaScript ≈ 162 µs (1,366 ms / 8,452) — so the field-name allocation is one measured difference
  between the two lowerings, but node counts differ too; how much of the Go figure it explains needs a CPU profile of
  the lowering walk (unavailable: no profile was taken). Converting the Go lowering to field ids is the cheap first
  step whatever the profile shows.
- **Scale of need.** Median file's native peak is under 1 MiB in every language; the largest is 303.85 MiB for a
  15.4 MiB C file (llvm-project). A fixed 256 MiB per worker is ≈300× the median need and below the largest.
