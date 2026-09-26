# 03 — Least resident memory: parse-tree cost per grammar, streaming, the arena release rule, runtime and allocator settings

Date 2026-09-26. Evidence is the source at the branch head, the pinned parser library and grammar modules read in the
Go module cache (cited as `<module>@<version> <path>:<line>`), pages fetched on this date (URLs inline), and the
**finished** benchmark rows of three public corpora, produced by the benchmark binary built at `36a529f`
(`internal/bench/flowbench_test.go` and `internal/bench/allocator.go` define every field):

| corpus | commit | rows | what it exercises |
|---|---|---|---|
| a TypeScript compiler checkout | `cf8cf4f6c17ada5e920949ad984791ceb9ce06df` | 31,411 parse, 122,259 function, 18,250 file | TypeScript, JavaScript, TSX, and the checkout's Go port |
| kubernetes | `dfd7b93a1783878be367e1fc4a780318330cb3bf` | 17,884 parse (17,858 Go) plus function and file rows | Go |
| home-assistant core | `5d0107199025d24c9155fc38584ea12476f67fb5` | 18,947 parse (18,931 Python) | Python |

Every quantile below is computed **per file** (one ratio per parse row, then the quantile over rows), never from a
summed total. Languages with fewer than 20 rows in a corpus are not summarised. No row was run for this note.

**None of these figures is a constant in a design.** They are evidence about the mechanism: what the design derives
from the repository and machine in front of it is stated in each recommendation.

One caveat about the corpus mix, stated before any number: the TypeScript checkout's JavaScript files are compiler
test baselines, and **10,722 of its 13,131 JavaScript files parse with errors** (its TypeScript: 1,087 of 12,804).
Its JavaScript figures describe **a fixture corpus with intentional syntax errors**. None of the finished corpora is
a JavaScript-majority production repository. C, C++, Java and Rust have no finished corpus (fixture rows only).

---

## (a) Parse-tree bytes per source byte, per grammar

### What the runtime's source says a node costs

From `go-tree-sitter@v0.25.0` (the binding `go.mod:13` pins; its vendored core library under `src/`), with the
struct sizes computed from the declarations under the x86-64 System V layout:

- `Subtree` is a union of an 8-byte inline record and a pointer (`src/subtree.h:157-160`): **8 bytes**.
- `SubtreeHeapData` (`src/subtree.h:111-154`): `ref_count` 4, two `Length` of 12 each (`src/length.h:9-12`),
  three `uint32` counters, symbol and parse state, 11 flag bits, then a union of the non-terminal fields (24 bytes)
  and `ExternalScannerState` (a 24-byte inline buffer or heap pointer plus a length, `src/subtree.h:31-37`: 32 bytes)
  aligned to 8: **80 bytes**.
- An internal node is **one block of `80 + 8·child_count` bytes**: its children array with the heap data placed after
  it (`ts_subtree_alloc_size`, `src/subtree.h:248-250`; `ts_subtree_new_node` reallocs the children array to that
  size, `src/subtree.c:485-490`).
- A leaf is **inline — only its 8-byte slot in the parent** — when `symbol <= UINT8_MAX`, it is not an external token,
  and its padding and size fit (`src/subtree.c:175-179`): padding under 255 bytes and 16 rows, size under 255 bytes
  **and on one row**, lookahead under 16 bytes (`src/subtree.c:155-163`, `TS_MAX_INLINE_TREE_LENGTH` = 255 at
  `src/subtree.c:22`). Otherwise the leaf is an **80-byte heap block** from the subtree pool (at most 32 recycled
  blocks, `src/subtree.c:23`, `:136-150`), plus its 8-byte slot.
- An external token whose serialized scanner state exceeds 24 bytes adds a **counted** `long_data` block of that
  length (`src/subtree.c:28-31`).
- Every block carries the C allocator's own header and rounding (glibc: 8-byte header, 16-byte granularity), which the
  counting allocator does not see (it records requested sizes, `internal/bench/allocator.go:63-73`).

So the structural cost is **≈ 88 bytes per internal or heap-leaf node plus 8 bytes per inline leaf**, before allocator
overhead. At the measured median of roughly 16–26 tree bytes per source byte, that is about one heap node per
4–5 source bytes.

### What makes the ratio differ between grammars

The `symbol <= UINT8_MAX` test does **not** discriminate among the pinned grammars: leaves are tokens, token ids come
first, and every pinned grammar's `TOKEN_COUNT` is at most 219 (`tree-sitter-cpp@v0.23.4 src/parser.c`; c 161, go 95,
java 138, javascript 134, python 108, rust 157, typescript 166, tsx 172). The discriminators are:

1. **External tokens are never inline.** `EXTERNAL_TOKEN_COUNT`: c, go, java 0; cpp 2; javascript 8; typescript and
   tsx 10; rust 11; python 12 (each grammar's `src/parser.c` header). Python's NEWLINE, INDENT and DEDENT are
   external, so every line break and indentation change is an 80-byte heap leaf; JavaScript's automatic semicolons
   and template characters likewise.
2. **Multi-line tokens are never inline** (`size.extent.row == 0`, `src/subtree.c:160`): block comments, docstrings,
   template and raw strings.
3. **Token density.** A leaf costs the same whatever its length, so a file that is one long literal costs almost
   nothing per byte, and a file of short tokens costs the most. Hidden rules that the grammar keeps as nodes add
   internal nodes per token (how many per grammar is unavailable: no row counts nodes).
4. **Error recovery** adds error nodes (always heap) and grows the parse stack, which shows in the peak, not the tree.
5. **Python's serialized scanner state** is `2 + delimiters + 2·(indent depth − 1)` bytes
   (`tree-sitter-python@v0.25.0 src/scanner.c:364-395`), so beyond indent depth 12 every external token also carries a
   counted `long_data` block.

**The scanners' own allocations are O(1) or O(nesting), not O(source).** Each grammar's vendored `alloc.h` maps
`ts_malloc`/`ts_calloc` to the parser library's replaceable pointers only under `TREE_SITTER_REUSE_ALLOCATOR`, and to
libc otherwise (`tree-sitter-python@v0.25.0 src/tree_sitter/alloc.h:13-39`, the same in cpp). No grammar's Go binding
defines it (`bindings/go/binding.go:3`: `-std=c11 -fPIC`). What they allocate: rust one `calloc` of a 1-byte struct
per parser (`tree-sitter-rust@v0.24.2 src/scanner.c:20-24`); cpp one struct of 68 bytes (a length byte and 16 wide
characters, `tree-sitter-cpp@v0.23.4 src/scanner.c:13-16`, `:97-98`); python one struct plus an indent stack and a
delimiter stack that grow with nesting (`tree-sitter-python@v0.25.0 src/scanner.c:86-88`, `:419-427`); javascript,
typescript and tsx nothing (their create returns NULL, `tree-sitter-javascript@v0.25.0 src/scanner.c:17`). The
benchmark marks cpp, python and rust `scanner_bytes: null` and the three ECMAScript grammars 0
(`internal/bench/flowbench_test.go:293-312`), which is correct as "unmeasured" for the first three, and the source bounds the unmeasured amount to bytes per parser, not bytes per source byte.

### What the finished rows show (per file)

`tree_bytes` is what closing the tree released (`flowbench_test.go:332-333`, `:386-389`); `native_peak_bytes` is the
counted high-water during the parse above the base (`:324-327`, `:373-378`).

| corpus · language | files | tree/src p50 | p90 | p99 | max | min | peak/src p50 | p90 | p99 | max |
|---|---|---|---|---|---|---|---|---|---|---|
| TypeScript checkout · typescript | 12,804 | 26.0 | 42.9 | 61.5 | 277.7 | 0.018 | 27.1 | 44.9 | 67.8 | 278.1 |
| TypeScript checkout · javascript (erroring fixtures) | 13,131 | 22.8 | 35.3 | 50.2 | 79.8 | 0.017 | 24.2 | 39.1 | 61.7 | 109.2 |
| TypeScript checkout · tsx | 350 | 21.2 | 30.1 | 41.0 | 48.0 | 5.7 | 21.8 | 31.5 | 41.1 | 48.6 |
| TypeScript checkout · go | 5,115 | 19.1 | 25.2 | 35.6 | 54.4 | 0.015 | 19.3 | 25.2 | 35.7 | 54.7 |
| kubernetes · go | 17,858 | 15.8 | 26.0 | 35.9 | 80.2 | 0.030 | 15.8 | 26.0 | 36.1 | 80.2 |
| home-assistant core · python | 18,931 | 22.9 | 29.2 | 40.4 | 77.8 | 3.9 | 23.0 | 29.8 | 41.8 | 78.0 |

Largest file per corpus and language, absolute:

| corpus · language | source bytes | tree bytes | peak bytes | input copies |
|---|---|---|---|---|
| TypeScript checkout · typescript (`checker.ts`, the largest peak too) | 3,151,774 | 52,172,504 | 52,173,024 | 5,914,569 |
| TypeScript checkout · javascript | 377,765 | 11,427,064 | 11,427,792 | 690,044 |
| TypeScript checkout · go (a test file that is one large literal) | 4,029,319 | 921,176 | 921,208 | 4,029,382 |
| TypeScript checkout · go, largest peak | 1,501,771 | 38,400,440 | 39,170,432 | 1,501,795 |
| kubernetes · go, largest file | 4,035,510 | 51,563,960 | 51,568,056 | 5,515,709 |
| kubernetes · go, largest peak | 1,704,050 | 61,473,072 | 61,474,112 | 2,359,524 |
| home-assistant core · python | 502,315 | 11,918,224 | 11,918,256 | 808,103 |

Readings:

- **The ratio is not a per-grammar constant.** Within one language it spans three orders of magnitude: 0.015 to 54 for
  Go in one corpus, 0.030 to 80 in another. The minimum files are one-token files (a literal or a comment); the
  maxima are small files (the 277.7 maximum is a 1,236-byte TypeScript file that parses with errors). Files of 1 MiB
  and more sit **below** the median (in the TypeScript checkout, 3 TypeScript files of ≥ 1 MiB at 8.5× median and
  4 Go files at 10.6× median — a list, too few for a quantile), because large files are disproportionately data. A per-byte reservation fitted to the median over-reserves large files and
  under-reserves small erroring ones.
- **Peak ≈ tree for clean parses.** Peak/tree p50 is 1.00–1.02 in every language; the parse stack is transient and
  small. For erroring fixtures it is not: JavaScript peak/tree p99 2.27, max 13.83; TypeScript p99 2.13, max 17.37.
  That is error-recovery stack growth, which a reservation must see and a tree-size model does not.
- **The grammar mechanism shows in the medians**: Python (12 external tokens, every line break a heap leaf) at 22.9
  sits above Go (no externals) at 15.8–19.1; TypeScript (10 externals, type
  annotations) is the highest at 26.0.
- **Input copies are 1.00–1.01× the source at the median and up to 2.0× on large files** (TypeScript files ≥ 64 KiB:
  p50 1.41, max 2.00; JavaScript max 3.94). The binding's read callback copies every chunk it returns into a C string
  kept until the parse ends (`go-tree-sitter@v0.25.0 parser.go:272-278`, freed at `:326-330`), after a Go
  `string(...)` conversion that copies it once more into the Go heap; the lexer re-reads chunks, so the copies exceed
  the source. The worker reads in 64 KiB slices (`internal/provider/treesitter/worker/extract.go:13-16`,
  `worker/worker.go:216-227`).

### What `rss_after − rss_before` says about what the counter misses

- **On a file that sets a new counted high-water, RSS growth ≈ counted peak + input copies.** `checker.ts`: counted
  peak 52.17 MB, copies 5.91 MB, RSS growth 55.89 MB. Kubernetes' largest-peak file: 61.47 MB counted, 63.05 MB RSS
  growth; its largest file: 51.57 MB counted + 5.52 MB copies against 59.55 MB growth. So the counter plus the known
  copies is a tight estimator of a tree's resident cost. The residual contains the C allocator's headers and the
  counter's own pointer table, which is Go heap, one map entry per live block (`internal/bench/allocator.go:65-73`) —
  a benchmark artefact, not a production cost — so the rows cannot split the residual further.
- **On every other file RSS does not move.** 29,819 of 31,411 parse rows (TypeScript checkout), 17,344 of 17,884
  (kubernetes) and 18,270 of 18,947 (home-assistant) show no RSS growth: freed tree memory is retained by the C
  allocator and reused. The process's RSS rose to 197.9 MB, 150.5 MB and 58.7 MB while the counted live maximum was
  53.0, 61.6 and 12.0 MB. **RSS is the allocator's high-water, not a file's need.**
- **RSS does fall sometimes** (kubernetes 146.8 → 87.5 MB between two consecutive rows, after a 309,597-byte file),
  but the rows do not split Go heap from C heap, so whether the drop was the Go scavenger or the C allocator trimming
  is **unavailable** from these rows.
- **Consequence for measurement in production**: a per-file RSS delta is zero for about 95% of files, so a worker's
  per-file need must come from resetting and reading its own peak (`clear_refs` value 5, then `VmHWM`; verified in
  `00-mechanism-probes.md` P1, 6.9 µs per status read in P2) — and that peak is only the file's need if the retained
  free memory has been returned first (see (d)).

**Recommendation.** The worker's per-file memory estimate is learned, per language, from the file's own observed
peak
(reset-and-read `VmHWM` around each file), keyed by source bytes and by whether the parse had errors, never from a
per-grammar constant. The model is not "k bytes per source byte": the rows show a ratio that falls with file size and
rises with error recovery, so the learned estimate is a per-language, per-size-class high quantile of observed
peak/source that updates as files complete.

**Strongest alternative, steel-manned.** Ship a per-grammar bytes-per-source-byte table derived from the runtime's
structure (88 bytes per heap node, the external-token count) and the matrix corpora: it needs no measurement
machinery, is deterministic, is available for the first file of a run, and the p99 figures above (36–68×) would cover
99% of files.

**Why the recommendation wins.** The same language spans 0.015× to 80× in one repository, so any table is a constant
tuned to the corpora that produced it; it would reserve 30× a 4 MB literal file that needs 0.23×, and under-reserve a
small erroring file at 277×. The observed peak needs no model of the grammar at all and includes what the table cannot
see (copies, scanner state, error-recovery stacks, allocator overhead).

**Trade-off accepted.** The first file of a never-seen language has no observation; it runs on the standing
allocation's admission rule (the answer to that belongs to the need-derived reservation question, not this one), and
the estimate is noisy until a size class has a few observations.

**The measurement the benchmark task must take to confirm it.** On every corpus of the matrix (one large repository
per language family plus mixed monorepos, including a JavaScript-majority production repository and C, C++, Java and
Rust corpora, none of which has finished rows yet): per file, the worker's `VmHWM` after a reset against the counted
peak plus input copies. **Pass**: per language, `VmHWM − RSS_before` is within the allocator-overhead band of counted
peak + copies on every file that follows a trim (see (d)); **fail**: any file whose `VmHWM` delta exceeds counted +
copies by more than that band names memory the model misses.

---

## (b) Streaming frames to the coordinator versus buffering a file

### What each side holds per file today

**Worker** (`internal/provider/treesitter/worker/worker.go:67-96`, `:141-168`): the source reassembled whole
(`wire.ReadMessage(in, SourceBytes)`, `:83`), the parse tree (16–26× the source at the median, above), the input
copies (1.0–2.0×) until the parse returns, then the extraction maps (`worker/extract.go:64-79`: declarations by span,
imports, references) built over the whole query run and sorted before anything is emitted (`extract.go:220-369`).
Frames leave through a 64 KiB buffered writer (`worker.go:56`) as each record is written, so output is already
streamed; the tree is closed only after the `done` frame is written (`worker.go:156`, `defer tree.Close()`).

**Parent** (`internal/provider/treesitter/provider.go:264-289`): the source read whole (`provider.go:316-343`), then
`pool.exchange` (`pool.go:757-808`) decodes every frame into `extraction` slices of `wire.Decl`, `wire.Import`,
`wire.Ref` (`pool.go:702-707`), held until the builder finishes. The builder then makes a second representation:
`declFact` embeds each `wire.Decl` with its signature, doc and search body copied out of the source
(`facts.go:190-256`), node facts with one `model.Evidence` per occurrence, relation facts deduplicated by relation ID
in a map with their evidence appended per occurrence (`facts.go:718-797`), aliases and search units. Only then does
`emit` hand everything to the sink in slices of `maxPutRecords` (`facts.go:813-855`). Peak parent holding is therefore
source + wire records + builder facts, simultaneously.

A `model.Evidence` (`internal/model/facts.go:215-241`) is twelve string or ID fields, a precision, two range
pointers, a native key and a detail, plus a separately allocated `SourceRange` of two positions
(`internal/model/source.go:12-23`): on the order of 250–350 bytes per occurrence by its field layout, most strings
shared. The per-file count of records and occurrences is **unavailable**: no benchmark row records it, so the parent's
per-file holding cannot be given in bytes here.

### What streaming would save, and what it costs

- **Straight into the batch sink, record by record — not possible without changing the fact model.** The builder does
  not emit one fact per record: a callee node gets another evidence row per call through it (`addEvidence`,
  `facts.go:734-745`), a relation is one fact with one evidence per occurrence (`putRelation`, `facts.go:781-797`),
  and the per-fact evidence clip is counted across the whole file (`evidenceFull`, `facts.go:763-771`). Streaming
  records straight into the sink would publish one fact per occurrence and depend on the store merging repeated
  identities within one unit; whether the unit writer merges or refuses a node or relation identity seen in two
  batches of the same unit was **not read** and is the precondition for this option.
- **Validation needs the whole file in two places.** Every offset is checked against the pinned bytes
  (`rangeOf`, `facts.go:175-188`), and signatures and docs are cut from them (`facts.go:233-240`), so the source must
  stay resident on the parent whatever is streamed. A reference is resolved against **every** declaration
  (`targets` reads `byName`, `facts.go:667-699`), so references can only be processed after the last declaration —
  which the worker's emission order already guarantees (all declarations, then imports, then references,
  `extract.go:311-367`).
- **Ordering is not an obstacle.** The sink already flushes nodes before any batch that references them
  (`internal/provider/sink.go:631-633`, `:707-722`), so node and relation puts may interleave.
- **Atomicity is the real cost.** Today a worker that dies mid-file is retried once with the same source
  (`provider.go:348-376`), and nothing of the first attempt was put. With records already in the sink, a retry would
  duplicate them unless the retry discards the unit's sink and starts the unit over.
- **What decoding frames into the builder as they arrive would save** is the `extraction` copy — the wire records of
  one file — while keeping the per-file fact aggregation. It is a second-order saving: in the worker the tree
  dominates (tens of bytes per source byte), and in the parent the builder's facts and evidence are a larger
  representation of the same records than the wire form.
- **The first-order lever is on the worker side, and it is not framing:** the tree is closed after the whole
  extraction has been emitted and the done frame written. Freeing the tree as soon as the query and lowering walks
  finish, before the records are serialised, is the one-pass-per-file question and is answered there, not here.

**Recommendation.** Keep per-file aggregation of facts on the parent (it is what makes one fact carry every
occurrence and the evidence clip file-wide), and remove the intermediate `extraction` buffer by validating and
folding each frame into the builder as it is decoded: declarations first, then imports, then references, exactly the
order the worker sends. Keep the retry-once rule, which remains correct because nothing reaches the sink before the
file completes.

**Strongest alternative, steel-manned.** Stream every record straight to the sink and let the store merge evidence per
identity: the parent would hold one frame and one batch rather than a file, which makes the parent's memory
independent of the largest file's fact count — the only term on the parent that still names a file's size.

**Why the recommendation wins.** The alternative changes three contracts to save a representation that is not the
dominant term: the fact model (one fact per identity with its evidence), the retry rule (a retry becomes a unit
restart), and the file-wide evidence clip; and it still cannot drop the source, which validation needs. The parent's
per-file holding is bounded by the file, which the per-file memory estimate already covers.

**Trade-off accepted.** The parent still holds one file's facts at once; a generated file with millions of call sites
holds millions of evidence rows until it is emitted.

**The measurement the benchmark task must take to confirm it.** Per file, on every matrix corpus: records emitted,
evidence rows, and the parent's Go heap high-water around the file (`runtime/metrics` `/memory/classes/heap/objects`
sampled around the build), against the worker's tree peak. **Pass**: parent holding per file ≤ the worker's tree peak
for the same file on every repository class (the tree remains the dominant term, so streaming stays second-order);
**fail**: any repository class where the parent's per-file holding exceeds the tree peak, which makes the
straight-to-sink option worth its contract changes.

---

## (c) The arena release threshold

`internal/provider/treesitter/flow/arena.go:17-22`, `:31-50`: at `Begin`, if the previous function used more than
`releaseBytes = 1 << 20` (arena bytes + scratch), every backing array is dropped; otherwise the backing is kept, and a
function that spilled across slabs gets one consolidated slab of everything it was handed (`arena.go:124-136`). ADR-0012
§2 states the threshold "is what stops the high-water mark falsifying decision 5" and cites nothing for the value.
**The 1 MiB is not fitted to anything**: no row, measurement or source is given for it in the ADR, the research note
(`docs/research/20-native-engine-post-mvp.md` §7.2) or the code.

### What the function and file rows show

| corpus · language | functions | use p50 | p99 | p99.9 | max | functions over 1 MiB | arena retained p50 |
|---|---|---|---|---|---|---|---|
| TypeScript checkout · javascript | 97,145 | 383 B | 11.4 KB | 50.5 KB | 2.67 MB | 2 | 404 KB |
| TypeScript checkout · go | 25,072 | 1.22 KB | 31.0 KB | 145 KB | 840 KB | 0 | 1.25 MB |
| kubernetes · go | 228,765 | 1.02 KB | 28.9 KB | 98.1 KB | 1.18 MB | 1 | 455 KB |

("use" is `arena_bytes + scratch_bytes`, `flowbench_test.go:437-445`; "retained" is `Arena.Retained`,
`arena.go:87-94`.)

Per file, the arena's peak against the file's counted native peak (`arena_peak_bytes / native_peak_bytes`,
`flowbench_test.go:543-546`): TypeScript checkout JavaScript p50 0.100, p99 0.483, max 2.46; its Go p50 0.087, p99
0.188; kubernetes Go p50 0.043, p99 0.276, max 1.97.

Readings:

- **The threshold almost never fires**: 3 of about 351,000 functions across the three corpora.
- **What it lets the worker retain is not small relative to a typical file**: retention sits at 0.4–1.25 MB, set by
  the slabs' doubling and consolidation, not by the threshold (and can exceed it: Go retained 1.25 MB > 1 MiB, because
  the rule tests the previous function's use, not the retained capacity). The median Go file's whole tree is
  3.6 KB × 15.8 ≈ 57 KB in kubernetes, so the arena kept for "the next function" is ~8–20× the median file's tree.
- **Against a large file it is noise**: ~1 MB against a 50–60 MB tree.
- The arena is the minor term per file at the median (4–10% of the native peak) and exceeds the tree only in a
  pathological function (the 20,005-node synthetic control-flow fixture).

**Recommendation.** Remove the constant: release every arena backing array at the **file boundary**, when the tree is
closed, and keep the within-file rule of reuse and consolidation. The arena then retains, between files, nothing, and
within a file at most what that file's largest function needed. A file is already the unit of scheduling and
caching (ADR-0012 §1), so this ties the arena's lifetime to a unit the design already has rather than to a byte count.

**Strongest alternative, steel-manned.** A scale-free rule inside the file stream: release when the previous function's
use exceeds k × a running high quantile of use (for example k × the running p99), so a worker in steady state
allocates nothing per function *or* per file, and only an outlier resets it. It adapts to the repository in front of
it and keeps the zero-allocation steady state the arena exists for.

A base of the running **median**, as first proposed, is the wrong base: p99/p50 of use is about 25–30× and max/p50
about 700× (Go) to 7,000× (JavaScript) in these rows, so a k that tolerates ordinary functions never fires and a k
that fires churns on every function above p90.

**Why the recommendation wins.** The alternative still carries a constant (k and the quantile), and the rows show its
benefit is at most a few slab allocations per file — the arena grows by doubling, so a file re-grows its backing in
about log2(largest function use) allocations — against a retained floor that exceeds the median file's whole tree.
Releasing at the file boundary is constant-free, and it makes the worker's between-files footprint independent of
every file it has ever seen, which is what makes a per-file peak measurement mean the file's need.

**Trade-off accepted.** Every file pays its arena growth afresh (a handful of zeroed `make` calls), and the dropped
backing is garbage until the next collection, so the Go heap does not shrink at the instant of release.

**The measurement the benchmark task must take to confirm it.** File rows with the release at the file boundary
against today's rule, on every matrix corpus with a lowering: `wall_ns` per file and the worker's between-files RSS.
**Pass**: per-file wall within the run-to-run noise of the two rules at p50 and p99 on every repository class, and the
between-files arena retention zero; **fail**: a p50 wall regression on any class larger than noise, which would price
the rule's allocations.

---

## (d) `GOGC` / `GOMEMLIMIT`, and the C allocator's retention

### Can production count cgo allocation cheaply?

- The binding **already** routes every core allocation through Go in production: its `init` installs Go closures and
  points the library's four pointers at C trampolines that call exported Go functions (`go-tree-sitter@v0.25.0
  allocator.go:22-36`, `:38-56`, `:105-110`; `allocator.c:8-14`; the library side, `src/alloc.c:33-48`). Every
  `malloc` a parse makes already crosses C → Go → C.
- A counter that is exact needs the pointer table the benchmark uses, because the binding frees its own `C.CString`
  copies through the same free hook (`parser.go:326-330`, `node.go:190-191`, `query.go:631-633`, `tree.go:83`,
  `:101`), so a table-free counter based on usable sizes would subtract memory it never added
  (`internal/bench/allocator.go:51-73`).
- Its measured cost: `counted_parse_ns / parse_ns` (the table against a paused table that still locks on frees,
  `flowbench_test.go:343-349`) is **p50 1.22–1.36, p90 1.44–1.72, p99 2.17–2.92** across the six summarised
  corpus-language pairs. The pause is not free of the counter, so the full cost against no hook is at least that.
- Production does not need it: the per-file peak comes from `VmHWM` (a), which sees scanners, copies and allocator
  overhead that the table does not. glibc's `mallinfo2` (since 2.33) reports in-use and free bytes, but "only the main
  memory allocation area. Allocations in other arenas are excluded" (https://man7.org/linux/man-pages/man3/mallinfo.3.html).

### What the Go runtime's limit governs

The limit is on "`Sys` − `HeapReleased`": "The memory limit only accounts for memory managed by the Go runtime";
C-allocated memory is outside it (https://go.dev/doc/gc-guide, "Memory limit"). The guide's own rules cut both ways:
"**Do** feel free to adjust the memory limit in real time … a cgo program where C libraries temporarily need to use
substantially more memory", but "**Don't** use the memory limit when deploying to an execution environment you don't
control, especially when your program's memory use is proportional to its inputs. A good example is a CLI tool or a
desktop application", and "**Don't** set a memory limit to avoid out-of-memory conditions when a program is already
close to its environment's memory limits". The protection against a limit set too low is a GC CPU cap of "roughly 50%,
with a `2 * GOMAXPROCS` CPU-second window", so "the program will slow down at most by 2x" — while the same guide calls
thrashing "particularly dangerous because it effectively stalls the program". `debug.SetMemoryLimit` is adjustable at
any time and returns the previous value (https://pkg.go.dev/runtime/debug#SetMemoryLimit).

- **Worker.** Its Go heap is the source, one file's extraction maps, the arena (~1 MB retained today) and the chunk
  copies' Go strings; the tree is C. A dynamic `SetMemoryLimit(reservation − native live)` would push the collector
  hardest exactly while a large tree is live, over a heap it can barely shrink. The limit governs almost nothing the
  worker spends.
- **Coordinator.** Its Go heap is most of its memory, but its queued records are already bounded by construction: the
  sink pool charges every queued record and outstanding decode against `index.queue_bytes` (`sink.go:50-70`,
  `:103-140`). A limit set near that bound is the guide's "already close to its environment's limits" case, with the
  2× slowdown as the upside and a stall as the downside, and a limit set with headroom above it binds nothing the pool
  does not already bind.

### What the C allocator does with a freed tree

- Freed tree blocks go back to glibc's free lists and are reused (the zero-RSS-growth rows in (a)); RSS does not fall.
- `malloc_trim` "attempts to release free memory from the heap (by calling sbrk(2) or madvise(2) …)", and "Since
  glibc 2.8 this function frees memory in all arenas and in all chunks with whole free pages"
  (https://man7.org/linux/man-pages/man3/malloc_trim.3.html). This host runs glibc 2.43. After a whole tree is freed,
  the tree's pages are wholly free except where they interleave with the parser's retained buffers, so a trim at the
  file boundary can return most of them; how much it returns and what it costs is **unavailable** (no row measures
  it).
- The mmap threshold starts at 128 KiB and rises "to the size of the freed block", up to 32 MiB on 64-bit, and
  "Dynamic adjustment of the mmap threshold is disabled if any of the M_TRIM_THRESHOLD, M_TOP_PAD, M_MMAP_THRESHOLD, or
  M_MMAP_MAX parameters is set" (https://man7.org/linux/man-pages/man3/mallopt.3.html). A large tree's biggest blocks
  (a root with tens of thousands of children is one `80 + 8·n` block) therefore move from `mmap` (returned on free) to
  the heap (retained) after the first large free.
- Arenas: without `M_ARENA_MAX` the limit is derived from the CPU count once `M_ARENA_TEST` arenas exist (default 8
  on 64-bit), and threads get their own arenas (same page). A parse runs on whichever OS thread the goroutine is on
  for the whole C call; successive files may run on different threads and so allocate from different arenas, and the
  freed memory of one arena does not serve another. The `checker.ts` row fits that: RSS grew 55.9 MB for a file whose
  counted peak exceeded the previous high-water by only 13.7 MB. It is a consistent explanation, not a measured one.
  `MALLOC_ARENA_MAX` must be set "before the first call to a memory-allocation function", i.e. in the worker's spawn
  environment; it is glibc-only (musl, macOS and Windows allocators have other rules).

**Recommendation.** Keep "set neither `GOGC` nor `GOMEMLIMIT`", for a corrected reason: counting cgo is possible, but
in the worker the limit would govern a small heap while the C tree is the need, and in the coordinator the heap is
already bounded by the sink pool, so a limit adds only thrashing risk. What returns a large file's memory is the C
allocator, so the worker, at the end of every file after closing the tree: runs the parse goroutine locked to one OS
thread for the worker's life (`runtime.LockOSThread`), so every tree comes from one arena and freed memory serves the
next file; calls `malloc_trim(0)` where the C library provides it; and then resets its peak for the next file
(`clear_refs` 5). On glibc the worker's spawn environment additionally sets `MALLOC_ARENA_MAX=1`, which makes
`mallinfo2` cover all of it; on other libraries the thread lock is what holds the tree to one heap and `VmHWM`
remains the measurement.

**Strongest alternative, steel-manned.** Set a dynamic soft limit in both processes, re-derived between files from the
admission ledger: `SetMemoryLimit(reservation − counted native live)` in the worker and `SetMemoryLimit(base
footprint reservation)` in the coordinator. It is exactly the guide's recommended real-time cgo use, it turns the
reservation from an estimate into something the runtime acts on, the 50% CPU cap bounds a mistake at a 2× slowdown,
and it makes the Go side return memory "more aggressively" when the host is short.

**Why the recommendation wins.** Its target is the wrong memory. In the worker the Go heap is a small term beside the
tree (the arena is 4–10% of the native peak at the median and the rest of the Go heap is one file's records), so a
limit there buys a few megabytes at the price of collector work concentrated on the largest files. In the coordinator
the retained queue is already bounded by the pool, and a limit near that bound is the case the guide warns replaces an
OOM risk with a slowdown. The retained memory the rows actually show — hundreds of megabytes of RSS against tens of
live — is C allocator retention, which no Go runtime setting reaches and a trim at the file boundary does.

**Trade-off accepted.** A trim per file costs a walk of the allocator's free lists (its cost is unmeasured) and the
next file re-requests pages from the kernel; the thread lock ties one OS thread to the parse loop; and the
arena-count setting is platform-specific, so on non-glibc platforms the worker's between-files RSS is whatever that
allocator retains, measured and disclosed but not trimmed.

**The measurement the benchmark task must take to confirm it.** On every matrix corpus, per file in a worker: RSS
after the tree is closed, with and without the thread lock, `MALLOC_ARENA_MAX=1` and `malloc_trim(0)`; the trim's
wall time; and, in the coordinator, the Go heap's high-water against the sink pool's capacity over a whole run.
**Pass**: with the three measures, the worker's RSS after each file returns to within the allocator-overhead band of
its between-files baseline on every repository class, the trim costs less than the noise band of a median file's
wall, and the coordinator's heap never exceeds the pool bound plus its measured idle overhead; **fail**: RSS after
large files stays above the baseline (trim ineffective on that allocator), the trim is a visible share of the median
file's wall, or the coordinator's heap outgrows the pool (which is then the case for a coordinator limit).

---

## Recommended design

1. **Per-file need is observed, not modelled.** The worker resets its peak before each file and reads it after
   (`clear_refs` 5 / `VmHWM`); the learned estimate is per language and per size class, with erroring parses kept
   apart, and nothing in it is a per-grammar constant. The runtime's structure (≈ 88 bytes per heap node plus 8 per
   inline leaf; external and multi-line tokens always on the heap) explains the spread and justifies learning by
   language, but supplies no number.
2. **The worker returns a file's memory before the next one.** Close the tree, release the arena's backing, trim the
   C heap, then reset the peak: the worker's between-files footprint then depends on no file it has seen, and the next
   file's peak is that file's need. The parse loop is pinned to one OS thread so one heap serves every tree; on glibc
   the spawn environment sets one arena.
3. **The arena has no byte threshold.** Reuse and consolidation within a file; everything dropped at the file
   boundary.
4. **Framing: decode into the builder, keep per-file facts.** The parent folds each frame into the builder as it
   arrives (declarations, imports, references in the order sent), drops the intermediate `extraction` copy, and still
   hands the sink one file's aggregated facts — preserving the fact model, the file-wide evidence clip and the
   retry-once rule.
5. **No Go runtime memory settings.** `GOGC` default, no `GOMEMLIMIT` in either process: the worker's need is C
   memory the limit cannot see, and the coordinator's retained records are already bounded by the sink pool.
6. **Unavailable, and what closes it**: per-file record and evidence counts (no row carries them); the trim's cost and
   yield; the split of RSS drops between the Go scavenger and the C allocator; all C, C++, Java and Rust corpora and a
   JavaScript-majority production repository (no finished rows). Each is a column the benchmark task adds: records and
   evidence per file, RSS after close with and without the three measures, trim wall time, and Go heap versus C heap
   from `runtime/metrics` beside `VmHWM`.
