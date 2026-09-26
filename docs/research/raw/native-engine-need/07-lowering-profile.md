# 07 — CPU profile of the lowering walk, and the per-function aggregate at `a3386d7`

Provenance: the benchmark package `internal/bench` (`TestParseMemory`, `TestFunctionAnalysis`, `TestFileAnalysis`),
test binary built at commit `a3386d7`, run 2026-09-26 by the benchmark task, memory-capped, one corpus at a time, over
the seven public corpora at the pins of `05` and `06`. This research ran neither the benchmark nor the profile; both
outputs are reproduced verbatim below and read here. At `a3386d7` the Go lowering reads fields by id only: it has no
`ChildByFieldName` call (`internal/provider/treesitter/worker/lower_go.go`, `grep -c` = 0), where `06` counted 49 at
`36a529f`.

## (a) The profile: `TestFunctionAnalysis` on kubernetes Go

One run of `TestFunctionAnalysis` alone over kubernetes at `dfd7b93a`, with the Go CPU profiler on. For each file the
test parses once, walks the tree for functions (`Lowering.Functions`, `worker/lower.go:171-190`), and for each function
lowers it once untimed with the three passes, then once timed (`internal/bench/flowbench_test.go:474-485`). **`Lower`
therefore runs twice per function in this profile.** Native counting is paused (`flowbench_test.go:468`), but every
allocation the parse makes still crosses into Go through the binding's hooks.

The listings, verbatim:

```
command: (ulimit -v 6000000; TMPDIR=... GOMEMLIMIT=3GiB GOMAXPROCS=4 CODECTX_BENCH_TREE=<kubernetes dfd7b93a> CODECTX_BENCH_OUT=<rows> bench.test -test.count=1 -test.timeout=0 -test.run TestFunctionAnalysis -test.cpuprofile lower.cpu), binary built at a3386d7
== top by flat
File: bench.test
Build ID: d185f1b0b723fd4ccc503802c0159e39fcecdf69
Type: cpu
Time: 2026-09-26 11:57:57 EDT
Duration: 68.01s, Total samples = 66.46s (97.72%)
Showing nodes accounting for 58.64s, 88.23% of 66.46s total
Dropped 532 nodes (cum <= 0.33s)
Showing top 40 nodes out of 146
      flat  flat%   sum%        cum   cum%
    34.15s 51.38% 51.38%     52.68s 79.27%  runtime.cgocall
    12.45s 18.73% 70.12%     12.45s 18.73%  internal/runtime/atomic.(*Uint32).CompareAndSwap (inline)
     1.84s  2.77% 72.89%      3.56s  5.36%  runtime.cgoCheckArg
     1.50s  2.26% 75.14%      9.38s 14.11%  runtime.reentersyscall
     1.26s  1.90% 77.04%      6.85s 10.31%  runtime.exitsyscall
     1.08s  1.63% 78.66%      1.08s  1.63%  internal/runtime/syscall/linux.Syscall6
     0.75s  1.13% 79.79%      1.65s  2.48%  runtime.cgoIsGoPointer
     0.66s  0.99% 80.79%      4.40s  6.62%  runtime.cgoCheckPointer
     0.60s   0.9% 81.69%      0.66s  0.99%  runtime.inHeapOrStack
     0.53s   0.8% 82.49%      0.53s   0.8%  runtime.save
     0.49s  0.74% 83.22%      1.10s  1.66%  runtime.mallocgcSmallScanNoHeaderSC4
     0.42s  0.63% 83.85%      2.78s  4.18%  github.com/Sawmonabo/codectx/internal/bench.(*nativeCounter).free
     0.36s  0.54% 84.40%      7.83s 11.78%  github.com/tree-sitter/go-tree-sitter._Cfunc_ts_node_symbol
     0.31s  0.47% 84.86%      0.48s  0.72%  runtime.getcallerfp
     0.28s  0.42% 85.28%      0.49s  0.74%  runtime.scanObject
     0.20s   0.3% 85.59%      9.88s 14.87%  runtime.entersyscall
     0.15s  0.23% 85.81%      9.20s 13.84%  github.com/tree-sitter/go-tree-sitter.(*Node).KindId.func1 (inline)
     0.15s  0.23% 86.04%      5.64s  8.49%  runtime.cgocallbackg1
     0.14s  0.21% 86.25%      9.34s 14.05%  github.com/tree-sitter/go-tree-sitter.(*Node).KindId
     0.12s  0.18% 86.43%      4.93s  7.42%  github.com/tree-sitter/go-tree-sitter.(*Node).IsNamed.func1 (inline)
     0.11s  0.17% 86.59%      6.12s  9.21%  runtime.cgocallbackg
     0.09s  0.14% 86.73%      4.07s  6.12%  github.com/tree-sitter/go-tree-sitter._Cfunc_ts_node_is_named
     0.08s  0.12% 86.85%      7.36s 11.07%  github.com/Sawmonabo/codectx/internal/provider/treesitter/worker.(*Lowering).isCallable
     0.07s  0.11% 86.95%      1.97s  2.96%  github.com/tree-sitter/go-tree-sitter._Cfunc_ts_node_named_child_count
     0.07s  0.11% 87.06%      1.74s  2.62%  github.com/tree-sitter/go-tree-sitter._Cfunc_ts_tree_cursor_current_node
     0.06s  0.09% 87.15%      2.16s  3.25%  github.com/Sawmonabo/codectx/internal/bench.(*nativeCounter).malloc
     0.06s  0.09% 87.24%     10.26s 15.44%  github.com/Sawmonabo/codectx/internal/provider/treesitter/worker.(*goLower).assign
     0.06s  0.09% 87.33%      7.82s 11.77%  github.com/tree-sitter/go-tree-sitter.(*Node).NamedChild
     0.06s  0.09% 87.42%      7.12s 10.71%  github.com/tree-sitter/go-tree-sitter.(*Node).NamedChild.func1 (inline)
     0.06s  0.09% 87.51%      2.22s  3.34%  github.com/tree-sitter/go-tree-sitter.SetAllocator.func1
     0.06s  0.09% 87.60%      6.74s 10.14%  github.com/tree-sitter/go-tree-sitter._Cfunc_ts_node_named_child
     0.06s  0.09% 87.69%      3.28s  4.94%  github.com/tree-sitter/go-tree-sitter._Cfunc_ts_tree_cursor_goto_next_sibling
     0.05s 0.075% 87.77%      0.36s  0.54%  encoding/json/v2.makeStructArshaler.func2
     0.05s 0.075% 87.84%     10.05s 15.12%  github.com/Sawmonabo/codectx/internal/provider/treesitter/worker.(*goLower).collect
     0.05s 0.075% 87.92%      7.69s 11.57%  github.com/Sawmonabo/codectx/internal/provider/treesitter/worker.(*goLower).hoist
     0.05s 0.075% 87.99%      2.83s  4.26%  github.com/tree-sitter/go-tree-sitter.SetAllocator.func7
     0.04s  0.06% 88.05%      1.99s  2.99%  github.com/Sawmonabo/codectx/internal/bench._Cfunc_free
     0.04s  0.06% 88.11%     39.45s 59.36%  github.com/Sawmonabo/codectx/internal/provider/treesitter/worker.(*Lowering).Functions
     0.04s  0.06% 88.17%      4.97s  7.48%  github.com/tree-sitter/go-tree-sitter.(*Node).IsNamed
     0.04s  0.06% 88.23%      2.27s  3.42%  github.com/tree-sitter/go-tree-sitter.(*Node).NamedChildCount
== top by cum
File: bench.test
Build ID: d185f1b0b723fd4ccc503802c0159e39fcecdf69
Type: cpu
Time: 2026-09-26 11:57:57 EDT
Duration: 68.01s, Total samples = 66.46s (97.72%)
Showing nodes accounting for 50.97s, 76.69% of 66.46s total
Dropped 532 nodes (cum <= 0.33s)
Showing top 40 nodes out of 146
      flat  flat%   sum%        cum   cum%
     0.01s 0.015% 0.015%     64.32s 96.78%  io/fs.walkDir
         0     0% 0.015%     64.29s 96.73%  github.com/Sawmonabo/codectx/internal/bench.TestFunctionAnalysis
         0     0% 0.015%     64.29s 96.73%  github.com/Sawmonabo/codectx/internal/bench.inputs
         0     0% 0.015%     64.27s 96.70%  github.com/Sawmonabo/codectx/internal/bench.treeFiles
         0     0% 0.015%     64.27s 96.70%  io/fs.WalkDir
         0     0% 0.015%     64.27s 96.70%  testing.tRunner
     0.01s 0.015%  0.03%     63.91s 96.16%  github.com/Sawmonabo/codectx/internal/bench.treeFiles.func1
         0     0%  0.03%     63.09s 94.93%  github.com/Sawmonabo/codectx/internal/bench.TestFunctionAnalysis.func1
    34.15s 51.38% 51.41%     52.68s 79.27%  runtime.cgocall
     0.04s  0.06% 51.47%     39.45s 59.36%  github.com/Sawmonabo/codectx/internal/provider/treesitter/worker.(*Lowering).Functions
     0.01s 0.015% 51.49%     24.76s 37.26%  github.com/Sawmonabo/codectx/internal/bench.TestFunctionAnalysis.func1.1
         0     0% 51.49%     22.50s 33.85%  github.com/Sawmonabo/codectx/internal/provider/treesitter/worker.(*Lowering).Lower
         0     0% 51.49%     22.22s 33.43%  github.com/Sawmonabo/codectx/internal/provider/treesitter/worker.lowerGo
     0.01s 0.015% 51.50%     21.16s 31.84%  github.com/Sawmonabo/codectx/internal/provider/treesitter/worker.(*goLower).stmts
         0     0% 51.50%     20.54s 30.91%  github.com/Sawmonabo/codectx/internal/provider/treesitter/worker.(*goLower).stmt
         0     0% 51.50%     19.38s 29.16%  github.com/Sawmonabo/codectx/internal/bench.parse
         0     0% 51.50%     19.38s 29.16%  github.com/Sawmonabo/codectx/internal/provider/treesitter/worker.Parse (inline)
         0     0% 51.50%     19.38s 29.16%  github.com/tree-sitter/go-tree-sitter.(*Parser).ParseWithOptions
         0     0% 51.50%     19.36s 29.13%  github.com/tree-sitter/go-tree-sitter.(*Parser).ParseWithOptions.func2 (inline)
         0     0% 51.50%     19.36s 29.13%  github.com/tree-sitter/go-tree-sitter._Cfunc_ts_parser_parse_with_options
    12.45s 18.73% 70.24%     12.45s 18.73%  internal/runtime/atomic.(*Uint32).CompareAndSwap (inline)
     0.06s  0.09% 70.33%     10.26s 15.44%  github.com/Sawmonabo/codectx/internal/provider/treesitter/worker.(*goLower).assign
     0.05s 0.075% 70.40%     10.05s 15.12%  github.com/Sawmonabo/codectx/internal/provider/treesitter/worker.(*goLower).collect
     0.20s   0.3% 70.70%      9.88s 14.87%  runtime.entersyscall
     1.50s  2.26% 72.96%      9.38s 14.11%  runtime.reentersyscall
     0.14s  0.21% 73.17%      9.34s 14.05%  github.com/tree-sitter/go-tree-sitter.(*Node).KindId
     0.15s  0.23% 73.40%      9.20s 13.84%  github.com/tree-sitter/go-tree-sitter.(*Node).KindId.func1 (inline)
     0.36s  0.54% 73.94%      7.83s 11.78%  github.com/tree-sitter/go-tree-sitter._Cfunc_ts_node_symbol
     0.06s  0.09% 74.03%      7.82s 11.77%  github.com/tree-sitter/go-tree-sitter.(*Node).NamedChild
     0.05s 0.075% 74.10%      7.69s 11.57%  github.com/Sawmonabo/codectx/internal/provider/treesitter/worker.(*goLower).hoist
     0.08s  0.12% 74.23%      7.36s 11.07%  github.com/Sawmonabo/codectx/internal/provider/treesitter/worker.(*Lowering).isCallable
     0.06s  0.09% 74.32%      7.12s 10.71%  github.com/tree-sitter/go-tree-sitter.(*Node).NamedChild.func1 (inline)
     1.26s  1.90% 76.21%      6.85s 10.31%  runtime.exitsyscall
     0.06s  0.09% 76.30%      6.74s 10.14%  github.com/tree-sitter/go-tree-sitter._Cfunc_ts_node_named_child
         0     0% 76.30%      6.57s  9.89%  github.com/Sawmonabo/codectx/internal/provider/treesitter/worker.(*goLower).block
         0     0% 76.30%      6.12s  9.21%  runtime.cgocallback
     0.11s  0.17% 76.47%      6.12s  9.21%  runtime.cgocallbackg
         0     0% 76.47%      6.12s  9.21%  runtime.systemstack_switch
         0     0% 76.47%      6.04s  9.09%  github.com/Sawmonabo/codectx/internal/provider/treesitter/worker.(*goLower).ifStmt
     0.15s  0.23% 76.69%      5.64s  8.49%  runtime.cgocallbackg1
```

## (b) The reading

All shares are of the 66.46 s of samples, from the cumulative column.

| part | cumulative | share | from |
|---|---|---|---|
| parse | 19.38 s | 29% | `worker.Parse` → `_Cfunc_ts_parser_parse_with_options` 19.36 s |
| `Lower`, two lowerings per function | 22.50 s | 34% | `(*Lowering).Lower`; `lowerGo` 22.22 s |
| the callable walk itself | ≈ 14.7 s | 22% | `(*Lowering).Functions` 39.45 s less its callback `TestFunctionAnalysis.func1.1` 24.76 s |
| the passes and row bookkeeping | ≈ 2.3 s | 3% | the callback 24.76 s less `Lower` 22.50 s |

- **Per function.** 22.50 s ÷ 2 ÷ 228,765 functions ≈ 49 µs per lowering under the profiler. The timed lowering in the
  unprofiled run of (c) is 10,011.9 ms for the same 228,765 functions (the FUNCTION row's total less its three
  passes), ≈ 44 µs; `06` gives ≈ 48 µs at `36a529f`, when the Go lowering still read fields by name.
- **The walk.** The Go lowering never calls `isCallable`, so its 7.36 s is all the walk's (`worker/lower.go:176`): a
  kind test on every node of every file. The rest of the walk is cursor steps (`_Cfunc_ts_tree_cursor_goto_next_sibling`
  3.28 s, `_Cfunc_ts_tree_cursor_current_node` 1.74 s).
- **Per-node native crossings dominate both the walk and the lowering.** The accessors the listings name, each a Go
  wrapper around one call into C, summed at the wrapper level so that no figure contains another: `KindId` 9.34 s,
  `NamedChild` 7.82 s, `IsNamed` 4.97 s, `NamedChildCount` 2.27 s, cursor next-sibling 3.28 s, cursor current-node
  1.74 s — **at least 29.4 s, 44% of all samples, and about 75% of the walk plus the lowering** (39.45 s, which also
  holds the 2.3 s of passes). "At least": accessors below the listings' cut-off (`ChildByFieldId`, `StartByte`,
  `GotoFirstChild`, `GotoParent` and others) are not counted.
- **Why the flat figure is not the accessor share.** `runtime.cgocall` is 34.15 s flat (51%) and 52.68 s cumulative
  (79%). Without a C symbolizer, time spent inside C is charged to `cgocall` itself, and that includes the whole parse
  (19.36 s cumulative, among it `cgocallbackg` 6.12 s of allocator hooks calling back into Go during the parse). So
  79% is every crossing into C, the parse included; the crossings outside the parse are ≈ 33.3 s (50%), 52.68 − 19.36.
- **Per-crossing runtime cost.** `entersyscall`/`reentersyscall` 9.88 s, `exitsyscall` 6.85 s, the pointer checks
  `cgoCheckPointer` 4.40 s and `cgoCheckArg` 3.56 s, and `atomic.(*Uint32).CompareAndSwap` 12.45 s flat, are runtime
  work paid per crossing, not C work. The listings show no caller for the compare-and-swap, so it is not attributed
  further here.

What this settles for `21` §2(b): after the switch to field ids the lowering's cost is the number of native calls per
node, in the lowering and in the walk alike. The lever is fewer crossings per node (one kind and child read per visit,
reused), not faster algorithms.

## (c) The seven-corpus run: summary and per-function aggregate

`summary.txt`, verbatim:

```
built a3386d7
11:31:03 rc=0 typescript 161s rows=255110 maxrss_kb=322436
11:34:58 rc=0 kubernetes 235s rows=264824 maxrss_kb=305048
11:35:55 rc=2 home-assistant-core 57s rows=84055 maxrss_kb=61076
11:41:50 rc=0 elasticsearch 355s rows=632108 maxrss_kb=692640
11:47:00 rc=0 vscode 310s rows=397006 maxrss_kb=691880
11:50:07 rc=0 rust 187s rows=335865 maxrss_kb=585412
11:56:53 rc=2 llvm-project 406s rows=207506 maxrss_kb=545964
ALL_DONE
```

**The two `rc=2` runs are lowering panics**, one Python and one C, both in `TestFunctionAnalysis` after
`TestParseMemory` passed. Both report `flow: Builder.CloseFrame on a loop with pending continues: ContinueHere was not
called, or a continue followed it`, at `internal/provider/treesitter/flow/builder.go:472`:

- home-assistant core: `(*pyLower).loopEnd` (`worker/lower_python.go:1203`) ← `forStmt` (`:1237`) ← `stmt` (`:896`) ←
  `block` (`:844`) ← `forStmt` (`:1236`). The construct is a Python loop whose `else` clause holds a `continue` of an
  enclosing loop.
- llvm-project: `(*cLower).loopEnd` (`worker/lower_c.go:924`) ← `forStmt` (`:993`) ← `stmt` (`:625`) ← `sub`
  (`:593`) ← `forStmt` (`:987`). The construct is a C `for` whose update clause holds a jump inside a statement
  expression.

The panic ends the test binary, so for these two corpora the FUNCTION rows stop at the panicking file and there are no
FILE rows. Their FUNCTION figures below are a prefix of the corpus, not the corpus.

The per-function and per-file aggregate, verbatim (its PARSE block repeats `06`'s tree rows within run-to-run noise
and is left out):

```
FUNCTION: corpus lang fns nodes_p50 p99 max | arenaKiB_max bound_ratio p50 p99 max | total_ms lower pd cd du
elasticsearch c 7860 5 74 449 | 163.26 0.83 2.94 4.76 | 620.70 4.75 0.97 5.84
elasticsearch cpp 4820 4 69 147 | 51.61 0.83 4.28 9.05 | 111.61 2.77 0.61 5.56
elasticsearch java 553107 5 45 725 | 487.88 0.83 3.88 20.92 | 11463.91 275.71 57.95 403.61
elasticsearch javascript 4 9 25 25 | 7.23 1.44 3.01 3.01 | 0.25 0.00 0.00 0.01
elasticsearch python 26 14 46 46 | 22.88 1.32 5.23 5.23 | 4.26 0.03 0.01 0.07
elasticsearch typescript 655 5 49 81 | 39.01 0.83 3.17 6.24 | 59.03 0.43 0.08 0.53
home-assistant-core javascript 1 3 3 3 | 0.27 0.78 0.78 0.78 | 0.04 0.00 0.00 0.00
home-assistant-core python 64924 8 56 580 | 164.11 0.93 3.39 13.61 | 7290.19 49.32 9.01 72.93
kubernetes c 11 12 22 22 | 3.43 1.05 1.61 1.61 | 0.30 0.01 0.01 0.01
kubernetes go 228765 7 81 1909 | 1123.10 1.02 3.88 25.21 | 10545.04 163.89 34.17 335.06
kubernetes javascript 37 5 370 370 | 537.24 0.88 15.46 15.46 | 5.10 0.04 0.01 0.35
kubernetes python 53 7 56 60 | 14.58 1.05 3.25 3.84 | 6.49 0.05 0.01 0.09
llvm-project c 60170 5 29 2051 | 163.66 0.86 1.44 9.16 | 1165.20 21.73 4.32 20.97
llvm-project cpp 70104 7 130 2972 | 8074.42 0.95 6.19 38.30 | 3273.63 72.35 15.69 208.82
llvm-project javascript 42 5 42 42 | 12.05 0.84 3.01 3.01 | 1.44 0.02 0.00 0.03
llvm-project python 1038 8 84 306 | 82.89 0.94 5.00 9.19 | 167.15 0.88 0.17 1.59
rust c 1439 5 18 38 | 12.36 0.86 1.43 5.34 | 18.92 0.49 0.10 0.50
rust cpp 388 6 52 101 | 40.88 0.88 3.98 6.44 | 8.94 0.21 0.04 0.31
rust javascript 8452 7 81 3278 | 1566.33 0.92 5.89 15.63 | 1729.18 7.27 1.39 18.83
rust python 832 7 92 193 | 85.72 1.04 5.00 9.38 | 135.46 1.07 0.17 1.69
rust rust 245053 5 56 10414 | 6666.20 0.83 3.62 84.99 | 5015.27 126.31 26.51 242.34
rust typescript 711 5 48 152 | 39.06 0.85 3.21 5.23 | 42.53 0.41 0.08 0.51
typescript go 25072 9 72 4429 | 634.17 0.95 4.84 22.92 | 2527.66 20.71 4.04 42.92
typescript javascript 97145 3 53 20005 | 1724.06 0.80 2.66 18.30 | 2503.11 39.39 8.53 49.02
typescript tsx 950 4 25 53 | 4.91 0.81 1.27 2.16 | 20.31 0.32 0.07 0.30
typescript typescript 68931 4 61 10003 | 1029.34 0.82 3.41 28.30 | 2985.35 33.03 7.02 51.84
vscode c 9 3 21 21 | 2.39 0.80 1.45 1.45 | 0.24 0.00 0.00 0.00
vscode cpp 94 13 204 350 | 61.15 1.12 3.35 4.69 | 6.18 0.13 0.03 0.24
vscode go 7 7 31 31 | 4.05 1 1.37 1.37 | 0.14 0.00 0.00 0.00
vscode java 26 4 26 26 | 2.96 0.80 1.19 1.19 | 0.18 0.01 0.00 0.01
vscode javascript 1764 6 102 11162 | 831.48 0.88 5.14 16.27 | 226.61 1.88 0.31 3.49
vscode python 278 5 35 90 | 39.83 0.88 2.92 4.69 | 19.57 0.16 0.03 0.23
vscode rust 1747 5 51 135 | 117.48 0.86 3.51 11.22 | 49.46 1.05 0.22 1.68
vscode tsx 4197 6 75 243 | 279.59 0.86 5.04 12.24 | 425.88 3.08 0.60 5.75
vscode typescript 360076 6 66 6205 | 27405.54 0.85 3.89 47.11 | 30573.78 226.19 44.19 402.85

FILE: corpus lang files wall_s arenaPeakKiB_max nativePeakMiB_max
elasticsearch c 8 8.58 183.77 136.74
elasticsearch cpp 31 1.27 55.94 39.09
elasticsearch java 32622 67.82 505.22 9.93
elasticsearch javascript 2 0.00 8.40 0.03
elasticsearch python 4 0.01 25.30 0.30
elasticsearch typescript 45 0.13 42.12 0.89
kubernetes c 6 0.00 4.54 0.05
kubernetes go 17858 53.32 1155.04 58.63
kubernetes javascript 2 0.01 556.84 0.63
kubernetes python 7 0.01 17.28 0.25
rust c 131 0.20 13.73 2.65
rust cpp 18 0.08 44.05 1.34
rust javascript 166 2.44 1738.33 68.80
rust python 44 0.24 91.53 1.37
rust rust 38999 37.35 6712.94 131.88
rust typescript 31 0.11 42.88 1.00
typescript go 5115 7.38 820.11 37.11
typescript javascript 13131 18.06 2603.13 19.71
typescript tsx 350 0.08 6.95 0.24
typescript typescript 12804 8.26 1301.64 49.76
vscode c 3 0.00 3.33 0.16
vscode cpp 22 0.04 74.47 1.86
vscode go 4 0.00 5.32 0.04
vscode java 7 0.00 3.99 0.03
vscode javascript 155 0.57 1191.27 4.91
vscode python 75 0.04 44.88 0.52
vscode rust 88 0.29 122.91 2.95
vscode tsx 269 1.00 293.33 2.05
vscode typescript 13675 70.70 27665.28 135.09
```

Column reading is `06`'s. The FUNCTION header names five figures after the last bar, but four follow on each row:
`total_ms`, then post-dominators, control dependence and def-use, as `06` reads them. Lowering is `total_ms` less the
three.

## Unavailable, with the reason

- **Callers of the compare-and-swap and of `cgocallbackg` outside the parse:** the listings are the profile's top 40
  by flat and by cumulative time; the call graph was not written out.
- **A profile of any lowering but Go's:** only kubernetes Go was profiled.
- **FUNCTION rows for the whole of home-assistant core and llvm-project, and their FILE rows:** the lowering panics end
  those runs.
