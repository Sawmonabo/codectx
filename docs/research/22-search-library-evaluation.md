# The lexical tier on an embedded search library: a measured evaluation against today's tier, every library feature, and an audit of our search rules

Research note for the codectx product owner, behind [ADR-0013](../adr/ADR-0013-search-index.md). Research date
2026-10-08. The binary under test is `main` at `16dfef6`. The library is tantivy 0.26.2, the newest release on
crates.io that day (no 0.27 exists).

**The question.** Should the lexical tier (the index behind `codectx search` and the lexical part of every context
answer) move off SQLite onto an embedded search library, and which of the library's features, and which of our own
rules, earn their place? The owner asked for every library feature to be considered and for each of our rules to be
audited on its merits, not for the library to be judged against the rules.

**What this note rests on.** Everything is under [`raw/search-library/`](raw/search-library/), cited by path from
that folder. Its [README](raw/search-library/README.md) lists what was published, what was not and why, and how host
paths were replaced.

- `runs/` — the kubernetes runs: `00` export, `01` our index, `02` the library's index, `03` search speed (with
  `03-pooled-latency.txt` and `03-first-page.txt` derived from the per-run lines), `04` quality, and the raw logs.
- `ours/` — how our arm reproduces production's write and query paths (`01`–`07`), and its source.
- `harness/` — the library arm: a summary (`00`), notes `01`–`12`, and its source.
- `features/` (`01`–`09`) and `features-matrix/` (`00`, `10`–`18`) — every library feature, read from its source.
- `web/` — eight literature and engine questions (`01`–`08`) and their summary (`00`).

Two arms ran on the same documents. **Our arm** is today's tier, driven through the real store and the real
`codectx search` first-page path. **The library arm** indexes the same documents in the library and answers three
ways (`harness/03-query-modes.md`):
- `library`: the library's own BM25 and its pruned top 200, as a typical application uses it;
- `library-all`: the library's BM25, every match scored;
- `product`: our rules (BM25F, exact statistics over visible documents, f64 and 1e-6 quantization, content-only
  ties, the fold) in a custom collector over the library's index.

## 0. Bottom line

1. **The library's index is 44 times faster to build and about half the size.** 9.0 s against 400.7 s, 48.8 s of
   CPU against 593.7 s, 0.85 GB peak against 2.45 GB, 337–387 MB against about 641 MB live (`runs/01-ours-index.md`, `runs/02-library-index.md`).
   Activation, 165.4 s today, disappears: the library merges in the background.
2. **Our exact rules inside the library are 12 times faster than today; the library's pruned top 200 is about 3 000
   times faster.** 30 queries, warm, first page of 200: 47.4 s today, 4.04 s for `product`, 129 ms for
   `library-all`, 16 ms for `library` (`runs/03-search-speed.txt`). `product` scores every match; a pruned collector
   for our formula is the design (decision 5) and its kubernetes run is being measured (§9).
3. **`product` returns today's answers.** Folded hit counts equal today's served counts on 30 of 30 queries, and the
   first page is identical on 22 of 30. The other 8 differ only because our arm records an unfolded, document-level
   page (`runs/03-first-page.txt`).
4. **The library's own scoring cannot be adopted.** On the doc set it collapses from 0.68 to 0.02 MRR@10, because it
   gives every column its own document frequency (§3). It wins the name and subword sets, which our scoring can get
   back through the tokenizer, not through per-column scoring (§2.4).
5. **The code-aware tokenizer is the largest quality lever measured.** Under the library's scoring it lifts subword
   MRR@10 from 0.60 to 0.70 and Recall@200 from 0.22 to 0.66. Under our scoring it is being measured (§9).
6. **Today's slowness is structural, not single-threading** (§3): two indexes over the same text, a SQLite page
   round trip per posting, a full sort spooled to disk before page 1, and statistics recomputed per query.
7. **Rule audit** (§5): the scoring rules hold. The "no top-k cap" rule for ranked lists is dropped by the owner's
   ruling of Oct 8, 2026: a ranked search serves its best results. Three choices stay pending on named results: the
   code-aware tokenizer, the weighted document length and the visibility route.

## 1. Question and method

### The export (`runs/00-export.md`, `ours/01-export-path.md` to `ours/04-export-fidelity-check.md`)

- **Corpus:** kubernetes at `dfd7b93a1783878be367e1fc4a780318330cb3bf` (Sept 26, 2026), 31 407 files.
- **Producer:** the real `codectx index` at `16dfef6`, with the scip, lsp and dependence providers turned off by a
  bench-only user configuration. A nil-guarded observer inside the store's own write path captures each search
  document after the store accepts it, with the store's own `token_count` (`ours/01-export-path.md`).
- **Run:** Oct 8, 5:24 PM to 5:44 PM; 19 min 25 s; peak 2.89 GB.
- **Documents:** 546 613 (filesystem 37 080, tree-sitter 509 533) in 49 280 units, 437 001 distinct terms. 1 285 of
  those units carry no document, so the library arm, which only needs units with documents, sees 47 995.
- **Completeness check:** with the 15 459 manifest documents the export leaves out, the total is 562 072, exactly
  the stored count of the Sept 27 full run of the same commit.
- **Fidelity:** on a public smoke repository (spf13/cobra) the index arm's store equals the real run's in rows,
  `token_count` sum, distinct keys and summed name and path lengths, at 1 and 16 workers (`ours/04-export-fidelity-check.md`).
- **Token counts:** the library arm recounted every document's tokens and matched our store's `token_count` on every
  document in every build (`runs/02-library-index.md`).

### The arms' fidelity

- **Our arm** builds through `BeginUnit`, `PutSearchUnits`, `SealUnit` and `Activate`, with the production sink
  limits and worker count, and writes the rows the store requires before a search document can exist (repository,
  blobs, snapshot, generation, unit inputs, node facts with one evidence row each). Those are timed apart from search
  work (`ours/02-index-arm.md`, `ours/03-rows-the-store-forces.md`). It queries through the real first-page path, including hydration and the cursor sign
  (`ours/06-query-arm-and-filters.md`). Its departures are listed in `ours/07-departures.md`.
- **The library arm** reproduces our tokenizer from tables probed out of the product's own SQLite build: 0 anomalies
  over every codepoint, and text identical over 303 391 tokens except one known mid-character cut at 32 768 bytes
  (`harness/01-tokenizer-probe.md`). Its IDF logarithm is a bit-identical port of Go's `math.Log`; the platform `ln`
  differs on 1 891 of 200 127 inputs (`harness/02-go-log-port.md`). On a fixture, an index-free reference agreed with
  `product` on hits and first page for 18 of 18 queries, with filters, and 19 of 19 after a delta. Its departures are
  `harness/05-departures.md`.

### The query set (`runs/queries.txt`)

30 queries in six bands of five:

| Band | Queries | Matching documents |
|---|---|---|
| common terms | `vendor`, `func`, `src`, `staging`, `org` | 167 741–253 589 |
| mid terms | `output`, `metrics`, `net`, `service`, `meta` | 9 930–10 033 |
| low terms | `actions`, `progress`, `tracker`, `encrypt`, `indicate` | 996–1 000 |
| rare terms | `comparelock`, `currentutilizationval`, `messageoptionsr`, `cachedresource`, `defaultstubrefreshtoken` | 2–7 |
| two terms | `pod scheduler`, `resource version`, `node status`, `client config`, `volume mount` | 14 606–65 848 |
| phrases | `"resource version"`, `"garbage collector"`, `"service account"`, `"node name"`, `"admission controller"` | 309–1 052 |

Each query ran once cold in a fresh process (index open untimed, OS page cache left warm) and five times warm. A
warm answer that differed from the cold one was an error.

### The quality sets (`harness/07-quality-evaluation.md`)

There are no judged queries for this corpus, so three known-item sets of 1 000 queries each were derived from the
export with a fixed seed. The population is tree-sitter declarations of kind function, method, struct, class,
interface, type, enum and trait with a non-empty name.

| Set | The query | Relevant | Measures |
|---|---|---|---|
| name | a declaration's exact name | every document with that name | exact-identifier lookup |
| doc | the declaration's 4 rarest body words absent from its name, path and qualified name (at least 6 such words) | that one declaration | concept search over documentation |
| subword | one camelCase or snake_case part of 4+ characters | every declaration whose name has that whole part | partial-identifier search |

Measures: MRR@10, Success@1 and @10, Recall@200, mean hits, zero-hit and missing queries.

**The doc-set caveat.** The doc set is built so that the target matches in its body only. That is exactly the case
where per-column scoring fails (§3). It measures a real search, concept words found only in a declaration's
documentation, but it is not a neutral average, and it favours a model with one saturation over all columns.

**The subword caveat.** Our tokenizer cannot match a camelCase part (`url` does not match `parseURL`), so this set
measures a tokenizer capability. snake_case parts already match, because `_` separates tokens.

## 2. Results

Every delta is against our current implementation. "×" is today's figure divided by the library arm's.

### 2.1 Index build, disk and activation (`runs/01-ours-index.md`, `runs/02-library-index.md`, `runs/tindex.log`)

Variants are threads, writer memory budget, commit cadence and tokenizer. "end" is one commit after every document.

| Arm | Wall | Δ wall | CPU | Peak RSS | Δ RSS | Disk | Δ disk vs 641 MB live |
|---|---|---|---|---|---|---|---|
| **today** (16 unit workers, one SQLite writer) | 400.7 s | — | 593.7 s | 2.45 GB | — | about 641 MB live; database 3.46 GB | — |
| 16 threads, 50 MB each, end | 9.0 s | 44× faster | 48.8 s | 0.85 GB | −65% | 361.9 MB | −44% |
| 1 thread, 50 MB, end | 18.1 s | 22× faster | 26.9 s | 0.49 GB | −80% | 337.2 MB | −47% |
| 16 threads, 2 048 MB total, end | 13.1 s | 31× faster | 57.9 s | 1.26 GB | −49% | 353.5 MB | −45% |
| 16 threads, 50 MB each, a commit every 1 000 units | 20.4 s | 20× faster | 70.0 s | 0.86 GB | −65% | 343.1 MB | −46% |
| 16 threads, 2 048 MB, end, code-aware tokenizer | 13.3 s | 30× faster | 67.0 s | 1.36 GB | −44% | 386.8 MB | −40% |
| 1 thread, 50 MB, a commit per unit | 954.4 s | 2.4× slower | 869.1 s | 0.23 GB | −91% | 337.4 MB | −47% |

- **Today's disk.** Live search bytes are the 7 segments the generation names (265.4 MB), the full-text table's data
  (148.0 MB) and size table (9.6 MB), and `search_units` with its indexes (217.7 MB). A further 2.12 GB of
  superseded per-unit segment parts stays until a collection pass removes it; the Sept 27 full run measured 268.4 MB
  of segment parts after collection, which agrees with the live figure. About 2.39 GB of parts were written to
  produce 265 MB live, about 9× write amplification, each byte also passing through the write-ahead log.
- **Activation.** Today: 165.4 s of the 400.7 s, almost all of it the lexical merge. The library has no activation
  step. Its final commit plus the wait for background merges ("final" in `runs/tindex.log`) took 0.8 s at 1 thread
  and 4.7–8.6 s at 16 threads; a production commit does not wait for merges.
- **Commit cadence.** A commit costs about 20 ms of fsync, so 47 995 commits cost 954 s. On the fixture a per-unit
  commit cost about 18 ms at 1 thread and 66 ms at 16, because every thread flushes (`harness/04-commit-cadence.md`).
- **The code-aware tokenizer** costs 33 MB (+9%) of disk, mostly positions (`features/04-directories-storage.md`).

### 2.2 Search speed by band (`runs/03-search-speed.txt`, `runs/ours-query.log`, `runs/tquery-speed.log`)

Sum over the band's five queries of each query's warm median, first page of 200, 1 search thread. Index: 16 threads,
2 048 MB, 2 segments.

| Band | Today | `product` | Δ | `library-all` | Δ | `library` | Δ |
|---|---|---|---|---|---|---|---|
| common terms | 27 356 ms | 2 574 ms | 10.6× | 90.1 ms | 304× | 5.59 ms | 4 896× |
| mid terms | 4 446 ms | 449 ms | 9.9× | 8.88 ms | 501× | 1.59 ms | 2 800× |
| low terms | 1 867 ms | 88.0 ms | 21× | 5.61 ms | 333× | 1.11 ms | 1 676× |
| rare terms | 419 ms | 2.3 ms | 183× | 0.15 ms | 2 792× | 0.08 ms | 5 170× |
| two terms | 10 813 ms | 874 ms | 12× | 17.3 ms | 626× | 4.27 ms | 2 532× |
| phrases | 2 492 ms | 51.3 ms | 49× | 7.34 ms | 340× | 3.39 ms | 735× |
| **all 30** | **47.4 s** | **4.04 s** | **11.7×** | **129 ms** | **367×** | **16.0 ms** | **2 962×** |

Per query (warm median; "Matches" counts documents, before the fold):

| Query | Matches | Today, cold | Today, warm | `product` | Δ | `library-all` | Δ | `library` | Δ |
|---|---|---|---|---|---|---|---|---|---|
| `vendor` | 253 589 | 9.78 s | 5.89 s | 708.6 ms | 8× | 32.5 ms | 181× | 1.24 ms | 4 752× |
| `func` | 201 750 | 7.99 s | 7.11 s | 525.6 ms | 14× | 26.9 ms | 265× | 1.49 ms | 4 787× |
| `src` | 169 436 | 5.05 s | 4.86 s | 457.3 ms | 11× | 10.6 ms | 460× | 0.86 ms | 5 641× |
| `staging` | 167 943 | 5.18 s | 5.34 s | 469.7 ms | 11× | 10.8 ms | 497× | 1.01 ms | 5 271× |
| `org` | 167 741 | 4.10 s | 4.15 s | 413.0 ms | 10× | 9.37 ms | 443× | 0.99 ms | 4 203× |
| `output` | 9 987 | 1.03 s | 1.00 s | 90.5 ms | 11× | 1.45 ms | 691× | 0.26 ms | 3 882× |
| `metrics` | 9 985 | 884.6 ms | 792.0 ms | 91.1 ms | 9× | 1.50 ms | 526× | 0.29 ms | 2 722× |
| `net` | 10 033 | 1.03 s | 949.1 ms | 95.3 ms | 10× | 1.99 ms | 478× | 0.53 ms | 1 787× |
| `service` | 9 946 | 1.03 s | 967.9 ms | 89.2 ms | 11× | 1.52 ms | 638× | 0.28 ms | 3 457× |
| `meta` | 9 930 | 770.7 ms | 735.1 ms | 83.1 ms | 9× | 2.42 ms | 304× | 0.23 ms | 3 224× |
| `actions` | 1 000 | 475.2 ms | 407.9 ms | 16.4 ms | 25× | 1.38 ms | 295× | 0.13 ms | 3 067× |
| `progress` | 999 | 495.7 ms | 430.3 ms | 17.8 ms | 24× | 1.01 ms | 425× | 0.24 ms | 1 771× |
| `tracker` | 997 | 385.3 ms | 309.9 ms | 18.4 ms | 17× | 1.03 ms | 300× | 0.14 ms | 2 198× |
| `encrypt` | 996 | 334.5 ms | 275.6 ms | 18.2 ms | 15× | 1.18 ms | 233× | 0.14 ms | 1 968× |
| `indicate` | 996 | 500.7 ms | 443.0 ms | 17.2 ms | 26× | 0.99 ms | 446× | 0.46 ms | 969× |
| `comparelock` | 2 | 142.4 ms | 80.5 ms | 0.41 ms | 195× | 0.02 ms | 4239× | 0.01 ms | 10 066× |
| `currentutilizationval` | 4 | 139.3 ms | 87.6 ms | 0.40 ms | 217× | 0.03 ms | 3368× | 0.01 ms | 9 731× |
| `messageoptionsr` | 2 | 149.7 ms | 79.9 ms | 0.47 ms | 170× | 0.02 ms | 4441× | 0.01 ms | 6 150× |
| `cachedresource` | 7 | 149.8 ms | 93.2 ms | 0.50 ms | 187× | 0.05 ms | 1863× | 0.01 ms | 6 655× |
| `defaultstubrefreshtoken` | 6 | 143.4 ms | 77.5 ms | 0.50 ms | 155× | 0.04 ms | 2096× | 0.04 ms | 2 096× |
| `pod scheduler` | 36 309 | 2.05 s | 1.99 s | 169.8 ms | 12× | 3.58 ms | 556× | 0.85 ms | 2 357× |
| `resource version` | 48 699 | 2.84 s | 2.74 s | 188.3 ms | 15× | 4.14 ms | 661× | 0.94 ms | 2 923× |
| `node status` | 31 325 | 2.18 s | 2.09 s | 166.4 ms | 13× | 2.94 ms | 713× | 1.00 ms | 2 087× |
| `client config` | 65 848 | 2.88 s | 2.88 s | 234.3 ms | 12× | 4.73 ms | 610× | 1.04 ms | 2 773× |
| `volume mount` | 14 606 | 1.20 s | 1.10 s | 115.1 ms | 10× | 1.89 ms | 583× | 0.44 ms | 2 485× |
| `"resource version"` | 485 | 961.9 ms | 512.8 ms | 8.88 ms | 58× | 1.83 ms | 280× | 1.38 ms | 371× |
| `"garbage collector"` | 851 | 221.1 ms | 188.4 ms | 7.18 ms | 26× | 1.08 ms | 174× | 0.22 ms | 856× |
| `"service account"` | 1 052 | 514.4 ms | 391.0 ms | 15.7 ms | 25× | 1.19 ms | 328× | 0.26 ms | 1 487× |
| `"node name"` | 935 | 1.68 s | 1.06 s | 14.2 ms | 75× | 2.00 ms | 531× | 1.18 ms | 897× |
| `"admission controller"` | 309 | 553.1 ms | 339.3 ms | 5.27 ms | 64× | 1.23 ms | 275× | 0.34 ms | 984× |

The five common terms take 4.15–7.11 s each today and 413–709 ms
under `product`. Cold runs today add 7.4 s over the warm sum (54.8 s), mostly on the common terms.

### 2.3 Filters, segments, pages and threads (`runs/03-pooled-latency.txt`, `runs/tquery-speed.log`)

Median over all 150 warm runs (30 queries × 5).

| Run | Today | `product` | Δ | `library-all` | `library` |
|---|---|---|---|---|---|
| unfiltered | 779 ms | 51.6 ms | 15× | 1.77 ms | 0.50 ms |
| kind filter (`function`, `method`) | 730 ms | 18.6 ms | 39× | 1.55 ms | 0.76 ms |
| folder filter (`pkg`) | 748 ms | 5.5 ms | 136× | 3.73 ms | 3.35 ms |
| 11-segment index, unfiltered | — | 54.3 ms | — | 1.82 ms | 0.55 ms |
| 16 search threads, unfiltered | — | one thread only in this run | — | 1.57 ms | 0.47 ms |

- **Filters** today are applied after the full lexical candidate walk, so they save little. In `product` they are
  term-ordinal sets and an ordinal range of the sorted path dictionary, tested inside the collector, so they cut the
  work (`harness/06-extension-points.md`).
- **Segments.** 11 segments against 2 cost `product` 1–5% and the library modes 3–12%, depending on the
  statistic (sum of per-query medians or median of all runs).
- **Deep pages.** The library's pruned top-k answered pages 1, 2, 3, 5 and 10 in 15.9, 16.2, 16.5, 16.4 and 18.3 ms
  for the 30 queries: about 0.5 ms a page whatever the page number.
- **Search threads.** 16 threads cut the library modes by 26% (`library`, 16.0 to 11.8 ms) and 20% (`library-all`,
  129.3 to 103.7 ms) over 30 queries. `product` ran on one thread in this run; per-segment threads for our collector
  are proven to give identical output on the fixture (`harness/12-product-topk.md`) and their kubernetes timing is
  being measured (§9).

### 2.4 Quality (`runs/04-quality.txt`)

MRR@10 / Success@10, Recall@200 in brackets where it matters. "Δ" is MRR@10 against today.

| Arm | name | Δ | doc | Δ | subword | Δ |
|---|---|---|---|---|---|---|
| **today** | 0.833 / 95.0% | — | 0.684 / 86.5% | — | 0.379 / 64.9% (0.213) | — |
| `product` (our scoring in the library) | 0.833 / 95.0% | 0.000 | 0.684 / 86.5% | +0.000 | 0.379 / 64.9% (0.214) | 0.000 |
| `library` (per-column scoring) | 0.901 / 99.1% | +0.068 | 0.024 / 9.3% | −0.661 | 0.602 / 75.7% (0.218) | +0.223 |
| `library`, code-aware tokenizer | 0.909 / 99.1% | +0.075 | 0.046 / 12.1% | −0.639 | 0.703 / 92.8% (0.661) | +0.324 |
| `library-all` | 0.907 / 99.1% | +0.074 | 0.023 / 9.1% | −0.661 | 0.602 / 75.9% (0.218) | +0.223 |
| `library-all`, code-aware tokenizer | 0.912 / 99.1% | +0.079 | 0.046 / 12.1% | −0.639 | 0.703 / 92.8% (0.661) | +0.324 |

- `product` equals today to the fourth decimal on every set. Its subword Recall@200 differs in the fourth place
  (0.2139 against 0.2130), consistent with its counting folded results, not documents.
- The code-aware tokenizer ends subword's 13 zero-hit queries.
- The library's per-column scoring wins name and subword. That gain is set against its collapse on doc (§3). The
  code-aware tokenizer gives most of the subword gain without the per-column model; under our scoring it is being
  measured (§9).

### 2.5 Top-k exactness (`harness/08-topk-check.md`, `harness/12-product-topk.md`, `runs/tquery-speed.log`)

| Check | Index | Result |
|---|---|---|
| the library's pruned top 200 against exhaustive scoring, 30 queries | kubernetes, 2 segments | equal on 30 of 30; 0 misses; 0 score differences between the two paths |
| the same | kubernetes, 11 segments | equal on 30 of 30; 0 misses |
| the same, 1 000 name-set queries | kubernetes, 11 segments | equal on 1 000 of 1 000; 0 misses |
| the same, 812 queries | fixture: 1, 2, 3 (after a delta) and 6 segments, 1 and 4 threads | equal on 812 of 812 every time; 0 misses; same-document scores up to 2 ulp apart between the two f32 paths |
| our formula's pruned collector (block-max bounds over our BM25F), pages 1–3 | fixture: 1, 6 and 3 (after a delta) segments, unicode61 and code-aware indexes, 1 and 4 threads | identical to exhaustive scoring on 2 436 of 2 436 query-page pairs every time |
| the same, kubernetes | — | being measured (§9) |

The library's pruned page differs from `library-all`'s only in how ties are broken (document address against `id`).
On the fixture our pruned collector was about 2× faster than exhaustive `product` on page 1 (1.07 against 2.05 ms),
although its bound skipped only about 2% of blocks there: the gain is the threshold keeping documents out of the fold.
The fixture's vocabulary is synthetic and flat; the real skip rate needs kubernetes.

### 2.6 Visibility smoke (`harness/11-hide-units.md`)

One index holding every document, with a seeded share of units hidden per searcher. Reference: an index built from
only the visible documents. Fixture: 6 segments, 18 queries.

| Hidden | Units / documents | `product`, every route | `library` hits | `library` pages | Visibility build |
|---|---|---|---|---|---|
| 0% | 0 / 0 | 18/18 equal | equal | 18/18 equal | 0.08–0.14 ms |
| 10% | 43 / 234 | 18/18 equal | equal | 3/18 equal | 0.08–0.19 ms |
| 50% | 234 / 2 356 | 18/18 equal | equal | 3/18 equal | 0.09–0.20 ms |

The library's pages differ because its statistics still count hidden documents. The three routes (a query wrapper,
a collector, a custom alive set per segment) ran within noise on the fixture; their kubernetes timing at 10% and 50%
decides the route (§9). A soft delta through hiding equalled the hard delete on 19 of 19 queries.

## 3. Why today's tier is slow, and why per-column scoring collapses on the doc set

### Today's cost (`runs/01-ours-index.md`, `ours/02-index-arm.md`, `ours/06-query-arm-and-filters.md`)

- **Two indexes over one text.** Every document is written to a full-text table, which exists only to serve phrases,
  and to packed per-unit posting segments, which serve ranking. The full-text table alone is 157.6 MB live.
- **Writes that are merged twice.** Each unit seals its own segment, and activation merges them into the
  generation's: about 2.39 GB written for 265 MB live, plus 165.4 s of activation.
- **A page round trip per posting.** Every posting of every query term is read through SQLite pages. The common
  band reads 168 000–254 000 postings per term.
- **No pruning, and a sort before page 1.** Every candidate is scored, and the full ranked set is sorted, spooled to
  disk, and only then is page 1 served. Cost tracks the match count, not the page: 5.9 s for `vendor` (253 589
  matches) against 0.09 s for a rare term.
- **Statistics per query.** Document frequency and corpus totals are recomputed per query rather than read from
  columns.

It is not single-threading. `product` also answers on one thread and is 12× faster, because the library's index
removes the first three costs, and its fast columns remove the fourth. Pruning removes the rest: the library's top
200 is about 250× faster than `product` on the same index.

### The doc-set collapse (`harness/09-doc-collapse.md`)

The library scores each column as its own BM25 and adds the columns. So each column has its own document frequency:
- a doc query's words sit in about 1 700 bodies of 4 800 on the fixture (IDF about 1.04), but in only 22–35 names
  and qualified names (IDF about 5.1–5.4 there);
- the name column's average length is 0.68 tokens, so a one-token name is near saturation;
- with the 5× boost, one stray query word that happens to be a name earns about 45 points (name 22.6, qualified name
  about 23), against about 6 for the target's whole four-word body match.

Every top-10 result in the explained queries matched one or two query words in name and qualified name; the target
ranked 179–208. Our BM25F weights the term frequency 5× inside one saturation, over one document frequency for the
union of columns and the whole document's length, so the target stays in reach.

What the variants showed on the fixture (520 doc queries; `harness/10-variants.md`):
- one combined column under the library's BM25 lifts MRR@10 from 0.000 to 0.113 (ours 0.135): one df and one length
  is the property that matters;
- the union df alone keeps MRR@10 at 0.000 but lifts Recall@200 to 1.000: per-column sums with large boosts still
  favour a one-word identifier match.

The literature agrees: BM25F's one saturation over weighted frequencies is the model built for this case, and the
combined-field queries of two major engines exist for it (`web/01-field-combination.md`).

## 4. The feature matrix

Every library feature in `features-matrix/00-feature-matrix.md`, with its place in the design. "Not decided" means
ADR-0013 does not settle it and no run here decides it. Decisions are ADR-0013's.

### Indexing

| # | Feature | In the design | Why |
|---|---|---|---|
| I1 | indexing threads | used, derived from the admission's measured need (decision 4) | 9.0 s at 16 threads, 18.1 s at 1 |
| I2 | writer memory budget | used, derived the same way (decision 4) | 0.49–1.36 GB peak across budgets |
| I3 | merge threads | used, governed with the other workers (decision 4) | merges must not freeze the host |
| I4 | merge-policy knobs | the default policy, merging in the background (decision 4) | no measured need to tune; segment count costs 5–10% (§2.3) |
| I5 | no merges, then one final merge | not used | its purposes (one segment, add-ordered ids) are met by content-keyed ties and our own pruning |
| I6 | commit | used, one per generation activation (decision 4) | a commit per unit cost 954 s |
| I7 | commit payload | used: the generation's id (decision 4) | crash order and reconciliation |
| I8 | one operation batch per unit | not used | fixture: 9.5 s against 8.6 s, no gain |
| I9 | delete by term | used, on the `unit` field (decision 3) | units are freed whole |
| I10 | delete by query | not used | delete by term covers every case |
| I11 | single-segment writer | not used | a hidden API; one-shot builds only |
| I12 | build in RAM, then persist | not used | identical bytes; holds the index in memory |
| I13 | index sorting | not available | removed upstream in 0.24 |

### Settings and fields

| # | Feature | In the design | Why |
|---|---|---|---|
| F1, F2 | document-store compressor and block size | not decided | fixture only: store 233 KB (default) against 140 KB (zstd 3) |
| F3 | document-store cache | not decided | fixture: misses fall from 9 618 to 42 with the default cache |
| F4 | nothing stored, values from fast fields | not decided | the library stores what it serves (decision 8); fast-field id reads cost about 6 µs each against the cached store |
| F5 | field norms off | not decided | our scoring never reads them; the library's scoring needs them |
| F6 | frequencies and positions | used on the five text columns (decisions 2 and 8) | positions serve phrases in place of the full-text table |
| F7 | indexed, fast and stored flags | used: `kind`, path, `unit` and `token_count` are fast (decision 7) | filters and statistics inside the collector |
| F8 | value coercion | not used | values are typed at export |
| F9 | fast-field codecs | automatic | no knob |
| F10 | sstable term dictionary | not decided | fixture: 180 KB against 394 KB, same answers |

### Tokenizers

| # | Feature | In the design | Why |
|---|---|---|---|
| T1 | custom tokenizer | used: today's rules (decision 6) | the only way to keep today's terms |
| T2 | stock simple, whitespace and raw tokenizers | not used | their terms differ from today's |
| T3 | regular-expression tokenizer | not used | no measured need |
| T4 | n-grams | not used | fixture identifier recall 0.64 against 0.29, at a disk cost; the code-aware form is the candidate |
| T5 | lower-casing and ASCII folding filters | not used | our tokenizer folds case and removes diacritics itself |
| T6 | long-token removal | avoided (decision 6) | the default analyzer silently drops tokens of 40 bytes or more |
| T7 | stemming and stop words | not used | they change which documents match |
| T8 | compound-word splitting | not used | needs a dictionary; identifiers are open vocabulary |
| T9 | camelCase and snake_case parts at the whole token's position | pending (decision 6) | the largest quality lever (§2.4); decided under our scoring (§9) |
| T10 | pre-tokenized text | not used | each document is tokenized once, in the library |
| T11 | the indexer's 65 530-byte token limit | never reached | our tokenizer cuts at 32 768 bytes first |

### Queries

| # | Feature | In the design | Why |
|---|---|---|---|
| Q1 | query parser | not used | it turns a multi-token literal into a phrase; codectx ORs its terms |
| Q2 | term query | postings read directly by the collector (decision 2) | our formula needs per-column frequencies |
| Q3 | term-set query | not used | the kind filter is an ordinal set inside the collector (decision 7) |
| Q4 | boolean query | not used | the collector drives the union itself |
| Q5 | phrase query and slop | phrases from positions; no slop | slop is new semantics no rule asks for |
| Q6 | phrase-prefix query | a new feature, opt-in, with an explicit limit (decision 7) | it caps silently at 50 expansions |
| Q7 | regular-expression query | a new feature, opt-in (decision 7) | the folder filter uses an ordinal range instead |
| Q8 | fuzzy query | a new feature, opt-in (decision 7) | new semantics |
| Q9 | range query | not used as a query | the folder filter is an ordinal range inside the collector |
| Q10 | exists query | not used | no codectx query needs it |
| Q11, Q14 | disjunction-max query | not used | under the pruned collector this release sums the disjuncts; the fix is merged upstream, not released |
| Q12 | boost query | not used | the column weights live in our formula |
| Q13 | constant-score query | not used | filters run inside the collector |
| Q15 | the library's block-max WAND | not used | pruning is built for our formula (decision 5) |
| Q16 | more-like-this | not used | not a codectx query |

### Scoring

| # | Feature | In the design | Why |
|---|---|---|---|
| S1 | the library's BM25 | not used (decision 2) | per column, f32; doc set 0.02 against 0.68 |
| S2 | statistics provider | not used | it cannot express a union df and counts deleted documents |
| S3 | one-byte field-norm code | not used | exact to 40 tokens, then up to about 11% low; we use `token_count` |
| S4 | block-max upper bounds | rebuilt for our formula (decision 5) | the library's bounds bake in its own model |
| S5 | score tweak and custom order | not used | ties are content-keyed in our comparator |
| S6 | scoring disabled in the library's loop | used | the collector scores |
| S7 | direct postings walk | used (decision 2) | per-column frequencies and positions |
| S8 | explain | not used in the product | a debugging tool; its value equalled the score on 360 of 360 fixture hits |

### Collectors and aggregations

| # | Feature | In the design | Why |
|---|---|---|---|
| C1 | top-docs collector with offset | not used | f32 scores and address-based ties |
| C2 | order by a fast field | not used | no query needs it |
| C3 | ties on document address | not used | addresses change with threads and merges |
| C4 | tuple sort keys | not used | still f32 |
| C5 | filter collector | not used | filters run inside our collector; the byte-column variant silently drops every document on a string fast field |
| C6 | count | not used | the match count comes from our collector, exact or marked as a lower bound (decision 5) |
| C7 | custom collector | used (decision 2) | it carries our formula, statistics, fold and comparator |
| C8 | facet and histogram collectors | a new feature, opt-in (decision 7) | per-folder counts |
| C9 | aggregations | a new feature, opt-in, with explicit limits (decision 7) | the terms aggregation returns 10 buckets by default |

### Search runtime and storage

| # | Feature | In the design | Why |
|---|---|---|---|
| RT1 | multi-threaded search executor | used, governed like every worker (decision 4) | identical output; timing for our collector being measured |
| RT2 | warmers | not decided | fixture only |
| RT3 | reload policy | not decided | the product opens a generation's view (decision 1) |
| RT4 | searcher reuse | used | a held searcher is a consistent view |
| RT5 | memory-map advice | not decided | fixture: 18/18 equal answers, open 0.14–0.20 ms |
| RT6 | space usage | measurement only | disk attribution in `features/04-directories-storage.md` |
| RT7 | snippets | a new feature, opt-in (decision 7) | no body is shown today |
| — | custom alive set per segment | used for visibility (decision 3) | hidden documents never become candidates |
| — | memory-mapped, immutable segments | used (decision 1) | — |

## 5. The rule audit

Each of codectx's search rules, what it buys, what it costs, and its verdict. Rule numbers are those of
`web/00-summary.md`. The rule-audit runs (`runs/06-rule-audit.md`) are being measured and will add their figures
to this table.

| Rule | What it buys | What it costs | Verdict |
|---|---|---|---|
| 1. exact df over the union of columns, visible documents only | the doc set (0.68 against 0.02); a delta equals a fresh index | a df pass per query term, from columns | **keep** (decisions 2 and 3) |
| 2. a delta equals a fresh index | identical answers whatever the build history | exact statistics over visible documents | **keep** (decision 3) |
| 3. no top-k cap on ranked results | every match reachable | scoring every match on every page: 4.04 s for 30 queries against milliseconds pruned | **drop**, by the owner's ruling of Oct 8, 2026. The no-default-limit rule covers the data an index produces (files, units, facts, rows, exact lookups), not ranked lists. A ranked search serves its best results through an exact block-max top-k (decision 5); the exact tier keeps returning every match |
| 4. content-only, deterministic ties | pages and cursors repeat across builds, threads and merges | a content tie key read per page | **keep** (decision 2) |
| 5. retained snapshots | branch switching, pinned reads, reattached hidden units | one index holds every retained unit | **keep** (decision 3) |
| 6. hidden units never count | a generation ranks like an index of its own units (18/18 on the fixture) | a visibility set per searcher, 0.08–0.2 ms | **keep** (decision 3); the route is **pending** its kubernetes time |
| 7. f64 BM25F, one saturation, weights 5/5/2/2/1, length = `token_count` | the doc set; exact lengths; a 1e-6 grid f32 cannot hold | our own collector and pinned logarithm | **keep** (decision 2); **pending**: the document length weighted per column, adopted if it raises MRR@10 on two of the three sets and lowers it on none |
| 8. today's tokenizer rules (letters, numbers, private use; case folded; diacritics removed) | today's terms and phrases | camelCase parts do not match | **keep** the rules (decision 6), and never a silent length cut; **pending**: the code-aware form, adopted if it lifts subword, holds name within 0.01 MRR@10 and does not lower doc, under our scoring |
| 9. tiers first, then lexical score | exact lookups before ranked text | — | **keep** (unchanged) |
| 10. keyset-cursor paging over one content-keyed total order | repeatable paging that survives merges | — | **keep**; page n is collected as the top 200·n (decision 5) |
| 11. scores quantized to 1e-6 | order independent of summation order | one rounding per score | **keep** (decision 2) |
| new: one commit per generation activation | 954 s of fsync avoided | units are invisible until their generation activates | **adopt** (decision 4) |

On the evidence (`web/00-summary.md`), rules 1, 2, 4, 5, 10 and 11 rest on identity and repeatability more than on
measured ranking quality: no source measures the quality effect of counting deleted documents, f32 against f64, or
exact against quantized lengths. Rule 7's length and rule 8 are the two the literature challenges, which is why both
are pending a measurement rather than settled by argument.

## 6. Engines: the landscape and the embedding evidence (`web/07-engine-landscape.md`)

No engine meets the rules out of the box. The library is the only one that serves as postings, columns and storage
under our own scoring.

| Engine | Why not chosen |
|---|---|
| SQLite FTS5 (today) | measured above; its `bm25()` is one saturation over column-weighted frequencies, the shape we keep |
| Lucene 10 | needs a JVM beside the Go binary, which ADR-0012 retires |
| Bleve (pure Go) | scores each column independently and adds them; no published performance evidence. The only alternative worth an arm; none was built |
| Bluge | unmaintained since July 2022; its IDF puts the +0.5 outside the ratio |
| Xapian | at most two concurrent versions, which breaks retention; GPL |
| Meilisearch | a server; caps hits at 1 000 by default |
| Zoekt | a substring (trigram) engine, a different class |
| PISA | in-memory research engine, single segment |

- **The only reproducible multi-engine benchmark** is the Search Benchmark Game: one field, read-only, English
  Wikipedia, query latency only. Lucene 10.3 now beats tantivy 0.25 on top-k, and tantivy is faster in the mode that
  visits every match. No benchmark uses a code corpus.
- **Embedding.** A cgo call costs about 30–40 ns, so a page comes back as one packed buffer, never one call per hit.
  Two production Go programs ship the library through cgo (Milvus, Anytype's binding). A Rust panic that unwinds
  across a C boundary aborts the process since Rust 1.81, so every export catches panics (decision 1).

## 7. Threats to validity

- **One corpus.** kubernetes is Go-majority and generated-code-heavy. The speed ratios are structural and expected
  to hold, but quality figures are this corpus's.
- **Derived judgments.** The quality sets are known-item sets built from the export, not human judgments. The doc
  set favours one-saturation scoring by construction (§1). Absolute MRR values are not comparable to published ones.
- **Our arm omits parts of production**: the exact tiers, relations, aliases, other providers' units and their
  writer contention, and capability states at activation (`ours/07-departures.md`). Its index is therefore, if
  anything, faster than a real run's.
- **The library arm's departures** (`harness/05-departures.md`): lexical tier only; a token cut at a character
  boundary rather than mid-character; extra fast fields counted in its disk; its library modes count raw documents,
  not folded results, and read every phrase position where ours stops at 512.
- **Cold means a fresh process on a warm page cache.** No cold-disk figure was measured.
- **A shared host.** The host's load and free memory were sampled every 5 s (`runs/host-load.tsv`); warm runs vary.
- **First-page comparison.** Our arm records an unfolded, document-level page, `product` a folded one. All 8
  differing first pages are fold differences: 3 rare queries differ at position 3 or 5, where our page lists a second
  document of an already-listed result, and 5 at positions 117–172 (`runs/03-first-page.txt`).
- **Fixture-only evidence.** The visibility routes, our pruned collector, our collector's threads and most variants
  were proven on a 4 800-document synthetic fixture. Its vocabulary is flat, so its timings show that a variant runs,
  not which is fast.

## 8. Settled by this record

ADR-0013 decisions 1 to 5 and 7 to 9 rest on §2–§6: the library's index, our ranking in a custom collector, one
index with a visibility set per generation, one commit per activation with the library committed first, a ranked
page as an exact top-k, filters from fast fields, and the removal of the full-text table, the packed segments, the
activation merge and the ranked-set spool.

## 9. Still being measured

On the same kubernetes export, running as of Oct 8, 2026 at 9:48 PM. Records land in `runs/05-variants.md`,
`runs/06-rule-audit.md` and `runs/07-delta.md`.

| Measurement | Decides | Rule |
|---|---|---|
| our formula's pruned collector: exactness on all 30 queries and every quality query, and its speed | confirms decision 5 on the real corpus | — |
| our collector with per-segment search threads | the thread share the governor grants (decision 4) | — |
| our scoring with the code-aware tokenizer, on all three sets | decision 6 | adopted if it lifts subword, holds name within 0.01 MRR@10 and does not lower doc |
| our scoring with the document length weighted per column | decision 2 | adopted if it raises MRR@10 on two of the three sets and lowers it on none |
| the three visibility routes at 10% and 50% hidden | decision 3 | the fastest; all give identical pages |
| deltas: our delta against delete and re-add, and statistics before and after a merge | decision 9's test 5 on the real corpus | a delta's pages equal a fresh index's |
| the rule-audit runs | re-measure §5 | — |

After integration, the kubernetes measurement is repeated on the integrated binary and recorded here (decision 9).

## Sources

Literature:
- S. Robertson, H. Zaragoza, *The Probabilistic Relevance Framework: BM25 and Beyond*, Foundations and Trends in IR
  3(4), 2009, §3.6, https://staff.city.ac.uk/~sbrp622/papers/foundations_bm25_review.pdf
- H. Zaragoza, N. Craswell, M. Taylor, S. Saria, S. Robertson, *Microsoft Cambridge at TREC-13: Web and HARD
  tracks*, TREC 2004, https://trec.nist.gov/pubs/trec13/papers/microsoft-cambridge.web.hard.pdf
- A. Broder, D. Carmel, M. Herscovici, A. Soffer, J. Zien, *Efficient query evaluation using a two-level retrieval
  process*, CIKM 2003, https://doi.org/10.1145/956863.956944
- S. Ding, T. Suel, *Faster top-k document retrieval using block-max indexes*, SIGIR 2011,
  https://research.engineering.nyu.edu/~suel/papers/bmw.pdf
- C. Kamphuis, A. P. de Vries, L. Boytsov, J. Lin, *Which BM25 Do You Mean? A Large-Scale Reproducibility Study of
  Scoring Variants*, ECIR 2020, https://doi.org/10.1007/978-3-030-45442-5_4
- J. Lin, P. Yang, *The Impact of Score Ties on Repeatability in Document Ranking*, SIGIR 2019,
  https://arxiv.org/abs/1807.05798
- E. Hill, D. Binkley, D. Lawrie, L. Pollock, K. Vijay-Shanker, *An Empirical Study of Identifier Splitting
  Techniques*, Empirical Software Engineering 19(6), 2014, https://doi.org/10.1007/s10664-013-9261-0
- Radha, Goktas, *Improving BM25 Code Retrieval Under Fixed Generic Tokenization*, arXiv:2605.18561, 2026,
  https://arxiv.org/abs/2605.18561

Engines and libraries:
- tantivy 0.26.2 source, https://github.com/quickwit-oss/tantivy/tree/0.26.2
- tantivy pull request 3022, the disjunction-max fix, https://github.com/quickwit-oss/tantivy/pull/3022
- Lucene `CombinedFieldQuery`, https://github.com/apache/lucene/blob/main/lucene/core/src/java/org/apache/lucene/search/CombinedFieldQuery.java
- Elasticsearch `combined_fields` query, https://www.elastic.co/docs/reference/query-languages/query-dsl/query-dsl-combined-fields-query
- J. Tibshirani, Sourcegraph, *Keeping it boring (and relevant) with BM25F*, https://sourcegraph.com/blog/keeping-it-boring-and-relevant-with-bm25f
- Zoekt design, https://github.com/sourcegraph/zoekt/blob/main/doc/design.md
- GitHub, *The technology behind GitHub's new code search*, 2023, https://github.blog/engineering/architecture-optimization/the-technology-behind-githubs-new-code-search/
- The Search Benchmark Game, https://github.com/quickwit-oss/search-benchmark-game
- A. Grand, *An analysis of Search Benchmark, the Game*, 2025, https://jpountz.github.io/2025/05/12/analysis-of-Search-Benchmark-the-Game.html

Embedding:
- The Rust 1.81 release notes, https://blog.rust-lang.org/2024/09/05/Rust-1.81.0/
- cgo documentation, https://pkg.go.dev/cmd/cgo
- Shane, *CGO Performance In Go 1.21*, 2023, https://shane.ai/posts/cgo-performance-in-go1.21/
- tantivy-go, https://github.com/anyproto/tantivy-go
- Milvus, https://github.com/milvus-io/milvus

Every other claim, with its source, year, URL and rating, is in `raw/search-library/web/` and
`raw/search-library/features/`.
